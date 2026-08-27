package gopay

import (
	"testing"
	"time"
)

func TestSessionJSONRestoresVerificationCodeResendState(t *testing.T) {
	loginSentAt := time.Date(2026, time.August, 27, 9, 10, 11, 123456789, time.UTC)
	pinSentAt := time.Date(2026, time.August, 27, 10, 20, 21, 987654321, time.UTC)
	want := Session{
		Phone:                      "81234567890",
		SMSCycle:                   4,
		VerificationCycleRequest:   VerificationCycleRequestAccepted,
		LoginStage:                 LoginStageReady2FA,
		LoginCodeSentAt:            loginSentAt,
		LoginCodeResends:           2,
		LoginCodeDispatchUncertain: true,
		PINStage:                   PINStageResetReadyCycle,
		PINCodeSentAt:              pinSentAt,
		PINCodeResends:             3,
		PINCodeDispatchUncertain:   true,
	}

	raw, err := want.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSession(raw)
	if err != nil {
		t.Fatal(err)
	}

	if got.LoginStage != want.LoginStage || !got.LoginCodeSentAt.Equal(loginSentAt) || got.LoginCodeResends != want.LoginCodeResends || !got.LoginCodeDispatchUncertain {
		t.Fatalf("restored login resend state = stage %q, sent %s, count %d, uncertain %t", got.LoginStage, got.LoginCodeSentAt, got.LoginCodeResends, got.LoginCodeDispatchUncertain)
	}
	if got.VerificationCycleRequest != VerificationCycleRequestAccepted {
		t.Fatalf("restored provider cycle request state = %q", got.VerificationCycleRequest)
	}
	if got.PINStage != want.PINStage || !got.PINCodeSentAt.Equal(pinSentAt) || got.PINCodeResends != want.PINCodeResends || !got.PINCodeDispatchUncertain {
		t.Fatalf("restored PIN resend state = stage %q, sent %s, count %d, uncertain %t", got.PINStage, got.PINCodeSentAt, got.PINCodeResends, got.PINCodeDispatchUncertain)
	}
}

func TestParseLegacySessionDefaultsVerificationCodeResendState(t *testing.T) {
	got, err := ParseSession([]byte(`{"phone":"81234567890","login_stage":"awaiting_login_1fa_otp","pin_stage":"awaiting_pin_otp"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !got.LoginCodeSentAt.IsZero() || got.LoginCodeResends != 0 || got.LoginCodeDispatchUncertain || got.VerificationCycleRequest != VerificationCycleRequestNone {
		t.Fatalf("legacy login resend state = sent %s, count %d; want zero values", got.LoginCodeSentAt, got.LoginCodeResends)
	}
	if !got.PINCodeSentAt.IsZero() || got.PINCodeResends != 0 || got.PINCodeDispatchUncertain {
		t.Fatalf("legacy PIN resend state = sent %s, count %d; want zero values", got.PINCodeSentAt, got.PINCodeResends)
	}
}
