package herotask

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/herosms"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type storeStub struct {
	createFn               func(context.Context, storage.CreateHeroSMSTasksParams) ([]domain.HeroSMSNumberTask, error)
	getFn                  func(context.Context, int64) (domain.HeroSMSNumberTask, error)
	listFn                 func(context.Context, storage.HeroSMSTaskFilter) ([]domain.HeroSMSNumberTask, error)
	claimFn                func(context.Context, string, time.Time, time.Duration, int) ([]domain.HeroSMSNumberTask, error)
	beginFn                func(context.Context, int64, string, int64, string) (domain.HeroSMSNumberTask, error)
	releasePurchaseFn      func(context.Context, int64, string, int64, string, time.Time, string) (domain.HeroSMSNumberTask, error)
	scheduleFn             func(context.Context, int64, string, int64, domain.HeroSMSNumberTaskStatus, time.Time, string) (domain.HeroSMSNumberTask, error)
	commitFn               func(context.Context, int64, string, int64, storage.CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error)
	recoverPurchaseFn      func(context.Context, int64, string, storage.CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error)
	markUnknownFn          func(context.Context, int64, string, int64, storage.MarkHeroSMSPurchaseUnknownParams) (domain.HeroSMSNumberTask, error)
	requestStopFn          func(context.Context, int64) (domain.HeroSMSNumberTask, error)
	restartFn              func(context.Context, int64, time.Time) (domain.HeroSMSNumberTask, error)
	prepareSettlementFn    func(context.Context, int64, string, int64, time.Time) (domain.HeroSMSNumberTask, error)
	beginContinuationFn    func(context.Context, int64, string, int64, time.Time) (domain.HeroSMSNumberTask, error)
	completeContinuationFn func(context.Context, int64, string, int64, int, time.Time) (domain.HeroSMSNumberTask, error)
	abortContinuationFn    func(context.Context, int64, string, int64, int, time.Time, string) (domain.HeroSMSNumberTask, error)
	finishFn               func(context.Context, int64, string, int64, domain.HeroSMSNumberTaskStatus, domain.HeroSMSRefundStatus, string) (domain.HeroSMSNumberTask, error)
	appendMessageFn        func(context.Context, storage.AppendHeroSMSTaskMessageParams) (storage.AppendHeroSMSTaskMessageResult, error)
	listTaskMessagesFn     func(context.Context, int64) ([]domain.HeroSMSNumberMessage, error)
}

func (s *storeStub) CreateHeroSMSTasks(ctx context.Context, params storage.CreateHeroSMSTasksParams) ([]domain.HeroSMSNumberTask, error) {
	if s.createFn == nil {
		panic("unexpected CreateHeroSMSTasks")
	}
	return s.createFn(ctx, params)
}

func (s *storeStub) GetHeroSMSTask(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	if s.getFn == nil {
		panic("unexpected GetHeroSMSTask")
	}
	return s.getFn(ctx, id)
}

func (s *storeStub) ListHeroSMSTasks(ctx context.Context, filter storage.HeroSMSTaskFilter) ([]domain.HeroSMSNumberTask, error) {
	if s.listFn == nil {
		panic("unexpected ListHeroSMSTasks")
	}
	return s.listFn(ctx, filter)
}

func (s *storeStub) ClaimDueHeroSMSTasks(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]domain.HeroSMSNumberTask, error) {
	if s.claimFn == nil {
		panic("unexpected ClaimDueHeroSMSTasks")
	}
	return s.claimFn(ctx, owner, now, lease, limit)
}

func (s *storeStub) BeginHeroSMSPurchaseOwned(ctx context.Context, id int64, owner string, version int64, token string) (domain.HeroSMSNumberTask, error) {
	if s.beginFn == nil {
		panic("unexpected BeginHeroSMSPurchaseOwned")
	}
	return s.beginFn(ctx, id, owner, version, token)
}

func (s *storeStub) ReleaseHeroSMSPurchaseOwned(ctx context.Context, id int64, owner string, version int64, token string, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
	if s.releasePurchaseFn == nil {
		panic("unexpected ReleaseHeroSMSPurchaseOwned")
	}
	return s.releasePurchaseFn(ctx, id, owner, version, token, next, lastError)
}

func (s *storeStub) ScheduleHeroSMSTaskOwned(ctx context.Context, id int64, owner string, version int64, status domain.HeroSMSNumberTaskStatus, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
	if s.scheduleFn == nil {
		panic("unexpected ScheduleHeroSMSTaskOwned")
	}
	return s.scheduleFn(ctx, id, owner, version, status, next, lastError)
}

func (s *storeStub) CommitHeroSMSPurchaseOwned(ctx context.Context, id int64, owner string, version int64, params storage.CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error) {
	if s.commitFn == nil {
		panic("unexpected CommitHeroSMSPurchaseOwned")
	}
	return s.commitFn(ctx, id, owner, version, params)
}

func (s *storeStub) RecoverHeroSMSPurchaseOutcome(ctx context.Context, id int64, token string, params storage.CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error) {
	if s.recoverPurchaseFn == nil {
		panic("unexpected RecoverHeroSMSPurchaseOutcome")
	}
	return s.recoverPurchaseFn(ctx, id, token, params)
}

func (s *storeStub) MarkHeroSMSPurchaseUnknownOwned(ctx context.Context, id int64, owner string, version int64, params storage.MarkHeroSMSPurchaseUnknownParams) (domain.HeroSMSNumberTask, error) {
	if s.markUnknownFn == nil {
		panic("unexpected MarkHeroSMSPurchaseUnknownOwned")
	}
	return s.markUnknownFn(ctx, id, owner, version, params)
}

func (s *storeStub) RequestHeroSMSTaskStop(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	if s.requestStopFn == nil {
		panic("unexpected RequestHeroSMSTaskStop")
	}
	return s.requestStopFn(ctx, id)
}

func (s *storeStub) RestartHeroSMSTask(ctx context.Context, id int64, next time.Time) (domain.HeroSMSNumberTask, error) {
	if s.restartFn == nil {
		panic("unexpected RestartHeroSMSTask")
	}
	return s.restartFn(ctx, id, next)
}

func (s *storeStub) PrepareHeroSMSTaskSettlementOwned(ctx context.Context, id int64, owner string, version int64, now time.Time) (domain.HeroSMSNumberTask, error) {
	if s.prepareSettlementFn == nil {
		panic("unexpected PrepareHeroSMSTaskSettlementOwned")
	}
	return s.prepareSettlementFn(ctx, id, owner, version, now)
}

func (s *storeStub) CompleteHeroSMSContinuationOwned(ctx context.Context, id int64, owner string, version int64, observedMessageCount int, next time.Time) (domain.HeroSMSNumberTask, error) {
	if s.completeContinuationFn == nil {
		panic("unexpected CompleteHeroSMSContinuationOwned")
	}
	return s.completeContinuationFn(ctx, id, owner, version, observedMessageCount, next)
}

func (s *storeStub) BeginHeroSMSContinuationOwned(ctx context.Context, id int64, owner string, version int64, now time.Time) (domain.HeroSMSNumberTask, error) {
	if s.beginContinuationFn == nil {
		panic("unexpected BeginHeroSMSContinuationOwned")
	}
	return s.beginContinuationFn(ctx, id, owner, version, now)
}

func (s *storeStub) AbortHeroSMSContinuationOwned(ctx context.Context, id int64, owner string, version int64, target int, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
	if s.abortContinuationFn == nil {
		panic("unexpected AbortHeroSMSContinuationOwned")
	}
	return s.abortContinuationFn(ctx, id, owner, version, target, next, lastError)
}

func (s *storeStub) FinishHeroSMSTaskOwned(ctx context.Context, id int64, owner string, version int64, status domain.HeroSMSNumberTaskStatus, refund domain.HeroSMSRefundStatus, lastError string) (domain.HeroSMSNumberTask, error) {
	if s.finishFn == nil {
		panic("unexpected FinishHeroSMSTaskOwned")
	}
	return s.finishFn(ctx, id, owner, version, status, refund, lastError)
}

func (s *storeStub) AppendHeroSMSTaskMessage(ctx context.Context, params storage.AppendHeroSMSTaskMessageParams) (storage.AppendHeroSMSTaskMessageResult, error) {
	if s.appendMessageFn == nil {
		panic("unexpected AppendHeroSMSTaskMessage")
	}
	return s.appendMessageFn(ctx, params)
}

