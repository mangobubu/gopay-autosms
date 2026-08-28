package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mangobubu/gopay-autosms/internal/domain"
)

const (
	sensitiveEnvelopeVersion      = 1
	storageKeyCheckPlaintext      = "autosms-postgres-sensitive-v1"
	accountPhoneBlindIndexPurpose = "account-phone/v1"
	activationPhoneIndexPurpose   = "activation-phone/v1"
	accountSessionLockPurpose     = "account-session-lock/v1"
	heroSMSWebhookIndexPurpose    = "legacy-herosms-webhook/v1"
	blindIndexEncodedLength       = 64
)

// Protector is the authenticated-encryption boundary required by PostgreSQL
// persistence. secure.Box satisfies it without coupling storage to one cipher.
type Protector interface {
	Seal([]byte) ([]byte, error)
	Open([]byte) ([]byte, error)
}

// BlindIndexer is an optional capability kept separate from Protector so the
// authenticated-encryption contract remains compatible with earlier releases.
// PostgreSQL requires both capabilities before it accepts persisted data.
type BlindIndexer interface {
	BlindIndex(purpose string, value []byte) string
}

func blindIndex(protector Protector, purpose string, value []byte) (string, error) {
	indexer, ok := protector.(BlindIndexer)
	if !ok {
		return "", errors.New("storage: sensitive-data protector does not support blind indexes")
	}
	result := indexer.BlindIndex(purpose, value)
	decoded, err := hex.DecodeString(result)
	if err != nil || len(result) != blindIndexEncodedLength || len(decoded) != blindIndexEncodedLength/2 {
		return "", errors.New("storage: sensitive-data protector returned an invalid blind index")
	}
	return result, nil
}

func accountPhoneBlindIndex(protector Protector, normalizedPhone string) (string, error) {
	digest := sha256.Sum256([]byte(normalizedPhone))
	return blindIndex(protector, accountPhoneBlindIndexPurpose, digest[:])
}

func activationPhoneBlindIndex(protector Protector, normalizedPhone string) (string, error) {
	digest := sha256.Sum256([]byte(normalizedPhone))
	return blindIndex(protector, activationPhoneIndexPurpose, digest[:])
}

func legacyPhoneFingerprint(normalizedPhone string) string {
	digest := sha256.Sum256([]byte(normalizedPhone))
	return hex.EncodeToString(digest[:])
}

func blindIndexLegacyDigest(protector Protector, purpose, legacyFingerprint string) (string, error) {
	digest, err := hex.DecodeString(legacyFingerprint)
	if err != nil || len(digest) != sha256.Size {
		return "", errors.New("storage: persisted legacy fingerprint is malformed")
	}
	return blindIndex(protector, purpose, digest)
}

type accountSensitiveEnvelope struct {
	Version          int    `json:"v"`
	Kind             string `json:"kind"`
	PhoneFingerprint string `json:"phone_fingerprint"`
	PhoneNumber      string `json:"phone_number"`
}

type activationSensitiveEnvelope struct {
	Version              int    `json:"v"`
	Kind                 string `json:"kind"`
	Provider             string `json:"provider"`
	ProviderActivationID string `json:"provider_activation_id"`
	PhoneFingerprint     string `json:"phone_fingerprint,omitempty"`
	PhoneNumber          string `json:"phone_number"`
	ProviderPayload      []byte `json:"provider_payload"`
}

type verificationSensitiveEnvelope struct {
	Version         int    `json:"v"`
	Kind            string `json:"kind"`
	ActivationID    int64  `json:"activation_id"`
	CycleNo         int    `json:"cycle_no"`
	Code            string `json:"code"`
	ProviderPayload []byte `json:"provider_payload"`
}

type heroSMSWebhookSensitiveEnvelope struct {
	Version            int     `json:"v"`
	Kind               string  `json:"kind"`
	PayloadFingerprint string  `json:"payload_fingerprint"`
	Code               *string `json:"code"`
	Text               *string `json:"text"`
	PhoneNumber        string  `json:"phone_number"`
	RawPayload         []byte  `json:"raw_payload"`
}

