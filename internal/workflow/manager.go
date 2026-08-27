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
	proxyaddr "github.com/mangobubu/gopay-autosms/internal/proxy"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

const purchaseCleanupWorkerCount = 4

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
		m.startErr = m.cancelStartupBatches(ctx)
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

func (m *Manager) CreateBatch(ctx context.Context, input CreateBatchInput) (domain.Batch, error) {
	if err := domain.ValidatePIN(input.PIN); err != nil {
		return domain.Batch{}, err
	}
	if input.Quantity <= 0 || strings.TrimSpace(input.ServiceCode) == "" || strings.TrimSpace(input.CountryCode) == "" || strings.TrimSpace(input.MaxPrice) == "" {
		return domain.Batch{}, storage.ErrInvalidInput
	}
	smsCfg, err := m.settings.GetSMSBower(ctx)
	if err != nil {
		return domain.Batch{}, err
	}
	if strings.TrimSpace(smsCfg.APIKey) == "" {
		return domain.Batch{}, fmt.Errorf("SMSBower API Key 尚未配置")
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
	var err error
	if item.Provider != "smsbower" {
		err = fmt.Errorf("unsupported purchase cleanup provider %q", item.Provider)
	} else {
		var client *smsbower.Client
		client, err = m.smsClient(ctx)
		if err == nil {
			_, err = client.SetStatus(ctx, item.ProviderActivationID, smsbower.SetStatusCancel)
		}
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

func batchReadyForPurchase(batch domain.Batch, now time.Time) bool {
	return !batch.Status.Terminal() &&
		batch.PurchaseReservedCount == 0 &&
		batch.PurchasedCount < batch.Quantity &&
		!batch.NextPurchaseAt.After(now)
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

func (m *Manager) smsClient(ctx context.Context) (*smsbower.Client, error) {
	cfg, err := m.settings.GetSMSBower(ctx)
	if err != nil {
		return nil, err
	}
	return smsbower.NewClient(smsbower.Config{APIKey: cfg.APIKey, BaseURL: cfg.BaseURL})
}

func (m *Manager) purchaseOne(ctx context.Context, batch domain.Batch) {
	client, err := m.smsClient(ctx)
	if err != nil {
		m.noteBatchError(ctx, batch, err)
		return
	}
	var options BatchOptions
	_ = json.Unmarshal(batch.Config, &options)
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
			m.logger.Error("SMSBower purchase result is unknown; stopping batch to prevent a duplicate purchase", "batch_id", batch.ID, "error", err)
			reason := "购买结果未知，已停止自动补购以避免重复扣费: " + err.Error()
			if _, freezeErr := m.freezePurchase(ctx, batch.ID, purchaseToken, "smsbower", "", reason); freezeErr != nil {
				m.logger.Error("freeze unknown purchase", "batch_id", batch.ID, "error", freezeErr)
			}
			return
		}
		m.logger.Warn("purchase attempt failed before allocation", "batch_id", batch.ID, "error", err)
		next := time.Now().UTC().Add(2 * time.Second)
		if releaseErr := m.releasePurchase(ctx, batch.ID, purchaseToken, next, err.Error()); releaseErr != nil {
			reason := "购买预占释放结果未知，已停止自动补购: " + releaseErr.Error()
			m.logger.Error("release failed purchase reservation", "batch_id", batch.ID, "error", releaseErr)
			if _, freezeErr := m.freezePurchase(ctx, batch.ID, purchaseToken, "smsbower", "", reason); freezeErr != nil &&
				!errors.Is(freezeErr, storage.ErrConflict) {
				m.logger.Error("freeze unreleased purchase", "batch_id", batch.ID, "error", freezeErr)
			}
		}
		return
	}
	payload, _ := json.Marshal(purchased)
	expires := time.Now().UTC().Add(m.cfg.ActivationTTL)
	params := storage.CreateActivationParams{
		PurchaseToken: purchaseToken, BatchID: batch.ID, Provider: "smsbower", ProviderActivationID: purchased.ActivationID,
		PhoneNumber: purchased.PhoneNumber, ServiceCode: batch.ServiceCode, CountryCode: batch.CountryCode,
		Operator: purchased.Operator, PurchasePriceAmount: firstNonEmpty(purchased.Cost, batch.MaxPriceAmount),
		Currency: firstNonEmpty(purchased.Currency, batch.Currency), ProviderPayload: payload,
		ProviderExpiresAt: &expires, NextRunAt: time.Now().UTC(),
	}
	created, err := m.persistPurchasedActivation(ctx, params)
	if err != nil {
		reason := fmt.Sprintf("SMSBower 号码 %s 落库异常，已停止自动补购: %v", purchased.ActivationID, err)
		resolvedState, freezeErr := m.freezePurchase(ctx, batch.ID, purchaseToken, "smsbower", purchased.ActivationID, reason)
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
	if created.Duplicate {
		m.logger.Info("historical number detected; cancellation queued", "activation_id", created.Activation.ID)
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
	if activation.ControlAction != domain.ControlActionNone {
		err = m.processControl(ctx, activation)
	} else {
		switch activation.Status {
		case domain.ActivationStatusPurchased:
			if m.activationExpired(activation) {
				err = m.expireAndCancel(ctx, activation)
			} else {
				err = m.probeAndStartLogin(ctx, activation)
			}
		case domain.ActivationStatusDuplicate, domain.ActivationStatusPINRequired, domain.ActivationStatusUnregistered, domain.ActivationStatusLoginFailed:
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
	err = m.finalizeLoginFailure(ctx, activation, err)
	if err != nil {
		m.logger.Warn("activation step failed", "activation_id", activation.ID, "state", activation.Status, "error", err)
		// Keep the last actionable reason with the activation so the dashboard
		// does not present a consumed/failed OTP as if it were still pending.
		// This is best-effort: lease ownership remains the source of truth.
		reason := err.Error()
		if len(reason) > 500 {
			reason = reason[:500]
		}
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

func (m *Manager) finalizeLoginFailure(ctx context.Context, activation domain.Activation, err error) error {
	if !errors.Is(err, gopay.ErrLoginFailed) {
		return err
	}
	return m.cancelAndClassify(ctx, activation, domain.ActivationStatusLoginFailed, "登录失败")
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

func (m *Manager) expireAndCancel(ctx context.Context, activation domain.Activation) error {
	client, err := m.smsClient(ctx)
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
	client, err := m.smsClient(ctx)
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
	if _, err = client.StartLogin(ctx); err != nil {
		return err
	}
	state := client.State()
	state.SMSCycle = activation.SMSCycle
	client.Restore(state)
	account, err := m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil)
	if err != nil {
		return err
	}
	if err = m.store.AttachActivationAccountOwned(ctx, activation.ID, account.ID, activation.LeaseOwner, activation.LeaseVersion); err != nil {
		return err
	}
	keepProxy = true
	_, err = m.store.TransitionActivationOwned(ctx, activation.ID, []domain.ActivationStatus{domain.ActivationStatusPurchased}, domain.ActivationStatusAwaitingLoginCode, "", activation.LeaseOwner, activation.LeaseVersion)
	return err
}

func (m *Manager) cancelAndClassify(ctx context.Context, activation domain.Activation, status domain.ActivationStatus, reason string) error {
	_, err := m.store.TransitionActivationOwned(ctx, activation.ID, nil, status, reason, activation.LeaseOwner, activation.LeaseVersion)
	if err != nil {
		return err
	}
	activation.Status = status
	return m.finalizeProviderAction(ctx, activation, smsbower.SetStatusCancel)
}

func (m *Manager) finalizeProviderAction(ctx context.Context, activation domain.Activation, action smsbower.SetStatus) error {
	client, err := m.smsClient(ctx)
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
	case gopay.LoginStageReady2FA:
		cycle := activation.SMSCycle
		if state.SMSCycle >= activation.SMSCycle {
			cycle, err = m.requestAnother(ctx, activation)
			if err != nil {
				return err
			}
		}
		state.LoginStage = gopay.LoginStageCycleReady2FA
		state.SMSCycle = cycle
		client.Restore(state)
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil)
		return err
	case gopay.LoginStageCycleReady2FA:
		if _, err = client.StartNextLoginOTP(ctx); err != nil {
			return err
		}
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil)
		return err
	}

	status, err := m.pollStatus(ctx, activation)
	if err != nil || status.Kind != smsbower.StatusOK {
		return err
	}
	storedCode, err := m.appendCode(ctx, activation, domain.VerificationPhaseLogin, status.Code)
	if err != nil {
		return err
	}
	result, err := client.VerifyLoginOTP(ctx, storedCode)
	if err != nil {
		return err
	}
	state = client.State()
	state.SMSCycle = activation.SMSCycle
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
	client.Restore(state)
	if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, &amount); err != nil {
		return err
	}
	_, err = m.store.TransitionActivationOwned(ctx, activation.ID, []domain.ActivationStatus{domain.ActivationStatusCheckingBalance}, domain.ActivationStatusAwaitingPINCode, "", activation.LeaseOwner, activation.LeaseVersion)
	return err
}

func (m *Manager) pollPINCode(ctx context.Context, activation domain.Activation) error {
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
		cycle := activation.SMSCycle
		if state.SMSCycle >= activation.SMSCycle {
			cycle, err = m.requestAnother(ctx, activation)
			if err != nil {
				return err
			}
		}
		state.PINStage = gopay.PINStageCycleReady
		state.SMSCycle = cycle
		client.Restore(state)
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
		return err
	case gopay.PINStageCycleReady:
		if _, err = client.StartPINSetup(ctx, targetPIN); err != nil {
			if isHTTPStatus(err, 401) {
				if refreshErr := m.refreshAndPersistSession(ctx, activation, client, targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); refreshErr != nil {
					err = refreshErr
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
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
		return err
	case gopay.PINStageResetReadyCycle:
		cycle := activation.SMSCycle
		if state.SMSCycle >= activation.SMSCycle {
			cycle, err = m.requestAnother(ctx, activation)
			if err != nil {
				return err
			}
		}
		state.PINStage = gopay.PINStageResetCycleReady
		state.SMSCycle = cycle
		client.Restore(state)
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
		return err
	case gopay.PINStageResetCycleReady:
		if _, err = client.StartPINReset(ctx, targetPIN); err != nil {
			if isHTTPStatus(err, 401) {
				if refreshErr := m.refreshAndPersistSession(ctx, activation, client, targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); refreshErr != nil {
					err = refreshErr
				} else {
					_, err = client.StartPINReset(ctx, targetPIN)
				}
			}
			if err != nil {
				return err
			}
		}
		_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
		return err
	case gopay.PINStageSetupVerified, gopay.PINStageResetVerified, gopay.PINStageComplete:
		return m.publishPINSettingState(ctx, activation)
	}

	status, err := m.pollStatus(ctx, activation)
	if err != nil || status.Kind != smsbower.StatusOK {
		return err
	}
	storedCode, err := m.appendCode(ctx, activation, domain.VerificationPhasePIN, status.Code)
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
	state.SMSCycle = activation.SMSCycle
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
	client.Restore(state)
	_, err := m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
	return err
}

func (m *Manager) completePINSetting(ctx context.Context, activation domain.Activation) error {
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
	if _, err := m.store.TransitionActivationOwned(ctx, activation.ID,
		[]domain.ActivationStatus{domain.ActivationStatusPINChanged},
		domain.ActivationStatusAwaitingSubsequentCode, "",
		activation.LeaseOwner, activation.LeaseVersion); err != nil {
		return err
	}
	now := time.Now().UTC()
	return m.store.TouchActivationPoll(ctx, activation.ID, activation.LeaseOwner, now, now)
}

func (m *Manager) pollFollowupCode(ctx context.Context, activation domain.Activation) error {
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
	client, err := m.smsClient(ctx)
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
		_, err = m.store.TransitionActivationOwned(ctx, activation.ID, nil, domain.ActivationStatusExpired, "SMSBower 激活已结束", activation.LeaseOwner, activation.LeaseVersion)
		return status, err
	case smsbower.StatusOK:
		return status, nil
	default:
		return status, fmt.Errorf("unknown SMSBower status %q", status.Kind)
	}
}

func (m *Manager) requestAnother(ctx context.Context, activation domain.Activation) (int, error) {
	client, err := m.smsClient(ctx)
	if err != nil {
		return 0, err
	}
	if _, err = client.SetStatus(ctx, activation.ProviderActivationID, smsbower.SetStatusRequestAnother); err != nil {
		return 0, err
	}
	return m.store.AdvanceSMSCycle(ctx, activation.ID, activation.LeaseOwner, time.Now().UTC().Add(m.cfg.PollInterval))
}

func (m *Manager) appendCode(ctx context.Context, activation domain.Activation, phase domain.VerificationPhase, code string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"code": code})
	now := time.Now().UTC()
	params := storage.AppendVerificationParams{
		ActivationID: activation.ID, CycleNo: activation.SMSCycle, Phase: phase,
		Code: code, ProviderPayload: payload, ProviderReceivedAt: &now,
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