func (s *storeStub) ListHeroSMSTaskMessages(ctx context.Context, id int64) ([]domain.HeroSMSNumberMessage, error) {
	if s.listTaskMessagesFn == nil {
		panic("unexpected ListHeroSMSTaskMessages")
	}
	return s.listTaskMessagesFn(ctx, id)
}

type clientStub struct {
	purchaseFn       func(context.Context, herosms.PurchaseRequest) (herosms.Purchase, error)
	messagesFn       func(context.Context, string, bool) ([]herosms.Message, error)
	requestAnotherFn func(context.Context, string) error
	finishFn         func(context.Context, string, bool) error
	cancelFn         func(context.Context, string, bool) error
}

func (c *clientStub) PurchaseOne(ctx context.Context, request herosms.PurchaseRequest) (herosms.Purchase, error) {
	if c.purchaseFn == nil {
		panic("unexpected PurchaseOne")
	}
	return c.purchaseFn(ctx, request)
}

func (c *clientStub) GetMessages(ctx context.Context, id string, rent bool) ([]herosms.Message, error) {
	if c.messagesFn == nil {
		panic("unexpected GetMessages")
	}
	return c.messagesFn(ctx, id, rent)
}

func (c *clientStub) RequestAnother(ctx context.Context, id string) error {
	if c.requestAnotherFn == nil {
		panic("unexpected RequestAnother")
	}
	return c.requestAnotherFn(ctx, id)
}

func (c *clientStub) Finish(ctx context.Context, id string, rent bool) error {
	if c.finishFn == nil {
		panic("unexpected Finish")
	}
	return c.finishFn(ctx, id, rent)
}

func (c *clientStub) Cancel(ctx context.Context, id string, rent bool) error {
	if c.cancelFn == nil {
		panic("unexpected Cancel")
	}
	return c.cancelFn(ctx, id, rent)
}

func testManager(store Store, client Client, now time.Time) *Manager {
	return New(store, client, Config{
		Now:                       func() time.Time { return now },
		StockRetryMinimum:         time.Second,
		StockRetryMaximum:         8 * time.Second,
		ErrorRetryMinimum:         2 * time.Second,
		ErrorRetryMaximum:         16 * time.Second,
		RefundWindow:              20 * time.Minute,
		DefaultActivationLifetime: 20 * time.Minute,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func leasedWaitingTask(now time.Time, duration *int) domain.HeroSMSNumberTask {
	leaseUntil := now.Add(time.Minute)
	kind := domain.HeroSMSProductActivation
	if duration != nil {
		kind = domain.HeroSMSProductRent
	}
	return domain.HeroSMSNumberTask{
		ID: 7, Status: domain.HeroSMSTaskWaitingNumber, ProductKind: kind,
		ServiceCode: "wa", CountryCode: "6", VerificationType: "sms",
		DurationHours: duration, MaxPriceAmount: "1.25", Currency: "USD",
		LeaseOwner: "worker:7:1", LeaseVersion: 1, LeaseUntil: &leaseUntil,
		ProviderPayload: json.RawMessage(`{"operator":"any"}`),
	}
}

func TestPurchasePersistsProviderExpiryAndRefundWindowForEveryProduct(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		duration         *int
		verification     herosms.VerificationType
		canGetAnotherSMS bool
		wantContinuation bool
	}{
		{name: "activation SMS supports continuation", verification: herosms.VerificationSMS, canGetAnotherSMS: true, wantContinuation: true},
		{name: "activation call never continues", verification: herosms.VerificationCall, canGetAnotherSMS: true},
		{name: "activation without provider flag", verification: herosms.VerificationSMS},
		{name: "rent never continues", duration: intPointer(24), verification: herosms.VerificationSMS, canGetAnotherSMS: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := leasedWaitingTask(now, test.duration)
			task.VerificationType = string(test.verification)
			var beginToken string
			var committed storage.CommitHeroSMSPurchaseParams
			store := &storeStub{}
			store.beginFn = func(_ context.Context, id int64, owner string, version int64, token string) (domain.HeroSMSNumberTask, error) {
				if id != task.ID || owner != task.LeaseOwner || version != task.LeaseVersion || token == "" {
					t.Fatalf("unexpected begin fence: id=%d owner=%q version=%d token=%q", id, owner, version, token)
				}
				beginToken = token
				task.Status = domain.HeroSMSTaskPurchasing
				task.PurchaseToken = token
				return task, nil
			}
			store.commitFn = func(_ context.Context, _ int64, _ string, _ int64, params storage.CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error) {
				committed = params
				return domain.HeroSMSNumberTask{Status: domain.HeroSMSTaskActive}, nil
			}
			client := &clientStub{purchaseFn: func(_ context.Context, request herosms.PurchaseRequest) (herosms.Purchase, error) {
				wantDuration := 0
				if test.duration != nil {
					wantDuration = *test.duration
				}
				if request.DurationHours != wantDuration || request.Ref != beginToken || request.Operator != "any" ||
					request.VerificationType != test.verification {
					t.Fatalf("purchase request mismatch: %+v", request)
				}
				return herosms.Purchase{
					ActivationID: "provider-7", PhoneNumber: "+628123", Cost: "0.75", Currency: "usd",
					CanGetAnotherSMS: test.canGetAnotherSMS,
					ActivatedAt:      now.Add(-time.Minute), ExpiresAt: now.Add(time.Duration(wantDuration+2) * time.Hour),
					Raw: json.RawMessage(`{"activationId":"provider-7"}`),
				}, nil
			}}
			manager := testManager(store, client, now)
			manager.processTask(context.Background(), task)
			if committed.PurchaseToken != beginToken || committed.ProviderActivationID != "provider-7" || committed.PhoneNumber != "+628123" {
				t.Fatalf("purchase identity was not committed: %+v", committed)
			}
			wantRefund := now.Add(-time.Minute).Add(20 * time.Minute)
			if committed.RefundableUntil == nil || !committed.RefundableUntil.Equal(wantRefund) {
				t.Fatalf("refund deadline = %v, want %v", committed.RefundableUntil, wantRefund)
			}
			if committed.ExpiresAt == nil || !committed.ExpiresAt.Equal(now.Add(time.Duration(durationValue(test.duration)+2)*time.Hour)) {
				t.Fatalf("provider expiry was not preserved: %v", committed.ExpiresAt)
			}
			if committed.RefundStatus != domain.HeroSMSRefundRefundable {
				t.Fatalf("refund status = %q", committed.RefundStatus)
			}
			if committed.SupportsContinuation != test.wantContinuation {
				t.Fatalf("supports continuation = %v, want %v", committed.SupportsContinuation, test.wantContinuation)
			}
		})
	}
}

func TestCreateParamsAcceptsActivationCallAndRejectsRentalCall(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 30, 0, 0, time.UTC)
	manager := testManager(&storeStub{}, &clientStub{}, now)
	params, err := manager.createParams(CreateTasksInput{
		ServiceCode: "wa", CountryCode: "6", VerificationType: herosms.VerificationCall, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("activation call rejected: %v", err)
	}
	if params.VerificationType != string(herosms.VerificationCall) || params.ProductKind != domain.HeroSMSProductActivation {
		t.Fatalf("unexpected call params: %+v", params)
	}
	duration := 24
	_, err = manager.createParams(CreateTasksInput{
		ServiceCode: "wa", CountryCode: "6", VerificationType: herosms.VerificationCall,
		DurationHours: &duration, Quantity: 1,
	})
	if !errors.Is(err, storage.ErrInvalidInput) {
		t.Fatalf("rental call error = %v, want invalid input", err)
	}
}

func TestNoNumbersReleasesPurchaseFenceForRetry(t *testing.T) {
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	task := leasedWaitingTask(now, nil)
	var token string
	released := false
	store := &storeStub{
		beginFn: func(_ context.Context, _ int64, _ string, _ int64, value string) (domain.HeroSMSNumberTask, error) {
			token = value
			task.Status = domain.HeroSMSTaskPurchasing
			task.PurchaseToken = value
			return task, nil
		},
		releasePurchaseFn: func(_ context.Context, _ int64, _ string, _ int64, value string, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
			released = true
			if value != token || !next.After(now) || lastError == "" {
				t.Fatalf("invalid release: token=%q next=%v error=%q", value, next, lastError)
			}
			return domain.HeroSMSNumberTask{Status: domain.HeroSMSTaskWaitingNumber}, nil
		},
	}
	client := &clientStub{purchaseFn: func(context.Context, herosms.PurchaseRequest) (herosms.Purchase, error) {
		return herosms.Purchase{}, herosms.ErrNoNumbers
	}}
	testManager(store, client, now).processTask(context.Background(), task)
	if !released {
		t.Fatal("conclusive no-number result did not release purchase fence")
	}
}

func TestAmbiguousPurchaseFreezesWithoutReleaseOrRepurchase(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	task := leasedWaitingTask(now, nil)
	var token string
	marked := false
	store := &storeStub{
		beginFn: func(_ context.Context, _ int64, _ string, _ int64, value string) (domain.HeroSMSNumberTask, error) {
			token = value
			task.Status = domain.HeroSMSTaskPurchasing
			task.PurchaseToken = value
			return task, nil
		},
		markUnknownFn: func(_ context.Context, _ int64, _ string, _ int64, params storage.MarkHeroSMSPurchaseUnknownParams) (domain.HeroSMSNumberTask, error) {
			marked = true
			if params.PurchaseToken != token || params.LastError == "" {
				t.Fatalf("invalid unknown purchase params: %+v", params)
			}
			return domain.HeroSMSNumberTask{Status: domain.HeroSMSTaskPurchaseUnknown}, nil
		},
	}
	client := &clientStub{purchaseFn: func(context.Context, herosms.PurchaseRequest) (herosms.Purchase, error) {
		return herosms.Purchase{}, &smsbower.PurchaseUnknownError{Action: "getNumber", Cause: errors.New("connection reset")}
	}}
	testManager(store, client, now).processTask(context.Background(), task)
	if !marked {
		t.Fatal("ambiguous purchase was not frozen")
	}

	purchaseCalls := 0
	store = &storeStub{scheduleFn: func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, _ time.Time, _ string) (domain.HeroSMSNumberTask, error) {
		if status != domain.HeroSMSTaskPurchaseUnknown {
			t.Fatalf("unexpected status %q", status)
		}
		return domain.HeroSMSNumberTask{Status: status}, nil
	}}
	client = &clientStub{purchaseFn: func(context.Context, herosms.PurchaseRequest) (herosms.Purchase, error) {
		purchaseCalls++
		return herosms.Purchase{}, nil
	}}
	unknown := leasedWaitingTask(now, nil)
	unknown.Status = domain.HeroSMSTaskPurchaseUnknown
	testManager(store, client, now).processTask(context.Background(), unknown)
	if purchaseCalls != 0 {
		t.Fatalf("purchase_unknown issued %d new purchases", purchaseCalls)
	}
}

