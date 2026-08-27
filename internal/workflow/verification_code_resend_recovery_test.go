package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

func TestLogin2FATimeoutRequestsAnotherSMSAndResendsGoPayOTP(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	oldSentAt := time.Now().UTC().Add(-verificationCodeWait - time.Second)
	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone:            "81234567890",
		CountryCode:      "+62",
		Device:           gopay.GenerateDeviceIdentity("81234567890"),
		VerificationID:   "login-verification-2",
		OTPToken:         "login-2fa-otp-token-1",
		AccountID:        "account-1",
		TwoFAToken:       "two-fa-token",
		LoginStage:       gopay.LoginStageAwaiting2FAOTP,
		LoginCodeSentAt:  oldSentAt,
		LoginCodeResends: 0,
	}, domain.ActivationStatusAwaitingLoginCode)

	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollLoginCode timeout: %v", err)
	}
	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageReady2FA || state.LoginCodeResends != 1 {
		t.Fatalf("login state after timeout = stage %q count %d", state.LoginStage, state.LoginCodeResends)
	}
	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollLoginCode provider resend: %v", err)
	}
	if state = store.session(t, box); state.LoginStage != gopay.LoginStageCycleReady2FA {
		t.Fatalf("login stage after provider resend = %q, want %q", state.LoginStage, gopay.LoginStageCycleReady2FA)
	}

	resendStartedAt := time.Now().UTC()
	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollLoginCode resend: %v", err)
	}
	resendFinishedAt := time.Now().UTC()

	state = store.session(t, box)
	if state.LoginStage != gopay.LoginStageAwaiting2FAOTP || state.LoginCodeResends != 1 {
		t.Fatalf("login resend state = stage %q count %d", state.LoginStage, state.LoginCodeResends)
	}
	if state.LoginCodeDispatchUncertain {
		t.Fatal("successful login OTP dispatch remained uncertain")
	}
	if state.OTPToken != "resent-otp-token" {
		t.Fatalf("login 2FA OTP token = %q, want resent token", state.OTPToken)
	}
	if state.LoginCodeSentAt.Before(resendStartedAt) || state.LoginCodeSentAt.After(resendFinishedAt) {
		t.Fatalf("login resend timestamp = %s, want within [%s, %s]", state.LoginCodeSentAt, resendStartedAt, resendFinishedAt)
	}
	store.mu.Lock()
	advanceCalls := store.advanceCalls
	cycle := store.activation.SMSCycle
	store.mu.Unlock()
	if advanceCalls != 1 || cycle != 1 || state.SMSCycle != 1 {
		t.Fatalf("provider SMS cycle = calls %d activation %d session %d", advanceCalls, cycle, state.SMSCycle)
	}
	wantEvents := []string{
		"provider:getStatus",
		"provider:getStatus",
		"provider:setStatus:3",
		"gopay:initiate:login_2fa",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
}

func TestPINSetupTimeoutRequestsAnotherSMSAndResendsGoPayOTP(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	oldSentAt := time.Now().UTC().Add(-verificationCodeWait - time.Second)
	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone:             "81234567890",
		CountryCode:       "+62",
		Device:            gopay.GenerateDeviceIdentity("81234567890"),
		AccessToken:       "access-token",
		PINVerificationID: "pin-verification-1",
		PINOTPToken:       "pin-otp-token-1",
		PINStage:          gopay.PINStageAwaiting,
		PINCodeSentAt:     oldSentAt,
		PINCodeResends:    0,
	}, domain.ActivationStatusAwaitingPINCode)

	if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollPINCode timeout: %v", err)
	}
	state := store.session(t, box)
	if state.PINStage != gopay.PINStageReadyCycle || state.PINCodeResends != 1 {
		t.Fatalf("PIN state after timeout = stage %q count %d", state.PINStage, state.PINCodeResends)
	}
	if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollPINCode provider resend: %v", err)
	}
	if state = store.session(t, box); state.PINStage != gopay.PINStageCycleReady {
		t.Fatalf("PIN stage after provider resend = %q, want %q", state.PINStage, gopay.PINStageCycleReady)
	}

	resendStartedAt := time.Now().UTC()
	if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollPINCode resend: %v", err)
	}
	resendFinishedAt := time.Now().UTC()

	state = store.session(t, box)
	if state.PINStage != gopay.PINStageAwaiting || state.PINCodeResends != 1 {
		t.Fatalf("PIN resend state = stage %q count %d", state.PINStage, state.PINCodeResends)
	}
	if state.PINCodeDispatchUncertain {
		t.Fatal("successful PIN OTP dispatch remained uncertain")
	}
	if state.PINOTPToken != "resent-otp-token" {
		t.Fatalf("PIN setup OTP token = %q, want resent token", state.PINOTPToken)
	}
	if state.PINCodeSentAt.Before(resendStartedAt) || state.PINCodeSentAt.After(resendFinishedAt) {
		t.Fatalf("PIN resend timestamp = %s, want within [%s, %s]", state.PINCodeSentAt, resendStartedAt, resendFinishedAt)
	}
	store.mu.Lock()
	advanceCalls := store.advanceCalls
	cycle := store.activation.SMSCycle
	store.mu.Unlock()
	if advanceCalls != 1 || cycle != 1 || state.SMSCycle != 1 {
		t.Fatalf("provider SMS cycle = calls %d activation %d session %d", advanceCalls, cycle, state.SMSCycle)
	}
	wantEvents := []string{
		"provider:getStatus",
		"provider:getStatus",
		"provider:setStatus:3",
		"gopay:pinAllowed",
		"gopay:pinMethods",
		"gopay:initiate:goto_pin_wa_sms",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
}

