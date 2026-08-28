package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mangobubu/gopay-autosms/internal/secure"
)

func TestPostgresSensitiveUpgradeFromV12(t *testing.T) {
	databaseURL := os.Getenv("AUTOSMS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AUTOSMS_TEST_DATABASE_URL is not set")
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(poolConfig.ConnConfig.Database, "autosms_upgrade_test") {
		t.Fatalf("refusing to reset non-test database %q", poolConfig.ConnConfig.Database)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	})

	applyMigrationsThrough(t, ctx, pool, 12)
	const (
		phone               = "+628123456789"
		providerPayload     = "{\n  \"activationId\": \"provider-42\", \"phone\": \"+628123456789\"\n}"
		verificationPayload = "{\"source\":\"hero-sms\",\"code\":\"123456\"}"
		webhookRawPayload   = "{\n  \"activationId\": \"provider-42\", \"code\": \"123456\", \"receivedAt\": \"2026-08-28T12:30:56.123456789Z\"\n}"
	)
	webhookReceivedAt := time.Date(2026, 8, 28, 12, 30, 56, 123456789, time.UTC)
	webhookCode, webhookText := "123456", "Your code is 123456"
	webhookParams := IngestHeroSMSWebhookParams{
		ProviderActivationID: "provider-42",
		Code:                 &webhookCode,
		Text:                 &webhookText,
		PhoneNumber:          phone,
		ServiceCode:          "go",
		CountryCode:          "6",
		ProviderReceivedAt:   &webhookReceivedAt,
		RawPayload:           json.RawMessage(webhookRawPayload),
	}
	webhookFingerprint := hex.EncodeToString(heroSMSWebhookFingerprintMaterial(webhookParams))
	box, err := secure.New("postgres-upgrade-test-key")
	if err != nil {
		t.Fatal(err)
	}
	legacyPIN, err := box.Seal([]byte("123456"))
	if err != nil {
		t.Fatal(err)
	}
	legacySession, err := box.Seal([]byte(`{"access_token":"legacy-token"}`))
	if err != nil {
		t.Fatal(err)
	}
	legacyProxy, err := box.Seal([]byte("http://user:pass@127.0.0.1:8080"))
	if err != nil {
		t.Fatal(err)
	}
	legacyAPIKey, err := box.Seal([]byte("legacy-provider-key"))
	if err != nil {
		t.Fatal(err)
	}
	legacyDraft, err := box.Seal([]byte(`{"pin":"123456","proxy":"proxy.example:8080"}`))
	if err != nil {
		t.Fatal(err)
	}
	batchConfig, err := json.Marshal(map[string]any{
		"proxy_pool": []map[string]any{{
			"id": 1, "encrypted": base64.StdEncoding.EncodeToString(legacyProxy), "status": "available",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var batchID, accountID, activationID int64
	err = pool.QueryRow(ctx, `INSERT INTO batches(
		service_code,service_name,country_code,country_name,max_price_amount,currency,
		target_pin_enc,config,quantity,purchase_protocol_version
	) VALUES('go','GoPay','6','Indonesia',1.25,'USD',$1,$2::jsonb,1,1) RETURNING id`,
		legacyPIN, batchConfig).Scan(&batchID)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO accounts(
		phone_number,phone_fingerprint,status,credentials_enc,target_pin_enc,token_state,device_state,metadata
	) VALUES($1,$2,'active',$3,$4,'{}','{}','{}') RETURNING id`,
		phone, legacyPhoneFingerprint(phone), legacySession, legacyPIN).Scan(&accountID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES
		('hero-sms',jsonb_build_object('api_key_encrypted',$1::text)),
		('batch-draft',jsonb_build_object('draft_encrypted',$2::text))`,
		base64.StdEncoding.EncodeToString(legacyAPIKey), base64.StdEncoding.EncodeToString(legacyDraft)); err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO activations(
		batch_id,account_id,provider,provider_activation_id,phone_number,phone_fingerprint,
		service_code,country_code,purchase_price_amount,currency,status,provider_payload,next_run_at
	) VALUES($1,$2,'hero-sms','provider-42',$3,$4,'go','6',0.75,'USD','awaiting_login_code',$5::jsonb,now())
	RETURNING id`, batchID, accountID, phone, legacyPhoneFingerprint(phone), providerPayload).Scan(&activationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO verification_codes(
		activation_id,cycle_no,phase,ordinal,code,provider_payload
	) VALUES($1,0,'login',0,'123456',$2::jsonb)`, activationID, verificationPayload); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO hero_sms_webhook_events(
		activation_id,provider_activation_id,code,message_text,phone_number,service_code,
		country_code,provider_received_at,raw_payload,payload_fingerprint
	) VALUES($1,'provider-42','123456','Your code is 123456',$2,'go','6',$3,$4::json,$5)`,
		activationID, phone, webhookReceivedAt, webhookRawPayload, webhookFingerprint); err != nil {
		t.Fatal(err)
	}

	wrongBox, err := secure.New("wrong-postgres-upgrade-key")
	if err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool, wrongBox); err == nil ||
		!strings.Contains(err.Error(), "storage encryption key does not match") {
		t.Fatalf("first upgrade accepted wrong key: %v", err)
	}
	assertWrongKeyUpgradeRolledBack(t, ctx, pool, batchID, accountID, legacyPIN, legacySession)

	// Simulate an interrupted development upgrade: the sentinel and v13 columns
	// exist, but schema_migrations does not record v13 yet. A valid sentinel must
	// not hide a single older ciphertext encrypted with another key.
	applyMigrationStatementsWithoutRecording(t, ctx, pool, 13)
	keyCheck, err := box.Seal([]byte(storageKeyCheckPlaintext))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO storage_encryption_state(id,key_check) VALUES(1,$1)`, keyCheck); err != nil {
		t.Fatal(err)
	}
	mixedAPIKey, err := wrongBox.Seal([]byte("mixed-key-provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE settings SET value=jsonb_build_object(
		'api_key_encrypted',$2::text) WHERE key=$1`, "hero-sms", base64.StdEncoding.EncodeToString(mixedAPIKey)); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool, box); err == nil ||
		!strings.Contains(err.Error(), "storage encryption key does not match") {
		t.Fatalf("partial v13 state hid a mixed-key row: %v", err)
	}
	assertMigration13UnrecordedAndPlaintextPreserved(t, ctx, pool, accountID, phone)
	if _, err = pool.Exec(ctx, `UPDATE settings SET value=jsonb_build_object(
		'api_key_encrypted',$2::text) WHERE key=$1`, "hero-sms", base64.StdEncoding.EncodeToString(legacyAPIKey)); err != nil {
		t.Fatal(err)
	}

	if err = Migrate(ctx, pool, box); err != nil {
		t.Fatalf("upgrade v12 to latest: %v", err)
	}
	store := &PostgresStore{pool: pool, protector: box}
	account, err := store.GetAccountByPhone(ctx, phone)
	if err != nil || account.PhoneNumber != phone {
		t.Fatalf("upgraded account = %+v, %v", account, err)
	}
	activation, err := store.GetActivation(ctx, activationID)
	if err != nil || activation.PhoneNumber != phone ||
		!jsonSemanticallyEqual(activation.ProviderPayload, []byte(providerPayload)) {
		t.Fatalf("upgraded activation = %+v, %v", activation, err)
	}
	verifications, err := store.ListVerificationCodes(ctx, activationID)
	if err != nil || len(verifications) != 1 || verifications[0].Code != "123456" ||
		!jsonSemanticallyEqual(verifications[0].ProviderPayload, []byte(verificationPayload)) {
		t.Fatalf("upgraded verifications = %+v, %v", verifications, err)
	}
	storedWebhookFingerprint, err := blindIndexLegacyDigest(box, heroSMSWebhookIndexPurpose, webhookFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.scanHeroSMSWebhookEvent(pool.QueryRow(ctx,
		`SELECT `+heroSMSWebhookEventColumns+` FROM hero_sms_webhook_events WHERE payload_fingerprint=$1`,
		storedWebhookFingerprint,
	))
	if err != nil || event.Code == nil || *event.Code != "123456" ||
		event.Text == nil || *event.Text != "Your code is 123456" ||
		event.PhoneNumber != phone || !bytes.Equal(event.RawPayload, []byte(webhookRawPayload)) {
		t.Fatalf("upgraded webhook = %+v, %v", event, err)
	}
	if event.ProviderReceivedAt == nil || event.ProviderReceivedAt.Equal(webhookReceivedAt) {
		t.Fatalf("PostgreSQL fixture did not exercise sub-microsecond timestamp loss: %v", event.ProviderReceivedAt)
	}
	retryParams := webhookParams
	retryParams.RawPayload = json.RawMessage(`{"code":"123456","activationId":"provider-42"}`)
	retry, err := store.IngestHeroSMSWebhook(ctx, retryParams)
	if err != nil || retry.Inserted || retry.Event.ID != event.ID {
		t.Fatalf("upgraded webhook retry = %+v, %v", retry, err)
	}
	activations, err := store.ListActivations(ctx, ActivationFilter{
		PhoneExact: phone, IncludeHidden: true, Page: Page{Limit: 10},
	})
	if err != nil || len(activations) != 1 || activations[0].ID != activationID {
		t.Fatalf("activation exact-phone lookup = %+v, %v", activations, err)
	}

	assertLegacySensitiveColumnsCleared(t, ctx, pool, accountID, activationID, storedWebhookFingerprint)
	assertSensitiveBlindIndexes(t, ctx, pool, box, phone, accountID, activationID, webhookFingerprint)
	if err = Migrate(ctx, pool, box); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
	if err = Migrate(ctx, pool, wrongBox); err == nil ||
		!strings.Contains(err.Error(), "storage encryption key does not match") {
		t.Fatalf("wrong-key migration error = %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO accounts(
		phone_number,phone_fingerprint,status,token_state,device_state,metadata
	) VALUES('+621','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
		'active','{}','{}','{}')`); err == nil {
		t.Fatal("plaintext account insert bypassed ciphertext constraint")
	}
}

