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
	"sync/atomic"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type loginCodeTimeoutTransition struct {
	expected []domain.ActivationStatus
	next     domain.ActivationStatus
	reason   string
}

type loginCodeTimeoutStore struct {
	storage.Store

	mu            sync.Mutex
	setting       domain.Setting
	currentStatus domain.ActivationStatus
	transitions   []loginCodeTimeoutTransition
	finalizations [][]domain.ActivationStatus
	releases      int
}

func (s *loginCodeTimeoutStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	if key != s.setting.Key {
		return domain.Setting{}, storage.ErrNotFound
	}
	return s.setting, nil
}

func (s *loginCodeTimeoutStore) GetActivation(context.Context, int64) (domain.Activation, error) {
	return domain.Activation{}, storage.ErrNotFound
}

func (s *loginCodeTimeoutStore) TransitionActivationOwned(
	_ context.Context,
	id int64,
	expected []domain.ActivationStatus,
	next domain.ActivationStatus,
	reason string,
	_ string,
	_ int64,
) (domain.Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(expected) > 0 {
		matched := false
		for _, status := range expected {
			if status == s.currentStatus {
				matched = true
				break
			}
		}
		if !matched {
			return domain.Activation{}, storage.ErrConflict
		}
	}
	s.transitions = append(s.transitions, loginCodeTimeoutTransition{
		expected: append([]domain.ActivationStatus(nil), expected...),
		next:     next,
		reason:   reason,
	})
	s.currentStatus = next
	return domain.Activation{ID: id, Status: next}, nil
}

func (s *loginCodeTimeoutStore) FinalizeActivationOwned(
	_ context.Context,
	id int64,
	expected []domain.ActivationStatus,
	_ string,
	_ int64,
) (domain.Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(expected) != 1 || expected[0] != s.currentStatus {
		return domain.Activation{}, storage.ErrConflict
	}
	s.finalizations = append(s.finalizations, append([]domain.ActivationStatus(nil), expected...))
	return domain.Activation{ID: id, Status: s.currentStatus}, nil
}

func (s *loginCodeTimeoutStore) ReleaseActivationLease(context.Context, int64, string, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	return nil
}

func newLoginCodeTimeoutManager(t *testing.T, store *loginCodeTimeoutStore, providerURL string) *Manager {
	t.Helper()
	box, err := secure.New("login-code-timeout-workflow-test")
	if err != nil {
		t.Fatal(err)
	}
	apiKeyCiphertext, err := box.Seal([]byte("fixture-api-key"))
	if err != nil {
		t.Fatal(err)
	}
	settingValue, err := json.Marshal(map[string]string{
		"api_key_encrypted": base64.StdEncoding.EncodeToString(apiKeyCiphertext),
	})
	if err != nil {
		t.Fatal(err)
	}
	store.setting = domain.Setting{Key: appsettings.SMSBowerKey, Value: settingValue}
	return New(
		store,
		appsettings.New(store, box, providerURL),
		box,
		Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func classifiedCodeTimeoutActivation(status domain.ActivationStatus, reason string) domain.Activation {
	return domain.Activation{
		ID:                   21,
		BatchID:              11,
		ProviderActivationID: "provider-activation-1",
		PhoneNumber:          "+6281234567890",
		Status:               status,
		FailureReason:        reason,
		LeaseOwner:           "worker-1",
		LeaseVersion:         3,
	}
}

func TestProcessActivationFinalizesClassifiedLoginCodeTimeout(t *testing.T) {
	var cancelCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "setStatus" || r.URL.Query().Get("status") != "8" {
			http.Error(w, "unexpected provider request", http.StatusBadRequest)
			return
		}
		cancelCalls.Add(1)
		_, _ = io.WriteString(w, "ACCESS_CANCEL")
	}))
	defer provider.Close()

	activation := classifiedCodeTimeoutActivation(
		domain.ActivationStatusLoginCodeTimeout,
		"登录验证码重发 3 次后仍未收到",
	)
	store := &loginCodeTimeoutStore{currentStatus: activation.Status}
	manager := newLoginCodeTimeoutManager(t, store, provider.URL)
	manager.processActivation(context.Background(), activation)

	store.mu.Lock()
	transitions := append([]loginCodeTimeoutTransition(nil), store.transitions...)
	finalizations := append([][]domain.ActivationStatus(nil), store.finalizations...)
	store.mu.Unlock()
	if got := cancelCalls.Load(); got != 1 {
		t.Fatalf("provider cancellation calls = %d, want 1", got)
	}
	if len(transitions) != 0 {
		t.Fatalf("transitions = %+v, want no reclassification", transitions)
	}
	wantFinalizations := [][]domain.ActivationStatus{{domain.ActivationStatusLoginCodeTimeout}}
	if !reflect.DeepEqual(finalizations, wantFinalizations) {
		t.Fatalf("finalizations = %v, want %v", finalizations, wantFinalizations)
	}
}

