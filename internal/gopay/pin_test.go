package gopay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func newPINFixtureClient(t *testing.T, server *httptest.Server, state Session) *Client {
	t.Helper()
	client, err := NewClient(Config{
		SSOBaseURL:   server.URL,
		GoPayBaseURL: server.URL,
		HTTPClient:   server.Client(),
		Session:      &state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestVerifyPINSetupOTPPersistedSessionDoesNotReplay(t *testing.T) {
	var verifyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/cvs/v1/verify" {
			http.NotFound(w, r)
			return
		}
		verifyCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"verification_token":"setup-verification-token"}}`)
	}))
	defer server.Close()

	state := Session{
		Phone:             "81234567890",
		Device:            GenerateDeviceIdentity("81234567890"),
		PINVerificationID: "setup-verification-id",
		PINOTPToken:       "setup-otp-token",
		PINStage:          PINStageAwaiting,
	}
	client := newPINFixtureClient(t, server, state)
	if err := client.VerifyPINSetupOTP(context.Background(), "9949"); err != nil {
		t.Fatal(err)
	}
	verified := client.State()
	if verified.PINVerificationToken != "setup-verification-token" || verified.PINStage != PINStageSetupVerified {
		t.Fatalf("verified state=%+v", verified)
	}

	persisted, err := verified.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ParseSession(persisted)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newPINFixtureClient(t, server, restored)
	if err := restarted.VerifyPINSetupOTP(context.Background(), "9949"); err != nil {
		t.Fatal(err)
	}
	if got := verifyCalls.Load(); got != 1 {
		t.Fatalf("/cvs/v1/verify calls=%d, want 1", got)
	}
	if got := restarted.State().PINStage; got != PINStageSetupVerified {
		t.Fatalf("restored PIN stage=%q, want %q", got, PINStageSetupVerified)
	}
}

func TestVerifyPINResetOTPPersistedSessionDoesNotReplay(t *testing.T) {
	var verifyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/cvs/v1/verify" {
			http.NotFound(w, r)
			return
		}
		verifyCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"verification_token":"reset-verification-token"}}`)
	}))
	defer server.Close()

	state := Session{
		Phone:             "81234567890",
		Device:            GenerateDeviceIdentity("81234567890"),
		PINVerificationID: "reset-verification-id",
		PINOTPToken:       "reset-otp-token",
		PINChallengeID:    "reset-challenge-id",
		PINClientID:       "reset-client-id",
		PINStage:          PINStageResetAwaiting,
	}
	client := newPINFixtureClient(t, server, state)
	if err := client.VerifyPINResetOTP(context.Background(), "9949"); err != nil {
		t.Fatal(err)
	}
	verified := client.State()
	if verified.PINVerificationToken != "reset-verification-token" || verified.PINStage != PINStageResetVerified {
		t.Fatalf("verified state=%+v", verified)
	}

	persisted, err := verified.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ParseSession(persisted)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newPINFixtureClient(t, server, restored)
	if err := restarted.VerifyPINResetOTP(context.Background(), "9949"); err != nil {
		t.Fatal(err)
	}
	if got := verifyCalls.Load(); got != 1 {
		t.Fatalf("/cvs/v1/verify calls=%d, want 1", got)
	}
	if got := restarted.State().PINStage; got != PINStageResetVerified {
		t.Fatalf("restored PIN stage=%q, want %q", got, PINStageResetVerified)
	}
}

func TestClassifyPINAlreadySetPreservesHTTPError(t *testing.T) {
	httpErr := &HTTPError{
		StatusCode: 466,
		Body:       []byte(`{"errors":[{"code":"GoPay-111","message":"PIN kamu sudah aktif"}]}`),
	}
	err := classifyPINError(httpErr)
	if !errors.Is(err, ErrPINAlreadySet) {
		t.Fatalf("err=%v, want ErrPINAlreadySet", err)
	}
	var restored *HTTPError
	if !errors.As(err, &restored) {
		t.Fatalf("err=%v does not preserve *HTTPError", err)
	}
	if restored != httpErr || restored.StatusCode != 466 {
		t.Fatalf("restored HTTP error=%+v, want original %+v", restored, httpErr)
	}
}

func TestClassifyPINVerificationIDInvalid(t *testing.T) {
	httpErr := &HTTPError{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       []byte(`{"errors":[{"code":"verification_id_invalid"}]}`),
	}
	err := classifyPINError(httpErr)
	if !errors.Is(err, ErrPINVerificationExpired) {
		t.Fatalf("err=%v, want ErrPINVerificationExpired", err)
	}
	var restored *HTTPError
	if !errors.As(err, &restored) || restored != httpErr {
		t.Fatalf("err=%v does not preserve original *HTTPError", err)
	}
}

func TestClassifyPINExplicitLoginFailurePreservesHTTPError(t *testing.T) {
	httpErr := &HTTPError{
		StatusCode: http.StatusForbidden,
		Body:       []byte(`{"errors":[{"code":"GoPay-112"}]}`),
	}
	err := classifyPINError(httpErr)
	if !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("err=%v, want ErrLoginFailed", err)
	}
	var restored *HTTPError
	if !errors.As(err, &restored) || restored != httpErr {
		t.Fatalf("err=%v does not preserve original *HTTPError", err)
	}
}