func TestReadyLogin2FAAndPINSetupConsumeLateCodeBeforeResend(t *testing.T) {
	t.Run("login 2FA", func(t *testing.T) {
		events := &verificationResendEventLog{}
		provider := newVerificationResendProvider(t, events, "STATUS_OK:9631")
		defer provider.Close()
		goPay := newVerificationResendGoPay(t, events)
		defer goPay.Close()

		manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
			Phone:            "81234567890",
			CountryCode:      "+62",
			Device:           gopay.GenerateDeviceIdentity("81234567890"),
			VerificationID:   "login-verification-2",
			OTPToken:         "login-2fa-otp-token-1",
			AccountID:        "account-1",
			TwoFAToken:       "two-fa-token",
			LoginStage:       gopay.LoginStageReady2FA,
			LoginCodeSentAt:  time.Now().UTC().Add(-verificationCodeWait - time.Second),
			LoginCodeResends: 2,
		}, domain.ActivationStatusAwaitingLoginCode)

		if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
			t.Fatal(err)
		}

		state := store.session(t, box)
		if state.LoginStage != gopay.LoginStageAuthenticated || state.AccessToken != "access-after-code" {
			t.Fatalf("login state = stage %q access %q", state.LoginStage, state.AccessToken)
		}
		if !state.LoginCodeSentAt.IsZero() || state.LoginCodeResends != 0 {
			t.Fatalf("login resend state after code = sent %s count %d", state.LoginCodeSentAt, state.LoginCodeResends)
		}
		store.mu.Lock()
		advanceCalls := store.advanceCalls
		store.mu.Unlock()
		if advanceCalls != 0 {
			t.Fatalf("setStatus=3 advance calls = %d, want 0", advanceCalls)
		}
		wantEvents := []string{
			"provider:getStatus",
			"gopay:verify:login_2fa:9631",
			"gopay:token:challenge",
		}
		if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
			t.Fatalf("events = %v, want %v", got, wantEvents)
		}
	})

	t.Run("PIN setup", func(t *testing.T) {
		events := &verificationResendEventLog{}
		provider := newVerificationResendProvider(t, events, "STATUS_OK:1742")
		defer provider.Close()
		goPay := newVerificationResendGoPay(t, events)
		defer goPay.Close()

		manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
			Phone:             "81234567890",
			CountryCode:       "+62",
			Device:            gopay.GenerateDeviceIdentity("81234567890"),
			AccessToken:       "access-token",
			PINVerificationID: "pin-verification-1",
			PINOTPToken:       "pin-otp-token-1",
			PINStage:          gopay.PINStageReadyCycle,
			PINCodeSentAt:     time.Now().UTC().Add(-verificationCodeWait - time.Second),
			PINCodeResends:    2,
		}, domain.ActivationStatusAwaitingPINCode)

		if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
			t.Fatal(err)
		}

		state := store.session(t, box)
		if state.PINStage != gopay.PINStageSetupVerified || state.PINVerificationToken != "verified-token" {
			t.Fatalf("PIN state = stage %q verification token %q", state.PINStage, state.PINVerificationToken)
		}
		if !state.PINCodeSentAt.IsZero() || state.PINCodeResends != 0 {
			t.Fatalf("PIN resend state after code = sent %s count %d", state.PINCodeSentAt, state.PINCodeResends)
		}
		store.mu.Lock()
		advanceCalls := store.advanceCalls
		store.mu.Unlock()
		if advanceCalls != 0 {
			t.Fatalf("setStatus=3 advance calls = %d, want 0", advanceCalls)
		}
		wantEvents := []string{
			"provider:getStatus",
			"gopay:verify:goto_pin_wa_sms:1742",
		}
		if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
			t.Fatalf("events = %v, want %v", got, wantEvents)
		}
	})
}

var errVerificationResendFinalSave = errors.New("injected verification resend final save failure")

type verificationResendFailingStore struct {
	*verificationResendFlowStore

	mu          sync.Mutex
	upsertCalls int
	failAt      int
}

