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
	max_price_amount, currency, target_pin_enc, config, next_purchase_at, quantity, fulfilled_count, inflight_count,
	failure_reason, created_at, updated_at, started_at, finished_at`

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
		&batch.FulfilledCount, &batch.InflightCount, &batch.FailureReason,
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
		max_price_amount, currency, target_pin_enc, config, quantity
	) VALUES('running', $1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)
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
	batch, err := scanBatch(tx.QueryRow(ctx, `UPDATE batches SET
		status=CASE WHEN status='failed' THEN status ELSE 'cancelled' END,
		finished_at=COALESCE(finished_at, now()), updated_at=now()
		WHERE id=$1 RETURNING `+batchColumns, id))
	if err != nil {
		return domain.Batch{}, mapError(err)
	}
	// Provider work is intentionally not finalized here. Workers must execute
	// the durable delete action first and then finalize each activation.
	if _, err = tx.Exec(ctx, `UPDATE activations SET
		control_action='delete', hidden_at=COALESCE(hidden_at, now()),
		next_run_at=now(), updated_at=now()
		WHERE batch_id=$1 AND finished_at IS NULL`, id); err != nil {
		return domain.Batch{}, mapError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Batch{}, fmt.Errorf("commit batch cancellation: %w", err)
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
