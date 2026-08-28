package workflow

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
	"github.com/mangobubu/gopay-autosms/internal/herosms"
	proxyaddr "github.com/mangobubu/gopay-autosms/internal/proxy"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/smsprovider"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

const (
	purchaseCleanupWorkerCount  = 4
	loginVerificationCodeWait   = 60 * time.Second
	pinVerificationCodeWait     = 80 * time.Second
	verificationCodeResends     = 3
	verificationCheckpointSaves = 3
	verificationCheckpointRetry = 25 * time.Millisecond
)

type Config struct {
	PollInterval  time.Duration
	ActivationTTL time.Duration
	// LoginStatusTTL bounds how long a remote GoPay profile result is reused.
	// Keep the default below the UI's five-second interval so timer and request
	// latency cannot stretch normal remote probes to every other browser poll.
	LoginStatusTTL time.Duration
	SchedulerTick  time.Duration
	LeaseDuration  time.Duration
	WorkerCount    int
	SSOBaseURL     string
	GoPayBaseURL   string
}

type BatchOptions struct {
	SMSProvider       string      `json:"sms_provider,omitempty"`
	ProviderIDs       []int64     `json:"provider_ids,omitempty"`
	ExceptProviderIDs []int64     `json:"except_provider_ids,omitempty"`
	MinPrice          string      `json:"min_price,omitempty"`
	ProxyPool         []ProxySlot `json:"proxy_pool,omitempty"`
	GoPayCountryCode  string      `json:"gopay_country_code,omitempty"`
}

