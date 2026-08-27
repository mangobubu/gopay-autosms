package gopay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// LoginStatus is the result of an authenticated GoPay profile probe.  It is
// deliberately separate from the account's business lifecycle status: an
// account can be disabled for business reasons while its session is still
// valid.
type LoginStatus string

const (
	LoginStatusValid   LoginStatus = "valid"
	LoginStatusInvalid LoginStatus = "invalid"
	LoginStatusUnknown LoginStatus = "unknown"
)

// ErrLoginExpired indicates that GoPay rejected the access token and a refresh
// either was not possible or was also rejected.  Transport, rate-limit, and
// server errors are returned without this sentinel so callers do not show a
// transient outage as a logged-out account.
var ErrLoginExpired = errors.New("gopay: login session has expired")

// LoginStatusResult contains the safe, non-secret outcome of a profile probe.
// HTTPStatus is useful for diagnostics and never contains credential data.
type LoginStatusResult struct {
	Status     LoginStatus `json:"status"`
	Refreshed  bool        `json:"refreshed"`
	HTTPStatus int         `json:"http_status,omitempty"`
}

func (r LoginStatusResult) Valid() bool { return r.Status == LoginStatusValid }

// CheckLoginStatus probes the authenticated GoPay profile using the same
// signed native Go request used by the rest of the client.  A rejected access
// token causes one refresh-token exchange and one profile retry.  Only a
// confirmed authentication rejection is classified as invalid; network,
// throttling, proxy, and server failures remain unknown.
func (c *Client) CheckLoginStatus(ctx context.Context) (LoginStatusResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkLoginStatusLocked(ctx)
}

// ProbeLoginStatus is an alias that makes the intent explicit at call sites
// which already use the ProbeLogin login-method operation.
func (c *Client) ProbeLoginStatus(ctx context.Context) (LoginStatusResult, error) {
	return c.CheckLoginStatus(ctx)
}

