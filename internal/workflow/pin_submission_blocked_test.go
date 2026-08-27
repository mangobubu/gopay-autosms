package workflow

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type pinSubmissionBlockedTransition struct {
	expected []domain.ActivationStatus
	next     domain.ActivationStatus
	reason   string
}

type pinSubmissionBlockedStore struct {
	storage.Store

	mu            sync.Mutex
	batch         domain.Batch
	account       domain.Account
	setting       domain.Setting
	activation    domain.Activation
	transitions   []pinSubmissionBlockedTransition
	finalizations [][]domain.ActivationStatus
	releases      int
	fulfilled     int
}

func (s *pinSubmissionBlockedStore) GetBatch(_ context.Context, id int64) (domain.Batch, error) {
	if id != s.batch.ID {
		return domain.Batch{}, storage.ErrNotFound
	}
	return s.batch, nil
}

func (s *pinSubmissionBlockedStore) GetAccountByPhone(_ context.Context, phone string) (domain.Account, error) {
	if phone != s.account.PhoneNumber {
		return domain.Account{}, storage.ErrNotFound
	}
	return s.account, nil
}

func (s *pinSubmissionBlockedStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	if key != s.setting.Key {
		return domain.Setting{}, storage.ErrNotFound
	}
	return s.setting, nil
}

func (s *pinSubmissionBlockedStore) GetActivation(_ context.Context, id int64) (domain.Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.activation.ID {
		return domain.Activation{}, storage.ErrNotFound
	}
	return s.activation, nil
}

func (s *pinSubmissionBlockedStore) TransitionActivationOwned(
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
		return domain.Activation{}, storage.ErrNotFound
	}
	if len(expected) != 0 && !statusIncluded(expected, s.activation.Status) {
		return domain.Activation{}, storage.ErrConflict
	}
	s.transitions = append(s.transitions, pinSubmissionBlockedTransition{
		expected: append([]domain.ActivationStatus(nil), expected...),
		next:     next,
		reason:   reason,
	})
	s.activation.Status = next
	s.activation.FailureReason = reason
	return s.activation, nil
}

func (s *pinSubmissionBlockedStore) FinalizeActivationOwned(
	_ context.Context,
	id int64,
	expected []domain.ActivationStatus,
	owner string,
	leaseVersion int64,
) (domain.Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.activation.ID || owner != s.activation.LeaseOwner || leaseVersion != s.activation.LeaseVersion ||
		!statusIncluded(expected, s.activation.Status) {
		return domain.Activation{}, storage.ErrConflict
	}
	s.finalizations = append(s.finalizations, append([]domain.ActivationStatus(nil), expected...))
	now := time.Now().UTC()
	s.activation.FinishedAt = &now
	return s.activation, nil
}

func (s *pinSubmissionBlockedStore) ReleaseActivationLease(_ context.Context, id int64, owner string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.activation.ID || owner != s.activation.LeaseOwner || s.activation.FinishedAt != nil {
		return storage.ErrConflict
	}
	s.releases++
	return nil
}

func (s *pinSubmissionBlockedStore) MarkActivationFulfilledOwned(context.Context, int64, string, int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fulfilled++
	return true, nil
}

func statusIncluded(statuses []domain.ActivationStatus, candidate domain.ActivationStatus) bool {
	for _, status := range statuses {
		if status == candidate {
			return true
		}
	}
	return false
}

