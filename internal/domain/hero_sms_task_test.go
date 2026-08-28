package domain

import "testing"

func TestHeroSMSNumberTaskStatuses(t *testing.T) {
	terminal := map[HeroSMSNumberTaskStatus]bool{
		HeroSMSTaskStopped:  true,
		HeroSMSTaskRefunded: true,
		HeroSMSTaskSettled:  true,
		HeroSMSTaskExpired:  true,
	}
	statuses := []HeroSMSNumberTaskStatus{
		HeroSMSTaskWaitingNumber, HeroSMSTaskPurchasing, HeroSMSTaskActive, HeroSMSTaskPurchaseUnknown,
		HeroSMSTaskSettling, HeroSMSTaskStopped, HeroSMSTaskRefunded,
		HeroSMSTaskSettled, HeroSMSTaskExpired,
	}
	for _, status := range statuses {
		if !status.Valid() {
			t.Fatalf("status %q is not valid", status)
		}
		if got := status.Terminal(); got != terminal[status] {
			t.Fatalf("status %q Terminal() = %v, want %v", status, got, terminal[status])
		}
	}
	if HeroSMSNumberTaskStatus("running").Valid() {
		t.Fatal("unrelated batch status unexpectedly valid for HeroSMS number task")
	}
}

func TestHeroSMSNumberTaskEnums(t *testing.T) {
	for _, product := range []HeroSMSProductKind{HeroSMSProductActivation, HeroSMSProductRent} {
		if !product.Valid() {
			t.Fatalf("product %q is not valid", product)
		}
	}
	for _, source := range []HeroSMSMessageSource{HeroSMSMessageWebhook, HeroSMSMessagePoll} {
		if !source.Valid() {
			t.Fatalf("message source %q is not valid", source)
		}
	}
	for _, status := range []HeroSMSRefundStatus{
		HeroSMSRefundUnknown, HeroSMSRefundRefundable, HeroSMSRefundRequested,
		HeroSMSRefunded, HeroSMSRefundUnavailable, HeroSMSRefundSettled,
	} {
		if !status.Valid() {
			t.Fatalf("refund status %q is not valid", status)
		}
	}
}
