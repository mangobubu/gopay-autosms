// Package herotask manages independent, receive-only HeroSMS number tasks.
// It intentionally has no dependency on the GoPay batch workflow: every task
// row is one requested number and owns its complete provider lifecycle.
package herotask

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/herosms"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

const (
	defaultSchedulerTick       = 500 * time.Millisecond
	defaultLeaseDuration       = 2 * time.Minute
	defaultPollInterval        = 45 * time.Second
	defaultStockRetryMinimum   = 2 * time.Second
	defaultStockRetryMaximum   = 30 * time.Second
	defaultErrorRetryMinimum   = 5 * time.Second
	defaultErrorRetryMaximum   = time.Minute
	defaultRefundWindow        = 20 * time.Minute
	defaultActivationLifetime  = 20 * time.Minute
	defaultWorkerCount         = 8
	purchaseUnknownRecheckWait = 24 * time.Hour
	maximumStoredErrorLength   = 1024
)

// Client is the receive-only HeroSMS boundary used by Manager. The concrete
// HeroSMS client implements it; keeping the boundary small makes lifecycle
// and crash-recovery behavior testable without HTTP fixtures.
type Client interface {
	PurchaseOne(context.Context, herosms.PurchaseRequest) (herosms.Purchase, error)
	GetMessages(context.Context, string, bool) ([]herosms.Message, error)
	RequestAnother(context.Context, string) error
	Finish(context.Context, string, bool) error
	Cancel(context.Context, string, bool) error
}

// Store is named locally so Manager tests can implement only the independent
// number-task persistence contract. PostgreSQL satisfies it structurally.
type Store interface {
	storage.HeroSMSTaskStore
}

type Config struct {
	SchedulerTick             time.Duration
	LeaseDuration             time.Duration
	PollInterval              time.Duration
	StockRetryMinimum         time.Duration
	StockRetryMaximum         time.Duration
	ErrorRetryMinimum         time.Duration
	ErrorRetryMaximum         time.Duration
	RefundWindow              time.Duration
	DefaultActivationLifetime time.Duration
	WorkerCount               int
	// Now is intended for deterministic tests. Production callers should leave
	// it nil so UTC wall-clock time is used.
	Now func() time.Time
}

// CreateTasksInput describes one user submission. Quantity creates that many
// durable rows immediately; it never replaces or groups existing tasks.
type CreateTasksInput struct {
	SubmissionID     string                    `json:"submission_id,omitempty"`
	ProductKind      domain.HeroSMSProductKind `json:"product_kind,omitempty"`
	ServiceCode      string                    `json:"service_code"`
	ServiceName      string                    `json:"service_name,omitempty"`
	CountryCode      string                    `json:"country_code"`
	CountryName      string                    `json:"country_name,omitempty"`
	VerificationType herosms.VerificationType  `json:"verification_type,omitempty"`
	DurationHours    *int                      `json:"duration_hours,omitempty"`
	MaxPrice         string                    `json:"max_price,omitempty"`
	Currency         string                    `json:"currency,omitempty"`
	Operator         string                    `json:"operator,omitempty"`
	Quantity         int                       `json:"quantity"`
}

// ReceiveMessageInput is suitable for both authenticated HeroSMS callbacks
// and other durable message fan-out adapters. An unknown provider activation
// ID is retained as an orphan and attached atomically when purchase commits.
type ReceiveMessageInput struct {
	TaskID               *int64                      `json:"task_id,omitempty"`
	ProviderActivationID string                      `json:"provider_activation_id"`
	ProviderMessageID    string                      `json:"provider_message_id,omitempty"`
	Source               domain.HeroSMSMessageSource `json:"source,omitempty"`
	Code                 *string                     `json:"code,omitempty"`
	Text                 *string                     `json:"text,omitempty"`
	ProviderReceivedAt   *time.Time                  `json:"provider_received_at,omitempty"`
	RawPayload           json.RawMessage             `json:"-"`
}

type persistedTaskOptions struct {
	Operator string `json:"operator,omitempty"`
}

type Manager struct {
	store  Store
	client Client
	cfg    Config
	logger *slog.Logger
	owner  string

	startOnce sync.Once
	wg        sync.WaitGroup
	sem       chan struct{}
}

func New(store Store, client Client, cfg Config, logger *slog.Logger) *Manager {
	cfg = withDefaults(cfg)
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		store: store, client: client, cfg: cfg, logger: logger,
		owner: "hero-task-" + bestEffortToken(), sem: make(chan struct{}, cfg.WorkerCount),
	}
}

func withDefaults(cfg Config) Config {
	if cfg.SchedulerTick <= 0 {
		cfg.SchedulerTick = defaultSchedulerTick
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = defaultLeaseDuration
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.StockRetryMinimum <= 0 {
		cfg.StockRetryMinimum = defaultStockRetryMinimum
	}
	if cfg.StockRetryMaximum < cfg.StockRetryMinimum {
		cfg.StockRetryMaximum = defaultStockRetryMaximum
		if cfg.StockRetryMaximum < cfg.StockRetryMinimum {
			cfg.StockRetryMaximum = cfg.StockRetryMinimum
		}
	}
	if cfg.ErrorRetryMinimum <= 0 {
		cfg.ErrorRetryMinimum = defaultErrorRetryMinimum
	}
	if cfg.ErrorRetryMaximum < cfg.ErrorRetryMinimum {
		cfg.ErrorRetryMaximum = defaultErrorRetryMaximum
		if cfg.ErrorRetryMaximum < cfg.ErrorRetryMinimum {
			cfg.ErrorRetryMaximum = cfg.ErrorRetryMinimum
		}
	}
	if cfg.RefundWindow <= 0 {
		cfg.RefundWindow = defaultRefundWindow
	}
	if cfg.DefaultActivationLifetime <= 0 {
		cfg.DefaultActivationLifetime = defaultActivationLifetime
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = defaultWorkerCount
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return cfg
}

// Run starts the scheduler once. It returns immediately; Wait joins the
// scheduler and all workers after ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	if m == nil || m.store == nil || m.client == nil {
		return errors.New("HeroSMS task manager is not configured")
	}
	m.startOnce.Do(func() {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.loop(ctx)
		}()
	})
	return nil
}

