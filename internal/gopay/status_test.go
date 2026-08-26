package gopay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newStatusFixtureClient(t *testing.T, transport http.RoundTripper, access, refresh string) *Client {
	t.Helper()
	client, err := NewClient(Config{
		SSOBaseURL:   "https://accounts.fixture.invalid",
		GoPayBaseURL: "https://customer.fixture.invalid",
		HTTPClient:   &http.Client{Transport: transport},
		Session: &Session{
			Phone:        "81234567890",
			AccountID:    "account-1",
			AccessToken:  access,
			RefreshToken: refresh,
			Device:       GenerateDeviceIdentity("81234567890"),
		},
		NonceReader: bytes.NewReader(make([]byte, 4096)),
		IDReader:    bytes.NewReader(make([]byte, 4096)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestCheckLoginStatusProfile200(t *testing.T) {
	var paths []string
	client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/v1/users/profile" || r.Method != http.MethodGet {
			return jsonResponse(http.StatusNotFound, `{}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer access-1" || r.Header.Get("User-uuid") != "account-1" {
			t.Fatalf("profile request did not restore account authentication headers")
		}
		for _, name := range []string{"X-M1", "X-E1", "X-E2", "X-UniqueId", "X-DeviceOS"} {
			if strings.TrimSpace(r.Header.Get(name)) == "" {
				t.Fatalf("profile request is missing signed device header %s", name)
			}
		}
		return jsonResponse(http.StatusOK, `{"data":{"id":"account-1"}}`), nil
	}), "access-1", "refresh-1")

	result, err := client.CheckLoginStatus(context.Background())
	if err != nil || result.Status != LoginStatusValid || !result.Valid() || result.Refreshed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(paths) != 1 || paths[0] != "/v1/users/profile" {
		t.Fatalf("paths=%v", paths)
	}
}

func TestCheckLoginStatusRefreshesAfter401(t *testing.T) {
	var profileCalls, refreshCalls int
	client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/users/profile":
			profileCalls++
			if profileCalls == 1 {
				return jsonResponse(http.StatusUnauthorized, `{"error":"expired"}`), nil
			}
			if got := r.Header.Get("Authorization"); got != "Bearer access-2" {
				return jsonResponse(http.StatusBadRequest, fmt.Sprintf(`{"error":"wrong auth %s"}`, got)), nil
			}
			return jsonResponse(http.StatusOK, `{"data":{}}`), nil
		case "/goto-auth/token":
			refreshCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["grant_type"] != "refresh_token" || body["token"] != "refresh-1" {
				return jsonResponse(http.StatusBadRequest, `{"error":"wrong grant"}`), nil
			}
			return jsonResponse(http.StatusOK, `{"data":{"access_token":"access-2","refresh_token":"refresh-2"}}`), nil
		default:
			return jsonResponse(http.StatusNotFound, `{}`), nil
		}
	}), "access-1", "refresh-1")

	result, err := client.CheckLoginStatus(context.Background())
	if err != nil || result.Status != LoginStatusValid || !result.Refreshed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if profileCalls != 2 || refreshCalls != 1 {
		t.Fatalf("profileCalls=%d refreshCalls=%d", profileCalls, refreshCalls)
	}
	state := client.State()
	if state.AccessToken != "access-2" || state.RefreshToken != "refresh-2" {
		t.Fatalf("rotated state=%+v", state)
	}
}

func TestCheckLoginStatusExpiredOnlyAfterRefreshRejected(t *testing.T) {
	client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/users/profile" {
			return jsonResponse(http.StatusUnauthorized, `{"error":"expired"}`), nil
		}
		if r.URL.Path == "/goto-auth/token" {
			return jsonResponse(http.StatusUnauthorized, `{"error":"invalid_grant"}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}), "access-1", "refresh-1")

	result, err := client.CheckLoginStatus(context.Background())
	if result.Status != LoginStatusInvalid || !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCheckLoginStatusKeepsTransientFailureUnknown(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(status, `{"error":"temporary"}`), nil
			}), "access-1", "refresh-1")
			result, err := client.CheckLoginStatus(context.Background())
			if result.Status != LoginStatusUnknown || err == nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestCheckLoginStatusRequiresProfileHTTP200(t *testing.T) {
	client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusNoContent, ""), nil
	}), "access-1", "refresh-1")

	result, err := client.CheckLoginStatus(context.Background())
	if result.Status != LoginStatusUnknown || result.HTTPStatus != http.StatusNoContent || err == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCheckLoginStatusDoesNotTreatAuthenticationOutageAsExpired(t *testing.T) {
	var refreshCalls int
	client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/goto-auth/token" {
			refreshCalls++
		}
		return jsonResponse(http.StatusForbidden, `{"error":"authentication service unavailable"}`), nil
	}), "access-1", "refresh-1")

	result, err := client.CheckLoginStatus(context.Background())
	if result.Status != LoginStatusUnknown || err == nil || refreshCalls != 0 {
		t.Fatalf("result=%+v err=%v refreshCalls=%d", result, err, refreshCalls)
	}
}