func sealEnvelope(protector Protector, value any) ([]byte, error) {
	if protector == nil {
		return nil, errors.New("storage: sensitive-data protector is not configured")
	}
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal sensitive envelope: %w", err)
	}
	ciphertext, err := protector.Seal(plain)
	if err != nil {
		return nil, fmt.Errorf("seal sensitive envelope: %w", err)
	}
	if len(ciphertext) == 0 {
		return nil, errors.New("seal sensitive envelope: empty ciphertext")
	}
	return ciphertext, nil
}

func openEnvelope(protector Protector, ciphertext []byte, target any) error {
	if protector == nil {
		return errors.New("storage: sensitive-data protector is not configured")
	}
	plain, err := protector.Open(ciphertext)
	if err != nil {
		return fmt.Errorf("open sensitive envelope: %w", err)
	}
	if err = json.Unmarshal(plain, target); err != nil {
		return fmt.Errorf("decode sensitive envelope: %w", err)
	}
	return nil
}

func sealAccountSensitive(protector Protector, fingerprint, phone string) ([]byte, error) {
	return sealEnvelope(protector, accountSensitiveEnvelope{
		Version: sensitiveEnvelopeVersion, Kind: "account",
		PhoneFingerprint: fingerprint, PhoneNumber: phone,
	})
}

func openAccountSensitive(protector Protector, ciphertext []byte, fingerprint string) (accountSensitiveEnvelope, error) {
	var envelope accountSensitiveEnvelope
	if err := openEnvelope(protector, ciphertext, &envelope); err != nil {
		return accountSensitiveEnvelope{}, err
	}
	if envelope.Version != sensitiveEnvelopeVersion || envelope.Kind != "account" ||
		envelope.PhoneFingerprint != fingerprint {
		return accountSensitiveEnvelope{}, errors.New("account sensitive envelope identity mismatch")
	}
	return envelope, nil
}

func sealActivationSensitive(
	protector Protector,
	provider, providerActivationID, phoneFingerprint, phone string,
	payload []byte,
) ([]byte, error) {
	return sealEnvelope(protector, activationSensitiveEnvelope{
		Version: sensitiveEnvelopeVersion, Kind: "activation", Provider: provider,
		ProviderActivationID: providerActivationID, PhoneFingerprint: phoneFingerprint, PhoneNumber: phone,
		ProviderPayload: append([]byte(nil), payload...),
	})
}

func openActivationSensitive(
	protector Protector,
	ciphertext []byte,
	provider, providerActivationID, phoneFingerprint string,
) (activationSensitiveEnvelope, error) {
	var envelope activationSensitiveEnvelope
	if err := openEnvelope(protector, ciphertext, &envelope); err != nil {
		return activationSensitiveEnvelope{}, err
	}
	if envelope.Version != sensitiveEnvelopeVersion || envelope.Kind != "activation" ||
		envelope.Provider != provider || envelope.ProviderActivationID != providerActivationID ||
		(envelope.PhoneFingerprint != "" && envelope.PhoneFingerprint != phoneFingerprint) ||
		!json.Valid(envelope.ProviderPayload) {
		return activationSensitiveEnvelope{}, errors.New("activation sensitive envelope identity mismatch")
	}
	return envelope, nil
}

func sealVerificationSensitive(
	protector Protector,
	activationID int64,
	cycleNo int,
	code string,
	payload []byte,
) ([]byte, error) {
	return sealEnvelope(protector, verificationSensitiveEnvelope{
		Version: sensitiveEnvelopeVersion, Kind: "verification",
		ActivationID: activationID, CycleNo: cycleNo, Code: code,
		ProviderPayload: append([]byte(nil), payload...),
	})
}

