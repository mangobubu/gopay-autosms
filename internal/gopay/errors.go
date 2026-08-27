package gopay

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	// ErrPINRequired means the account advertises goto_pin and must not be
	// consumed by the OTP-only login flow.
	ErrPINRequired = errors.New("gopay: existing PIN is required")
	// ErrUnregistered means the phone number is not registered with GoPay.
	ErrUnregistered = errors.New("gopay: phone number is not registered")
	// ErrLoginFailed marks an explicit account-level login rejection. It is
	// deliberately narrower than a generic HTTP 403/429: callers should only
	// use it when GoPay returns one of the structured terminal error codes.
	ErrLoginFailed = errors.New("gopay: login failed")
	// ErrBalanceUnknown is returned when a successful balance response does not
	// contain a trustworthy numeric value. Callers must not treat it as zero.
	ErrBalanceUnknown = errors.New("gopay: balance is unknown")
	// ErrPINAlreadySet is returned when GoPay reports GoPay-111. It aliases
	// ErrPINRequired so callers written for the original login classifier keep
	// working; the caller must use the authenticated reset-PIN flow instead of
	// replaying setup OTP.
	ErrPINAlreadySet = ErrPINRequired
	// ErrPINVerificationExpired marks a consumed or expired one-time CVS
	// verification. Replaying the same OTP can never recover it.
	ErrPINVerificationExpired = errors.New("gopay: PIN verification has expired")
	// ErrPINVerificationInvalid is a descriptive compatibility alias.
	ErrPINVerificationInvalid = ErrPINVerificationExpired
	ErrPINNotAllowed          = errors.New("gopay: requested PIN is not allowed")
	ErrInvalidPIN             = errors.New("gopay: PIN must contain exactly 6 digits")
	ErrInvalidState           = errors.New("gopay: operation is invalid for the current session state")
)

func classifyPINError(err error) error {
	if err == nil {
		return nil
	}
	if loginFailureError(err) {
		return fmt.Errorf("%w: %w", ErrLoginFailed, err)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	text := strings.ToLower(string(httpErr.Body))
	switch {
	case strings.Contains(text, "gopay-111"),
		strings.Contains(text, "pin sudah terpasang"),
		strings.Contains(text, "pin kamu sudah aktif"):
		return fmt.Errorf("%w: %w", ErrPINAlreadySet, err)
	case strings.Contains(text, "verification_id_invalid"):
		return fmt.Errorf("%w: %w", ErrPINVerificationExpired, err)
	default:
		return err
	}
}

// HTTPError retains the response status and a bounded copy of the response
// body. It is useful for logging without discarding the API's error details.
type HTTPError struct {
	StatusCode int
	Body       []byte
}

func (e *HTTPError) Error() string {
	if len(e.Body) == 0 {
		return fmt.Sprintf("gopay: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("gopay: HTTP %d: %s", e.StatusCode, e.RedactedBody())
}

// RedactedBody returns the retained upstream response with credential-shaped
// values masked. Callers that expose an HTTP error outside logs should still
// select only the fields needed for that view instead of returning the whole
// response body.
func (e *HTTPError) RedactedBody() string {
	if e == nil {
		return ""
	}
	return redactBody(string(e.Body))
}

var (
	sensitiveJSON   = regexp.MustCompile(`(?i)("(?:access_token|refresh_token|token|otp_token|pin_otp_token|verification_token|1fa_token|2fa_token|one_fa_token|two_fa_token|client_secret|device_token|authorization|cookie|set_cookie|session|session_id|phone|phone_number|mobile|msisdn|email|email_address|otp|pin|password|passcode|account_id|user_id|device_id|unique_id|verification_id|pin_verification_id|verification_code|sms_code|challenge_id|transaction_id|proxy_url)"\s*:\s*")[^"]+(")`)
	sensitivePair   = regexp.MustCompile(`(?i)(\b(?:access_token|refresh_token|token|otp_token|pin_otp_token|verification_token|1fa_token|2fa_token|one_fa_token|two_fa_token|client_secret|device_token|authorization|cookie|set_cookie|session|session_id|phone|phone_number|mobile|msisdn|email|email_address|otp|pin|password|passcode|account_id|user_id|device_id|unique_id|verification_id|pin_verification_id|verification_code|sms_code|challenge_id|transaction_id|proxy_url)\b\s*[:=]\s*["']?)[^&\s"',}\]]+`)
	sensitiveBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	sensitiveNumber = regexp.MustCompile(`\b[0-9]{4,19}\b`)
	sensitiveUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	sensitiveEmail  = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
)

func redactBody(value string) string {
	value = sensitiveBearer.ReplaceAllString(value, `Bearer ***`)
	value = sensitiveJSON.ReplaceAllString(value, `${1}***${2}`)
	value = sensitivePair.ReplaceAllString(value, `${1}***`)
	return value
}

// RedactErrorDetail masks credential-shaped values plus numeric identifiers in
// a small upstream message selected for display. It is deliberately more
// conservative than RedactedBody, which is primarily intended for logs.
func RedactErrorDetail(value string) string {
	value = redactBody(value)
	value = sensitiveUUID.ReplaceAllString(value, `***`)
	value = sensitiveEmail.ReplaceAllString(value, `***`)
	return sensitiveNumber.ReplaceAllString(value, `***`)
}