func (m *Manager) Wait() {
	if m != nil {
		m.wg.Wait()
	}
}

func (m *Manager) CreateTasks(ctx context.Context, input CreateTasksInput) ([]domain.HeroSMSNumberTask, error) {
	params, err := m.createParams(input)
	if err != nil {
		return nil, err
	}
	tasks, err := m.store.CreateHeroSMSTasks(ctx, params)
	if err != nil {
		return nil, err
	}
	for index := range tasks {
		tasks[index] = m.visibleTask(tasks[index], m.now())
	}
	return tasks, nil
}

func (m *Manager) createParams(input CreateTasksInput) (storage.CreateHeroSMSTasksParams, error) {
	input.SubmissionID = strings.TrimSpace(input.SubmissionID)
	input.ServiceCode = strings.TrimSpace(input.ServiceCode)
	input.ServiceName = strings.TrimSpace(input.ServiceName)
	input.CountryCode = strings.TrimSpace(input.CountryCode)
	input.CountryName = strings.TrimSpace(input.CountryName)
	input.MaxPrice = strings.TrimSpace(input.MaxPrice)
	input.Currency = strings.TrimSpace(input.Currency)
	input.Operator = strings.TrimSpace(input.Operator)
	if input.Quantity <= 0 || input.Quantity > storage.MaxHeroSMSTaskQuantity ||
		input.ServiceCode == "" || input.CountryCode == "" {
		return storage.CreateHeroSMSTasksParams{}, storage.ErrInvalidInput
	}
	country, err := strconv.Atoi(input.CountryCode)
	if err != nil || country < 0 || country > 999 {
		return storage.CreateHeroSMSTasksParams{}, storage.ErrInvalidInput
	}

	kind := input.ProductKind
	if kind == "" {
		kind = domain.HeroSMSProductActivation
		if input.DurationHours != nil {
			kind = domain.HeroSMSProductRent
		}
	}
	if !kind.Valid() ||
		(kind == domain.HeroSMSProductActivation && input.DurationHours != nil) ||
		(kind == domain.HeroSMSProductRent && (input.DurationHours == nil || *input.DurationHours <= 0)) {
		return storage.CreateHeroSMSTasksParams{}, storage.ErrInvalidInput
	}
	verification := herosms.VerificationType(strings.ToLower(strings.TrimSpace(string(input.VerificationType))))
	if verification == "" {
		verification = herosms.VerificationSMS
	}
	if verification != herosms.VerificationSMS && verification != herosms.VerificationCall {
		return storage.CreateHeroSMSTasksParams{}, storage.ErrInvalidInput
	}
	// HeroSMS exposes activationType for short activations, while rental
	// purchase currently supports SMS only.
	if kind == domain.HeroSMSProductRent && verification != herosms.VerificationSMS {
		return storage.CreateHeroSMSTasksParams{}, storage.ErrInvalidInput
	}
	options, err := json.Marshal(persistedTaskOptions{Operator: input.Operator})
	if err != nil {
		return storage.CreateHeroSMSTasksParams{}, err
	}
	return storage.CreateHeroSMSTasksParams{
		SubmissionID: input.SubmissionID, ProductKind: kind,
		ServiceCode: input.ServiceCode, ServiceName: input.ServiceName,
		CountryCode: input.CountryCode, CountryName: input.CountryName,
		VerificationType: string(verification), DurationHours: cloneInt(input.DurationHours),
		MaxPriceAmount: input.MaxPrice, Currency: input.Currency,
		ProviderPayload: options, NextRunAt: m.now(), Quantity: input.Quantity,
	}, nil
}

func (m *Manager) GetTask(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	if id <= 0 {
		return domain.HeroSMSNumberTask{}, storage.ErrInvalidInput
	}
	task, err := m.store.GetHeroSMSTask(ctx, id)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	return m.withMessages(ctx, task)
}

