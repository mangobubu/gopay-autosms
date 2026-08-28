package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mangobubu/gopay-autosms/internal/domain"
)

const verificationColumns = `id, activation_id, cycle_no, phase, ordinal, code,
	provider_payload, provider_received_at, created_at, sensitive_enc`

const ownedVerificationLeaseSQL = `SELECT id FROM activations
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3 AND finished_at IS NULL
	FOR UPDATE`

func (s *PostgresStore) scanVerification(row rowScanner) (domain.VerificationCode, error) {
	var verification domain.VerificationCode
	var phase string
	var legacyCode string
	var payload, sensitive []byte
	var providerReceivedAt sql.NullTime
	err := row.Scan(
		&verification.ID, &verification.ActivationID, &verification.CycleNo,
		&phase, &verification.Ordinal, &legacyCode, &payload,
		&providerReceivedAt, &verification.CreatedAt, &sensitive,
	)
	if err != nil {
		return domain.VerificationCode{}, err
	}
	verification.Phase = domain.VerificationPhase(phase)
	verification.Code = legacyCode
	verification.ProviderPayload = cloneJSON(payload)
	if len(sensitive) > 0 {
		envelope, openErr := openVerificationSensitive(
			s.protector, sensitive, verification.ActivationID, verification.CycleNo,
		)
		if openErr != nil {
			return domain.VerificationCode{}, fmt.Errorf("decrypt verification %d: %w", verification.ID, openErr)
		}
		verification.Code = envelope.Code
		verification.ProviderPayload = cloneJSON(envelope.ProviderPayload)
	}
	if providerReceivedAt.Valid {
		verification.ProviderReceivedAt = &providerReceivedAt.Time
	}
	return verification, nil
}

func (s *PostgresStore) AppendVerificationCode(
	ctx context.Context,
	params AppendVerificationParams,
) (AppendVerificationResult, error) {
	return s.appendVerificationCode(ctx, params, "", 0, false)
}

func (s *PostgresStore) AppendVerificationCodeOwned(
	ctx context.Context,
	params AppendVerificationParams,
	owner string,
	leaseVersion int64,
) (AppendVerificationResult, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || leaseVersion <= 0 {
		return AppendVerificationResult{}, ErrInvalidInput
	}
	return s.appendVerificationCode(ctx, params, owner, leaseVersion, true)
}

func (s *PostgresStore) appendVerificationCode(
	ctx context.Context,
	params AppendVerificationParams,
	owner string,
	leaseVersion int64,
	requireLease bool,
) (AppendVerificationResult, error) {
	params.Code = strings.TrimSpace(params.Code)
	if params.ActivationID <= 0 || params.CycleNo < 0 || params.Code == "" || !params.Phase.Valid() {
		return AppendVerificationResult{}, ErrInvalidInput
	}
	payload := validJSONOrObject(params.ProviderPayload)
	sensitive, err := sealVerificationSensitive(
		s.protector, params.ActivationID, params.CycleNo, params.Code, payload,
	)
	if err != nil {
		return AppendVerificationResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AppendVerificationResult{}, fmt.Errorf("begin append verification: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize per activation. This makes allocation of subsequent ordinals
	// race-free while the cycle unique key makes every two-second poll idempotent.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, params.ActivationID); err != nil {
		return AppendVerificationResult{}, mapError(err)
	}
	// The worker lease is checked while holding the activation row lock. Task
	// cancellation clears the owner and increments lease_version, so a worker
	// from an older process cannot append after cancellation has committed.
	if requireLease {
		var activationID int64
		if err = tx.QueryRow(ctx, ownedVerificationLeaseSQL,
			params.ActivationID, owner, leaseVersion,
		).Scan(&activationID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return AppendVerificationResult{}, ErrConflict
			}
			return AppendVerificationResult{}, mapError(err)
		}
	}
	// Ordinal is an append-only sequence across the activation. Login and PIN
	// remain separately labelled, while subsequent messages render as 1, 2, 3…
	query := `INSERT INTO verification_codes(
		activation_id, cycle_no, phase, ordinal, code, provider_payload, provider_received_at, sensitive_enc
	) SELECT $1,$2,$3,
		CASE WHEN $3='subsequent' THEN COALESCE((SELECT max(ordinal)+1 FROM verification_codes WHERE activation_id=$1 AND phase='subsequent'),1) ELSE 0 END,
		'','{}'::jsonb,$4,$5
	ON CONFLICT(activation_id, cycle_no) DO NOTHING
	RETURNING ` + verificationColumns
	verification, err := s.scanVerification(tx.QueryRow(ctx, query,
		params.ActivationID, params.CycleNo, string(params.Phase),
		params.ProviderReceivedAt, sensitive,
	))
	if err == nil {
		if err = tx.Commit(ctx); err != nil {
			return AppendVerificationResult{}, fmt.Errorf("commit verification: %w", err)
		}
		return AppendVerificationResult{Verification: verification, Inserted: true}, nil
	}
	if err != pgx.ErrNoRows {
		return AppendVerificationResult{}, mapError(err)
	}
	verification, err = s.scanVerification(tx.QueryRow(ctx,
		`SELECT `+verificationColumns+` FROM verification_codes WHERE activation_id=$1 AND cycle_no=$2`,
		params.ActivationID, params.CycleNo,
	))
	if err != nil {
		return AppendVerificationResult{}, mapError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return AppendVerificationResult{}, fmt.Errorf("commit existing verification: %w", err)
	}
	return AppendVerificationResult{Verification: verification, Inserted: false}, nil
}

func (s *PostgresStore) ListVerificationCodes(ctx context.Context, activationID int64) ([]domain.VerificationCode, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+verificationColumns+` FROM verification_codes
		WHERE activation_id=$1
		ORDER BY cycle_no, id`, activationID)
	if err != nil {
		return nil, fmt.Errorf("list verification codes: %w", err)
	}
	defer rows.Close()
	result := make([]domain.VerificationCode, 0)
	for rows.Next() {
		verification, scanErr := s.scanVerification(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan verification code: %w", scanErr)
		}
		result = append(result, verification)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verification codes: %w", err)
	}
	return result, nil
}