func openVerificationSensitive(
	protector Protector,
	ciphertext []byte,
	activationID int64,
	cycleNo int,
) (verificationSensitiveEnvelope, error) {
	var envelope verificationSensitiveEnvelope
	if err := openEnvelope(protector, ciphertext, &envelope); err != nil {
		return verificationSensitiveEnvelope{}, err
	}
	if envelope.Version != sensitiveEnvelopeVersion || envelope.Kind != "verification" ||
		envelope.ActivationID != activationID || envelope.CycleNo != cycleNo ||
		envelope.Code == "" || !json.Valid(envelope.ProviderPayload) {
		return verificationSensitiveEnvelope{}, errors.New("verification sensitive envelope identity mismatch")
	}
	return envelope, nil
}

func sealHeroSMSWebhookSensitive(
	protector Protector,
	fingerprint string,
	code, text *string,
	phone string,
	rawPayload []byte,
) ([]byte, error) {
	return sealEnvelope(protector, heroSMSWebhookSensitiveEnvelope{
		Version: sensitiveEnvelopeVersion, Kind: "hero_sms_webhook",
		PayloadFingerprint: fingerprint, Code: cloneOptionalString(code),
		Text: cloneOptionalString(text), PhoneNumber: phone,
		RawPayload: append([]byte(nil), rawPayload...),
	})
}

func openHeroSMSWebhookSensitive(
	protector Protector,
	ciphertext []byte,
	fingerprint string,
) (heroSMSWebhookSensitiveEnvelope, error) {
	var envelope heroSMSWebhookSensitiveEnvelope
	if err := openEnvelope(protector, ciphertext, &envelope); err != nil {
		return heroSMSWebhookSensitiveEnvelope{}, err
	}
	if envelope.Version != sensitiveEnvelopeVersion || envelope.Kind != "hero_sms_webhook" ||
		envelope.PayloadFingerprint != fingerprint || !json.Valid(envelope.RawPayload) {
		return heroSMSWebhookSensitiveEnvelope{}, errors.New("HeroSMS webhook sensitive envelope identity mismatch")
	}
	return envelope, nil
}

func cloneOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func verifyStorageProtector(ctx context.Context, tx pgx.Tx, protector Protector) error {
	var ciphertext []byte
	err := tx.QueryRow(ctx, `SELECT key_check FROM storage_encryption_state WHERE id=1 FOR UPDATE`).Scan(&ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = verifyLegacyCiphertexts(ctx, tx, protector); err != nil {
			return err
		}
		ciphertext, err = protector.Seal([]byte(storageKeyCheckPlaintext))
		if err != nil {
			return fmt.Errorf("seal storage key check: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO storage_encryption_state(id,key_check) VALUES(1,$1)`, ciphertext); err != nil {
			return fmt.Errorf("persist storage key check: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read storage key check: %w", err)
	}
	plain, err := protector.Open(ciphertext)
	if err != nil || !bytes.Equal(plain, []byte(storageKeyCheckPlaintext)) {
		return errors.New("storage encryption key does not match persisted data")
	}
	return nil
}

// verifyLegacyCiphertexts proves that a key supplied for the first migration
// can open every ciphertext which predates storage_encryption_state. Without
// this check, an accidentally changed key could be persisted while API keys,
// PINs, proxies and GoPay sessions remain encrypted with the previous key.
func verifyLegacyCiphertexts(ctx context.Context, tx pgx.Tx, protector Protector) error {
	rows, err := tx.Query(ctx, `
		SELECT source,row_id,ciphertext FROM (
			SELECT 'batches.target_pin_enc'::text AS source,id AS row_id,target_pin_enc AS ciphertext
			FROM batches WHERE octet_length(target_pin_enc)>0
			UNION ALL
			SELECT 'accounts.credentials_enc',id,credentials_enc
			FROM accounts WHERE octet_length(credentials_enc)>0
			UNION ALL
			SELECT 'accounts.target_pin_enc',id,target_pin_enc
			FROM accounts WHERE octet_length(target_pin_enc)>0
			UNION ALL
			SELECT 'accounts.sensitive_enc',id,sensitive_enc
			FROM accounts WHERE octet_length(sensitive_enc)>0
			UNION ALL
			SELECT 'activations.sensitive_enc',id,sensitive_enc
			FROM activations WHERE octet_length(sensitive_enc)>0
			UNION ALL
			SELECT 'verification_codes.sensitive_enc',id,sensitive_enc
			FROM verification_codes WHERE octet_length(sensitive_enc)>0
			UNION ALL
			SELECT 'hero_sms_webhook_events.sensitive_enc',id,sensitive_enc
			FROM hero_sms_webhook_events WHERE octet_length(sensitive_enc)>0
		) encrypted_rows ORDER BY source,row_id`)
	if err != nil {
		return fmt.Errorf("inspect persisted ciphertext: %w", err)
	}
	for rows.Next() {
		var source string
		var rowID int64
		var candidate []byte
		if err = rows.Scan(&source, &rowID, &candidate); err != nil {
			rows.Close()
			return fmt.Errorf("scan persisted ciphertext: %w", err)
		}
		if _, openErr := protector.Open(candidate); openErr != nil {
			rows.Close()
			return fmt.Errorf("storage encryption key does not match persisted data (%s row %d)", source, rowID)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate persisted ciphertext: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		SELECT source,row_id,ciphertext_b64 FROM (
			SELECT 'settings.api_key_encrypted'::text AS source,
				row_number() OVER (ORDER BY key)::bigint AS row_id,
				value->>'api_key_encrypted' AS ciphertext_b64
			FROM settings WHERE key IN ('smsbower','hero-sms')
			UNION ALL
			SELECT 'settings.draft_encrypted',1::bigint,value->>'draft_encrypted'
			FROM settings WHERE key='batch-draft'
			UNION ALL
			SELECT 'batches.proxy_pool',
				(batch.id * 1000000 + proxy.ordinality)::bigint,
				proxy.item->>'encrypted'
			FROM batches AS batch
			CROSS JOIN LATERAL jsonb_array_elements(
				CASE WHEN jsonb_typeof(batch.config->'proxy_pool')='array'
					THEN batch.config->'proxy_pool' ELSE '[]'::jsonb END
			) WITH ORDINALITY AS proxy(item,ordinality)
		) encoded_rows
		WHERE btrim(COALESCE(ciphertext_b64,''))<>''
		ORDER BY source,row_id`)
	if err != nil {
		return fmt.Errorf("inspect persisted encoded ciphertext: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var source, encoded string
		var rowID int64
		if err = rows.Scan(&source, &rowID, &encoded); err != nil {
			return fmt.Errorf("scan persisted encoded ciphertext: %w", err)
		}
		candidate, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil || len(candidate) == 0 {
			return fmt.Errorf("persisted encrypted data is malformed (%s row %d)", source, rowID)
		}
		if _, openErr := protector.Open(candidate); openErr != nil {
			return fmt.Errorf("storage encryption key does not match persisted data (%s row %d)", source, rowID)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iterate persisted encoded ciphertext: %w", err)
	}
	return nil
}

func migrateLegacySensitiveData(ctx context.Context, tx pgx.Tx, protector Protector) error {
	if err := verifyStorageProtector(ctx, tx, protector); err != nil {
		return err
	}
	// A development database may already contain the key check while migration
	// 13 itself is still unrecorded. Validate every pre-marker ciphertext here as
	// well so one damaged or mixed-key row cannot be hidden by a valid sentinel.
	if err := verifyLegacyCiphertexts(ctx, tx, protector); err != nil {
		return err
	}
	return backfillLegacySensitiveData(ctx, tx, protector)
}

func backfillLegacySensitiveData(ctx context.Context, tx pgx.Tx, protector Protector) error {
	if err := backfillLegacyAccounts(ctx, tx, protector); err != nil {
		return err
	}
	if err := backfillLegacyActivations(ctx, tx, protector); err != nil {
		return err
	}
	if err := backfillLegacyVerifications(ctx, tx, protector); err != nil {
		return err
	}
	return backfillLegacyHeroSMSWebhooks(ctx, tx, protector)
}

func backfillLegacyAccounts(ctx context.Context, tx pgx.Tx, protector Protector) error {
	type legacyRow struct {
		id                 int64
		fingerprint, phone string
	}
	rows, err := tx.Query(ctx, `SELECT id,phone_fingerprint,phone_number FROM accounts
		WHERE octet_length(sensitive_enc)=0 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select legacy account sensitive data: %w", err)
	}
	items := make([]legacyRow, 0)
	for rows.Next() {
		var item legacyRow
		if err = rows.Scan(&item.id, &item.fingerprint, &item.phone); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy account sensitive data: %w", err)
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("iterate legacy account sensitive data: %w", err)
	}
	for _, item := range items {
		ciphertext, sealErr := sealAccountSensitive(protector, item.fingerprint, item.phone)
		if sealErr != nil {
			return sealErr
		}
		if _, err = tx.Exec(ctx, `UPDATE accounts SET phone_number='',sensitive_enc=$2 WHERE id=$1`, item.id, ciphertext); err != nil {
			return fmt.Errorf("backfill account sensitive data: %w", err)
		}
	}
	return nil
}

func backfillLegacyActivations(ctx context.Context, tx pgx.Tx, protector Protector) error {
	type legacyRow struct {
		id                          int64
		provider, providerID, phone string
		payload                     []byte
	}
	rows, err := tx.Query(ctx, `SELECT id,provider,provider_activation_id,phone_number,provider_payload
		FROM activations WHERE octet_length(sensitive_enc)=0 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select legacy activation sensitive data: %w", err)
	}
	items := make([]legacyRow, 0)
	for rows.Next() {
		var item legacyRow
		if err = rows.Scan(&item.id, &item.provider, &item.providerID, &item.phone, &item.payload); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy activation sensitive data: %w", err)
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("iterate legacy activation sensitive data: %w", err)
	}
	for _, item := range items {
		ciphertext, sealErr := sealActivationSensitive(
			protector, item.provider, item.providerID,
			legacyPhoneFingerprint(item.phone), item.phone, item.payload,
		)
		if sealErr != nil {
			return sealErr
		}
		if _, err = tx.Exec(ctx, `UPDATE activations SET phone_number='',provider_payload='{}'::jsonb,sensitive_enc=$2 WHERE id=$1`, item.id, ciphertext); err != nil {
			return fmt.Errorf("backfill activation sensitive data: %w", err)
		}
	}
	return nil
}

func backfillLegacyVerifications(ctx context.Context, tx pgx.Tx, protector Protector) error {
	type legacyRow struct {
		id, activationID int64
		cycleNo          int
		code             string
		payload          []byte
	}
	rows, err := tx.Query(ctx, `SELECT id,activation_id,cycle_no,code,provider_payload
		FROM verification_codes WHERE octet_length(sensitive_enc)=0 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select legacy verification sensitive data: %w", err)
	}
	items := make([]legacyRow, 0)
	for rows.Next() {
		var item legacyRow
		if err = rows.Scan(&item.id, &item.activationID, &item.cycleNo, &item.code, &item.payload); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy verification sensitive data: %w", err)
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("iterate legacy verification sensitive data: %w", err)
	}
	for _, item := range items {
		ciphertext, sealErr := sealVerificationSensitive(
			protector, item.activationID, item.cycleNo, item.code, item.payload,
		)
		if sealErr != nil {
			return sealErr
		}
		if _, err = tx.Exec(ctx, `UPDATE verification_codes SET code='',provider_payload='{}'::jsonb,sensitive_enc=$2 WHERE id=$1`, item.id, ciphertext); err != nil {
			return fmt.Errorf("backfill verification sensitive data: %w", err)
		}
	}
	return nil
}

func backfillLegacyHeroSMSWebhooks(ctx context.Context, tx pgx.Tx, protector Protector) error {
	type legacyRow struct {
		id                 int64
		fingerprint, phone string
		code, text         sql.NullString
		raw                []byte
	}
	rows, err := tx.Query(ctx, `SELECT id,payload_fingerprint,code,message_text,phone_number,raw_payload
		FROM hero_sms_webhook_events WHERE octet_length(sensitive_enc)=0 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select legacy HeroSMS webhook sensitive data: %w", err)
	}
	items := make([]legacyRow, 0)
	for rows.Next() {
		var item legacyRow
		if err = rows.Scan(&item.id, &item.fingerprint, &item.code, &item.text, &item.phone, &item.raw); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy HeroSMS webhook sensitive data: %w", err)
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("iterate legacy HeroSMS webhook sensitive data: %w", err)
	}
	for _, item := range items {
		ciphertext, sealErr := sealHeroSMSWebhookSensitive(
			protector, item.fingerprint, nullStringPointer(item.code),
			nullStringPointer(item.text), item.phone, item.raw,
		)
		if sealErr != nil {
			return sealErr
		}
		if _, err = tx.Exec(ctx, `UPDATE hero_sms_webhook_events SET
			code=NULL,message_text=NULL,phone_number='',raw_payload='{}'::json,sensitive_enc=$2
			WHERE id=$1`, item.id, ciphertext); err != nil {
			return fmt.Errorf("backfill HeroSMS webhook sensitive data: %w", err)
		}
	}
	return nil
}

func migrateSensitiveBlindIndexes(ctx context.Context, tx pgx.Tx, protector Protector) error {
	if err := migrateAccountPhoneBlindIndexes(ctx, tx, protector); err != nil {
		return err
	}
	if err := migrateActivationPhoneBlindIndexes(ctx, tx, protector); err != nil {
		return err
	}
	return migrateHeroSMSWebhookBlindIndexes(ctx, tx, protector)
}

func migrateAccountPhoneBlindIndexes(ctx context.Context, tx pgx.Tx, protector Protector) error {
	type accountRow struct {
		id          int64
		fingerprint string
		sensitive   []byte
	}
	rows, err := tx.Query(ctx, `SELECT id,phone_fingerprint,sensitive_enc FROM accounts ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select account phone indexes: %w", err)
	}
	items := make([]accountRow, 0)
	for rows.Next() {
		var item accountRow
		if err = rows.Scan(&item.id, &item.fingerprint, &item.sensitive); err != nil {
			rows.Close()
			return fmt.Errorf("scan account phone index: %w", err)
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("iterate account phone indexes: %w", err)
	}
	for _, item := range items {
		envelope, openErr := openAccountSensitive(protector, item.sensitive, item.fingerprint)
		if openErr != nil {
			return fmt.Errorf("open account %d for blind-index migration: %w", item.id, openErr)
		}
		normalized, normalizeErr := domain.NormalizePhone(envelope.PhoneNumber)
		if normalizeErr != nil {
			return fmt.Errorf("normalize account %d phone for blind-index migration: %w", item.id, normalizeErr)
		}
		if legacyPhoneFingerprint(normalized) != item.fingerprint {
			return fmt.Errorf("account %d phone fingerprint does not match encrypted phone", item.id)
		}
		fingerprint, indexErr := accountPhoneBlindIndex(protector, normalized)
		if indexErr != nil {
			return fmt.Errorf("index account %d: %w", item.id, indexErr)
		}
		ciphertext, sealErr := sealAccountSensitive(protector, fingerprint, normalized)
		if sealErr != nil {
			return fmt.Errorf("reseal account %d: %w", item.id, sealErr)
		}
		if _, err = tx.Exec(ctx, `UPDATE accounts SET phone_fingerprint=$2,sensitive_enc=$3 WHERE id=$1`,
			item.id, fingerprint, ciphertext); err != nil {
			return fmt.Errorf("update account %d phone index: %w", item.id, err)
		}
	}
	return nil
}

func migrateActivationPhoneBlindIndexes(ctx context.Context, tx pgx.Tx, protector Protector) error {
	type activationRow struct {
		id                                int64
		fingerprint, provider, providerID string
		sensitive                         []byte
	}
	rows, err := tx.Query(ctx, `SELECT id,phone_fingerprint,provider,provider_activation_id,sensitive_enc
		FROM activations ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select activation phone indexes: %w", err)
	}
	items := make([]activationRow, 0)
	for rows.Next() {
		var item activationRow
		if err = rows.Scan(&item.id, &item.fingerprint, &item.provider, &item.providerID, &item.sensitive); err != nil {
			rows.Close()
			return fmt.Errorf("scan activation phone index: %w", err)
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("iterate activation phone indexes: %w", err)
	}
	for _, item := range items {
		envelope, openErr := openActivationSensitive(
			protector, item.sensitive, item.provider, item.providerID, item.fingerprint,
		)
		if openErr != nil {
			return fmt.Errorf("open activation %d for blind-index migration: %w", item.id, openErr)
		}
		normalized, normalizeErr := domain.NormalizePhone(envelope.PhoneNumber)
		if normalizeErr != nil {
			return fmt.Errorf("normalize activation %d phone for blind-index migration: %w", item.id, normalizeErr)
		}
		if legacyPhoneFingerprint(normalized) != item.fingerprint {
			return fmt.Errorf("activation %d phone fingerprint does not match encrypted phone", item.id)
		}
		fingerprint, indexErr := activationPhoneBlindIndex(protector, normalized)
		if indexErr != nil {
			return fmt.Errorf("index activation %d: %w", item.id, indexErr)
		}
		ciphertext, sealErr := sealActivationSensitive(
			protector, item.provider, item.providerID, fingerprint, normalized, envelope.ProviderPayload,
		)
		if sealErr != nil {
			return fmt.Errorf("reseal activation %d: %w", item.id, sealErr)
		}
		if _, err = tx.Exec(ctx, `UPDATE activations SET phone_fingerprint=$2,sensitive_enc=$3 WHERE id=$1`,
			item.id, fingerprint, ciphertext); err != nil {
			return fmt.Errorf("update activation %d phone index: %w", item.id, err)
		}
	}
	return nil
}

func migrateHeroSMSWebhookBlindIndexes(ctx context.Context, tx pgx.Tx, protector Protector) error {
	type webhookRow struct {
		id          int64
		fingerprint string
		sensitive   []byte
	}
	rows, err := tx.Query(ctx, `SELECT id,payload_fingerprint,sensitive_enc
		FROM hero_sms_webhook_events ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select HeroSMS webhook indexes: %w", err)
	}
	items := make([]webhookRow, 0)
	for rows.Next() {
		var item webhookRow
		if err = rows.Scan(&item.id, &item.fingerprint, &item.sensitive); err != nil {
			rows.Close()
			return fmt.Errorf("scan HeroSMS webhook index: %w", err)
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("iterate HeroSMS webhook indexes: %w", err)
	}
	for _, item := range items {
		envelope, openErr := openHeroSMSWebhookSensitive(protector, item.sensitive, item.fingerprint)
		if openErr != nil {
			return fmt.Errorf("open HeroSMS webhook %d for blind-index migration: %w", item.id, openErr)
		}
		// The envelope authenticates and binds the old fingerprint. Wrap that
		// exact digest instead of rebuilding it from timestamptz columns: Go can
		// parse provider nanoseconds while PostgreSQL stores microseconds, so a
		// reconstruction could reject an otherwise valid historical callback.
		fingerprint, indexErr := blindIndexLegacyDigest(
			protector, heroSMSWebhookIndexPurpose, item.fingerprint,
		)
		if indexErr != nil {
			return fmt.Errorf("index HeroSMS webhook %d: %w", item.id, indexErr)
		}
		ciphertext, sealErr := sealHeroSMSWebhookSensitive(
			protector, fingerprint, envelope.Code, envelope.Text,
			envelope.PhoneNumber, envelope.RawPayload,
		)
		if sealErr != nil {
			return fmt.Errorf("reseal HeroSMS webhook %d: %w", item.id, sealErr)
		}
		if _, err = tx.Exec(ctx, `UPDATE hero_sms_webhook_events
			SET payload_fingerprint=$2,sensitive_enc=$3 WHERE id=$1`,
			item.id, fingerprint, ciphertext); err != nil {
			return fmt.Errorf("update HeroSMS webhook %d index: %w", item.id, err)
		}
	}
	return nil
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}
