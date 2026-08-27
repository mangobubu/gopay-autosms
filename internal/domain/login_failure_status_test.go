package domain

import "testing"

func TestLoginFailedStatusIsValidAndFinalizedByProviderAction(t *testing.T) {
	if !ActivationStatusLoginFailed.Valid() {
		t.Fatal("login_failed must be a valid activation status")
	}
	if ActivationStatusLoginFailed.Terminal() {
		t.Fatal("login_failed must remain non-terminal until the provider cancellation is finalized")
	}
}
