package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mangobubu/gopay-autosms/internal/domain"
)

const batchColumns = `id, status, service_code, service_name, country_code, country_name,
	max_price_amount, currency, target_pin_enc, config, next_purchase_at, quantity, purchased_count, purchase_reserved_count, fulfilled_count, inflight_count,
	failure_reason, created_at, updated_at, started_at, finished_at`

const reserveBatchPurchaseSQL = `UPDATE batches SET
	purchase_reserved_count=purchase_reserved_count+1,
	started_at=COALESCE(started_at, now()),
	updated_at=now()
	WHERE id=$1
		AND status IN ('pending','running')
		AND purchase_protocol_version=1
		AND purchase_reserved_count=0
		AND purchased_count+purchase_reserved_count < quantity`

const releaseBatchPurchaseSQL = `UPDATE batches SET
	purchase_reserved_count=purchase_reserved_count-1,
	next_purchase_at=CASE WHEN status IN ('pending','running') THEN $2 ELSE next_purchase_at END,
	failure_reason=CASE WHEN status IN ('pending','running') THEN $3 ELSE failure_reason END,
	updated_at=now()
	WHERE id=$1 AND purchase_reserved_count > 0`

const freezeBatchPurchaseSQL = `UPDATE batches SET
	status=CASE WHEN status IN ('pending','running') THEN 'failed' ELSE status END,
	failure_reason=CASE WHEN status IN ('pending','running') THEN $2 ELSE failure_reason END,
	finished_at=CASE WHEN status IN ('pending','running') THEN COALESCE(finished_at, now()) ELSE finished_at END,
	updated_at=now()
	WHERE id=$1`

const conflictBatchPurchaseAttemptSQL = `UPDATE batch_purchase_attempts SET
	state='conflicted', failure_reason=$2, decided_at=COALESCE(decided_at, now()),
	cleanup_state='', cleanup_next_at=NULL,
	cleanup_lease_owner='', cleanup_lease_until=NULL,
	cleanup_lease_version=cleanup_lease_version+1
	WHERE token=$1 AND state IN ('reserved','sent','unknown','conflicted')`

// cancelBatchActivationsSQL queues unfinished activations for provider cleanup
// and invalidates any worker lease that may still be in flight. Activations
// already classified for a provider completion/cancellation retain that
// durable intent and visibility; every other activation is queued for deletion.
// Clearing the lease makes cleanup immediately claimable, while incrementing
// lease_version fences stale workers from their next write.
const cancelBatchActivationsSQL = `UPDATE activations SET
	control_action=CASE
		WHEN status IN ('pin_submission_blocked','login_code_timeout','pin_code_timeout') THEN control_action
		ELSE 'delete'
	END,
	hidden_at=CASE
		WHEN status IN ('pin_submission_blocked','login_code_timeout','pin_code_timeout')
			AND control_action='' THEN hidden_at
		ELSE COALESCE(hidden_at, now())
	END,
	lease_owner='', lease_until=NULL, lease_version=lease_version+1,
	next_run_at=now(), updated_at=now()
	WHERE batch_id=$1 AND finished_at IS NULL`

type rowScanner interface {
	Scan(...any) error
}

func scanBatch(row rowScanner) (domain.Batch, error) {
	var batch domain.Batch
	var status string
	var config []byte
	var startedAt, finishedAt sql.NullTime
	err := row.Scan(
		&batch.ID, &status, &batch.ServiceCode, &batch.ServiceName,
		&batch.CountryCode, &batch.CountryName, &batch.MaxPriceAmount,
		&batch.Currency, &batch.TargetPINEnc, &config, &batch.NextPurchaseAt, &batch.Quantity,
		&batch.PurchasedCount, &batch.PurchaseReservedCount, &batch.FulfilledCount, &batch.InflightCount, &batch.FailureReason,
		&batch.CreatedAt, &batch.UpdatedAt, &startedAt, &finishedAt,
	)
	if err != nil {
		return domain.Batch{}, err
	}
	batch.Status = domain.BatchStatus(status)
	batch.Config = cloneJSON(config)
	batch.ProxyAvailable, batch.ProxyTotal = proxyCounts(config)
	if startedAt.Valid {
		batch.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		batch.FinishedAt = &finishedAt.Time
	}
	return batch, nil
}