func TestIdleActiveTaskPollsAndSchedulesFortyFiveSecondFallback(t *testing.T) {
	now := time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC)
	task := activeTask(now)
	scheduled := false
	store := &storeStub{
		scheduleFn: func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
			scheduled = true
			if status != domain.HeroSMSTaskActive || !next.Equal(now.Add(45*time.Second)) || lastError != "" {
				t.Fatalf("unexpected fallback schedule: status=%s next=%v error=%q", status, next, lastError)
			}
			return task, nil
		},
	}
	polls := 0
	client := &clientStub{messagesFn: func(_ context.Context, id string, rent bool) ([]herosms.Message, error) {
		polls++
		if id != task.ProviderActivationID || rent {
			t.Fatalf("unexpected activation poll: id=%q rent=%v", id, rent)
		}
		return []herosms.Message{}, nil
	}}
	testManager(store, client, now).processTask(context.Background(), task)
	if polls != 1 {
		t.Fatalf("provider polls = %d, want 1", polls)
	}
	if !scheduled {
		t.Fatal("active task fallback deadline was not scheduled")
	}
}

func TestWebhookWakeRequestsAnotherWithoutProviderPoll(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	task := activeTask(now)
	wake := now
	task.WebhookWakeupAt = &wake
	task.MessageCount = 1
	store := &storeStub{
		beginContinuationFn: func(_ context.Context, _ int64, _ string, _ int64, _ time.Time) (domain.HeroSMSNumberTask, error) {
			task.ContinuationPendingCount = 1
			return task, nil
		},
		completeContinuationFn: func(_ context.Context, _ int64, _ string, _ int64, target int, _ time.Time) (domain.HeroSMSNumberTask, error) {
			if target != 1 {
				t.Fatalf("continuation target = %d", target)
			}
			return domain.HeroSMSNumberTask{Status: domain.HeroSMSTaskActive}, nil
		},
	}
	requestAnother := 0
	polls := 0
	client := &clientStub{
		messagesFn: func(context.Context, string, bool) ([]herosms.Message, error) {
			polls++
			return nil, nil
		},
		requestAnotherFn: func(context.Context, string) error {
			requestAnother++
			return nil
		},
	}
	testManager(store, client, now).processTask(context.Background(), task)
	if requestAnother != 1 {
		t.Fatalf("RequestAnother calls = %d, want 1", requestAnother)
	}
	if polls != 0 {
		t.Fatalf("webhook wake performed %d provider polls", polls)
	}
}

func TestRentFallbackPollUsesGetAllSMSMode(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 15, 0, 0, time.UTC)
	duration := 24
	task := activeTask(now)
	task.ProductKind = domain.HeroSMSProductRent
	task.DurationHours = &duration
	store := &storeStub{scheduleFn: func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, next time.Time, _ string) (domain.HeroSMSNumberTask, error) {
		if status != domain.HeroSMSTaskActive || !next.Equal(now.Add(45*time.Second)) {
			t.Fatalf("unexpected rent fallback schedule: %s %v", status, next)
		}
		return task, nil
	}}
	polls := 0
	client := &clientStub{messagesFn: func(_ context.Context, id string, rent bool) ([]herosms.Message, error) {
		polls++
		if id != task.ProviderActivationID || !rent {
			t.Fatalf("rent fallback called with id=%q rent=%v", id, rent)
		}
		return []herosms.Message{}, nil
	}}
	testManager(store, client, now).processTask(context.Background(), task)
	if polls != 1 {
		t.Fatalf("rent provider polls = %d, want 1", polls)
	}
}

func TestFallbackPollFailureUsesPollIntervalFloorAndDeadlineClamps(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 20, 0, 0, time.UTC)
	tests := []struct {
		name      string
		expiresIn time.Duration
		refundIn  time.Duration
		wantDelay time.Duration
	}{
		{name: "poll interval floor", expiresIn: time.Hour, refundIn: 10 * time.Minute, wantDelay: 45 * time.Second},
		{name: "expiry clamp", expiresIn: 20 * time.Second, refundIn: 10 * time.Minute, wantDelay: 20 * time.Second},
		{name: "refund deadline clamp", expiresIn: time.Hour, refundIn: 30 * time.Second, wantDelay: 30 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := activeTask(now)
			task.ExpiresAt = timePointer(now.Add(test.expiresIn))
			task.RefundableUntil = timePointer(now.Add(test.refundIn))
			store := &storeStub{scheduleFn: func(
				_ context.Context, _ int64, _ string, _ int64,
				status domain.HeroSMSNumberTaskStatus, next time.Time, lastError string,
			) (domain.HeroSMSNumberTask, error) {
				if status != domain.HeroSMSTaskActive || !next.Equal(now.Add(test.wantDelay)) ||
					!strings.Contains(lastError, "temporary poll outage") {
					t.Fatalf("poll retry = %s %v %q, want delay %v", status, next, lastError, test.wantDelay)
				}
				return task, nil
			}}
			client := &clientStub{messagesFn: func(context.Context, string, bool) ([]herosms.Message, error) {
				return nil, errors.New("temporary poll outage")
			}}
			testManager(store, client, now).processTask(context.Background(), task)
		})
	}
}

