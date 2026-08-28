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
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/smsprovider"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type loginFailureTransition struct {
	next   domain.ActivationStatus
	reason string
}

type loginFailureStore struct {
	storage.Store

	mu            sync.Mutex
	batch         domain.Batch
	setting       domain.Setting
	transitions   []loginFailureTransition
	finalizations [][]domain.ActivationStatus
	releases      int
}

func loginFailureSMSSetting(t *testing.T, box *secure.Box) domain.Setting {
	return loginFailureSMSProviderSetting(t, box, appsettings.SMSBowerKey)
}

func loginFailureSMSProviderSetting(t *testing.T, box *secure.Box, key string) domain.Setting {
	t.Helper()
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
	return domain.Setting{Key: key, Value: settingValue}
}

func (s *loginFailureStore) ReleaseActivationLease(context.Context, int64, string, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	return nil
}

func (s *loginFailureStore) GetBatch(_ context.Context, id int64) (domain.Batch, error) {
	if id != s.batch.ID {
		return domain.Batch{}, storage.ErrNotFound
	}
	return s.batch, nil
}

func (s *loginFailureStore) ListActivations(context.Context, storage.ActivationFilter) ([]domain.Activation, error) {
	return nil, nil
}

func (s *loginFailureStore) GetActivation(context.Context, int64) (domain.Activation, error) {
	// The worker has already persisted and finalized the classification by the
	// time it performs this optional cleanup lookup. No account/proxy fixture is
	// needed for this test.
	return domain.Activation{}, storage.ErrNotFound
}

func (s *loginFailureStore) GetAccountByPhone(context.Context, string) (domain.Account, error) {
	return domain.Account{}, storage.ErrNotFound
}

func (s *loginFailureStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	if key != s.setting.Key {
		return domain.Setting{}, storage.ErrNotFound
	}
	return s.setting, nil
}

func (s *loginFailureStore) TransitionActivationOwned(
	_ context.Context,
	id int64,
	_ []domain.ActivationStatus,
	next domain.ActivationStatus,
	reason string,
	_ string,
	_ int64,
) (domain.Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transitions = append(s.transitions, loginFailureTransition{next: next, reason: reason})
	return domain.Activation{ID: id, Status: next}, nil
}

func (s *loginFailureStore) FinalizeActivationOwned(
	_ context.Context,
	id int64,
	expected []domain.ActivationStatus,
	_ string,
	_ int64,
) (domain.Activation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalizations = append(s.finalizations, append([]domain.ActivationStatus(nil), expected...))
	return domain.Activation{ID: id, Status: expected[0]}, nil
}

func TestProcessActivationFinalizesKnownLoginFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body       string
		wantReason string
	}{
		{
			name:       "GoPay-112",
			status:     http.StatusForbidden,
			body:       `{"success":false,"errors":[{"code":"GoPay-112","message_title":"Akunmu diblokir sementara"}]}`,
			wantReason: "GoPay 登录失败（阶段：登录初始化；HTTP 403；错误码：GoPay-112；GoPay 信息：Akunmu diblokir sementara）",
		},
		{
			name:       "auth error rate limited",
			status:     http.StatusTooManyRequests,
			body:       `{"success":false,"errors":[{"code":"auth:error:ratelimited"}]}`,
			wantReason: "GoPay 登录失败（阶段：登录初始化；HTTP 429；错误码：auth:error:ratelimited；说明：GoPay 登录请求频率受限）",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gopayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/goto-auth/login/methods" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer gopayServer.Close()

			var cancelCalls int
			smsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("action") != "setStatus" || r.URL.Query().Get("status") != "8" {
					http.Error(w, fmt.Sprintf("unexpected SMSBower request: %s?%s", r.URL.Path, r.URL.RawQuery), http.StatusBadRequest)
					return
				}
				cancelCalls++
				_, _ = io.WriteString(w, "ACCESS_CANCEL")
			}))
			defer smsServer.Close()

			box, err := secure.New("login-failure-workflow-test")
			if err != nil {
				t.Fatal(err)
			}
			pinCiphertext, err := box.Seal([]byte("123456"))
			if err != nil {
				t.Fatal(err)
			}
			store := &loginFailureStore{
				batch: domain.Batch{
					ID: 11, CountryCode: "6", TargetPINEnc: pinCiphertext, Config: json.RawMessage(`{}`),
				},
				setting: loginFailureSMSSetting(t, box),
			}
			manager := New(
				store,
				appsettings.New(store, box, smsServer.URL),
				box,
				Config{SSOBaseURL: gopayServer.URL, GoPayBaseURL: gopayServer.URL},
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)
			activation := domain.Activation{
				ID: 21, BatchID: store.batch.ID, ProviderActivationID: "provider-activation-1",
				PhoneNumber: "+6281234567890", Status: domain.ActivationStatusPurchased,
				LeaseOwner: "worker-1", LeaseVersion: 3,
			}

			manager.processActivation(context.Background(), activation)
			store.mu.Lock()
			transitions := append([]loginFailureTransition(nil), store.transitions...)
			finalizations := append([][]domain.ActivationStatus(nil), store.finalizations...)
			store.mu.Unlock()
			if cancelCalls != 1 {
				store.mu.Lock()
				gotTransitions := append([]loginFailureTransition(nil), store.transitions...)
				gotFinalizations := append([][]domain.ActivationStatus(nil), store.finalizations...)
				gotReleases := store.releases
				store.mu.Unlock()
				t.Fatalf("SMSBower cancel calls=%d, want 1; transitions=%+v finalizations=%v releases=%d", cancelCalls, gotTransitions, gotFinalizations, gotReleases)
			}
			wantTransitions := []loginFailureTransition{{next: domain.ActivationStatusLoginFailed, reason: test.wantReason}}
			if !reflect.DeepEqual(transitions, wantTransitions) {
				t.Fatalf("transitions=%+v, want %+v", transitions, wantTransitions)
			}
			wantFinalizations := [][]domain.ActivationStatus{{domain.ActivationStatusLoginFailed}}
			if !reflect.DeepEqual(finalizations, wantFinalizations) {
				t.Fatalf("finalizations=%v, want %v", finalizations, wantFinalizations)
			}
		})
	}
}

