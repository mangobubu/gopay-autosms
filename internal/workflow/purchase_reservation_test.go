package workflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type purchaseReservationStore struct {
	storage.Store

	mu                sync.Mutex
	batch             domain.Batch
	setting           domain.Setting
	reservedToken     string
	attemptState      storage.PurchaseAttemptState
	reserveErr        error
	markSentErr       error
	createErr         error
	freezeErr         error
	freezeState       storage.PurchaseAttemptState
	cleanupPending    bool
	cleanupClaimed    bool
	cleanupCompleted  bool
	cleanupClaimLimit int
	reserveCalls      int
	activationCreates int
}

func (s *purchaseReservationStore) GetSetting(context.Context, string) (domain.Setting, error) {
	return s.setting, nil
}

func (s *purchaseReservationStore) ReserveBatchPurchase(_ context.Context, batchID int64, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserveCalls++
	if batchID != s.batch.ID || token == "" {
		return storage.ErrInvalidInput
	}
	if s.reserveErr != nil {
		return s.reserveErr
	}
	if s.reservedToken != "" {
		return storage.ErrPurchaseInProgress
	}
	if s.batch.FulfilledCount+s.batch.InflightCount+s.batch.PurchaseReservedCount >= s.batch.Quantity {
		return storage.ErrBatchCapacity
	}
	s.reservedToken = token
	s.attemptState = storage.PurchaseAttemptReserved
	s.batch.PurchaseReservedCount++
	return nil
}

func (s *purchaseReservationStore) MarkBatchPurchaseSent(_ context.Context, batchID int64, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markSentErr != nil {
		return s.markSentErr
	}
	if batchID != s.batch.ID || token != s.reservedToken || s.attemptState != storage.PurchaseAttemptReserved {
		return storage.ErrConflict
	}
	s.attemptState = storage.PurchaseAttemptSent
	return nil
}

func (s *purchaseReservationStore) ReleaseBatchPurchaseReservation(
	_ context.Context,
	batchID int64,
	token string,
	_ time.Time,
	_ string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if batchID != s.batch.ID {
		return storage.ErrConflict
	}
	if s.reservedToken == "" {
		return nil
	}
	if token != s.reservedToken {
		return storage.ErrConflict
	}
	s.reservedToken = ""
	s.attemptState = storage.PurchaseAttemptReleased
	s.batch.PurchaseReservedCount--
	return nil
}

func (s *purchaseReservationStore) FreezeBatchPurchase(
	_ context.Context,
	batchID int64,
	token, provider, providerID, reason string,
) (storage.PurchaseAttemptState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.freezeErr != nil {
		return "", s.freezeErr
	}
	if batchID != s.batch.ID || token != s.reservedToken {
		return "", storage.ErrConflict
	}
	state := s.freezeState
	if state == "" {
		state = storage.PurchaseAttemptUnknown
	}
	s.attemptState = state
	if state == storage.PurchaseAttemptCommitted {
		return state, nil
	}
	if state == storage.PurchaseAttemptUnknown && provider != "" && providerID != "" {
		s.cleanupPending = true
	}
	s.batch.Status = domain.BatchStatusFailed
	s.batch.FailureReason = reason
	return s.attemptState, nil
}

func (s *purchaseReservationStore) ClaimPurchaseCleanupAttempts(
	_ context.Context,
	owner string,
	_ time.Time,
	_ time.Duration,
	limit int,
) ([]storage.PurchaseCleanupAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupClaimLimit = limit
	if !s.cleanupPending || s.cleanupClaimed {
		return nil, nil
	}
	s.cleanupClaimed = true
	return []storage.PurchaseCleanupAttempt{{
		Token: s.reservedToken, BatchID: s.batch.ID, Provider: "smsbower",
		ProviderActivationID: "remote-2", LeaseOwner: owner, LeaseVersion: 1,
	}}, nil
}

func (s *purchaseReservationStore) CompletePurchaseCleanup(_ context.Context, token, _ string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cleanupPending || token != s.reservedToken {
		return storage.ErrConflict
	}
	s.cleanupPending = false
	s.cleanupClaimed = false
	s.cleanupCompleted = true
	return nil
}

func (s *purchaseReservationStore) RetryPurchaseCleanup(_ context.Context, token, _ string, _ int64, _ time.Time, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cleanupPending || token != s.reservedToken {
		return storage.ErrConflict
	}
	s.cleanupClaimed = false
	return nil
}

func (s *purchaseReservationStore) CreateActivationAtomically(
	_ context.Context,
	params storage.CreateActivationParams,
) (storage.CreateActivationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return storage.CreateActivationResult{}, s.createErr
	}
	if params.BatchID != s.batch.ID || params.PurchaseToken == "" || params.PurchaseToken != s.reservedToken ||
		s.attemptState != storage.PurchaseAttemptSent {
		return storage.CreateActivationResult{}, storage.ErrConflict
	}
	s.activationCreates++
	s.batch.PurchasedCount++
	s.batch.InflightCount++
	s.batch.PurchaseReservedCount--
	s.reservedToken = ""
	s.attemptState = storage.PurchaseAttemptCommitted
	return storage.CreateActivationResult{Activation: domain.Activation{
		ID:                   int64(s.activationCreates),
		BatchID:              params.BatchID,
		Provider:             params.Provider,
		ProviderActivationID: params.ProviderActivationID,
		PhoneNumber:          params.PhoneNumber,
	}}, nil
}

