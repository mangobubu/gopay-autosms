package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mangobubu/gopay-autosms/internal/domain"
)

const heroSMSProvider = "hero-sms"

const heroSMSWebhookActivationFinishedReason = "activation already finished"

const heroSMSWebhookEventColumns = `id, activation_id, provider_activation_id,
	code, message_text, phone_number, service_code, country_code,
	provider_received_at, raw_payload, payload_fingerprint, status, attempts,
	last_error, next_attempt_at, claimed_lease_owner, claimed_lease_version,
	received_at, processed_at, sensitive_enc`

const heroSMSWebhookInsertSQL = `INSERT INTO hero_sms_webhook_events(
	activation_id, provider_activation_id, code, message_text, phone_number,
	service_code, country_code, provider_received_at, raw_payload,
	payload_fingerprint, status, last_error, next_attempt_at, received_at,
	processed_at, sensitive_enc
) VALUES(
	$1,$2,NULL,NULL,'',$3,$4,$5,'{}'::json,$6,
	CASE WHEN $8::boolean THEN 'ignored' ELSE 'received' END,
	CASE WHEN $8::boolean THEN '` + heroSMSWebhookActivationFinishedReason + `' ELSE '' END,
	now(),now(),CASE WHEN $8::boolean THEN now() ELSE NULL END,$7
)
ON CONFLICT(payload_fingerprint) DO NOTHING
RETURNING ` + heroSMSWebhookEventColumns

// Wakeups which race an active worker are held separately. The worker's later
// ReleaseActivationLease consumes this timestamp instead of overwriting it with
// a stale future schedule. When no lease is active, the regular schedule can be
// moved directly and no wake marker is needed.
const wakeHeroSMSActivationSQL = `UPDATE activations SET
	next_run_at=CASE
		WHEN lease_owner='' OR lease_until IS NULL OR lease_until <= now()
			THEN LEAST(next_run_at,$2)
		ELSE next_run_at
	END,
	webhook_wakeup_at=CASE
		WHEN lease_owner<>''
			THEN LEAST(COALESCE(webhook_wakeup_at,$2),$2)
		ELSE NULL
	END,
	updated_at=now()
	WHERE id=$1 AND finished_at IS NULL`

const ownedHeroSMSWebhookLeaseSQL = `SELECT provider_activation_id FROM activations
	WHERE id=$1 AND provider='` + heroSMSProvider + `'
		AND lease_owner=$2 AND lease_version=$3
		AND lease_until > $4 AND finished_at IS NULL
	FOR UPDATE`

var claimNextHeroSMSWebhookEventSQL = `WITH candidate AS (
	SELECT id FROM hero_sms_webhook_events
	WHERE activation_id=$1
		AND ((status='received' AND next_attempt_at <= $4)
			OR (status='processing'
				AND (claimed_lease_owner<>$2 OR claimed_lease_version<>$3)))
	ORDER BY (status='processing') DESC,
		provider_received_at NULLS LAST, received_at, id
	FOR UPDATE SKIP LOCKED
	LIMIT 1
)
UPDATE hero_sms_webhook_events event SET
	status='processing', attempts=event.attempts+1,
	claimed_lease_owner=$2, claimed_lease_version=$3,
	processed_at=NULL
FROM candidate WHERE event.id=candidate.id
RETURNING ` + prefixedHeroSMSWebhookEventColumns("event")

const refreshHeroSMSWebhookWakeSQL = `UPDATE activations SET
	webhook_wakeup_at=(SELECT min(next_attempt_at) FROM hero_sms_webhook_events
		WHERE activation_id=$1 AND status='received'),
	updated_at=now()
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3 AND finished_at IS NULL`

const ignoreOpenHeroSMSWebhookEventsSQL = `UPDATE hero_sms_webhook_events SET
	status='ignored', last_error='` + heroSMSWebhookActivationFinishedReason + `',
	processed_at=now()
	WHERE activation_id=$1 AND status IN ('received','processing')`

func ignoreOpenHeroSMSWebhookEvents(ctx context.Context, tx pgx.Tx, activationID int64) error {
	if _, err := tx.Exec(ctx, ignoreOpenHeroSMSWebhookEventsSQL, activationID); err != nil {
		return mapError(err)
	}
	return nil
}

