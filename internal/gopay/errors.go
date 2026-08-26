package gopay

import (
	"errors"
	"fmt"
	"regexp"
)

var (
	// ErrPINRequired means the account advertises goto_pin and must not be
	// consumed by the OTP-only login flow.
	ErrPINRequired = errors.New("gopay: existing PIN is required")
	// ErrUnregistered means the phone number is not registered with GoPay.
	ErrUnregistered = errors.New("gopay: phone number is not registered")
	// ErrBalanceUnknown is returned when a successful balance response does not
	// contain a trustworthy numeric value. Callers must not treat it as zero.
	ErrBalanceUnknown = errors.New("gopay: balance is unknown")
	ErrInvalidPIN     = errors.New("gopay: PIN must contain exactly 6 digits")
	ErrInvalidState   = errors.New("gopay: operation is invalid for the current session state")
)

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
	return fmt.Sprintf("gopay: HTTP %d: %s", e.StatusCode, redactBody(string(e.Body)))
}

var sensitiveJSON = regexp.MustCompile(`(?i)("(?:access_token|refresh_token|token|otp_token|verification_token|phone|phone_number)"\s*:\s*")[^"]+(")`)

func redactBody(value string) string {
	return sensitiveJSON.ReplaceAllString(value, `${1}***${2}`)
}
