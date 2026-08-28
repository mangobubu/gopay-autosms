package workflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type verificationResendFlowTransition struct {
	expected []domain.ActivationStatus
	next     domain.ActivationStatus
	reason   string
}

type verificationResendFlowStore struct {
	storage.Store

	mu            sync.Mutex
	setting       domain.Setting
	batch         domain.Batch
	account       domain.Account
	activation    domain.Activation
	transitions   []verificationResendFlowTransition
	finalizations [][]domain.ActivationStatus
	verifications []storage.AppendVerificationParams
	advanceCalls  int
}

func (s *verificationResendFlowStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != s.setting.Key {
		return domain.Setting{}, storage.ErrNotFound
	}
	return s.setting, nil
}

func (s *verificationResendFlowStore) GetBatch(_ context.Context, id int64) (domain.Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.batch.ID {
		return domain.Batch{}, storage.ErrNotFound
	}
	return s.batch, nil
}

func (s *verificationResendFlowStore) GetAccountByPhone(_ context.Context, phone string) (domain.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if phone != s.account.PhoneNumber {
		return domain.Account{}, storage.ErrNotFound
	}
	return s.account, nil
}

func (s *verificationResendFlowStore) UpsertAccount(_ context.Context, params storage.UpsertAccountParams) (domain.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account.PhoneNumber = params.PhoneNumber
	s.account.Status = params.Status
	s.account.BalanceRP = params.BalanceRP
	s.account.CredentialsEnc = append([]byte(nil), params.CredentialsEnc...)
	s.account.TargetPINEnc = append([]byte(nil), params.TargetPINEnc...)
	s.account.TokenState = append(json.RawMessage(nil), params.TokenState...)
	s.account.DeviceState = append(json.RawMessage(nil), params.DeviceState...)
	s.account.Metadata = append(json.RawMessage(nil), params.Metadata...)
	s.account.LastLoginAt = params.LastLoginAt
	return s.account, nil
}

func (s *verificationResendFlowStore) TouchActivationPoll(
	context.Context,
	int64,
	string,
	time.Time,
	time.Time,
) error {
	return nil
}

func (s *verificationResendFlowStore) AdvanceSMSCycle(_ context.Context, id int64, owner string, _ time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.activation.ID || owner != s.activation.LeaseOwner {
		return 0, storage.ErrConflict
	}
	s.advanceCalls++
	s.activation.SMSCycle++
	return s.activation.SMSCycle, nil
}

func (s *verificationResendFlowStore) AppendVerificationCodeOwned(
	_ context.Context,
	params storage.AppendVerificationParams,
	owner string,
	leaseVersion int64,
) (storage.AppendVerificationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if params.ActivationID != s.activation.ID || owner != s.activation.LeaseOwner || leaseVersion != s.activation.LeaseVersion {
		return storage.AppendVerificationResult{}, storage.ErrConflict
	}
	s.verifications = append(s.verifications, params)
	return storage.AppendVerificationResult{
		Verification: domain.VerificationCode{
			ActivationID: params.ActivationID,
			CycleNo:      params.CycleNo,
			Phase:        params.Phase,
			Code:         params.Code,
		},
		Inserted: true,
	}, nil
}

func (s *verificationResendFlowStore) TransitionActivationOwned(
	_ context.Context,
	id int64,
	expected []domain.ActivationStatus,
	next domain.ActivationStatus,
	reason string,
	owner string,
	leaseVersion int64,
) (domain.Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.activation.ID || owner != s.activation.LeaseOwner || leaseVersion != s.activation.LeaseVersion {
		return domain.Activation{}, storage.ErrConflict
	}
	if len(expected) != 0 {
		matched := false
		for _, status := range expected {
			if status == s.activation.Status {
				matched = true
				break
			}
		}
		if !matched {
			return domain.Activation{}, storage.ErrConflict
		}
	}
	s.transitions = append(s.transitions, verificationResendFlowTransition{
		expected: append([]domain.ActivationStatus(nil), expected...),
		next:     next,
		reason:   reason,
	})
	s.activation.Status = next
	s.activation.FailureReason = reason
	return s.activation, nil
}

