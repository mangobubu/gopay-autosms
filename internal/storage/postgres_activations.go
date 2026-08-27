package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mangobubu/gopay-autosms/internal/domain"
)

const activationColumns = `id, batch_id, account_id, provider, provider_activation_id,
	phone_number, phone_fingerprint, service_code, country_code, operator,
	purchase_price_amount, currency, status, failure_reason, balance_rp,
	balance_checked_at, ever_fulfilled, slot_reserved, sms_cycle, next_run_at, lease_owner,
	lease_until, lease_version, control_action, provider_payload, provider_expires_at,
	last_polled_at, hidden_at, created_at, updated_at, finished_at`

const providerActivationLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1, 1))`

const recordBatchPurchaseSQL = `UPDATE batches SET
	purchase_reserved_count=purchase_reserved_count-1,
	purchased_count=purchased_count+1,
	inflight_count=inflight_count+1,
	status=CASE WHEN status IN ('pending','running') THEN 'running' ELSE status END,
	started_at=COALESCE(started_at, now()),
	updated_at=now()
	WHERE id=$1
		AND purchase_reserved_count > 0
		AND purchased_count < quantity`

const releaseBatchSlotSQL = `UPDATE batches SET
	fulfilled_count=fulfilled_count+CASE WHEN $2::boolean THEN 1 ELSE 0 END,
	inflight_count=GREATEST(inflight_count-1, 0),
	updated_at=now()
	WHERE id=$1 AND (NOT $2::boolean OR fulfilled_count < quantity)`

func lockProviderActivation(ctx context.Context, tx pgx.Tx, provider, providerID string) error {
	// Use a length-prefixed key and a namespace seed distinct from phone
	// fingerprints. The same lock is taken by persistence and failure
	// resolution, so a no-row observation cannot race an uncommitted insert.
	key := strconv.Itoa(len(provider)) + ":" + provider + providerID
	if _, err := tx.Exec(ctx, providerActivationLockSQL, key); err != nil {
		return mapError(err)
	}
	return nil
}

func scanActivation(row rowScanner) (domain.Activation, error) {
	var activation domain.Activation
	var accountID sql.NullInt64
	var balance sql.NullFloat64
	var balanceCheckedAt, leaseUntil, providerExpiresAt sql.NullTime
	var lastPolledAt, hiddenAt, finishedAt sql.NullTime
	var status, controlAction string
	var providerPayload []byte
	err := row.Scan(
		&activation.ID, &activation.BatchID, &accountID, &activation.Provider,
		&activation.ProviderActivationID, &activation.PhoneNumber, &activation.PhoneFingerprint,
		&activation.ServiceCode, &activation.CountryCode, &activation.Operator,
		&activation.PurchasePriceAmount, &activation.Currency, &status,
		&activation.FailureReason, &balance, &balanceCheckedAt, &activation.EverFulfilled,
		&activation.SlotReserved, &activation.SMSCycle, &activation.NextRunAt, &activation.LeaseOwner, &leaseUntil,
		&activation.LeaseVersion,
		&controlAction, &providerPayload, &providerExpiresAt, &lastPolledAt, &hiddenAt,
		&activation.CreatedAt, &activation.UpdatedAt, &finishedAt,
	)
	if err != nil {
		return domain.Activation{}, err
	}
	activation.Status = domain.ActivationStatus(status)
	activation.ControlAction = domain.ControlAction(controlAction)
	activation.ProviderPayload = cloneJSON(providerPayload)
	if accountID.Valid {
		activation.AccountID = &accountID.Int64
	}
	if balance.Valid {
		activation.BalanceRP = &balance.Float64
	}
	if balanceCheckedAt.Valid {
		activation.BalanceCheckedAt = &balanceCheckedAt.Time
	}
	if leaseUntil.Valid {
		activation.LeaseUntil = &leaseUntil.Time
	}
	if providerExpiresAt.Valid {
		activation.ProviderExpiresAt = &providerExpiresAt.Time
	}
	if lastPolledAt.Valid {
		activation.LastPolledAt = &lastPolledAt.Time
	}
	if hiddenAt.Valid {
		activation.HiddenAt = &hiddenAt.Time
	}
	if finishedAt.Valid {
		activation.FinishedAt = &finishedAt.Time
	}
	return activation, nil
}

func (s *PostgresStore) CreateActivationAtomically(
	ctx context.Context,
	params CreateActivationParams,
) (CreateActivationResult, error) {
	params.PurchaseToken = strings.TrimSpace(params.PurchaseToken)
	normalized, err := domain.NormalizePhone(params.PhoneNumber)
	if err != nil {
		return CreateActivationResult{}, ErrInvalidInput
	}
	params.Provider = strings.TrimSpace(params.Provider)
	params.ProviderActivationID = strings.TrimSpace(params.ProviderActivationID)
	params.ServiceCode = strings.TrimSpace(params.ServiceCode)
	params.CountryCode = strings.TrimSpace(params.CountryCode)
	params.PurchasePriceAmount = strings.TrimSpace(params.PurchasePriceAmount)
	if params.PurchaseToken == "" || params.BatchID <= 0 || params.Provider == "" || params.ProviderActivationID == "" ||
		params.ServiceCode == "" || params.CountryCode == "" || params.PurchasePriceAmount == "" {
		return CreateActivationResult{}, ErrInvalidInput
	}
	if params.NextRunAt.IsZero() {
		params.NextRunAt = time.Now().UTC()
	}
	fingerprint := domain.PhoneFingerprint(normalized)
	payload := validJSONOrObject(params.ProviderPayload)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CreateActivationResult{}, fmt.Errorf("begin activation insert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Every quota-changing transaction locks the batch before its attempt or
	// activation rows. This matches cancellation and worker finalization.
	var quantity, purchased, reserved int
	var batchStatus string
	err = tx.QueryRow(ctx, `SELECT quantity, purchased_count, purchase_reserved_count, status
		FROM batches WHERE id=$1 FOR UPDATE`, params.BatchID).
		Scan(&quantity, &purchased, &reserved, &batchStatus)
	if err != nil {
		return CreateActivationResult{}, mapError(err)
	}

	var attemptBatchID int64
	var attemptState string
	var committedActivationID sql.NullInt64
	err = tx.QueryRow(ctx, `SELECT batch_id, state, activation_id
		FROM batch_purchase_attempts WHERE token=$1 FOR UPDATE`, params.PurchaseToken).
		Scan(&attemptBatchID, &attemptState, &committedActivationID)
	if err != nil {
		return CreateActivationResult{}, mapError(err)
	}
	if attemptBatchID != params.BatchID {
		return CreateActivationResult{}, ErrConflict
	}
	if attemptState == "committed" {
		if !committedActivationID.Valid {
			return CreateActivationResult{}, ErrConflict
		}
		existing, scanErr := scanActivation(tx.QueryRow(ctx,
			`SELECT `+activationColumns+` FROM activations WHERE id=$1`, committedActivationID.Int64,
		))
		if scanErr != nil {
			return CreateActivationResult{}, mapError(scanErr)
		}
		expectedFingerprint := domain.PhoneFingerprint(normalized)
		if existing.BatchID != params.BatchID || existing.PhoneFingerprint != expectedFingerprint ||
			existing.ServiceCode != params.ServiceCode || existing.CountryCode != params.CountryCode ||
			existing.Provider != params.Provider || existing.ProviderActivationID != params.ProviderActivationID {
			return CreateActivationResult{}, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return CreateActivationResult{}, fmt.Errorf("%w: commit idempotent activation: %v", ErrCommitUnknown, err)
		}
		return CreateActivationResult{Activation: existing, Duplicate: existing.Status == domain.ActivationStatusDuplicate}, nil
	}
	if attemptState != string(PurchaseAttemptSent) {
		return CreateActivationResult{}, ErrConflict
	}
	if purchased >= quantity || reserved <= 0 {
		return CreateActivationResult{}, ErrBatchCapacity
	}
	if err = lockProviderActivation(ctx, tx, params.Provider, params.ProviderActivationID); err != nil {
		return CreateActivationResult{}, err
	}
	// A provider activation can only be consumed by the token that first
	// commits it. A different reserved token is left unresolved for audit.
	if _, scanErr := scanActivation(tx.QueryRow(ctx,
		`SELECT `+activationColumns+` FROM activations WHERE provider=$1 AND provider_activation_id=$2`,
		params.Provider, params.ProviderActivationID,
	)); scanErr == nil {
		return CreateActivationResult{}, ErrConflict
	} else if scanErr != pgx.ErrNoRows {
		return CreateActivationResult{}, mapError(scanErr)
	}
	// Serialize all inserts for a phone fingerprint. PostgreSQL uniqueness alone
	// prevents two history rows, but an advisory lock also makes the winner and
	// duplicate activation deterministic without transient FK/update races.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, fingerprint); err != nil {
		return CreateActivationResult{}, mapError(err)
	}

	controlAction := string(domain.ControlActionNone)
	var hiddenAt *time.Time
	if domain.BatchStatus(batchStatus).Terminal() {
		controlAction = string(domain.ControlActionDelete)
		now := time.Now().UTC()
		hiddenAt = &now
	}
	insertQuery := `INSERT INTO activations(
		batch_id, provider, provider_activation_id, phone_number, phone_fingerprint,
		service_code, country_code, operator, purchase_price_amount, currency,
		status, slot_reserved, provider_payload, provider_expires_at, next_run_at,
		control_action, hidden_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'purchased',true,$11::jsonb,$12,$13,$14,$15)
	ON CONFLICT(provider, provider_activation_id) DO NOTHING
	RETURNING ` + activationColumns
	activation, err := scanActivation(tx.QueryRow(ctx, insertQuery,
		params.BatchID, params.Provider, params.ProviderActivationID, normalized, fingerprint,
		params.ServiceCode, params.CountryCode, params.Operator, params.PurchasePriceAmount,
		params.Currency, payload, params.ProviderExpiresAt, params.NextRunAt, controlAction, hiddenAt,
	))
	if err == pgx.ErrNoRows {
		return CreateActivationResult{}, ErrConflict
	}
	if err != nil {
		return CreateActivationResult{}, mapError(err)
	}

	result, err := tx.Exec(ctx, `INSERT INTO phone_history(
		phone_fingerprint, phone_number, first_activation_id, last_activation_id
	) VALUES($1,$2,$3,$3) ON CONFLICT(phone_fingerprint) DO NOTHING`,
		fingerprint, normalized, activation.ID,
	)
	if err != nil {
		return CreateActivationResult{}, mapError(err)
	}
	duplicate := result.RowsAffected() == 0
	if duplicate {
		if _, err = tx.Exec(ctx, `UPDATE phone_history SET
			phone_number=$2, last_activation_id=$3, times_seen=times_seen+1, last_seen_at=now()
			WHERE phone_fingerprint=$1`, fingerprint, normalized, activation.ID); err != nil {
			return CreateActivationResult{}, mapError(err)
		}
		activation, err = scanActivation(tx.QueryRow(ctx, `UPDATE activations SET
			status='duplicate', failure_reason='historical phone number', next_run_at=now(), updated_at=now()
			WHERE id=$1 RETURNING `+activationColumns, activation.ID))
		if err != nil {
			return CreateActivationResult{}, mapError(err)
		}
	}
	attemptResult, err := tx.Exec(ctx, `UPDATE batch_purchase_attempts SET
		state='committed', provider=$2, provider_activation_id=$3,
		activation_id=$4, failure_reason='', decided_at=now()
		WHERE token=$1 AND batch_id=$5 AND state='sent'`,
		params.PurchaseToken, params.Provider, params.ProviderActivationID, activation.ID, params.BatchID)
	if err != nil {
		return CreateActivationResult{}, mapError(err)
	}
	if attemptResult.RowsAffected() == 0 {
		return CreateActivationResult{}, ErrConflict
	}
	result, err = tx.Exec(ctx, recordBatchPurchaseSQL, params.BatchID)
	if err != nil {
		return CreateActivationResult{}, mapError(err)
	}
	if result.RowsAffected() == 0 {
		return CreateActivationResult{}, ErrBatchCapacity
	}
	if err = tx.Commit(ctx); err != nil {
		return CreateActivationResult{}, fmt.Errorf("%w: %v", ErrCommitUnknown, err)
	}
	return CreateActivationResult{Activation: activation, Duplicate: duplicate}, nil
}

func (s *PostgresStore) GetActivation(ctx context.Context, id int64) (domain.Activation, error) {
	activation, err := scanActivation(s.pool.QueryRow(ctx,
		`SELECT `+activationColumns+` FROM activations WHERE id=$1`, id))
	if err != nil {
		return domain.Activation{}, mapError(err)
	}
	return activation, nil
}

func (s *PostgresStore) GetActivationByProviderID(ctx context.Context, provider, providerID string) (domain.Activation, error) {
	activation, err := scanActivation(s.pool.QueryRow(ctx,
		`SELECT `+activationColumns+` FROM activations WHERE provider=$1 AND provider_activation_id=$2`,
		provider, providerID,
	))
	if err != nil {
		return domain.Activation{}, mapError(err)
	}
	return activation, nil
}

func (s *PostgresStore) ListActivations(ctx context.Context, filter ActivationFilter) ([]domain.Activation, error) {
	query, args := buildActivationListQuery(filter)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list activations: %w", err)
	}
	defer rows.Close()
	activations := make([]domain.Activation, 0)
	for rows.Next() {
		activation, scanErr := scanActivation(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan activation: %w", scanErr)
		}
		activations = append(activations, activation)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activations: %w", err)
	}
	return activations, nil
}

func buildActivationListQuery(filter ActivationFilter) (string, []any) {
	page := normalizePage(filter.Page)
	query := `SELECT ` + activationColumns + ` FROM activations`
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 6)
	addArg := func(value any) string {
		args = append(args, value)
		return `$` + strconv.Itoa(len(args))
	}
	if filter.BatchID != nil {
		conditions = append(conditions, `batch_id=`+addArg(*filter.BatchID))
	}
	if len(filter.Statuses) > 0 {
		statuses := make([]string, 0, len(filter.Statuses))
		for _, status := range filter.Statuses {
			statuses = append(statuses, string(status))
		}
		conditions = append(conditions, `status=ANY(`+addArg(statuses)+`::text[])`)
	}
	if !filter.IncludeHidden {
		conditions = append(conditions, `hidden_at IS NULL`)
	}
	if phone := strings.TrimSpace(filter.PhoneContains); phone != "" {
		conditions = append(conditions, `phone_number ILIKE `+addArg(`%`+phone+`%`))
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	limitPlaceholder := addArg(page.Limit)
	offsetPlaceholder := addArg(page.Offset)
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + limitPlaceholder + ` OFFSET ` + offsetPlaceholder
	return query, args
}

func (s *PostgresStore) ListRecoverableActivations(ctx context.Context, limit int) ([]domain.Activation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+activationColumns+` FROM activations
		WHERE finished_at IS NULL ORDER BY next_run_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recoverable activations: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Activation, 0)
	for rows.Next() {
		activation, scanErr := scanActivation(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan recoverable activation: %w", scanErr)
		}
		result = append(result, activation)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable activations: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) ClaimRunnableActivations(
	ctx context.Context,
	owner string,
	now time.Time,
	leaseDuration time.Duration,
	limit int,
) ([]domain.Activation, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || leaseDuration <= 0 || limit <= 0 {
		return nil, ErrInvalidInput
	}
	if limit > 500 {
		limit = 500
	}
	leaseUntil := now.Add(leaseDuration)
	query := `WITH candidates AS (
		SELECT id FROM activations
		WHERE finished_at IS NULL
			AND next_run_at <= $2
			AND (lease_until IS NULL OR lease_until <= $2)
		ORDER BY CASE WHEN control_action <> '' THEN 0 ELSE 1 END, next_run_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT $4
	)
	UPDATE activations a SET lease_owner=$1 || ':' || a.id || ':' || (a.lease_version+1), lease_until=$3,
		lease_version=lease_version+1, updated_at=now()
	FROM candidates c WHERE a.id=c.id
	RETURNING ` + prefixedActivationColumns("a")
	rows, err := s.pool.Query(ctx, query, owner, now, leaseUntil, limit)
	if err != nil {
		return nil, fmt.Errorf("claim runnable activations: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Activation, 0)
	for rows.Next() {
		activation, scanErr := scanActivation(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan claimed activation: %w", scanErr)
		}
		result = append(result, activation)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed activations: %w", err)
	}
	return result, nil
}

func prefixedActivationColumns(prefix string) string {
	parts := strings.Split(activationColumns, ",")
	for i, part := range parts {
		parts[i] = prefix + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

func (s *PostgresStore) ReleaseActivationLease(ctx context.Context, id int64, owner string, nextRunAt time.Time) error {
	result, err := s.pool.Exec(ctx, `UPDATE activations SET
		lease_owner='', lease_until=NULL, next_run_at=$3, updated_at=now()
		WHERE id=$1 AND lease_owner=$2`, id, owner, nextRunAt)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return s.notFoundOrConflict(ctx, "activations", id)
	}
	return nil
}

func (s *PostgresStore) TransitionActivation(
	ctx context.Context,
	id int64,
	expected []domain.ActivationStatus,
	next domain.ActivationStatus,
	failureReason string,
) (domain.Activation, error) {
	return s.transitionActivation(ctx, id, expected, next, failureReason, "", 0)
}

func (s *PostgresStore) TransitionActivationOwned(
	ctx context.Context,
	id int64,
	expected []domain.ActivationStatus,
	next domain.ActivationStatus,
	failureReason, owner string,
	leaseVersion int64,
) (domain.Activation, error) {
	if strings.TrimSpace(owner) == "" || leaseVersion <= 0 {
		return domain.Activation{}, ErrInvalidInput
	}
	return s.transitionActivation(ctx, id, expected, next, failureReason, owner, leaseVersion)
}

// lockActivationBatch acquires the parent task row before any activation row
// lock. All transactions that may change task counters follow this order;
// otherwise stopping a task (task then activation) could deadlock with a
// worker finalizing a number (activation then task).
func lockActivationBatch(ctx context.Context, tx pgx.Tx, id int64, owner string, leaseVersion int64, predicate string) (int64, error) {
	query := `SELECT batch_id FROM activations WHERE id=$1`
	args := []any{id}
	if predicate != "" {
		query += ` ` + predicate
	}
	if owner != "" {
		query += ` AND lease_owner=$2 AND lease_version=$3`
		args = append(args, owner, leaseVersion)
	}
	var batchID int64
	if err := tx.QueryRow(ctx, query, args...).Scan(&batchID); err != nil {
		return 0, mapError(err)
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM batches WHERE id=$1 FOR UPDATE`, batchID).Scan(&batchID); err != nil {
		return 0, mapError(err)
	}
	return batchID, nil
}

func (s *PostgresStore) transitionActivation(
	ctx context.Context,
	id int64,
	expected []domain.ActivationStatus,
	next domain.ActivationStatus,
	failureReason, owner string,
	leaseVersion int64,
) (domain.Activation, error) {
	if !next.Valid() {
		return domain.Activation{}, ErrInvalidInput
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Activation{}, fmt.Errorf("begin activation transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = lockActivationBatch(ctx, tx, id, owner, leaseVersion, ""); err != nil {
		return domain.Activation{}, err
	}
	selectQuery := `SELECT ` + activationColumns + ` FROM activations WHERE id=$1`
	selectArgs := []any{id}
	if owner != "" {
		selectQuery += ` AND lease_owner=$2 AND lease_version=$3`
		selectArgs = append(selectArgs, owner, leaseVersion)
	}
	selectQuery += ` FOR UPDATE`
	current, err := scanActivation(tx.QueryRow(ctx, selectQuery, selectArgs...))
	if err != nil {
		return domain.Activation{}, mapError(err)
	}
	if current.FinishedAt != nil {
		return domain.Activation{}, ErrConflict
	}
	if len(expected) > 0 {
		matched := false
		for _, status := range expected {
			if !status.Valid() {
				return domain.Activation{}, ErrInvalidInput
			}
			if current.Status == status {
				matched = true
			}
		}
		if !matched {
			return domain.Activation{}, ErrConflict
		}
	}
	terminal := next.Terminal()
	query := `UPDATE activations SET status=$2, failure_reason=$3,
		finished_at=CASE WHEN $4 THEN COALESCE(finished_at, now()) ELSE NULL END,
		slot_reserved=CASE WHEN $4 THEN false ELSE slot_reserved END,
		lease_owner=CASE WHEN $4 THEN '' ELSE lease_owner END,
		lease_until=CASE WHEN $4 THEN NULL ELSE lease_until END,
		control_action=CASE WHEN $4 THEN '' ELSE control_action END,
		updated_at=now() WHERE id=$1`
	updateArgs := []any{id, string(next), failureReason, terminal}
	if owner != "" {
		query += ` AND lease_owner=$5 AND lease_version=$6`
		updateArgs = append(updateArgs, owner, leaseVersion)
	}
	query += ` RETURNING ` + activationColumns
	updated, err := scanActivation(tx.QueryRow(ctx, query, updateArgs...))
	if err != nil {
		return domain.Activation{}, mapError(err)
	}
	if terminal && current.SlotReserved {
		if err = releaseBatchSlot(ctx, tx, current.BatchID, false); err != nil {
			return domain.Activation{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Activation{}, fmt.Errorf("commit activation transition: %w", err)
	}
	return updated, nil
}

func (s *PostgresStore) FinalizeActivation(
	ctx context.Context,
	id int64,
	expected []domain.ActivationStatus,
) (domain.Activation, error) {
	return s.finalizeActivation(ctx, id, expected, "", 0)
}

func (s *PostgresStore) FinalizeActivationOwned(
	ctx context.Context,
	id int64,
	expected []domain.ActivationStatus,
	owner string,
	leaseVersion int64,
) (domain.Activation, error) {
	if strings.TrimSpace(owner) == "" || leaseVersion <= 0 {
		return domain.Activation{}, ErrInvalidInput
	}
	return s.finalizeActivation(ctx, id, expected, owner, leaseVersion)
}

func (s *PostgresStore) finalizeActivation(
	ctx context.Context,
	id int64,
	expected []domain.ActivationStatus,
	owner string,
	leaseVersion int64,
) (domain.Activation, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Activation{}, fmt.Errorf("begin activation finalization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = lockActivationBatch(ctx, tx, id, owner, leaseVersion, ""); err != nil {
		return domain.Activation{}, err
	}
	selectQuery := `SELECT ` + activationColumns + ` FROM activations WHERE id=$1`
	selectArgs := []any{id}
	if owner != "" {
		selectQuery += ` AND lease_owner=$2 AND lease_version=$3`
		selectArgs = append(selectArgs, owner, leaseVersion)
	}
	selectQuery += ` FOR UPDATE`
	current, err := scanActivation(tx.QueryRow(ctx, selectQuery, selectArgs...))
	if err != nil {
		return domain.Activation{}, mapError(err)
	}
	if current.FinishedAt != nil {
		return current, nil
	}
	if len(expected) > 0 {
		matched := false
		for _, status := range expected {
			if current.Status == status {
				matched = true
				break
			}
		}
		if !matched {
			return domain.Activation{}, ErrConflict
		}
	}
	updateQuery := `UPDATE activations SET
		finished_at=now(), slot_reserved=false, lease_owner='', lease_until=NULL,
		control_action='', updated_at=now() WHERE id=$1`
	updateArgs := []any{id}
	if owner != "" {
		updateQuery += ` AND lease_owner=$2 AND lease_version=$3`
		updateArgs = append(updateArgs, owner, leaseVersion)
	}
	updateQuery += ` RETURNING ` + activationColumns
	updated, err := scanActivation(tx.QueryRow(ctx, updateQuery, updateArgs...))
	if err != nil {
		return domain.Activation{}, mapError(err)
	}
	if current.SlotReserved {
		if err = releaseBatchSlot(ctx, tx, current.BatchID, false); err != nil {
			return domain.Activation{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Activation{}, fmt.Errorf("commit activation finalization: %w", err)
	}
	return updated, nil
}

func (s *PostgresStore) SetActivationBalance(ctx context.Context, id int64, balance *float64, checkedAt *time.Time) error {
	return s.setActivationBalance(ctx, id, balance, checkedAt, "", 0)
}

func (s *PostgresStore) SetActivationBalanceOwned(ctx context.Context, id int64, balance *float64, checkedAt *time.Time, owner string, leaseVersion int64) error {
	if strings.TrimSpace(owner) == "" || leaseVersion <= 0 {
		return ErrInvalidInput
	}
	return s.setActivationBalance(ctx, id, balance, checkedAt, owner, leaseVersion)
}

func (s *PostgresStore) setActivationBalance(ctx context.Context, id int64, balance *float64, checkedAt *time.Time, owner string, leaseVersion int64) error {
	if balance != nil && checkedAt == nil {
		now := time.Now().UTC()
		checkedAt = &now
	}
	query := `UPDATE activations SET balance_rp=$2, balance_checked_at=$3, updated_at=now() WHERE id=$1`
	args := []any{id, balance, checkedAt}
	if owner != "" {
		query += ` AND lease_owner=$4 AND lease_version=$5`
		args = append(args, owner, leaseVersion)
	}
	result, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) AttachActivationAccount(ctx context.Context, activationID, accountID int64) error {
	return s.attachActivationAccount(ctx, activationID, accountID, "", 0)
}

func (s *PostgresStore) AttachActivationAccountOwned(ctx context.Context, activationID, accountID int64, owner string, leaseVersion int64) error {
	if strings.TrimSpace(owner) == "" || leaseVersion <= 0 {
		return ErrInvalidInput
	}
	return s.attachActivationAccount(ctx, activationID, accountID, owner, leaseVersion)
}

func (s *PostgresStore) attachActivationAccount(ctx context.Context, activationID, accountID int64, owner string, leaseVersion int64) error {
	query := `UPDATE activations SET account_id=$2, updated_at=now() WHERE id=$1`
	args := []any{activationID, accountID}
	if owner != "" {
		query += ` AND lease_owner=$3 AND lease_version=$4`
		args = append(args, owner, leaseVersion)
	}
	result, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) MarkActivationFulfilled(ctx context.Context, id int64) (bool, error) {
	return s.markActivationFulfilled(ctx, id, "", 0)
}

func (s *PostgresStore) MarkActivationFulfilledOwned(ctx context.Context, id int64, owner string, leaseVersion int64) (bool, error) {
	if strings.TrimSpace(owner) == "" || leaseVersion <= 0 {
		return false, ErrInvalidInput
	}
	return s.markActivationFulfilled(ctx, id, owner, leaseVersion)
}

func (s *PostgresStore) markActivationFulfilled(ctx context.Context, id int64, owner string, leaseVersion int64) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin mark fulfilled: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = lockActivationBatch(ctx, tx, id, owner, leaseVersion, "AND finished_at IS NULL"); err != nil {
		return false, err
	}
	var batchID int64
	var already, slotReserved bool
	var status string
	selectQuery := `SELECT batch_id, ever_fulfilled, slot_reserved, status
		FROM activations WHERE id=$1 AND finished_at IS NULL`
	selectArgs := []any{id}
	if owner != "" {
		selectQuery += ` AND lease_owner=$2 AND lease_version=$3`
		selectArgs = append(selectArgs, owner, leaseVersion)
	}
	selectQuery += ` FOR UPDATE`
	err = tx.QueryRow(ctx, selectQuery, selectArgs...).
		Scan(&batchID, &already, &slotReserved, &status)
	if err != nil {
		return false, mapError(err)
	}
	if already {
		if err = tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit fulfilled no-op: %w", err)
		}
		return false, nil
	}
	if domain.ActivationStatus(status) != domain.ActivationStatusSettingPIN || !slotReserved {
		return false, ErrConflict
	}
	updateQuery := `UPDATE activations SET ever_fulfilled=true, slot_reserved=false, updated_at=now() WHERE id=$1`
	updateArgs := []any{id}
	if owner != "" {
		updateQuery += ` AND lease_owner=$2 AND lease_version=$3`
		updateArgs = append(updateArgs, owner, leaseVersion)
	}
	if _, err = tx.Exec(ctx, updateQuery, updateArgs...); err != nil {
		return false, mapError(err)
	}
	if err = releaseBatchSlot(ctx, tx, batchID, true); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit mark fulfilled: %w", err)
	}
	return true, nil
}

func releaseBatchSlot(ctx context.Context, tx pgx.Tx, batchID int64, fulfilled bool) error {
	result, err := tx.Exec(ctx, releaseBatchSlotSQL, batchID, fulfilled)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		if fulfilled {
			return ErrBatchCapacity
		}
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) AdvanceSMSCycle(ctx context.Context, id int64, owner string, nextRunAt time.Time) (int, error) {
	var cycle int
	err := s.pool.QueryRow(ctx, `UPDATE activations SET
		sms_cycle=sms_cycle+1, next_run_at=$3, updated_at=now()
		WHERE id=$1 AND lease_owner=$2 AND finished_at IS NULL
		RETURNING sms_cycle`, id, owner, nextRunAt).Scan(&cycle)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, s.notFoundOrConflict(ctx, "activations", id)
		}
		return 0, mapError(err)
	}
	return cycle, nil
}

func (s *PostgresStore) TouchActivationPoll(ctx context.Context, id int64, owner string, polledAt, nextRunAt time.Time) error {
	result, err := s.pool.Exec(ctx, `UPDATE activations SET
		last_polled_at=$3, next_run_at=$4, updated_at=now()
		WHERE id=$1 AND lease_owner=$2 AND finished_at IS NULL`, id, owner, polledAt, nextRunAt)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return s.notFoundOrConflict(ctx, "activations", id)
	}
	return nil
}

func (s *PostgresStore) SetControlAction(ctx context.Context, id int64, action domain.ControlAction) error {
	if !action.Valid() || action == domain.ControlActionNone {
		return ErrInvalidInput
	}
	result, err := s.pool.Exec(ctx, `UPDATE activations SET
		control_action=$2,
		next_run_at=now(), updated_at=now()
		WHERE id=$1 AND finished_at IS NULL AND (control_action='' OR control_action=$2)`, id, string(action))
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ClearControlAction(ctx context.Context, id int64, expected domain.ControlAction) error {
	if !expected.Valid() || expected == domain.ControlActionNone {
		return ErrInvalidInput
	}
	result, err := s.pool.Exec(ctx, `UPDATE activations SET control_action='', updated_at=now()
		WHERE id=$1 AND control_action=$2`, id, string(expected))
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return s.notFoundOrConflict(ctx, "activations", id)
	}
	return nil
}

func (s *PostgresStore) SoftDeleteActivation(ctx context.Context, id int64) error {
	return s.SetControlAction(ctx, id, domain.ControlActionDelete)
}

func (s *PostgresStore) HideActivation(ctx context.Context, id int64) error {
	result, err := s.pool.Exec(ctx, `UPDATE activations SET hidden_at=COALESCE(hidden_at, now()), updated_at=now() WHERE id=$1`, id)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) GetPhoneHistory(ctx context.Context, phone string) (domain.PhoneHistory, error) {
	normalized, err := domain.NormalizePhone(phone)
	if err != nil {
		return domain.PhoneHistory{}, ErrInvalidInput
	}
	fingerprint := domain.PhoneFingerprint(normalized)
	var history domain.PhoneHistory
	err = s.pool.QueryRow(ctx, `SELECT phone_fingerprint, phone_number,
		first_activation_id, last_activation_id, times_seen, first_seen_at, last_seen_at
		FROM phone_history WHERE phone_fingerprint=$1`, fingerprint).Scan(
		&history.PhoneFingerprint, &history.PhoneNumber, &history.FirstActivationID,
		&history.LastActivationID, &history.TimesSeen, &history.FirstSeenAt, &history.LastSeenAt,
	)
	if err != nil {
		return domain.PhoneHistory{}, mapError(err)
	}
	return history, nil
}