func (s *verificationResendFailingStore) UpsertAccount(ctx context.Context, params storage.UpsertAccountParams) (domain.Account, error) {
	s.mu.Lock()
	s.upsertCalls++
	call := s.upsertCalls
	s.mu.Unlock()
	if call == s.failAt {
		return domain.Account{}, errVerificationResendFinalSave
	}
	return s.verificationResendFlowStore.UpsertAccount(ctx, params)
}

type verificationResendRecoveryCase struct {
	name          string
	state         gopay.Session
	poll          func(*Manager, context.Context, domain.Activation) error
	age           func(*gopay.Session)
	stage         func(gopay.Session) string
	count         func(gopay.Session) int
	sentAt        func(gopay.Session) time.Time
	uncertain     func(gopay.Session) bool
	readyStage    string
	cycleStage    string
	awaitingStage string
	dispatchEvent string
	timeoutStatus domain.ActivationStatus
	timeoutReason string
}

func TestVerificationResendFinalSaveFailureRecoversWithoutUncountedDispatch(t *testing.T) {
	for _, test := range []verificationResendRecoveryCase{
		{
			name: "login 2FA",
			state: gopay.Session{
				Phone:            "81234567890",
				CountryCode:      "+62",
				Device:           gopay.GenerateDeviceIdentity("81234567890"),
				VerificationID:   "login-verification-2",
				OTPToken:         "login-2fa-otp-token-1",
				AccountID:        "account-1",
				TwoFAToken:       "two-fa-token",
				SMSCycle:         1,
				LoginStage:       gopay.LoginStageCycleReady2FA,
				LoginCodeResends: 1,
			},
			poll: func(manager *Manager, ctx context.Context, activation domain.Activation) error {
				return manager.pollLoginCode(ctx, activation)
			},
			age: func(state *gopay.Session) {
				state.LoginCodeSentAt = time.Now().UTC().Add(-verificationCodeWait - time.Second)
			},
			stage: func(state gopay.Session) string { return string(state.LoginStage) },
			count: func(state gopay.Session) int { return state.LoginCodeResends },
			sentAt: func(state gopay.Session) time.Time {
				return state.LoginCodeSentAt
			},
			uncertain:     func(state gopay.Session) bool { return state.LoginCodeDispatchUncertain },
			readyStage:    string(gopay.LoginStageReady2FA),
			cycleStage:    string(gopay.LoginStageCycleReady2FA),
			awaitingStage: string(gopay.LoginStageAwaiting2FAOTP),
			dispatchEvent: "gopay:initiate:login_2fa",
			timeoutStatus: domain.ActivationStatusLoginCodeTimeout,
			timeoutReason: "登录验证码重发 3 次后仍未收到",
		},
		{
			name: "PIN setup",
			state: gopay.Session{
				Phone:             "81234567890",
				CountryCode:       "+62",
				Device:            gopay.GenerateDeviceIdentity("81234567890"),
				AccessToken:       "access-token",
				PINVerificationID: "pin-verification-1",
				PINOTPToken:       "pin-otp-token-1",
				SMSCycle:          1,
				PINStage:          gopay.PINStageCycleReady,
				PINCodeResends:    1,
			},
			poll: func(manager *Manager, ctx context.Context, activation domain.Activation) error {
				return manager.pollPINCode(ctx, activation)
			},
			age: func(state *gopay.Session) {
				state.PINCodeSentAt = time.Now().UTC().Add(-verificationCodeWait - time.Second)
			},
			stage: func(state gopay.Session) string { return string(state.PINStage) },
			count: func(state gopay.Session) int { return state.PINCodeResends },
			sentAt: func(state gopay.Session) time.Time {
				return state.PINCodeSentAt
			},
			uncertain:     func(state gopay.Session) bool { return state.PINCodeDispatchUncertain },
			readyStage:    string(gopay.PINStageReadyCycle),
			cycleStage:    string(gopay.PINStageCycleReady),
			awaitingStage: string(gopay.PINStageAwaiting),
			dispatchEvent: "gopay:initiate:goto_pin_wa_sms",
			timeoutStatus: domain.ActivationStatusPINCodeTimeout,
			timeoutReason: "改 PIN 验证码重发 3 次后仍未收到",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			exerciseVerificationResendFinalSaveFailure(t, test)
		})
	}
}

