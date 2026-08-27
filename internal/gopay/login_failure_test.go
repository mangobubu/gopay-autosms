package gopay

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestClassifyLoginErrorRecognizesTerminalLoginFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "temporarily blocked account",
			status: http.StatusForbidden,
			body:   `{"success":false,"errors":[{"code":"GoPay-112","message":"Kami melihat ada aktivitas yang tidak wajar di akunmu.","message_title":"Akunmu diblokir sementara"}]}`,
		},
		{
			name:   "authentication rate limited",
			status: http.StatusTooManyRequests,
			body:   `{"success":false,"errors":[{"code":"auth:error:ratelimited"}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			httpErr := &HTTPError{StatusCode: test.status, Body: body}
			err := classifyLoginError(apiResponse{status: test.status, body: body}, httpErr)
			if !errors.Is(err, ErrLoginFailed) {
				t.Fatalf("err=%v, want ErrLoginFailed", err)
			}
			var restored *HTTPError
			if !errors.As(err, &restored) || restored != httpErr {
				t.Fatalf("err=%v does not preserve original *HTTPError", err)
			}
		})
	}
}

func TestClassifyLoginErrorKeepsUnrelated403And429Retryable(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "generic forbidden",
			status: http.StatusForbidden,
			body:   `{"success":false,"errors":[{"code":"GoPay-113"}]}`,
		},
		{
			name:   "generic rate limit",
			status: http.StatusTooManyRequests,
			body:   `{"success":false,"errors":[{"code":"rate_limited"}]}`,
		},
		{
			name:   "GoPay-112 with wrong status",
			status: http.StatusTooManyRequests,
			body:   `{"success":false,"errors":[{"code":"GoPay-112"}]}`,
		},
		{
			name:   "authentication rate limit with wrong status",
			status: http.StatusForbidden,
			body:   `{"success":false,"errors":[{"code":"auth:error:ratelimited"}]}`,
		},
		{
			name:   "GoPay-112 text outside code field",
			status: http.StatusForbidden,
			body:   `{"success":false,"errors":[{"code":"GoPay-113","message":"upstream mentioned GoPay-112"}]}`,
		},
		{
			name:   "authentication rate limit text outside code field",
			status: http.StatusTooManyRequests,
			body:   `{"success":false,"errors":[{"code":"temporary","message":"auth:error:ratelimited"}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			httpErr := &HTTPError{StatusCode: test.status, Body: body}
			err := classifyLoginError(apiResponse{status: test.status, body: body}, httpErr)
			if errors.Is(err, ErrLoginFailed) {
				t.Fatalf("err=%v unexpectedly classified as ErrLoginFailed", err)
			}
			if err != httpErr {
				t.Fatalf("err=%v, want original retryable HTTP error", err)
			}
		})
	}
}

func TestIssueTokenDoesNotTreatGoPay112AsTwoFAChallenge(t *testing.T) {
	body := `{"success":false,"errors":[{"code":"GoPay-112","message_title":"Akunmu diblokir sementara"}]}`
	client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/goto-auth/token" {
			return jsonResponse(http.StatusNotFound, `{}`), nil
		}
		return jsonResponse(http.StatusForbidden, body), nil
	}), "access-1", "refresh-1")

	authenticated, err := client.issueTokenLocked(context.Background(), "cvs", "one-fa-token")
	if authenticated || !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("authenticated=%v err=%v, want terminal login failure", authenticated, err)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusForbidden {
		t.Fatalf("err=%v does not preserve the 403 response", err)
	}
	if state := client.State(); state.TwoFAToken != "" || len(state.TwoFAMethods) != 0 {
		t.Fatalf("GoPay-112 was stored as a 2FA challenge: %+v", state)
	}
}

func TestCheckLoginStatusRecognizesKnownLoginFailuresWithoutRefresh(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "GoPay-112",
			status: http.StatusForbidden,
			body:   `{"success":false,"errors":[{"code":"GoPay-112"}]}`,
		},
		{
			name:   "auth error rate limited",
			status: http.StatusTooManyRequests,
			body:   `{"success":false,"errors":[{"code":"auth:error:ratelimited"}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var refreshCalls int
			client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/goto-auth/token" {
					refreshCalls++
					return jsonResponse(http.StatusOK, `{"data":{"access_token":"access-2"}}`), nil
				}
				return jsonResponse(test.status, test.body), nil
			}), "access-1", "refresh-1")

			result, err := client.CheckLoginStatus(context.Background())
			if result.Status != LoginStatusInvalid || result.HTTPStatus != test.status || result.Refreshed {
				t.Fatalf("result=%+v, want invalid HTTP %d without refresh", result, test.status)
			}
			if !errors.Is(err, ErrLoginFailed) {
				t.Fatalf("err=%v, want ErrLoginFailed", err)
			}
			if refreshCalls != 0 {
				t.Fatalf("refresh calls=%d, want 0", refreshCalls)
			}
		})
	}
}