func newPurchaseTestManager(t *testing.T, store *purchaseReservationStore, providerURL string) *Manager {
	t.Helper()
	box, err := secure.New("purchase-reservation-test")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Seal([]byte("test-api-key"))
	if err != nil {
		t.Fatal(err)
	}
	settingValue, err := json.Marshal(map[string]string{
		"api_key_encrypted": base64.StdEncoding.EncodeToString(ciphertext),
	})
	if err != nil {
		t.Fatal(err)
	}
	store.setting = domain.Setting{Key: appsettings.SMSBowerKey, Value: settingValue}
	settings := appsettings.New(store, box, providerURL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, settings, box, Config{ActivationTTL: time.Minute}, logger)
}

func TestPurchaseReservationSerializesProviderCallsAcrossManagers(t *testing.T) {
	requestStarted := make(chan struct{})
	allowResponse := make(chan struct{})
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if action := r.URL.Query().Get("action"); action != "getNumberV2" {
			t.Errorf("供应商 action = %q，期望 getNumberV2", action)
			http.Error(w, "unexpected action", http.StatusBadRequest)
			return
		}
		if providerCalls.Add(1) == 1 {
			close(requestStarted)
			<-allowResponse
		}
		_, _ = fmt.Fprint(w, `{"activationId":"remote-1","phoneNumber":"628111222333","activationCost":"1.25","currency":"USD"}`)
	}))
	defer provider.Close()

	store := &purchaseReservationStore{
		batch: domain.Batch{
			ID: 17, Status: domain.BatchStatusRunning, ServiceCode: "ni", CountryCode: "6",
			MaxPriceAmount: "1.25", Currency: "USD", Quantity: 1,
		},
	}
	first := newPurchaseTestManager(t, store, provider.URL)
	second := newPurchaseTestManager(t, store, provider.URL)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		first.purchaseOne(context.Background(), store.batch)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("首个实例未发起供应商请求")
	}

	second.purchaseOne(context.Background(), store.batch)
	close(allowResponse)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("首个实例购买流程未结束")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := providerCalls.Load(); got != 1 {
		t.Fatalf("供应商调用次数 = %d，期望 1", got)
	}
	if store.reserveCalls != 2 {
		t.Fatalf("预占调用次数 = %d，期望 2", store.reserveCalls)
	}
	if store.activationCreates != 1 || store.batch.PurchasedCount != 1 {
		t.Fatalf("落库次数 = %d，已购买 = %d，期望均为 1", store.activationCreates, store.batch.PurchasedCount)
	}
	if store.reservedToken != "" || store.batch.PurchaseReservedCount != 0 {
		t.Fatalf("成功消费后仍存在预占：token=%q count=%d", store.reservedToken, store.batch.PurchaseReservedCount)
	}
}

func TestPurchaseDoesNotContactProviderWhenSendFenceLosesToStop(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		_, _ = fmt.Fprint(w, `{"activationId":"unexpected","phoneNumber":"628111222333"}`)
	}))
	defer provider.Close()

	store := &purchaseReservationStore{
		batch: domain.Batch{
			ID: 18, Status: domain.BatchStatusRunning, ServiceCode: "ni", CountryCode: "6",
			MaxPriceAmount: "1.25", Currency: "USD", Quantity: 1,
		},
		markSentErr: storage.ErrConflict,
	}
	manager := newPurchaseTestManager(t, store, provider.URL)
	manager.purchaseOne(context.Background(), store.batch)

	if got := providerCalls.Load(); got != 0 {
		t.Fatalf("发送栅栏失败后供应商调用次数 = %d，期望 0", got)
	}
}

func TestUncertainSendFenceReleasesQuotaBeforeAnyProviderCall(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		_, _ = fmt.Fprint(w, `{"activationId":"unexpected","phoneNumber":"628111222333"}`)
	}))
	defer provider.Close()

	store := &purchaseReservationStore{
		batch: domain.Batch{
			ID: 20, Status: domain.BatchStatusRunning, ServiceCode: "ni", CountryCode: "6",
			MaxPriceAmount: "1.25", Currency: "USD", Quantity: 1,
		},
		markSentErr: storage.ErrCommitUnknown,
	}
	manager := newPurchaseTestManager(t, store, provider.URL)
	manager.purchaseOne(context.Background(), store.batch)

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := providerCalls.Load(); got != 0 {
		t.Fatalf("发送回执未知后供应商调用次数 = %d，期望 0", got)
	}
	if store.batch.Status != domain.BatchStatusRunning || store.batch.PurchaseReservedCount != 0 ||
		store.attemptState != storage.PurchaseAttemptReleased {
		t.Fatalf("发送回执未知后未安全释放名额：batch=%+v state=%q", store.batch, store.attemptState)
	}
}