func applyMigrationStatementsWithoutRecording(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	version int64,
) {
	t.Helper()
	for _, migration := range migrations {
		if migration.version != version {
			continue
		}
		for _, statement := range migration.statements {
			if _, err := pool.Exec(ctx, statement); err != nil {
				t.Fatalf("apply unrecorded migration %d statement: %v", version, err)
			}
		}
		return
	}
	t.Fatalf("migration %d not found", version)
}

func assertMigration13UnrecordedAndPlaintextPreserved(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID int64,
	wantPhone string,
) {
	t.Helper()
	var applied bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM schema_migrations WHERE version=13
	)`).Scan(&applied); err != nil || applied {
		t.Fatalf("mixed-key migration version persisted: applied=%v err=%v", applied, err)
	}
	var phone string
	var sensitive []byte
	if err := pool.QueryRow(ctx, `SELECT phone_number,sensitive_enc FROM accounts WHERE id=$1`, accountID).
		Scan(&phone, &sensitive); err != nil || phone != wantPhone || len(sensitive) != 0 {
		t.Fatalf("mixed-key migration changed account: phone=%q sensitive=%d err=%v", phone, len(sensitive), err)
	}
}

func assertSensitiveBlindIndexes(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	protector Protector,
	phone string,
	accountID, activationID int64,
	legacyWebhookFingerprint string,
) {
	t.Helper()
	wantAccount, err := accountPhoneBlindIndex(protector, phone)
	if err != nil {
		t.Fatal(err)
	}
	wantActivation, err := activationPhoneBlindIndex(protector, phone)
	if err != nil {
		t.Fatal(err)
	}
	wantWebhook, err := blindIndexLegacyDigest(protector, heroSMSWebhookIndexPurpose, legacyWebhookFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	var gotAccount, gotActivation, gotWebhook string
	if err = pool.QueryRow(ctx, `SELECT phone_fingerprint FROM accounts WHERE id=$1`, accountID).Scan(&gotAccount); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT phone_fingerprint FROM activations WHERE id=$1`, activationID).Scan(&gotActivation); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT payload_fingerprint FROM hero_sms_webhook_events
		WHERE activation_id=$1`, activationID).Scan(&gotWebhook); err != nil {
		t.Fatal(err)
	}
	if gotAccount != wantAccount || gotActivation != wantActivation || gotWebhook != wantWebhook {
		t.Fatalf("blind indexes = account %q activation %q webhook %q", gotAccount, gotActivation, gotWebhook)
	}
	if gotAccount == legacyPhoneFingerprint(phone) || gotActivation == legacyPhoneFingerprint(phone) ||
		gotWebhook == legacyWebhookFingerprint {
		t.Fatal("migration left an enumerable unkeyed fingerprint in PostgreSQL")
	}
}

func assertWrongKeyUpgradeRolledBack(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	batchID, accountID int64,
	wantPIN, wantSession []byte,
) {
	t.Helper()
	var migrationApplied, sensitiveColumnExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM schema_migrations WHERE version=13
	)`).Scan(&migrationApplied); err != nil || migrationApplied {
		t.Fatalf("wrong-key migration version persisted: applied=%v err=%v", migrationApplied, err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM information_schema.columns
		WHERE table_schema='public' AND table_name='accounts' AND column_name='sensitive_enc'
	)`).Scan(&sensitiveColumnExists); err != nil || sensitiveColumnExists {
		t.Fatalf("wrong-key migration DDL persisted: column=%v err=%v", sensitiveColumnExists, err)
	}
	var gotPIN, gotSession []byte
	if err := pool.QueryRow(ctx, `SELECT target_pin_enc FROM batches WHERE id=$1`, batchID).Scan(&gotPIN); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT credentials_enc FROM accounts WHERE id=$1`, accountID).Scan(&gotSession); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPIN, wantPIN) || !bytes.Equal(gotSession, wantSession) {
		t.Fatal("wrong-key migration changed legacy ciphertext despite rollback")
	}
}

