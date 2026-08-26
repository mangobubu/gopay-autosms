package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mangobubu/gopay-autosms/internal/domain"
)

const verificationColumns = `id, activation_id, cycle_no, phase, ordinal, code,
	provider_payload, provider_received_at, created_at`

func scanVerification(row rowScanner) (domain.VerificationCode, error) {
	var verification domain.VerificationCode
	var phase string
	var payload []byte
	var providerReceivedAt sql.NullTime
	err := row.Scan(
		&verification.ID, &verification.ActivationID, &verification.CycleNo,
		&phase, &verification.Ordinal, &verification.Code, &payload,
		&providerReceivedAt, &verification.CreatedAt,
	)
	if err != nil {
		return domain.VerificationCode{}, err
	}
	verification.Phase = domain.VerificationPhase(phase)
	verification.ProviderPayload = cloneJSON(payload)
	if providerReceivedAt.Valid {
		verification.ProviderReceivedAt = &providerReceivedAt.Time
	}
	return verification, nil
}

func (s *PostgresStore) AppendVerificationCode(
	ctx context.Context,
	params AppendVerificationParams,
) (AppendVerificationResult, error) {
	params.Code = strings.TrimSpace(params.Code)
	if params.ActivationID <= 0 || params.CycleNo < 0 || params.Code == "" || !params.Phase.Valid() {
		return AppendVerificationResult{}, ErrInvalidInput
	}
	payload := validJSONOrObject(params.ProviderPayload)
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
	// Ordinal is an append-only sequence across the activation. Login and PIN
	// remain separately labelled, while subsequent messages render as 1, 2, 3…
	query := `INSERT INTO verification_codes(
		activation_id, cycle_no, phase, ordinal, code, provider_payload, provider_received_at
	) SELECT $1,$2,$3,
		CASE WHEN $3='subsequent' THEN COALESCE((SELECT max(ordinal)+1 FROM verification_codes WHERE activation_id=$1 AND phase='subsequent'),1) ELSE 0 END,
		$4,$5::jsonb,$6
	ON CONFLICT(activation_id, cycle_no) DO NOTHING
	RETURNING ` + verificationColumns
	verification, err := scanVerification(tx.QueryRow(ctx, query,
		params.ActivationID, params.CycleNo, string(params.Phase), params.Code,
		payload, params.ProviderReceivedAt,
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
	verification, err = scanVerification(tx.QueryRow(ctx,
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
		verification, scanErr := scanVerification(rows)
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