func proxyCounts(config []byte) (available, total int) {
	var value struct {
		ProxyPool []struct {
			Status string `json:"status"`
		} `json:"proxy_pool"`
	}
	if len(config) == 0 || json.Unmarshal(config, &value) != nil {
		return 0, 0
	}
	for _, item := range value.ProxyPool {
		total++
		if item.Status == "" || item.Status == "available" {
			available++
		}
	}
	return available, total
}

func (s *PostgresStore) CreateBatch(ctx context.Context, params CreateBatchParams) (domain.Batch, error) {
	params.ServiceCode = strings.TrimSpace(params.ServiceCode)
	params.CountryCode = strings.TrimSpace(params.CountryCode)
	params.Currency = strings.TrimSpace(params.Currency)
	params.MaxPriceAmount = strings.TrimSpace(params.MaxPriceAmount)
	if params.ServiceCode == "" || params.CountryCode == "" || params.MaxPriceAmount == "" || params.Quantity <= 0 {
		return domain.Batch{}, ErrInvalidInput
	}
	if len(params.TargetPINEnc) == 0 {
		return domain.Batch{}, fmt.Errorf("%w: encrypted PIN is required", ErrInvalidInput)
	}
	config := validJSONOrObject(params.Config)
	if !json.Valid(config) {
		return domain.Batch{}, fmt.Errorf("%w: malformed batch config", ErrInvalidInput)
	}
	query := `INSERT INTO batches(
		status, service_code, service_name, country_code, country_name,
		max_price_amount, currency, target_pin_enc, config, quantity, purchase_protocol_version
	) VALUES('running', $1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, 1)
	RETURNING ` + batchColumns
	batch, err := scanBatch(s.pool.QueryRow(ctx, query,
		params.ServiceCode, params.ServiceName, params.CountryCode, params.CountryName,
		params.MaxPriceAmount, params.Currency, params.TargetPINEnc, config, params.Quantity,
	))
	if err != nil {
		return domain.Batch{}, mapError(err)
	}
	return batch, nil
}

func (s *PostgresStore) GetBatch(ctx context.Context, id int64) (domain.Batch, error) {
	batch, err := scanBatch(s.pool.QueryRow(ctx, `SELECT `+batchColumns+` FROM batches WHERE id=$1`, id))
	if err != nil {
		return domain.Batch{}, mapError(err)
	}
	return batch, nil
}

func (s *PostgresStore) ReserveBatchPurchase(ctx context.Context, batchID int64, token string) error {
	token = strings.TrimSpace(token)
	if batchID <= 0 || token == "" {
		return ErrInvalidInput
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin purchase reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var quantity, purchased, reserved, protocolVersion int
	err = tx.QueryRow(ctx, `SELECT status, quantity, purchased_count,
		purchase_reserved_count, purchase_protocol_version
		FROM batches WHERE id=$1 FOR UPDATE`, batchID).
		Scan(&status, &quantity, &purchased, &reserved, &protocolVersion)
	if err != nil {
		return mapError(err)
	}

	var attemptBatchID int64
	var attemptState string
	err = tx.QueryRow(ctx, `SELECT batch_id, state FROM batch_purchase_attempts
		WHERE token=$1 FOR UPDATE`, token).Scan(&attemptBatchID, &attemptState)
	if err == nil {
		if attemptBatchID != batchID || attemptState != "reserved" || domain.BatchStatus(status).Terminal() {
			return ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("%w: commit idempotent purchase reservation: %v", ErrCommitUnknown, err)
		}
		return nil
	}
	if err != pgx.ErrNoRows {
		return mapError(err)
	}

	switch {
	case protocolVersion != 1:
		return ErrConflict
	case domain.BatchStatus(status).Terminal():
		return ErrConflict
	case reserved > 0:
		return ErrPurchaseInProgress
	case purchased+reserved >= quantity:
		return ErrBatchCapacity
	}

	if _, err = tx.Exec(ctx, `INSERT INTO batch_purchase_attempts(token, batch_id, state)
		VALUES($1,$2,'reserved')`, token, batchID); err != nil {
		return mapError(err)
	}
	result, err := tx.Exec(ctx, reserveBatchPurchaseSQL, batchID)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit purchase reservation: %v", ErrCommitUnknown, err)
	}
	return nil
}

