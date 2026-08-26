package gopay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ProbeLogin asks GoPay which login methods are available. It never sends an
// OTP. Accounts that advertise goto_pin return ErrPINRequired immediately;
// missing accounts return ErrUnregistered.
func (c *Client) ProbeLogin(ctx context.Context, countryCode, phone string) (LoginMethods, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if countryCode == "" {
		countryCode = "+62"
	}
	if phone == "" {
		return LoginMethods{}, fmt.Errorf("gopay: phone number is required")
	}
	c.session.Phone = phone
	c.session.CountryCode = countryCode
	c.session.TransactionID = c.newID()
	c.session.LoginStage = LoginStageIdle

	body := struct {
		ClientID                  string `json:"client_id"`
		ClientSecret              string `json:"client_secret"`
		CountryCode               string `json:"country_code"`
		DeviceVerificationTokenID string `json:"device_verification_token_id"`
		Email                     string `json:"email"`
		PhoneNumber               string `json:"phone_number"`
	}{c.clientID, c.secret, countryCode, "", "", phone}
	response, err := c.ssoPost(ctx, "/goto-auth/login/methods", "", body, nil)
	if err != nil {
		return LoginMethods{}, classifyLoginError(response, err)
	}
	if err := classifyLoginError(response, nil); err != nil {
		return LoginMethods{}, err
	}
	data := dataObject(response.json)
	methods := stringsFrom(data["methods"])
	c.session.VerificationID = stringValue(data, "verification_id")
	c.session.Methods = append([]string(nil), methods...)
	result := LoginMethods{Methods: append([]string(nil), methods...), VerificationID: c.session.VerificationID}
	hasSMS := false
	for _, method := range methods {
		if method == "goto_pin" {
			return result, ErrPINRequired
		}
		if method == "otp_sms" {
			hasSMS = true
		}
	}
	if c.session.VerificationID == "" || len(methods) == 0 {
		return result, fmt.Errorf("gopay: login methods response is incomplete")
	}
	if !hasSMS {
		return result, fmt.Errorf("gopay: account does not offer otp_sms login")
	}
	return result, nil
}

// StartLogin initiates OTP-only login after ProbeLogin. It is deliberately
// separate so a purchased SMS number is not consumed before the PIN check.
func (c *Client) StartLogin(ctx context.Context) (OTPChallenge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session.VerificationID == "" || c.session.Phone == "" {
		return OTPChallenge{}, fmt.Errorf("%w: call ProbeLogin first", ErrInvalidState)
	}
	for _, method := range c.session.Methods {
		if method == "goto_pin" {
			return OTPChallenge{}, ErrPINRequired
		}
		if method == "otp_sms" {
			return c.initiateLoginOTPLocked(ctx, OTPPurposeLogin1FA)
		}
	}
	return OTPChallenge{}, fmt.Errorf("gopay: account does not offer otp_sms login")
}

func (c *Client) initiateLoginOTPLocked(ctx context.Context, purpose OTPPurpose) (OTPChallenge, error) {
	flow := string(purpose)
	body := struct {
		ClientID           string `json:"client_id"`
		ClientSecret       string `json:"client_secret"`
		Flow               string `json:"flow"`
		VerificationID     string `json:"verification_id"`
		VerificationMethod string `json:"verification_method"`
		CountryCode        string `json:"country_code"`
		PhoneNumber        string `json:"phone_number"`
		IsMultipleMethod   bool   `json:"is_multiple_method"`
	}{c.clientID, c.secret, flow, c.session.VerificationID, "otp_sms", c.session.CountryCode, c.session.Phone, true}
	extra := make(http.Header)
	extra.Set("key", "value")
	response, err := c.ssoPost(ctx, c.cvsInitiatePath, "/cvs/v1/initiate", body, extra)
	if err != nil {
		return OTPChallenge{}, classifyLoginError(response, err)
	}
	if err := classifyLoginError(response, nil); err != nil {
		return OTPChallenge{}, err
	}
	data := dataObject(response.json)
	c.session.OTPToken = stringValue(data, "otp_token")
	c.session.OTPChannel = "otp_sms"
	c.session.OTPLength = intNumber(data["otp_length"], 4)
	if c.session.OTPToken == "" {
		return OTPChallenge{}, fmt.Errorf("gopay: OTP initiate response has no token")
	}
	if purpose == OTPPurposeLogin2FA {
		c.session.LoginStage = LoginStageAwaiting2FAOTP
	} else {
		c.session.LoginStage = LoginStageAwaiting1FAOTP
	}
	return OTPChallenge{Purpose: purpose, Method: "otp_sms", Token: c.session.OTPToken, Length: c.session.OTPLength, VerificationID: c.session.VerificationID}, nil
}