func TestUncertainAbsentReservationRetriesWithoutProviderCall(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		_, _ = fmt.Fprint(w, `{"activationId":"unexpected","phoneNumber":"628111222333"}`)
	}))
	defer provider.Close()

	store := &purchaseReservationStore{
		batch: domain.Batch{
			ID: 21, Status: domain.BatchStatusRunning, ServiceCode: "ni", CountryCode: "6",
			MaxPriceAmount: "1.25", Currency: "USD", Quantity: 1,
		},
		reserveErr: storage.ErrCommitUnknown,
	}
	manager := newPurchaseTestManager(t, store, provider.URL)
	manager.purchaseOne(context.Background(), store.batch)

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := providerCalls.Load(); got != 0 {
		t.Fatalf("预占回执未知后供应商调用次数 = %d，期望 0", got)
	}
	if store.batch.Status != domain.BatchStatusRunning || store.batch.PurchaseReservedCount != 0 {
		t.Fatalf("未提交的预占不应冻结任务或占用名额：%+v", store.batch)
	}
}

func TestUncertainPersistenceCancelsOnlyAfterLockedUnknownResolution(t *testing.T) {
	tests := []struct {
		name        string
		freezeState storage.PurchaseAttemptState
		freezeErr   error
		wantCancel  int32
	}{
		{name: "令牌已提交时保留远端号码", freezeState: storage.PurchaseAttemptCommitted},
		{name: "供应商号码已归属其他记录时保留远端号码", freezeState: storage.PurchaseAttemptConflicted},
		{name: "冻结提交仍未知时保留远端号码", freezeErr: storage.ErrCommitUnknown},
		{name: "锁定解析为未知时撤销远端号码", freezeState: storage.PurchaseAttemptUnknown, wantCancel: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cancelCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Query().Get("action") {
				case "getNumberV2":
					_, _ = fmt.Fprint(w, `{"activationId":"remote-2","phoneNumber":"628111222333","activationCost":"1.25","currency":"USD"}`)
				case "setStatus":
					if r.URL.Query().Get("status") == "8" {
						cancelCalls.Add(1)
					}
					_, _ = fmt.Fprint(w, "ACCESS_CANCEL")
				default:
					http.Error(w, "unexpected action", http.StatusBadRequest)
				}
			}))
			defer provider.Close()

			store := &purchaseReservationStore{
				batch: domain.Batch{
					ID: 19, Status: domain.BatchStatusRunning, ServiceCode: "ni", CountryCode: "6",
					MaxPriceAmount: "1.25", Currency: "USD", Quantity: 1,
				},
				createErr:   storage.ErrConflict,
				freezeErr:   tt.freezeErr,
				freezeState: tt.freezeState,
			}
			manager := newPurchaseTestManager(t, store, provider.URL)
			manager.purchaseOne(context.Background(), store.batch)
			manager.processPurchaseCleanups(context.Background())
			manager.Wait()

			if got := cancelCalls.Load(); got != tt.wantCancel {
				t.Fatalf("远端撤销次数 = %d，期望 %d", got, tt.wantCancel)
			}
			store.mu.Lock()
			completed := store.cleanupCompleted
			store.mu.Unlock()
			if completed != (tt.wantCancel == 1) {
				t.Fatalf("撤销待办完成状态 = %v，期望 %v", completed, tt.wantCancel == 1)
			}
		})
	}
}

func TestPurchaseCleanupDispatchDoesNotWaitForProvider(t *testing.T) {
	requestStarted := make(chan struct{})
	allowResponse := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(allowResponse) }) }

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "setStatus" {
			http.Error(w, "unexpected action", http.StatusBadRequest)
			return
		}
		close(requestStarted)
		<-allowResponse
		_, _ = fmt.Fprint(w, "ACCESS_CANCEL")
	}))
	defer provider.Close()
	defer unblock()

	store := &purchaseReservationStore{
		batch:          domain.Batch{ID: 22, Status: domain.BatchStatusFailed, Quantity: 1},
		reservedToken:  "cleanup-token",
		attemptState:   storage.PurchaseAttemptUnknown,
		cleanupPending: true,
	}
	manager := newPurchaseTestManager(t, store, provider.URL)
	dispatched := make(chan struct{})
	go func() {
		manager.processPurchaseCleanups(context.Background())
		close(dispatched)
	}()

	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("清理分派被远端撤销请求阻塞")
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("后台清理未发起远端撤销请求")
	}

	unblock()
	manager.Wait()
	store.mu.Lock()
	completed := store.cleanupCompleted
	claimLimit := store.cleanupClaimLimit
	store.mu.Unlock()
	if !completed {
		t.Fatal("后台清理完成后未持久化完成状态")
	}
	if claimLimit != purchaseCleanupWorkerCount {
		t.Fatalf("清理待办领取上限 = %d，期望 %d", claimLimit, purchaseCleanupWorkerCount)
	}
}
