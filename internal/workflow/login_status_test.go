package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type credentialCASFixtureStore struct {
	storage.Store

	mu            sync.Mutex
	account       domain.Account
	updateCalls   int
	forceConflict bool
	retryableLeft int
	contextErr    error
}

func (s *credentialCASFixtureStore) GetAccount(_ context.Context, id int64) (domain.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.account.ID {
		return domain.Account{}, storage.ErrNotFound
	}
	return cloneFixtureAccount(s.account), nil
}

func (s *credentialCASFixtureStore) UpdateAccountCredentialsIfUnchanged(
	ctx context.Context,
	id int64,
	expectedCredentialsEnc []byte,
	nextCredentialsEnc []byte,
	deviceState json.RawMessage,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateCalls++
	s.contextErr = ctx.Err()
	if s.retryableLeft > 0 {
		s.retryableLeft--
		return storage.ErrRetryable
	}
	if id != s.account.ID {
		return storage.ErrNotFound
	}
	if s.forceConflict {
		s.account.CredentialsEnc = []byte("concurrent-session-ciphertext")
		return storage.ErrConflict
	}
	if !bytes.Equal(s.account.CredentialsEnc, expectedCredentialsEnc) {
		return storage.ErrConflict
	}
	s.account.CredentialsEnc = append([]byte(nil), nextCredentialsEnc...)
	s.account.DeviceState = append(json.RawMessage(nil), deviceState...)
	s.account.UpdatedAt = time.Now().UTC()
	return nil
}

type noCredentialCASFixtureStore struct {
	storage.Store
	upsertCalls int
}

func (s *noCredentialCASFixtureStore) UpsertAccount(context.Context, storage.UpsertAccountParams) (domain.Account, error) {
	s.upsertCalls++
	return domain.Account{}, nil
}

func cloneFixtureAccount(account domain.Account) domain.Account {
	account.CredentialsEnc = append([]byte(nil), account.CredentialsEnc...)
	account.TargetPINEnc = append([]byte(nil), account.TargetPINEnc...)
	account.TokenState = append(json.RawMessage(nil), account.TokenState...)
	account.DeviceState = append(json.RawMessage(nil), account.DeviceState...)
	account.Metadata = append(json.RawMessage(nil), account.Metadata...)
	return account
}

func newCredentialPersistenceFixture(t *testing.T) (*Manager, *credentialCASFixtureStore, domain.Account, gopay.Session) {
	t.Helper()
	box, err := secure.New("login-status-persistence-test")
	if err != nil {
		t.Fatal(err)
	}
	originalSession := gopay.Session{
		Phone:        "81234567890",
		AccountID:    "account-1",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		PINStage:     gopay.PINStageSetupVerified,
		SMSCycle:     3,
		Device:       gopay.GenerateDeviceIdentity("81234567890"),
	}
	raw, err := originalSession.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	balance := 42.5
	now := time.Now().UTC()
	account := domain.Account{
		ID: 7, PhoneNumber: "+6281234567890", Status: domain.AccountStatusActive,
		BalanceRP: &balance, CredentialsEnc: encrypted, TargetPINEnc: []byte("pin-ciphertext"),
		TokenState:  json.RawMessage(`{"worker":"state"}`),
		DeviceState: json.RawMessage(`{"old":"device"}`),
		Metadata:    json.RawMessage(`{"keep":true}`),
		LastLoginAt: &now, UpdatedAt: now,
	}
	store := &credentialCASFixtureStore{account: cloneFixtureAccount(account)}
	return &Manager{store: store, box: box}, store, account, originalSession
}

func TestPersistRotatedSessionUsesCASAfterRequestCancellation(t *testing.T) {
	manager, store, account, rotated := newCredentialPersistenceFixture(t)
	rotated.AccessToken = "access-2"
	rotated.RefreshToken = "refresh-2"

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	version, err := manager.persistRotatedSession(requestCtx, account, rotated)
	if err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	saved := cloneFixtureAccount(store.account)
	updateCalls := store.updateCalls
	updateContextErr := store.contextErr
	store.mu.Unlock()
	if updateCalls != 1 || updateContextErr != nil {
		t.Fatalf("updateCalls=%d updateContextErr=%v", updateCalls, updateContextErr)
	}
	if version != accountCredentialDigest(saved.CredentialsEnc) {
		t.Fatal("returned credential version does not match durable ciphertext")
	}
	raw, err := manager.box.Open(saved.CredentialsEnc)
	if err != nil {
		t.Fatal(err)
	}
	session, err := gopay.ParseSession(raw)
	if err != nil {
		t.Fatal(err)
	}
	if session.AccessToken != "access-2" || session.RefreshToken != "refresh-2" {
		t.Fatalf("saved tokens access=%q refresh=%q", session.AccessToken, session.RefreshToken)
	}
	if saved.Status != account.Status || saved.BalanceRP == nil || *saved.BalanceRP != *account.BalanceRP ||
		!bytes.Equal(saved.TargetPINEnc, account.TargetPINEnc) ||
		!bytes.Equal(saved.TokenState, account.TokenState) ||
		!bytes.Equal(saved.Metadata, account.Metadata) ||
		!saved.LastLoginAt.Equal(*account.LastLoginAt) {
		t.Fatalf("non-credential account fields changed: saved=%+v", saved)
	}
}