func (m *Manager) ListTasks(ctx context.Context, filter storage.HeroSMSTaskFilter) ([]domain.HeroSMSNumberTask, error) {
	tasks, err := m.store.ListHeroSMSTasks(ctx, filter)
	if err != nil {
		return nil, err
	}
	for index := range tasks {
		tasks[index], err = m.withMessages(ctx, tasks[index])
		if err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

func (m *Manager) StartTask(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	if id <= 0 {
		return domain.HeroSMSNumberTask{}, storage.ErrInvalidInput
	}
	task, err := m.store.RestartHeroSMSTask(ctx, id, m.now())
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	return m.withMessages(ctx, task)
}

// StopTask persists user intent before returning. The scheduler then chooses
// refund cancellation or normal settlement from authoritative stored state.
func (m *Manager) StopTask(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	if id <= 0 {
		return domain.HeroSMSNumberTask{}, storage.ErrInvalidInput
	}
	task, err := m.store.RequestHeroSMSTaskStop(ctx, id)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	return m.withMessages(ctx, task)
}

func (m *Manager) ReceiveMessage(
	ctx context.Context,
	input ReceiveMessageInput,
) (storage.AppendHeroSMSTaskMessageResult, error) {
	input.ProviderActivationID = strings.TrimSpace(input.ProviderActivationID)
	input.ProviderMessageID = strings.TrimSpace(input.ProviderMessageID)
	code := normalizedOptional(input.Code)
	text := normalizedOptional(input.Text)
	if input.ProviderActivationID == "" {
		return storage.AppendHeroSMSTaskMessageResult{}, storage.ErrInvalidInput
	}
	// HeroSMS documents both fields as nullable. An empty callback is useful to
	// the legacy audit inbox but contains no receive-only message to append.
	if code == "" && text == "" {
		return storage.AppendHeroSMSTaskMessageResult{}, nil
	}
	source := input.Source
	if source == "" {
		source = domain.HeroSMSMessageWebhook
	}
	if source != domain.HeroSMSMessageWebhook {
		return storage.AppendHeroSMSTaskMessageResult{}, storage.ErrInvalidInput
	}
	var receivedAt *time.Time
	if input.ProviderReceivedAt != nil && !input.ProviderReceivedAt.IsZero() {
		value := input.ProviderReceivedAt.UTC()
		receivedAt = &value
	}
	result, err := m.store.AppendHeroSMSTaskMessage(ctx, storage.AppendHeroSMSTaskMessageParams{
		TaskID: cloneInt64(input.TaskID), ProviderActivationID: input.ProviderActivationID,
		ProviderMessageID: input.ProviderMessageID, Source: source,
		Code: code, Text: text, ProviderReceivedAt: receivedAt,
		RawPayload: validJSONPayload(input.RawPayload),
	})
	if err != nil {
		return storage.AppendHeroSMSTaskMessageResult{}, err
	}
	if result.Task != nil {
		visible := m.visibleTask(*result.Task, m.now())
		result.Task = &visible
	}
	return result, nil
}

// ReceiveHeroSMSMessage is a descriptive alias for webhook fan-out adapters.
func (m *Manager) ReceiveHeroSMSMessage(
	ctx context.Context,
	input ReceiveMessageInput,
) (storage.AppendHeroSMSTaskMessageResult, error) {
	return m.ReceiveMessage(ctx, input)
}

func (m *Manager) withMessages(ctx context.Context, task domain.HeroSMSNumberTask) (domain.HeroSMSNumberTask, error) {
	if task.Messages != nil {
		return m.visibleTask(task, m.now()), nil
	}
	messages, err := m.store.ListHeroSMSTaskMessages(ctx, task.ID)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	task.Messages = messages
	return m.visibleTask(task, m.now()), nil
}

func (m *Manager) visibleTask(task domain.HeroSMSNumberTask, now time.Time) domain.HeroSMSNumberTask {
	if task.Status == domain.HeroSMSTaskActive && task.RefundStatus == domain.HeroSMSRefundRefundable &&
		task.RefundableUntil != nil && !now.Before(task.RefundableUntil.UTC()) {
		task.RefundStatus = domain.HeroSMSRefundUnavailable
	}
	if task.Messages == nil {
		task.Messages = make([]domain.HeroSMSNumberMessage, 0)
	}
	return task
}

func (m *Manager) loop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.SchedulerTick)
	defer ticker.Stop()
	m.runTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runTick(ctx)
		}
	}
}

func (m *Manager) runTick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	available := cap(m.sem) - len(m.sem)
	if available <= 0 {
		return
	}
	tasks, err := m.store.ClaimDueHeroSMSTasks(
		ctx, m.owner, m.now(), m.cfg.LeaseDuration, available,
	)
	if err != nil {
		m.logger.Error("claim HeroSMS number tasks", "error", err)
		return
	}
	for _, task := range tasks {
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		m.wg.Add(1)
		go func(task domain.HeroSMSNumberTask) {
			defer m.wg.Done()
			defer func() { <-m.sem }()
			m.processTask(ctx, task)
		}(task)
	}
}

func (m *Manager) processTask(ctx context.Context, task domain.HeroSMSNumberTask) {
	switch task.Status {
	case domain.HeroSMSTaskWaitingNumber:
		m.processWaitingNumber(ctx, task)
	case domain.HeroSMSTaskPurchasing:
		// This state is a durable "request may have been sent" fence. It is
		// never legal to issue another provider purchase from it.
		m.markStalePurchaseUnknown(ctx, task)
	case domain.HeroSMSTaskActive:
		m.processActive(ctx, task)
	case domain.HeroSMSTaskSettling:
		m.processSettlement(ctx, task, false)
	case domain.HeroSMSTaskPurchaseUnknown:
		// There is no provider reconciliation endpoint in the current API.
		// Retain this visible state and, critically, never purchase again. A
		// manual stop may still close a known provider allocation or terminate a
		// purely local unknown row without making it restartable.
		if task.StopRequested {
			if strings.TrimSpace(task.ProviderActivationID) == "" {
				m.finish(ctx, task, domain.HeroSMSTaskStopped, domain.HeroSMSRefundUnknown,
					"购号结果不明，已停止本地任务且不会再次购买")
				return
			}
			m.processSettlement(ctx, task, true)
			return
		}
		m.schedule(ctx, task, domain.HeroSMSTaskPurchaseUnknown,
			m.now().Add(purchaseUnknownRecheckWait), task.LastError)
	default:
		m.logger.Warn("claimed non-runnable HeroSMS number task", "task_id", task.ID, "status", task.Status)
	}
}