func (s *verificationResendFlowStore) FinalizeActivationOwned(
	_ context.Context,
	id int64,
	expected []domain.ActivationStatus,
	owner string,
	leaseVersion int64,
) (domain.Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.activation.ID || owner != s.activation.LeaseOwner || leaseVersion != s.activation.LeaseVersion {
		return domain.Activation{}, storage.ErrConflict
	}
	if len(expected) != 1 || expected[0] != s.activation.Status {
		return domain.Activation{}, storage.ErrConflict
	}
	s.finalizations = append(s.finalizations, append([]domain.ActivationStatus(nil), expected...))
	return s.activation, nil
}

func (s *verificationResendFlowStore) activationSnapshot() domain.Activation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activation
}

func (s *verificationResendFlowStore) session(t *testing.T, box *secure.Box) gopay.Session {
	t.Helper()
	s.mu.Lock()
	encrypted := append([]byte(nil), s.account.CredentialsEnc...)
	s.mu.Unlock()
	raw, err := box.Open(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	state, err := gopay.ParseSession(raw)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

type verificationResendEventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *verificationResendEventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *verificationResendEventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func newVerificationResendProvider(t *testing.T, events *verificationResendEventLog, statusResponses ...string) *httptest.Server {
	t.Helper()
	statusResponse := "STATUS_WAIT_CODE"
	if len(statusResponses) != 0 {
		statusResponse = statusResponses[0]
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getStatus":
			events.add("provider:getStatus")
			_, _ = io.WriteString(w, statusResponse)
		case "setStatus":
			status := r.URL.Query().Get("status")
			events.add("provider:setStatus:" + status)
			switch status {
			case "3":
				_, _ = io.WriteString(w, "ACCESS_RETRY_GET")
			case "8":
				_, _ = io.WriteString(w, "ACCESS_CANCEL")
			default:
				http.Error(w, "unexpected setStatus", http.StatusBadRequest)
			}
		default:
			http.Error(w, "unexpected provider action", http.StatusBadRequest)
		}
	}))
}