func TestPersistRotatedSessionDoesNotOverwriteConcurrentSession(t *testing.T) {
	manager, store, account, rotated := newCredentialPersistenceFixture(t)
	rotated.AccessToken = "access-2"
	rotated.RefreshToken = "refresh-2"
	store.forceConflict = true
	_, err := manager.persistRotatedSession(context.Background(), account, rotated)
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
	store.mu.Lock()
	savedCiphertext := append([]byte(nil), store.account.CredentialsEnc...)
	store.mu.Unlock()
	if !bytes.Equal(savedCiphertext, []byte("concurrent-session-ciphertext")) {
		t.Fatalf("conflicting session was overwritten: %x", savedCiphertext)
	}
}

func TestPersistRotatedSessionRetriesRetryableDatabaseFailure(t *testing.T) {
	manager, store, account, rotated := newCredentialPersistenceFixture(t)
	rotated.AccessToken = "access-2"
	rotated.RefreshToken = "refresh-2"
	store.retryableLeft = 1

	if _, err := manager.persistRotatedSession(context.Background(), account, rotated); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	updateCalls := store.updateCalls
	store.mu.Unlock()
	if updateCalls != 2 {
		t.Fatalf("updateCalls=%d want 2", updateCalls)
	}
}

func TestPersistRotatedSessionHasNoUnsafeUpsertFallback(t *testing.T) {
	box, err := secure.New("login-status-no-fallback-test")
	if err != nil {
		t.Fatal(err)
	}
	store := &noCredentialCASFixtureStore{}
	manager := &Manager{store: store, box: box}
	account := domain.Account{ID: 9, PhoneNumber: "+62812", CredentialsEnc: []byte("old")}
	session := gopay.Session{AccessToken: "access", RefreshToken: "refresh", Device: gopay.GenerateDeviceIdentity("812")}

	_, err = manager.persistRotatedSession(context.Background(), account, session)
	if err == nil || store.upsertCalls != 0 {
		t.Fatalf("err=%v upsertCalls=%d", err, store.upsertCalls)
	}
}

func TestLoginStatusCacheWindowStartsAtCheckedAt(t *testing.T) {
	manager, store, account, _ := newCredentialPersistenceFixture(t)
	account.CredentialsEnc = nil
	store.mu.Lock()
	store.account = cloneFixtureAccount(account)
	store.mu.Unlock()
	manager.cfg.LoginStatusTTL = 4 * time.Second

	view := manager.accountLoginStatus(context.Background(), account)
	manager.loginStatusMu.Lock()
	entry, ok := manager.loginStatusCache[account.ID]
	manager.loginStatusMu.Unlock()
	if !ok {
		t.Fatal("login status result was not cached")
	}
	wantExpiry := view.CheckedAt.Add(manager.cfg.LoginStatusTTL)
	if !entry.expiresAt.Equal(wantExpiry) {
		t.Fatalf("cache expiry=%s want checked_at+ttl=%s", entry.expiresAt, wantExpiry)
	}
}

func TestDefaultLoginStatusCacheExpiresBeforeFiveSecondBrowserPoll(t *testing.T) {
	manager := New(nil, nil, nil, Config{}, nil)
	if manager.cfg.LoginStatusTTL >= 5*time.Second {
		t.Fatalf("default login status cache ttl=%s must be shorter than browser poll", manager.cfg.LoginStatusTTL)
	}
	checkedAt := time.Now().UTC()
	if !checkedAt.Add(manager.cfg.LoginStatusTTL).Before(checkedAt.Add(5 * time.Second)) {
		t.Fatal("cached result would still be fresh at the next browser poll")
	}
}

func TestPublicProbeErrorNeverEchoesUpstreamSecrets(t *testing.T) {
	secret := "Bearer TOP_SECRET_TOKEN"
	for _, err := range []error{
		&gopay.HTTPError{StatusCode: http.StatusBadGateway, Body: []byte(`{"client_secret":"TOP_SECRET_TOKEN"}`)},
		errors.New("upstream echoed " + secret),
	} {
		message := publicProbeError(err)
		if strings.Contains(message, "TOP_SECRET_TOKEN") || strings.Contains(message, "client_secret") || strings.Contains(message, "Bearer") {
			t.Fatalf("public message leaked upstream detail: %q", message)
		}
	}
}

