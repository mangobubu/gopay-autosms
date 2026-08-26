package gopay

import (
	"strings"
	"testing"
)

func TestHTTPErrorRedactsCredentialShapes(t *testing.T) {
	err := (&HTTPError{StatusCode: 400, Body: []byte(
		`{"client_secret":"secret-json","device_token":"device-json","authorization":"Bearer auth-json"} ` +
			`client_secret=secret-form&token=token-form Authorization: Bearer raw-token`,
	)}).Error()
	for _, secret := range []string{"secret-json", "device-json", "auth-json", "secret-form", "token-form", "raw-token"} {
		if strings.Contains(err, secret) {
			t.Fatalf("HTTP error leaked %q: %s", secret, err)
		}
	}
}
