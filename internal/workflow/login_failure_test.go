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
		name   string
		status int
		body   string
	}{
		{
			name:   "GoPay-112",
			status: http.StatusForbidden,
			body:   `{"success":false,"errors":[{"code":"GoPay-112","message_title":"Akunmu diblokir sementara"}]}`,
		},
		{
			name:   "auth error rate limited",
			status: http.StatusTooManyRequests,
			body:   `{"success":false,"errors":[{"code":"auth:error:ratelimited"}]}`,
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

			store := &loginFailureStore{
				batch: domain.Batch{
					ID: 11, CountryCode: "6", TargetPINEnc: pinCiphertext, Config: json.RawMessage(`{}`),
				},
				setting: domain.Setting{Key: appsettings.SMSBowerKey, Value: settingValue},
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
			wantTransitions := []loginFailureTransition{{next: domain.ActivationStatusLoginFailed, reason: "登录失败"}}
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
