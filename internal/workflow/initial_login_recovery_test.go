package workflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

var errInitialLoginCheckpoint = errors.New("injected initial login checkpoint failure")

type initialLoginCheckpointStore struct {
	*verificationResendFlowStore

	eventMu            sync.Mutex
	events             []string
	upsertCalls        int
	failUpsertAt       int
	failAttachOnce     bool
	failTransitionOnce bool
}

func (s *initialLoginCheckpointStore) addEvent(event string) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	s.events = append(s.events, event)
}

func (s *initialLoginCheckpointStore) eventSnapshot() []string {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *initialLoginCheckpointStore) GetAccountByPhone(ctx context.Context, phone string) (domain.Account, error) {
	return s.verificationResendFlowStore.GetAccountByPhone(ctx, phone)
}

func (s *initialLoginCheckpointStore) ListActivations(context.Context, storage.ActivationFilter) ([]domain.Activation, error) {
	return []domain.Activation{s.activationSnapshot()}, nil
}

func (s *initialLoginCheckpointStore) UpsertAccount(ctx context.Context, params storage.UpsertAccountParams) (domain.Account, error) {
	s.eventMu.Lock()
	s.upsertCalls++
	call := s.upsertCalls
	s.events = append(s.events, "save-session")
	fail := s.failUpsertAt == call
	s.eventMu.Unlock()
	if fail {
		return domain.Account{}, errInitialLoginCheckpoint
	}
	return s.verificationResendFlowStore.UpsertAccount(ctx, params)
}