func TestProcessActivationPreservesLoginFailureDetailAcrossProviderRetry(t *testing.T) {
	var cancelCalls int
	smsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "setStatus" || r.URL.Query().Get("status") != "8" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		cancelCalls++
		if cancelCalls == 1 {
			http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ACCESS_CANCEL")
	}))
	defer smsServer.Close()

	box, err := secure.New("login-failure-provider-retry-test")
	if err != nil {
		t.Fatal(err)
	}
	store := &loginFailureStore{setting: loginFailureSMSSetting(t, box)}
	manager := New(
		store,
		appsettings.New(store, box, smsServer.URL),
		box,
		Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	const originalReason = "GoPay 操作失败（阶段：改 PIN 验证；HTTP 403；错误码：GoPay-112）"
	activation := domain.Activation{
		ID: 31, ProviderActivationID: "provider-activation-retry",
		PhoneNumber: "+6281234567890", Status: domain.ActivationStatusLoginFailed,
		FailureReason: originalReason, LeaseOwner: "worker-retry", LeaseVersion: 5,
	}

	manager.processActivation(context.Background(), activation)
	manager.processActivation(context.Background(), activation)

	store.mu.Lock()
	transitions := append([]loginFailureTransition(nil), store.transitions...)
	finalizations := append([][]domain.ActivationStatus(nil), store.finalizations...)
	releases := store.releases
	store.mu.Unlock()
	if cancelCalls != 2 {
		t.Fatalf("cancel calls=%d, want 2", cancelCalls)
	}
	wantTransitions := []loginFailureTransition{{next: domain.ActivationStatusLoginFailed, reason: originalReason}}
	if !reflect.DeepEqual(transitions, wantTransitions) {
		t.Fatalf("transitions=%+v, want %+v", transitions, wantTransitions)
	}
	wantFinalizations := [][]domain.ActivationStatus{{domain.ActivationStatusLoginFailed}}
	if !reflect.DeepEqual(finalizations, wantFinalizations) {
		t.Fatalf("finalizations=%v, want %v", finalizations, wantFinalizations)
	}
	if releases != 1 {
		t.Fatalf("lease releases=%d, want 1 after the failed provider attempt", releases)
	}
}

