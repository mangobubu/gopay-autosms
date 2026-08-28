package storage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/secure"
)

var _ HeroSMSWebhookStore = (*PostgresStore)(nil)

func TestHeroSMSWebhookFingerprintUsesStableNormalizedFields(t *testing.T) {
	box, err := secure.New("webhook-fingerprint-test-key")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := func(params IngestHeroSMSWebhookParams) string {
		t.Helper()
		value, fingerprintErr := heroSMSWebhookFingerprint(box, params)
		if fingerprintErr != nil {
			t.Fatal(fingerprintErr)
		}
		return value
	}
	receivedAt := time.Date(2026, time.August, 28, 5, 4, 3, 1200, time.FixedZone("UTC+7", 7*60*60))
	code := " 123456 "
	messageText := " code 123456 "
	first, err := normalizeHeroSMSWebhookParams(IngestHeroSMSWebhookParams{
		ProviderActivationID: " activation-42 ",
		Code:                 &code,
		Text:                 &messageText,
		PhoneNumber:          " +6281234 ",
		ServiceCode:          " go ",
		CountryCode:          " 6 ",
		ProviderReceivedAt:   &receivedAt,
		RawPayload:           json.RawMessage(`{"code":"123456","activationId":"activation-42"}`),
	})
	if err != nil {
		t.Fatalf("normalize first payload: %v", err)
	}
	second := first
	second.RawPayload = json.RawMessage("{\n  \"activationId\": \"activation-42\", \"code\": \"123456\"\n}")
	if got, want := fingerprint(first), fingerprint(second); got != want {
		t.Fatalf("equivalent provider event fingerprints differ: %q != %q", got, want)
	}
	changed := first
	changedCode := "654321"
	changed.Code = &changedCode
	if fingerprint(first) == fingerprint(changed) {
		t.Fatal("different callback fields unexpectedly shared a fingerprint")
	}
	if first.ProviderReceivedAt == nil || first.ProviderReceivedAt.Location() != time.UTC {
		t.Fatalf("provider timestamp was not normalized to UTC: %v", first.ProviderReceivedAt)
	}
	empty := ""
	nullCode := first
	nullCode.Code = nil
	emptyCode := first
	emptyCode.Code = &empty
	if fingerprint(nullCode) == fingerprint(emptyCode) {
		t.Fatal("null and empty webhook codes unexpectedly shared a fingerprint")
	}
	nullText := first
	nullText.Text = nil
	emptyText := first
	emptyText.Text = &empty
	if fingerprint(nullText) == fingerprint(emptyText) {
		t.Fatal("null and empty webhook text unexpectedly shared a fingerprint")
	}
	otherBox, err := secure.New("different-webhook-fingerprint-test-key")
	if err != nil {
		t.Fatal(err)
	}
	otherFingerprint, err := heroSMSWebhookFingerprint(otherBox, first)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint(first) == otherFingerprint {
		t.Fatal("different storage keys shared a webhook blind index")
	}
}

func TestNormalizeHeroSMSWebhookParamsRejectsMissingIdentityAndInvalidJSON(t *testing.T) {
	for _, params := range []IngestHeroSMSWebhookParams{
		{ProviderActivationID: "", RawPayload: json.RawMessage(`{}`)},
		{ProviderActivationID: "activation-1", RawPayload: json.RawMessage(`not-json`)},
		{ProviderActivationID: "activation-1"},
	} {
		if _, err := normalizeHeroSMSWebhookParams(params); err != ErrInvalidInput {
			t.Fatalf("normalize params error = %v, want ErrInvalidInput", err)
		}
	}
}

func TestHeroSMSWebhookMigrationProvidesPermanentIdempotentInbox(t *testing.T) {
	var migrationSQL strings.Builder
	for _, migration := range migrations {
		if migration.version != 10 {
			continue
		}
		for _, statement := range migration.statements {
			migrationSQL.WriteString(statement)
			migrationSQL.WriteByte('\n')
		}
	}
	schema := migrationSQL.String()
	for _, required := range []string{
		"webhook_wakeup_at timestamptz",
		"CREATE TABLE IF NOT EXISTS hero_sms_webhook_events",
		"activation_id bigint REFERENCES activations(id) ON DELETE SET NULL",
		"provider_activation_id text NOT NULL",
		"code text,",
		"message_text text,",
		"phone_number text NOT NULL DEFAULT ''",
		"service_code text NOT NULL DEFAULT ''",
		"country_code text NOT NULL DEFAULT ''",
		"provider_received_at timestamptz",
		"raw_payload json NOT NULL",
		"payload_fingerprint char(64) NOT NULL UNIQUE",
		"status IN ('received','processing','processed','ignored')",
		"attempts integer NOT NULL DEFAULT 0",
		"last_error text NOT NULL DEFAULT ''",
		"next_attempt_at timestamptz NOT NULL DEFAULT now()",
		"received_at timestamptz NOT NULL DEFAULT now()",
		"processed_at timestamptz",
		"hero_sms_webhook_events_activation_claim_idx",
		"hero_sms_webhook_events_processing_idx",
		"WHERE activation_id IS NULL",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("HeroSMS webhook migration missing %q: %s", required, schema)
		}
	}
	for _, destructive := range []string{"ON DELETE CASCADE", "DELETE FROM hero_sms_webhook_events"} {
		if strings.Contains(schema, destructive) {
			t.Fatalf("webhook audit migration contains destructive behavior %q: %s", destructive, schema)
		}
	}
	if strings.Contains(schema, "//") {
		t.Fatalf("HeroSMS webhook migration contains a non-SQL comment: %s", schema)
	}
}