func (s *initialLoginCheckpointStore) AttachActivationAccountOwned(
	_ context.Context,
	activationID, accountID int64,
	owner string,
	leaseVersion int64,
) error {
	s.addEvent("attach-account")
	if s.failAttachOnce {
		s.failAttachOnce = false
		return errInitialLoginCheckpoint
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if activationID != s.activation.ID || owner != s.activation.LeaseOwner ||
		leaseVersion != s.activation.LeaseVersion || accountID != s.account.ID {
		return storage.ErrConflict
	}
	id := accountID
	s.activation.AccountID = &id
	return nil
}

func (s *initialLoginCheckpointStore) TransitionActivationOwned(
	ctx context.Context,
	id int64,
	expected []domain.ActivationStatus,
	next domain.ActivationStatus,
	reason, owner string,
	leaseVersion int64,
) (domain.Activation, error) {
	s.addEvent("transition:" + string(next))
	if s.failTransitionOnce {
		s.failTransitionOnce = false
		return domain.Activation{}, errInitialLoginCheckpoint
	}
	return s.verificationResendFlowStore.TransitionActivationOwned(
		ctx, id, expected, next, reason, owner, leaseVersion,
	)
}

func newInitialLoginCheckpointFixture(
	t *testing.T,
) (*Manager, *initialLoginCheckpointStore, *secure.Box, *int, func()) {
	t.Helper()
	box, err := secure.New("initial-login-checkpoint-test")
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
	base := &verificationResendFlowStore{
		setting: domain.Setting{Key: appsettings.SMSBowerKey, Value: settingValue},
		batch: domain.Batch{
			ID: 11, CountryCode: "6", TargetPINEnc: targetPIN, Config: json.RawMessage(`{}`),
		},
		account: domain.Account{ID: 31},
		activation: domain.Activation{
			ID: 21, BatchID: 11, Provider: "smsbower",
			ProviderActivationID: "provider-activation-1",
			PhoneNumber:          "+6281234567890", Status: domain.ActivationStatusPurchased,
			LeaseOwner: "worker-1", LeaseVersion: 3,
		},
	}
	store := &initialLoginCheckpointStore{verificationResendFlowStore: base}
	initiateCalls := 0
	gopayServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/goto-auth/login/methods":
			store.addEvent("gopay:probe")
			_, _ = io.WriteString(writer, `{"data":{"verification_id":"login-verification-1","methods":["otp_sms"]}}`)
		case "/cvs/v1//initiate":
			initiateCalls++
			store.addEvent("gopay:initiate")
			_, _ = io.WriteString(writer, `{"data":{"otp_token":"initial-otp-token","otp_length":4}}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("action") != "getStatus" {
			http.Error(writer, "unexpected provider action", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(writer, "STATUS_WAIT_CODE")
	}))
	manager := New(
		store,
		appsettings.New(store, box, providerServer.URL),
		box,
		Config{PollInterval: 2 * time.Second, SSOBaseURL: gopayServer.URL, GoPayBaseURL: gopayServer.URL},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	cleanup := func() {
		gopayServer.Close()
		providerServer.Close()
	}
	return manager, store, box, &initiateCalls, cleanup
}

func TestInitialLoginPersistsUncertainIntentBeforeOTPDispatch(t *testing.T) {
	manager, store, box, initiateCalls, cleanup := newInitialLoginCheckpointFixture(t)
	defer cleanup()

	if err := manager.probeAndStartLogin(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"gopay:probe", "save-session", "attach-account",
		"transition:awaiting_login_code", "gopay:initiate", "save-session",
	}
	if got := store.eventSnapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
	if *initiateCalls != 1 {
		t.Fatalf("initial OTP dispatches = %d, want 1", *initiateCalls)
	}
	state := store.session(t, box)
	if state.WorkflowActivationID != store.activation.ID || state.LoginCodeDispatchUncertain ||
		state.LoginStage != gopay.LoginStageAwaiting1FAOTP || state.OTPToken != "initial-otp-token" ||
		state.LoginCodeSentAt.IsZero() {
		t.Fatalf("persisted initial login state = %+v", state)
	}
	activation := store.activationSnapshot()
	if activation.Status != domain.ActivationStatusAwaitingLoginCode || activation.AccountID == nil || *activation.AccountID != store.account.ID {
		t.Fatalf("activation after initial dispatch = %+v", activation)
	}
}

func TestInitialLoginLocalCheckpointGapsRecoverWithoutOTPDispatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*initialLoginCheckpointStore)
	}{
		{name: "account attach", prepare: func(store *initialLoginCheckpointStore) { store.failAttachOnce = true }},
		{name: "activation transition", prepare: func(store *initialLoginCheckpointStore) { store.failTransitionOnce = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, store, box, initiateCalls, cleanup := newInitialLoginCheckpointFixture(t)
			defer cleanup()
			test.prepare(store)

			if err := manager.probeAndStartLogin(context.Background(), store.activationSnapshot()); !errors.Is(err, errInitialLoginCheckpoint) {
				t.Fatalf("first attempt error = %v", err)
			}
			if *initiateCalls != 0 {
				t.Fatalf("OTP dispatched before local checkpoint completed: %d", *initiateCalls)
			}
			state := store.session(t, box)
			if state.WorkflowActivationID != store.activation.ID || !state.LoginCodeDispatchUncertain ||
				state.LoginStage != gopay.LoginStageAwaiting1FAOTP || !state.LoginCodeSentAt.IsZero() {
				t.Fatalf("prepared state = %+v", state)
			}

			if err := manager.probeAndStartLogin(context.Background(), store.activationSnapshot()); err != nil {
				t.Fatalf("checkpoint recovery: %v", err)
			}
			if *initiateCalls != 0 {
				t.Fatalf("checkpoint recovery dispatched an OTP: %d", *initiateCalls)
			}
			activation := store.activationSnapshot()
			if activation.Status != domain.ActivationStatusAwaitingLoginCode || activation.AccountID == nil {
				t.Fatalf("recovered activation = %+v", activation)
			}
			probeCalls := 0
			for _, event := range store.eventSnapshot() {
				if event == "gopay:probe" {
					probeCalls++
				}
			}
			if probeCalls != 1 {
				t.Fatalf("GoPay probe calls = %d, want 1", probeCalls)
			}
		})
	}
}

func TestInitialLoginFinalSaveFailureDoesNotRedispatchOnRecovery(t *testing.T) {
	manager, store, box, initiateCalls, cleanup := newInitialLoginCheckpointFixture(t)
	defer cleanup()
	store.failUpsertAt = 2

	if err := manager.probeAndStartLogin(context.Background(), store.activationSnapshot()); !errors.Is(err, errInitialLoginCheckpoint) {
		t.Fatalf("initial dispatch error = %v", err)
	}
	if *initiateCalls != 1 {
		t.Fatalf("initial OTP dispatches = %d, want 1", *initiateCalls)
	}
	state := store.session(t, box)
	if !state.LoginCodeDispatchUncertain || !state.LoginCodeSentAt.IsZero() || state.OTPToken != "" {
		t.Fatalf("durable crash checkpoint = %+v", state)
	}
	activation := store.activationSnapshot()
	if activation.Status != domain.ActivationStatusAwaitingLoginCode {
		t.Fatalf("activation status = %q", activation.Status)
	}

	started := time.Now().UTC()
	if err := manager.pollLoginCode(context.Background(), activation); err != nil {
		t.Fatalf("recovery wait: %v", err)
	}
	state = store.session(t, box)
	if *initiateCalls != 1 {
		t.Fatalf("recovery re-dispatched initial OTP: %d", *initiateCalls)
	}
	if !state.LoginCodeDispatchUncertain || state.LoginCodeSentAt.Before(started) {
		t.Fatalf("recovered uncertain wait state = %+v", state)
	}
}
