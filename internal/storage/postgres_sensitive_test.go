package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mangobubu/gopay-autosms/internal/secure"
)

func TestSensitiveEnvelopesRoundTripAndBindIdentity(t *testing.T) {
	box, err := secure.New("storage-sensitive-test-key")
	if err != nil {
		t.Fatal(err)
	}
	phone := "+628123456789"
	payload := []byte("{\n  \"code\": \"123456\", \"phone\": \"+628123456789\"\n}")

	accountCiphertext, err := sealAccountSensitive(box, "phone-hash", phone)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(accountCiphertext, []byte(phone)) {
		t.Fatal("account ciphertext contains plaintext phone")
	}
	account, err := openAccountSensitive(box, accountCiphertext, "phone-hash")
	if err != nil || account.PhoneNumber != phone {
		t.Fatalf("account round trip = %+v, %v", account, err)
	}
	if _, err = openAccountSensitive(box, accountCiphertext, "other-hash"); err == nil {
		t.Fatal("account envelope accepted the wrong identity")
	}

	legacyFingerprint := legacyPhoneFingerprint(phone)
	activationCiphertext, err := sealActivationSensitive(
		box, "hero-sms", "activation-1", legacyFingerprint, phone, payload,
	)
	if err != nil {
		t.Fatal(err)
	}
	activation, err := openActivationSensitive(
		box, activationCiphertext, "hero-sms", "activation-1", legacyFingerprint,
	)
	if err != nil || activation.PhoneNumber != phone || !bytes.Equal(activation.ProviderPayload, payload) {
		t.Fatalf("activation round trip = %+v, %v", activation, err)
	}
	if _, err = openActivationSensitive(
		box, activationCiphertext, "hero-sms", "activation-2", legacyFingerprint,
	); err == nil {
		t.Fatal("activation envelope accepted the wrong provider identity")
	}
	if _, err = openActivationSensitive(
		box, activationCiphertext, "hero-sms", "activation-1", strings.Repeat("f", 64),
	); err == nil {
		t.Fatal("activation envelope accepted the wrong phone identity")
	}

	verificationCiphertext, err := sealVerificationSensitive(box, 7, 2, "123456", payload)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := openVerificationSensitive(box, verificationCiphertext, 7, 2)
	if err != nil || verification.Code != "123456" || !bytes.Equal(verification.ProviderPayload, payload) {
		t.Fatalf("verification round trip = %+v, %v", verification, err)
	}
	if bytes.Contains(verificationCiphertext, []byte("123456")) {
		t.Fatal("verification ciphertext contains plaintext OTP")
	}
	if _, err = openVerificationSensitive(box, verificationCiphertext, 7, 3); err == nil {
		t.Fatal("verification envelope accepted the wrong cycle")
	}
}

func TestHeroSMSWebhookSensitiveEnvelopePreservesNullableFieldsAndRawBytes(t *testing.T) {
	box, err := secure.New("webhook-sensitive-test-key")
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	raw := []byte("{\n  \"activationId\": \"42\", \"code\": null\n}")
	ciphertext, err := sealHeroSMSWebhookSensitive(box, "fingerprint", nil, &empty, "+621", raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, raw) || bytes.Contains(ciphertext, []byte("+621")) {
		t.Fatal("webhook ciphertext contains plaintext callback data")
	}
	envelope, err := openHeroSMSWebhookSensitive(box, ciphertext, "fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Code != nil || envelope.Text == nil || *envelope.Text != "" ||
		envelope.PhoneNumber != "+621" || !bytes.Equal(envelope.RawPayload, raw) {
		t.Fatalf("webhook envelope lost nullable or raw data: %+v", envelope)
	}
	wrongBox, err := secure.New("different-storage-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = openHeroSMSWebhookSensitive(wrongBox, ciphertext, "fingerprint"); err == nil {
		t.Fatal("webhook ciphertext opened with the wrong key")
	}
}

func TestSensitiveStorageMigrationsCoverOnlyLegacyWorkflowTables(t *testing.T) {
	var schema strings.Builder
	foundBlindIndexHook := false
	for _, migration := range migrations {
		if migration.version != 13 && migration.version != 14 && migration.version != 16 {
			continue
		}
		if migration.version == 16 && migration.apply != nil {
			foundBlindIndexHook = true
		}
		for _, statement := range migration.statements {
			schema.WriteString(statement)
			schema.WriteByte('\n')
		}
	}
	sql := schema.String()
	if !foundBlindIndexHook || !strings.Contains(sql, "DROP TABLE IF EXISTS phone_history") {
		t.Fatalf("sensitive storage migration is missing the blind-index upgrade: %s", sql)
	}
	for _, required := range []string{
		"storage_encryption_state", "accounts", "activations",
		"verification_codes", "hero_sms_webhook_events", "sensitive_enc bytea",
		"accounts_sensitive_ciphertext_chk", "activations_sensitive_ciphertext_chk",
		"verification_codes_sensitive_ciphertext_chk", "hero_sms_webhook_events_sensitive_ciphertext_chk",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("sensitive storage migration missing %q: %s", required, sql)
		}
	}
	for _, independentTable := range []string{"hero_sms_number_tasks", "hero_sms_number_messages"} {
		if strings.Contains(sql, independentTable) {
			t.Fatalf("legacy encryption migration changed independent table %q", independentTable)
		}
	}
}

func TestPhoneBlindIndexesAreKeyedAndPurposeSeparated(t *testing.T) {
	box, err := secure.New("phone-blind-index-test-key")
	if err != nil {
		t.Fatal(err)
	}
	phone := "+628123456789"
	accountIndex, err := accountPhoneBlindIndex(box, phone)
	if err != nil {
		t.Fatal(err)
	}
	activationIndex, err := activationPhoneBlindIndex(box, phone)
	if err != nil {
		t.Fatal(err)
	}
	if accountIndex == activationIndex {
		t.Fatal("account and activation phone indexes are not purpose-separated")
	}
	if accountIndex == legacyPhoneFingerprintForUnitTest(phone) ||
		activationIndex == legacyPhoneFingerprintForUnitTest(phone) {
		t.Fatal("phone blind index degraded to an enumerable SHA-256 digest")
	}
}

func legacyPhoneFingerprintForUnitTest(phone string) string {
	digest := sha256.Sum256([]byte(phone))
	return hex.EncodeToString(digest[:])
}

func TestSensitiveEnvelopeRejectsMalformedJSONPayload(t *testing.T) {
	box, err := secure.New("malformed-sensitive-test-key")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealEnvelope(box, activationSensitiveEnvelope{
		Version: sensitiveEnvelopeVersion, Kind: "activation", Provider: "hero-sms",
		ProviderActivationID: "1", ProviderPayload: []byte("not-json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = openActivationSensitive(box, ciphertext, "hero-sms", "1", ""); err == nil {
		t.Fatal("activation envelope accepted malformed provider JSON")
	}
	if !json.Valid([]byte(`{"ok":true}`)) {
		t.Fatal("test fixture JSON is invalid")
	}
}