func (c *Client) checkLoginStatusLocked(ctx context.Context) (LoginStatusResult, error) {
	refreshed := false
	// A session restored during an interrupted login may have no access token
	// yet.  If a refresh token exists, bootstrap it before probing the profile.
	if strings.TrimSpace(c.session.AccessToken) == "" {
		if strings.TrimSpace(c.session.RefreshToken) == "" {
			return LoginStatusResult{Status: LoginStatusUnknown}, fmt.Errorf("gopay: login session is not established")
		}
		if err := c.refreshLocked(ctx); err != nil {
			if loginFailureError(err) {
				return LoginStatusResult{Status: LoginStatusInvalid, HTTPStatus: httpStatus(err)}, fmt.Errorf("%w: %w", ErrLoginFailed, err)
			}
			if refreshRejected(err) {
				return LoginStatusResult{Status: LoginStatusInvalid, HTTPStatus: httpStatus(err)}, fmt.Errorf("%w: refresh token rejected", ErrLoginExpired)
			}
			return LoginStatusResult{Status: LoginStatusUnknown, HTTPStatus: httpStatus(err)}, fmt.Errorf("gopay: login status refresh: %w", err)
		}
		refreshed = true
	}

	response, err := c.gopayRequest(ctx, http.MethodGet, "/v1/users/profile", nil, nil)
	if err == nil && response.status == http.StatusOK {
		return LoginStatusResult{Status: LoginStatusValid, Refreshed: refreshed, HTTPStatus: response.status}, nil
	}
	if err == nil {
		return LoginStatusResult{Status: LoginStatusUnknown, Refreshed: refreshed, HTTPStatus: response.status}, fmt.Errorf("gopay: login status probe returned unexpected HTTP %d", response.status)
	}
	if loginFailureResponse(response.status, response.body) {
		return LoginStatusResult{Status: LoginStatusInvalid, Refreshed: refreshed, HTTPStatus: response.status}, fmt.Errorf("%w: %w", ErrLoginFailed, err)
	}
	if !loginAuthFailure(response) {
		return LoginStatusResult{Status: LoginStatusUnknown, Refreshed: refreshed, HTTPStatus: response.status}, fmt.Errorf("gopay: login status probe: %w", err)
	}
	if refreshed {
		return LoginStatusResult{Status: LoginStatusInvalid, Refreshed: true, HTTPStatus: response.status}, fmt.Errorf("%w: profile rejected refreshed token", ErrLoginExpired)
	}

	// The profile endpoint rejected the access token.  Refresh and retry while
	// holding the client lock so a concurrent operation cannot rotate the token
	// between these two requests.
	if refreshErr := c.refreshLocked(ctx); refreshErr != nil {
		if loginFailureError(refreshErr) {
			return LoginStatusResult{Status: LoginStatusInvalid, HTTPStatus: httpStatus(refreshErr)}, fmt.Errorf("%w: %w", ErrLoginFailed, refreshErr)
		}
		if refreshRejected(refreshErr) {
			return LoginStatusResult{Status: LoginStatusInvalid, HTTPStatus: httpStatus(refreshErr)}, fmt.Errorf("%w: refresh token rejected", ErrLoginExpired)
		}
		return LoginStatusResult{Status: LoginStatusUnknown, HTTPStatus: httpStatus(refreshErr)}, fmt.Errorf("gopay: login status refresh: %w", refreshErr)
	}
	refreshed = true

	retryResponse, retryErr := c.gopayRequest(ctx, http.MethodGet, "/v1/users/profile", nil, nil)
	if retryErr == nil && retryResponse.status == http.StatusOK {
		return LoginStatusResult{Status: LoginStatusValid, Refreshed: true, HTTPStatus: retryResponse.status}, nil
	}
	if retryErr == nil {
		return LoginStatusResult{Status: LoginStatusUnknown, Refreshed: true, HTTPStatus: retryResponse.status}, fmt.Errorf("gopay: login status retry returned unexpected HTTP %d", retryResponse.status)
	}
	if loginFailureResponse(retryResponse.status, retryResponse.body) {
		return LoginStatusResult{Status: LoginStatusInvalid, Refreshed: true, HTTPStatus: retryResponse.status}, fmt.Errorf("%w: %w", ErrLoginFailed, retryErr)
	}
	if loginAuthFailure(retryResponse) {
		return LoginStatusResult{Status: LoginStatusInvalid, Refreshed: true, HTTPStatus: retryResponse.status}, fmt.Errorf("%w: profile rejected refreshed token", ErrLoginExpired)
	}
	return LoginStatusResult{Status: LoginStatusUnknown, Refreshed: refreshed, HTTPStatus: retryResponse.status}, fmt.Errorf("gopay: login status retry: %w", retryErr)
}

func loginFailureError(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && loginFailureResponse(httpErr.StatusCode, httpErr.Body)
}

func loginAuthFailure(response apiResponse) bool {
	// 403 is also emitted by WAF, risk, and temporary policy layers, so it must
	// remain unknown. Only the profile endpoint's unambiguous 401 triggers a
	// refresh and can ultimately establish that the session has expired.
	return response.status == http.StatusUnauthorized
}

func refreshRejected(err error) bool {
	if errors.Is(err, ErrInvalidState) {
		return true
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.StatusCode != http.StatusBadRequest && httpErr.StatusCode != http.StatusUnauthorized {
		return false
	}
	var payload any
	if json.Unmarshal(httpErr.Body, &payload) == nil {
		return containsRefreshRejection(payload, 0)
	}
	return refreshRejectionValue(string(httpErr.Body))
}

func containsRefreshRejection(value any, depth int) bool {
	if depth > 5 {
		return false
	}
	switch typed := value.(type) {
	case string:
		return refreshRejectionValue(typed)
	case map[string]any:
		for _, nested := range typed {
			if containsRefreshRejection(nested, depth+1) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsRefreshRejection(nested, depth+1) {
				return true
			}
		}
	}
	return false
}

func refreshRejectionValue(value string) bool {
	normalized := strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`))
	switch normalized {
	case "invalid_grant", "invalid_token", "invalid_refresh_token",
		"refresh_token_expired", "refresh_token_revoked",
		"refresh token expired", "refresh token is expired",
		"invalid refresh token", "refresh token invalid", "refresh token is invalid",
		"refresh token revoked", "refresh token is revoked", "refresh token has been revoked":
		return true
	default:
		return false
	}
}

func httpStatus(err error) int {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}
	return 0
}