func exerciseVerificationResendFinalSaveFailure(t *testing.T, test verificationResendRecoveryCase) {
	t.Helper()
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	status := domain.ActivationStatusAwaitingLoginCode
	if test.timeoutStatus == domain.ActivationStatusPINCodeTimeout {
		status = domain.ActivationStatusAwaitingPINCode
	}
	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, test.state, status)
	store.mu.Lock()
	store.activation.SMSCycle = 1
	store.mu.Unlock()
	failingStore := &verificationResendFailingStore{
		verificationResendFlowStore: store,
		failAt:                      2,
	}
	manager.store = failingStore

	err := test.poll(manager, context.Background(), store.activationSnapshot())
	if !errors.Is(err, errVerificationResendFinalSave) {
		t.Fatalf("dispatch error = %v, want injected final save error", err)
	}
	state := store.session(t, box)
	if got := test.stage(state); got != test.awaitingStage {
		t.Fatalf("persisted recovery stage = %q, want %q", got, test.awaitingStage)
	}
	if got := test.count(state); got != 1 {
		t.Fatalf("persisted recovery resend count = %d, want 1", got)
	}
	if !test.uncertain(state) {
		t.Fatal("persisted pre-dispatch checkpoint is not marked uncertain")
	}
	if sentAt := test.sentAt(state); !sentAt.IsZero() {
		t.Fatalf("pre-dispatch recovery timestamp = %s, want zero until recovery anchors a full wait", sentAt)
	}
	if got := countVerificationResendEvent(events.snapshot(), test.dispatchEvent); got != 1 {
		t.Fatalf("GoPay dispatches after failed final save = %d, want 1", got)
	}

	// A restarted worker restores the pre-dispatch awaiting checkpoint. During
	// its full wait window it must poll the provider without sending another
	// uncounted GoPay OTP.
	recoveryStartedAt := time.Now().UTC()
	if err := test.poll(manager, context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("immediate recovery poll: %v", err)
	}
	recoveryFinishedAt := time.Now().UTC()
	state = store.session(t, box)
	if got := test.stage(state); got != test.awaitingStage || test.count(state) != 1 {
		t.Fatalf("immediate recovery state = stage %q count %d", got, test.count(state))
	}
	if !test.uncertain(state) {
		t.Fatal("immediate recovery cleared the uncertain dispatch before a new token was saved")
	}
	if sentAt := test.sentAt(state); sentAt.Before(recoveryStartedAt) || sentAt.After(recoveryFinishedAt) {
		t.Fatalf("recovery wait timestamp = %s, want within [%s, %s]", sentAt, recoveryStartedAt, recoveryFinishedAt)
	}
	if got := countVerificationResendEvent(events.snapshot(), test.dispatchEvent); got != 1 {
		t.Fatalf("GoPay dispatches during recovery wait = %d, want 1", got)
	}

	// Complete attempts two and three. The timeout transition reserves each
	// attempt before either the provider or GoPay is asked to send again.
	for attempt := 2; attempt <= verificationCodeResends; attempt++ {
		rewriteVerificationResendSession(t, store, box, test.age)
		if err := test.poll(manager, context.Background(), store.activationSnapshot()); err != nil {
			t.Fatalf("attempt %d timeout transition: %v", attempt, err)
		}
		state = store.session(t, box)
		if got := test.stage(state); got != test.readyStage || test.count(state) != attempt {
			t.Fatalf("attempt %d ready state = stage %q count %d", attempt, got, test.count(state))
		}

		if err := test.poll(manager, context.Background(), store.activationSnapshot()); err != nil {
			t.Fatalf("attempt %d provider resend: %v", attempt, err)
		}
		state = store.session(t, box)
		if got := test.stage(state); got != test.cycleStage || test.count(state) != attempt {
			t.Fatalf("attempt %d cycle state = stage %q count %d", attempt, got, test.count(state))
		}

		if err := test.poll(manager, context.Background(), store.activationSnapshot()); err != nil {
			t.Fatalf("attempt %d GoPay resend: %v", attempt, err)
		}
		state = store.session(t, box)
		if got := test.stage(state); got != test.awaitingStage || test.count(state) != attempt {
			t.Fatalf("attempt %d awaiting state = stage %q count %d", attempt, got, test.count(state))
		}
		if test.uncertain(state) {
			t.Fatalf("attempt %d successful dispatch remained uncertain", attempt)
		}
	}

	if got := countVerificationResendEvent(events.snapshot(), test.dispatchEvent); got != verificationCodeResends {
		t.Fatalf("GoPay dispatches before terminal wait = %d, want %d", got, verificationCodeResends)
	}
	rewriteVerificationResendSession(t, store, box, test.age)
	if err := test.poll(manager, context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("terminal timeout poll: %v", err)
	}

	activation := store.activationSnapshot()
	if activation.Status != test.timeoutStatus || activation.FailureReason != test.timeoutReason {
		t.Fatalf("terminal activation = status %q reason %q", activation.Status, activation.FailureReason)
	}
	allEvents := events.snapshot()
	if got := countVerificationResendEvent(allEvents, test.dispatchEvent); got != verificationCodeResends {
		t.Fatalf("terminal GoPay dispatches = %d, want %d", got, verificationCodeResends)
	}
	if got := countVerificationResendEvent(allEvents, "provider:setStatus:8"); got != 1 {
		t.Fatalf("provider cancellations = %d, want 1", got)
	}
}

