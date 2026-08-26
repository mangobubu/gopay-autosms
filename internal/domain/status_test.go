package domain

import "testing"

func TestNormalizePhoneAndFingerprint(t *testing.T) {
	normalized, err := NormalizePhone(" +62 (812) 345-678 ")
	if err != nil {
		t.Fatalf("NormalizePhone() error = %v", err)
	}
	if normalized != "+62812345678" {
		t.Fatalf("NormalizePhone() = %q", normalized)
	}
	if PhoneFingerprint(normalized) != PhoneFingerprint("+62812345678") {
		t.Fatal("fingerprint must be deterministic")
	}
	if PhoneFingerprint(normalized) == PhoneFingerprint("+62812345679") {
		t.Fatal("different phones unexpectedly shared a fingerprint")
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
		ActivationStatusPINRequired,
		ActivationStatusUnregistered,
		ActivationStatusZeroBalanceUsed,
	} {
		if status.Terminal() {
			t.Fatalf("%q must stay recoverable until remote finalization succeeds", status)
		}
	}
}
