// Package smsprovider defines the stable provider identifiers persisted with
// batches and activations.
package smsprovider

import (
	"fmt"
	"strings"
)

const (
	SMSBower = "smsbower"
	HeroSMS  = "hero-sms"
)

// Normalize returns the canonical provider identifier. Empty values belong to
// pre-provider-selection batches and intentionally retain SMSBower behaviour.
func Normalize(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", SMSBower:
		return SMSBower, nil
	case HeroSMS:
		return HeroSMS, nil
	default:
		return "", fmt.Errorf("invalid sms_provider %q", value)
	}
}