func TestUncertainVerificationDispatchDoesNotConsumeCodeWithStaleToken(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      gopay.Session
		status     domain.ActivationStatus
		poll       func(*Manager, context.Context, domain.Activation) error
		age        func(*gopay.Session)
		stage      func(gopay.Session) string
		count      func(gopay.Session) int
		readyStage string
	}{
		{
			name: "login 2FA",
			state: gopay.Session{
				Phone:                      "81234567890",
				CountryCode:                "+62",
				Device:                     gopay.GenerateDeviceIdentity("81234567890"),
				VerificationID:             "login-verification-2",
				OTPToken:                   "stale-login-token",
				AccountID:                  "account-1",
				TwoFAToken:                 "two-fa-token",
				LoginStage:                 gopay.LoginStageAwaiting2FAOTP,
				LoginCodeSentAt:            time.Now().UTC(),
				LoginCodeResends:           1,
				LoginCodeDispatchUncertain: true,
			},
			status: domain.ActivationStatusAwaitingLoginCode,
			poll: func(manager *Manager, ctx context.Context, activation domain.Activation) error {
				return manager.pollLoginCode(ctx, activation)
			},
			age: func(state *gopay.Session) {
				state.LoginCodeSentAt = time.Now().UTC().Add(-verificationCodeWait - time.Second)
			},
			stage:      func(state gopay.Session) string { return string(state.LoginStage) },
			count:      func(state gopay.Session) int { return state.LoginCodeResends },
			readyStage: string(gopay.LoginStageReady2FA),
		},
		{
			name: "PIN setup",
			state: gopay.Session{
				Phone:                    "81234567890",
				CountryCode:              "+62",
				Device:                   gopay.GenerateDeviceIdentity("81234567890"),
				AccessToken:              "access-token",
				PINVerificationID:        "pin-verification-1",
				PINOTPToken:              "stale-pin-token",
				PINStage:                 gopay.PINStageAwaiting,
				PINCodeSentAt:            time.Now().UTC(),
				PINCodeResends:           1,
				PINCodeDispatchUncertain: true,
			},
			status: domain.ActivationStatusAwaitingPINCode,
			poll: func(manager *Manager, ctx context.Context, activation domain.Activation) error {
				return manager.pollPINCode(ctx, activation)
			},
			age: func(state *gopay.Session) {
				state.PINCodeSentAt = time.Now().UTC().Add(-verificationCodeWait - time.Second)
			},
			stage:      func(state gopay.Session) string { return string(state.PINStage) },
			count:      func(state gopay.Session) int { return state.PINCodeResends },
			readyStage: string(gopay.PINStageReadyCycle),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &verificationResendEventLog{}
			provider := newVerificationResendProvider(t, events, "STATUS_OK:4815")
			defer provider.Close()
			goPay := newVerificationResendGoPay(t, events)
			defer goPay.Close()
			manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, test.state, test.status)

			if err := test.poll(manager, context.Background(), store.activationSnapshot()); err != nil {
				t.Fatalf("poll inside uncertain wait: %v", err)
			}
			if got := events.snapshot(); !reflect.DeepEqual(got, []string{"provider:getStatus"}) {
				t.Fatalf("events inside uncertain wait = %v; stale token must not verify or redispatch", got)
			}
			store.mu.Lock()
			verificationCount := len(store.verifications)
			store.mu.Unlock()
			if verificationCount != 0 {
				t.Fatalf("stored verifications inside uncertain wait = %d, want 0", verificationCount)
			}

			rewriteVerificationResendSession(t, store, box, test.age)
			if err := test.poll(manager, context.Background(), store.activationSnapshot()); err != nil {
				t.Fatalf("poll after uncertain wait: %v", err)
			}
			state := store.session(t, box)
			if got := test.stage(state); got != test.readyStage || test.count(state) != 2 {
				t.Fatalf("state after uncertain wait = stage %q count %d, want %q count 2", got, test.count(state), test.readyStage)
			}
			if got := events.snapshot(); !reflect.DeepEqual(got, []string{"provider:getStatus", "provider:getStatus"}) {
				t.Fatalf("events after uncertain wait = %v; stale token must not verify", got)
			}
		})
	}
}