func TestProcessActivationPreservesUnregisteredReasonAcrossHeroConflictRetry(t *testing.T) {
	var smsBowerCalls int
	smsBowerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		smsBowerCalls++
		_, _ = io.WriteString(w, "ACCESS_CANCEL")
	}))
	defer smsBowerServer.Close()

	var heroCalls int
	heroServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "setStatus" ||
			r.URL.Query().Get("status") != "8" ||
			r.URL.Query().Get("id") != "hero-activation-retry" {
			http.Error(w, "unexpected HeroSMS request", http.StatusBadRequest)
			return
		}
		heroCalls++
		if heroCalls == 1 {
			http.Error(w, "early cancellation conflict", http.StatusConflict)
			return
		}
		_, _ = io.WriteString(w, "ACCESS_CANCEL")
	}))
	defer heroServer.Close()

	box, err := secure.New("unregistered-hero-provider-retry-test")
	if err != nil {
		t.Fatal(err)
	}
	store := &loginFailureStore{
		setting: loginFailureSMSProviderSetting(t, box, appsettings.HeroSMSKey),
	}
	manager := New(
		store,
		appsettings.New(store, box, smsBowerServer.URL, heroServer.URL),
		box,
		Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	activation := domain.Activation{
		ID: 41, Provider: smsprovider.HeroSMS, ProviderActivationID: "hero-activation-retry",
		PhoneNumber: "+6281234567890", Status: domain.ActivationStatusUnregistered,
		FailureReason: "未注册", LeaseOwner: "worker-hero-retry", LeaseVersion: 7,
	}

	manager.processActivation(context.Background(), activation)
	manager.processActivation(context.Background(), activation)

	store.mu.Lock()
	transitions := append([]loginFailureTransition(nil), store.transitions...)
	finalizations := append([][]domain.ActivationStatus(nil), store.finalizations...)
	releases := store.releases
	store.mu.Unlock()
	if smsBowerCalls != 0 {
		t.Fatalf("SMSBower calls=%d, want 0 for hero-sms activation", smsBowerCalls)
	}
	if heroCalls != 2 {
		t.Fatalf("HeroSMS cancel calls=%d, want 2", heroCalls)
	}
	wantTransitions := []loginFailureTransition{{
		next: domain.ActivationStatusUnregistered, reason: "未注册",
	}}
	if !reflect.DeepEqual(transitions, wantTransitions) {
		t.Fatalf("transitions=%+v, want preserved classification %+v", transitions, wantTransitions)
	}
	wantFinalizations := [][]domain.ActivationStatus{{domain.ActivationStatusUnregistered}}
	if !reflect.DeepEqual(finalizations, wantFinalizations) {
		t.Fatalf("finalizations=%v, want %v", finalizations, wantFinalizations)
	}
	if releases != 1 {
		t.Fatalf("lease releases=%d, want 1 after the HTTP 409 attempt", releases)
	}
}

func TestProcessActivationRepairsLegacyUnregisteredHeroConflictBeforeSuccessfulRetry(t *testing.T) {
	var heroCalls int
	heroServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "setStatus" || r.URL.Query().Get("status") != "8" {
			http.Error(w, "unexpected HeroSMS request", http.StatusBadRequest)
			return
		}
		heroCalls++
		_, _ = io.WriteString(w, "ACCESS_CANCEL")
	}))
	defer heroServer.Close()

	box, err := secure.New("legacy-unregistered-hero-retry-test")
	if err != nil {
		t.Fatal(err)
	}
	store := &loginFailureStore{
		setting: loginFailureSMSProviderSetting(t, box, appsettings.HeroSMSKey),
	}
	manager := New(
		store,
		appsettings.New(store, box, "https://smsbower.invalid", heroServer.URL),
		box,
		Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	activation := domain.Activation{
		ID: 42, Provider: smsprovider.HeroSMS, ProviderActivationID: "legacy-hero-activation",
		PhoneNumber: "+6281234567890", Status: domain.ActivationStatusUnregistered,
		FailureReason: "smsbower setStatus: HTTP_409: Conflict",
		LeaseOwner:    "worker-legacy-hero-retry", LeaseVersion: 8,
	}

	manager.processActivation(context.Background(), activation)

	store.mu.Lock()
	transitions := append([]loginFailureTransition(nil), store.transitions...)
	finalizations := append([][]domain.ActivationStatus(nil), store.finalizations...)
	releases := store.releases
	store.mu.Unlock()
	if heroCalls != 1 {
		t.Fatalf("HeroSMS cancel calls=%d, want 1", heroCalls)
	}
	wantTransitions := []loginFailureTransition{{
		next: domain.ActivationStatusUnregistered, reason: "未注册",
	}}
	if !reflect.DeepEqual(transitions, wantTransitions) {
		t.Fatalf("transitions=%+v, want legacy reason repair %+v", transitions, wantTransitions)
	}
	wantFinalizations := [][]domain.ActivationStatus{{domain.ActivationStatusUnregistered}}
	if !reflect.DeepEqual(finalizations, wantFinalizations) {
		t.Fatalf("finalizations=%v, want %v", finalizations, wantFinalizations)
	}
	if releases != 0 {
		t.Fatalf("lease releases=%d, want 0 after successful finalization", releases)
	}
}