type ProxySlot struct {
	ID        int    `json:"id"`
	Encrypted string `json:"encrypted"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

type CreateBatchInput struct {
	ServiceCode string
	ServiceName string
	CountryCode string
	CountryName string
	MaxPrice    string
	Currency    string
	Quantity    int
	PIN         string
	Options     BatchOptions
	Proxies     []proxyaddr.Entry
}

// HeroSMSWebhookPayload is the provider callback after the HTTP boundary has
// authenticated and decoded it. Pointer fields retain HeroSMS's documented
// null values; RawPayload preserves the exact callback for durable auditing.
type HeroSMSWebhookPayload struct {
	ActivationID string
	PhoneFrom    string
	Service      string
	Text         *string
	Code         *string
	Country      int
	ReceivedAt   time.Time
	RawPayload   json.RawMessage
}

type Manager struct {
	store    storage.Store
	settings *appsettings.Manager
	box      *secure.Box
	cfg      Config
	logger   *slog.Logger
	owner    string

	startOnce   sync.Once
	startErr    error
	wg          sync.WaitGroup
	sem         chan struct{}
	cleanupSem  chan struct{}
	purchase    sync.Mutex
	workerMu    sync.Mutex
	workers     map[int64]map[int64]activationWorker
	proxyMu     sync.Mutex
	activeProxy map[int64]map[string]struct{}

	loginStatusMu       sync.Mutex
	loginStatusCache    map[int64]loginStatusCacheEntry
	loginStatusFlights  map[int64]*loginStatusFlight
	accountSessionMu    sync.Mutex
	accountSessionLocks map[string]*accountSessionLockEntry
}

type accountSessionLockEntry struct {
	gate chan struct{}
	refs int
}

type activationWorker struct {
	leaseVersion int64
	cancel       context.CancelFunc
}

func New(store storage.Store, settings *appsettings.Manager, box *secure.Box, cfg Config, logger *slog.Logger) *Manager {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.ActivationTTL <= 0 {
		cfg.ActivationTTL = 20 * time.Minute
	}
	if cfg.LoginStatusTTL <= 0 {
		cfg.LoginStatusTTL = 4 * time.Second
	}
	if cfg.SchedulerTick <= 0 {
		cfg.SchedulerTick = 500 * time.Millisecond
	}
	if cfg.LeaseDuration <= 0 {
		// A PIN reset is a sequence of several independently signed remote
		// requests, each with its own network timeout. Keep one worker fenced for
		// the complete step instead of allowing a second worker to overlap it.
		cfg.LeaseDuration = 3 * time.Minute
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 8
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		store: store, settings: settings, box: box, cfg: cfg, logger: logger,
		owner:               fmt.Sprintf("autosms-%d", time.Now().UnixNano()),
		sem:                 make(chan struct{}, cfg.WorkerCount),
		cleanupSem:          make(chan struct{}, purchaseCleanupWorkerCount),
		workers:             make(map[int64]map[int64]activationWorker),
		activeProxy:         make(map[int64]map[string]struct{}),
		loginStatusCache:    make(map[int64]loginStatusCacheEntry),
		loginStatusFlights:  make(map[int64]*loginStatusFlight),
		accountSessionLocks: make(map[string]*accountSessionLockEntry),
	}
}

func (m *Manager) Run(ctx context.Context) error {
	m.startOnce.Do(func() {
		m.startErr = m.recoverStartupBatches(ctx)
		if m.startErr != nil {
			return
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.loop(ctx)
		}()
	})
	return m.startErr
}

func (m *Manager) Wait() { m.wg.Wait() }

// ReceiveHeroSMSWebhook durably records every authenticated callback before
// returning to the HTTP handler. Ingestion also wakes a matching activation;
// callbacks which beat activation creation remain unattached in the inbox and
// are associated when the worker later claims them.
func (m *Manager) ReceiveHeroSMSWebhook(ctx context.Context, payload HeroSMSWebhookPayload) error {
	inbox, ok := m.store.(storage.HeroSMSWebhookStore)
	if !ok {
		return fmt.Errorf("HeroSMS webhook inbox is not configured")
	}
	providerReceivedAt := payload.ReceivedAt.UTC()
	_, err := inbox.IngestHeroSMSWebhook(ctx, storage.IngestHeroSMSWebhookParams{
		ProviderActivationID: strings.TrimSpace(payload.ActivationID),
		Code:                 cloneOptionalString(payload.Code),
		Text:                 cloneOptionalString(payload.Text),
		PhoneNumber:          strings.TrimSpace(payload.PhoneFrom),
		ServiceCode:          strings.TrimSpace(payload.Service),
		CountryCode:          strconv.Itoa(payload.Country),
		ProviderReceivedAt:   &providerReceivedAt,
		RawPayload:           append(json.RawMessage(nil), payload.RawPayload...),
	})
	return err
}

func cloneOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (m *Manager) CreateBatch(ctx context.Context, input CreateBatchInput) (domain.Batch, error) {
	if err := domain.ValidatePIN(input.PIN); err != nil {
		return domain.Batch{}, err
	}
	if input.Quantity <= 0 || input.Quantity > domain.MaxBatchQuantity ||
		strings.TrimSpace(input.ServiceCode) == "" || strings.TrimSpace(input.CountryCode) == "" || strings.TrimSpace(input.MaxPrice) == "" {
		return domain.Batch{}, storage.ErrInvalidInput
	}
	provider, err := smsprovider.Normalize(input.Options.SMSProvider)
	if err != nil {
		return domain.Batch{}, fmt.Errorf("%w: %v", storage.ErrInvalidInput, err)
	}
	input.Options.SMSProvider = provider
	if err = m.validateSMSProviderKey(ctx, provider); err != nil {
		return domain.Batch{}, err
	}
	for _, entry := range input.Proxies {
		ciphertext, sealErr := m.box.Seal([]byte(entry.URL))
		if sealErr != nil {
			return domain.Batch{}, sealErr
		}
		input.Options.ProxyPool = append(input.Options.ProxyPool, ProxySlot{
			ID: entry.ID, Encrypted: base64.StdEncoding.EncodeToString(ciphertext), Status: "available",
		})
	}
	options, err := json.Marshal(input.Options)
	if err != nil {
		return domain.Batch{}, err
	}
	pinEncrypted, err := m.box.Seal([]byte(input.PIN))
	if err != nil {
		return domain.Batch{}, err
	}
	batch, err := m.store.CreateBatch(ctx, storage.CreateBatchParams{
		ServiceCode: input.ServiceCode, ServiceName: input.ServiceName,
		CountryCode: input.CountryCode, CountryName: input.CountryName,
		MaxPriceAmount: input.MaxPrice, Currency: input.Currency, Quantity: input.Quantity,
		TargetPINEnc: pinEncrypted, Config: options,
	})
	if err != nil {
		return domain.Batch{}, err
	}
	return batch, nil
}

func (m *Manager) validateSMSProviderKey(ctx context.Context, provider string) error {
	switch provider {
	case smsprovider.SMSBower:
		cfg, err := m.settings.GetSMSBower(ctx)
		if err != nil {
			return err
		}
		if strings.TrimSpace(cfg.APIKey) == "" {
			return fmt.Errorf("SMSBower API Key 尚未配置")
		}
	case smsprovider.HeroSMS:
		cfg, err := m.settings.GetHeroSMS(ctx)
		if err != nil {
			return err
		}
		if strings.TrimSpace(cfg.APIKey) == "" {
			return fmt.Errorf("HeroSMS API Key 尚未配置")
		}
	default:
		return fmt.Errorf("%w: invalid sms_provider %q", storage.ErrInvalidInput, provider)
	}
	return nil
}

func (m *Manager) claimProxy(ctx context.Context, batch domain.Batch) (string, error) {
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	latest, err := m.store.GetBatch(ctx, batch.ID)
	if err != nil {
		return "", err
	}
	var options BatchOptions
	if err := json.Unmarshal(latest.Config, &options); err != nil {
		return "", err
	}
	m.hydrateActiveProxies(ctx, batch.ID)
	active := m.activeProxy[batch.ID]
	available := make([]int, 0, len(options.ProxyPool))
	decodedURLs := make([]string, len(options.ProxyPool))
	for index, entry := range options.ProxyPool {
		decoded, decodeErr := m.openProxySlot(entry)
		if decodeErr != nil {
			continue
		}
		decodedURLs[index] = decoded
		if entry.Status == "" || entry.Status == "available" {
			if _, busy := active[decoded]; busy {
				continue
			}
			available = append(available, index)
		}
	}
	if len(options.ProxyPool) == 0 {
		return "", nil // direct mode when no pool was supplied
	}
	if len(available) == 0 {
		return "", fmt.Errorf("%w: 0/%d", proxyaddr.ErrExhausted, len(options.ProxyPool))
	}
	choice := available[0]
	var raw [8]byte
	if _, readErr := cryptorand.Read(raw[:]); readErr == nil {
		choice = available[int(binary.BigEndian.Uint64(raw[:])%uint64(len(available)))]
	}
	selectedURL := decodedURLs[choice]
	// A repeated input line remains visible in the total count, but represents
	// the same endpoint. Once that endpoint is claimed, retire every duplicate
	// slot so it can never be assigned to a second number later.
	for index := range options.ProxyPool {
		if decodedURLs[index] == selectedURL {
			options.ProxyPool[index].Status = "used"
			options.ProxyPool[index].Error = ""
		}
	}
	updated, err := json.Marshal(options)
	if err != nil {
		return "", err
	}
	if err := m.store.UpdateBatchConfig(ctx, batch.ID, updated); err != nil {
		return "", err
	}
	if m.activeProxy[batch.ID] == nil {
		m.activeProxy[batch.ID] = make(map[string]struct{})
	}
	m.activeProxy[batch.ID][selectedURL] = struct{}{}
	return selectedURL, nil
}

func (m *Manager) hydrateActiveProxies(ctx context.Context, batchID int64) {
	if m.activeProxy[batchID] == nil {
		m.activeProxy[batchID] = make(map[string]struct{})
	}
	activations, err := m.store.ListActivations(ctx, storage.ActivationFilter{
		BatchID: &batchID, IncludeHidden: true, Page: storage.Page{Limit: 500},
	})
	if err != nil {
		return
	}
	for _, activation := range activations {
		if activation.FinishedAt != nil {
			continue
		}
		account, accountErr := m.store.GetAccountByPhone(ctx, activation.PhoneNumber)
		if accountErr != nil {
			continue
		}
		raw, openErr := m.box.Open(account.CredentialsEnc)
		if openErr != nil {
			continue
		}
		session, parseErr := gopay.ParseSession(raw)
		if parseErr == nil && session.ProxyURL != "" {
			m.activeProxy[batchID][session.ProxyURL] = struct{}{}
		}
	}
}

func (m *Manager) releaseProxy(batchID int64, proxyURL string) {
	if proxyURL == "" {
		return
	}
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	delete(m.activeProxy[batchID], proxyURL)
}

func (m *Manager) openProxySlot(slot ProxySlot) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(slot.Encrypted)
	if err != nil {
		return "", err
	}
	plain, err := m.box.Open(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (m *Manager) markProxyFailure(ctx context.Context, batchID int64, proxyURL string, cause error) {
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	batch, err := m.store.GetBatch(ctx, batchID)
	if err != nil {
		return
	}
	var options BatchOptions
	if json.Unmarshal(batch.Config, &options) != nil {
		return
	}
	for index := range options.ProxyPool {
		decoded, _ := m.openProxySlot(options.ProxyPool[index])
		if decoded == proxyURL {
			options.ProxyPool[index].Status = "used"
			options.ProxyPool[index].Error = cause.Error()
		}
	}
	if updated, marshalErr := json.Marshal(options); marshalErr == nil {
		_ = m.store.UpdateBatchConfig(ctx, batchID, updated)
	}
}

func (m *Manager) MarkSuccess(ctx context.Context, activationID int64) error {
	activation, err := m.store.GetActivation(ctx, activationID)
	if err != nil {
		return err
	}
	if activation.Status != domain.ActivationStatusActive && activation.Status != domain.ActivationStatusAwaitingSubsequentCode {
		return fmt.Errorf("%w: only numbers waiting for subsequent codes can be marked successful", storage.ErrConflict)
	}
	return m.store.SetControlAction(ctx, activationID, domain.ControlActionSuccess)
}

func (m *Manager) Delete(ctx context.Context, activationID int64) error {
	activation, err := m.store.GetActivation(ctx, activationID)
	if err != nil {
		return err
	}
	if activation.Status.Terminal() {
		return m.store.HideActivation(ctx, activationID)
	}
	return m.store.SetControlAction(ctx, activationID, domain.ControlActionDelete)
}

// StopBatch first persists the durable delete actions and fences every claimed
// activation in storage. Holding workerMu across that transaction closes the
// gap where the scheduler could otherwise claim an old workflow after the
// cancellation snapshot was taken. In-process workers are then interrupted so
// the newly queued delete actions can be claimed without waiting for a remote
// request timeout.
func (m *Manager) StopBatch(ctx context.Context, batchID int64) (domain.Batch, error) {
	// Let an already-sent GetNumber attempt reach a conclusive response before
	// cancellation. This avoids turning a known allocation into an unknown one;
	// no subsequent purchase can start while this gate is held.
	m.purchase.Lock()
	defer m.purchase.Unlock()

	m.workerMu.Lock()
	defer m.workerMu.Unlock()
	if m.workers == nil {
		m.workers = make(map[int64]map[int64]activationWorker)
	}

	batch, err := m.cancelBatchPersistently(ctx, batchID)
	if err != nil {
		return domain.Batch{}, err
	}
	for _, worker := range m.workers[batchID] {
		worker.cancel()
	}
	delete(m.workers, batchID)
	return batch, nil
}

func (m *Manager) cancelBatchPersistently(ctx context.Context, batchID int64) (domain.Batch, error) {
	var batch domain.Batch
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		attemptCtx := ctx
		cancel := func() {}
		if attempt > 0 {
			attemptCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		}
		batch, err = m.store.CancelBatch(attemptCtx, batchID)
		cancel()
		if err == nil || (!errors.Is(err, storage.ErrCommitUnknown) && !errors.Is(err, storage.ErrRetryable)) {
			return batch, err
		}
	}
	return batch, err
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
	m.processPurchaseCleanups(ctx)
	m.purchase.Lock()
	if batches, err := m.store.ListBatches(ctx, storage.BatchFilter{
		Statuses: []domain.BatchStatus{domain.BatchStatusPending, domain.BatchStatusRunning}, Page: storage.Page{Limit: 500},
	}); err != nil {
		m.logger.Error("list purchase batches", "error", err)
	} else {
		for _, batch := range batches {
			if batchProxyPoolExhausted(batch) {
				failErr := m.failBatchForExhaustedProxies(ctx, batch.ID)
				if failErr != nil && !errors.Is(failErr, storage.ErrConflict) {
					m.logger.Error("fail batch with exhausted proxy pool", "batch_id", batch.ID, "error", failErr)
				}
				continue
			}
			if batchReadyForPurchase(batch, time.Now()) {
				m.purchaseBatch(ctx, batch)
			}
		}
	}
	m.purchase.Unlock()

	available := cap(m.sem) - len(m.sem)
	if available <= 0 {
		return
	}
	// Claiming a lease and registering its cancellation handle must be one
	// critical section. Otherwise StopBatch could fence the lease while the
	// activation is still between ClaimRunnableActivations and registration,
	// allowing a stale worker to be launched after the stop has returned.
	type pendingWorker struct {
		activation domain.Activation
		ctx        context.Context
		cancel     context.CancelFunc
	}
	pending := make([]pendingWorker, 0, available)
	m.workerMu.Lock()
	activations, err := m.store.ClaimRunnableActivations(ctx, m.owner, time.Now().UTC(), m.cfg.LeaseDuration, available)
	if err != nil {
		m.workerMu.Unlock()
		m.logger.Error("claim activations", "error", err)
		return
	}
	for _, activation := range activations {
		workerCtx, cancel := context.WithCancel(ctx)
		if m.workers == nil {
			m.workers = make(map[int64]map[int64]activationWorker)
		}
		if m.workers[activation.BatchID] == nil {
			m.workers[activation.BatchID] = make(map[int64]activationWorker)
		}
		if previous, ok := m.workers[activation.BatchID][activation.ID]; ok {
			previous.cancel()
		}
		m.workers[activation.BatchID][activation.ID] = activationWorker{
			leaseVersion: activation.LeaseVersion,
			cancel:       cancel,
		}
		pending = append(pending, pendingWorker{activation: activation, ctx: workerCtx, cancel: cancel})
	}
	m.workerMu.Unlock()
	for _, item := range pending {
		activation := item.activation
		workerCtx := item.ctx
		cancel := item.cancel
		select {
		case m.sem <- struct{}{}:
		case <-workerCtx.Done():
			cancel()
			m.unregisterWorker(activation.BatchID, activation.ID, activation.LeaseVersion)
			continue
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer func() { <-m.sem }()
			defer cancel()
			defer m.unregisterWorker(activation.BatchID, activation.ID, activation.LeaseVersion)
			m.processActivation(workerCtx, activation)
		}()
	}
}

func (m *Manager) processPurchaseCleanups(ctx context.Context) {
	reservedSlots := 0
reserveSlots:
	for reservedSlots < cap(m.cleanupSem) {
		select {
		case m.cleanupSem <- struct{}{}:
			reservedSlots++
		default:
			break reserveSlots
		}
	}
	if reservedSlots == 0 {
		return
	}
	items, err := m.store.ClaimPurchaseCleanupAttempts(
		ctx, m.owner, time.Now().UTC(), m.cfg.LeaseDuration, reservedSlots,
	)
	if err != nil {
		for range reservedSlots {
			<-m.cleanupSem
		}
		m.logger.Error("claim purchase cleanups", "error", err)
		return
	}
	for range reservedSlots - len(items) {
		<-m.cleanupSem
	}
	for _, item := range items {
		m.wg.Add(1)
		go func(item storage.PurchaseCleanupAttempt) {
			defer m.wg.Done()
			defer func() { <-m.cleanupSem }()
			m.processPurchaseCleanupItem(ctx, item)
		}(item)
	}
}

func (m *Manager) processPurchaseCleanupItem(ctx context.Context, item storage.PurchaseCleanupAttempt) {
	client, err := m.smsClient(ctx, item.Provider)
	if err == nil {
		_, err = client.SetStatus(ctx, item.ProviderActivationID, smsbower.SetStatusCancel)
	}
	if providerActionConcluded(err) {
		if completeErr := m.store.CompletePurchaseCleanup(ctx, item.Token, item.LeaseOwner, item.LeaseVersion); completeErr != nil {
			m.logger.Error("complete purchase cleanup", "token", item.Token, "error", completeErr)
		}
		return
	}
	m.logger.Warn("purchase cleanup failed", "token", item.Token, "provider_id", item.ProviderActivationID, "error", err)
	if retryErr := m.store.RetryPurchaseCleanup(
		ctx, item.Token, item.LeaseOwner, item.LeaseVersion,
		time.Now().UTC().Add(5*time.Second), err.Error(),
	); retryErr != nil {
		m.logger.Error("schedule purchase cleanup retry", "token", item.Token, "error", retryErr)
	}
}

func (m *Manager) unregisterWorker(batchID, activationID, leaseVersion int64) {
	m.workerMu.Lock()
	defer m.workerMu.Unlock()
	batchWorkers := m.workers[batchID]
	worker, ok := batchWorkers[activationID]
	if !ok || worker.leaseVersion != leaseVersion {
		return
	}
	delete(batchWorkers, activationID)
	if len(batchWorkers) == 0 {
		delete(m.workers, batchID)
	}
}

func (m *Manager) failBatchForExhaustedProxies(ctx context.Context, batchID int64) error {
	const reason = "代理池已耗尽，成功数量未达到计划购买数量，任务已停止"
	if store, ok := m.store.(storage.ProxyExhaustionStore); ok {
		_, err := store.FailBatchForExhaustedProxies(ctx, batchID, reason)
		return err
	}
	// Lightweight stores predating the optional atomic operation still get a
	// useful best-effort transition; the production Postgres store takes the
	// guarded path above.
	_, err := m.store.TransitionBatch(ctx, batchID,
		[]domain.BatchStatus{domain.BatchStatusPending, domain.BatchStatusRunning},
		domain.BatchStatusFailed, reason)
	return err
}

func batchReadyForPurchase(batch domain.Batch, now time.Time) bool {
	return !batch.Status.Terminal() &&
		(batch.ProxyTotal == 0 || batch.ProxyAvailable > 0) &&
		batch.PurchaseReservedCount == 0 &&
		batch.FulfilledCount+batch.InflightCount+batch.PurchaseReservedCount < batch.Quantity &&
		!batch.NextPurchaseAt.After(now)
}

func batchProxyPoolExhausted(batch domain.Batch) bool {
	return !batch.Status.Terminal() &&
		batch.ProxyTotal > 0 && batch.ProxyAvailable == 0 &&
		batch.FulfilledCount < batch.Quantity &&
		batch.InflightCount == 0 && batch.PurchaseReservedCount == 0
}

func (m *Manager) purchaseBatch(ctx context.Context, batch domain.Batch) {
	// Re-read under the same gate used by StopBatch. A batch returned by the
	// scheduler query may have been stopped while that query was in flight.
	m.workerMu.Lock()
	latest, err := m.store.GetBatch(ctx, batch.ID)
	if err != nil {
		m.workerMu.Unlock()
		m.logger.Warn("refresh purchase batch", "batch_id", batch.ID, "error", err)
		return
	}
	if !batchReadyForPurchase(latest, time.Now()) {
		m.workerMu.Unlock()
		return
	}
	m.workerMu.Unlock()

	// Never interrupt GetNumber after it has been sent: its outcome may be a
	// remotely allocated number even when the response is lost. StopBatch is
	// serialized by m.purchase and therefore waits for this attempt to either be
	// persisted or explicitly cancelled before it returns.
	m.purchaseOne(ctx, latest)
}

func (m *Manager) smsClient(ctx context.Context, provider string) (smsbower.API, error) {
	provider, err := smsprovider.Normalize(provider)
	if err != nil {
		return nil, err
	}
	switch provider {
	case smsprovider.SMSBower:
		cfg, getErr := m.settings.GetSMSBower(ctx)
		if getErr != nil {
			return nil, getErr
		}
		return smsbower.NewClient(smsbower.Config{APIKey: cfg.APIKey, BaseURL: cfg.BaseURL})
	case smsprovider.HeroSMS:
		cfg, getErr := m.settings.GetHeroSMS(ctx)
		if getErr != nil {
			return nil, getErr
		}
		return herosms.NewClient(herosms.Config{APIKey: cfg.APIKey, BaseURL: cfg.BaseURL})
	default:
		return nil, fmt.Errorf("invalid sms_provider %q", provider)
	}
}

func (m *Manager) purchaseOne(ctx context.Context, batch domain.Batch) {
	var options BatchOptions
	if len(batch.Config) != 0 {
		if err := json.Unmarshal(batch.Config, &options); err != nil {
			m.noteBatchError(ctx, batch, err)
			return
		}
	}
	provider, err := smsprovider.Normalize(options.SMSProvider)
	if err != nil {
		m.noteBatchError(ctx, batch, err)
		return
	}
	client, err := m.smsClient(ctx, provider)
	if err != nil {
		m.noteBatchError(ctx, batch, err)
		return
	}
	country, err := strconv.Atoi(batch.CountryCode)
	if err != nil {
		m.noteBatchError(ctx, batch, fmt.Errorf("invalid country ID %q", batch.CountryCode))
		return
	}
	purchaseToken, err := newPurchaseToken()
	if err != nil {
		m.noteBatchError(ctx, batch, err)
		return
	}
	if err = m.reservePurchase(ctx, batch.ID, purchaseToken); err != nil {
		switch {
		case errors.Is(err, storage.ErrPurchaseInProgress), errors.Is(err, storage.ErrBatchCapacity):
			return
		case errors.Is(err, storage.ErrCommitUnknown):
			reason := "购买名额预占回执未知，供应商请求尚未发送: " + err.Error()
			if releaseErr := m.releasePurchase(ctx, batch.ID, purchaseToken, time.Now().UTC().Add(2*time.Second), reason); releaseErr != nil {
				m.logger.Error("resolve uncertain pre-provider reservation", "batch_id", batch.ID, "error", releaseErr)
			}
			return
		default:
			m.noteBatchError(ctx, batch, err)
			return
		}
	}
	if err = m.markPurchaseSent(ctx, batch.ID, purchaseToken); err != nil {
		if errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) {
			return
		}
		reason := "购号请求发送标记异常，供应商请求尚未发送: " + err.Error()
		if releaseErr := m.releasePurchase(ctx, batch.ID, purchaseToken, time.Now().UTC().Add(2*time.Second), reason); releaseErr != nil {
			m.logger.Error("resolve uncertain purchase send fence", "batch_id", batch.ID, "error", releaseErr)
		}
		return
	}
	purchased, err := client.GetNumber(ctx, smsbower.NumberRequest{
		Service: batch.ServiceCode, Country: country, MinPrice: options.MinPrice,
		MaxPrice: batch.MaxPriceAmount, ProviderIDs: options.ProviderIDs,
		ExceptProviderIDs: options.ExceptProviderIDs, UserID: purchaseToken, Ref: "autosms",
	})
	if err != nil {
		if errors.Is(err, smsbower.ErrPurchaseUnknown) {
			m.logger.Error("SMS provider purchase result is unknown; stopping batch to prevent a duplicate purchase", "batch_id", batch.ID, "provider", provider, "error", err)
			reason := "购买结果未知，已停止自动补购以避免重复扣费: " + err.Error()
			if _, freezeErr := m.freezePurchase(ctx, batch.ID, purchaseToken, provider, "", reason); freezeErr != nil {
				m.logger.Error("freeze unknown purchase", "batch_id", batch.ID, "error", freezeErr)
			}
			return
		}
		m.logger.Warn("purchase attempt failed before allocation", "batch_id", batch.ID, "error", err)
		next := time.Now().UTC().Add(2 * time.Second)
		if releaseErr := m.releasePurchase(ctx, batch.ID, purchaseToken, next, err.Error()); releaseErr != nil {
			reason := "购买预占释放结果未知，已停止自动补购: " + releaseErr.Error()
			m.logger.Error("release failed purchase reservation", "batch_id", batch.ID, "error", releaseErr)
			if _, freezeErr := m.freezePurchase(ctx, batch.ID, purchaseToken, provider, "", reason); freezeErr != nil &&
				!errors.Is(freezeErr, storage.ErrConflict) {
				m.logger.Error("freeze unreleased purchase", "batch_id", batch.ID, "error", freezeErr)
			}
		}
		return
	}
	payload, _ := json.Marshal(purchased)
	expires := time.Now().UTC().Add(m.cfg.ActivationTTL)
	params := storage.CreateActivationParams{
		PurchaseToken: purchaseToken, BatchID: batch.ID, Provider: provider, ProviderActivationID: purchased.ActivationID,
		PhoneNumber: purchased.PhoneNumber, ServiceCode: batch.ServiceCode, CountryCode: batch.CountryCode,
		Operator: purchased.Operator, PurchasePriceAmount: firstNonEmpty(purchased.Cost, batch.MaxPriceAmount),
		Currency: firstNonEmpty(purchased.Currency, batch.Currency), ProviderPayload: payload,
		ProviderExpiresAt: &expires, NextRunAt: time.Now().UTC(),
	}
	_, err = m.persistPurchasedActivation(ctx, params)
	if err != nil {
		reason := fmt.Sprintf("%s 号码 %s 落库异常，已停止自动补购: %v", provider, purchased.ActivationID, err)
		resolvedState, freezeErr := m.freezePurchase(ctx, batch.ID, purchaseToken, provider, purchased.ActivationID, reason)
		if freezeErr != nil {
			m.logger.Error("freeze unpersisted purchase", "batch_id", batch.ID, "provider_id", purchased.ActivationID, "error", freezeErr)
			return
		}
		// Freeze acquires the batch and attempt locks, so an unknown result proves
		// that no activation commit won the race. A committed result must remain
		// active even when the original COMMIT response was lost.
		if resolvedState != storage.PurchaseAttemptUnknown {
			return
		}
		m.logger.Warn("queued durable cleanup for unpersisted number", "provider_id", purchased.ActivationID)
		return
	}
}

func newPurchaseToken() (string, error) {
	var raw [32]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate purchase token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (m *Manager) reservePurchase(ctx context.Context, batchID int64, token string) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		attemptCtx := ctx
		cancel := func() {}
		if attempt > 0 {
			attemptCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		}
		err = m.store.ReserveBatchPurchase(attemptCtx, batchID, token)
		cancel()
		if err == nil || (!errors.Is(err, storage.ErrCommitUnknown) && !errors.Is(err, storage.ErrRetryable)) {
			return err
		}
	}
	return err
}

func (m *Manager) markPurchaseSent(ctx context.Context, batchID int64, token string) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		attemptCtx := ctx
		cancel := func() {}
		if attempt > 0 {
			attemptCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		}
		err = m.store.MarkBatchPurchaseSent(attemptCtx, batchID, token)
		cancel()
		if err == nil || (!errors.Is(err, storage.ErrCommitUnknown) && !errors.Is(err, storage.ErrRetryable)) {
			return err
		}
	}
	return err
}

func (m *Manager) releasePurchase(
	ctx context.Context,
	batchID int64,
	token string,
	next time.Time,
	reason string,
) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resolutionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = m.store.ReleaseBatchPurchaseReservation(resolutionCtx, batchID, token, next, reason)
		cancel()
		if err == nil || (!errors.Is(err, storage.ErrCommitUnknown) && !errors.Is(err, storage.ErrRetryable)) {
			return err
		}
	}
	return err
}

func (m *Manager) freezePurchase(
	ctx context.Context,
	batchID int64,
	token, provider, providerActivationID, reason string,
) (storage.PurchaseAttemptState, error) {
	var state storage.PurchaseAttemptState
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resolutionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		state, err = m.store.FreezeBatchPurchase(
			resolutionCtx, batchID, token, provider, providerActivationID, reason,
		)
		cancel()
		if err == nil || (!errors.Is(err, storage.ErrCommitUnknown) && !errors.Is(err, storage.ErrRetryable)) {
			return state, err
		}
	}
	return state, err
}

func (m *Manager) persistPurchasedActivation(
	ctx context.Context,
	params storage.CreateActivationParams,
) (storage.CreateActivationResult, error) {
	var result storage.CreateActivationResult
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resolutionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		result, err = m.store.CreateActivationAtomically(resolutionCtx, params)
		cancel()
		if err == nil || (!errors.Is(err, storage.ErrCommitUnknown) && !errors.Is(err, storage.ErrRetryable)) {
			return result, err
		}
	}
	return result, err
}

func (m *Manager) noteBatchError(ctx context.Context, batch domain.Batch, err error) {
	m.logger.Warn("purchase attempt failed", "batch_id", batch.ID, "error", err)
	_ = m.store.ScheduleBatchPurchase(ctx, batch.ID, time.Now().UTC().Add(2*time.Second), err.Error())
}

func (m *Manager) processActivation(ctx context.Context, activation domain.Activation) {
	// A status probe can refresh the same account session that this worker is
	// advancing. Serialize them by phone so an older worker snapshot cannot
	// overwrite a freshly rotated token in this process.
	releaseAccount, lockErr := m.acquireAccountSessionLock(ctx, activation.PhoneNumber)
	if lockErr != nil {
		m.logger.Warn("acquire account session lock", "activation_id", activation.ID, "error", lockErr)
		_ = m.store.ReleaseActivationLease(ctx, activation.ID, activation.LeaseOwner, time.Now().UTC().Add(m.cfg.PollInterval))
		return
	}
	defer releaseAccount()

	var err error
	// Once GoPay has consumed the PIN submission and blocked the account, the
	// normal worker path owes the SMS number a provider completion. Explicit
	// user control actions still retain their usual semantics.
	if activation.Status == domain.ActivationStatusPINSubmissionBlocked &&
		activation.ControlAction == domain.ControlActionNone {
		err = m.finalizeProviderAction(ctx, activation, smsbower.SetStatusComplete)
	} else if activation.ControlAction != domain.ControlActionNone {
		err = m.processControl(ctx, activation)
	} else {
		switch activation.Status {
		case domain.ActivationStatusPurchased:
			if m.activationExpired(activation) {
				err = m.expireAndCancel(ctx, activation)
			} else {
				err = m.probeAndStartLogin(ctx, activation)
			}
		case domain.ActivationStatusDuplicate, domain.ActivationStatusPhoneInUse,
			domain.ActivationStatusPINRequired, domain.ActivationStatusUnregistered,
			domain.ActivationStatusLoginFailed, domain.ActivationStatusLoginCodeTimeout,
			domain.ActivationStatusPINCodeTimeout:
			err = m.finalizeProviderAction(ctx, activation, smsbower.SetStatusCancel)
		case domain.ActivationStatusAwaitingLoginCode:
			if m.activationExpired(activation) {
				err = m.expireAndCancel(ctx, activation)
			} else {
				err = m.pollLoginCode(ctx, activation)
			}
		case domain.ActivationStatusCheckingBalance:
			if m.activationExpired(activation) {
				err = m.expireAndCancel(ctx, activation)
			} else {
				err = m.checkBalance(ctx, activation)
			}
		case domain.ActivationStatusZeroBalanceUsed:
			err = m.finalizeProviderAction(ctx, activation, smsbower.SetStatusComplete)
		case domain.ActivationStatusAwaitingPINCode:
			if m.activationExpired(activation) {
				err = m.expireAndCancel(ctx, activation)
			} else {
				err = m.pollPINCode(ctx, activation)
			}
		case domain.ActivationStatusSettingPIN:
			if m.activationExpired(activation) {
				err = m.expireAndCancel(ctx, activation)
			} else {
				err = m.completePINSetting(ctx, activation)
			}
		case domain.ActivationStatusPINChanged:
			err = m.transitionToSubsequentPolling(ctx, activation)
		case domain.ActivationStatusAwaitingSubsequentCode:
			if m.activationExpired(activation) {
				err = m.expireAndCancel(ctx, activation)
			} else {
				err = m.pollFollowupCode(ctx, activation)
			}
		case domain.ActivationStatusActive:
			if m.activationExpired(activation) {
				err = m.expireAndCancel(ctx, activation)
			} else {
				err = m.pollFollowupCode(ctx, activation)
			}
		default:
			_, err = m.store.TransitionActivationOwned(ctx, activation.ID, nil, domain.ActivationStatusFailed, "unsupported workflow state: "+string(activation.Status), activation.LeaseOwner, activation.LeaseVersion)
		}
	}
	err = m.finalizeLoginFailure(ctx, &activation, err)
	if err != nil {
		m.logger.Warn("activation step failed", "activation_id", activation.ID, "state", activation.Status, "error", err)
		// Keep the last actionable reason with the activation so the dashboard
		// does not present a consumed/failed OTP as if it were still pending.
		// This is best-effort: lease ownership remains the source of truth.
		reason := activationStepFailureReason(activation, err)
		_, _ = m.store.TransitionActivationOwned(ctx, activation.ID,
			[]domain.ActivationStatus{activation.Status}, activation.Status, reason,
			activation.LeaseOwner, activation.LeaseVersion)
		// Protocol/network errors are recoverable. Keep the exact state and try
		// again; explicit business classifications transition to terminal states.
		_ = m.store.ReleaseActivationLease(ctx, activation.ID, activation.LeaseOwner, time.Now().UTC().Add(m.cfg.PollInterval))
		return
	}
	latest, latestErr := m.store.GetActivation(ctx, activation.ID)
	if latestErr == nil && latest.FinishedAt != nil {
		m.releaseActivationProxy(ctx, latest)
	}
	if latestErr == nil && !latest.Status.Terminal() {
		// Release only the lease claimed by this worker. A newer claim has a
		// distinct owner and must never be released by a slow previous worker.
		_ = m.store.ReleaseActivationLease(ctx, activation.ID, activation.LeaseOwner, latest.NextRunAt)
	}
}

func (m *Manager) finalizeLoginFailure(ctx context.Context, activation *domain.Activation, err error) error {
	if !errors.Is(err, gopay.ErrLoginFailed) {
		return err
	}
	reason := loginFailureReason(activation.Status, err)
	if pinSubmissionBlockedFailure(activation.Status, err) {
		_, transitionErr := m.store.TransitionActivationOwned(ctx, activation.ID,
			[]domain.ActivationStatus{domain.ActivationStatusSettingPIN},
			domain.ActivationStatusPINSubmissionBlocked, reason,
			activation.LeaseOwner, activation.LeaseVersion)
		if transitionErr != nil {
			return transitionErr
		}
		activation.Status = domain.ActivationStatusPINSubmissionBlocked
		activation.FailureReason = reason
		return m.finalizeProviderAction(ctx, *activation, smsbower.SetStatusComplete)
	}
	return m.cancelAndClassify(ctx, *activation, domain.ActivationStatusLoginFailed, reason)
}

func (m *Manager) acquireAccountSessionLock(ctx context.Context, phone string) (func(), error) {
	key := strings.TrimSpace(phone)
	if normalized, err := domain.NormalizePhone(key); err == nil {
		key = normalized
	}

	m.accountSessionMu.Lock()
	if m.accountSessionLocks == nil {
		m.accountSessionLocks = make(map[string]*accountSessionLockEntry)
	}
	entry := m.accountSessionLocks[key]
	if entry == nil {
		entry = &accountSessionLockEntry{gate: make(chan struct{}, 1)}
		m.accountSessionLocks[key] = entry
	}
	entry.refs++
	m.accountSessionMu.Unlock()

	select {
	case entry.gate <- struct{}{}:
	case <-ctx.Done():
		m.releaseAccountSessionEntry(key, entry, false)
		return nil, ctx.Err()
	}

	var distributedRelease func(context.Context) error
	if locker, ok := m.store.(storage.AccountSessionLockStore); ok {
		var err error
		distributedRelease, err = locker.AcquireAccountSessionLock(ctx, key)
		if err != nil {
			m.releaseAccountSessionEntry(key, entry, true)
			return nil, err
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if distributedRelease != nil {
				releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := distributedRelease(releaseCtx); err != nil {
					m.logger.Warn("release distributed account session lock", "phone", key, "error", err)
				}
				cancel()
			}
			m.releaseAccountSessionEntry(key, entry, true)
		})
	}, nil
}

func (m *Manager) releaseAccountSessionEntry(key string, entry *accountSessionLockEntry, acquired bool) {
	if acquired {
		<-entry.gate
	}
	m.accountSessionMu.Lock()
	entry.refs--
	if entry.refs == 0 && m.accountSessionLocks[key] == entry {
		delete(m.accountSessionLocks, key)
	}
	m.accountSessionMu.Unlock()
}

func (m *Manager) releaseActivationProxy(ctx context.Context, activation domain.Activation) {
	account, err := m.store.GetAccountByPhone(ctx, activation.PhoneNumber)
	if err != nil {
		return
	}
	raw, err := m.box.Open(account.CredentialsEnc)
	if err != nil {
		return
	}
	session, err := gopay.ParseSession(raw)
	if err == nil {
		m.releaseProxy(activation.BatchID, session.ProxyURL)
	}
}

func (m *Manager) activationExpired(activation domain.Activation) bool {
	return activation.ProviderExpiresAt != nil && time.Now().UTC().After(*activation.ProviderExpiresAt)
}

func verificationCodeWaitTimedOut(sentAt, now time.Time, wait time.Duration) bool {
	return !sentAt.IsZero() && now.After(sentAt.Add(wait))
}

func (m *Manager) expireAndCancel(ctx context.Context, activation domain.Activation) error {
	client, err := m.smsClient(ctx, activation.Provider)
	if err != nil {
		return err
	}
	if _, cancelErr := client.SetStatus(ctx, activation.ProviderActivationID, smsbower.SetStatusCancel); !providerActionConcluded(cancelErr) {
		return cancelErr
	}
	_, err = m.store.TransitionActivationOwned(ctx, activation.ID, nil, domain.ActivationStatusExpired, "号码已过期", activation.LeaseOwner, activation.LeaseVersion)
	return err
}

func providerActionConcluded(err error) bool {
	return err == nil || smsbower.IsAPIError(err, "NO_ACTIVATION") || smsbower.IsAPIError(err, "BAD_STATUS")
}

func (m *Manager) processControl(ctx context.Context, activation domain.Activation) error {
	client, err := m.smsClient(ctx, activation.Provider)
	if err != nil {
		return err
	}
	switch activation.ControlAction {
	case domain.ControlActionSuccess:
		if _, err = client.SetStatus(ctx, activation.ProviderActivationID, smsbower.SetStatusComplete); !providerActionConcluded(err) {
			return err
		}
		_, err = m.store.TransitionActivationOwned(ctx, activation.ID, []domain.ActivationStatus{activation.Status}, domain.ActivationStatusSuccess, "", activation.LeaseOwner, activation.LeaseVersion)
		return err
	case domain.ControlActionDelete:
		if _, err = client.SetStatus(ctx, activation.ProviderActivationID, smsbower.SetStatusCancel); !providerActionConcluded(err) {
			return err
		}
		_, err = m.store.TransitionActivationOwned(ctx, activation.ID, []domain.ActivationStatus{activation.Status}, domain.ActivationStatusCancelled, "手动删除", activation.LeaseOwner, activation.LeaseVersion)
		if err == nil {
			err = m.store.HideActivation(ctx, activation.ID)
		}
		return err
	default:
		return m.store.ClearControlAction(ctx, activation.ID, activation.ControlAction)
	}
}

func (m *Manager) probeAndStartLogin(ctx context.Context, activation domain.Activation) error {
	batch, err := m.store.GetBatch(ctx, activation.BatchID)
	if err != nil {
		return err
	}
	targetPIN, err := m.targetPIN(batch)
	if err != nil {
		return err
	}
	// A previous worker may have stopped after persisting its non-idempotent
	// initial OTP intent but before attaching the account or changing the
	// activation status. Adopt that checkpoint instead of probing and sending a
	// second uncounted OTP.
	if recovered, recoverErr := m.recoverPreparedInitialLogin(ctx, activation); recoverErr != nil || recovered {
		return recoverErr
	}
	proxyURL, err := m.claimProxy(ctx, batch)
	if err != nil {
		return err
	}
	keepProxy := false
	defer func() {
		if !keepProxy {
			m.releaseProxy(batch.ID, proxyURL)
		}
	}()
	if proxyURL != "" {
		if err := gopay.PreflightProxy(ctx, proxyURL); err != nil {
			m.markProxyFailure(ctx, batch.ID, proxyURL, err)
			m.releaseProxy(batch.ID, proxyURL)
			m.logger.Warn("proxy preflight failed; slot removed", "batch_id", batch.ID, "proxy", proxyaddr.Mask(proxyURL), "error", err)
			return err
		}
	}
	client, err := m.newGoPayClient(activation.PhoneNumber, batch, nil, proxyURL)
	if err != nil {
		return err
	}
	countryCode := gopayCountryCode(activation, batch)
	local := localPhone(activation.PhoneNumber, countryCode)
	_, err = client.ProbeLogin(ctx, countryCode, local)
	if errors.Is(err, gopay.ErrPINRequired) {
		return m.cancelAndClassify(ctx, activation, domain.ActivationStatusPINRequired, "账号需要已有 PIN 登录")
	}
	if errors.Is(err, gopay.ErrUnregistered) {
		return m.cancelAndClassify(ctx, activation, domain.ActivationStatusUnregistered, "未注册")
	}
	if err != nil {
		return err
	}
	state := client.State()
	state.SMSCycle = activation.SMSCycle
	state.WorkflowActivationID = activation.ID
	state.LoginStage = gopay.LoginStageAwaiting1FAOTP
	state.LoginCodeDispatchUncertain = true
	state.LoginCodeSentAt = time.Time{}
	state.LoginCodeResends = 0
	client.Restore(state)
	account, err := m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil)
	if err != nil {
		return err
	}
	// From here on the claimed proxy is durably recoverable from the encrypted
	// session, even if a following association write is interrupted.
	keepProxy = true
	if err = m.store.AttachActivationAccountOwned(ctx, activation.ID, account.ID, activation.LeaseOwner, activation.LeaseVersion); err != nil {
		return err
	}
	if _, err = m.store.TransitionActivationOwned(ctx, activation.ID,
		[]domain.ActivationStatus{domain.ActivationStatusPurchased},
		domain.ActivationStatusAwaitingLoginCode, "",
		activation.LeaseOwner, activation.LeaseVersion,
	); err != nil {
		return err
	}

	// StartLogin can deliver an OTP. The uncertain checkpoint above is therefore
	// the last durable write before this call. Only a successfully persisted
	// response clears it and records the new OTP token.
	if _, err = client.StartLogin(ctx); err != nil {
		return err
	}
	state = client.State()
	state.SMSCycle = activation.SMSCycle
	state.WorkflowActivationID = activation.ID
	state.LoginCodeSentAt = time.Now().UTC()
	state.LoginCodeResends = 0
	state.LoginCodeDispatchUncertain = false
	client.Restore(state)
	_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil)
	return err
}

// recoverPreparedInitialLogin repairs the two local write gaps between saving
// the initial dispatch intent, attaching its account and transitioning the
// activation. No provider or GoPay call is made on this path.
func (m *Manager) recoverPreparedInitialLogin(
	ctx context.Context,
	activation domain.Activation,
) (bool, error) {
	account, err := m.store.GetAccountByPhone(ctx, activation.PhoneNumber)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, err := m.box.Open(account.CredentialsEnc)
	if err != nil {
		return false, err
	}
	state, err := gopay.ParseSession(raw)
	if err != nil {
		return false, err
	}
	if state.WorkflowActivationID != activation.ID ||
		!state.LoginCodeDispatchUncertain ||
		state.LoginStage != gopay.LoginStageAwaiting1FAOTP {
		return false, nil
	}
	if activation.AccountID == nil || *activation.AccountID != account.ID {
		if err = m.store.AttachActivationAccountOwned(
			ctx, activation.ID, account.ID, activation.LeaseOwner, activation.LeaseVersion,
		); err != nil {
			return true, err
		}
	}
	_, err = m.store.TransitionActivationOwned(ctx, activation.ID,
		[]domain.ActivationStatus{domain.ActivationStatusPurchased},
		domain.ActivationStatusAwaitingLoginCode, "",
		activation.LeaseOwner, activation.LeaseVersion,
	)
	return true, err
}

func (m *Manager) cancelAndClassify(ctx context.Context, activation domain.Activation, status domain.ActivationStatus, reason string) error {
	return m.cancelAndClassifyFrom(ctx, activation, nil, status, reason)
}

func (m *Manager) cancelAndClassifyFrom(
	ctx context.Context,
	activation domain.Activation,
	expected []domain.ActivationStatus,
	status domain.ActivationStatus,
	reason string,
) error {
	_, err := m.store.TransitionActivationOwned(ctx, activation.ID, expected, status, reason, activation.LeaseOwner, activation.LeaseVersion)
	if err != nil {
		return err
	}
	activation.Status = status
	return m.finalizeProviderAction(ctx, activation, smsbower.SetStatusCancel)
}

func (m *Manager) finalizeProviderAction(ctx context.Context, activation domain.Activation, action smsbower.SetStatus) error {
	if repairedReason, repair := repairedProviderFinalizationReason(activation); repair {
		_, err := m.store.TransitionActivationOwned(ctx, activation.ID,
			[]domain.ActivationStatus{activation.Status}, activation.Status, repairedReason,
			activation.LeaseOwner, activation.LeaseVersion)
		if err != nil {
			return err
		}
		activation.FailureReason = repairedReason
	}
	client, err := m.smsClient(ctx, activation.Provider)
	if err != nil {
		return err
	}
	_, err = client.SetStatus(ctx, activation.ProviderActivationID, action)
	if !providerActionConcluded(err) {
		return err
	}
	_, err = m.store.FinalizeActivationOwned(ctx, activation.ID, []domain.ActivationStatus{activation.Status}, activation.LeaseOwner, activation.LeaseVersion)
	return err
}

func (m *Manager) pollLoginCode(ctx context.Context, activation domain.Activation) error {
	if isHeroSMSActivation(activation) {
		return m.waitHeroSMSLoginCode(ctx, activation)
	}
	batch, _, client, err := m.restoreGoPayClient(ctx, activation)
	if err != nil {
		return err
	}
	targetPIN, err := m.targetPIN(batch)
	if err != nil {
		return err
	}
	state := client.State()
	switch state.LoginStage {
	case gopay.LoginStageAuthenticated:
		_, err = m.store.TransitionActivationOwned(ctx, activation.ID, []domain.ActivationStatus{domain.ActivationStatusAwaitingLoginCode}, domain.ActivationStatusCheckingBalance, "", activation.LeaseOwner, activation.LeaseVersion)
		return err
	case gopay.LoginStageReady1FA, gopay.LoginStageReady2FA:
		// Ready2FA also represents a brand-new challenge after 1FA. Only
		// re-check the provider when this ready state came from an elapsed wait;
		// otherwise the previous cycle's login code may still be visible.
		if !state.LoginCodeSentAt.IsZero() {
			status, pollErr := m.pollStatus(ctx, activation)
			if pollErr != nil {
				return pollErr
			}
			code, received := providerVerificationCode(status)
			if received && !state.LoginCodeDispatchUncertain {
				return m.consumeLoginVerificationCode(ctx, activation, client, targetPIN, code)
			}
			if status.Kind == smsbower.StatusOK && !received {
				return fmt.Errorf("SMSBower returned an empty login verification code")
			}
			// A code returned for an uncertain dispatch belongs to a token which
			// may never have reached durable storage. The timeout has already
			// elapsed in a ready state, so abandon that code and advance to the
			// next counted attempt instead of retrying it with the older token.
			if !providerStillWaiting(status.Kind) && !(received && state.LoginCodeDispatchUncertain) {
				return nil
			}
		}
		cycle := activation.SMSCycle
		if state.SMSCycle >= activation.SMSCycle {
			cycle, state, err = m.advanceVerificationSMSCycle(ctx, activation, state, func(checkpoint gopay.Session) error {
				_, saveErr := m.saveSession(ctx, activation.PhoneNumber, checkpoint, targetPIN, domain.AccountStatusPending, nil)
				return saveErr
			})
			if err != nil {
				return err
			}
		}
		if state.LoginStage == gopay.LoginStageReady1FA {
			state.LoginStage = gopay.LoginStageCycleReady1FA
		} else {
			state.LoginStage = gopay.LoginStageCycleReady2FA
		}
		state.SMSCycle = cycle
		state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
		client.Restore(state)
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil)
		return err
	case gopay.LoginStageCycleReady1FA, gopay.LoginStageCycleReady2FA:
		// Persist the attempt before the non-idempotent GoPay call. If the
		// process stops after dispatch, recovery waits for this attempt instead
		// of sending an uncounted duplicate. Until the newly returned OTP token
		// is saved, any code received for this dispatch is deliberately treated
		// as unusable rather than verified with the previous token.
		pending := state
		if state.LoginStage == gopay.LoginStageCycleReady1FA {
			pending.LoginStage = gopay.LoginStageAwaiting1FAOTP
		} else {
			pending.LoginStage = gopay.LoginStageAwaiting2FAOTP
		}
		pending.LoginCodeDispatchUncertain = true
		// The OTP has not been acknowledged yet. Leave the wait anchor empty so
		// a timeout or process interruption receives a fresh, conservative
		// 60-second window when recovery first observes the uncertain dispatch.
		pending.LoginCodeSentAt = time.Time{}
		if _, err = m.saveSession(ctx, activation.PhoneNumber, pending, targetPIN, domain.AccountStatusPending, nil); err != nil {
			return err
		}
		if state.LoginStage == gopay.LoginStageCycleReady1FA {
			_, err = client.StartLogin(ctx)
		} else {
			_, err = client.StartNextLoginOTP(ctx)
		}
		if err != nil {
			return err
		}
		state = client.State()
		state.LoginCodeSentAt = time.Now().UTC()
		state.LoginCodeDispatchUncertain = false
		client.Restore(state)
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil)
		return err
	}

	status, err := m.pollStatus(ctx, activation)
	if err != nil {
		return err
	}
	code, received := providerVerificationCode(status)
	if received && !state.LoginCodeDispatchUncertain {
		return m.consumeLoginVerificationCode(ctx, activation, client, targetPIN, code)
	}
	if !received {
		if status.Kind == smsbower.StatusOK {
			return fmt.Errorf("SMSBower returned an empty login verification code")
		}
		if !providerStillWaiting(status.Kind) {
			return nil
		}
		now := time.Now().UTC()
		if state.LoginCodeSentAt.IsZero() {
			state.LoginCodeSentAt = now
			client.Restore(state)
			_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil)
			return err
		}
		if !verificationCodeWaitTimedOut(state.LoginCodeSentAt, now, loginVerificationCodeWait) {
			return nil
		}
	}
	// A successful provider response can still be unusable after a process or
	// database failure between GoPay dispatch and saving the new OTP token. It
	// remains one counted attempt and observes the same full 60-second window.
	now := time.Now().UTC()
	if state.LoginCodeSentAt.IsZero() {
		state.LoginCodeSentAt = now
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil); err != nil {
			return err
		}
	}
	if !verificationCodeWaitTimedOut(state.LoginCodeSentAt, now, loginVerificationCodeWait) {
		if received && status.Kind == smsbower.StatusOK {
			return m.store.TouchActivationPoll(ctx, activation.ID, activation.LeaseOwner, now, now.Add(m.cfg.PollInterval))
		}
		return nil
	}
	if state.LoginCodeResends >= verificationCodeResends {
		return m.cancelAndClassifyFrom(ctx, activation,
			[]domain.ActivationStatus{domain.ActivationStatusAwaitingLoginCode},
			domain.ActivationStatusLoginCodeTimeout,
			"登录验证码重发 3 次后仍未收到")
	}
	switch state.LoginStage {
	case gopay.LoginStageAwaiting1FAOTP:
		state.LoginStage = gopay.LoginStageReady1FA
	case gopay.LoginStageAwaiting2FAOTP:
		state.LoginStage = gopay.LoginStageReady2FA
	default:
		return fmt.Errorf("unexpected login OTP wait stage %q", state.LoginStage)
	}
	state.LoginCodeResends++
	client.Restore(state)
	_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil)
	return err
}

func (m *Manager) consumeLoginVerificationCode(
	ctx context.Context,
	activation domain.Activation,
	client *gopay.Client,
	targetPIN string,
	code string,
) error {
	state := client.State()
	// A timeout is durably represented by a ready stage. If the previous code
	// arrives during the final provider re-check, restore its awaiting stage so
	// the GoPay verifier can still consume that challenge.
	switch state.LoginStage {
	case gopay.LoginStageReady1FA:
		state.LoginStage = gopay.LoginStageAwaiting1FAOTP
		client.Restore(state)
	case gopay.LoginStageReady2FA:
		state.LoginStage = gopay.LoginStageAwaiting2FAOTP
		client.Restore(state)
	}
	storedCode, err := m.appendCode(ctx, activation, domain.VerificationPhaseLogin, code)
	if err != nil {
		return err
	}
	result, err := client.VerifyLoginOTP(ctx, storedCode)
	if err != nil {
		return err
	}
	state = client.State()
	if state.SMSCycle < activation.SMSCycle {
		state.SMSCycle = activation.SMSCycle
	}
	if result.NeedsOTP {
		state.LoginCodeSentAt = time.Time{}
		state.LoginCodeResends = 0
		state.LoginCodeDispatchUncertain = false
		state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	} else if result.Authenticated {
		state.LoginCodeSentAt = time.Time{}
		state.LoginCodeResends = 0
		state.LoginCodeDispatchUncertain = false
		state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	}
	client.Restore(state)
	accountStatus := domain.AccountStatusPending
	if result.Authenticated {
		accountStatus = domain.AccountStatusAuthenticated
	}
	if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, accountStatus, nil); err != nil {
		return err
	}
	if !result.Authenticated && !result.NeedsOTP {
		return fmt.Errorf("GoPay login did not authenticate")
	}
	return nil
}

func providerVerificationCode(status smsbower.ActivationStatus) (string, bool) {
	code := strings.TrimSpace(status.Code)
	if code == "" {
		return "", false
	}
	switch status.Kind {
	case smsbower.StatusOK, smsbower.StatusWaitRetry, smsbower.StatusWaitResend:
		return code, true
	default:
		return "", false
	}
}

func providerStillWaiting(kind smsbower.StatusKind) bool {
	switch kind {
	case smsbower.StatusWaitCode, smsbower.StatusWaitRetry, smsbower.StatusWaitResend, smsbower.StatusUnknown:
		return true
	default:
		return false
	}
}

func (m *Manager) checkBalance(ctx context.Context, activation domain.Activation) error {
	batch, _, client, err := m.restoreGoPayClient(ctx, activation)
	if err != nil {
		return err
	}
	targetPIN, err := m.targetPIN(batch)
	if err != nil {
		return err
	}
	balance, err := client.GetBalance(ctx)
	if isHTTPStatus(err, 401) {
		if refreshErr := m.refreshAndPersistSession(ctx, activation, client, targetPIN, domain.AccountStatusAuthenticated, activation.BalanceRP); refreshErr != nil {
			err = refreshErr
		} else {
			balance, err = client.GetBalance(ctx)
		}
	}
	if err != nil || !balance.Known {
		if err == nil {
			err = gopay.ErrBalanceUnknown
		}
		return err
	}
	amount := float64(balance.Amount)
	now := time.Now().UTC()
	if err = m.store.SetActivationBalanceOwned(ctx, activation.ID, &amount, &now, activation.LeaseOwner, activation.LeaseVersion); err != nil {
		return err
	}
	if balance.Amount == 0 {
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusDisabled, &amount); err != nil {
			return err
		}
		_, err = m.store.TransitionActivationOwned(ctx, activation.ID, nil, domain.ActivationStatusZeroBalanceUsed, "0RP已被使用", activation.LeaseOwner, activation.LeaseVersion)
		if err != nil {
			return err
		}
		activation.Status = domain.ActivationStatusZeroBalanceUsed
		return m.finalizeProviderAction(ctx, activation, smsbower.SetStatusComplete)
	}
	if balance.Amount < 0 {
		return gopay.ErrBalanceUnknown
	}
	// Select reset immediately when the authenticated profile exposes an
	// existing PIN. Compatible deployments which omit the flag or profile
	// endpoint still fall back to setup; GoPay-111 remains the authoritative
	// setup-to-reset fallback.
	pinStage := gopay.PINStageReadyCycle
	pinStatus, pinStatusErr := client.GetPINStatus(ctx)
	if isHTTPStatus(pinStatusErr, 401) {
		if refreshErr := m.refreshAndPersistSession(ctx, activation, client, targetPIN, domain.AccountStatusAuthenticated, &amount); refreshErr != nil {
			return refreshErr
		} else {
			pinStatus, pinStatusErr = client.GetPINStatus(ctx)
		}
	}
	if isHTTPStatus(pinStatusErr, 401) {
		return pinStatusErr
	}
	// Do not silently fall back to PIN setup when GoPay explicitly rejected
	// the login/session (for example GoPay-112 or auth:error:ratelimited).
	if errors.Is(pinStatusErr, gopay.ErrLoginFailed) {
		return pinStatusErr
	}
	if pinStatusErr == nil && pinStatus.Known && pinStatus.Set {
		pinStage = gopay.PINStageResetReadyCycle
	} else if pinStatusErr != nil {
		m.logger.Debug("PIN profile probe unavailable; falling back to setup", "activation_id", activation.ID, "error", pinStatusErr)
	}
	state := client.State()
	state.PINStage = pinStage
	state.SMSCycle = activation.SMSCycle
	state.PINCodeSentAt = time.Time{}
	state.PINCodeResends = 0
	state.PINCodeDispatchUncertain = false
	state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	client.Restore(state)
	if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, &amount); err != nil {
		return err
	}
	_, err = m.store.TransitionActivationOwned(ctx, activation.ID, []domain.ActivationStatus{domain.ActivationStatusCheckingBalance}, domain.ActivationStatusAwaitingPINCode, "", activation.LeaseOwner, activation.LeaseVersion)
	return err
}

func (m *Manager) pollPINCode(ctx context.Context, activation domain.Activation) error {
	if isHeroSMSActivation(activation) {
		return m.waitHeroSMSPINCode(ctx, activation)
	}
	batch, _, client, err := m.restoreGoPayClient(ctx, activation)
	if err != nil {
		return err
	}
	targetPIN, err := m.targetPIN(batch)
	if err != nil {
		return err
	}
	state := client.State()
	switch state.PINStage {
	case gopay.PINStageReadyCycle:
		if !state.PINCodeSentAt.IsZero() {
			status, pollErr := m.pollStatus(ctx, activation)
			if pollErr != nil {
				return pollErr
			}
			code, received := providerVerificationCode(status)
			if received && !state.PINCodeDispatchUncertain {
				return m.consumePINVerificationCode(ctx, activation, client, targetPIN, code)
			}
			if status.Kind == smsbower.StatusOK && !received {
				return fmt.Errorf("SMSBower returned an empty PIN verification code")
			}
			if !providerStillWaiting(status.Kind) && !(received && state.PINCodeDispatchUncertain) {
				return nil
			}
		}
		cycle := activation.SMSCycle
		if state.SMSCycle >= activation.SMSCycle {
			cycle, state, err = m.advanceVerificationSMSCycle(ctx, activation, state, func(checkpoint gopay.Session) error {
				_, saveErr := m.saveSession(ctx, activation.PhoneNumber, checkpoint, targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
				return saveErr
			})
			if err != nil {
				return err
			}
		}
		state.PINStage = gopay.PINStageCycleReady
		state.SMSCycle = cycle
		state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
		client.Restore(state)
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
		return err
	case gopay.PINStageCycleReady:
		if err = m.savePINDispatchCheckpoint(ctx, activation, state, targetPIN, gopay.PINStageAwaiting); err != nil {
			return err
		}
		if _, err = client.StartPINSetup(ctx, targetPIN); err != nil {
			if isHTTPStatus(err, 401) {
				if refreshErr := client.Refresh(ctx); refreshErr != nil {
					err = refreshErr
				} else if checkpointErr := m.savePINDispatchCheckpoint(ctx, activation, client.State(), targetPIN, gopay.PINStageAwaiting); checkpointErr != nil {
					err = checkpointErr
				} else {
					_, err = client.StartPINSetup(ctx, targetPIN)
				}
			}
			if errors.Is(err, gopay.ErrPINAlreadySet) {
				return m.preparePINReset(ctx, activation, client, targetPIN)
			}
			if err != nil {
				return err
			}
		}
		state = client.State()
		state.PINCodeSentAt = time.Now().UTC()
		state.PINCodeDispatchUncertain = false
		client.Restore(state)
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
		return err
	case gopay.PINStageResetReadyCycle:
		if !state.PINCodeSentAt.IsZero() {
			status, pollErr := m.pollStatus(ctx, activation)
			if pollErr != nil {
				return pollErr
			}
			code, received := providerVerificationCode(status)
			if received && !state.PINCodeDispatchUncertain {
				return m.consumePINVerificationCode(ctx, activation, client, targetPIN, code)
			}
			if status.Kind == smsbower.StatusOK && !received {
				return fmt.Errorf("SMSBower returned an empty PIN verification code")
			}
			if !providerStillWaiting(status.Kind) && !(received && state.PINCodeDispatchUncertain) {
				return nil
			}
		}
		cycle := activation.SMSCycle
		if state.SMSCycle >= activation.SMSCycle {
			cycle, state, err = m.advanceVerificationSMSCycle(ctx, activation, state, func(checkpoint gopay.Session) error {
				_, saveErr := m.saveSession(ctx, activation.PhoneNumber, checkpoint, targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
				return saveErr
			})
			if err != nil {
				return err
			}
		}
		state.PINStage = gopay.PINStageResetCycleReady
		state.SMSCycle = cycle
		state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
		client.Restore(state)
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
		return err
	case gopay.PINStageResetCycleReady:
		if err = m.savePINDispatchCheckpoint(ctx, activation, state, targetPIN, gopay.PINStageResetAwaiting); err != nil {
			return err
		}
		if _, err = client.StartPINReset(ctx, targetPIN); err != nil {
			if isHTTPStatus(err, 401) {
				if refreshErr := client.Refresh(ctx); refreshErr != nil {
					err = refreshErr
				} else if checkpointErr := m.savePINDispatchCheckpoint(ctx, activation, client.State(), targetPIN, gopay.PINStageResetAwaiting); checkpointErr != nil {
					err = checkpointErr
				} else {
					_, err = client.StartPINReset(ctx, targetPIN)
				}
			}
			if err != nil {
				return err
			}
		}
		state = client.State()
		state.PINCodeSentAt = time.Now().UTC()
		state.PINCodeDispatchUncertain = false
		client.Restore(state)
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
		return err
	case gopay.PINStageSetupVerified, gopay.PINStageResetVerified, gopay.PINStageComplete:
		return m.publishPINSettingState(ctx, activation)
	}

	status, err := m.pollStatus(ctx, activation)
	if err != nil {
		return err
	}
	code, received := providerVerificationCode(status)
	if received && !state.PINCodeDispatchUncertain {
		return m.consumePINVerificationCode(ctx, activation, client, targetPIN, code)
	}
	if !received {
		if status.Kind == smsbower.StatusOK {
			return fmt.Errorf("SMSBower returned an empty PIN verification code")
		}
		if !providerStillWaiting(status.Kind) {
			return nil
		}
		now := time.Now().UTC()
		if state.PINCodeSentAt.IsZero() {
			state.PINCodeSentAt = now
			client.Restore(state)
			_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
			return err
		}
		if !verificationCodeWaitTimedOut(state.PINCodeSentAt, now, pinVerificationCodeWait) {
			return nil
		}
	}
	now := time.Now().UTC()
	if state.PINCodeSentAt.IsZero() {
		state.PINCodeSentAt = now
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); err != nil {
			return err
		}
	}
	if !verificationCodeWaitTimedOut(state.PINCodeSentAt, now, pinVerificationCodeWait) {
		if received && status.Kind == smsbower.StatusOK {
			return m.store.TouchActivationPoll(ctx, activation.ID, activation.LeaseOwner, now, now.Add(m.cfg.PollInterval))
		}
		return nil
	}
	if state.PINCodeResends >= verificationCodeResends {
		return m.cancelAndClassifyFrom(ctx, activation,
			[]domain.ActivationStatus{domain.ActivationStatusAwaitingPINCode},
			domain.ActivationStatusPINCodeTimeout,
			"改 PIN 验证码重发 3 次后仍未收到")
	}
	switch state.PINStage {
	case gopay.PINStageAwaiting:
		state.PINStage = gopay.PINStageReadyCycle
	case gopay.PINStageResetAwaiting:
		state.PINStage = gopay.PINStageResetReadyCycle
	default:
		return fmt.Errorf("unexpected PIN OTP wait stage %q", state.PINStage)
	}
	state.PINCodeResends++
	client.Restore(state)
	_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
	return err
}

func (m *Manager) savePINDispatchCheckpoint(
	ctx context.Context,
	activation domain.Activation,
	state gopay.Session,
	targetPIN string,
	awaitingStage gopay.PINStage,
) error {
	state.PINStage = awaitingStage
	state.PINCodeSentAt = time.Time{}
	state.PINCodeDispatchUncertain = true
	_, err := m.saveSession(ctx, activation.PhoneNumber, state, targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
	return err
}

func (m *Manager) consumePINVerificationCode(
	ctx context.Context,
	activation domain.Activation,
	client *gopay.Client,
	targetPIN string,
	code string,
) error {
	state := client.State()
	switch state.PINStage {
	case gopay.PINStageReadyCycle:
		state.PINStage = gopay.PINStageAwaiting
		client.Restore(state)
	case gopay.PINStageResetReadyCycle:
		state.PINStage = gopay.PINStageResetAwaiting
		client.Restore(state)
	}
	storedCode, err := m.appendCode(ctx, activation, domain.VerificationPhasePIN, code)
	if err != nil {
		return err
	}
	if state.PINStage == gopay.PINStageResetAwaiting {
		err = client.VerifyPINResetOTP(ctx, storedCode)
	} else {
		err = client.VerifyPINSetupOTP(ctx, storedCode)
	}
	if err != nil {
		if isHTTPStatus(err, 401) {
			if refreshErr := m.refreshAndPersistSession(ctx, activation, client, targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); refreshErr != nil {
				err = refreshErr
			} else {
				if state.PINStage == gopay.PINStageResetAwaiting {
					err = client.VerifyPINResetOTP(ctx, storedCode)
				} else {
					err = client.VerifyPINSetupOTP(ctx, storedCode)
				}
			}
		}
	}
	// pollPINCode always owns an awaiting_pin_code activation. Rebuild the
	// durable protocol session in place; there is no setting_pin transition to
	// reverse at this point.
	if errors.Is(err, gopay.ErrPINVerificationExpired) {
		return m.recoverExpiredPINVerification(ctx, activation, client, targetPIN)
	}
	if errors.Is(err, gopay.ErrPINAlreadySet) {
		return m.preparePINReset(ctx, activation, client, targetPIN)
	}
	if err != nil {
		return err
	}
	state = client.State()
	if state.SMSCycle < activation.SMSCycle {
		state.SMSCycle = activation.SMSCycle
	}
	state.PINCodeSentAt = time.Time{}
	state.PINCodeResends = 0
	state.PINCodeDispatchUncertain = false
	state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	client.Restore(state)
	if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); err != nil {
		return err
	}
	return m.publishPINSettingState(ctx, activation)
}

func (m *Manager) publishPINSettingState(ctx context.Context, activation domain.Activation) error {
	// Schedule before publishing so the visible setting state cannot be skipped
	// by the scheduler or lost in a crash between the two writes.
	now := time.Now().UTC()
	if err := m.store.TouchActivationPoll(ctx, activation.ID, activation.LeaseOwner, now, now.Add(pinStatusDisplayDuration(m.cfg.PollInterval))); err != nil {
		return err
	}
	_, err := m.store.TransitionActivationOwned(ctx, activation.ID,
		[]domain.ActivationStatus{domain.ActivationStatusAwaitingPINCode},
		domain.ActivationStatusSettingPIN, "",
		activation.LeaseOwner, activation.LeaseVersion)
	return err
}

func (m *Manager) preparePINReset(ctx context.Context, activation domain.Activation, client *gopay.Client, targetPIN string) error {
	state := client.State()
	state.PINStage = gopay.PINStageResetReadyCycle
	state.PINVerificationID = ""
	state.PINOTPToken = ""
	state.PINVerificationToken = ""
	state.PINChallengeID = ""
	state.PINClientID = ""
	state.PINCodeSentAt = time.Time{}
	state.PINCodeResends = 0
	state.PINCodeDispatchUncertain = false
	state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	client.Restore(state)
	_, err := m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
	return err
}

func (m *Manager) recoverExpiredPINVerification(ctx context.Context, activation domain.Activation, client *gopay.Client, targetPIN string) error {
	state := client.State()
	if state.PINStage == gopay.PINStageResetAwaiting || state.PINStage == gopay.PINStageResetVerified {
		state.PINStage = gopay.PINStageResetReadyCycle
	} else {
		state.PINStage = gopay.PINStageReadyCycle
	}
	state.PINVerificationID = ""
	state.PINOTPToken = ""
	state.PINVerificationToken = ""
	state.PINChallengeID = ""
	state.PINClientID = ""
	state.PINCodeSentAt = time.Time{}
	state.PINCodeResends = 0
	state.PINCodeDispatchUncertain = false
	state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	client.Restore(state)
	_, err := m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
	return err
}

func (m *Manager) completePINSetting(ctx context.Context, activation domain.Activation) error {
	if isHeroSMSActivation(activation) {
		if err := m.reconcileHeroSMSConsumedEvents(ctx, activation, domain.VerificationPhasePIN); err != nil {
			return err
		}
	}
	batch, _, client, err := m.restoreGoPayClient(ctx, activation)
	if err != nil {
		return err
	}
	targetPIN, err := m.targetPIN(batch)
	if err != nil {
		return err
	}
	state := client.State()
	switch state.PINStage {
	case gopay.PINStageSetupVerified:
		err = client.CompletePINSetup(ctx, targetPIN)
	case gopay.PINStageResetVerified:
		err = client.CompletePINReset(ctx, targetPIN)
	case gopay.PINStageComplete:
		return m.finalizePINSetting(ctx, activation)
	default:
		return fmt.Errorf("unexpected PIN completion stage %q", state.PINStage)
	}
	if isHTTPStatus(err, 401) {
		if refreshErr := m.refreshAndPersistSession(ctx, activation, client, targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); refreshErr != nil {
			err = refreshErr
		} else {
			if state.PINStage == gopay.PINStageResetVerified {
				err = client.CompletePINReset(ctx, targetPIN)
			} else {
				err = client.CompletePINSetup(ctx, targetPIN)
			}
		}
	}
	if errors.Is(err, gopay.ErrPINAlreadySet) {
		if saveErr := m.preparePINReset(ctx, activation, client, targetPIN); saveErr != nil {
			return saveErr
		}
		_, transitionErr := m.store.TransitionActivationOwned(ctx, activation.ID, []domain.ActivationStatus{domain.ActivationStatusSettingPIN}, domain.ActivationStatusAwaitingPINCode, "", activation.LeaseOwner, activation.LeaseVersion)
		return transitionErr
	}
	if errors.Is(err, gopay.ErrPINVerificationExpired) {
		if saveErr := m.recoverExpiredPINVerification(ctx, activation, client, targetPIN); saveErr != nil {
			return saveErr
		}
		_, transitionErr := m.store.TransitionActivationOwned(ctx, activation.ID, []domain.ActivationStatus{domain.ActivationStatusSettingPIN}, domain.ActivationStatusAwaitingPINCode, "", activation.LeaseOwner, activation.LeaseVersion)
		return transitionErr
	}
	if err != nil {
		return err
	}
	if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusActive, activation.BalanceRP); err != nil {
		return err
	}
	return m.finalizePINSetting(ctx, activation)
}

func (m *Manager) finalizePINSetting(ctx context.Context, activation domain.Activation) error {
	if _, err := m.store.MarkActivationFulfilledOwned(ctx, activation.ID, activation.LeaseOwner, activation.LeaseVersion); err != nil {
		return err
	}
	// Schedule the next state before publishing pin_changed. If a process stops
	// between these writes it merely retries setting_pin later; once pin_changed
	// is visible it remains long enough for the dashboard to observe it.
	now := time.Now().UTC()
	if err := m.store.TouchActivationPoll(ctx, activation.ID, activation.LeaseOwner, now, now.Add(pinStatusDisplayDuration(m.cfg.PollInterval))); err != nil {
		return err
	}
	if _, err := m.store.TransitionActivationOwned(ctx, activation.ID, []domain.ActivationStatus{activation.Status}, domain.ActivationStatusPINChanged, "", activation.LeaseOwner, activation.LeaseVersion); err != nil {
		return err
	}
	return nil
}

func pinStatusDisplayDuration(pollInterval time.Duration) time.Duration {
	const minimumDisplay = 4 * time.Second
	if pollInterval > minimumDisplay {
		return pollInterval
	}
	return minimumDisplay
}

func (m *Manager) transitionToSubsequentPolling(ctx context.Context, activation domain.Activation) error {
	if isHeroSMSActivation(activation) {
		var err error
		activation, err = m.ensureHeroSMSFollowupCycle(ctx, activation)
		if err != nil {
			return err
		}
	}
	if _, err := m.store.TransitionActivationOwned(ctx, activation.ID,
		[]domain.ActivationStatus{domain.ActivationStatusPINChanged},
		domain.ActivationStatusAwaitingSubsequentCode, "",
		activation.LeaseOwner, activation.LeaseVersion); err != nil {
		return err
	}
	now := time.Now().UTC()
	if isHeroSMSActivation(activation) {
		return m.scheduleHeroSMSWait(ctx, activation, m.heroSMSProviderDeadline(activation))
	}
	return m.store.TouchActivationPoll(ctx, activation.ID, activation.LeaseOwner, now, now)
}

func (m *Manager) pollFollowupCode(ctx context.Context, activation domain.Activation) error {
	if isHeroSMSActivation(activation) {
		return m.waitHeroSMSFollowupCode(ctx, activation)
	}
	status, err := m.pollStatus(ctx, activation)
	if err != nil || status.Kind != smsbower.StatusOK {
		return err
	}
	if _, err = m.appendCode(ctx, activation, domain.VerificationPhaseSubsequent, status.Code); err != nil {
		return err
	}
	_, err = m.requestAnother(ctx, activation)
	return err
}

func (m *Manager) pollStatus(ctx context.Context, activation domain.Activation) (smsbower.ActivationStatus, error) {
	client, err := m.smsClient(ctx, activation.Provider)
	if err != nil {
		return smsbower.ActivationStatus{}, err
	}
	status, err := client.GetStatus(ctx, activation.ProviderActivationID)
	if err != nil {
		return status, err
	}
	now := time.Now().UTC()
	switch status.Kind {
	case smsbower.StatusWaitCode, smsbower.StatusWaitRetry, smsbower.StatusWaitResend, smsbower.StatusUnknown:
		return status, m.store.TouchActivationPoll(ctx, activation.ID, activation.LeaseOwner, now, now.Add(m.cfg.PollInterval))
	case smsbower.StatusCancel:
		_, err = m.store.TransitionActivationOwned(ctx, activation.ID, nil, domain.ActivationStatusExpired, "短信激活已结束", activation.LeaseOwner, activation.LeaseVersion)
		return status, err
	case smsbower.StatusOK:
		return status, nil
	default:
		return status, fmt.Errorf("unknown SMS provider status %q", status.Kind)
	}
}

func (m *Manager) requestAnother(ctx context.Context, activation domain.Activation) (int, error) {
	client, err := m.smsClient(ctx, activation.Provider)
	if err != nil {
		return 0, err
	}
	if _, err = client.SetStatus(ctx, activation.ProviderActivationID, smsbower.SetStatusRequestAnother); err != nil {
		return 0, err
	}
	return m.store.AdvanceSMSCycle(ctx, activation.ID, activation.LeaseOwner, time.Now().UTC().Add(m.cfg.PollInterval))
}

// advanceVerificationSMSCycle makes the provider's non-idempotent
// setStatus=3 request recoverable. The dispatching checkpoint is written
// before the call, the accepted checkpoint before the local cycle update, and
// the caller persists the cleared state together with its next GoPay stage.
// A BAD_STATUS is accepted only while recovering a previously persisted,
// outcome-uncertain dispatch; a first-call BAD_STATUS remains a real rejection
// so a code racing with the request can still be observed on the next poll.
func (m *Manager) advanceVerificationSMSCycle(
	ctx context.Context,
	activation domain.Activation,
	state gopay.Session,
	persist func(gopay.Session) error,
) (int, gopay.Session, error) {
	// Construct and validate the local provider client before persisting an
	// external-call intent. Configuration failures are known not to have sent a
	// request and therefore must not enter ambiguous-dispatch recovery.
	client, err := m.smsClient(ctx, activation.Provider)
	if err != nil {
		return 0, state, err
	}
	recovering := false
	switch state.VerificationCycleRequest {
	case gopay.VerificationCycleRequestNone:
		state.VerificationCycleRequest = gopay.VerificationCycleRequestDispatching
		if err := persistVerificationCycleCheckpoint(ctx, persist, state); err != nil {
			return 0, state, err
		}
	case gopay.VerificationCycleRequestDispatching:
		recovering = true
	case gopay.VerificationCycleRequestAccepted:
		cycle, err := m.store.AdvanceSMSCycle(ctx, activation.ID, activation.LeaseOwner, time.Now().UTC().Add(m.cfg.PollInterval))
		if err != nil {
			return 0, state, err
		}
		state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
		return cycle, state, nil
	default:
		return 0, state, fmt.Errorf("unexpected verification cycle request state %q", state.VerificationCycleRequest)
	}

	_, err = client.SetStatus(ctx, activation.ProviderActivationID, smsbower.SetStatusRequestAnother)
	if err != nil && !(recovering && smsbower.IsAPIError(err, "BAD_STATUS")) {
		if !recovering && smsbower.IsAPIError(err, "BAD_STATUS") {
			state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
			if saveErr := persistVerificationCycleCheckpoint(ctx, persist, state); saveErr != nil {
				return 0, state, errors.Join(err, saveErr)
			}
		}
		return 0, state, err
	}

	state.VerificationCycleRequest = gopay.VerificationCycleRequestAccepted
	if err = persistVerificationCycleCheckpoint(ctx, persist, state); err != nil {
		return 0, state, err
	}
	cycle, err := m.store.AdvanceSMSCycle(ctx, activation.ID, activation.LeaseOwner, time.Now().UTC().Add(m.cfg.PollInterval))
	if err != nil {
		return 0, state, err
	}
	state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	return cycle, state, nil
}

func persistVerificationCycleCheckpoint(
	ctx context.Context,
	persist func(gopay.Session) error,
	state gopay.Session,
) error {
	var err error
	for attempt := 0; attempt < verificationCheckpointSaves; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err = persist(state); err == nil {
			return nil
		}
		if errors.Is(err, storage.ErrConflict) {
			return err
		}
		if attempt+1 < verificationCheckpointSaves {
			timer := time.NewTimer(verificationCheckpointRetry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return err
}

func (m *Manager) appendCode(ctx context.Context, activation domain.Activation, phase domain.VerificationPhase, code string) (string, error) {
	payload, receivedAt := verificationProviderMetadata(ctx, code)
	params := storage.AppendVerificationParams{
		ActivationID: activation.ID, CycleNo: activation.SMSCycle, Phase: phase,
		Code: code, ProviderPayload: payload, ProviderReceivedAt: receivedAt,
	}
	owned, ok := m.store.(storage.OwnedVerificationStore)
	if !ok {
		return "", storage.ErrConflict
	}
	result, err := owned.AppendVerificationCodeOwned(ctx, params, activation.LeaseOwner, activation.LeaseVersion)
	if err != nil {
		return "", err
	}
	return result.Verification.Code, nil
}

func (m *Manager) newGoPayClient(phone string, batch domain.Batch, session *gopay.Session, proxyURL string) (*gopay.Client, error) {
	var options BatchOptions
	_ = json.Unmarshal(batch.Config, &options)
	if session != nil && session.ProxyURL != "" {
		proxyURL = session.ProxyURL
	}
	return gopay.NewClientForPhone(phone, gopay.Config{
		SSOBaseURL: m.cfg.SSOBaseURL, GoPayBaseURL: m.cfg.GoPayBaseURL,
		ProxyURL: proxyURL, Session: session,
	})
}

func (m *Manager) saveSession(ctx context.Context, phone string, session gopay.Session, pin string, status domain.AccountStatus, balance *float64) (domain.Account, error) {
	raw, err := session.Marshal()
	if err != nil {
		return domain.Account{}, err
	}
	encrypted, err := m.box.Seal(raw)
	if err != nil {
		return domain.Account{}, err
	}
	pinEncrypted, err := m.box.Seal([]byte(pin))
	if err != nil {
		return domain.Account{}, err
	}
	device, _ := json.Marshal(session.Device)
	now := time.Now().UTC()
	return m.store.UpsertAccount(ctx, storage.UpsertAccountParams{
		PhoneNumber: phone, Status: status, BalanceRP: balance,
		CredentialsEnc: encrypted, TargetPINEnc: pinEncrypted,
		TokenState: json.RawMessage(`{}`), DeviceState: device, Metadata: json.RawMessage(`{}`), LastLoginAt: &now,
	})
}

func (m *Manager) refreshAndPersistSession(
	ctx context.Context,
	activation domain.Activation,
	client *gopay.Client,
	targetPIN string,
	status domain.AccountStatus,
	balance *float64,
) error {
	if err := client.Refresh(ctx); err != nil {
		return err
	}
	_, err := m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, status, balance)
	return err
}

func (m *Manager) restoreGoPayClient(ctx context.Context, activation domain.Activation) (domain.Batch, domain.Account, *gopay.Client, error) {
	batch, err := m.store.GetBatch(ctx, activation.BatchID)
	if err != nil {
		return batch, domain.Account{}, nil, err
	}
	account, err := m.store.GetAccountByPhone(ctx, activation.PhoneNumber)
	if err != nil {
		return batch, account, nil, err
	}
	raw, err := m.box.Open(account.CredentialsEnc)
	if err != nil {
		return batch, account, nil, err
	}
	session, err := gopay.ParseSession(raw)
	if err != nil {
		return batch, account, nil, err
	}
	client, err := m.newGoPayClient(activation.PhoneNumber, batch, &session, session.ProxyURL)
	return batch, account, client, err
}

func (m *Manager) targetPIN(batch domain.Batch) (string, error) {
	plain, err := m.box.Open(batch.TargetPINEnc)
	if err != nil {
		return "", err
	}
	pin := string(plain)
	if err := domain.ValidatePIN(pin); err != nil {
		return "", err
	}
	return pin, nil
}

func gopayCountryCode(activation domain.Activation, batch domain.Batch) string {
	var provider smsbower.Activation
	if json.Unmarshal(activation.ProviderPayload, &provider) == nil {
		if value := normalizeDialCode(provider.CountryPhoneCode); value != "" {
			return value
		}
		if value := normalizeDialCode(provider.CountryCode); value != "" {
			return value
		}
	}
	var options BatchOptions
	_ = json.Unmarshal(batch.Config, &options)
	if value := normalizeDialCode(options.GoPayCountryCode); value != "" {
		return value
	}
	if batch.CountryCode == "6" {
		return "+62"
	}
	return "+62"
}

func normalizeDialCode(value string) string {
	digits := ""
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits += string(r)
		}
	}
	if digits == "" {
		return ""
	}
	return "+" + digits
}

func localPhone(phone, countryCode string) string {
	phoneDigits := strings.TrimPrefix(strings.TrimSpace(phone), "+")
	countryDigits := strings.TrimPrefix(countryCode, "+")
	return strings.TrimPrefix(phoneDigits, countryDigits)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "0"
}

func isHTTPStatus(err error, status int) bool {
	var httpErr *gopay.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}