func (m *Manager) processWaitingNumber(ctx context.Context, task domain.HeroSMSNumberTask) {
	if task.StopRequested {
		m.finish(ctx, task, domain.HeroSMSTaskStopped, domain.HeroSMSRefundUnknown, "")
		return
	}
	country, err := strconv.Atoi(task.CountryCode)
	if err != nil {
		m.scheduleError(ctx, task, domain.HeroSMSTaskWaitingNumber,
			fmt.Errorf("invalid HeroSMS country %q", task.CountryCode))
		return
	}
	token, err := newToken()
	if err != nil {
		m.scheduleError(ctx, task, domain.HeroSMSTaskWaitingNumber, err)
		return
	}
	purchasing, err := m.store.BeginHeroSMSPurchaseOwned(
		ctx, task.ID, task.LeaseOwner, task.LeaseVersion, token,
	)
	if err != nil {
		m.logger.Error("begin HeroSMS purchase", "task_id", task.ID, "error", err)
		return
	}
	var options persistedTaskOptions
	if len(purchasing.ProviderPayload) != 0 {
		_ = json.Unmarshal(purchasing.ProviderPayload, &options)
	}
	duration := 0
	if purchasing.DurationHours != nil {
		duration = *purchasing.DurationHours
	}
	purchase, purchaseErr := m.client.PurchaseOne(ctx, herosms.PurchaseRequest{
		Service: purchasing.ServiceCode, Country: country, DurationHours: duration,
		MaxPrice: purchasing.MaxPriceAmount, Operator: options.Operator,
		Currency: purchasing.Currency, Ref: token,
		VerificationType: herosms.VerificationType(purchasing.VerificationType),
	})
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	now := m.now()
	if purchaseErr != nil {
		switch {
		case herosms.IsNoNumbers(purchaseErr):
			m.releasePurchase(persistCtx, purchasing, token,
				now.Add(m.retryDelay(purchasing.RetryCount, m.cfg.StockRetryMinimum, m.cfg.StockRetryMaximum)),
				"该服务暂无可用号码，正在继续尝试")
		case errors.Is(purchaseErr, smsbower.ErrPurchaseUnknown),
			errors.Is(purchaseErr, context.Canceled), errors.Is(purchaseErr, context.DeadlineExceeded):
			m.markPurchaseUnknown(persistCtx, purchasing, token, "", nil, purchaseErr)
		default:
			// HeroSMS clients classify transport/5xx/parse failures as unknown.
			// Remaining errors conclusively rejected the request and may retry.
			m.releasePurchase(persistCtx, purchasing, token,
				now.Add(m.retryDelay(purchasing.RetryCount, m.cfg.ErrorRetryMinimum, m.cfg.ErrorRetryMaximum)),
				purchaseErr.Error())
		}
		return
	}
	if strings.TrimSpace(purchase.ActivationID) == "" || strings.TrimSpace(purchase.PhoneNumber) == "" {
		m.markPurchaseUnknown(persistCtx, purchasing, token, purchase.ActivationID, purchase.Raw,
			errors.New("HeroSMS purchase response omitted activation ID or phone number"))
		return
	}
	purchasedAt := purchase.ActivatedAt.UTC()
	if purchasedAt.IsZero() {
		purchasedAt = now
	}
	expiresAt := purchase.ExpiresAt.UTC()
	if expiresAt.IsZero() {
		expiresAt = purchasedAt.Add(m.cfg.DefaultActivationLifetime)
	}
	refundableUntil := purchasedAt.Add(m.cfg.RefundWindow)
	if expiresAt.Before(refundableUntil) {
		refundableUntil = expiresAt
	}
	next := m.nextActiveRun(now, &expiresAt, &refundableUntil, domain.HeroSMSRefundRefundable)
	commitParams := storage.CommitHeroSMSPurchaseParams{
		PurchaseToken: token, ProviderActivationID: strings.TrimSpace(purchase.ActivationID),
		PhoneNumber: strings.TrimSpace(purchase.PhoneNumber), Operator: strings.TrimSpace(purchase.Operator),
		ActivationCost: strings.TrimSpace(purchase.Cost), Currency: normalizedCurrency(purchase.Currency, purchasing.Currency),
		PurchasedAt: purchasedAt, ExpiresAt: &expiresAt, RefundableUntil: &refundableUntil,
		RefundStatus: domain.HeroSMSRefundRefundable, ProviderPayload: validJSONPayload(purchase.Raw),
		NextRunAt: next,
		SupportsContinuation: purchasing.ProductKind == domain.HeroSMSProductActivation &&
			strings.EqualFold(strings.TrimSpace(purchasing.VerificationType), string(herosms.VerificationSMS)) &&
			purchase.CanGetAnotherSMS,
	}
	committed, commitErr := m.store.CommitHeroSMSPurchaseOwned(
		persistCtx, purchasing.ID, purchasing.LeaseOwner, purchasing.LeaseVersion,
		commitParams,
	)
	if commitErr != nil {
		// The provider allocation is certain. If the worker lease expired while
		// the response was in flight (or the commit acknowledgement was lost),
		// recover by the durable purchase token and provider identity. This path
		// can never return the slot to waiting_number.
		var recoverErr error
		committed, recoverErr = m.store.RecoverHeroSMSPurchaseOutcome(
			persistCtx, purchasing.ID, token, commitParams,
		)
		if recoverErr != nil {
			m.logger.Error("recover confirmed HeroSMS purchase", "task_id", purchasing.ID,
				"provider_activation_id", purchase.ActivationID,
				"commit_error", commitErr, "recovery_error", recoverErr)
			return
		}
	}
	if committed.StopRequested || (committed.ExpiresAt != nil && !now.Before(committed.ExpiresAt.UTC())) {
		// A stop can race the provider call without cancelling it. The persisted
		// allocation is immediately woken for its independent settlement worker.
		// Commit storage keeps next_run_at at now when stop_requested is set.
		return
	}
}