// VerifyLoginOTP advances the login state machine by one SMS code. If GoPay
// requires 2FA it returns NeedsOTP with the new challenge; otherwise it returns
// an authenticated session. The caller can therefore keep a single OTP loop.
func (c *Client) VerifyLoginOTP(ctx context.Context, otp string) (LoginResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if otp == "" {
		return LoginResult{}, fmt.Errorf("gopay: OTP is required")
	}
	if c.session.LoginStage != LoginStageAwaiting1FAOTP && c.session.LoginStage != LoginStageAwaiting2FAOTP {
		return LoginResult{}, ErrInvalidState
	}
	flow := "login_1fa"
	if c.session.LoginStage == LoginStageAwaiting2FAOTP {
		flow = "login_2fa"
	}
	if err := c.verifyOTPLocked(ctx, otp, flow); err != nil {
		return LoginResult{}, err
	}

	if flow == "login_1fa" {
		if err := c.accountListLocked(ctx); err != nil {
			return LoginResult{}, err
		}
		authenticated, err := c.issueTokenLocked(ctx, "cvs", c.session.OneFAToken)
		if err != nil {
			return LoginResult{}, err
		}
		if authenticated {
			c.session.LoginStage = LoginStageAuthenticated
			return LoginResult{Authenticated: true, Session: cloneSession(c.session)}, nil
		}
		c.session.LoginStage = LoginStageReady2FA
		return LoginResult{NeedsOTP: true, Session: cloneSession(c.session)}, nil
	}

	authenticated, err := c.issueTokenLocked(ctx, "challenge", c.session.TwoFAToken)
	if err != nil {
		return LoginResult{}, err
	}
	if !authenticated {
		return LoginResult{}, fmt.Errorf("gopay: challenge token exchange did not authenticate")
	}
	c.session.LoginStage = LoginStageAuthenticated
	return LoginResult{Authenticated: true, Session: cloneSession(c.session)}, nil
}

func (c *Client) StartNextLoginOTP(ctx context.Context) (OTPChallenge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session.LoginStage != LoginStageCycleReady2FA {
		return OTPChallenge{}, fmt.Errorf("%w: login 2FA cycle is not ready", ErrInvalidState)
	}
	return c.initiateLoginOTPLocked(ctx, OTPPurposeLogin2FA)
}

// Refresh exchanges the persisted refresh token for a fresh access token.
// It mirrors the attachment's refresh_token grant and is useful after a long
// holding period or service restart.
func (c *Client) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshLocked(ctx)
}

