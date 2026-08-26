package gopay

import "encoding/json"

type LoginStage string

const (
	LoginStageIdle           LoginStage = "idle"
	LoginStageAwaiting1FAOTP LoginStage = "awaiting_login_1fa_otp"
	LoginStageReady2FA       LoginStage = "ready_login_2fa_cycle"
	LoginStageCycleReady2FA  LoginStage = "login_2fa_cycle_ready"
	LoginStageAwaiting2FAOTP LoginStage = "awaiting_login_2fa_otp"
	LoginStageAuthenticated  LoginStage = "authenticated"
)

type OTPPurpose string

const (
	OTPPurposeLogin1FA OTPPurpose = "login_1fa"
	OTPPurposeLogin2FA OTPPurpose = "login_2fa"
	OTPPurposePINSetup OTPPurpose = "pin_setup"
)

type PINStage string

const (
	PINStageIdle       PINStage = ""
	PINStageReadyCycle PINStage = "ready_pin_cycle"
	PINStageCycleReady PINStage = "pin_cycle_ready"
	PINStageAwaiting   PINStage = "awaiting_pin_otp"
	PINStageComplete   PINStage = "pin_complete"
)

// Session contains all protocol state needed to resume a multi-request login
// or PIN setup. It can be persisted directly as JSON.
type Session struct {
	Phone       string         `json:"phone"`
	CountryCode string         `json:"country_code"`
	Device      DeviceIdentity `json:"device"`
	ProxyURL    string         `json:"proxy_url,omitempty"`
	DeviceToken string         `json:"device_token,omitempty"`
	SMSCycle    int            `json:"sms_cycle"`

	TransactionID     string     `json:"transaction_id"`
	VerificationID    string     `json:"verification_id"`
	OTPToken          string     `json:"otp_token"`
	OTPLength         int        `json:"otp_length"`
	OTPChannel        string     `json:"otp_channel"`
	VerificationToken string     `json:"verification_token"`
	OneFAToken        string     `json:"one_fa_token"`
	AccountID         string     `json:"account_id"`
	AccessToken       string     `json:"access_token"`
	RefreshToken      string     `json:"refresh_token"`
	TwoFAToken        string     `json:"two_fa_token"`
	TwoFAMethods      []string   `json:"two_fa_methods,omitempty"`
	Methods           []string   `json:"methods,omitempty"`
	LoginStage        LoginStage `json:"login_stage"`

	PINVerificationID    string   `json:"pin_verification_id"`
	PINOTPToken          string   `json:"pin_otp_token"`
	PINVerificationToken string   `json:"pin_verification_token"`
	PINToken             string   `json:"pin_token,omitempty"`
	PINStage             PINStage `json:"pin_stage,omitempty"`
}

func (s Session) Marshal() ([]byte, error) { return json.Marshal(s) }

func ParseSession(data []byte) (Session, error) {
	var s Session
	err := json.Unmarshal(data, &s)
	return s, err
}

type LoginMethods struct {
	Methods        []string `json:"methods"`
	VerificationID string   `json:"verification_id"`
}

type OTPChallenge struct {
	Purpose        OTPPurpose `json:"purpose"`
	Method         string     `json:"method"`
	Token          string     `json:"token"`
	Length         int        `json:"length"`
	VerificationID string     `json:"verification_id"`
}

type LoginResult struct {
	Authenticated bool          `json:"authenticated"`
	NeedsOTP      bool          `json:"needs_otp"`
	Challenge     *OTPChallenge `json:"challenge,omitempty"`
	Session       Session       `json:"session"`
}

// BalanceResult differentiates an authentic zero from an unknown value.
type BalanceResult struct {
	Amount   int64           `json:"amount"`
	Currency string          `json:"currency,omitempty"`
	Known    bool            `json:"known"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}