func TestActivationFallbackPollAppendsAndDurablyContinues(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 30, 0, 0, time.UTC)
	task := activeTask(now)
	receivedAt := now.Add(-time.Second)
	appends, continuations := 0, 0
	store := &storeStub{
		appendMessageFn: func(_ context.Context, params storage.AppendHeroSMSTaskMessageParams) (storage.AppendHeroSMSTaskMessageResult, error) {
			appends++
			if params.TaskID == nil || *params.TaskID != task.ID ||
				params.ProviderActivationID != task.ProviderActivationID ||
				params.ProviderMessageID != "poll-message-1" || params.Source != domain.HeroSMSMessagePoll ||
				params.Code != "123456" || params.Text != "verification 123456" ||
				params.ProviderReceivedAt == nil || !params.ProviderReceivedAt.Equal(receivedAt) {
				t.Fatalf("unexpected polled append: %+v", params)
			}
			updated := task
			updated.MessageCount = 1
			updated.RefundStatus = domain.HeroSMSRefundUnavailable
			return storage.AppendHeroSMSTaskMessageResult{Inserted: true, Task: &updated}, nil
		},
		beginContinuationFn: func(_ context.Context, id int64, owner string, version int64, at time.Time) (domain.HeroSMSNumberTask, error) {
			if id != task.ID || owner != task.LeaseOwner || version != task.LeaseVersion || !at.Equal(now) {
				t.Fatalf("unexpected continuation begin: id=%d owner=%q version=%d at=%v", id, owner, version, at)
			}
			updated := task
			updated.MessageCount = 1
			updated.RefundStatus = domain.HeroSMSRefundUnavailable
			updated.ContinuationPendingCount = 1
			return updated, nil
		},
		completeContinuationFn: func(_ context.Context, id int64, owner string, version int64, observed int, next time.Time) (domain.HeroSMSNumberTask, error) {
			continuations++
			if id != task.ID || owner != task.LeaseOwner || version != task.LeaseVersion || observed != 1 ||
				!next.Equal(now.Add(45*time.Second)) {
				t.Fatalf("unexpected continuation completion: id=%d owner=%q version=%d observed=%d next=%v", id, owner, version, observed, next)
			}
			updated := task
			updated.MessageCount = 1
			updated.ContinuationCount = 1
			return updated, nil
		},
	}
	polls, requestAnother := 0, 0
	client := &clientStub{
		messagesFn: func(_ context.Context, id string, rent bool) ([]herosms.Message, error) {
			polls++
			if id != task.ProviderActivationID || rent {
				t.Fatalf("activation fallback called with id=%q rent=%v", id, rent)
			}
			return []herosms.Message{{
				ID: "poll-message-1", Code: "123456", Text: "verification 123456",
				ReceivedAt: receivedAt, Raw: json.RawMessage(`{"code":"123456"}`),
			}}, nil
		},
		requestAnotherFn: func(_ context.Context, id string) error {
			requestAnother++
			if id != task.ProviderActivationID {
				t.Fatalf("unexpected continuation activation %q", id)
			}
			return nil
		},
	}
	testManager(store, client, now).processTask(context.Background(), task)
	if polls != 1 || appends != 1 || requestAnother != 1 || continuations != 1 {
		t.Fatalf("polls=%d appends=%d requestAnother=%d continuations=%d, want 1 each", polls, appends, requestAnother, continuations)
	}
}

func TestPollStopBarrierSettlesBeforeRequestAnother(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 45, 0, 0, time.UTC)
	task := activeTask(now)
	pollStarted := make(chan struct{})
	stopRequested := make(chan struct{})
	releasePoll := make(chan struct{})
	appends, prepares, localFinishes := 0, 0, 0
	store := &storeStub{
		appendMessageFn: func(_ context.Context, params storage.AppendHeroSMSTaskMessageParams) (storage.AppendHeroSMSTaskMessageResult, error) {
			appends++
			select {
			case <-stopRequested:
			default:
				t.Fatal("message append happened before the stop barrier")
			}
			if params.TaskID == nil || *params.TaskID != task.ID {
				t.Fatalf("append task id = %v", params.TaskID)
			}
			updated := task
			updated.MessageCount = 1
			updated.StopRequested = true
			updated.RefundStatus = domain.HeroSMSRefundUnavailable
			return storage.AppendHeroSMSTaskMessageResult{Inserted: true, Task: &updated}, nil
		},
		prepareSettlementFn: func(_ context.Context, id int64, owner string, version int64, at time.Time) (domain.HeroSMSNumberTask, error) {
			prepares++
			if id != task.ID || owner != task.LeaseOwner || version != task.LeaseVersion || !at.Equal(now) {
				t.Fatalf("unexpected settlement fence: id=%d owner=%q version=%d at=%v", id, owner, version, at)
			}
			settling := task
			settling.Status = domain.HeroSMSTaskSettling
			settling.MessageCount = 1
			settling.StopRequested = true
			settling.RefundStatus = domain.HeroSMSRefundUnavailable
			return settling, nil
		},
		finishFn: func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, refund domain.HeroSMSRefundStatus, _ string) (domain.HeroSMSNumberTask, error) {
			localFinishes++
			if status != domain.HeroSMSTaskSettled || refund != domain.HeroSMSRefundSettled {
				t.Fatalf("stop settlement = %s/%s", status, refund)
			}
			return domain.HeroSMSNumberTask{Status: status, RefundStatus: refund}, nil
		},
	}
	requestAnother, providerFinishes := 0, 0
	client := &clientStub{
		messagesFn: func(context.Context, string, bool) ([]herosms.Message, error) {
			close(pollStarted)
			<-releasePoll
			return []herosms.Message{{ID: "poll-stop", Code: "123456", Text: "verification 123456"}}, nil
		},
		requestAnotherFn: func(context.Context, string) error {
			requestAnother++
			return nil
		},
		finishFn: func(context.Context, string, bool) error {
			providerFinishes++
			return nil
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		testManager(store, client, now).processTask(context.Background(), task)
	}()
	select {
	case <-pollStarted:
	case <-time.After(time.Second):
		t.Fatal("provider poll did not reach the barrier")
	}
	close(stopRequested)
	close(releasePoll)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll worker did not finish after stop")
	}
	if appends != 1 || prepares != 1 || providerFinishes != 1 || localFinishes != 1 {
		t.Fatalf("appends=%d prepares=%d provider finishes=%d local finishes=%d, want 1 each",
			appends, prepares, providerFinishes, localFinishes)
	}
	if requestAnother != 0 {
		t.Fatalf("RequestAnother calls after stop = %d, want 0", requestAnother)
	}
}

func TestStopChoosesRefundSettlementOrExpiry(t *testing.T) {
	now := time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		prepare    domain.HeroSMSRefundStatus
		expired    bool
		wantCancel int
		wantFinish int
		wantStatus domain.HeroSMSNumberTaskStatus
		wantRefund domain.HeroSMSRefundStatus
	}{
		{name: "refund", prepare: domain.HeroSMSRefundRequested, wantCancel: 1, wantStatus: domain.HeroSMSTaskRefunded, wantRefund: domain.HeroSMSRefunded},
		{name: "message makes refund unavailable", prepare: domain.HeroSMSRefundUnavailable, wantFinish: 1, wantStatus: domain.HeroSMSTaskSettled, wantRefund: domain.HeroSMSRefundSettled},
		{name: "expired", prepare: domain.HeroSMSRefundUnavailable, expired: true, wantFinish: 1, wantStatus: domain.HeroSMSTaskExpired, wantRefund: domain.HeroSMSRefundUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := activeTask(now)
			task.StopRequested = true
			if test.expired {
				expiry := now
				task.ExpiresAt = &expiry
			}
			store := &storeStub{
				prepareSettlementFn: func(_ context.Context, _ int64, _ string, _ int64, at time.Time) (domain.HeroSMSNumberTask, error) {
					if !at.Equal(now) {
						t.Fatalf("prepare time = %v", at)
					}
					task.Status = domain.HeroSMSTaskSettling
					task.RefundStatus = test.prepare
					return task, nil
				},
				finishFn: func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, refund domain.HeroSMSRefundStatus, _ string) (domain.HeroSMSNumberTask, error) {
					if status != test.wantStatus || refund != test.wantRefund {
						t.Fatalf("finish = %s/%s, want %s/%s", status, refund, test.wantStatus, test.wantRefund)
					}
					return domain.HeroSMSNumberTask{Status: status, RefundStatus: refund}, nil
				},
			}
			cancelCalls, finishCalls := 0, 0
			client := &clientStub{
				cancelFn: func(context.Context, string, bool) error { cancelCalls++; return nil },
				finishFn: func(context.Context, string, bool) error { finishCalls++; return nil },
			}
			testManager(store, client, now).processTask(context.Background(), task)
			if cancelCalls != test.wantCancel || finishCalls != test.wantFinish {
				t.Fatalf("cancel=%d finish=%d, want %d/%d", cancelCalls, finishCalls, test.wantCancel, test.wantFinish)
			}
		})
	}
}