func (m *Manager) releasePurchase(
	ctx context.Context,
	task domain.HeroSMSNumberTask,
	token string,
	next time.Time,
	lastError string,
) {
	_, err := m.store.ReleaseHeroSMSPurchaseOwned(
		ctx, task.ID, task.LeaseOwner, task.LeaseVersion, token, next.UTC(), truncateError(lastError),
	)
	if err != nil {
		m.logger.Error("release conclusive HeroSMS purchase", "task_id", task.ID, "error", err)
	}
}

func (m *Manager) markStalePurchaseUnknown(ctx context.Context, task domain.HeroSMSNumberTask) {
	token := strings.TrimSpace(task.PurchaseToken)
	if token == "" {
		token = "unknown-" + strconv.FormatInt(task.ID, 10)
	}
	m.markPurchaseUnknown(ctx, task, token, task.ProviderActivationID, task.ProviderPayload,
		errors.New("purchase lease expired before outcome was persisted"))
}

func (m *Manager) markPurchaseUnknown(
	ctx context.Context,
	task domain.HeroSMSNumberTask,
	token, providerID string,
	payload json.RawMessage,
	cause error,
) {
	lastError := errorText(cause)
	_, err := m.store.MarkHeroSMSPurchaseUnknownOwned(
		ctx, task.ID, task.LeaseOwner, task.LeaseVersion,
		storage.MarkHeroSMSPurchaseUnknownParams{
			PurchaseToken: token, ProviderActivationID: strings.TrimSpace(providerID),
			ProviderPayload: validJSONPayload(payload),
			NextRunAt:       m.now().Add(purchaseUnknownRecheckWait), LastError: lastError,
		},
	)
	if err != nil {
		m.logger.Error("mark HeroSMS purchase unknown", "task_id", task.ID, "error", err)
	}
}

func (m *Manager) processActive(ctx context.Context, task domain.HeroSMSNumberTask) {
	now := m.now()
	if task.StopRequested || expired(task, now) {
		m.processSettlement(ctx, task, true)
		return
	}
	continuable := task.ProductKind == domain.HeroSMSProductActivation && task.SupportsContinuation
	if continuable && (task.ContinuationPendingCount > 0 || task.MessageCount > task.ContinuationCount) {
		// Persisting the target before status=3 distinguishes a fresh provider
		// rejection from recovery after an outcome-unknown request.
		m.continueActivation(ctx, task)
		return
	}
	if task.WebhookWakeupAt != nil {
		// Rental callbacks need no status=3 transition. A duplicate activation
		// callback may also leave no continuation delta. Consume either wake
		// without reading messages back from the provider.
		m.schedule(ctx, task, domain.HeroSMSTaskActive,
			m.nextActiveRun(now, task.ExpiresAt, task.RefundableUntil, task.RefundStatus), "")
		return
	}
	messages, err := m.client.GetMessages(
		ctx, task.ProviderActivationID, task.ProductKind == domain.HeroSMSProductRent,
	)
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	if err != nil {
		m.schedulePollError(persistCtx, task, err)
		return
	}
	for _, message := range messages {
		code := strings.TrimSpace(message.Code)
		text := strings.TrimSpace(message.Text)
		if code == "" && text == "" {
			continue
		}
		var receivedAt *time.Time
		if !message.ReceivedAt.IsZero() {
			value := message.ReceivedAt.UTC()
			receivedAt = &value
		}
		taskID := task.ID
		result, appendErr := m.store.AppendHeroSMSTaskMessage(
			persistCtx,
			storage.AppendHeroSMSTaskMessageParams{
				TaskID: &taskID, ProviderActivationID: task.ProviderActivationID,
				ProviderMessageID: strings.TrimSpace(message.ID), Source: domain.HeroSMSMessagePoll,
				Code: code, Text: text, ProviderReceivedAt: receivedAt,
				RawPayload: validJSONPayload(message.Raw),
			},
		)
		if appendErr != nil {
			m.scheduleError(persistCtx, task, domain.HeroSMSTaskActive, appendErr)
			return
		}
		if result.Task != nil {
			task.Status = result.Task.Status
			task.MessageCount = result.Task.MessageCount
			task.ContinuationCount = result.Task.ContinuationCount
			task.ContinuationPendingCount = result.Task.ContinuationPendingCount
			task.SupportsContinuation = result.Task.SupportsContinuation
			task.StopRequested = result.Task.StopRequested
			task.RefundStatus = result.Task.RefundStatus
			task.RefundableUntil = result.Task.RefundableUntil
			task.ExpiresAt = result.Task.ExpiresAt
			task.WebhookWakeupAt = result.Task.WebhookWakeupAt
		} else if result.Inserted {
			// Production storage always returns the attached task. Keeping this
			// fallback makes alternate stores retain continuation semantics.
			task.MessageCount++
			task.RefundStatus = domain.HeroSMSRefundUnavailable
		}
		if task.StopRequested || expired(task, m.now()) {
			// A stop can race a blocking provider read. The append returns the
			// latest task row, so honor that durable signal before status=3 can
			// request another message cycle.
			m.processSettlement(ctx, task, true)
			return
		}
	}
	continuable = task.ProductKind == domain.HeroSMSProductActivation && task.SupportsContinuation
	if continuable && (task.ContinuationPendingCount > 0 || task.MessageCount > task.ContinuationCount) {
		m.continueActivation(ctx, task)
		return
	}
	m.schedule(persistCtx, task, domain.HeroSMSTaskActive,
		m.nextActiveRun(now, task.ExpiresAt, task.RefundableUntil, task.RefundStatus), "")
}