func TestPINSubmissionBlockedCompletesProviderAndRetryKeepsDurableIntent(t *testing.T) {
	const blockedBody = `{"errors":[{"code":"GoPay-112","message_title":"Akunmu diblokir sementara","message":"Kami melihat ada aktivitas yang tidak wajar di akunmu."}]}`
	var pinSubmissionCalls int
	gopayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/users/pins/setup/tokens" {
			http.NotFound(w, r)
			return
		}
		pinSubmissionCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, blockedBody)
	}))
	defer gopayServer.Close()

	var providerStatuses []string
	smsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "setStatus" {
			http.Error(w, "unexpected provider action", http.StatusBadRequest)
			return
		}
		providerStatuses = append(providerStatuses, r.URL.Query().Get("status"))
		if len(providerStatuses) == 1 {
			http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ACCESS_ACTIVATION")
	}))
	defer smsServer.Close()

	box, err := secure.New("pin-submission-blocked-workflow-test")
	if err != nil {
		t.Fatal(err)
	}
	pinCiphertext, err := box.Seal([]byte("123456"))
	if err != nil {
		t.Fatal(err)
	}
	session := gopay.Session{
		Phone:                "81234567890",
		PINStage:             gopay.PINStageSetupVerified,
		PINVerificationToken: "pin-verification-token",
		Device:               gopay.GenerateDeviceIdentity("81234567890"),
	}
	sessionJSON, err := session.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := box.Seal(sessionJSON)
	if err != nil {
		t.Fatal(err)
	}
	activation := domain.Activation{
		ID: 81, BatchID: 71, ProviderActivationID: "provider-pin-blocked-1",
		PhoneNumber: "+6281234567890", Status: domain.ActivationStatusSettingPIN,
		LeaseOwner: "worker-pin-blocked", LeaseVersion: 9,
	}
	store := &pinSubmissionBlockedStore{
		batch: domain.Batch{
			ID: activation.BatchID, CountryCode: "6", TargetPINEnc: pinCiphertext,
			Config: json.RawMessage(`{}`),
		},
		account:    domain.Account{PhoneNumber: activation.PhoneNumber, CredentialsEnc: credentials},
		setting:    loginFailureSMSSetting(t, box),
		activation: activation,
	}
	manager := New(
		store,
		appsettings.New(store, box, smsServer.URL),
		box,
		Config{SSOBaseURL: gopayServer.URL, GoPayBaseURL: gopayServer.URL},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	manager.processActivation(context.Background(), activation)

	store.mu.Lock()
	retry := store.activation
	firstReason := retry.FailureReason
	firstFinished := retry.FinishedAt
	store.mu.Unlock()
	if retry.Status != domain.ActivationStatusPINSubmissionBlocked || firstFinished != nil {
		t.Fatalf("after provider failure status=%q finished=%v, want durable unfinished pin_submission_blocked", retry.Status, firstFinished)
	}
	for _, want := range []string{"阶段：提交新 PIN", "HTTP 403", "错误码：GoPay-112", "Akunmu diblokir sementara"} {
		if !containsText(firstReason, want) {
			t.Fatalf("failure reason=%q, want %q", firstReason, want)
		}
	}

	// Even after the provider TTL has elapsed, the durable classification must
	// retry completion rather than enter the ordinary expiry/cancel path.
	expired := time.Now().UTC().Add(-time.Minute)
	retry.ProviderExpiresAt = &expired
	manager.processActivation(context.Background(), retry)

	store.mu.Lock()
	transitions := append([]pinSubmissionBlockedTransition(nil), store.transitions...)
	finalizations := append([][]domain.ActivationStatus(nil), store.finalizations...)
	finalReason := store.activation.FailureReason
	finished := store.activation.FinishedAt
	releases := store.releases
	fulfilled := store.fulfilled
	store.mu.Unlock()

	if pinSubmissionCalls != 1 {
		t.Fatalf("GoPay PIN submission calls=%d, want 1", pinSubmissionCalls)
	}
	if want := []string{"6", "6"}; !reflect.DeepEqual(providerStatuses, want) {
		t.Fatalf("provider statuses=%v, want %v (status 8 must never be sent)", providerStatuses, want)
	}
	if len(transitions) != 2 ||
		transitions[0].next != domain.ActivationStatusPINSubmissionBlocked ||
		transitions[1].next != domain.ActivationStatusPINSubmissionBlocked {
		t.Fatalf("transitions=%+v, want classification followed by same-state reason preservation", transitions)
	}
	if transitions[0].reason != firstReason || transitions[1].reason != firstReason || finalReason != firstReason {
		t.Fatalf("reason changed across provider retry: first=%q transitions=%+v final=%q", firstReason, transitions, finalReason)
	}
	if want := [][]domain.ActivationStatus{{domain.ActivationStatusPINSubmissionBlocked}}; !reflect.DeepEqual(finalizations, want) {
		t.Fatalf("finalizations=%v, want %v", finalizations, want)
	}
	if finished == nil || releases != 1 || fulfilled != 0 {
		t.Fatalf("finished=%v releases=%d fulfilled=%d, want finalized after one retry without fulfillment", finished, releases, fulfilled)
	}
}

func containsText(value, fragment string) bool {
	return strings.Contains(value, fragment)
}