func TestLoginStatusRefreshSurvivesBrowserCancellation(t *testing.T) {
	manager, store, account, _ := newCredentialPersistenceFixture(t)
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var startedOnce sync.Once
	var profileCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/users/profile":
			profileCalls++
			if profileCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"token_expired"}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"account-1"}}`))
		case "/goto-auth/token":
			startedOnce.Do(func() { close(refreshStarted) })
			<-releaseRefresh
			_, _ = w.Write([]byte(`{"data":{"access_token":"access-2","refresh_token":"refresh-2"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager.cfg.SSOBaseURL = server.URL
	manager.cfg.GoPayBaseURL = server.URL
	manager.cfg.LoginStatusTTL = time.Minute

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	resultCh := make(chan AccountLoginStatusView, 1)
	go func() { resultCh <- manager.accountLoginStatus(requestCtx, account) }()
	select {
	case <-refreshStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh request did not start")
	}
	cancelRequest()
	select {
	case view := <-resultCh:
		if view.Status != gopay.LoginStatusUnknown {
			t.Fatalf("canceled caller status=%s", view.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled browser caller did not return promptly")
	}

	manager.loginStatusMu.Lock()
	flight := manager.loginStatusFlights[account.ID]
	manager.loginStatusMu.Unlock()
	if flight == nil {
		t.Fatal("shared status flight ended before refresh was released")
	}
	close(releaseRefresh)
	select {
	case <-flight.done:
	case <-time.After(3 * time.Second):
		t.Fatal("shared status flight did not finish")
	}

	store.mu.Lock()
	saved := cloneFixtureAccount(store.account)
	store.mu.Unlock()
	raw, err := manager.box.Open(saved.CredentialsEnc)
	if err != nil {
		t.Fatal(err)
	}
	session, err := gopay.ParseSession(raw)
	if err != nil {
		t.Fatal(err)
	}
	if session.AccessToken != "access-2" || session.RefreshToken != "refresh-2" {
		t.Fatalf("rotated tokens were not saved: access=%q refresh=%q", session.AccessToken, session.RefreshToken)
	}
}

func TestAccountSessionLockWaitIsCancelableAndEntriesAreReleased(t *testing.T) {
	manager := &Manager{}
	releaseFirst, err := manager.acquireAccountSessionLock(context.Background(), "+6281234567890")
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancelWait := context.WithCancel(context.Background())
	waitResult := make(chan error, 1)
	go func() {
		_, lockErr := manager.acquireAccountSessionLock(waitCtx, "+6281234567890")
		waitResult <- lockErr
	}()
	cancelWait()
	select {
	case lockErr := <-waitResult:
		if !errors.Is(lockErr, context.Canceled) {
			t.Fatalf("lockErr=%v", lockErr)
		}
	case <-time.After(time.Second):
		t.Fatal("account session lock wait ignored cancellation")
	}
	releaseFirst()

	manager.accountSessionMu.Lock()
	entries := len(manager.accountSessionLocks)
	manager.accountSessionMu.Unlock()
	if entries != 0 {
		t.Fatalf("account session lock entries=%d want 0", entries)
	}
}

func TestAccountLoginStatusDoesNotReturnStaleFlightForNewCredentials(t *testing.T) {
	manager, store, oldAccount, session := newCredentialPersistenceFixture(t)
	session.AccessToken = "access-2"
	session.RefreshToken = "refresh-2"
	raw, err := session.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	newCiphertext, err := manager.box.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	currentAccount := cloneFixtureAccount(oldAccount)
	currentAccount.CredentialsEnc = newCiphertext
	store.mu.Lock()
	store.account = cloneFixtureAccount(currentAccount)
	store.mu.Unlock()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/profile" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"account-1"}}`))
	}))
	defer server.Close()
	manager.cfg.SSOBaseURL = server.URL
	manager.cfg.GoPayBaseURL = server.URL
	manager.cfg.LoginStatusTTL = time.Minute
	manager.loginStatusCache = make(map[int64]loginStatusCacheEntry)
	manager.loginStatusFlights = make(map[int64]*loginStatusFlight)
	manager.accountSessionLocks = make(map[string]*accountSessionLockEntry)

	staleFlight := &loginStatusFlight{
		done:              make(chan struct{}),
		credentialVersion: accountCredentialDigest(oldAccount.CredentialsEnc),
		view: AccountLoginStatusView{
			ID: oldAccount.ID, PhoneNumber: oldAccount.PhoneNumber,
			Status: gopay.LoginStatusInvalid, State: gopay.LoginStatusInvalid,
			CheckedAt: time.Now().UTC(), Error: "登录已失效，请重新登录",
		},
	}
	close(staleFlight.done)
	manager.loginStatusFlights[oldAccount.ID] = staleFlight

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	view := manager.accountLoginStatus(ctx, currentAccount)
	if view.Status != gopay.LoginStatusValid || !view.Valid {
		t.Fatalf("stale flight was returned for newer credentials: %+v", view)
	}
}