func (m *Manager) continueActivation(ctx context.Context, task domain.HeroSMSNumberTask) {
	now := m.now()
	if task.StopRequested || expired(task, now) {
		m.processSettlement(ctx, task, true)
		return
	}
	if task.ProductKind != domain.HeroSMSProductActivation || !task.SupportsContinuation {
		return
	}
	recovering := task.ContinuationPendingCount > 0
	if !recovering && task.MessageCount <= task.ContinuationCount {
		return
	}
	target := task.ContinuationPendingCount
	if !recovering {
		begun, err := m.store.BeginHeroSMSContinuationOwned(
			ctx, task.ID, task.LeaseOwner, task.LeaseVersion, now,
		)
		if err != nil {
			m.logger.Error("begin HeroSMS activation continuation", "task_id", task.ID, "error", err)
			// RequestStop deliberately keeps an active worker lease so a provider
			// read already in flight can finish. If stop wins the Begin CAS, release
			// that lease immediately; storage preserves next_run_at=now for stop.
			m.schedule(ctx, task, domain.HeroSMSTaskActive, now, "")
			return
		}
		task = begun
		target = task.ContinuationPendingCount
		if target <= task.ContinuationCount || target > task.MessageCount {
			m.logger.Error("begin HeroSMS activation continuation returned invalid target",
				"task_id", task.ID, "target", target,
				"continuation_count", task.ContinuationCount, "message_count", task.MessageCount)
			return
		}
		if expired(task, m.now()) {
			// Begin persisted an intent, but no provider request has been made yet.
			// Clear that fresh intent and leave the now-due task for settlement.
			_, abortErr := m.store.AbortHeroSMSContinuationOwned(
				ctx, task.ID, task.LeaseOwner, task.LeaseVersion,
				target, m.now(), "号码在续收请求前已到期",
			)
			if abortErr != nil {
				m.logger.Error("abort expired HeroSMS activation continuation", "task_id", task.ID,
					"target", target, "error", abortErr)
			}
			return
		}
	}
	requestErr := m.client.RequestAnother(ctx, task.ProviderActivationID)
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	outcome := classifyContinuationRequest(requestErr)
	if recovering {
		if outcome == continuationRequestApplied || outcome == continuationRequestBadStatus {
			m.completeContinuation(persistCtx, task, target)
			return
		}
		// Once an intent is pending, every non-BAD_STATUS error is uncertain:
		// aborting could replay a provider transition which was already applied.
		m.scheduleError(persistCtx, task, domain.HeroSMSTaskActive, requestErr)
		return
	}
	switch outcome {
	case continuationRequestApplied:
		m.completeContinuation(persistCtx, task, target)
	case continuationRequestBadStatus, continuationRequestRejected:
		next := m.nextActiveErrorRun(task, m.now())
		_, err := m.store.AbortHeroSMSContinuationOwned(
			persistCtx, task.ID, task.LeaseOwner, task.LeaseVersion,
			target, next, errorText(requestErr),
		)
		if err != nil {
			m.logger.Error("abort HeroSMS activation continuation", "task_id", task.ID,
				"target", target, "error", err)
		}
	case continuationRequestAmbiguous:
		m.scheduleError(persistCtx, task, domain.HeroSMSTaskActive, requestErr)
	}
}

func (m *Manager) completeContinuation(
	ctx context.Context,
	task domain.HeroSMSNumberTask,
	target int,
) {
	next := m.nextActiveRun(m.now(), task.ExpiresAt, task.RefundableUntil, task.RefundStatus)
	_, err := m.store.CompleteHeroSMSContinuationOwned(
		ctx, task.ID, task.LeaseOwner, task.LeaseVersion, target, next,
	)
	if err != nil {
		m.logger.Error("complete HeroSMS activation continuation", "task_id", task.ID,
			"target", target, "error", err)
	}
}

type continuationRequestOutcome uint8

const (
	continuationRequestApplied continuationRequestOutcome = iota
	continuationRequestBadStatus
	continuationRequestRejected
	continuationRequestAmbiguous
)

