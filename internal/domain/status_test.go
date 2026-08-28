package domain

import "testing"

func TestNormalizePhone(t *testing.T) {
	normalized, err := NormalizePhone(" +62 (812) 345-678 ")
	if err != nil {
		t.Fatalf("NormalizePhone() error = %v", err)
	}
	if normalized != "+62812345678" {
		t.Fatalf("NormalizePhone() = %q", normalized)
	}
}

func TestNormalizePhoneRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "123", "+12a3456", "++628123456"} {
		if _, err := NormalizePhone(value); err == nil {
			t.Fatalf("NormalizePhone(%q) unexpectedly succeeded", value)
		}
	}
}

func TestValidatePIN(t *testing.T) {
	if err := ValidatePIN("012345"); err != nil {
		t.Fatalf("ValidatePIN(valid) = %v", err)
	}
	for _, pin := range []string{"12345", "1234567", "12a456", "１２３４５６"} {
		if err := ValidatePIN(pin); err == nil {
			t.Fatalf("ValidatePIN(%q) unexpectedly succeeded", pin)
		}
	}
}

func TestTerminalStatuses(t *testing.T) {
	if ActivationStatusActive.Terminal() {
		t.Fatal("active activation is not terminal")
	}
	for _, status := range []ActivationStatus{
		ActivationStatusSuccess,
		ActivationStatusExpired,
		ActivationStatusCancelled,
		ActivationStatusFailed,
	} {
		if !status.Terminal() {
			t.Fatalf("%q should be terminal", status)
		}
	}
	for _, status := range []ActivationStatus{
		ActivationStatusDuplicate,
		ActivationStatusPhoneInUse,
		ActivationStatusPINRequired,
		ActivationStatusUnregistered,
		ActivationStatusLoginFailed,
		ActivationStatusLoginCodeTimeout,
		ActivationStatusZeroBalanceUsed,
		ActivationStatusSettingPIN,
		ActivationStatusPINSubmissionBlocked,
		ActivationStatusPINCodeTimeout,
		ActivationStatusPINChanged,
		ActivationStatusAwaitingSubsequentCode,
	} {
		if status.Terminal() {
			t.Fatalf("%q must stay recoverable until remote finalization succeeds", status)
		}
	}
}

func TestPINSubmissionBlockedStatusIsDurableAndNonTerminal(t *testing.T) {
	if !ActivationStatusPINSubmissionBlocked.Valid() {
		t.Fatal("pin_submission_blocked must be a valid activation status")
	}
	if ActivationStatusPINSubmissionBlocked.Terminal() {
		t.Fatal("pin_submission_blocked must remain non-terminal until provider completion is finalized")
	}
}

func TestPhoneInUseStatusIsValidAndNonTerminal(t *testing.T) {
	if !ActivationStatusPhoneInUse.Valid() {
		t.Fatal("phone_in_use must be a valid activation status")
	}
	if ActivationStatusPhoneInUse.Terminal() {
		t.Fatal("phone_in_use must remain non-terminal until provider cancellation is finalized")
	}
}

func TestLoginCodeTimeoutStatusIsValidAndNonTerminal(t *testing.T) {
	if !ActivationStatusLoginCodeTimeout.Valid() {
		t.Fatal("login_code_timeout must be a valid activation status")
	}
	if ActivationStatusLoginCodeTimeout.Terminal() {
		t.Fatal("login_code_timeout must remain non-terminal until provider cancellation is finalized")
	}
}

func TestPINCodeTimeoutStatusIsValidAndNonTerminal(t *testing.T) {
	if !ActivationStatusPINCodeTimeout.Valid() {
		t.Fatal("pin_code_timeout must be a valid activation status")
	}
	if ActivationStatusPINCodeTimeout.Terminal() {
		t.Fatal("pin_code_timeout must remain non-terminal until provider cancellation is finalized")
	}
}

func TestPINWorkflowStatusesAreValid(t *testing.T) {
	for _, status := range []ActivationStatus{
		ActivationStatusSettingPIN,
		ActivationStatusPINChanged,
		ActivationStatusAwaitingSubsequentCode,
	} {
		if !status.Valid() {
			t.Fatalf("%q should be valid", status)
		}
	}
}

func TestHeroSMSWebhookEventStatusesAreValid(t *testing.T) {
	for _, status := range []HeroSMSWebhookEventStatus{
		HeroSMSWebhookEventReceived,
		HeroSMSWebhookEventProcessing,
		HeroSMSWebhookEventProcessed,
		HeroSMSWebhookEventIgnored,
	} {
		if !status.Valid() {
			t.Fatalf("%q should be a valid HeroSMS webhook event status", status)
		}
	}
	if HeroSMSWebhookEventStatus("failed").Valid() {
		t.Fatal("unknown HeroSMS webhook event status unexpectedly valid")
	}
}