func TestSuccessfulVerificationDispatchWaitStartsAfterGoPayReturns(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     gopay.Session
		status    domain.ActivationStatus
		poll      func(*Manager, context.Context, domain.Activation) error
		sentAt    func(gopay.Session) time.Time
		wantEvent string
	}{
		{
			name: "login 2FA",
			state: gopay.Session{
				Phone:            "81234567890",
				CountryCode:      "+62",
				Device:           gopay.GenerateDeviceIdentity("81234567890"),
				VerificationID:   "login-verification-2",
				OTPToken:         "login-token-1",
				AccountID:        "account-1",
				TwoFAToken:       "two-fa-token",
				LoginStage:       gopay.LoginStageCycleReady2FA,
				LoginCodeResends: 1,
			},
			status: domain.ActivationStatusAwaitingLoginCode,
			poll: func(manager *Manager, ctx context.Context, activation domain.Activation) error {
				return manager.pollLoginCode(ctx, activation)
			},
			sentAt:    func(state gopay.Session) time.Time { return state.LoginCodeSentAt },
			wantEvent: "gopay:initiate:login_2fa",
		},
		{
			name: "PIN setup",
			state: gopay.Session{
				Phone:          "81234567890",
				CountryCode:    "+62",
				Device:         gopay.GenerateDeviceIdentity("81234567890"),
				AccessToken:    "access-token",
				PINStage:       gopay.PINStageCycleReady,
				PINCodeResends: 1,
			},
			status: domain.ActivationStatusAwaitingPINCode,
			poll: func(manager *Manager, ctx context.Context, activation domain.Activation) error {
				return manager.pollPINCode(ctx, activation)
			},
			sentAt:    func(state gopay.Session) time.Time { return state.PINCodeSentAt },
			wantEvent: "gopay:initiate:goto_pin_wa_sms",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &verificationResendEventLog{}
			provider := newVerificationResendProvider(t, events)
			defer provider.Close()
			goPay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/users/pins/allowed":
					events.add("gopay:pinAllowed")
					_, _ = io.WriteString(w, `{"data":{"allowed":true}}`)
				case "/cvs/v1/methods":
					events.add("gopay:pinMethods")
					_, _ = io.WriteString(w, `{"data":{"verification_id":"pin-verification-2","methods":["otp_sms"]}}`)
				case "/cvs/v1//initiate":
					var body struct {
						Flow string `json:"flow"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, "invalid JSON", http.StatusBadRequest)
						return
					}
					events.add("gopay:initiate:" + body.Flow)
					time.Sleep(80 * time.Millisecond)
					_, _ = io.WriteString(w, `{"data":{"otp_token":"delayed-token","otp_length":4}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer goPay.Close()

			manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, test.state, test.status)
			startedAt := time.Now().UTC()
			if err := test.poll(manager, context.Background(), store.activationSnapshot()); err != nil {
				t.Fatal(err)
			}
			finishedAt := time.Now().UTC()
			if finishedAt.Sub(startedAt) < 70*time.Millisecond {
				t.Fatalf("fixture dispatch only took %s; delay was not exercised", finishedAt.Sub(startedAt))
			}
			sentAt := test.sentAt(store.session(t, box))
			if sentAt.Before(finishedAt.Add(-20 * time.Millisecond)) {
				t.Fatalf("saved sentAt %s precedes dispatch completion %s", sentAt, finishedAt)
			}
			if countVerificationResendEvent(events.snapshot(), test.wantEvent) != 1 {
				t.Fatalf("events = %v", events.snapshot())
			}
		})
	}
}

var errVerificationCycleAdvance = errors.New("injected verification cycle advance failure")

type verificationCycleAdvanceFailingStore struct {
	*verificationResendFlowStore

	mu      sync.Mutex
	attempt int
}

func (s *verificationCycleAdvanceFailingStore) AdvanceSMSCycle(ctx context.Context, id int64, owner string, nextRunAt time.Time) (int, error) {
	s.mu.Lock()
	s.attempt++
	attempt := s.attempt
	s.mu.Unlock()
	if attempt == 1 {
		return 0, errVerificationCycleAdvance
	}
	return s.verificationResendFlowStore.AdvanceSMSCycle(ctx, id, owner, nextRunAt)
}

func TestVerificationCycleRecoversWhenProviderAdvancedBeforeLocalSave(t *testing.T) {
	events := &verificationResendEventLog{}
	var setStatusCalls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getStatus":
			events.add("provider:getStatus")
			_, _ = io.WriteString(w, "STATUS_WAIT_CODE")
		case "setStatus":
			events.add("provider:setStatus:" + r.URL.Query().Get("status"))
			setStatusCalls++
			if setStatusCalls == 1 {
				_, _ = io.WriteString(w, "ACCESS_RETRY_GET")
			} else {
				_, _ = io.WriteString(w, "BAD_STATUS")
			}
		default:
			http.Error(w, "unexpected provider action", http.StatusBadRequest)
		}
	}))
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone:            "81234567890",
		CountryCode:      "+62",
		Device:           gopay.GenerateDeviceIdentity("81234567890"),
		VerificationID:   "login-verification-1",
		OTPToken:         "login-token-1",
		Methods:          []string{"otp_sms"},
		LoginStage:       gopay.LoginStageReady1FA,
		LoginCodeSentAt:  time.Now().UTC().Add(-verificationCodeWait - time.Second),
		LoginCodeResends: 1,
	}, domain.ActivationStatusAwaitingLoginCode)
	failingStore := &verificationCycleAdvanceFailingStore{verificationResendFlowStore: store}
	manager.store = failingStore

	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); !errors.Is(err, errVerificationCycleAdvance) {
		t.Fatalf("first provider cycle request error = %v, want injected local save failure", err)
	}
	if state := store.session(t, box); state.LoginStage != gopay.LoginStageReady1FA || state.VerificationCycleRequest != gopay.VerificationCycleRequestAccepted {
		t.Fatalf("state after failed local cycle save = stage %q request %q, want ready/accepted", state.LoginStage, state.VerificationCycleRequest)
	}
	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("recover provider cycle after BAD_STATUS: %v", err)
	}
	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageCycleReady1FA || state.SMSCycle != 1 {
		t.Fatalf("recovered cycle state = stage %q cycle %d", state.LoginStage, state.SMSCycle)
	}
	if state.VerificationCycleRequest != gopay.VerificationCycleRequestNone {
		t.Fatalf("recovered provider request state = %q, want empty", state.VerificationCycleRequest)
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, []string{
		"provider:getStatus",
		"provider:setStatus:3",
		"provider:getStatus",
	}) {
		t.Fatalf("recovery events = %v", got)
	}
}