func TestCheckLoginStatusNeverRefreshesOrExpiresOn403(t *testing.T) {
	var refreshCalls int
	client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/users/profile":
			return jsonResponse(http.StatusForbidden, `{"error":"unauthorized","detail":"token_expired"}`), nil
		case "/goto-auth/token":
			refreshCalls++
			return jsonResponse(http.StatusOK, `{"data":{"access_token":"access-2","refresh_token":"refresh-2"}}`), nil
		default:
			return jsonResponse(http.StatusNotFound, `{}`), nil
		}
	}), "access-1", "refresh-1")

	result, err := client.CheckLoginStatus(context.Background())
	if err == nil || result.Status != LoginStatusUnknown || result.Refreshed || refreshCalls != 0 {
		t.Fatalf("result=%+v err=%v refreshCalls=%d", result, err, refreshCalls)
	}
}

func TestCheckLoginStatusKeepsAmbiguousRefreshRejectionUnknown(t *testing.T) {
	client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/users/profile" {
			return jsonResponse(http.StatusUnauthorized, `{"error":"token_expired"}`), nil
		}
		return jsonResponse(http.StatusUnauthorized, `{"error":"invalid_client"}`), nil
	}), "access-1", "refresh-1")

	result, err := client.CheckLoginStatus(context.Background())
	if result.Status != LoginStatusUnknown || errors.Is(err, ErrLoginExpired) || err == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCheckLoginStatusBootstrapsRefreshOnlyOnce(t *testing.T) {
	var refreshCalls int
	client := newStatusFixtureClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/goto-auth/token" {
			refreshCalls++
			return jsonResponse(http.StatusOK, `{"data":{"access_token":"access-2","refresh_token":"refresh-2"}}`), nil
		}
		return jsonResponse(http.StatusUnauthorized, `{"error":"token_expired"}`), nil
	}), "", "refresh-1")

	result, err := client.CheckLoginStatus(context.Background())
	if result.Status != LoginStatusInvalid || !result.Refreshed || !errors.Is(err, ErrLoginExpired) || refreshCalls != 1 {
		t.Fatalf("result=%+v err=%v refreshCalls=%d", result, err, refreshCalls)
	}
}

func TestRefreshRejectedUsesExactStructuredCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{name: "invalid grant code", body: `{"error":"invalid_grant"}`, want: true},
		{name: "invalid token code", body: `{"error":{"code":"invalid_token"}}`, want: true},
		{name: "expired message", body: `{"message":"refresh token is expired"}`, want: true},
		{name: "invalid grant type", body: `{"error":"invalid grant type"}`, want: false},
		{name: "invalid client", body: `{"error":"invalid_client"}`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := refreshRejected(&HTTPError{StatusCode: http.StatusUnauthorized, Body: []byte(test.body)})
			if got != test.want {
				t.Fatalf("refreshRejected(%s)=%v want %v", test.body, got, test.want)
			}
		})
	}
}
