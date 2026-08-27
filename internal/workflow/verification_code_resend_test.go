package workflow

import (
	"testing"

	"github.com/mangobubu/gopay-autosms/internal/smsbower"
)

func TestProviderVerificationCodeAndWaitingClassification(t *testing.T) {
	tests := []struct {
		name        string
		status      smsbower.ActivationStatus
		wantCode    string
		wantCodeOK  bool
		wantWaiting bool
	}{
		{name: "ok code", status: smsbower.ActivationStatus{Kind: smsbower.StatusOK, Code: " 123456 "}, wantCode: "123456", wantCodeOK: true},
		{name: "wait retry code has priority", status: smsbower.ActivationStatus{Kind: smsbower.StatusWaitRetry, Code: " 234567 "}, wantCode: "234567", wantCodeOK: true, wantWaiting: true},
		{name: "wait resend code has priority", status: smsbower.ActivationStatus{Kind: smsbower.StatusWaitResend, Code: "345678"}, wantCode: "345678", wantCodeOK: true, wantWaiting: true},
		{name: "wait code without code", status: smsbower.ActivationStatus{Kind: smsbower.StatusWaitCode}, wantWaiting: true},
		{name: "wait retry without code", status: smsbower.ActivationStatus{Kind: smsbower.StatusWaitRetry}, wantWaiting: true},
		{name: "wait resend without code", status: smsbower.ActivationStatus{Kind: smsbower.StatusWaitResend}, wantWaiting: true},
		{name: "unknown remains waiting", status: smsbower.ActivationStatus{Kind: smsbower.StatusUnknown}, wantWaiting: true},
		{name: "cancel is not waiting", status: smsbower.ActivationStatus{Kind: smsbower.StatusCancel}},
		{name: "ok without code is not waiting", status: smsbower.ActivationStatus{Kind: smsbower.StatusOK}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, ok := providerVerificationCode(test.status)
			if code != test.wantCode || ok != test.wantCodeOK {
				t.Fatalf("providerVerificationCode(%+v) = %q, %v; want %q, %v", test.status, code, ok, test.wantCode, test.wantCodeOK)
			}
			if waiting := providerStillWaiting(test.status.Kind); waiting != test.wantWaiting {
				t.Fatalf("providerStillWaiting(%q) = %v, want %v", test.status.Kind, waiting, test.wantWaiting)
			}
		})
	}
}
