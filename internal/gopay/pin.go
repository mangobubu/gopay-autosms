package gopay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

var sixDigitPIN = regexp.MustCompile(`^[0-9]{6}$`)

// StartPINSetup checks the candidate PIN, discovers CVS methods, then sends the
// PIN-change SMS. The returned challenge is safe to expose to the workflow.
func (c *Client) StartPINSetup(ctx context.Context, pin string) (OTPChallenge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !sixDigitPIN.MatchString(pin) {
		return OTPChallenge{}, ErrInvalidPIN
	}
	if c.session.AccessToken == "" {
		return OTPChallenge{}, fmt.Errorf("%w: access token is missing", ErrInvalidState)
	}
	if err := c.checkPINAllowedLocked(ctx, pin); err != nil {
		return OTPChallenge{}, err
	}
	c.session.PINVerificationToken = ""
	c.session.PINVerificationID = ""
	c.session.PINOTPToken = ""
	c.session.PINChallengeID = ""
	c.session.PINClientID = ""

	c.session.TransactionID = c.newID()
	methodsBody := struct {
		ClientID                  string  `json:"client_id"`
		ClientSecret              string  `json:"client_secret"`
		CountryCode               *string `json:"country_code"`
		DeviceVerificationTokenID *string `json:"device_verification_token_id"`
		EmailAddress              *string `json:"email_address"`
		Flow                      string  `json:"flow"`
		PhoneNumber               *string `json:"phone_number"`
	}{ClientID: c.clientID, ClientSecret: c.secret, Flow: "goto_pin_wa_sms"}
	methodsResponse, err := c.ssoPost(ctx, "/cvs/v1/methods", "", methodsBody, nil)
	if err != nil {
		return OTPChallenge{}, classifyPINError(err)
	}
	methodsData := dataObject(methodsResponse.json)
	c.session.PINVerificationID = stringValue(methodsData, "verification_id")
	methods := stringsFrom(methodsData["methods"])
	if c.session.PINVerificationID == "" {
		return OTPChallenge{}, fmt.Errorf("gopay: PIN methods response has no verification ID")
	}
	foundSMS := false
	for _, method := range methods {
		if method == "otp_sms" {
			foundSMS = true
			break
		}
	}
	if len(methods) != 0 && !foundSMS {
		return OTPChallenge{}, fmt.Errorf("gopay: PIN setup does not offer otp_sms")
	}

	initBody := struct {
		ClientID                  string  `json:"client_id"`
		ClientSecret              string  `json:"client_secret"`
		CountryCode               *string `json:"country_code"`
		DeviceVerificationTokenID *string `json:"device_verification_token_id"`
		EmailAddress              *string `json:"email_address"`
		Flow                      string  `json:"flow"`
		IsMultipleMethod          *bool   `json:"is_multiple_method"`
		PhoneNumber               *string `json:"phone_number"`
		VerificationID            string  `json:"verification_id"`
		VerificationMethod        string  `json:"verification_method"`
	}{ClientID: c.clientID, ClientSecret: c.secret, Flow: "goto_pin_wa_sms", VerificationID: c.session.PINVerificationID, VerificationMethod: "otp_sms"}
	extra := make(http.Header)
	extra.Set("key", "value")
	initResponse, err := c.ssoPost(ctx, c.cvsInitiatePath, "/cvs/v1/initiate", initBody, extra)
	if err != nil {
		return OTPChallenge{}, classifyPINError(err)
	}
	initData := dataObject(initResponse.json)
	c.session.PINOTPToken = stringValue(initData, "otp_token")
	length := intNumber(initData["otp_length"], 4)
	if c.session.PINOTPToken == "" {
		return OTPChallenge{}, fmt.Errorf("gopay: PIN OTP initiate response has no token")
	}
	c.session.PINStage = PINStageAwaiting
	return OTPChallenge{Purpose: OTPPurposePINSetup, Method: "otp_sms", Token: c.session.PINOTPToken, Length: length, VerificationID: c.session.PINVerificationID}, nil
}

func (c *Client) checkPINAllowedLocked(ctx context.Context, pin string) error {
	response, err := c.gopayRequest(ctx, http.MethodPost, "/api/v1/users/pins/allowed", struct {
		PIN string `json:"pin"`
	}{pin}, nil)
	if err != nil {
		return classifyPINError(err)
	}
	if allowed, ok := dataObject(response.json)["allowed"].(bool); ok && !allowed {
		return ErrPINNotAllowed
	}
	return nil
}

// GetPINStatus reads the authenticated profile before selecting setup versus
// reset. Some compatible responses omit the flag, in which case Known=false.
func (c *Client) GetPINStatus(ctx context.Context) (PINStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session.AccessToken == "" {
		return PINStatus{}, fmt.Errorf("%w: access token is missing", ErrInvalidState)
	}
	response, err := c.gopayRequest(ctx, http.MethodGet, "/v1/users/profile", nil, nil)
	if err != nil {
		return PINStatus{}, classifyPINError(err)
	}
	value, ok := findPINSetupFlag(response.json)
	return PINStatus{Known: ok, Set: value}, nil
}