func TestRefundFinishConflictFromConcurrentMessageConvergesToSettled(t *testing.T) {
	now := time.Date(2026, 8, 28, 13, 30, 0, 0, time.UTC)
	task := activeTask(now)
	task.Status = domain.HeroSMSTaskSettling
	task.RefundStatus = domain.HeroSMSRefundRequested
	finishes := 0
	store := &storeStub{finishFn: func(
		_ context.Context,
		_ int64,
		_ string,
		_ int64,
		status domain.HeroSMSNumberTaskStatus,
		refund domain.HeroSMSRefundStatus,
		lastError string,
	) (domain.HeroSMSNumberTask, error) {
		finishes++
		switch finishes {
		case 1:
			if status != domain.HeroSMSTaskRefunded || refund != domain.HeroSMSRefunded {
				t.Fatalf("first finish = %s/%s, want refunded/refunded", status, refund)
			}
			return domain.HeroSMSNumberTask{}, storage.ErrConflict
		case 2:
			if status != domain.HeroSMSTaskSettled || refund != domain.HeroSMSRefundSettled ||
				lastError != "验证码在退款结算期间到达，已按不可退款结算" {
				t.Fatalf("conflict fallback = %s/%s/%q", status, refund, lastError)
			}
			return domain.HeroSMSNumberTask{Status: status, RefundStatus: refund}, nil
		default:
			t.Fatalf("unexpected finish call %d", finishes)
			return domain.HeroSMSNumberTask{}, nil
		}
	}}
	testManager(store, &clientStub{}, now).finish(
		context.Background(), task,
		domain.HeroSMSTaskRefunded, domain.HeroSMSRefunded, "",
	)
	if finishes != 2 {
		t.Fatalf("finish calls = %d, want 2", finishes)
	}
}

func TestExpiredSettlementFailureKeepsDurableCancelIntent(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC)
	task := activeTask(now)
	task.Status = domain.HeroSMSTaskSettling
	task.RefundStatus = domain.HeroSMSRefundRequested
	task.ExpiresAt = timePointer(now)
	deadline := now.Add(-time.Minute)
	task.RefundableUntil = &deadline
	store := &storeStub{scheduleFn: func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
		if status != domain.HeroSMSTaskSettling || !next.After(now) || lastError == "" {
			t.Fatalf("settlement retry lost intent: %s %v %q", status, next, lastError)
		}
		return task, nil
	}}
	cancelCalls := 0
	client := &clientStub{cancelFn: func(context.Context, string, bool) error {
		cancelCalls++
		return errors.New("temporary network error")
	}}
	testManager(store, client, now).processTask(context.Background(), task)
	if cancelCalls != 1 {
		t.Fatalf("cancel calls = %d", cancelCalls)
	}
}

func TestExpiredSettlementFinishesLocallyWhenProviderCleanupFails(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 15, 0, 0, time.UTC)
	task := activeTask(now)
	task.Status = domain.HeroSMSTaskSettling
	task.RefundStatus = domain.HeroSMSRefundUnavailable
	task.ExpiresAt = timePointer(now)
	finishCalls := 0
	store := &storeStub{finishFn: func(
		_ context.Context, _ int64, _ string, _ int64,
		status domain.HeroSMSNumberTaskStatus, refund domain.HeroSMSRefundStatus, lastError string,
	) (domain.HeroSMSNumberTask, error) {
		if status != domain.HeroSMSTaskExpired || refund != domain.HeroSMSRefundUnavailable {
			t.Fatalf("terminal outcome = %s/%s, want expired/unavailable", status, refund)
		}
		if !strings.Contains(lastError, "供应商到期清理失败") ||
			!strings.Contains(lastError, "temporary provider outage") {
			t.Fatalf("terminal cleanup error = %q", lastError)
		}
		return domain.HeroSMSNumberTask{Status: status, RefundStatus: refund}, nil
	}}
	client := &clientStub{finishFn: func(_ context.Context, id string, rent bool) error {
		finishCalls++
		if id != task.ProviderActivationID || rent {
			t.Fatalf("unexpected expiry cleanup: id=%q rent=%v", id, rent)
		}
		return errors.New("temporary provider outage")
	}}
	testManager(store, client, now).processTask(context.Background(), task)
	if finishCalls != 1 {
		t.Fatalf("provider Finish calls = %d, want 1", finishCalls)
	}
}

func TestUnexpiredManualFinishFailureKeepsDurableSettlement(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 20, 0, 0, time.UTC)
	task := activeTask(now)
	task.Status = domain.HeroSMSTaskSettling
	task.StopRequested = true
	task.RefundStatus = domain.HeroSMSRefundUnavailable
	schedules := 0
	store := &storeStub{scheduleFn: func(
		_ context.Context, _ int64, _ string, _ int64,
		status domain.HeroSMSNumberTaskStatus, next time.Time, lastError string,
	) (domain.HeroSMSNumberTask, error) {
		schedules++
		if status != domain.HeroSMSTaskSettling || !next.Equal(now.Add(2*time.Second)) ||
			!strings.Contains(lastError, "temporary provider outage") {
			t.Fatalf("settlement retry = %s %v %q", status, next, lastError)
		}
		return task, nil
	}}
	finishCalls := 0
	client := &clientStub{finishFn: func(_ context.Context, id string, rent bool) error {
		finishCalls++
		if id != task.ProviderActivationID || rent {
			t.Fatalf("unexpected manual cleanup: id=%q rent=%v", id, rent)
		}
		return errors.New("temporary provider outage")
	}}
	testManager(store, client, now).processTask(context.Background(), task)
	if finishCalls != 1 || schedules != 1 {
		t.Fatalf("provider finishes=%d schedules=%d, want 1 each", finishCalls, schedules)
	}
}

func TestPurchaseUnknownManualStopNeverRepurchases(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 30, 0, 0, time.UTC)
	tests := []struct {
		name       string
		providerID string
		wantStatus domain.HeroSMSNumberTaskStatus
		wantFinish int
	}{
		{name: "unknown provider identity stops locally", wantStatus: domain.HeroSMSTaskStopped},
		{name: "known provider identity is settled", providerID: "provider-unknown", wantStatus: domain.HeroSMSTaskSettled, wantFinish: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := leasedWaitingTask(now, nil)
			task.Status = domain.HeroSMSTaskPurchaseUnknown
			task.StopRequested = true
			task.PurchaseToken = "uncertain-purchase"
			task.ProviderActivationID = test.providerID
			store := &storeStub{}
			if test.providerID != "" {
				store.prepareSettlementFn = func(_ context.Context, _ int64, _ string, _ int64, _ time.Time) (domain.HeroSMSNumberTask, error) {
					task.Status = domain.HeroSMSTaskSettling
					task.RefundStatus = domain.HeroSMSRefundUnavailable
					return task, nil
				}
			}
			store.finishFn = func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, _ domain.HeroSMSRefundStatus, _ string) (domain.HeroSMSNumberTask, error) {
				if status != test.wantStatus {
					t.Fatalf("terminal status = %s, want %s", status, test.wantStatus)
				}
				return domain.HeroSMSNumberTask{Status: status}, nil
			}
			finishCalls, purchaseCalls := 0, 0
			client := &clientStub{
				purchaseFn: func(context.Context, herosms.PurchaseRequest) (herosms.Purchase, error) {
					purchaseCalls++
					return herosms.Purchase{}, nil
				},
				finishFn: func(context.Context, string, bool) error {
					finishCalls++
					return nil
				},
			}
			testManager(store, client, now).processTask(context.Background(), task)
			if purchaseCalls != 0 || finishCalls != test.wantFinish {
				t.Fatalf("purchase=%d finish=%d, want 0/%d", purchaseCalls, finishCalls, test.wantFinish)
			}
		})
	}
}