func TestVerificationCycleRetriesAcceptedCheckpointSave(t *testing.T) {
	events := &verificationResendEventLog{}
	var setStatusCalls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getStatus":
			events.add("provider:getStatus")
			_, _ = io.WriteString(w, "STATUS_WAIT_CODE")
		case "setStatus":
			events.add("provider:setStatus:" + r.URL.Query().Get("status"))
			setStatusCalls++
			if setStatusCalls == 1 {
				_, _ = io.WriteString(w, "ACCESS_RETRY_GET")
			} else {
				_, _ = io.WriteString(w, "BAD_STATUS")
			}
		default:
			http.Error(w, "unexpected provider action", http.StatusBadRequest)
		}
	}))
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone:            "81234567890",
		CountryCode:      "+62",
		Device:           gopay.GenerateDeviceIdentity("81234567890"),
		VerificationID:   "login-verification-1",
		OTPToken:         "login-token-1",
		Methods:          []string{"otp_sms"},
		LoginStage:       gopay.LoginStageReady1FA,
		LoginCodeSentAt:  time.Now().UTC().Add(-verificationCodeWait - time.Second),
		LoginCodeResends: 1,
	}, domain.ActivationStatusAwaitingLoginCode)
	failingStore := &verificationResendFailingStore{
		verificationResendFlowStore: store,
		failAt:                      2,
	}
	manager.store = failingStore

	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("provider cycle request after transient accepted-checkpoint failure: %v", err)
	}
	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageCycleReady1FA || state.SMSCycle != 1 || state.VerificationCycleRequest != gopay.VerificationCycleRequestNone {
		t.Fatalf("state after checkpoint retry = stage %q cycle %d request %q", state.LoginStage, state.SMSCycle, state.VerificationCycleRequest)
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, []string{
		"provider:getStatus",
		"provider:setStatus:3",
	}) {
		t.Fatalf("checkpoint retry events = %v", got)
	}
}

func TestVerificationCycleRetriesRejectedCheckpointSave(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getStatus":
			events.add("provider:getStatus")
			_, _ = io.WriteString(w, "STATUS_WAIT_CODE")
		case "setStatus":
			events.add("provider:setStatus:" + r.URL.Query().Get("status"))
			_, _ = io.WriteString(w, "BAD_STATUS")
		default:
			http.Error(w, "unexpected provider action", http.StatusBadRequest)
		}
	}))
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone:            "81234567890",
		CountryCode:      "+62",
		Device:           gopay.GenerateDeviceIdentity("81234567890"),
		VerificationID:   "login-verification-1",
		OTPToken:         "login-token-1",
		Methods:          []string{"otp_sms"},
		LoginStage:       gopay.LoginStageReady1FA,
		LoginCodeSentAt:  time.Now().UTC().Add(-verificationCodeWait - time.Second),
		LoginCodeResends: 1,
	}, domain.ActivationStatusAwaitingLoginCode)
	failingStore := &verificationResendFailingStore{
		verificationResendFlowStore: store,
		failAt:                      2,
	}
	manager.store = failingStore

	err := manager.pollLoginCode(context.Background(), store.activationSnapshot())
	if !smsbower.IsAPIError(err, "BAD_STATUS") {
		t.Fatalf("first BAD_STATUS error = %v", err)
	}
	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageReady1FA || state.SMSCycle != 0 || state.VerificationCycleRequest != gopay.VerificationCycleRequestNone {
		t.Fatalf("state after rejected-checkpoint retry = stage %q cycle %d request %q", state.LoginStage, state.SMSCycle, state.VerificationCycleRequest)
	}
	store.mu.Lock()
	advanceCalls := store.advanceCalls
	store.mu.Unlock()
	if advanceCalls != 0 {
		t.Fatalf("local cycle advances after rejected-checkpoint retry = %d, want 0", advanceCalls)
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, []string{"provider:getStatus", "provider:setStatus:3"}) {
		t.Fatalf("rejected-checkpoint retry events = %v", got)
	}
}