func TestVerificationCodeWaitTimedOutUsesStrictConfiguredBoundary(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		wait         time.Duration
		sentAt       time.Time
		wantTimedOut bool
	}{
		{name: "login over 60 seconds", wait: loginVerificationCodeWait, sentAt: now.Add(-loginVerificationCodeWait - time.Nanosecond), wantTimedOut: true},
		{name: "login exactly 60 seconds", wait: loginVerificationCodeWait, sentAt: now.Add(-loginVerificationCodeWait)},
		{name: "PIN over 80 seconds", wait: pinVerificationCodeWait, sentAt: now.Add(-pinVerificationCodeWait - time.Nanosecond), wantTimedOut: true},
		{name: "PIN exactly 80 seconds", wait: pinVerificationCodeWait, sentAt: now.Add(-pinVerificationCodeWait)},
		{name: "PIN under 80 seconds", wait: pinVerificationCodeWait, sentAt: now.Add(-pinVerificationCodeWait + time.Nanosecond)},
		{name: "zero persisted time", wait: pinVerificationCodeWait},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := verificationCodeWaitTimedOut(test.sentAt, now, test.wait); got != test.wantTimedOut {
				t.Fatalf("verificationCodeWaitTimedOut() = %v, want %v", got, test.wantTimedOut)
			}
		})
	}
}

func TestActivationStepFailureReasonPreservesVerificationCodeTimeoutClassification(t *testing.T) {
	for _, test := range []struct {
		name   string
		status domain.ActivationStatus
		reason string
	}{
		{name: "login", status: domain.ActivationStatusLoginCodeTimeout, reason: "登录验证码重发 3 次后仍未收到"},
		{name: "pin", status: domain.ActivationStatusPINCodeTimeout, reason: "改 PIN 验证码重发 3 次后仍未收到"},
	} {
		t.Run(test.name, func(t *testing.T) {
			activation := domain.Activation{Status: test.status, FailureReason: test.reason}
			if reason := activationStepFailureReason(activation, errors.New("temporary provider failure")); reason != activation.FailureReason {
				t.Fatalf("reason = %q, want %q", reason, activation.FailureReason)
			}
		})
	}
}

func TestProcessActivationRetriesCancellationAfterTimeoutClassification(t *testing.T) {
	for _, test := range []struct {
		name   string
		status domain.ActivationStatus
		reason string
	}{
		{name: "login", status: domain.ActivationStatusLoginCodeTimeout, reason: "登录验证码重发 3 次后仍未收到"},
		{name: "pin", status: domain.ActivationStatusPINCodeTimeout, reason: "改 PIN 验证码重发 3 次后仍未收到"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cancelCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("action") != "setStatus" || r.URL.Query().Get("status") != "8" {
					http.Error(w, "unexpected provider request", http.StatusBadRequest)
					return
				}
				if cancelCalls.Add(1) == 1 {
					http.Error(w, "temporary provider failure", http.StatusBadGateway)
					return
				}
				_, _ = io.WriteString(w, "ACCESS_CANCEL")
			}))
			defer provider.Close()

			activation := classifiedCodeTimeoutActivation(test.status, test.reason)
			store := &loginCodeTimeoutStore{currentStatus: activation.Status}
			manager := newLoginCodeTimeoutManager(t, store, provider.URL)
			manager.processActivation(context.Background(), activation)

			store.mu.Lock()
			statusAfterFailure := store.currentStatus
			finalizationsAfterFailure := len(store.finalizations)
			store.mu.Unlock()
			if statusAfterFailure != test.status || finalizationsAfterFailure != 0 {
				t.Fatalf("after failed cancellation status=%q finalizations=%d", statusAfterFailure, finalizationsAfterFailure)
			}

			activation.LeaseOwner = "worker-2"
			activation.LeaseVersion++
			manager.processActivation(context.Background(), activation)

			store.mu.Lock()
			transitions := append([]loginCodeTimeoutTransition(nil), store.transitions...)
			finalizations := append([][]domain.ActivationStatus(nil), store.finalizations...)
			store.mu.Unlock()
			if got := cancelCalls.Load(); got != 2 {
				t.Fatalf("provider cancellation calls = %d, want 2", got)
			}
			if len(transitions) != 1 || transitions[0].next != test.status || transitions[0].reason != test.reason {
				t.Fatalf("timeout classification was not preserved across retry: %+v", transitions)
			}
			wantFinalizations := [][]domain.ActivationStatus{{test.status}}
			if !reflect.DeepEqual(finalizations, wantFinalizations) {
				t.Fatalf("finalizations = %v, want %v", finalizations, wantFinalizations)
			}
		})
	}
}