func TestCancelMissingActivationDoesNotClaimRefund(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 45, 0, 0, time.UTC)
	task := activeTask(now)
	task.Status = domain.HeroSMSTaskSettling
	task.RefundStatus = domain.HeroSMSRefundRequested
	store := &storeStub{finishFn: func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, refund domain.HeroSMSRefundStatus, _ string) (domain.HeroSMSNumberTask, error) {
		if status != domain.HeroSMSTaskSettled || refund != domain.HeroSMSRefundSettled {
			t.Fatalf("missing activation reported as %s/%s", status, refund)
		}
		return domain.HeroSMSNumberTask{Status: status}, nil
	}}
	client := &clientStub{cancelFn: func(context.Context, string, bool) error {
		return &smsbower.APIError{Provider: "HeroSMS", Action: "setStatus", Code: "NO_ACTIVATION"}
	}}
	testManager(store, client, now).processTask(context.Background(), task)
}

func TestBADStatusFinalizationConvergesOnFirstAndRecoveryAttempts(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 47, 0, 0, time.UTC)
	tests := []struct {
		name          string
		recovery      bool
		refundIntent  bool
		wantLastError bool
	}{
		{name: "first finish"},
		{name: "recovered finish", recovery: true},
		{name: "first cancel", refundIntent: true, wantLastError: true},
		{name: "recovered cancel", recovery: true, refundIntent: true, wantLastError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := activeTask(now)
			task.StopRequested = true
			settlementRefund := domain.HeroSMSRefundUnavailable
			if test.refundIntent {
				settlementRefund = domain.HeroSMSRefundRequested
			}
			prepareCalls := 0
			store := &storeStub{}
			if test.recovery {
				task.Status = domain.HeroSMSTaskSettling
				task.RefundStatus = settlementRefund
			} else {
				store.prepareSettlementFn = func(_ context.Context, _ int64, _ string, _ int64, _ time.Time) (domain.HeroSMSNumberTask, error) {
					prepareCalls++
					settling := task
					settling.Status = domain.HeroSMSTaskSettling
					settling.RefundStatus = settlementRefund
					return settling, nil
				}
			}
			terminalCalls := 0
			store.finishFn = func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, refund domain.HeroSMSRefundStatus, lastError string) (domain.HeroSMSNumberTask, error) {
				terminalCalls++
				if status != domain.HeroSMSTaskSettled || refund != domain.HeroSMSRefundSettled {
					t.Fatalf("BAD_STATUS outcome = %s/%s, want settled/settled", status, refund)
				}
				hasUnconfirmedRefund := strings.Contains(lastError, "未确认退款")
				if hasUnconfirmedRefund != test.wantLastError {
					t.Fatalf("BAD_STATUS last error = %q, want unconfirmed=%v", lastError, test.wantLastError)
				}
				return domain.HeroSMSNumberTask{Status: status, RefundStatus: refund}, nil
			}
			finishCalls, cancelCalls := 0, 0
			badStatus := func() error {
				return &smsbower.APIError{Provider: "HeroSMS", Action: "setStatus", Code: "BAD_STATUS"}
			}
			client := &clientStub{
				finishFn: func(context.Context, string, bool) error {
					finishCalls++
					return badStatus()
				},
				cancelFn: func(context.Context, string, bool) error {
					cancelCalls++
					return badStatus()
				},
			}
			testManager(store, client, now).processTask(context.Background(), task)
			wantPrepare := 1
			if test.recovery {
				wantPrepare = 0
			}
			wantFinish, wantCancel := 1, 0
			if test.refundIntent {
				wantFinish, wantCancel = 0, 1
			}
			if prepareCalls != wantPrepare || finishCalls != wantFinish || cancelCalls != wantCancel || terminalCalls != 1 {
				t.Fatalf("prepare=%d finish=%d cancel=%d terminal=%d, want %d/%d/%d/1",
					prepareCalls, finishCalls, cancelCalls, terminalCalls, wantPrepare, wantFinish, wantCancel)
			}
		})
	}
}

func TestContinuationCrashProtocolFreshAndRecovery(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 50, 0, 0, time.UTC)
	badStatus := &smsbower.APIError{Provider: "HeroSMS", Action: "setStatus", Code: "BAD_STATUS"}
	businessRejection := &smsbower.APIError{Provider: "HeroSMS", Action: "setStatus", Code: "NO_ACTIVATION"}
	tests := []struct {
		name         string
		recovering   bool
		requestErr   error
		messageCount int
		continuation int
		pending      int
		beginMessage int
		beginTarget  int
		wantComplete int
		wantAbort    int
		wantSchedule int
		wantTarget   int
		wantSequence []string
	}{
		{
			name: "fresh success", messageCount: 2, continuation: 1, beginMessage: 2, beginTarget: 2,
			wantComplete: 1, wantTarget: 2, wantSequence: []string{"begin", "request", "complete"},
		},
		{
			name:         "fresh begin uses authoritative concurrent message count",
			messageCount: 1, beginMessage: 2, beginTarget: 2,
			wantComplete: 1, wantTarget: 2, wantSequence: []string{"begin", "request", "complete"},
		},
		{
			name: "fresh BAD_STATUS aborts without acknowledgement", requestErr: badStatus,
			messageCount: 1, beginMessage: 1, beginTarget: 1,
			wantAbort: 1, wantTarget: 1, wantSequence: []string{"begin", "request", "abort"},
		},
		{
			name: "fresh conclusive rejection aborts", requestErr: businessRejection,
			messageCount: 1, beginMessage: 1, beginTarget: 1,
			wantAbort: 1, wantTarget: 1, wantSequence: []string{"begin", "request", "abort"},
		},
		{
			name: "fresh ambiguous response preserves pending", requestErr: errors.New("transport response lost"),
			messageCount: 1, beginMessage: 1, beginTarget: 1,
			wantSchedule: 1, wantTarget: 1, wantSequence: []string{"begin", "request", "schedule"},
		},
		{
			name: "recovery success completes frozen target and leaves newer delta", recovering: true,
			messageCount: 2, pending: 1, wantComplete: 1, wantTarget: 1,
			wantSequence: []string{"request", "complete"},
		},
		{
			name: "recovery BAD_STATUS acknowledges prior intent", recovering: true, requestErr: badStatus,
			messageCount: 1, pending: 1, wantComplete: 1, wantTarget: 1,
			wantSequence: []string{"request", "complete"},
		},
		{
			name: "recovery conclusive error still preserves pending", recovering: true, requestErr: businessRejection,
			messageCount: 1, pending: 1, wantSchedule: 1, wantTarget: 1,
			wantSequence: []string{"request", "schedule"},
		},
		{
			name: "recovery ambiguous error preserves pending", recovering: true, requestErr: context.DeadlineExceeded,
			messageCount: 1, pending: 1, wantSchedule: 1, wantTarget: 1,
			wantSequence: []string{"request", "schedule"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := activeTask(now)
			task.MessageCount = test.messageCount
			task.ContinuationCount = test.continuation
			task.ContinuationPendingCount = test.pending
			sequence := make([]string, 0, 3)
			begins, completes, aborts, schedules := 0, 0, 0, 0
			store := &storeStub{}
			if !test.recovering {
				store.beginContinuationFn = func(_ context.Context, id int64, owner string, version int64, at time.Time) (domain.HeroSMSNumberTask, error) {
					sequence = append(sequence, "begin")
					begins++
					if id != task.ID || owner != task.LeaseOwner || version != task.LeaseVersion || !at.Equal(now) {
						t.Fatalf("unexpected begin fence: id=%d owner=%q version=%d at=%v", id, owner, version, at)
					}
					begun := task
					begun.MessageCount = test.beginMessage
					begun.ContinuationPendingCount = test.beginTarget
					return begun, nil
				}
			}
			if test.wantComplete > 0 {
				store.completeContinuationFn = func(_ context.Context, _ int64, _ string, _ int64, target int, next time.Time) (domain.HeroSMSNumberTask, error) {
					sequence = append(sequence, "complete")
					completes++
					if target != test.wantTarget || !next.Equal(now.Add(45*time.Second)) {
						t.Fatalf("complete target/next = %d/%v, want %d/%v", target, next, test.wantTarget, now.Add(45*time.Second))
					}
					updated := task
					updated.ContinuationCount = target
					updated.ContinuationPendingCount = 0
					return updated, nil
				}
			}
			if test.wantAbort > 0 {
				store.abortContinuationFn = func(_ context.Context, _ int64, _ string, _ int64, target int, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
					sequence = append(sequence, "abort")
					aborts++
					if target != test.wantTarget || !next.Equal(now.Add(2*time.Second)) || lastError == "" {
						t.Fatalf("abort target/next/error = %d/%v/%q", target, next, lastError)
					}
					return task, nil
				}
			}
			if test.wantSchedule > 0 {
				store.scheduleFn = func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
					sequence = append(sequence, "schedule")
					schedules++
					if status != domain.HeroSMSTaskActive || !next.Equal(now.Add(2*time.Second)) || lastError == "" {
						t.Fatalf("pending retry = %s/%v/%q", status, next, lastError)
					}
					return task, nil
				}
			}
			requests := 0
			client := &clientStub{requestAnotherFn: func(_ context.Context, id string) error {
				sequence = append(sequence, "request")
				requests++
				if id != task.ProviderActivationID {
					t.Fatalf("request activation = %q", id)
				}
				return test.requestErr
			}}
			testManager(store, client, now).processTask(context.Background(), task)
			wantBegins := 1
			if test.recovering {
				wantBegins = 0
			}
			if begins != wantBegins || requests != 1 || completes != test.wantComplete ||
				aborts != test.wantAbort || schedules != test.wantSchedule {
				t.Fatalf("begin/request/complete/abort/schedule = %d/%d/%d/%d/%d",
					begins, requests, completes, aborts, schedules)
			}
			if strings.Join(sequence, ",") != strings.Join(test.wantSequence, ",") {
				t.Fatalf("operation order = %v, want %v", sequence, test.wantSequence)
			}
		})
	}
}