func TestVerificationCycleDoesNotAdvanceOnFirstBadStatus(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getStatus":
			events.add("provider:getStatus")
			_, _ = io.WriteString(w, "STATUS_WAIT_CODE")
		case "setStatus":
			events.add("provider:setStatus:" + r.URL.Query().Get("status"))
			_, _ = io.WriteString(w, "BAD_STATUS")
		default:
			http.Error(w, "unexpected provider action", http.StatusBadRequest)
		}
	}))
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone:            "81234567890",
		CountryCode:      "+62",
		Device:           gopay.GenerateDeviceIdentity("81234567890"),
		VerificationID:   "login-verification-1",
		OTPToken:         "login-token-1",
		Methods:          []string{"otp_sms"},
		LoginStage:       gopay.LoginStageReady1FA,
		LoginCodeSentAt:  time.Now().UTC().Add(-verificationCodeWait - time.Second),
		LoginCodeResends: 1,
	}, domain.ActivationStatusAwaitingLoginCode)

	err := manager.pollLoginCode(context.Background(), store.activationSnapshot())
	if !smsbower.IsAPIError(err, "BAD_STATUS") {
		t.Fatalf("first BAD_STATUS error = %v", err)
	}
	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageReady1FA || state.SMSCycle != 0 || state.VerificationCycleRequest != gopay.VerificationCycleRequestNone {
		t.Fatalf("state after first BAD_STATUS = stage %q cycle %d request %q", state.LoginStage, state.SMSCycle, state.VerificationCycleRequest)
	}
	store.mu.Lock()
	advanceCalls := store.advanceCalls
	store.mu.Unlock()
	if advanceCalls != 0 {
		t.Fatalf("local cycle advances after first BAD_STATUS = %d, want 0", advanceCalls)
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, []string{"provider:getStatus", "provider:setStatus:3"}) {
		t.Fatalf("first BAD_STATUS events = %v", got)
	}
}

func TestVerificationCycleRecoversAmbiguousProviderResponse(t *testing.T) {
	for _, test := range []struct {
		name          string
		firstResponse func(http.ResponseWriter)
	}{
		{
			name: "HTTP 500",
			firstResponse: func(w http.ResponseWriter) {
				http.Error(w, "gateway lost the acknowledgement", http.StatusInternalServerError)
			},
		},
		{
			name:          "empty 200 response",
			firstResponse: func(http.ResponseWriter) {},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &verificationResendEventLog{}
			var setStatusCalls int
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Query().Get("action") {
				case "getStatus":
					events.add("provider:getStatus")
					_, _ = io.WriteString(w, "STATUS_WAIT_CODE")
				case "setStatus":
					events.add("provider:setStatus:" + r.URL.Query().Get("status"))
					setStatusCalls++
					if setStatusCalls == 1 {
						test.firstResponse(w)
					} else {
						_, _ = io.WriteString(w, "BAD_STATUS")
					}
				default:
					http.Error(w, "unexpected provider action", http.StatusBadRequest)
				}
			}))
			defer provider.Close()
			goPay := newVerificationResendGoPay(t, events)
			defer goPay.Close()

			manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
				Phone:            "81234567890",
				CountryCode:      "+62",
				Device:           gopay.GenerateDeviceIdentity("81234567890"),
				VerificationID:   "login-verification-1",
				OTPToken:         "login-token-1",
				Methods:          []string{"otp_sms"},
				LoginStage:       gopay.LoginStageReady1FA,
				LoginCodeSentAt:  time.Now().UTC().Add(-verificationCodeWait - time.Second),
				LoginCodeResends: 1,
			}, domain.ActivationStatusAwaitingLoginCode)

			if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err == nil {
				t.Fatal("ambiguous provider response error = nil")
			}
			state := store.session(t, box)
			if state.LoginStage != gopay.LoginStageReady1FA || state.VerificationCycleRequest != gopay.VerificationCycleRequestDispatching {
				t.Fatalf("ambiguous response state = stage %q request %q", state.LoginStage, state.VerificationCycleRequest)
			}
			if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
				t.Fatalf("recover ambiguous response through BAD_STATUS: %v", err)
			}
			state = store.session(t, box)
			if state.LoginStage != gopay.LoginStageCycleReady1FA || state.SMSCycle != 1 || state.VerificationCycleRequest != gopay.VerificationCycleRequestNone {
				t.Fatalf("recovered state = stage %q cycle %d request %q", state.LoginStage, state.SMSCycle, state.VerificationCycleRequest)
			}
			if got := events.snapshot(); !reflect.DeepEqual(got, []string{
				"provider:getStatus",
				"provider:setStatus:3",
				"provider:getStatus",
				"provider:setStatus:3",
			}) {
				t.Fatalf("ambiguous response recovery events = %v", got)
			}
		})
	}
}

func rewriteVerificationResendSession(
	t *testing.T,
	store *verificationResendFlowStore,
	box *secure.Box,
	mutate func(*gopay.Session),
) {
	t.Helper()
	state := store.session(t, box)
	mutate(&state)
	raw, err := state.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.account.CredentialsEnc = encrypted
	store.mu.Unlock()
}

func countVerificationResendEvent(events []string, want string) int {
	count := 0
	for _, event := range events {
		if event == want {
			count++
		}
	}
	return count
}
