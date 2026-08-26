package gopay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestOTPOnlyLoginWithSecondOTP(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	verifyCalls, tokenCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("X-E1") == "" || r.Header.Get("X-E2") != SignatureV2ID {
			http.Error(w, "missing signature", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/goto-auth/login/methods":
			fmt.Fprint(w, `{"data":{"verification_id":"vid-1","methods":["otp_sms"]}}`)
		case "/cvs/v1//initiate":
			if r.Header.Get("key") != "value" {
				http.Error(w, "missing compatibility header", http.StatusBadRequest)
				return
			}
			token := "otp-1"
			if tokenCalls > 0 {
				token = "otp-2"
			}
			fmt.Fprintf(w, `{"data":{"otp_token":%q,"otp_length":4}}`, token)
		case "/cvs/v1/verify":
			verifyCalls++
			fmt.Fprintf(w, `{"data":{"verification_token":"verify-%d"}}`, verifyCalls)
		case "/goto-auth/accountlist":
			if r.Header.Get("verification-token") != "Bearer verify-1" {
				http.Error(w, "wrong verification token", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"data":{"1fa_token":"one-fa","account_list":[{"account_id":"acct-1"}]}}`)
		case "/goto-auth/token":
			tokenCalls++
			if tokenCalls == 1 {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"data":{"2fa_token":"two-fa","methods":["otp_sms"],"verification_id":"vid-2"}}`)
				return
			}
			fmt.Fprint(w, `{"data":{"access_token":"access","refresh_token":"refresh"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClientForPhone("81234567890", Config{
		SSOBaseURL: server.URL, GoPayBaseURL: server.URL,
		HTTPClient: server.Client(), Now: func() time.Time { return time.UnixMilli(1700000000123) },
		NonceReader: bytes.NewReader(make([]byte, 4096)), IDReader: bytes.NewReader(make([]byte, 4096)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = client.ProbeLogin(ctx, "+62", "81234567890"); err != nil {
		t.Fatal(err)
	}
	challenge, err := client.StartLogin(ctx)
	if err != nil || challenge.Purpose != OTPPurposeLogin1FA || challenge.Token != "otp-1" {
		t.Fatalf("first challenge=%+v err=%v", challenge, err)
	}
	first, err := client.VerifyLoginOTP(ctx, "1111")
	if err != nil {
		t.Fatal(err)
	}
	if !first.NeedsOTP || first.Authenticated || first.Challenge != nil || first.Session.LoginStage != LoginStageReady2FA {
		t.Fatalf("first result = %+v", first)
	}
	state := client.State()
	state.LoginStage = LoginStageCycleReady2FA
	client.Restore(state)
	secondChallenge, err := client.StartNextLoginOTP(ctx)
	if err != nil || secondChallenge.Purpose != OTPPurposeLogin2FA || secondChallenge.Token != "otp-2" {
		t.Fatalf("second challenge=%+v err=%v", secondChallenge, err)
	}
	second, err := client.VerifyLoginOTP(ctx, "2222")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Authenticated || second.NeedsOTP || second.Session.AccessToken != "access" || second.Session.RefreshToken != "refresh" {
		t.Fatalf("second result = %+v", second)
	}
	wantPaths := []string{"/goto-auth/login/methods", "/cvs/v1//initiate", "/cvs/v1/verify", "/goto-auth/accountlist", "/goto-auth/token", "/cvs/v1//initiate", "/cvs/v1/verify", "/goto-auth/token"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("paths=%v", paths)
	}
}

func TestProbeClassifiesPINAndUnregistered(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"pin", 200, `{"data":{"verification_id":"vid","methods":["goto_pin","otp_sms"]}}`, ErrPINRequired},
		{"not-found", 401, `{"errors":[{"code":"user:not_found"}]}`, ErrUnregistered},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			client, err := NewClientForPhone("8123", Config{SSOBaseURL: server.URL, GoPayBaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ProbeLogin(context.Background(), "+62", "8123")
			if !errors.Is(err, test.want) {
				t.Fatalf("err=%v, want %v", err, test.want)
			}
		})
	}
}

func TestBalanceAndPINSetup(t *testing.T) {
	mode := "zero"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/payment-options/balances":
			if mode == "zero" {
				fmt.Fprint(w, `{"data":[{"balance":{"value":0,"currency":"IDR"}}]}`)
			} else {
				fmt.Fprint(w, `{"data":[{"balance":{}}]}`)
			}
		case "/api/v1/users/pins/allowed":
			fmt.Fprint(w, `{"data":{"allowed":true}}`)
		case "/cvs/v1/methods":
			fmt.Fprint(w, `{"data":{"verification_id":"pin-vid","methods":["otp_sms"]}}`)
		case "/cvs/v1//initiate":
			fmt.Fprint(w, `{"data":{"otp_token":"pin-otp","otp_length":4}}`)
		case "/cvs/v1/verify":
			fmt.Fprint(w, `{"data":{"verification_token":"pin-verify"}}`)
		case "/api/v2/users/pins/setup/tokens":
			if r.Header.Get("Verification-Token") != "Bearer pin-verify" {
				http.Error(w, "bad token", 400)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["pin"] != "123456" {
				http.Error(w, "bad pin", 400)
				return
			}
			fmt.Fprint(w, `{"data":{"token":"pin-token"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	state := Session{Phone: "8123", Device: GenerateDeviceIdentity("8123"), AccessToken: "access", AccountID: "acct", LoginStage: LoginStageAuthenticated}
	client, err := NewClient(Config{SSOBaseURL: server.URL, GoPayBaseURL: server.URL, HTTPClient: server.Client(), Session: &state})
	if err != nil {
		t.Fatal(err)
	}
	balance, err := client.GetBalance(context.Background())
	if err != nil || !balance.Known || balance.Amount != 0 {
		t.Fatalf("balance=%+v err=%v", balance, err)
	}
	mode = "unknown"
	unknown, err := client.GetBalance(context.Background())
	if !errors.Is(err, ErrBalanceUnknown) || unknown.Known {
		t.Fatalf("balance=%+v err=%v", unknown, err)
	}
	if _, err = client.StartPINSetup(context.Background(), "12345"); !errors.Is(err, ErrInvalidPIN) {
		t.Fatalf("invalid pin err=%v", err)
	}
	challenge, err := client.StartPINSetup(context.Background(), "123456")
	if err != nil || challenge.Token != "pin-otp" {
		t.Fatalf("challenge=%+v err=%v", challenge, err)
	}
	if err = client.FinishPINSetup(context.Background(), "123456", "4567"); err != nil {
		t.Fatal(err)
	}
	if client.State().PINToken != "pin-token" {
		t.Fatalf("PIN token not persisted")
	}
}
