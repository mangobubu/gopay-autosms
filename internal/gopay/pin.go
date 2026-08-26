package gopay

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
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
	if _, err := c.gopayRequest(ctx, http.MethodPost, "/api/v1/users/pins/allowed", struct {
		PIN string `json:"pin"`
	}{pin}, nil); err != nil {
		return OTPChallenge{}, err
	}

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
		return OTPChallenge{}, err
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
		return OTPChallenge{}, err
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

// FinishPINSetup verifies the SMS code and installs the six-digit PIN.
func (c *Client) FinishPINSetup(ctx context.Context, pin, otp string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !sixDigitPIN.MatchString(pin) {
		return ErrInvalidPIN
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
		return err
	}
	c.session.PINVerificationToken = stringValue(dataObject(verifyResponse.json), "verification_token")
	if c.session.PINVerificationToken == "" {
		return fmt.Errorf("gopay: PIN OTP response has no verification token")
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
		return err
	}
	c.session.PINToken = stringValue(dataObject(setupResponse.json), "token")
	c.session.PINStage = PINStageComplete
	return nil
}