func TestGetPINStatusClassifiesExplicitLoginFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"errors":[{"code":"GoPay-112"}]}`)
	}))
	defer server.Close()
	client := newPINFixtureClient(t, server, Session{
		Phone: "81234567890", AccessToken: "access-token", Device: GenerateDeviceIdentity("81234567890"),
	})
	_, err := client.GetPINStatus(context.Background())
	if !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("GetPINStatus() error=%v, want ErrLoginFailed", err)
	}
}

func TestCompletePINResetRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/api/v2/users/pins/reset/tokens" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			http.Error(w, "wrong authorization", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Verification-Token"); got != "Bearer reset-verification-token" {
			http.Error(w, "wrong verification token", http.StatusBadRequest)
			return
		}
		var body struct {
			PIN         string `json:"pin"`
			ClientID    string `json:"client_id"`
			ChallengeID string `json:"challenge_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		want := struct {
			PIN         string `json:"pin"`
			ClientID    string `json:"client_id"`
			ChallengeID string `json:"challenge_id"`
		}{PIN: "123456", ClientID: "reset-client-id", ChallengeID: "reset-challenge-id"}
		if body != want {
			http.Error(w, fmt.Sprintf("wrong body: %+v", body), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"token":"reset-pin-token"}}`)
	}))
	defer server.Close()

	state := Session{
		Phone:                "81234567890",
		Device:               GenerateDeviceIdentity("81234567890"),
		AccountID:            "account-id",
		AccessToken:          "access-token",
		PINVerificationToken: "reset-verification-token",
		PINChallengeID:       "reset-challenge-id",
		PINClientID:          "reset-client-id",
		PINStage:             PINStageResetVerified,
	}
	client := newPINFixtureClient(t, server, state)
	if err := client.CompletePINReset(context.Background(), "123456"); err != nil {
		t.Fatal(err)
	}
	got := client.State()
	if got.PINToken != "reset-pin-token" || got.PINStage != PINStageComplete {
		t.Fatalf("completed state=%+v", got)
	}
}

func TestPINNotAllowedStopsSetupAndReset(t *testing.T) {
	for _, test := range []struct {
		name  string
		start func(*Client) error
	}{
		{
			name: "setup",
			start: func(client *Client) error {
				_, err := client.StartPINSetup(context.Background(), "123456")
				return err
			},
		},
		{
			name: "reset",
			start: func(client *Client) error {
				_, err := client.StartPINReset(context.Background(), "123456")
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/users/pins/allowed" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":{"allowed":false}}`)
			}))
			defer server.Close()

			state := Session{
				Phone:       "81234567890",
				Device:      GenerateDeviceIdentity("81234567890"),
				AccessToken: "access-token",
			}
			client := newPINFixtureClient(t, server, state)
			if err := test.start(client); !errors.Is(err, ErrPINNotAllowed) {
				t.Fatalf("err=%v, want ErrPINNotAllowed", err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("request calls=%d, want only allowed check", got)
			}
		})
	}
}

func TestPINSessionIntermediateStagesMarshalAndRestore(t *testing.T) {
	stages := []PINStage{
		PINStageIdle,
		PINStageReadyCycle,
		PINStageCycleReady,
		PINStageAwaiting,
		PINStageSetupVerified,
		PINStageResetReadyCycle,
		PINStageResetCycleReady,
		PINStageResetAwaiting,
		PINStageResetVerified,
		PINStageComplete,
	}
	for index, stage := range stages {
		name := string(stage)
		if name == "" {
			name = "idle"
		}
		t.Run(name, func(t *testing.T) {
			want := Session{
				Phone:                "81234567890",
				Device:               GenerateDeviceIdentity("81234567890"),
				PINVerificationID:    fmt.Sprintf("verification-%d", index),
				PINOTPToken:          fmt.Sprintf("otp-%d", index),
				PINVerificationToken: fmt.Sprintf("verification-token-%d", index),
				PINChallengeID:       fmt.Sprintf("challenge-%d", index),
				PINClientID:          fmt.Sprintf("client-%d", index),
				PINToken:             fmt.Sprintf("pin-token-%d", index),
				PINStage:             stage,
			}
			data, err := want.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseSession(data)
			if err != nil {
				t.Fatal(err)
			}

			client, err := NewClient(Config{
				SSOBaseURL:   "https://accounts.fixture.invalid",
				GoPayBaseURL: "https://customer.fixture.invalid",
			})
			if err != nil {
				t.Fatal(err)
			}
			client.Restore(parsed)
			got := client.State()
			if got.PINStage != want.PINStage {
				t.Fatalf("PINStage=%q, want %q", got.PINStage, want.PINStage)
			}
			gotFields := []string{
				got.PINVerificationID,
				got.PINOTPToken,
				got.PINVerificationToken,
				got.PINChallengeID,
				got.PINClientID,
				got.PINToken,
			}
			wantFields := []string{
				want.PINVerificationID,
				want.PINOTPToken,
				want.PINVerificationToken,
				want.PINChallengeID,
				want.PINClientID,
				want.PINToken,
			}
			if !reflect.DeepEqual(gotFields, wantFields) {
				t.Fatalf("restored PIN fields=%v, want %v", gotFields, wantFields)
			}
		})
	}
}