func newVerificationResendGoPay(t *testing.T, events *verificationResendEventLog) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/users/pins/allowed":
			events.add("gopay:pinAllowed")
			_, _ = io.WriteString(w, `{"data":{"allowed":true}}`)
		case "/api/v1/users/pin/challenges":
			events.add("gopay:pinChallenge")
			_, _ = io.WriteString(w, `{"data":{"challenge_id":"challenge-2","client_id":"client-2"}}`)
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
			_, _ = io.WriteString(w, `{"data":{"otp_token":"resent-otp-token","otp_length":4}}`)
		case "/cvs/v1/verify":
			var body struct {
				Flow string `json:"flow"`
				Data struct {
					OTP string `json:"otp"`
				} `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			events.add("gopay:verify:" + body.Flow + ":" + body.Data.OTP)
			_, _ = io.WriteString(w, `{"data":{"verification_token":"verified-token"}}`)
		case "/goto-auth/accountlist":
			events.add("gopay:accountList")
			_, _ = io.WriteString(w, `{"data":{"1fa_token":"one-fa-token","account_list":[{"account_id":"account-1"}]}}`)
		case "/goto-auth/token":
			var body struct {
				GrantType string `json:"grant_type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			events.add("gopay:token:" + body.GrantType)
			_, _ = io.WriteString(w, `{"data":{"access_token":"access-after-code","refresh_token":"refresh-after-code"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func newVerificationResendFlowManager(
	t *testing.T,
	providerURL string,
	goPayURL string,
	state gopay.Session,
	status domain.ActivationStatus,
) (*Manager, *verificationResendFlowStore, *secure.Box) {
	t.Helper()
	box, err := secure.New("verification-code-resend-flow-test")
	if err != nil {
		t.Fatal(err)
	}
	rawSession, err := state.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := box.Seal(rawSession)
	if err != nil {
		t.Fatal(err)
	}
	targetPIN, err := box.Seal([]byte("123456"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, err := box.Seal([]byte("fixture-api-key"))
	if err != nil {
		t.Fatal(err)
	}
	settingValue, err := json.Marshal(map[string]string{
		"api_key_encrypted": base64.StdEncoding.EncodeToString(apiKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &verificationResendFlowStore{
		setting: domain.Setting{Key: appsettings.SMSBowerKey, Value: settingValue},
		batch: domain.Batch{
			ID:           11,
			TargetPINEnc: targetPIN,
		},
		account: domain.Account{
			ID:             31,
			PhoneNumber:    "+6281234567890",
			Status:         domain.AccountStatusPending,
			CredentialsEnc: credentials,
		},
		activation: domain.Activation{
			ID:                   21,
			BatchID:              11,
			Provider:             "smsbower",
			ProviderActivationID: "provider-activation-1",
			PhoneNumber:          "+6281234567890",
			Status:               status,
			LeaseOwner:           "worker-1",
			LeaseVersion:         3,
		},
	}
	manager := New(
		store,
		appsettings.New(store, box, providerURL),
		box,
		Config{PollInterval: 2 * time.Second, SSOBaseURL: goPayURL, GoPayBaseURL: goPayURL},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return manager, store, box
}

func TestLogin1FATimeoutRequestsAnotherSMSAndResendsGoPayOTP(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	oldSentAt := time.Now().UTC().Add(-loginVerificationCodeWait - time.Second)
	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone:            "81234567890",
		CountryCode:      "+62",
		Device:           gopay.GenerateDeviceIdentity("81234567890"),
		VerificationID:   "login-verification-1",
		Methods:          []string{"otp_sms"},
		LoginStage:       gopay.LoginStageAwaiting1FAOTP,
		LoginCodeSentAt:  oldSentAt,
		LoginCodeResends: 0,
	}, domain.ActivationStatusAwaitingLoginCode)

	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollLoginCode timeout: %v", err)
	}
	if state := store.session(t, box); state.LoginStage != gopay.LoginStageReady1FA {
		t.Fatalf("login stage after timeout = %q, want %q", state.LoginStage, gopay.LoginStageReady1FA)
	}
	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollLoginCode provider resend: %v", err)
	}
	if state := store.session(t, box); state.LoginStage != gopay.LoginStageCycleReady1FA {
		t.Fatalf("login stage after provider resend = %q, want %q", state.LoginStage, gopay.LoginStageCycleReady1FA)
	}
	resendStartedAt := time.Now().UTC()
	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollLoginCode resend: %v", err)
	}
	resendFinishedAt := time.Now().UTC()

	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageAwaiting1FAOTP || state.LoginCodeResends != 1 {
		t.Fatalf("login resend state = stage %q count %d", state.LoginStage, state.LoginCodeResends)
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
		"gopay:initiate:login_1fa",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
}

func TestPINResetTimeoutRequestsAnotherSMSAndResendsGoPayOTP(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	oldSentAt := time.Now().UTC().Add(-pinVerificationCodeWait - time.Second)
	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone:             "81234567890",
		CountryCode:       "+62",
		Device:            gopay.GenerateDeviceIdentity("81234567890"),
		AccessToken:       "access-token",
		PINVerificationID: "pin-verification-1",
		PINOTPToken:       "pin-otp-token-1",
		PINChallengeID:    "challenge-1",
		PINClientID:       "client-1",
		PINStage:          gopay.PINStageResetAwaiting,
		PINCodeSentAt:     oldSentAt,
		PINCodeResends:    0,
	}, domain.ActivationStatusAwaitingPINCode)

	if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollPINCode timeout: %v", err)
	}
	if state := store.session(t, box); state.PINStage != gopay.PINStageResetReadyCycle {
		t.Fatalf("PIN stage after timeout = %q, want %q", state.PINStage, gopay.PINStageResetReadyCycle)
	}
	if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollPINCode provider resend: %v", err)
	}
	if state := store.session(t, box); state.PINStage != gopay.PINStageResetCycleReady {
		t.Fatalf("PIN stage after provider resend = %q, want %q", state.PINStage, gopay.PINStageResetCycleReady)
	}
	resendStartedAt := time.Now().UTC()
	if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatalf("pollPINCode resend: %v", err)
	}
	resendFinishedAt := time.Now().UTC()

	state := store.session(t, box)
	if state.PINStage != gopay.PINStageResetAwaiting || state.PINCodeResends != 1 {
		t.Fatalf("PIN resend state = stage %q count %d", state.PINStage, state.PINCodeResends)
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
		"gopay:pinChallenge",
		"gopay:pinMethods",
		"gopay:initiate:goto_pin_wa_sms_gp_app",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
}

func TestPINCodeDoesNotResendBeforeEightySeconds(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
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
		PINStage:          gopay.PINStageAwaiting,
		PINCodeSentAt:     time.Now().UTC().Add(-61 * time.Second),
	}, domain.ActivationStatusAwaitingPINCode)

	if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}

	state := store.session(t, box)
	if state.PINStage != gopay.PINStageAwaiting || state.PINCodeResends != 0 {
		t.Fatalf("PIN state before 80 seconds = stage %q count %d", state.PINStage, state.PINCodeResends)
	}
	store.mu.Lock()
	advanceCalls := store.advanceCalls
	store.mu.Unlock()
	if advanceCalls != 0 {
		t.Fatalf("provider resend calls before 80 seconds = %d, want 0", advanceCalls)
	}
	if got, want := events.snapshot(), []string{"provider:getStatus"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events before 80 seconds = %v, want %v", got, want)
	}
}

func TestLoginReady1FAConsumesLateCodeWithoutRequestingAnotherSMS(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events, "STATUS_OK:7412")
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()

	manager, store, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone:            "81234567890",
		CountryCode:      "+62",
		Device:           gopay.GenerateDeviceIdentity("81234567890"),
		VerificationID:   "login-verification-1",
		OTPToken:         "login-otp-token-1",
		Methods:          []string{"otp_sms"},
		LoginStage:       gopay.LoginStageReady1FA,
		LoginCodeSentAt:  time.Now().UTC().Add(-loginVerificationCodeWait - time.Second),
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
	verifications := append([]storage.AppendVerificationParams(nil), store.verifications...)
	accountStatus := store.account.Status
	store.mu.Unlock()
	if advanceCalls != 0 {
		t.Fatalf("setStatus=3 advance calls = %d, want 0", advanceCalls)
	}
	if len(verifications) != 1 || verifications[0].Phase != domain.VerificationPhaseLogin || verifications[0].Code != "7412" {
		t.Fatalf("login verifications = %+v", verifications)
	}
	if accountStatus != domain.AccountStatusAuthenticated {
		t.Fatalf("account status = %q, want authenticated", accountStatus)
	}
	wantEvents := []string{
		"provider:getStatus",
		"gopay:verify:login_1fa:7412",
		"gopay:accountList",
		"gopay:token:cvs",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
}

func TestPINResetReadyCycleConsumesLateCodeWithResetVerifierWithoutRequestingAnotherSMS(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events, "STATUS_OK:8523")
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
		PINChallengeID:    "challenge-1",
		PINClientID:       "client-1",
		PINStage:          gopay.PINStageResetReadyCycle,
		PINCodeSentAt:     time.Now().UTC().Add(-pinVerificationCodeWait - time.Second),
		PINCodeResends:    2,
	}, domain.ActivationStatusAwaitingPINCode)

	if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}

	state := store.session(t, box)
	if state.PINStage != gopay.PINStageResetVerified || state.PINVerificationToken != "verified-token" {
		t.Fatalf("PIN state = stage %q verification token %q", state.PINStage, state.PINVerificationToken)
	}
	if !state.PINCodeSentAt.IsZero() || state.PINCodeResends != 0 {
		t.Fatalf("PIN resend state after code = sent %s count %d", state.PINCodeSentAt, state.PINCodeResends)
	}
	store.mu.Lock()
	advanceCalls := store.advanceCalls
	verifications := append([]storage.AppendVerificationParams(nil), store.verifications...)
	transitions := append([]verificationResendFlowTransition(nil), store.transitions...)
	store.mu.Unlock()
	if advanceCalls != 0 {
		t.Fatalf("setStatus=3 advance calls = %d, want 0", advanceCalls)
	}
	if len(verifications) != 1 || verifications[0].Phase != domain.VerificationPhasePIN || verifications[0].Code != "8523" {
		t.Fatalf("PIN verifications = %+v", verifications)
	}
	wantTransitions := []verificationResendFlowTransition{{
		expected: []domain.ActivationStatus{domain.ActivationStatusAwaitingPINCode},
		next:     domain.ActivationStatusSettingPIN,
	}}
	if !reflect.DeepEqual(transitions, wantTransitions) {
		t.Fatalf("transitions = %+v, want %+v", transitions, wantTransitions)
	}
	wantEvents := []string{
		"provider:getStatus",
		"gopay:verify:goto_pin_wa_sms_gp_app:8523",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
}

func TestThirdVerificationCodeResendTimeoutCancelsAndClassifiesFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     domain.ActivationStatus
		wantStatus domain.ActivationStatus
		wantReason string
		state      gopay.Session
		poll       func(*Manager, context.Context, domain.Activation) error
	}{
		{
			name:       "login",
			status:     domain.ActivationStatusAwaitingLoginCode,
			wantStatus: domain.ActivationStatusLoginCodeTimeout,
			wantReason: "登录验证码重发 3 次后仍未收到",
			state: gopay.Session{
				Phone:            "81234567890",
				CountryCode:      "+62",
				Device:           gopay.GenerateDeviceIdentity("81234567890"),
				VerificationID:   "login-verification-1",
				Methods:          []string{"otp_sms"},
				LoginStage:       gopay.LoginStageAwaiting1FAOTP,
				LoginCodeSentAt:  time.Now().UTC().Add(-loginVerificationCodeWait - time.Second),
				LoginCodeResends: verificationCodeResends,
			},
			poll: func(manager *Manager, ctx context.Context, activation domain.Activation) error {
				return manager.pollLoginCode(ctx, activation)
			},
		},
		{
			name:       "pin reset",
			status:     domain.ActivationStatusAwaitingPINCode,
			wantStatus: domain.ActivationStatusPINCodeTimeout,
			wantReason: "改 PIN 验证码重发 3 次后仍未收到",
			state: gopay.Session{
				Phone:          "81234567890",
				CountryCode:    "+62",
				Device:         gopay.GenerateDeviceIdentity("81234567890"),
				AccessToken:    "access-token",
				PINStage:       gopay.PINStageResetAwaiting,
				PINCodeSentAt:  time.Now().UTC().Add(-pinVerificationCodeWait - time.Second),
				PINCodeResends: verificationCodeResends,
			},
			poll: func(manager *Manager, ctx context.Context, activation domain.Activation) error {
				return manager.pollPINCode(ctx, activation)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &verificationResendEventLog{}
			provider := newVerificationResendProvider(t, events)
			defer provider.Close()
			goPay := newVerificationResendGoPay(t, events)
			defer goPay.Close()
			manager, store, _ := newVerificationResendFlowManager(t, provider.URL, goPay.URL, test.state, test.status)

			if err := test.poll(manager, context.Background(), store.activationSnapshot()); err != nil {
				t.Fatal(err)
			}

			store.mu.Lock()
			transitions := append([]verificationResendFlowTransition(nil), store.transitions...)
			finalizations := append([][]domain.ActivationStatus(nil), store.finalizations...)
			activation := store.activation
			advanceCalls := store.advanceCalls
			store.mu.Unlock()
			wantTransitions := []verificationResendFlowTransition{{
				expected: []domain.ActivationStatus{test.status},
				next:     test.wantStatus,
				reason:   test.wantReason,
			}}
			if !reflect.DeepEqual(transitions, wantTransitions) {
				t.Fatalf("transitions = %+v, want %+v", transitions, wantTransitions)
			}
			wantFinalizations := [][]domain.ActivationStatus{{test.wantStatus}}
			if !reflect.DeepEqual(finalizations, wantFinalizations) {
				t.Fatalf("finalizations = %v, want %v", finalizations, wantFinalizations)
			}
			if activation.Status != test.wantStatus || activation.FailureReason != test.wantReason {
				t.Fatalf("activation = status %q reason %q", activation.Status, activation.FailureReason)
			}
			if advanceCalls != 0 {
				t.Fatalf("setStatus=3 advance calls = %d, want 0 after third resend wait", advanceCalls)
			}
			wantEvents := []string{"provider:getStatus", "provider:setStatus:8"}
			if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
				t.Fatalf("events = %v, want %v", got, wantEvents)
			}
		})
	}
}