func jsonSemanticallyEqual(first, second []byte) bool {
	var firstValue, secondValue any
	if json.Unmarshal(first, &firstValue) != nil || json.Unmarshal(second, &secondValue) != nil {
		return false
	}
	return reflect.DeepEqual(firstValue, secondValue)
}

func applyMigrationsThrough(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version int64) {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `CREATE TABLE schema_migrations(
		version bigint PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version > version {
			break
		}
		for _, statement := range migration.statements {
			if _, err = tx.Exec(ctx, statement); err != nil {
				t.Fatalf("apply fixture migration %d: %v", migration.version, err)
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, migration.version); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertLegacySensitiveColumnsCleared(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID, activationID int64,
	webhookFingerprint string,
) {
	t.Helper()
	var phone string
	var ciphertext []byte
	if err := pool.QueryRow(ctx, `SELECT phone_number,sensitive_enc FROM accounts WHERE id=$1`, accountID).
		Scan(&phone, &ciphertext); err != nil || phone != "" || len(ciphertext) == 0 {
		t.Fatalf("account legacy columns not cleared: phone=%q ciphertext=%d err=%v", phone, len(ciphertext), err)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT phone_number,provider_payload,sensitive_enc FROM activations WHERE id=$1`, activationID).
		Scan(&phone, &payload, &ciphertext); err != nil || phone != "" || string(payload) != "{}" || len(ciphertext) == 0 {
		t.Fatalf("activation legacy columns not cleared: phone=%q payload=%q ciphertext=%d err=%v", phone, payload, len(ciphertext), err)
	}
	var code string
	if err := pool.QueryRow(ctx, `SELECT code,provider_payload,sensitive_enc FROM verification_codes WHERE activation_id=$1`, activationID).
		Scan(&code, &payload, &ciphertext); err != nil || code != "" || string(payload) != "{}" || len(ciphertext) == 0 {
		t.Fatalf("verification legacy columns not cleared: code=%q payload=%q ciphertext=%d err=%v", code, payload, len(ciphertext), err)
	}
	var codeNull, textNull bool
	if err := pool.QueryRow(ctx, `SELECT code IS NULL,message_text IS NULL,phone_number,raw_payload,sensitive_enc
		FROM hero_sms_webhook_events WHERE payload_fingerprint=$1`, webhookFingerprint).
		Scan(&codeNull, &textNull, &phone, &payload, &ciphertext); err != nil || !codeNull || !textNull ||
		phone != "" || string(payload) != "{}" || len(ciphertext) == 0 {
		t.Fatalf("webhook legacy columns not cleared: code_null=%v text_null=%v phone=%q payload=%q ciphertext=%d err=%v",
			codeNull, textNull, phone, payload, len(ciphertext), err)
	}
}