func findPINSetupFlag(value any) (bool, bool) {
	switch node := value.(type) {
	case map[string]any:
		for _, key := range []string{"is_pin_setup", "isPinSetup", "pin_setup", "pinSetup", "pin_enabled", "pinEnabled", "has_pin", "hasPin"} {
			if flag, ok := pinFlag(node[key]); ok {
				return flag, true
			}
		}
		for _, child := range node {
			if flag, ok := findPINSetupFlag(child); ok {
				return flag, true
			}
		}
	case []any:
		for _, child := range node {
			if flag, ok := findPINSetupFlag(child); ok {
				return flag, true
			}
		}
	}
	return false, false
}

func pinFlag(value any) (bool, bool) {
	switch flag := value.(type) {
	case bool:
		return flag, true
	case json.Number:
		return flag.String() != "0", true
	case float64:
		return flag != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(flag)) {
		case "1", "true", "yes", "y", "on":
			return true, true
		case "0", "false", "no", "n", "off":
			return false, true
		}
	}
	return false, false
}

// VerifyPINSetupOTP consumes the one-time CVS challenge. Callers must persist
// State after this succeeds before attempting the separately retryable setup.
func (c *Client) VerifyPINSetupOTP(ctx context.Context, otp string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session.PINVerificationToken != "" {
		c.session.PINStage = PINStageSetupVerified
		return nil
	}
	if otp == "" {
		return fmt.Errorf("gopay: PIN OTP is required")
	}
	if c.session.PINOTPToken == "" || c.session.PINVerificationID == "" {
		return fmt.Errorf("%w: call StartPINSetup first", ErrInvalidState)
	}
	verifyBody := struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Data         struct {
			OTP      string `json:"otp"`
			OTPToken string `json:"otp_token"`
		} `json:"data"`
		Flow               string `json:"flow"`
		VerificationID     string `json:"verification_id"`
		VerificationMethod string `json:"verification_method"`
	}{ClientID: c.clientID, ClientSecret: c.secret, Flow: "goto_pin_wa_sms", VerificationID: c.session.PINVerificationID, VerificationMethod: "otp_sms"}
	verifyBody.Data.OTP = otp
	verifyBody.Data.OTPToken = c.session.PINOTPToken
	verifyResponse, err := c.ssoPost(ctx, "/cvs/v1/verify", "", verifyBody, nil)
	if err != nil {
		return classifyPINError(err)
	}
	c.session.PINVerificationToken = stringValue(dataObject(verifyResponse.json), "verification_token")
	if c.session.PINVerificationToken == "" {
		return fmt.Errorf("gopay: PIN OTP response has no verification token")
	}
	c.session.PINStage = PINStageSetupVerified
	return nil
}

// CompletePINSetup installs a PIN after VerifyPINSetupOTP has been persisted.
func (c *Client) CompletePINSetup(ctx context.Context, pin string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !sixDigitPIN.MatchString(pin) {
		return ErrInvalidPIN
	}
	if c.session.PINVerificationToken == "" {
		return fmt.Errorf("%w: verify PIN OTP first", ErrInvalidState)
	}
	headers := make(http.Header)
	headers.Set("Verification-Token", bearer(c.session.PINVerificationToken))
	headers.Set("is-token-required", "false")
	setupResponse, err := c.gopayRequest(ctx, http.MethodPost, "/api/v2/users/pins/setup/tokens", struct {
		ChallengeID string `json:"challenge_id"`
		ClientID    string `json:"client_id"`
		PIN         string `json:"pin"`
	}{PIN: pin}, headers)
	if err != nil {
		return classifyPINError(err)
	}
	c.session.PINToken = stringValue(dataObject(setupResponse.json), "token")
	c.session.PINStage = PINStageComplete
	return nil
}

// FinishPINSetup is kept as a convenience for direct callers. Durable
// workflows should use the split verify/complete methods above.
func (c *Client) FinishPINSetup(ctx context.Context, pin, otp string) error {
	if err := c.VerifyPINSetupOTP(ctx, otp); err != nil {
		return err
	}
	return c.CompletePINSetup(ctx, pin)
}