func TestClassifyContinuationRequest(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want continuationRequestOutcome
	}{
		{name: "success", want: continuationRequestApplied},
		{name: "BAD_STATUS", err: &smsbower.APIError{Code: "BAD_STATUS"}, want: continuationRequestBadStatus},
		{name: "business API error", err: &smsbower.APIError{Code: "NO_ACTIVATION"}, want: continuationRequestRejected},
		{name: "HTTP 409", err: &smsbower.APIError{Code: "HTTP_409"}, want: continuationRequestRejected},
		{name: "HTTP 500", err: &smsbower.APIError{Code: "HTTP_500"}, want: continuationRequestAmbiguous},
		{name: "empty response", err: &smsbower.APIError{Code: "EMPTY_RESPONSE"}, want: continuationRequestAmbiguous},
		{name: "transport", err: errors.New("connection reset"), want: continuationRequestAmbiguous},
		{name: "context cancellation", err: context.Canceled, want: continuationRequestAmbiguous},
		{name: "parse failure", err: errors.New("decode setStatus: invalid character"), want: continuationRequestAmbiguous},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyContinuationRequest(test.err); got != test.want {
				t.Fatalf("classification = %d, want %d", got, test.want)
			}
		})
	}
}

func TestUnsupportedContinuationKeepsPollingWithoutStatusThree(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 52, 0, 0, time.UTC)
	task := activeTask(now)
	task.SupportsContinuation = false
	task.MessageCount = 1
	polls, schedules, requests := 0, 0, 0
	store := &storeStub{scheduleFn: func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
		schedules++
		if status != domain.HeroSMSTaskActive || !next.Equal(now.Add(45*time.Second)) || lastError != "" {
			t.Fatalf("unsupported continuation schedule = %s/%v/%q", status, next, lastError)
		}
		return task, nil
	}}
	client := &clientStub{
		messagesFn: func(context.Context, string, bool) ([]herosms.Message, error) {
			polls++
			return nil, nil
		},
		requestAnotherFn: func(context.Context, string) error {
			requests++
			return nil
		},
	}
	testManager(store, client, now).processTask(context.Background(), task)
	if polls != 1 || schedules != 1 || requests != 0 {
		t.Fatalf("polls/schedules/status3 = %d/%d/%d, want 1/1/0", polls, schedules, requests)
	}
}

func TestContinuationBeginStopCASReleasesLeaseWithoutProviderCall(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 53, 0, 0, time.UTC)
	task := activeTask(now)
	task.MessageCount = 1
	schedules, requests := 0, 0
	store := &storeStub{
		beginContinuationFn: func(context.Context, int64, string, int64, time.Time) (domain.HeroSMSNumberTask, error) {
			return domain.HeroSMSNumberTask{}, storage.ErrConflict
		},
		scheduleFn: func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
			schedules++
			if status != domain.HeroSMSTaskActive || !next.Equal(now) || lastError != "" {
				t.Fatalf("stop-CAS lease release = %s/%v/%q", status, next, lastError)
			}
			return task, nil
		},
	}
	client := &clientStub{requestAnotherFn: func(context.Context, string) error {
		requests++
		return nil
	}}
	testManager(store, client, now).processTask(context.Background(), task)
	if schedules != 1 || requests != 0 {
		t.Fatalf("lease releases/status3 = %d/%d, want 1/0", schedules, requests)
	}
}

func TestContinuationStopAfterBeginAllowsCurrentPersistedIntent(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 54, 0, 0, time.UTC)
	task := activeTask(now)
	task.MessageCount = 1
	beginPersisted := make(chan struct{})
	stopCommitted := make(chan struct{})
	store := &storeStub{
		beginContinuationFn: func(context.Context, int64, string, int64, time.Time) (domain.HeroSMSNumberTask, error) {
			close(beginPersisted)
			<-stopCommitted
			begun := task
			begun.ContinuationPendingCount = 1
			return begun, nil
		},
		completeContinuationFn: func(_ context.Context, _ int64, _ string, _ int64, target int, _ time.Time) (domain.HeroSMSNumberTask, error) {
			if target != 1 {
				t.Fatalf("completion target = %d", target)
			}
			return task, nil
		},
	}
	requests := 0
	client := &clientStub{requestAnotherFn: func(context.Context, string) error {
		select {
		case <-stopCommitted:
		default:
			t.Fatal("status=3 ran before the concurrent stop barrier")
		}
		requests++
		return nil
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		testManager(store, client, now).processTask(context.Background(), task)
	}()
	select {
	case <-beginPersisted:
	case <-time.After(time.Second):
		t.Fatal("continuation Begin did not reach barrier")
	}
	close(stopCommitted)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("continuation did not finish after stop barrier")
	}
	if requests != 1 {
		t.Fatalf("status=3 calls = %d, want the one persisted intent", requests)
	}
}

func TestContinuationExpiryAfterBeginAbortsBeforeProviderCall(t *testing.T) {
	start := time.Date(2026, 8, 28, 14, 55, 0, 0, time.UTC)
	expiresAt := start.Add(time.Second)
	current := start
	task := activeTask(start)
	task.MessageCount = 1
	task.ExpiresAt = &expiresAt
	aborts, requests := 0, 0
	store := &storeStub{
		beginContinuationFn: func(_ context.Context, _ int64, _ string, _ int64, at time.Time) (domain.HeroSMSNumberTask, error) {
			if !at.Equal(start) {
				t.Fatalf("begin time = %v", at)
			}
			current = expiresAt
			begun := task
			begun.ContinuationPendingCount = 1
			return begun, nil
		},
		abortContinuationFn: func(_ context.Context, _ int64, _ string, _ int64, target int, next time.Time, lastError string) (domain.HeroSMSNumberTask, error) {
			aborts++
			if target != 1 || !next.Equal(expiresAt) || !strings.Contains(lastError, "已到期") {
				t.Fatalf("expiry abort = target %d next %v error %q", target, next, lastError)
			}
			return task, nil
		},
	}
	client := &clientStub{requestAnotherFn: func(context.Context, string) error {
		requests++
		return nil
	}}
	manager := New(store, client, Config{Now: func() time.Time { return current }},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.processTask(context.Background(), task)
	if aborts != 1 || requests != 0 {
		t.Fatalf("expiry aborts/status3 = %d/%d, want 1/0", aborts, requests)
	}
}

func TestContinuationCompletionPersistsAfterWorkerCancellation(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 56, 0, 0, time.UTC)
	task := activeTask(now)
	task.MessageCount = 1
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	store := &storeStub{
		beginContinuationFn: func(context.Context, int64, string, int64, time.Time) (domain.HeroSMSNumberTask, error) {
			begun := task
			begun.ContinuationPendingCount = 1
			return begun, nil
		},
		completeContinuationFn: func(ctx context.Context, _ int64, _ string, _ int64, target int, _ time.Time) (domain.HeroSMSNumberTask, error) {
			if ctx.Err() != nil {
				t.Fatalf("completion inherited cancelled worker context: %v", ctx.Err())
			}
			if target != 1 {
				t.Fatalf("completion target = %d", target)
			}
			return task, nil
		},
	}
	client := &clientStub{requestAnotherFn: func(context.Context, string) error {
		cancelWorker()
		return nil
	}}
	testManager(store, client, now).processTask(workerCtx, task)
}