func classifyContinuationRequest(err error) continuationRequestOutcome {
	if err == nil {
		return continuationRequestApplied
	}
	if smsbower.IsAPIError(err, "BAD_STATUS") {
		return continuationRequestBadStatus
	}
	var apiErr *smsbower.APIError
	if !errors.As(err, &apiErr) {
		// Transport, context and response parsing errors do not prove whether
		// status=3 reached or was applied by the provider.
		return continuationRequestAmbiguous
	}
	code := strings.ToUpper(strings.TrimSpace(apiErr.Code))
	if code == "" || code == "EMPTY_RESPONSE" {
		return continuationRequestAmbiguous
	}
	if strings.HasPrefix(code, "HTTP_") {
		status, parseErr := strconv.Atoi(strings.TrimPrefix(code, "HTTP_"))
		if parseErr != nil || status >= 500 {
			return continuationRequestAmbiguous
		}
		return continuationRequestRejected
	}
	for _, prefix := range []string{"SERVER_", "INTERNAL_", "EXCEPTION_", "ERROR"} {
		if strings.HasPrefix(code, prefix) {
			return continuationRequestAmbiguous
		}
	}
	return continuationRequestRejected
}

func (m *Manager) processSettlement(ctx context.Context, task domain.HeroSMSNumberTask, prepare bool) {
	now := m.now()
	settling := task
	var err error
	if prepare {
		settling, err = m.store.PrepareHeroSMSTaskSettlementOwned(
			ctx, task.ID, task.LeaseOwner, task.LeaseVersion, now,
		)
		if err != nil {
			m.logger.Error("prepare HeroSMS task settlement", "task_id", task.ID, "error", err)
			return
		}
	}
	if strings.TrimSpace(settling.ProviderActivationID) == "" {
		m.finish(ctx, settling, domain.HeroSMSTaskStopped, domain.HeroSMSRefundUnknown, "")
		return
	}
	rent := settling.ProductKind == domain.HeroSMSProductRent
	refund := settling.RefundStatus == domain.HeroSMSRefundRequested
	if expired(settling, now) && !refund {
		// Expiry is a local terminal boundary. The provider cleanup is best-effort:
		// retaining a settling retry after the number has expired would keep the
		// task alive forever when the provider is unavailable. A durable Cancel
		// intent remains authoritative even after expiry because changing it to
		// Finish could lose an uncertain refund result.
		cleanupErr := m.client.Finish(ctx, settling.ProviderActivationID, rent)
		persistCtx, cancel := persistenceContext(ctx)
		defer cancel()
		lastError := ""
		if cleanupErr != nil {
			lastError = "供应商到期清理失败: " + errorText(cleanupErr)
		}
		m.finish(persistCtx, settling,
			domain.HeroSMSTaskExpired, domain.HeroSMSRefundUnavailable, lastError)
		return
	}
	if refund {
		err = m.client.Cancel(ctx, settling.ProviderActivationID, rent)
	} else {
		err = m.client.Finish(ctx, settling.ProviderActivationID, rent)
	}
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	finalOutcome := classifyProviderFinalization(err)
	if finalOutcome == providerFinalizationRetry {
		// Keep the durable settlement action. A transport error may mean the
		// provider applied it, so changing Cancel into Finish would be unsafe.
		m.schedule(persistCtx, settling, domain.HeroSMSTaskSettling,
			m.now().Add(m.retryDelay(settling.RetryCount, m.cfg.ErrorRetryMinimum, m.cfg.ErrorRetryMaximum)),
			err.Error())
		return
	}
	if refund && (finalOutcome == providerFinalizationApplied || finalOutcome == providerFinalizationCancelled) {
		m.finish(persistCtx, settling, domain.HeroSMSTaskRefunded, domain.HeroSMSRefunded, "")
		return
	}
	// Missing/already-finished does not prove that a requested cancellation
	// actually produced a refund. Expose the conservative settled outcome.
	if refund {
		m.finish(persistCtx, settling, domain.HeroSMSTaskSettled, domain.HeroSMSRefundSettled,
			"供应商号码已结束，未确认退款")
		return
	}
	m.finish(persistCtx, settling, domain.HeroSMSTaskSettled, domain.HeroSMSRefundSettled, "")
}

type providerFinalizationOutcome uint8

const (
	providerFinalizationRetry providerFinalizationOutcome = iota
	providerFinalizationApplied
	providerFinalizationCancelled
	providerFinalizationFinished
	providerFinalizationMissing
)

func classifyProviderFinalization(err error) providerFinalizationOutcome {
	if err == nil {
		return providerFinalizationApplied
	}
	if smsbower.IsAPIError(err, "STATUS_CANCEL") {
		return providerFinalizationCancelled
	}
	if smsbower.IsAPIError(err, "STATUS_FINISH") {
		return providerFinalizationFinished
	}
	if smsbower.IsAPIError(err, "BAD_STATUS") {
		// Settlement intent is persisted before the provider call. BAD_STATUS on
		// either the first attempt or crash recovery therefore means the number
		// is already in another terminal state, not that retry can make progress.
		return providerFinalizationFinished
	}
	for _, code := range []string{"NO_ACTIVATION", "ACTIVATION_NOT_FOUND", "NOT_FOUND"} {
		if smsbower.IsAPIError(err, code) {
			return providerFinalizationMissing
		}
	}
	return providerFinalizationRetry
}