func prefixedHeroSMSWebhookEventColumns(prefix string) string {
	parts := strings.Split(heroSMSWebhookEventColumns, ",")
	for i, part := range parts {
		parts[i] = prefix + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

func (s *PostgresStore) scanHeroSMSWebhookEvent(row rowScanner) (domain.HeroSMSWebhookEvent, error) {
	var event domain.HeroSMSWebhookEvent
	var activationID sql.NullInt64
	var code, messageText sql.NullString
	var providerReceivedAt, processedAt sql.NullTime
	var legacyPhone string
	var payload, sensitive []byte
	var status string
	err := row.Scan(
		&event.ID, &activationID, &event.ProviderActivationID,
		&code, &messageText, &legacyPhone, &event.ServiceCode,
		&event.CountryCode, &providerReceivedAt, &payload,
		&event.PayloadFingerprint, &status, &event.Attempts, &event.LastError,
		&event.NextAttemptAt, &event.ClaimedLeaseOwner,
		&event.ClaimedLeaseVersion, &event.ReceivedAt, &processedAt, &sensitive,
	)
	if err != nil {
		return domain.HeroSMSWebhookEvent{}, err
	}
	if activationID.Valid {
		event.ActivationID = &activationID.Int64
	}
	event.PhoneNumber = legacyPhone
	if code.Valid {
		event.Code = &code.String
	}
	if messageText.Valid {
		event.Text = &messageText.String
	}
	if providerReceivedAt.Valid {
		event.ProviderReceivedAt = &providerReceivedAt.Time
	}
	if processedAt.Valid {
		event.ProcessedAt = &processedAt.Time
	}
	event.RawPayload = cloneJSON(payload)
	if len(sensitive) > 0 {
		envelope, openErr := openHeroSMSWebhookSensitive(
			s.protector, sensitive, event.PayloadFingerprint,
		)
		if openErr != nil {
			return domain.HeroSMSWebhookEvent{}, fmt.Errorf("decrypt HeroSMS webhook event %d: %w", event.ID, openErr)
		}
		event.Code = cloneOptionalString(envelope.Code)
		event.Text = cloneOptionalString(envelope.Text)
		event.PhoneNumber = envelope.PhoneNumber
		event.RawPayload = cloneJSON(envelope.RawPayload)
	}
	event.Status = domain.HeroSMSWebhookEventStatus(status)
	return event, nil
}

// heroSMSWebhookFingerprint intentionally hashes normalized provider fields,
// not the raw body. Provider retries which reorder JSON keys or change only
// whitespace therefore remain idempotent. Length prefixes make field boundaries
// unambiguous without relying on a delimiter which could occur in message text.
func heroSMSWebhookFingerprint(protector Protector, params IngestHeroSMSWebhookParams) (string, error) {
	return blindIndex(protector, heroSMSWebhookIndexPurpose, heroSMSWebhookFingerprintMaterial(params))
}

func heroSMSWebhookFingerprintMaterial(params IngestHeroSMSWebhookParams) []byte {
	hash := sha256.New()
	writeField := func(value string) {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	writeNullableField := func(value *string) {
		if value == nil {
			_, _ = hash.Write([]byte{0})
			return
		}
		_, _ = hash.Write([]byte{1})
		writeField(*value)
	}
	writeField(params.ProviderActivationID)
	if params.ProviderReceivedAt == nil {
		writeField("")
	} else {
		writeField(params.ProviderReceivedAt.UTC().Format(time.RFC3339Nano))
	}
	writeField(params.PhoneNumber)
	writeField(params.ServiceCode)
	writeField(params.CountryCode)
	writeNullableField(params.Code)
	writeNullableField(params.Text)
	return hash.Sum(nil)
}

func normalizeHeroSMSWebhookParams(params IngestHeroSMSWebhookParams) (IngestHeroSMSWebhookParams, error) {
	params.ProviderActivationID = strings.TrimSpace(params.ProviderActivationID)
	params.Code = normalizedOptionalWebhookString(params.Code)
	params.Text = normalizedOptionalWebhookString(params.Text)
	params.PhoneNumber = strings.TrimSpace(params.PhoneNumber)
	params.ServiceCode = strings.TrimSpace(params.ServiceCode)
	params.CountryCode = strings.TrimSpace(params.CountryCode)
	if params.ProviderActivationID == "" || len(params.RawPayload) == 0 || !json.Valid(params.RawPayload) {
		return IngestHeroSMSWebhookParams{}, ErrInvalidInput
	}
	params.RawPayload = cloneJSON(params.RawPayload)
	if params.ProviderReceivedAt != nil {
		receivedAt := params.ProviderReceivedAt.UTC()
		if receivedAt.IsZero() {
			params.ProviderReceivedAt = nil
		} else {
			params.ProviderReceivedAt = &receivedAt
		}
	}
	return params, nil
}

func normalizedOptionalWebhookString(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	return &normalized
}

func (s *PostgresStore) IngestHeroSMSWebhook(
	ctx context.Context,
	params IngestHeroSMSWebhookParams,
) (IngestHeroSMSWebhookResult, error) {
	params, err := normalizeHeroSMSWebhookParams(params)
	if err != nil {
		return IngestHeroSMSWebhookResult{}, err
	}
	fingerprint, err := heroSMSWebhookFingerprint(s.protector, params)
	if err != nil {
		return IngestHeroSMSWebhookResult{}, err
	}
	sensitive, err := sealHeroSMSWebhookSensitive(
		s.protector, fingerprint, params.Code, params.Text, params.PhoneNumber, params.RawPayload,
	)
	if err != nil {
		return IngestHeroSMSWebhookResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return IngestHeroSMSWebhookResult{}, fmt.Errorf("begin HeroSMS webhook ingest: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// This is the same provider-identity lock used by activation persistence.
	// Whichever transaction commits first attaches the event; the other sees and
	// attaches the row before it commits, covering callbacks delivered during the
	// provider-call/database-commit gap.
	if err = lockProviderActivation(ctx, tx, heroSMSProvider, params.ProviderActivationID); err != nil {
		return IngestHeroSMSWebhookResult{}, err
	}
	var activationID sql.NullInt64
	var finishedAt sql.NullTime
	err = tx.QueryRow(ctx, `SELECT id, finished_at FROM activations
		WHERE provider=$1 AND provider_activation_id=$2 FOR UPDATE`,
		heroSMSProvider, params.ProviderActivationID,
	).Scan(&activationID, &finishedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return IngestHeroSMSWebhookResult{}, mapError(err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		activationID = sql.NullInt64{}
		finishedAt = sql.NullTime{}
	}

	event, scanErr := s.scanHeroSMSWebhookEvent(tx.QueryRow(ctx, heroSMSWebhookInsertSQL,
		nullableInt64Value(activationID), params.ProviderActivationID,
		params.ServiceCode, params.CountryCode, params.ProviderReceivedAt,
		fingerprint, sensitive, finishedAt.Valid,
	))
	inserted := scanErr == nil
	if scanErr != nil && !errors.Is(scanErr, pgx.ErrNoRows) {
		return IngestHeroSMSWebhookResult{}, mapError(scanErr)
	}
	if !inserted {
		// A retry can be the first request which observes the activation. Attach
		// the original audit row and converge an event for an already-finished
		// activation to ignored rather than leaving it pending forever.
		if activationID.Valid {
			_, err = tx.Exec(ctx, `UPDATE hero_sms_webhook_events SET
				activation_id=COALESCE(activation_id,$2),
				status=CASE WHEN $3::boolean AND status IN ('received','processing') THEN 'ignored' ELSE status END,
				last_error=CASE WHEN $3::boolean AND status IN ('received','processing')
					THEN $4 ELSE last_error END,
				processed_at=CASE WHEN $3::boolean AND status IN ('received','processing')
					THEN now() ELSE processed_at END
				WHERE payload_fingerprint=$1`,
				fingerprint, activationID.Int64, finishedAt.Valid, heroSMSWebhookActivationFinishedReason,
			)
			if err != nil {
				return IngestHeroSMSWebhookResult{}, mapError(err)
			}
		}
		event, err = s.scanHeroSMSWebhookEvent(tx.QueryRow(ctx,
			`SELECT `+heroSMSWebhookEventColumns+` FROM hero_sms_webhook_events
				WHERE payload_fingerprint=$1`, fingerprint,
		))
		if err != nil {
			return IngestHeroSMSWebhookResult{}, mapError(err)
		}
	}

	if activationID.Valid && !finishedAt.Valid &&
		(event.Status == domain.HeroSMSWebhookEventReceived || event.Status == domain.HeroSMSWebhookEventProcessing) {
		if _, err = tx.Exec(ctx, wakeHeroSMSActivationSQL, activationID.Int64, event.NextAttemptAt); err != nil {
			return IngestHeroSMSWebhookResult{}, mapError(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return IngestHeroSMSWebhookResult{}, fmt.Errorf("%w: commit HeroSMS webhook ingest: %v", ErrCommitUnknown, err)
	}
	return IngestHeroSMSWebhookResult{Event: event, Inserted: inserted}, nil
}

func nullableInt64Value(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

func (s *PostgresStore) ClaimNextHeroSMSWebhookEventOwned(
	ctx context.Context,
	activationID int64,
	leaseOwner string,
	leaseVersion int64,
	now time.Time,
) (domain.HeroSMSWebhookEvent, error) {
	leaseOwner = strings.TrimSpace(leaseOwner)
	if activationID <= 0 || leaseOwner == "" || leaseVersion <= 0 || now.IsZero() {
		return domain.HeroSMSWebhookEvent{}, ErrInvalidInput
	}
	now = now.UTC()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.HeroSMSWebhookEvent{}, fmt.Errorf("begin HeroSMS webhook claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var providerActivationID string
	if err = tx.QueryRow(ctx, ownedHeroSMSWebhookLeaseSQL,
		activationID, leaseOwner, leaseVersion, now,
	).Scan(&providerActivationID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSWebhookEvent{}, ErrConflict
		}
		return domain.HeroSMSWebhookEvent{}, mapError(err)
	}
	// Attach callbacks which committed before CreateActivationAtomically. The
	// activation row lock serializes this with ingestion and lease release.
	if _, err = tx.Exec(ctx, `UPDATE hero_sms_webhook_events SET activation_id=$1
		WHERE activation_id IS NULL AND provider_activation_id=$2`,
		activationID, providerActivationID,
	); err != nil {
		return domain.HeroSMSWebhookEvent{}, mapError(err)
	}

	event, err := s.scanHeroSMSWebhookEvent(tx.QueryRow(ctx,
		claimNextHeroSMSWebhookEventSQL, activationID, leaseOwner, leaseVersion, now,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		// Preserve the early-event association even if every event is delayed or
		// already audited as processed.
		if result, refreshErr := tx.Exec(ctx, refreshHeroSMSWebhookWakeSQL,
			activationID, leaseOwner, leaseVersion,
		); refreshErr != nil {
			return domain.HeroSMSWebhookEvent{}, mapError(refreshErr)
		} else if result.RowsAffected() == 0 {
			return domain.HeroSMSWebhookEvent{}, ErrConflict
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return domain.HeroSMSWebhookEvent{}, fmt.Errorf("commit empty HeroSMS webhook claim: %w", commitErr)
		}
		return domain.HeroSMSWebhookEvent{}, ErrNotFound
	}
	if err != nil {
		return domain.HeroSMSWebhookEvent{}, mapError(err)
	}
	// The claimed event no longer needs to wake this activation. Preserve the
	// earliest other received event instead; callbacks which arrive after this
	// row lock is released will merge their own wake time atomically.
	if result, refreshErr := tx.Exec(ctx, refreshHeroSMSWebhookWakeSQL,
		activationID, leaseOwner, leaseVersion,
	); refreshErr != nil {
		return domain.HeroSMSWebhookEvent{}, mapError(refreshErr)
	} else if result.RowsAffected() == 0 {
		return domain.HeroSMSWebhookEvent{}, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.HeroSMSWebhookEvent{}, fmt.Errorf("%w: commit HeroSMS webhook claim: %v", ErrCommitUnknown, err)
	}
	return event, nil
}

func (s *PostgresStore) CompleteHeroSMSWebhookEventOwned(
	ctx context.Context,
	eventID, activationID int64,
	leaseOwner string,
	leaseVersion int64,
) error {
	return s.finishHeroSMSWebhookEventOwned(
		ctx, eventID, activationID, leaseOwner, leaseVersion,
		domain.HeroSMSWebhookEventProcessed, "",
	)
}

func (s *PostgresStore) IgnoreHeroSMSWebhookEventOwned(
	ctx context.Context,
	eventID, activationID int64,
	leaseOwner string,
	leaseVersion int64,
	reason string,
) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrInvalidInput
	}
	return s.finishHeroSMSWebhookEventOwned(
		ctx, eventID, activationID, leaseOwner, leaseVersion,
		domain.HeroSMSWebhookEventIgnored, reason,
	)
}

func (s *PostgresStore) finishHeroSMSWebhookEventOwned(
	ctx context.Context,
	eventID, activationID int64,
	leaseOwner string,
	leaseVersion int64,
	status domain.HeroSMSWebhookEventStatus,
	reason string,
) error {
	leaseOwner = strings.TrimSpace(leaseOwner)
	if eventID <= 0 || activationID <= 0 || leaseOwner == "" || leaseVersion <= 0 ||
		(status != domain.HeroSMSWebhookEventProcessed && status != domain.HeroSMSWebhookEventIgnored) {
		return ErrInvalidInput
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin HeroSMS webhook completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = validateOwnedHeroSMSWebhookActivation(
		ctx, tx, activationID, leaseOwner, leaseVersion, time.Now().UTC(),
	); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE hero_sms_webhook_events SET
		status=$5,
		last_error=CASE WHEN $5='ignored' THEN $6 ELSE last_error END,
		processed_at=now()
		WHERE id=$1 AND activation_id=$2 AND status='processing'
			AND claimed_lease_owner=$3 AND claimed_lease_version=$4`,
		eventID, activationID, leaseOwner, leaseVersion, string(status), reason,
	)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return heroSMSWebhookEventConflict(ctx, tx, eventID, activationID, leaseOwner, leaseVersion, status)
	}
	if result, refreshErr := tx.Exec(ctx, refreshHeroSMSWebhookWakeSQL,
		activationID, leaseOwner, leaseVersion,
	); refreshErr != nil {
		return mapError(refreshErr)
	} else if result.RowsAffected() == 0 {
		return ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit HeroSMS webhook completion: %v", ErrCommitUnknown, err)
	}
	return nil
}

func (s *PostgresStore) FailHeroSMSWebhookEventOwned(
	ctx context.Context,
	eventID, activationID int64,
	leaseOwner string,
	leaseVersion int64,
	retryAt time.Time,
	lastError string,
) error {
	leaseOwner = strings.TrimSpace(leaseOwner)
	lastError = strings.TrimSpace(lastError)
	if eventID <= 0 || activationID <= 0 || leaseOwner == "" || leaseVersion <= 0 ||
		retryAt.IsZero() || lastError == "" {
		return ErrInvalidInput
	}
	retryAt = retryAt.UTC()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin HeroSMS webhook retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = validateOwnedHeroSMSWebhookActivation(
		ctx, tx, activationID, leaseOwner, leaseVersion, time.Now().UTC(),
	); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE hero_sms_webhook_events SET
		status='received', last_error=$5, next_attempt_at=$6,
		claimed_lease_owner='', claimed_lease_version=0, processed_at=NULL
		WHERE id=$1 AND activation_id=$2 AND status='processing'
			AND claimed_lease_owner=$3 AND claimed_lease_version=$4`,
		eventID, activationID, leaseOwner, leaseVersion, lastError, retryAt,
	)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return heroSMSWebhookEventConflict(
			ctx, tx, eventID, activationID, "", 0, domain.HeroSMSWebhookEventReceived,
		)
	}
	if _, err = tx.Exec(ctx, wakeHeroSMSActivationSQL, activationID, retryAt); err != nil {
		return mapError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit HeroSMS webhook retry: %v", ErrCommitUnknown, err)
	}
	return nil
}

func validateOwnedHeroSMSWebhookActivation(
	ctx context.Context,
	tx pgx.Tx,
	activationID int64,
	leaseOwner string,
	leaseVersion int64,
	now time.Time,
) error {
	var providerActivationID string
	if err := tx.QueryRow(ctx, ownedHeroSMSWebhookLeaseSQL,
		activationID, leaseOwner, leaseVersion, now,
	).Scan(&providerActivationID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		return mapError(err)
	}
	return nil
}

func heroSMSWebhookEventConflict(
	ctx context.Context,
	tx pgx.Tx,
	eventID, activationID int64,
	leaseOwner string,
	leaseVersion int64,
	wantedStatus domain.HeroSMSWebhookEventStatus,
) error {
	var storedActivationID sql.NullInt64
	var status, owner string
	var version int64
	err := tx.QueryRow(ctx, `SELECT activation_id, status, claimed_lease_owner,
		claimed_lease_version FROM hero_sms_webhook_events WHERE id=$1`, eventID,
	).Scan(&storedActivationID, &status, &owner, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return mapError(err)
	}
	if storedActivationID.Valid && storedActivationID.Int64 == activationID &&
		domain.HeroSMSWebhookEventStatus(status) == wantedStatus &&
		(leaseOwner == "" || (owner == leaseOwner && version == leaseVersion)) {
		return nil
	}
	return ErrConflict
}