func (s *PostgresStore) MarkBatchPurchaseSent(ctx context.Context, batchID int64, token string) error {
	token = strings.TrimSpace(token)
	if batchID <= 0 || token == "" {
		return ErrInvalidInput
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin purchase send fence: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM batches WHERE id=$1 FOR UPDATE`, batchID).Scan(&status); err != nil {
		return mapError(err)
	}
	var attemptBatchID int64
	var state string
	if err = tx.QueryRow(ctx, `SELECT batch_id, state FROM batch_purchase_attempts
		WHERE token=$1 FOR UPDATE`, token).Scan(&attemptBatchID, &state); err != nil {
		return mapError(err)
	}
	if attemptBatchID != batchID || domain.BatchStatus(status).Terminal() {
		return ErrConflict
	}
	switch state {
	case string(PurchaseAttemptSent):
		// A retry after an uncertain COMMIT has not contacted the provider yet;
		// observing sent under the row lock resolves that uncertainty.
	case string(PurchaseAttemptReserved):
		result, updateErr := tx.Exec(ctx, `UPDATE batch_purchase_attempts SET state='sent'
			WHERE token=$1 AND batch_id=$2 AND state='reserved'`, token, batchID)
		if updateErr != nil {
			return mapError(updateErr)
		}
		if result.RowsAffected() == 0 {
			return ErrConflict
		}
	default:
		return ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit purchase send fence: %v", ErrCommitUnknown, err)
	}
	return nil
}

func (s *PostgresStore) ReleaseBatchPurchaseReservation(
	ctx context.Context,
	batchID int64,
	token string,
	next time.Time,
	reason string,
) error {
	token = strings.TrimSpace(token)
	if batchID <= 0 || token == "" {
		return ErrInvalidInput
	}
	if next.IsZero() {
		next = time.Now().UTC()
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin purchase reservation release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = tx.QueryRow(ctx, `SELECT id FROM batches WHERE id=$1 FOR UPDATE`, batchID).Scan(&batchID); err != nil {
		return mapError(err)
	}

	var attemptBatchID int64
	var state string
	err = tx.QueryRow(ctx, `SELECT batch_id, state FROM batch_purchase_attempts
		WHERE token=$1 FOR UPDATE`, token).Scan(&attemptBatchID, &state)
	if err == pgx.ErrNoRows {
		// The reservation COMMIT did not take effect. No provider call can have
		// occurred for this token, so scheduling the next attempt is sufficient.
		if _, err = tx.Exec(ctx, `UPDATE batches SET
			next_purchase_at=CASE WHEN status IN ('pending','running') THEN $2 ELSE next_purchase_at END,
			failure_reason=CASE WHEN status IN ('pending','running') THEN $3 ELSE failure_reason END,
			updated_at=now() WHERE id=$1`, batchID, next, reason); err != nil {
			return mapError(err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("%w: commit absent purchase release: %v", ErrCommitUnknown, err)
		}
		return nil
	}
	if err != nil {
		return mapError(err)
	}
	if attemptBatchID != batchID {
		return ErrConflict
	}
	if state == "released" {
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("%w: commit idempotent purchase release: %v", ErrCommitUnknown, err)
		}
		return nil
	}
	if state != string(PurchaseAttemptReserved) && state != string(PurchaseAttemptSent) {
		return ErrConflict
	}
	result, err := tx.Exec(ctx, `UPDATE batch_purchase_attempts SET
		state='released', failure_reason=$2, decided_at=now()
		WHERE token=$1 AND state IN ('reserved','sent')`, token, reason)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrConflict
	}
	result, err = tx.Exec(ctx, releaseBatchPurchaseSQL, batchID, next, reason)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit purchase release: %v", ErrCommitUnknown, err)
	}
	return nil
}

func (s *PostgresStore) FreezeBatchPurchase(
	ctx context.Context,
	batchID int64,
	token, provider, providerActivationID, reason string,
) (PurchaseAttemptState, error) {
	token = strings.TrimSpace(token)
	provider = strings.TrimSpace(provider)
	providerActivationID = strings.TrimSpace(providerActivationID)
	reason = strings.TrimSpace(reason)
	if batchID <= 0 || token == "" || reason == "" {
		return "", ErrInvalidInput
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin purchase freeze: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = tx.QueryRow(ctx, `SELECT id FROM batches WHERE id=$1 FOR UPDATE`, batchID).Scan(&batchID); err != nil {
		return "", mapError(err)
	}

	var attemptBatchID int64
	var state, storedProvider, storedProviderID string
	err = tx.QueryRow(ctx, `SELECT batch_id, state, provider, provider_activation_id
		FROM batch_purchase_attempts WHERE token=$1 FOR UPDATE`, token).
		Scan(&attemptBatchID, &state, &storedProvider, &storedProviderID)
	if err != nil {
		return "", mapError(err)
	}
	if attemptBatchID != batchID {
		return "", ErrConflict
	}
	effectiveProvider, effectiveProviderID, identityErr := resolveProviderActivationIdentity(
		storedProvider, storedProviderID, provider, providerActivationID,
	)
	if identityErr != nil {
		return "", ErrConflict
	}
	if state != string(PurchaseAttemptCommitted) && effectiveProvider != "" && effectiveProviderID != "" {
		if err = lockProviderActivation(ctx, tx, effectiveProvider, effectiveProviderID); err != nil {
			return "", err
		}
		var ownedByOtherAttempt, activationExists bool
		lookupErr := tx.QueryRow(ctx, `SELECT
			EXISTS(SELECT 1 FROM batch_purchase_attempts
				WHERE provider=$1 AND provider_activation_id=$2 AND token<>$3),
			EXISTS(SELECT 1 FROM activations
				WHERE provider=$1 AND provider_activation_id=$2)`,
			effectiveProvider, effectiveProviderID, token).Scan(&ownedByOtherAttempt, &activationExists)
		if lookupErr != nil {
			return "", mapError(lookupErr)
		}
		if ownedByOtherAttempt || activationExists {
			if state != string(PurchaseAttemptReserved) && state != string(PurchaseAttemptSent) &&
				state != string(PurchaseAttemptUnknown) && state != string(PurchaseAttemptConflicted) {
				return "", ErrConflict
			}
			// Do not copy the conflicting provider key onto this token. The unique
			// ownership index belongs to the existing attempt; this token only
			// records the conflict and retains its reserved quota for audit.
			result, updateErr := tx.Exec(ctx, conflictBatchPurchaseAttemptSQL, token, reason)
			if updateErr != nil {
				return "", mapError(updateErr)
			}
			if result.RowsAffected() == 0 {
				return "", ErrConflict
			}
			if _, updateErr = tx.Exec(ctx, freezeBatchPurchaseSQL, batchID,
				"供应商号码已归属其他购买记录，已停止自动购号: "+reason); updateErr != nil {
				return "", mapError(updateErr)
			}
			if err = tx.Commit(ctx); err != nil {
				return "", fmt.Errorf("%w: commit conflicted purchase freeze: %v", ErrCommitUnknown, err)
			}
			return PurchaseAttemptConflicted, nil
		}
	}
	switch state {
	case "committed":
		// The activation transaction actually committed before its connection
		// reported an uncertain outcome. Do not fail the healthy batch.
		if err = tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("%w: commit resolved purchase freeze: %v", ErrCommitUnknown, err)
		}
		return PurchaseAttemptCommitted, nil
	case "reserved", "sent":
		result, updateErr := tx.Exec(ctx, `UPDATE batch_purchase_attempts SET
			state='unknown', provider=CASE WHEN $2='' THEN provider ELSE $2 END,
			provider_activation_id=CASE WHEN $3='' THEN provider_activation_id ELSE $3 END,
			failure_reason=$4, decided_at=now(),
			cleanup_state=CASE
				WHEN btrim(CASE WHEN $2='' THEN provider ELSE $2 END) <> ''
					AND btrim(CASE WHEN $3='' THEN provider_activation_id ELSE $3 END) <> '' THEN 'pending'
				ELSE '' END,
			cleanup_next_at=CASE
				WHEN btrim(CASE WHEN $2='' THEN provider ELSE $2 END) <> ''
					AND btrim(CASE WHEN $3='' THEN provider_activation_id ELSE $3 END) <> '' THEN now()
				ELSE NULL END
			WHERE token=$1 AND state IN ('reserved','sent')`, token, provider, providerActivationID, reason)
		if updateErr != nil {
			return "", mapError(updateErr)
		}
		if result.RowsAffected() == 0 {
			return "", ErrConflict
		}
		state = string(PurchaseAttemptUnknown)
	case "unknown":
		if _, err = tx.Exec(ctx, `UPDATE batch_purchase_attempts SET
			provider=CASE WHEN $2='' THEN provider ELSE $2 END,
			provider_activation_id=CASE WHEN $3='' THEN provider_activation_id ELSE $3 END,
			failure_reason=$4,
			cleanup_state=CASE
				WHEN cleanup_state='done' THEN 'done'
				WHEN btrim(CASE WHEN $2='' THEN provider ELSE $2 END) <> ''
					AND btrim(CASE WHEN $3='' THEN provider_activation_id ELSE $3 END) <> '' THEN 'pending'
				ELSE cleanup_state END,
			cleanup_next_at=CASE
				WHEN cleanup_state='done' THEN NULL
				WHEN btrim(CASE WHEN $2='' THEN provider ELSE $2 END) <> ''
					AND btrim(CASE WHEN $3='' THEN provider_activation_id ELSE $3 END) <> ''
					THEN COALESCE(cleanup_next_at, now())
				ELSE cleanup_next_at END
			WHERE token=$1`, token, provider, providerActivationID, reason); err != nil {
			return "", mapError(err)
		}
	case "conflicted":
		if _, err = tx.Exec(ctx, `UPDATE batch_purchase_attempts SET
			provider=CASE WHEN $2='' THEN provider ELSE $2 END,
			provider_activation_id=CASE WHEN $3='' THEN provider_activation_id ELSE $3 END,
			failure_reason=$4 WHERE token=$1`, token, provider, providerActivationID, reason); err != nil {
			return "", mapError(err)
		}
	default:
		return "", ErrConflict
	}
	if _, err = tx.Exec(ctx, freezeBatchPurchaseSQL, batchID, reason); err != nil {
		return "", mapError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("%w: commit purchase freeze: %v", ErrCommitUnknown, err)
	}
	return PurchaseAttemptState(state), nil
}

func resolveProviderActivationIdentity(
	storedProvider, storedProviderID, provider, providerID string,
) (string, string, error) {
	if storedProvider != "" && provider != "" && storedProvider != provider {
		return "", "", ErrConflict
	}
	if storedProviderID != "" && providerID != "" && storedProviderID != providerID {
		return "", "", ErrConflict
	}
	if provider == "" {
		provider = storedProvider
	}
	if providerID == "" {
		providerID = storedProviderID
	}
	return provider, providerID, nil
}

func (s *PostgresStore) RecoverBatchPurchaseOnStartup(ctx context.Context, batchID int64) error {
	if batchID <= 0 {
		return ErrInvalidInput
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin startup purchase recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = tx.QueryRow(ctx, `SELECT id FROM batches WHERE id=$1 FOR UPDATE`, batchID).Scan(&batchID); err != nil {
		return mapError(err)
	}
	var token, state string
	err = tx.QueryRow(ctx, `SELECT token, state FROM batch_purchase_attempts
		WHERE batch_id=$1 AND state IN ('reserved','sent','unknown','conflicted')
		LIMIT 1 FOR UPDATE`, batchID).Scan(&token, &state)
	if err == pgx.ErrNoRows {
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("%w: commit empty startup purchase recovery: %v", ErrCommitUnknown, err)
		}
		return nil
	}
	if err != nil {
		return mapError(err)
	}
	if PurchaseAttemptState(state) == PurchaseAttemptSent {
		reason := "服务在购号请求完成前停止，购买结果未知；已保留名额并停止自动补购"
		// Keep the batch active in this transaction. Startup cancellation is a
		// separate retryable step; if this COMMIT response is lost, the next
		// process must still list the batch and finish activation fencing.
		result, updateErr := tx.Exec(ctx, `UPDATE batch_purchase_attempts SET
			state='unknown', failure_reason=$2, decided_at=now()
			WHERE token=$1 AND state='sent'`, token, reason)
		if updateErr != nil {
			return mapError(updateErr)
		}
		if result.RowsAffected() == 0 {
			return ErrConflict
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit startup purchase recovery: %v", ErrCommitUnknown, err)
	}
	return nil
}

func (s *PostgresStore) ClaimPurchaseCleanupAttempts(
	ctx context.Context,
	owner string,
	now time.Time,
	leaseDuration time.Duration,
	limit int,
) ([]PurchaseCleanupAttempt, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || leaseDuration <= 0 {
		return nil, ErrInvalidInput
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `WITH candidates AS (
		SELECT token FROM batch_purchase_attempts
		WHERE state='unknown' AND cleanup_state='pending'
			AND cleanup_next_at <= $2
			AND (cleanup_lease_until IS NULL OR cleanup_lease_until <= $2)
		ORDER BY cleanup_next_at, reserved_at, token
		FOR UPDATE SKIP LOCKED LIMIT $4
	)
	UPDATE batch_purchase_attempts a SET
		cleanup_lease_owner=$1, cleanup_lease_until=$3,
		cleanup_lease_version=a.cleanup_lease_version+1
	FROM candidates c WHERE a.token=c.token
	RETURNING a.token, a.batch_id, a.provider, a.provider_activation_id,
		a.cleanup_lease_owner, a.cleanup_lease_version`,
		owner, now, now.Add(leaseDuration), limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	result := make([]PurchaseCleanupAttempt, 0)
	for rows.Next() {
		var item PurchaseCleanupAttempt
		if err = rows.Scan(&item.Token, &item.BatchID, &item.Provider, &item.ProviderActivationID,
			&item.LeaseOwner, &item.LeaseVersion); err != nil {
			return nil, mapError(err)
		}
		result = append(result, item)
	}
	if err = rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return result, nil
}

func (s *PostgresStore) CompletePurchaseCleanup(ctx context.Context, token, owner string, leaseVersion int64) error {
	token = strings.TrimSpace(token)
	owner = strings.TrimSpace(owner)
	if token == "" || owner == "" || leaseVersion <= 0 {
		return ErrInvalidInput
	}
	result, err := s.pool.Exec(ctx, `UPDATE batch_purchase_attempts SET
		cleanup_state='done', cleanup_next_at=NULL,
		cleanup_lease_owner='', cleanup_lease_until=NULL,
		cleanup_failure_reason=''
		WHERE token=$1 AND state='unknown' AND cleanup_state='pending'
			AND cleanup_lease_owner=$2 AND cleanup_lease_version=$3`, token, owner, leaseVersion)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresStore) RetryPurchaseCleanup(
	ctx context.Context,
	token, owner string,
	leaseVersion int64,
	next time.Time,
	reason string,
) error {
	token = strings.TrimSpace(token)
	owner = strings.TrimSpace(owner)
	if token == "" || owner == "" || leaseVersion <= 0 {
		return ErrInvalidInput
	}
	if next.IsZero() {
		next = time.Now().UTC()
	}
	result, err := s.pool.Exec(ctx, `UPDATE batch_purchase_attempts SET
		cleanup_next_at=$4, cleanup_lease_owner='', cleanup_lease_until=NULL,
		cleanup_failure_reason=$5
		WHERE token=$1 AND state='unknown' AND cleanup_state='pending'
			AND cleanup_lease_owner=$2 AND cleanup_lease_version=$3`,
		token, owner, leaseVersion, next, reason)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresStore) ScheduleBatchPurchase(ctx context.Context, id int64, next time.Time, reason string) error {
	result, err := s.pool.Exec(ctx, `UPDATE batches SET next_purchase_at=$2,
		failure_reason=$3, updated_at=now() WHERE id=$1 AND status IN ('pending','running')`, id, next, reason)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return s.notFoundOrConflict(ctx, "batches", id)
	}
	return nil
}

func (s *PostgresStore) UpdateBatchConfig(ctx context.Context, id int64, config json.RawMessage) error {
	if len(config) == 0 || !json.Valid(config) {
		return ErrInvalidInput
	}
	result, err := s.pool.Exec(ctx, `UPDATE batches SET config=$2::jsonb, updated_at=now() WHERE id=$1`, id, config)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListBatches(ctx context.Context, filter BatchFilter) ([]domain.Batch, error) {
	query, args := buildBatchListQuery(filter)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list batches: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Batch, 0)
	for rows.Next() {
		batch, scanErr := scanBatch(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan batch: %w", scanErr)
		}
		result = append(result, batch)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate batches: %w", err)
	}
	return result, nil
}

func buildBatchListQuery(filter BatchFilter) (string, []any) {
	page := normalizePage(filter.Page)
	query := `SELECT ` + batchColumns + ` FROM batches`
	args := make([]any, 0, 3)
	if len(filter.Statuses) > 0 {
		statuses := make([]string, 0, len(filter.Statuses))
		for _, status := range filter.Statuses {
			statuses = append(statuses, string(status))
		}
		args = append(args, statuses)
		query += ` WHERE status=ANY($1::text[])`
	}
	args = append(args, page.Limit)
	limitPos := len(args)
	args = append(args, page.Offset)
	query += ` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(limitPos) + ` OFFSET $` + strconv.Itoa(limitPos+1)
	return query, args
}

func (s *PostgresStore) TransitionBatch(
	ctx context.Context,
	id int64,
	expected []domain.BatchStatus,
	next domain.BatchStatus,
	failureReason string,
) (domain.Batch, error) {
	if !next.Valid() {
		return domain.Batch{}, ErrInvalidInput
	}
	expectedText := make([]string, 0, len(expected))
	for _, status := range expected {
		if !status.Valid() {
			return domain.Batch{}, ErrInvalidInput
		}
		expectedText = append(expectedText, string(status))
	}
	query := `UPDATE batches SET
		status=$2,
		failure_reason=$3,
		started_at=CASE WHEN $2='running' THEN COALESCE(started_at, now()) ELSE started_at END,
		finished_at=CASE WHEN $2=ANY(ARRAY['completed','cancelled','failed']) THEN COALESCE(finished_at, now()) ELSE NULL END,
		updated_at=now()
	WHERE id=$1 AND (cardinality($4::text[])=0 OR status=ANY($4::text[]))
	RETURNING ` + batchColumns
	batch, err := scanBatch(s.pool.QueryRow(ctx, query, id, string(next), failureReason, expectedText))
	if err == nil {
		return batch, nil
	}
	if !errorsIsNoRows(err) {
		return domain.Batch{}, mapError(err)
	}
	return domain.Batch{}, s.notFoundOrConflict(ctx, "batches", id)
}

func (s *PostgresStore) CancelBatch(ctx context.Context, id int64) (domain.Batch, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Batch{}, fmt.Errorf("begin batch cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock the batch first. Activation terminal transitions use the same order,
	// so stopping a task cannot deadlock with a worker finishing a number.
	batch, err := scanBatch(tx.QueryRow(ctx, `SELECT `+batchColumns+`
		FROM batches WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return domain.Batch{}, mapError(err)
	}
	var unresolvedToken, unresolvedState string
	err = tx.QueryRow(ctx, `SELECT token, state FROM batch_purchase_attempts
		WHERE batch_id=$1 AND state IN ('reserved','sent','unknown','conflicted')
		LIMIT 1 FOR UPDATE`, id).Scan(&unresolvedToken, &unresolvedState)
	if err != nil && err != pgx.ErrNoRows {
		return domain.Batch{}, mapError(err)
	}
	if err == nil {
		switch PurchaseAttemptState(unresolvedState) {
		case PurchaseAttemptSent:
			// Another instance has crossed the durable pre-request fence. Keep
			// the batch active until that request resolves so Stop never reports
			// success before a provider allocation can arrive.
			return domain.Batch{}, ErrPurchaseInProgress
		case PurchaseAttemptReserved:
			result, updateErr := tx.Exec(ctx, `UPDATE batch_purchase_attempts SET
				state='released', failure_reason='任务已在发送购号请求前停止', decided_at=now()
				WHERE token=$1 AND state='reserved'`, unresolvedToken)
			if updateErr != nil {
				return domain.Batch{}, mapError(updateErr)
			}
			if result.RowsAffected() == 0 {
				return domain.Batch{}, ErrConflict
			}
			if _, updateErr = tx.Exec(ctx, `UPDATE batches SET
				purchase_reserved_count=purchase_reserved_count-1, updated_at=now()
				WHERE id=$1 AND purchase_reserved_count > 0`, id); updateErr != nil {
				return domain.Batch{}, mapError(updateErr)
			}
		case PurchaseAttemptUnknown, PurchaseAttemptConflicted:
			// The quota stays consumed because the provider outcome is unknown.
		}
	}
	if batch.Status == domain.BatchStatusPending || batch.Status == domain.BatchStatusRunning {
		batch, err = scanBatch(tx.QueryRow(ctx, `UPDATE batches SET
			status='cancelled', finished_at=COALESCE(finished_at, now()), updated_at=now()
			WHERE id=$1 AND status IN ('pending','running') RETURNING `+batchColumns, id))
		if err != nil {
			return domain.Batch{}, mapError(err)
		}
	}
	// Provider work is intentionally not finalized here. Workers must execute
	// each durable cleanup intent (including a protected PIN-blocked completion)
	// before finalizing the activation.
	if _, err = tx.Exec(ctx, cancelBatchActivationsSQL, id); err != nil {
		return domain.Batch{}, mapError(err)
	}
	// Unknown purchase outcomes intentionally retain their token and quota. A
	// later reconciliation can identify the provider allocation, while this
	// terminal batch can never refill the retained slot.
	if err = tx.Commit(ctx); err != nil {
		return domain.Batch{}, fmt.Errorf("%w: commit batch cancellation: %v", ErrCommitUnknown, err)
	}
	return batch, nil
}

func errorsIsNoRows(err error) bool {
	return err == pgx.ErrNoRows
}

func (s *PostgresStore) notFoundOrConflict(ctx context.Context, table string, id int64) error {
	// table is selected only by package-owned call sites; never pass user input.
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, id).Scan(&exists)
	if err != nil {
		return mapError(err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrConflict
}