func TestProviderResponseIsPersistedAfterWorkerCancellation(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 55, 0, 0, time.UTC)
	task := leasedWaitingTask(now, nil)
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	store := &storeStub{}
	store.beginFn = func(_ context.Context, _ int64, _ string, _ int64, token string) (domain.HeroSMSNumberTask, error) {
		task.Status = domain.HeroSMSTaskPurchasing
		task.PurchaseToken = token
		return task, nil
	}
	persisted := false
	store.commitFn = func(ctx context.Context, _ int64, _ string, _ int64, _ storage.CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error) {
		if ctx.Err() != nil {
			t.Fatalf("commit inherited cancelled worker context: %v", ctx.Err())
		}
		persisted = true
		return domain.HeroSMSNumberTask{Status: domain.HeroSMSTaskActive}, nil
	}
	client := &clientStub{purchaseFn: func(context.Context, herosms.PurchaseRequest) (herosms.Purchase, error) {
		cancelWorker()
		return herosms.Purchase{
			ActivationID: "provider-cancel-race", PhoneNumber: "+62000",
			ActivatedAt: now, ExpiresAt: now.Add(time.Hour), Raw: json.RawMessage(`{"ok":true}`),
		}, nil
	}}
	testManager(store, client, now).processTask(workerCtx, task)
	if !persisted {
		t.Fatal("provider allocation was not persisted after worker cancellation")
	}
}

func TestConfirmedPurchaseRecoversAfterLeaseFenceConflict(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 58, 0, 0, time.UTC)
	task := leasedWaitingTask(now, nil)
	var token string
	store := &storeStub{}
	store.beginFn = func(_ context.Context, _ int64, _ string, _ int64, value string) (domain.HeroSMSNumberTask, error) {
		token = value
		task.Status = domain.HeroSMSTaskPurchasing
		task.PurchaseToken = value
		return task, nil
	}
	store.commitFn = func(context.Context, int64, string, int64, storage.CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error) {
		return domain.HeroSMSNumberTask{}, storage.ErrConflict
	}
	recovered := false
	store.recoverPurchaseFn = func(_ context.Context, id int64, value string, params storage.CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error) {
		recovered = true
		if id != task.ID || value != token || params.PurchaseToken != token ||
			params.ProviderActivationID != "provider-after-lease" || params.PhoneNumber != "+62111" {
			t.Fatalf("invalid confirmed-purchase recovery: id=%d token=%q params=%+v", id, value, params)
		}
		return domain.HeroSMSNumberTask{
			ID: task.ID, Status: domain.HeroSMSTaskActive,
			ProviderActivationID: params.ProviderActivationID, PhoneNumber: params.PhoneNumber,
		}, nil
	}
	client := &clientStub{purchaseFn: func(context.Context, herosms.PurchaseRequest) (herosms.Purchase, error) {
		return herosms.Purchase{
			ActivationID: "provider-after-lease", PhoneNumber: "+62111",
			ActivatedAt: now, ExpiresAt: now.Add(time.Hour), Raw: json.RawMessage(`{"ok":true}`),
		}, nil
	}}
	testManager(store, client, now).processTask(context.Background(), task)
	if !recovered {
		t.Fatal("confirmed allocation was lost after owned commit conflict")
	}
}

func TestRefundDeadlineIsExclusive(t *testing.T) {
	now := time.Date(2026, 8, 28, 15, 0, 0, 0, time.UTC)
	manager := testManager(&storeStub{}, &clientStub{}, now)
	task := activeTask(now)
	task.RefundStatus = domain.HeroSMSRefundRefundable
	task.RefundableUntil = timePointer(now)
	if got := manager.visibleTask(task, now).RefundStatus; got != domain.HeroSMSRefundUnavailable {
		t.Fatalf("refund status at deadline = %q", got)
	}
	task.RefundableUntil = timePointer(now.Add(time.Nanosecond))
	if got := manager.visibleTask(task, now).RefundStatus; got != domain.HeroSMSRefundRefundable {
		t.Fatalf("refund status before deadline = %q", got)
	}
}

func TestExpiryBoundaryFinishesOnlyAtDeadline(t *testing.T) {
	now := time.Date(2026, 8, 28, 15, 30, 0, 0, time.UTC)
	tests := []struct {
		name         string
		expiresAt    time.Time
		wantSchedule int
		wantFinish   int
	}{
		{name: "one nanosecond before expiry", expiresAt: now.Add(time.Nanosecond), wantSchedule: 1},
		{name: "at expiry", expiresAt: now, wantFinish: 1},
		{name: "after expiry", expiresAt: now.Add(-time.Nanosecond), wantFinish: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := activeTask(now)
			task.ExpiresAt = timePointer(test.expiresAt)
			task.RefundableUntil = timePointer(test.expiresAt)
			schedules, finishes := 0, 0
			store := &storeStub{}
			if test.wantSchedule > 0 {
				store.scheduleFn = func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, _ time.Time, _ string) (domain.HeroSMSNumberTask, error) {
					schedules++
					if status != domain.HeroSMSTaskActive {
						t.Fatalf("scheduled status = %s", status)
					}
					return task, nil
				}
			} else {
				store.prepareSettlementFn = func(_ context.Context, _ int64, _ string, _ int64, _ time.Time) (domain.HeroSMSNumberTask, error) {
					task.Status = domain.HeroSMSTaskSettling
					task.RefundStatus = domain.HeroSMSRefundUnavailable
					return task, nil
				}
				store.finishFn = func(_ context.Context, _ int64, _ string, _ int64, status domain.HeroSMSNumberTaskStatus, _ domain.HeroSMSRefundStatus, _ string) (domain.HeroSMSNumberTask, error) {
					if status != domain.HeroSMSTaskExpired {
						t.Fatalf("terminal status = %s", status)
					}
					return domain.HeroSMSNumberTask{Status: status}, nil
				}
			}
			client := &clientStub{
				messagesFn: func(context.Context, string, bool) ([]herosms.Message, error) {
					return []herosms.Message{}, nil
				},
				finishFn: func(context.Context, string, bool) error {
					finishes++
					return nil
				},
			}
			testManager(store, client, now).processTask(context.Background(), task)
			if schedules != test.wantSchedule || finishes != test.wantFinish {
				t.Fatalf("schedules=%d finishes=%d, want %d/%d", schedules, finishes, test.wantSchedule, test.wantFinish)
			}
		})
	}
}

func activeTask(now time.Time) domain.HeroSMSNumberTask {
	expires := now.Add(time.Hour)
	refundable := now.Add(10 * time.Minute)
	leaseUntil := now.Add(time.Minute)
	return domain.HeroSMSNumberTask{
		ID: 9, Status: domain.HeroSMSTaskActive, ProductKind: domain.HeroSMSProductActivation,
		ServiceCode: "wa", CountryCode: "6", VerificationType: string(herosms.VerificationSMS),
		ProviderActivationID: "provider-9", SupportsContinuation: true,
		RefundStatus: domain.HeroSMSRefundRefundable, RefundableUntil: &refundable,
		ExpiresAt: &expires, LeaseOwner: "worker:9:1", LeaseVersion: 1, LeaseUntil: &leaseUntil,
	}
}

func intPointer(value int) *int { return &value }

func timePointer(value time.Time) *time.Time { return &value }

func durationValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
