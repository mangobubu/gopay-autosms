package gopay

import (
	"strings"
	"testing"
)

func TestHTTPErrorRedactsCredentialShapes(t *testing.T) {
	err := (&HTTPError{StatusCode: 400, Body: []byte(
		`{"client_secret":"secret-json","device_token":"device-json","authorization":"Bearer auth-json","challenge_id":"challenge-json","2fa_token":"two-fa-json"} ` +
			`client_secret=secret-form&token=token-form Authorization: Bearer raw-token ` +
			`phone=628123456789 otp=2614 PIN: 9142 account_id=590445936 password=hunter2 session=session-value`,
	)}).Error()
	for _, secret := range []string{
		"secret-json", "device-json", "auth-json", "secret-form", "token-form", "raw-token",
		"challenge-json", "two-fa-json", "628123456789", "2614", "9142", "590445936", "hunter2", "session-value",
	} {
		if strings.Contains(err, secret) {
			t.Fatalf("HTTP error leaked %q: %s", secret, err)
		}
	}
}

func TestRedactErrorDetailMasksUnlabelledIdentifiers(t *testing.T) {
	detail := RedactErrorDetail("OTP 2614 for 628123456789 account 550e8400-e29b-41d4-a716-446655440000 owner@example.com")
	for _, secret := range []string{"2614", "628123456789", "550e8400-e29b-41d4-a716-446655440000", "owner@example.com"} {
		if strings.Contains(detail, secret) {
			t.Fatalf("display detail leaked %q: %s", secret, detail)
		}
	}
}

func TestRedactedBodyKeepsJSONValidWhenMessageEndsWithSensitivePair(t *testing.T) {
	err := &HTTPError{
		StatusCode: 403,
		Body:       []byte(`{"errors":[{"code":"GoPay-112","message":"client_secret=TOP_SECRET_TOKEN"}]}`),
	}
	redacted := err.RedactedBody()
	if strings.Contains(redacted, "TOP_SECRET_TOKEN") {
		t.Fatalf("redacted body leaked secret: %s", redacted)
	}
	if !strings.Contains(redacted, `"code":"GoPay-112"`) || !strings.Contains(redacted, `"message":"client_secret=***"`) {
		t.Fatalf("redacted body corrupted JSON fields: %s", redacted)
	}
}