func TestHeroSMSWebhookClaimIsOrderedIdempotentAndLeaseFenced(t *testing.T) {
	for _, required := range []string{
		"provider='hero-sms'",
		"lease_owner=$2",
		"lease_version=$3",
		"lease_until > $4",
		"finished_at IS NULL",
		"FOR UPDATE",
	} {
		if !strings.Contains(ownedHeroSMSWebhookLeaseSQL, required) {
			t.Fatalf("owned HeroSMS lease SQL missing %q: %s", required, ownedHeroSMSWebhookLeaseSQL)
		}
	}
	for _, required := range []string{
		"status='received' AND next_attempt_at <= $4",
		"status='processing'",
		"claimed_lease_owner<>$2 OR claimed_lease_version<>$3",
		"provider_received_at NULLS LAST, received_at, id",
		"FOR UPDATE SKIP LOCKED",
		"status='processing'",
		"attempts=event.attempts+1",
		"claimed_lease_owner=$2",
		"claimed_lease_version=$3",
	} {
		if !strings.Contains(claimNextHeroSMSWebhookEventSQL, required) {
			t.Fatalf("HeroSMS event claim SQL missing %q: %s", required, claimNextHeroSMSWebhookEventSQL)
		}
	}
	for _, required := range []string{
		"ON CONFLICT(payload_fingerprint) DO NOTHING",
		"CASE WHEN $8::boolean THEN 'ignored' ELSE 'received' END",
		"sensitive_enc",
		heroSMSWebhookActivationFinishedReason,
	} {
		if !strings.Contains(heroSMSWebhookInsertSQL, required) {
			t.Fatalf("HeroSMS ingestion SQL missing %q: %s", required, heroSMSWebhookInsertSQL)
		}
	}
	for _, required := range []string{
		"status='ignored'",
		"last_error='" + heroSMSWebhookActivationFinishedReason + "'",
		"processed_at=now()",
		"status IN ('received','processing')",
	} {
		if !strings.Contains(ignoreOpenHeroSMSWebhookEventsSQL, required) {
			t.Fatalf("finished activation leaves an unclaimable webhook event %q: %s", required, ignoreOpenHeroSMSWebhookEventsSQL)
		}
	}
}

func TestHeroSMSWebhookWakeSurvivesStaleWorkerRelease(t *testing.T) {
	for _, required := range []string{
		"lease_owner<>''",
		"LEAST(COALESCE(webhook_wakeup_at,$2),$2)",
		"WHERE id=$1 AND finished_at IS NULL",
	} {
		if !strings.Contains(wakeHeroSMSActivationSQL, required) {
			t.Fatalf("HeroSMS wake SQL missing %q: %s", required, wakeHeroSMSActivationSQL)
		}
	}
	for _, required := range []string{
		"next_run_at=LEAST($3,COALESCE(webhook_wakeup_at,$3))",
		"webhook_wakeup_at=NULL",
		"WHERE id=$1 AND lease_owner=$2",
	} {
		if !strings.Contains(releaseActivationLeaseSQL, required) {
			t.Fatalf("activation lease release loses webhook wakeup %q: %s", required, releaseActivationLeaseSQL)
		}
	}
	if strings.Contains(releaseActivationLeaseSQL, "next_run_at=LEAST(next_run_at") {
		t.Fatalf("lease release would preserve an already-due schedule and busy-loop: %s", releaseActivationLeaseSQL)
	}
	for _, required := range []string{
		"webhook_wakeup_at=(SELECT min(next_attempt_at)",
		"activation_id=$1 AND status='received'",
		"lease_owner=$2 AND lease_version=$3",
		"finished_at IS NULL",
	} {
		if !strings.Contains(refreshHeroSMSWebhookWakeSQL, required) {
			t.Fatalf("claimed event wakeup is not consumed safely %q: %s", required, refreshHeroSMSWebhookWakeSQL)
		}
	}
	for _, required := range []string{
		"next_run_at <= $2",
		"webhook_wakeup_at <= $2",
		"webhook_event.activation_id=activations.id",
		"webhook_event.status='processing'",
	} {
		if !strings.Contains(runnableActivationDueSQL, required) {
			t.Fatalf("expired worker could strand a durable webhook wakeup %q: %s", required, runnableActivationDueSQL)
		}
	}
	if !strings.Contains(runnableActivationOrderSQL, "LEAST(next_run_at,COALESCE(webhook_wakeup_at,next_run_at))") {
		t.Fatalf("runnable activation ordering ignores webhook wakeup: %s", runnableActivationOrderSQL)
	}
}