// StartPINReset sends an OTP for the authenticated forgotten/change-PIN flow.
func (c *Client) StartPINReset(ctx context.Context, pin string) (OTPChallenge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !sixDigitPIN.MatchString(pin) {
		return OTPChallenge{}, ErrInvalidPIN
	}
	if c.session.AccessToken == "" {
		return OTPChallenge{}, fmt.Errorf("%w: access token is missing", ErrInvalidState)
	}
	if err := c.checkPINAllowedLocked(ctx, pin); err != nil {
		return OTPChallenge{}, err
	}
	c.session.PINVerificationToken = ""
	c.session.PINVerificationID = ""
	c.session.PINOTPToken = ""
	c.session.PINChallengeID = ""
	c.session.PINClientID = ""
	challenge, err := c.gopayRequest(ctx, http.MethodPost, "/api/v1/users/pin/challenges", map[string]string{"flow": "pin_change"}, nil)
	if err != nil {
		return OTPChallenge{}, classifyPINError(err)
	}
	challengeData := dataObject(challenge.json)
	c.session.PINChallengeID = stringValue(challengeData, "challenge_id")
	c.session.PINClientID = stringValue(challengeData, "client_id")
	if c.session.PINChallengeID == "" || c.session.PINClientID == "" {
		return OTPChallenge{}, fmt.Errorf("gopay: PIN reset challenge is incomplete")
	}
	c.session.TransactionID = c.newID()
	methodsBody := map[string]any{
		"country_code": nil, "email_address": nil, "client_id": c.clientID,
		"phone_number": nil, "client_secret": c.secret,
		"flow": "goto_pin_wa_sms_gp_app", "device_verification_token_id": nil,
	}
	methods, err := c.ssoPost(ctx, "/cvs/v1/methods", "", methodsBody, nil)
	if err != nil {
		return OTPChallenge{}, classifyPINError(err)
	}
	methodsData := dataObject(methods.json)
	c.session.PINVerificationID = stringValue(methodsData, "verification_id")
	if c.session.PINVerificationID == "" {
		return OTPChallenge{}, fmt.Errorf("gopay: PIN reset methods response has no verification ID")
	}
	resetMethods := stringsFrom(methodsData["methods"])
	foundSMS := false
	for _, method := range resetMethods {
		if method == "otp_sms" {
			foundSMS = true
			break
		}
	}
	if len(resetMethods) != 0 && !foundSMS {
		return OTPChallenge{}, fmt.Errorf("gopay: PIN reset does not offer otp_sms")
	}
	initBody := map[string]any{}
	for key, value := range methodsBody {
		initBody[key] = value
	}
	initBody["verification_id"] = c.session.PINVerificationID
	initBody["verification_method"] = "otp_sms"
	initBody["is_multiple_method"] = nil
	extra := make(http.Header)
	extra.Set("key", "value")
	initResponse, err := c.ssoPost(ctx, c.cvsInitiatePath, "/cvs/v1/initiate", initBody, extra)
	if err != nil {
		return OTPChallenge{}, classifyPINError(err)
	}
	initData := dataObject(initResponse.json)
	c.session.PINOTPToken = stringValue(initData, "otp_token")
	length := intNumber(initData["otp_length"], 4)
	if c.session.PINOTPToken == "" {
		return OTPChallenge{}, fmt.Errorf("gopay: PIN reset OTP response has no token")
	}
	c.session.PINVerificationToken = ""
	c.session.PINStage = PINStageResetAwaiting
	return OTPChallenge{Purpose: OTPPurposePINSetup, Method: "otp_sms", Token: c.session.PINOTPToken, Length: length, VerificationID: c.session.PINVerificationID}, nil
}

func (c *Client) VerifyPINResetOTP(ctx context.Context, otp string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session.PINVerificationToken != "" {
		c.session.PINStage = PINStageResetVerified
		return nil
	}
	if strings.TrimSpace(otp) == "" || c.session.PINOTPToken == "" || c.session.PINVerificationID == "" || c.session.PINChallengeID == "" || c.session.PINClientID == "" {
		return fmt.Errorf("%w: PIN reset OTP state is incomplete", ErrInvalidState)
	}
	body := map[string]any{
		"client_id": c.clientID, "client_secret": c.secret,
		"flow": "goto_pin_wa_sms_gp_app", "verification_method": "otp_sms",
		"verification_id": c.session.PINVerificationID,
		"data":            map[string]string{"otp": strings.TrimSpace(otp), "otp_token": c.session.PINOTPToken},
	}
	response, err := c.ssoPost(ctx, "/cvs/v1/verify", "", body, nil)
	if err != nil {
		return classifyPINError(err)
	}
	c.session.PINVerificationToken = stringValue(dataObject(response.json), "verification_token")
	if c.session.PINVerificationToken == "" {
		return fmt.Errorf("gopay: PIN reset OTP response has no verification token")
	}
	c.session.PINStage = PINStageResetVerified
	return nil
}

func (c *Client) CompletePINReset(ctx context.Context, pin string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !sixDigitPIN.MatchString(pin) {
		return ErrInvalidPIN
	}
	if c.session.PINVerificationToken == "" || c.session.PINChallengeID == "" || c.session.PINClientID == "" {
		return fmt.Errorf("%w: verify PIN reset OTP first", ErrInvalidState)
	}
	headers := make(http.Header)
	headers.Set("Verification-Token", bearer(c.session.PINVerificationToken))
	response, err := c.gopayRequest(ctx, http.MethodPut, "/api/v2/users/pins/reset/tokens", struct {
		PIN         string `json:"pin"`
		ClientID    string `json:"client_id"`
		ChallengeID string `json:"challenge_id"`
	}{PIN: pin, ClientID: c.session.PINClientID, ChallengeID: c.session.PINChallengeID}, headers)
	if err != nil {
		return classifyPINError(err)
	}
	c.session.PINToken = stringValue(dataObject(response.json), "token")
	c.session.PINStage = PINStageComplete
	return nil
}
