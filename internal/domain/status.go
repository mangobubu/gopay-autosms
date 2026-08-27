package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
)

var (
	ErrInvalidPhone = errors.New("invalid phone number")
	ErrInvalidPIN   = errors.New("PIN must contain exactly 6 digits")
)

func (s BatchStatus) Valid() bool {
	switch s {
	case BatchStatusPending, BatchStatusRunning, BatchStatusCompleted, BatchStatusCancelled, BatchStatusFailed:
		return true
	default:
		return false
	}
}

func (s BatchStatus) Terminal() bool {
	return s == BatchStatusCompleted || s == BatchStatusCancelled || s == BatchStatusFailed
}

func (s ActivationStatus) Valid() bool {
	switch s {
	case ActivationStatusPurchased, ActivationStatusDuplicate, ActivationStatusAwaitingLoginCode,
		ActivationStatusLoggingIn, ActivationStatusPINRequired, ActivationStatusUnregistered,
		ActivationStatusLoginFailed, ActivationStatusLoginCodeTimeout,
		ActivationStatusCheckingBalance, ActivationStatusZeroBalanceUsed, ActivationStatusSettingPIN,
		ActivationStatusPINSubmissionBlocked,
		ActivationStatusAwaitingPINCode, ActivationStatusPINCodeTimeout,
		ActivationStatusPINChanged, ActivationStatusAwaitingSubsequentCode,
		ActivationStatusActive, ActivationStatusSuccess,
		ActivationStatusExpired, ActivationStatusCancelled, ActivationStatusFailed:
		return true
	default:
		return false
	}
}

func (s ActivationStatus) Terminal() bool {
	switch s {
	case ActivationStatusSuccess, ActivationStatusExpired, ActivationStatusCancelled, ActivationStatusFailed:
		return true
	default:
		return false
	}
}

func (p VerificationPhase) Valid() bool {
	return p == VerificationPhaseLogin || p == VerificationPhasePIN || p == VerificationPhaseSubsequent
}

func (s AccountStatus) Valid() bool {
	switch s {
	case AccountStatusPending, AccountStatusAuthenticated, AccountStatusPINPending, AccountStatusActive, AccountStatusDisabled, AccountStatusError:
		return true
	default:
		return false
	}
}

func (a ControlAction) Valid() bool {
	return a == ControlActionNone || a == ControlActionSuccess || a == ControlActionDelete
}

// NormalizePhone converts common display formats to a stable E.164-like value.
// It deliberately does not infer a country prefix: callers must provide the
// full number returned by the SMS provider.
func NormalizePhone(phone string) (string, error) {
	phone = strings.TrimSpace(phone)
	var b strings.Builder
	for i, r := range phone {
		switch {
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case unicode.IsSpace(r), r == '-', r == '(', r == ')', r == '.':
			continue
		default:
			return "", ErrInvalidPhone
		}
	}
	normalized := b.String()
	digits := strings.TrimPrefix(normalized, "+")
	if len(digits) < 6 || len(digits) > 20 {
		return "", ErrInvalidPhone
	}
	if normalized[0] != '+' {
		normalized = "+" + normalized
	}
	return normalized, nil
}

// PhoneFingerprint avoids keeping an extra clear-text unique index while still
// providing deterministic, atomic history de-duplication.
func PhoneFingerprint(normalizedPhone string) string {
	sum := sha256.Sum256([]byte(normalizedPhone))
	return hex.EncodeToString(sum[:])
}

func ValidatePIN(pin string) error {
	if len(pin) != 6 {
		return ErrInvalidPIN
	}
	for _, r := range pin {
		if r < '0' || r > '9' {
			return ErrInvalidPIN
		}
	}
	return nil
}