// refreshLocked exchanges the persisted refresh token for a fresh access
// token. The caller must hold c.mu. Keeping the operation in a lock-aware
// helper lets login-status probing refresh and retry atomically without
// racing another request for the same account.
func (c *Client) refreshLocked(ctx context.Context) error {
	if c.session.RefreshToken == "" {
		return fmt.Errorf("%w: refresh token is missing", ErrInvalidState)
	}
	body := struct {
		GrantType    string `json:"grant_type"`
		Token        string `json:"token"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}{"refresh_token", c.session.RefreshToken, c.clientID, c.secret}
	response, err := c.ssoPost(ctx, "/goto-auth/token", "", body, nil)
	if err != nil {
		return err
	}
	data := dataObject(response.json)
	access := stringValue(data, "access_token")
	if access == "" {
		return fmt.Errorf("gopay: refresh response has no access token")
	}
	c.session.AccessToken = access
	if refresh := stringValue(data, "refresh_token"); refresh != "" {
		c.session.RefreshToken = refresh
	}
	return nil
}

func (c *Client) verifyOTPLocked(ctx context.Context, otp, flow string) error {
	body := struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Data         struct {
			OTP      string `json:"otp"`
			OTPToken string `json:"otp_token"`
		} `json:"data"`
		Flow               string `json:"flow"`
		VerificationID     string `json:"verification_id"`
		VerificationMethod string `json:"verification_method"`
	}{ClientID: c.clientID, ClientSecret: c.secret, Flow: flow, VerificationID: c.session.VerificationID, VerificationMethod: "otp_sms"}
	body.Data.OTP = otp
	body.Data.OTPToken = c.session.OTPToken
	response, err := c.ssoPost(ctx, "/cvs/v1/verify", "", body, nil)
	if err != nil {
		return classifyLoginError(response, err)
	}
	if err := classifyLoginError(response, nil); err != nil {
		return err
	}
	c.session.VerificationToken = stringValue(dataObject(response.json), "verification_token")
	if c.session.VerificationToken == "" {
		return fmt.Errorf("gopay: OTP verification response has no verification token")
	}
	return nil
}

func (c *Client) accountListLocked(ctx context.Context) error {
	body := struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}{c.clientID, c.secret}
	headers := make(http.Header)
	headers.Set("verification-token", bearer(c.session.VerificationToken))
	response, err := c.ssoPost(ctx, "/goto-auth/accountlist", "", body, headers)
	if err != nil {
		return classifyLoginError(response, err)
	}
	if err := classifyLoginError(response, nil); err != nil {
		return err
	}
	data := dataObject(response.json)
	c.session.OneFAToken = stringValue(data, "1fa_token")
	accounts, _ := data["account_list"].([]any)
	if len(accounts) != 0 {
		if account, ok := accounts[0].(map[string]any); ok {
			c.session.AccountID = anyString(account["account_id"])
		}
	}
	if c.session.OneFAToken == "" || c.session.AccountID == "" {
		return fmt.Errorf("gopay: account list response is incomplete")
	}
	return nil
}

func (c *Client) issueTokenLocked(ctx context.Context, grantType, token string) (bool, error) {
	body := struct {
		ClientID     string  `json:"client_id"`
		ClientSecret string  `json:"client_secret"`
		GrantType    string  `json:"grant_type"`
		Token        string  `json:"token"`
		AccountID    string  `json:"account_id"`
		ExtUserToken *string `json:"ext_user_token"`
	}{c.clientID, c.secret, grantType, token, c.session.AccountID, nil}
	headers := make(http.Header)
	if grantType == "cvs" {
		headers.Set("verification-token", c.session.OneFAToken)
	} else {
		headers.Set("verification-token", bearer(c.session.VerificationToken))
	}
	response, err := c.ssoPost(ctx, "/goto-auth/token", "", body, headers)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden {
			data := dataObject(response.json)
			c.session.TwoFAToken = stringValue(data, "2fa_token")
			c.session.TwoFAMethods = stringsFrom(data["methods"])
			if nextID := stringValue(data, "verification_id"); nextID != "" {
				c.session.VerificationID = nextID
			}
			if c.session.TwoFAToken == "" {
				return false, fmt.Errorf("gopay: 2FA response has no token")
			}
			return false, nil
		}
		return false, classifyLoginError(response, err)
	}
	if err := classifyLoginError(response, nil); err != nil {
		return false, err
	}
	data := dataObject(response.json)
	c.session.AccessToken = stringValue(data, "access_token")
	c.session.RefreshToken = stringValue(data, "refresh_token")
	if c.session.AccessToken == "" {
		return false, fmt.Errorf("gopay: token response has no access token")
	}
	return true, nil
}

func isUnregisteredResponse(status int, body []byte, err error) bool {
	text := strings.ToLower(string(body))
	if text == "" && err != nil {
		text = strings.ToLower(err.Error())
	}
	return strings.Contains(text, "auth:error:user:not_found") ||
		strings.Contains(text, "user:not_found") ||
		strings.Contains(text, "could not find the user") ||
		(status == http.StatusNotFound && strings.Contains(text, "not_found"))
}

func classifyLoginError(response apiResponse, err error) error {
	text := strings.ToLower(string(response.body))
	if strings.Contains(text, "gopay-119") || strings.Contains(text, "pin salah") || strings.Contains(text, "requires_pin") || strings.Contains(text, "goto_pin") {
		return ErrPINRequired
	}
	if isUnregisteredResponse(response.status, response.body, err) {
		return ErrUnregistered
	}
	return err
}

func intNumber(value any, fallback int) int {
	switch n := value.(type) {
	case json.Number:
		parsed, err := strconv.Atoi(n.String())
		if err == nil {
			return parsed
		}
	case float64:
		return int(n)
	case int:
		return n
	default:
		return fallback
	}
	return fallback
}

func anyString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		return ""
	}
}