func (m *Manager) finish(
	ctx context.Context,
	task domain.HeroSMSNumberTask,
	status domain.HeroSMSNumberTaskStatus,
	refund domain.HeroSMSRefundStatus,
	lastError string,
) {
	_, err := m.store.FinishHeroSMSTaskOwned(
		ctx, task.ID, task.LeaseOwner, task.LeaseVersion, status, refund, truncateError(lastError),
	)
	if status == domain.HeroSMSTaskRefunded && errors.Is(err, storage.ErrConflict) {
		// A callback can land after the remote cancel intent was persisted. The
		// message makes the task non-refundable locally; retain it and converge
		// to settled instead of repeatedly reporting a refund.
		_, settleErr := m.store.FinishHeroSMSTaskOwned(
			ctx, task.ID, task.LeaseOwner, task.LeaseVersion,
			domain.HeroSMSTaskSettled, domain.HeroSMSRefundSettled,
			"验证码在退款结算期间到达，已按不可退款结算",
		)
		if settleErr == nil {
			return
		}
		err = errors.Join(err, settleErr)
	}
	if err != nil {
		m.logger.Error("finish HeroSMS number task", "task_id", task.ID, "status", status, "error", err)
	}
}

func (m *Manager) scheduleError(
	ctx context.Context,
	task domain.HeroSMSNumberTask,
	status domain.HeroSMSNumberTaskStatus,
	cause error,
) {
	now := m.now()
	next := now.Add(m.retryDelay(task.RetryCount, m.cfg.ErrorRetryMinimum, m.cfg.ErrorRetryMaximum))
	if status == domain.HeroSMSTaskActive {
		next = m.nextActiveErrorRun(task, now)
	}
	m.schedule(ctx, task, status, next, errorText(cause))
}

func (m *Manager) nextActiveErrorRun(task domain.HeroSMSNumberTask, now time.Time) time.Time {
	next := now.Add(m.retryDelay(task.RetryCount, m.cfg.ErrorRetryMinimum, m.cfg.ErrorRetryMaximum))
	next = earlier(next, task.ExpiresAt)
	if task.RefundStatus == domain.HeroSMSRefundRefundable && task.RefundableUntil != nil &&
		now.Before(task.RefundableUntil.UTC()) {
		next = earlier(next, task.RefundableUntil)
	}
	return next
}

func (m *Manager) schedulePollError(
	ctx context.Context,
	task domain.HeroSMSNumberTask,
	cause error,
) {
	now := m.now()
	delay := m.retryDelay(task.RetryCount, m.cfg.ErrorRetryMinimum, m.cfg.ErrorRetryMaximum)
	if delay < m.cfg.PollInterval {
		delay = m.cfg.PollInterval
	}
	next := earlier(now.Add(delay), task.ExpiresAt)
	if task.RefundStatus == domain.HeroSMSRefundRefundable && task.RefundableUntil != nil &&
		now.Before(task.RefundableUntil.UTC()) {
		next = earlier(next, task.RefundableUntil)
	}
	m.schedule(ctx, task, domain.HeroSMSTaskActive, next, errorText(cause))
}

func (m *Manager) schedule(
	ctx context.Context,
	task domain.HeroSMSNumberTask,
	status domain.HeroSMSNumberTaskStatus,
	next time.Time,
	lastError string,
) {
	_, err := m.store.ScheduleHeroSMSTaskOwned(
		ctx, task.ID, task.LeaseOwner, task.LeaseVersion, status, next.UTC(), truncateError(lastError),
	)
	if err != nil {
		m.logger.Error("schedule HeroSMS number task", "task_id", task.ID, "status", status, "error", err)
	}
}

func (m *Manager) nextActiveRun(
	now time.Time,
	expiresAt, refundableUntil *time.Time,
	refundStatus domain.HeroSMSRefundStatus,
) time.Time {
	// Authenticated webhooks wake tasks immediately. This low-frequency read is
	// the fallback for callbacks which were delayed or never delivered.
	next := now.Add(m.cfg.PollInterval)
	next = earlier(next, expiresAt)
	if refundStatus == domain.HeroSMSRefundRefundable && refundableUntil != nil && now.Before(refundableUntil.UTC()) {
		next = earlier(next, refundableUntil)
	}
	return next
}

func (m *Manager) retryDelay(retryCount int, minimum, maximum time.Duration) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	delay := minimum
	for attempt := 0; attempt < retryCount && delay < maximum; attempt++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func (m *Manager) now() time.Time { return m.cfg.Now().UTC() }

func expired(task domain.HeroSMSNumberTask, now time.Time) bool {
	return task.ExpiresAt != nil && !now.Before(task.ExpiresAt.UTC())
}

func earlier(candidate time.Time, limit *time.Time) time.Time {
	if limit != nil && !limit.IsZero() && limit.UTC().Before(candidate) {
		return limit.UTC()
	}
	return candidate
}

func persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func normalizedCurrency(providerValue, requested string) string {
	if value := strings.ToUpper(strings.TrimSpace(providerValue)); value != "" {
		return value
	}
	return strings.ToUpper(strings.TrimSpace(requested))
}

func normalizedOptional(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func validJSONPayload(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return json.RawMessage(`{}`)
	}
	if json.Valid(payload) {
		return append(json.RawMessage(nil), payload...)
	}
	wrapped, _ := json.Marshal(map[string]string{"raw": string(payload)})
	return wrapped
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return truncateError(err.Error())
}

func truncateError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maximumStoredErrorLength {
		return value[:maximumStoredErrorLength]
	}
	return value
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func newToken() (string, error) {
	value := make([]byte, 16)
	if _, err := cryptorand.Read(value); err != nil {
		return "", fmt.Errorf("generate HeroSMS purchase token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func bestEffortToken() string {
	token, err := newToken()
	if err == nil {
		return token
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}
