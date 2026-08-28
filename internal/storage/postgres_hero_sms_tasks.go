package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mangobubu/gopay-autosms/internal/domain"
)

const heroSMSTaskColumns = `id, submission_id, status, product_kind,
	service_code, service_name, country_code, country_name, verification_type,
	duration_hours, max_price_amount, provider, purchase_token,
	provider_activation_id, phone_number, operator, activation_cost, currency,
	purchased_at, expires_at, refundable_until, refund_status,
	message_count, continuation_count, continuation_pending_count, supports_continuation,
	first_message_at, next_run_at, lease_owner, lease_until,
	lease_version, stop_requested, retry_count, last_error, last_polled_at,
	webhook_wakeup_at, provider_payload, created_at, updated_at, finished_at`

const heroSMSTaskMessageColumns = `id, task_id, provider_activation_id,
	provider_message_id, source, code, message_text, provider_received_at,
	raw_payload, payload_fingerprint, created_at`

var claimDueHeroSMSTasksSQL = `WITH candidates AS (
	SELECT id FROM hero_sms_number_tasks
	WHERE finished_at IS NULL
		AND (lease_until IS NULL OR lease_until <= $2)
		AND (
			(status='purchasing' AND lease_until <= $2)
			OR (status IN ('waiting_number','active')
				AND (stop_requested OR next_run_at <= $2 OR webhook_wakeup_at <= $2))
			OR (status='settling'
				AND (next_run_at <= $2 OR webhook_wakeup_at <= $2))
			OR (status='purchase_unknown' AND stop_requested)
		)
	ORDER BY CASE WHEN stop_requested AND status<>'settling' THEN 0
		WHEN status='purchasing' THEN 1 ELSE 2 END,
		LEAST(next_run_at,COALESCE(webhook_wakeup_at,next_run_at)), id
	FOR UPDATE SKIP LOCKED
	LIMIT $4
)
UPDATE hero_sms_number_tasks task SET
	status=CASE WHEN task.status='purchasing' THEN 'purchase_unknown' ELSE task.status END,
	last_error=CASE WHEN task.status='purchasing'
		THEN 'purchase lease expired before outcome was persisted' ELSE task.last_error END,
	retry_count=task.retry_count+CASE WHEN task.status='purchasing' THEN 1 ELSE 0 END,
	lease_owner=$1 || ':' || task.id || ':' || (task.lease_version+1),
	lease_until=$3, lease_version=task.lease_version+1, updated_at=now()
FROM candidates WHERE task.id=candidates.id
RETURNING ` + prefixedHeroSMSTaskColumns("task")

const beginHeroSMSPurchaseOwnedSQL = `UPDATE hero_sms_number_tasks SET
	status='purchasing', purchase_token=$5, last_error='', updated_at=now()
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3
		AND lease_until > $4 AND status='waiting_number'
		AND finished_at IS NULL AND NOT stop_requested AND purchase_token=''
	RETURNING ` + heroSMSTaskColumns

const releaseHeroSMSPurchaseOwnedSQL = `UPDATE hero_sms_number_tasks SET
	status='waiting_number', purchase_token='',
	next_run_at=CASE WHEN stop_requested THEN now()
		ELSE LEAST($6,COALESCE(webhook_wakeup_at,$6)) END,
	webhook_wakeup_at=NULL, lease_owner='', lease_until=NULL,
	retry_count=retry_count+1, last_error=$7, updated_at=now()
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3
		AND lease_until > $4 AND status='purchasing' AND purchase_token=$5
		AND finished_at IS NULL
	RETURNING ` + heroSMSTaskColumns

const scheduleHeroSMSTaskOwnedSQL = `UPDATE hero_sms_number_tasks SET
	status=$5,
	refund_status=CASE
		WHEN $5='active' AND refund_status='refundable'
			AND (message_count>0 OR refundable_until IS NULL OR refundable_until <= now())
			THEN 'unavailable'
		ELSE refund_status
	END,
	next_run_at=CASE WHEN stop_requested AND $5<>'settling' THEN now()
		WHEN $5='active' AND btrim($7)<>'' AND continuation_pending_count>0 THEN $6
		ELSE LEAST($6,COALESCE(webhook_wakeup_at,$6)) END,
	webhook_wakeup_at=NULL,
	lease_owner='', lease_until=NULL,
	retry_count=CASE WHEN btrim($7)<>'' THEN retry_count+1 ELSE 0 END,
	last_error=$7,
	last_polled_at=CASE WHEN status='active' THEN now() ELSE last_polled_at END,
	updated_at=now()
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3
		AND lease_until > $4 AND finished_at IS NULL
		AND status<>'purchasing'
		AND (continuation_pending_count=0 OR $5='active')
	RETURNING ` + heroSMSTaskColumns

const wakeHeroSMSTaskSQL = `UPDATE hero_sms_number_tasks SET
	message_count=(SELECT count(*)::integer FROM hero_sms_number_messages WHERE task_id=$1),
	first_message_at=(SELECT min(COALESCE(provider_received_at,created_at))
		FROM hero_sms_number_messages WHERE task_id=$1),
	refund_status=CASE WHEN status='active' THEN 'unavailable' ELSE refund_status END,
	next_run_at=CASE WHEN finished_at IS NOT NULL THEN next_run_at
		WHEN lease_owner='' OR lease_until IS NULL OR lease_until <= now()
		THEN LEAST(next_run_at,$2) ELSE next_run_at END,
	webhook_wakeup_at=CASE WHEN finished_at IS NOT NULL THEN webhook_wakeup_at
		ELSE LEAST(COALESCE(webhook_wakeup_at,$2),$2) END,
	updated_at=now()
	WHERE id=$1`

const prepareHeroSMSTaskSettlementOwnedSQL = `UPDATE hero_sms_number_tasks SET
	status='settling', continuation_pending_count=0,
	refund_status=CASE
		WHEN status='active' AND message_count=0 AND refundable_until IS NOT NULL
			AND refundable_until > $4 AND (expires_at IS NULL OR expires_at > $4)
			AND refund_status='refundable'
			THEN 'requested'
		ELSE 'unavailable'
	END,
	next_run_at=$4, updated_at=now()
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3
		AND lease_until > $4
		AND (status='active' OR (status='purchase_unknown' AND provider_activation_id IS NOT NULL))
		AND finished_at IS NULL
	RETURNING ` + heroSMSTaskColumns

const finishHeroSMSTaskOwnedSQL = `UPDATE hero_sms_number_tasks SET
	status=$5, refund_status=$6, stop_requested=false,
	continuation_pending_count=0,
	lease_owner='', lease_until=NULL, webhook_wakeup_at=NULL,
	last_error=$7, finished_at=COALESCE(finished_at,now()), updated_at=now()
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3
		AND lease_until > $4 AND finished_at IS NULL
		AND (
			($5='refunded' AND status='settling'
				AND message_count=0 AND refund_status='requested')
			OR ($5='settled' AND status='settling')
			OR ($5='stopped' AND status IN ('waiting_number','purchase_unknown') AND provider_activation_id IS NULL)
			OR ($5='expired' AND status IN ('active','settling'))
		)
	RETURNING ` + heroSMSTaskColumns

const beginHeroSMSContinuationOwnedSQL = `UPDATE hero_sms_number_tasks SET
	continuation_pending_count=message_count, updated_at=now()
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3 AND lease_until > $4
		AND status='active' AND product_kind='activation'
		AND supports_continuation=true AND continuation_pending_count=0
		AND continuation_count < message_count
		AND NOT stop_requested AND finished_at IS NULL
		AND (expires_at IS NULL OR expires_at > $4)
	RETURNING ` + heroSMSTaskColumns

const completeHeroSMSContinuationOwnedSQL = `UPDATE hero_sms_number_tasks SET
	continuation_count=$5, continuation_pending_count=0,
	next_run_at=CASE WHEN stop_requested THEN now()
		WHEN message_count>$5
		THEN LEAST(next_run_at,COALESCE(webhook_wakeup_at,now())) ELSE $6 END,
	webhook_wakeup_at=CASE WHEN stop_requested THEN webhook_wakeup_at
		WHEN message_count>$5
		THEN COALESCE(webhook_wakeup_at,now()) ELSE NULL END,
	lease_owner='', lease_until=NULL, retry_count=0, last_error='', updated_at=now()
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3 AND lease_until > $4
		AND status='active' AND product_kind='activation' AND supports_continuation=true
		AND continuation_pending_count=$5 AND finished_at IS NULL
		AND continuation_count < $5 AND $5 <= message_count
	RETURNING ` + heroSMSTaskColumns

const abortHeroSMSContinuationOwnedSQL = `UPDATE hero_sms_number_tasks SET
	continuation_pending_count=0,
	next_run_at=CASE WHEN stop_requested THEN now()
		WHEN message_count>$5
		THEN LEAST(next_run_at,COALESCE(webhook_wakeup_at,now())) ELSE $6 END,
	webhook_wakeup_at=CASE WHEN stop_requested THEN webhook_wakeup_at
		WHEN message_count>$5
		THEN COALESCE(webhook_wakeup_at,now()) ELSE NULL END,
	lease_owner='', lease_until=NULL, retry_count=retry_count+1,
	last_error=$7, updated_at=now()
	WHERE id=$1 AND lease_owner=$2 AND lease_version=$3 AND lease_until > $4
		AND status='active' AND product_kind='activation' AND supports_continuation=true
		AND continuation_pending_count=$5 AND finished_at IS NULL
	RETURNING ` + heroSMSTaskColumns

const restartHeroSMSTaskSQL = `UPDATE hero_sms_number_tasks SET
	status='waiting_number', purchase_token='', stop_requested=false,
	continuation_pending_count=0,
	next_run_at=$2, lease_owner='', lease_until=NULL,
	webhook_wakeup_at=NULL, last_error='', finished_at=NULL, updated_at=now()
	WHERE id=$1 AND status='stopped' AND provider_activation_id IS NULL
		AND purchase_token=''
	RETURNING ` + heroSMSTaskColumns

const requestHeroSMSTaskStopSQL = `UPDATE hero_sms_number_tasks SET
	stop_requested=true, next_run_at=now(),
	lease_owner=CASE WHEN status='waiting_number' THEN '' ELSE lease_owner END,
	lease_until=CASE WHEN status='waiting_number' THEN NULL ELSE lease_until END,
	lease_version=lease_version+CASE WHEN status='waiting_number' THEN 1 ELSE 0 END,
	updated_at=now()
	WHERE id=$1 AND finished_at IS NULL
	RETURNING ` + heroSMSTaskColumns

const findHeroSMSMessageByProviderIDSQL = `SELECT ` + heroSMSTaskMessageColumns + `
	FROM hero_sms_number_messages
	WHERE provider_activation_id=$1 AND provider_message_id=$2
	LIMIT 1`

// continuation_count acknowledges append order, not wall-clock order. A
// callback and getStatus response for the current activation cycle can expose
// different metadata (timestamp/text), but they share the code. Only an
// unacknowledged row may be folded this way, so a later cycle which legitimately
// repeats the same code remains a new message.
const findPendingHeroSMSActivationMessageSQL = `SELECT ` + heroSMSTaskMessageColumns + ` FROM (
	SELECT message.*, row_number() OVER (ORDER BY message.id) AS message_ordinal
	FROM hero_sms_number_messages message WHERE message.task_id=$1
) pending
	WHERE pending.message_ordinal > $2
		AND (($3<>'' AND pending.code=$3)
			OR ($3='' AND pending.code='' AND pending.message_text=$4))
	ORDER BY pending.message_ordinal
	LIMIT 1`

// A rich callback can arrive just after a metadata-free poll row was confirmed
// and acknowledged by RequestAnother. Fold it only when the provider says the
// message existed no later than the poll row was stored. A positive time
// tolerance could consume a legitimate same-code message from the next cycle.
// The second metadata branch lets the same callback find the row again after
// its ID/timestamp enrichment.
const findDelayedConfirmedHeroSMSActivationMessageSQL = `SELECT ` + heroSMSTaskMessageColumns + ` FROM (
	SELECT message.*, row_number() OVER (ORDER BY message.id) AS message_ordinal
	FROM hero_sms_number_messages message WHERE message.task_id=$1
) confirmed
	WHERE confirmed.message_ordinal <= $2
		AND confirmed.source='poll'
		AND (($3<>'' AND confirmed.code=$3)
			OR ($3='' AND confirmed.code='' AND confirmed.message_text=$4))
		AND $5::timestamptz <= confirmed.created_at
		AND (
			(confirmed.provider_message_id='' AND confirmed.provider_received_at IS NULL)
			OR (confirmed.provider_received_at=$5 AND (
				confirmed.provider_message_id='' OR btrim($6)=''
				OR confirmed.provider_message_id=$6
			))
		)
	ORDER BY confirmed.created_at, confirmed.id
	LIMIT 1`

const enrichExistingHeroSMSTaskMessageSQL = `UPDATE hero_sms_number_messages SET
	task_id=CASE WHEN task_id IS NULL AND $2::bigint IS NOT NULL THEN $2 ELSE task_id END,
	provider_message_id=CASE WHEN provider_message_id='' AND btrim($4)<>''
		THEN $4 ELSE provider_message_id END,
	provider_received_at=COALESCE(provider_received_at,$5)
	WHERE id=$1 AND provider_activation_id=$3
		AND (task_id IS NULL OR $2::bigint IS NULL OR task_id=$2)
		AND (provider_message_id='' OR btrim($4)='' OR provider_message_id=$4)
	RETURNING ` + heroSMSTaskMessageColumns

const recoverHeroSMSPurchaseOutcomeSQL = `SELECT status, provider_activation_id
	FROM hero_sms_number_tasks
	WHERE id=$1 AND purchase_token=$2
		AND status IN ('purchasing','purchase_unknown','active','stopped')
		AND (finished_at IS NULL OR status='stopped')
	FOR UPDATE`

const persistHeroSMSPurchaseOutcomeSQL = `UPDATE hero_sms_number_tasks SET
	status='active', provider_activation_id=$2, phone_number=$3, operator=$4,
	activation_cost=$5, currency=CASE WHEN btrim($6)<>'' THEN $6 ELSE currency END,
	purchased_at=$7, expires_at=$8, refundable_until=$9, refund_status=$10,
	supports_continuation=$11, continuation_pending_count=0,
	provider_payload=$12::json, next_run_at=$13,
	lease_owner='', lease_until=NULL, retry_count=0,
	last_error='', finished_at=NULL, updated_at=now()
	WHERE id=$1`

type heroSMSTaskQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func prefixedHeroSMSTaskColumns(prefix string) string {
	parts := strings.Split(heroSMSTaskColumns, ",")
	for i, part := range parts {
		parts[i] = prefix + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

func scanHeroSMSTask(row rowScanner) (domain.HeroSMSNumberTask, error) {
	var task domain.HeroSMSNumberTask
	var status, productKind, refundStatus string
	var duration sql.NullInt32
	var maxPrice, providerActivationID, activationCost sql.NullString
	var purchasedAt, expiresAt, refundableUntil, firstMessageAt sql.NullTime
	var leaseUntil, lastPolledAt, webhookWakeupAt, finishedAt sql.NullTime
	var payload []byte
	err := row.Scan(
		&task.ID, &task.SubmissionID, &status, &productKind,
		&task.ServiceCode, &task.ServiceName, &task.CountryCode, &task.CountryName,
		&task.VerificationType, &duration, &maxPrice, &task.Provider,
		&task.PurchaseToken, &providerActivationID, &task.PhoneNumber,
		&task.Operator, &activationCost, &task.Currency, &purchasedAt, &expiresAt,
		&refundableUntil, &refundStatus, &task.MessageCount, &task.ContinuationCount,
		&task.ContinuationPendingCount, &task.SupportsContinuation, &firstMessageAt,
		&task.NextRunAt, &task.LeaseOwner, &leaseUntil, &task.LeaseVersion,
		&task.StopRequested, &task.RetryCount, &task.LastError, &lastPolledAt,
		&webhookWakeupAt, &payload, &task.CreatedAt, &task.UpdatedAt, &finishedAt,
	)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	task.Status = domain.HeroSMSNumberTaskStatus(status)
	task.ProductKind = domain.HeroSMSProductKind(productKind)
	task.RefundStatus = domain.HeroSMSRefundStatus(refundStatus)
	if duration.Valid {
		value := int(duration.Int32)
		task.DurationHours = &value
	}
	if maxPrice.Valid {
		task.MaxPriceAmount = maxPrice.String
	}
	if providerActivationID.Valid {
		task.ProviderActivationID = providerActivationID.String
	}
	if activationCost.Valid {
		task.ActivationCost = activationCost.String
	}
	assignOptionalTime(&task.PurchasedAt, purchasedAt)
	assignOptionalTime(&task.ExpiresAt, expiresAt)
	assignOptionalTime(&task.RefundableUntil, refundableUntil)
	assignOptionalTime(&task.FirstMessageAt, firstMessageAt)
	assignOptionalTime(&task.LeaseUntil, leaseUntil)
	assignOptionalTime(&task.LastPolledAt, lastPolledAt)
	assignOptionalTime(&task.WebhookWakeupAt, webhookWakeupAt)
	assignOptionalTime(&task.FinishedAt, finishedAt)
	task.ProviderPayload = cloneJSON(payload)
	task.Messages = make([]domain.HeroSMSNumberMessage, 0)
	return task, nil
}

func assignOptionalTime(target **time.Time, value sql.NullTime) {
	if value.Valid {
		at := value.Time
		*target = &at
	}
}

func scanHeroSMSTaskMessage(row rowScanner) (domain.HeroSMSNumberMessage, error) {
	var message domain.HeroSMSNumberMessage
	var taskID sql.NullInt64
	var source string
	var receivedAt sql.NullTime
	var payload []byte
	err := row.Scan(
		&message.ID, &taskID, &message.ProviderActivationID,
		&message.ProviderMessageID, &source, &message.Code, &message.Text,
		&receivedAt, &payload, &message.PayloadFingerprint, &message.CreatedAt,
	)
	if err != nil {
		return domain.HeroSMSNumberMessage{}, err
	}
	if taskID.Valid {
		value := taskID.Int64
		message.TaskID = &value
	}
	if receivedAt.Valid {
		value := receivedAt.Time
		message.ProviderReceivedAt = &value
	}
	message.Source = domain.HeroSMSMessageSource(source)
	message.RawPayload = cloneJSON(payload)
	return message, nil
}

func normalizeCreateHeroSMSTasksParams(params CreateHeroSMSTasksParams) (CreateHeroSMSTasksParams, error) {
	params.SubmissionID = strings.TrimSpace(params.SubmissionID)
	params.ServiceCode = strings.TrimSpace(params.ServiceCode)
	params.ServiceName = strings.TrimSpace(params.ServiceName)
	params.CountryCode = strings.TrimSpace(params.CountryCode)
	params.CountryName = strings.TrimSpace(params.CountryName)
	params.VerificationType = strings.TrimSpace(params.VerificationType)
	params.MaxPriceAmount = strings.TrimSpace(params.MaxPriceAmount)
	params.Currency = strings.TrimSpace(params.Currency)
	if params.ProductKind == "" {
		params.ProductKind = domain.HeroSMSProductActivation
	}
	if !params.ProductKind.Valid() || params.ServiceCode == "" || params.CountryCode == "" ||
		params.VerificationType == "" || params.Quantity <= 0 || params.Quantity > MaxHeroSMSTaskQuantity {
		return CreateHeroSMSTasksParams{}, ErrInvalidInput
	}
	if (params.ProductKind == domain.HeroSMSProductActivation && params.DurationHours != nil) ||
		(params.ProductKind == domain.HeroSMSProductRent && (params.DurationHours == nil || *params.DurationHours <= 0)) {
		return CreateHeroSMSTasksParams{}, ErrInvalidInput
	}
	if params.SubmissionID == "" {
		generated, err := newHeroSMSSubmissionID()
		if err != nil {
			return CreateHeroSMSTasksParams{}, fmt.Errorf("generate HeroSMS submission ID: %w", err)
		}
		params.SubmissionID = generated
	}
	params.ProviderPayload = validJSONOrObject(params.ProviderPayload)
	if !json.Valid(params.ProviderPayload) {
		return CreateHeroSMSTasksParams{}, ErrInvalidInput
	}
	if params.NextRunAt.IsZero() {
		params.NextRunAt = time.Now().UTC()
	}
	return params, nil
}

func newHeroSMSSubmissionID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func heroSMSTaskSubmissionFingerprint(params CreateHeroSMSTasksParams) string {
	hash := sha256.New()
	write := func(value string) {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	write(string(params.ProductKind))
	write(params.ServiceCode)
	write(params.CountryCode)
	write(params.VerificationType)
	if params.DurationHours == nil {
		write("")
	} else {
		write(strconv.Itoa(*params.DurationHours))
	}
	write(strconv.Itoa(params.Quantity))
	return hex.EncodeToString(hash.Sum(nil))
}

func nullableNumeric(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableText(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func (s *PostgresStore) CreateHeroSMSTasks(
	ctx context.Context,
	params CreateHeroSMSTasksParams,
) ([]domain.HeroSMSNumberTask, error) {
	params, err := normalizeCreateHeroSMSTasksParams(params)
	if err != nil {
		return nil, err
	}
	fingerprint := heroSMSTaskSubmissionFingerprint(params)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin HeroSMS task creation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := tx.Exec(ctx, `INSERT INTO hero_sms_task_submissions(id, request_fingerprint, quantity)
		VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING`, params.SubmissionID, fingerprint, params.Quantity)
	if err != nil {
		return nil, mapError(err)
	}
	createdSubmission := result.RowsAffected() == 1
	var storedFingerprint string
	var storedQuantity int
	if err = tx.QueryRow(ctx, `SELECT request_fingerprint, quantity
		FROM hero_sms_task_submissions WHERE id=$1 FOR UPDATE`, params.SubmissionID).
		Scan(&storedFingerprint, &storedQuantity); err != nil {
		return nil, mapError(err)
	}
	if storedFingerprint != fingerprint || storedQuantity != params.Quantity {
		return nil, ErrConflict
	}

	if !createdSubmission {
		tasks, listErr := listHeroSMSTasksBySubmission(ctx, tx, params.SubmissionID)
		if listErr != nil {
			return nil, listErr
		}
		if len(tasks) != params.Quantity {
			return nil, fmt.Errorf("%w: incomplete HeroSMS submission", ErrConflict)
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("%w: commit idempotent HeroSMS task creation: %v", ErrCommitUnknown, err)
		}
		if err = s.attachHeroSMSTaskMessages(ctx, tasks); err != nil {
			return nil, err
		}
		return tasks, nil
	}

	insertSQL := `INSERT INTO hero_sms_number_tasks(
		submission_id, status, product_kind, service_code, service_name,
		country_code, country_name, verification_type, duration_hours,
		max_price_amount, currency, provider_payload, next_run_at
	) VALUES($1,'waiting_number',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::json,$12)
	RETURNING ` + heroSMSTaskColumns
	tasks := make([]domain.HeroSMSNumberTask, 0, params.Quantity)
	for index := 0; index < params.Quantity; index++ {
		task, scanErr := scanHeroSMSTask(tx.QueryRow(ctx, insertSQL,
			params.SubmissionID, string(params.ProductKind), params.ServiceCode,
			params.ServiceName, params.CountryCode, params.CountryName,
			params.VerificationType, params.DurationHours, nullableNumeric(params.MaxPriceAmount),
			params.Currency, params.ProviderPayload, params.NextRunAt,
		))
		if scanErr != nil {
			return nil, mapError(scanErr)
		}
		tasks = append(tasks, task)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("%w: commit HeroSMS task creation: %v", ErrCommitUnknown, err)
	}
	return tasks, nil
}

func listHeroSMSTasksBySubmission(
	ctx context.Context,
	queryer heroSMSTaskQueryer,
	submissionID string,
) ([]domain.HeroSMSNumberTask, error) {
	rows, err := queryer.Query(ctx, `SELECT `+heroSMSTaskColumns+`
		FROM hero_sms_number_tasks WHERE submission_id=$1 ORDER BY id`, submissionID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return scanHeroSMSTaskRows(rows)
}

func scanHeroSMSTaskRows(rows pgx.Rows) ([]domain.HeroSMSNumberTask, error) {
	tasks := make([]domain.HeroSMSNumberTask, 0)
	for rows.Next() {
		task, err := scanHeroSMSTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *PostgresStore) GetHeroSMSTask(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	if id <= 0 {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx,
		`SELECT `+heroSMSTaskColumns+` FROM hero_sms_number_tasks WHERE id=$1`, id))
	if err != nil {
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	messages, err := s.ListHeroSMSTaskMessages(ctx, id)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	task.Messages = messages
	return task, nil
}

func (s *PostgresStore) ListHeroSMSTasks(
	ctx context.Context,
	filter HeroSMSTaskFilter,
) ([]domain.HeroSMSNumberTask, error) {
	page := normalizePage(filter.Page)
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		if !status.Valid() {
			return nil, ErrInvalidInput
		}
		statuses = append(statuses, string(status))
	}
	query := `SELECT ` + heroSMSTaskColumns + ` FROM hero_sms_number_tasks`
	args := make([]any, 0, 3)
	if len(statuses) > 0 {
		query += ` WHERE status=ANY($1::text[]) ORDER BY created_at DESC,id DESC LIMIT $2 OFFSET $3`
		args = append(args, statuses, page.Limit, page.Offset)
	} else {
		query += ` ORDER BY created_at DESC,id DESC LIMIT $1 OFFSET $2`
		args = append(args, page.Limit, page.Offset)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	tasks, err := scanHeroSMSTaskRows(rows)
	if err != nil {
		return nil, fmt.Errorf("scan HeroSMS tasks: %w", err)
	}
	if err = s.attachHeroSMSTaskMessages(ctx, tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *PostgresStore) attachHeroSMSTaskMessages(
	ctx context.Context,
	tasks []domain.HeroSMSNumberTask,
) error {
	if len(tasks) == 0 {
		return nil
	}
	ids := make([]int64, len(tasks))
	indexes := make(map[int64]int, len(tasks))
	for index := range tasks {
		ids[index] = tasks[index].ID
		indexes[tasks[index].ID] = index
		tasks[index].Messages = make([]domain.HeroSMSNumberMessage, 0)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+heroSMSTaskMessageColumns+`
		FROM hero_sms_number_messages WHERE task_id=ANY($1::bigint[])
		ORDER BY task_id, provider_received_at NULLS LAST, created_at, id`, ids)
	if err != nil {
		return mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		message, scanErr := scanHeroSMSTaskMessage(rows)
		if scanErr != nil {
			return scanErr
		}
		if message.TaskID == nil {
			continue
		}
		if index, ok := indexes[*message.TaskID]; ok {
			tasks[index].Messages = append(tasks[index].Messages, message)
		}
	}
	return rows.Err()
}

func (s *PostgresStore) ListHeroSMSTaskMessages(
	ctx context.Context,
	taskID int64,
) ([]domain.HeroSMSNumberMessage, error) {
	if taskID <= 0 {
		return nil, ErrInvalidInput
	}
	rows, err := s.pool.Query(ctx, `SELECT `+heroSMSTaskMessageColumns+`
		FROM hero_sms_number_messages WHERE task_id=$1
		ORDER BY provider_received_at NULLS LAST, created_at, id`, taskID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	messages := make([]domain.HeroSMSNumberMessage, 0)
	for rows.Next() {
		message, scanErr := scanHeroSMSTaskMessage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		messages = append(messages, message)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *PostgresStore) ClaimDueHeroSMSTasks(
	ctx context.Context,
	owner string,
	now time.Time,
	leaseDuration time.Duration,
	limit int,
) ([]domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || now.IsZero() || leaseDuration <= 0 || limit <= 0 {
		return nil, ErrInvalidInput
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, claimDueHeroSMSTasksSQL,
		owner, now, now.Add(leaseDuration), limit)
	if err != nil {
		return nil, fmt.Errorf("claim due HeroSMS tasks: %w", mapError(err))
	}
	defer rows.Close()
	tasks, err := scanHeroSMSTaskRows(rows)
	if err != nil {
		return nil, fmt.Errorf("scan claimed HeroSMS tasks: %w", err)
	}
	return tasks, nil
}

func (s *PostgresStore) BeginHeroSMSPurchaseOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	purchaseToken string,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	purchaseToken = strings.TrimSpace(purchaseToken)
	if id <= 0 || owner == "" || leaseVersion <= 0 || purchaseToken == "" {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx, beginHeroSMSPurchaseOwnedSQL,
		id, owner, leaseVersion, time.Now().UTC(), purchaseToken,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func (s *PostgresStore) ReleaseHeroSMSPurchaseOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	purchaseToken string,
	nextRunAt time.Time,
	lastError string,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	purchaseToken = strings.TrimSpace(purchaseToken)
	if id <= 0 || owner == "" || leaseVersion <= 0 || purchaseToken == "" || nextRunAt.IsZero() {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx, releaseHeroSMSPurchaseOwnedSQL,
		id, owner, leaseVersion, time.Now().UTC(), purchaseToken, nextRunAt, strings.TrimSpace(lastError),
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func (s *PostgresStore) ScheduleHeroSMSTaskOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	status domain.HeroSMSNumberTaskStatus,
	nextRunAt time.Time,
	lastError string,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	if id <= 0 || owner == "" || leaseVersion <= 0 || !status.Valid() ||
		status.Terminal() || status == domain.HeroSMSTaskPurchasing || nextRunAt.IsZero() {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx, scheduleHeroSMSTaskOwnedSQL,
		id, owner, leaseVersion, time.Now().UTC(), string(status), nextRunAt, strings.TrimSpace(lastError),
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func normalizeCommitHeroSMSPurchaseParams(
	params CommitHeroSMSPurchaseParams,
) (CommitHeroSMSPurchaseParams, error) {
	params.PurchaseToken = strings.TrimSpace(params.PurchaseToken)
	params.ProviderActivationID = strings.TrimSpace(params.ProviderActivationID)
	params.PhoneNumber = strings.TrimSpace(params.PhoneNumber)
	params.Operator = strings.TrimSpace(params.Operator)
	params.ActivationCost = strings.TrimSpace(params.ActivationCost)
	params.Currency = strings.TrimSpace(params.Currency)
	if params.PurchaseToken == "" || params.ProviderActivationID == "" || params.PhoneNumber == "" {
		return CommitHeroSMSPurchaseParams{}, ErrInvalidInput
	}
	if params.PurchasedAt.IsZero() {
		params.PurchasedAt = time.Now().UTC()
	}
	if params.NextRunAt.IsZero() {
		params.NextRunAt = params.PurchasedAt
	}
	if !params.RefundStatus.Valid() {
		return CommitHeroSMSPurchaseParams{}, ErrInvalidInput
	}
	if params.RefundStatus == domain.HeroSMSRefundUnknown {
		if params.RefundableUntil != nil && params.RefundableUntil.After(params.PurchasedAt) {
			params.RefundStatus = domain.HeroSMSRefundRefundable
		} else {
			params.RefundStatus = domain.HeroSMSRefundUnavailable
		}
	}
	params.ProviderPayload = validJSONOrObject(params.ProviderPayload)
	if !json.Valid(params.ProviderPayload) {
		return CommitHeroSMSPurchaseParams{}, ErrInvalidInput
	}
	return params, nil
}

func (s *PostgresStore) CommitHeroSMSPurchaseOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	params CommitHeroSMSPurchaseParams,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	if id <= 0 || owner == "" || leaseVersion <= 0 {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	params, err := normalizeCommitHeroSMSPurchaseParams(params)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.HeroSMSNumberTask{}, fmt.Errorf("begin HeroSMS purchase commit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = lockProviderActivation(ctx, tx, "hero-sms", params.ProviderActivationID); err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	now := time.Now().UTC()
	_, err = scanHeroSMSTask(tx.QueryRow(ctx, `SELECT `+heroSMSTaskColumns+`
		FROM hero_sms_number_tasks WHERE id=$1 AND lease_owner=$2 AND lease_version=$3
			AND lease_until > $4 AND status='purchasing' AND purchase_token=$5
			AND finished_at IS NULL FOR UPDATE`, id, owner, leaseVersion, now, params.PurchaseToken))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	task, err := persistHeroSMSPurchaseOutcome(ctx, tx, id, params)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.HeroSMSNumberTask{}, fmt.Errorf("%w: commit HeroSMS purchase: %v", ErrCommitUnknown, err)
	}
	return task, nil
}

func persistHeroSMSPurchaseOutcome(
	ctx context.Context,
	tx pgx.Tx,
	id int64,
	params CommitHeroSMSPurchaseParams,
) (domain.HeroSMSNumberTask, error) {
	_, err := tx.Exec(ctx, persistHeroSMSPurchaseOutcomeSQL,
		id, params.ProviderActivationID, params.PhoneNumber, params.Operator,
		nullableNumeric(params.ActivationCost), params.Currency, params.PurchasedAt,
		params.ExpiresAt, params.RefundableUntil, string(params.RefundStatus),
		params.SupportsContinuation, params.ProviderPayload, params.NextRunAt)
	if err != nil {
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE hero_sms_number_messages SET task_id=$1
		WHERE task_id IS NULL AND provider_activation_id=$2`, id, params.ProviderActivationID); err != nil {
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	// Attach and count early callbacks in this same transaction. They revoke the
	// refund display before the purchased task can become externally visible.
	if _, err = tx.Exec(ctx, `UPDATE hero_sms_number_tasks SET
		message_count=(SELECT count(*)::integer FROM hero_sms_number_messages WHERE task_id=$1),
		first_message_at=(SELECT min(COALESCE(provider_received_at,created_at))
			FROM hero_sms_number_messages WHERE task_id=$1),
		refund_status=CASE WHEN EXISTS(
			SELECT 1 FROM hero_sms_number_messages WHERE task_id=$1
		) THEN 'unavailable' ELSE refund_status END,
		next_run_at=CASE WHEN stop_requested OR EXISTS(
			SELECT 1 FROM hero_sms_number_messages WHERE task_id=$1
		) THEN LEAST(next_run_at,now())
		ELSE LEAST(next_run_at,COALESCE(webhook_wakeup_at,next_run_at)) END,
		webhook_wakeup_at=CASE WHEN EXISTS(
			SELECT 1 FROM hero_sms_number_messages WHERE task_id=$1
		) THEN now() ELSE NULL END,
		updated_at=now()
		WHERE id=$1`, id); err != nil {
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	task, err := scanHeroSMSTask(tx.QueryRow(ctx,
		`SELECT `+heroSMSTaskColumns+` FROM hero_sms_number_tasks WHERE id=$1`, id))
	if err != nil {
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

// RecoverHeroSMSPurchaseOutcome records a provider success after the worker's
// lease expired or its commit response was lost. The original purchase token,
// provider identity lock, and provider partial-unique index form the fence; no
// path from this method returns the slot to waiting_number.
func (s *PostgresStore) RecoverHeroSMSPurchaseOutcome(
	ctx context.Context,
	id int64,
	purchaseToken string,
	params CommitHeroSMSPurchaseParams,
) (domain.HeroSMSNumberTask, error) {
	purchaseToken = strings.TrimSpace(purchaseToken)
	if id <= 0 || purchaseToken == "" {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	params.PurchaseToken = purchaseToken
	params, err := normalizeCommitHeroSMSPurchaseParams(params)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.HeroSMSNumberTask{}, fmt.Errorf("begin HeroSMS purchase recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = lockProviderActivation(ctx, tx, "hero-sms", params.ProviderActivationID); err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	var status string
	var storedProviderID sql.NullString
	if err = tx.QueryRow(ctx, recoverHeroSMSPurchaseOutcomeSQL, id, purchaseToken).
		Scan(&status, &storedProviderID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	if storedProviderID.Valid && storedProviderID.String != params.ProviderActivationID {
		return domain.HeroSMSNumberTask{}, ErrConflict
	}
	if status == string(domain.HeroSMSTaskStopped) {
		// A stop may win while a successful remote response is returning after
		// lease expiry. Reopen only this token-matched row and preserve the stop
		// intent so the recovered allocation is immediately settled.
		if _, err = tx.Exec(ctx, `UPDATE hero_sms_number_tasks SET
			stop_requested=true, finished_at=NULL WHERE id=$1`, id); err != nil {
			return domain.HeroSMSNumberTask{}, mapError(err)
		}
	}
	task, err := persistHeroSMSPurchaseOutcome(ctx, tx, id, params)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.HeroSMSNumberTask{}, fmt.Errorf("%w: commit recovered HeroSMS purchase: %v", ErrCommitUnknown, err)
	}
	return task, nil
}

func normalizeMarkHeroSMSPurchaseUnknownParams(
	params MarkHeroSMSPurchaseUnknownParams,
) (MarkHeroSMSPurchaseUnknownParams, error) {
	params.PurchaseToken = strings.TrimSpace(params.PurchaseToken)
	params.ProviderActivationID = strings.TrimSpace(params.ProviderActivationID)
	params.LastError = strings.TrimSpace(params.LastError)
	if params.PurchaseToken == "" || params.NextRunAt.IsZero() {
		return MarkHeroSMSPurchaseUnknownParams{}, ErrInvalidInput
	}
	params.ProviderPayload = validJSONOrObject(params.ProviderPayload)
	if !json.Valid(params.ProviderPayload) {
		return MarkHeroSMSPurchaseUnknownParams{}, ErrInvalidInput
	}
	return params, nil
}

func (s *PostgresStore) MarkHeroSMSPurchaseUnknownOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	params MarkHeroSMSPurchaseUnknownParams,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	if id <= 0 || owner == "" || leaseVersion <= 0 {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	params, err := normalizeMarkHeroSMSPurchaseUnknownParams(params)
	if err != nil {
		return domain.HeroSMSNumberTask{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.HeroSMSNumberTask{}, fmt.Errorf("begin unknown HeroSMS purchase: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if params.ProviderActivationID != "" {
		if err = lockProviderActivation(ctx, tx, "hero-sms", params.ProviderActivationID); err != nil {
			return domain.HeroSMSNumberTask{}, err
		}
	}
	now := time.Now().UTC()
	result, err := tx.Exec(ctx, `UPDATE hero_sms_number_tasks SET
		status='purchase_unknown',
		provider_activation_id=COALESCE($6,provider_activation_id),
		provider_payload=$7::json, next_run_at=$8,
		lease_owner='', lease_until=NULL, retry_count=retry_count+1,
		last_error=$9, updated_at=now()
		WHERE id=$1 AND lease_owner=$2 AND lease_version=$3 AND lease_until > $4
			AND status='purchasing' AND purchase_token=$5 AND finished_at IS NULL`,
		id, owner, leaseVersion, now, params.PurchaseToken,
		nullableText(params.ProviderActivationID), params.ProviderPayload,
		params.NextRunAt, params.LastError)
	if err != nil {
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	if result.RowsAffected() == 0 {
		return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
	}
	if params.ProviderActivationID != "" {
		if _, err = tx.Exec(ctx, `UPDATE hero_sms_number_messages SET task_id=$1
			WHERE task_id IS NULL AND provider_activation_id=$2`, id, params.ProviderActivationID); err != nil {
			return domain.HeroSMSNumberTask{}, mapError(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE hero_sms_number_tasks SET
			message_count=(SELECT count(*)::integer FROM hero_sms_number_messages WHERE task_id=$1),
			first_message_at=(SELECT min(COALESCE(provider_received_at,created_at))
				FROM hero_sms_number_messages WHERE task_id=$1)
			WHERE id=$1`, id); err != nil {
			return domain.HeroSMSNumberTask{}, mapError(err)
		}
	}
	task, err := scanHeroSMSTask(tx.QueryRow(ctx,
		`SELECT `+heroSMSTaskColumns+` FROM hero_sms_number_tasks WHERE id=$1`, id))
	if err != nil {
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.HeroSMSNumberTask{}, fmt.Errorf("%w: commit unknown HeroSMS purchase: %v", ErrCommitUnknown, err)
	}
	return task, nil
}

func (s *PostgresStore) RequestHeroSMSTaskStop(
	ctx context.Context,
	id int64,
) (domain.HeroSMSNumberTask, error) {
	if id <= 0 {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx, requestHeroSMSTaskStopSQL, id))
	if err == nil {
		return task, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	// Stopping an already terminal task is idempotent.
	task, err = scanHeroSMSTask(s.pool.QueryRow(ctx,
		`SELECT `+heroSMSTaskColumns+` FROM hero_sms_number_tasks WHERE id=$1`, id))
	if err != nil {
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func (s *PostgresStore) RestartHeroSMSTask(
	ctx context.Context,
	id int64,
	nextRunAt time.Time,
) (domain.HeroSMSNumberTask, error) {
	if id <= 0 || nextRunAt.IsZero() {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	// Only a never-purchased stopped slot can restart. Refunded, settled,
	// expired, or any row with a provider allocation is immutable.
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx, restartHeroSMSTaskSQL, id, nextRunAt))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func (s *PostgresStore) PrepareHeroSMSTaskSettlementOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	now time.Time,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	if id <= 0 || owner == "" || leaseVersion <= 0 || now.IsZero() {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	// requested is a durable remote-cancel intent. Once selected it is not
	// downgraded merely because the clock advances while the provider call is
	// in flight. Concurrent messages increment message_count and fence a later
	// refunded finish without erasing that audit intent.
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx,
		prepareHeroSMSTaskSettlementOwnedSQL, id, owner, leaseVersion, now))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func (s *PostgresStore) CompleteHeroSMSContinuationOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	observedMessageCount int,
	nextRunAt time.Time,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	if id <= 0 || owner == "" || leaseVersion <= 0 || observedMessageCount <= 0 || nextRunAt.IsZero() {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx, completeHeroSMSContinuationOwnedSQL,
		id, owner, leaseVersion, time.Now().UTC(), observedMessageCount, nextRunAt))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func (s *PostgresStore) BeginHeroSMSContinuationOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	now time.Time,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	if id <= 0 || owner == "" || leaseVersion <= 0 || now.IsZero() {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx, beginHeroSMSContinuationOwnedSQL,
		id, owner, leaseVersion, now.UTC()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func (s *PostgresStore) AbortHeroSMSContinuationOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	target int,
	nextRunAt time.Time,
	lastError string,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	if id <= 0 || owner == "" || leaseVersion <= 0 || target <= 0 || nextRunAt.IsZero() {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx, abortHeroSMSContinuationOwnedSQL,
		id, owner, leaseVersion, time.Now().UTC(), target, nextRunAt.UTC(), strings.TrimSpace(lastError)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func (s *PostgresStore) FinishHeroSMSTaskOwned(
	ctx context.Context,
	id int64,
	owner string,
	leaseVersion int64,
	status domain.HeroSMSNumberTaskStatus,
	refundStatus domain.HeroSMSRefundStatus,
	lastError string,
) (domain.HeroSMSNumberTask, error) {
	owner = strings.TrimSpace(owner)
	if id <= 0 || owner == "" || leaseVersion <= 0 || !status.Valid() ||
		!status.Terminal() || !refundStatus.Valid() {
		return domain.HeroSMSNumberTask{}, ErrInvalidInput
	}
	now := time.Now().UTC()
	task, err := scanHeroSMSTask(s.pool.QueryRow(ctx, finishHeroSMSTaskOwnedSQL,
		id, owner, leaseVersion, now, string(status), string(refundStatus), strings.TrimSpace(lastError),
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberTask{}, s.notFoundOrConflict(ctx, "hero_sms_number_tasks", id)
		}
		return domain.HeroSMSNumberTask{}, mapError(err)
	}
	return task, nil
}

func normalizeAppendHeroSMSTaskMessageParams(
	params AppendHeroSMSTaskMessageParams,
) (AppendHeroSMSTaskMessageParams, error) {
	params.ProviderActivationID = strings.TrimSpace(params.ProviderActivationID)
	params.ProviderMessageID = strings.TrimSpace(params.ProviderMessageID)
	params.Code = strings.TrimSpace(params.Code)
	params.Text = strings.TrimSpace(params.Text)
	if params.TaskID != nil && *params.TaskID <= 0 {
		return AppendHeroSMSTaskMessageParams{}, ErrInvalidInput
	}
	if params.ProviderActivationID == "" || !params.Source.Valid() ||
		(params.Code == "" && params.Text == "") || len(params.RawPayload) == 0 ||
		!json.Valid(params.RawPayload) {
		return AppendHeroSMSTaskMessageParams{}, ErrInvalidInput
	}
	params.RawPayload = cloneJSON(params.RawPayload)
	if params.ProviderReceivedAt != nil {
		at := params.ProviderReceivedAt.UTC()
		if at.IsZero() {
			params.ProviderReceivedAt = nil
		} else {
			params.ProviderReceivedAt = &at
		}
	}
	return params, nil
}

// heroSMSTaskMessageFingerprint excludes delivery source and raw JSON
// formatting. HeroSMS callbacks omit the rent-message ID while getAllSms
// includes it, so a provider timestamp takes precedence whenever present.
// That lets the two transports converge while preserving the same code sent
// again at a later time. Provider ID is used only when no timestamp exists;
// short activations without either use content plus the pending-cycle query.
func heroSMSTaskMessageFingerprint(params AppendHeroSMSTaskMessageParams) string {
	return heroSMSTaskMessageFingerprintForTask(params, "", 0)
}

func heroSMSTaskMessageFingerprintForTask(
	params AppendHeroSMSTaskMessageParams,
	productKind domain.HeroSMSProductKind,
	continuationCount int,
) string {
	hash := sha256.New()
	write := func(value string) {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	write(params.ProviderActivationID)
	if params.ProviderReceivedAt != nil {
		write("provider-received-at")
		write(params.ProviderReceivedAt.UTC().Format(time.RFC3339Nano))
		if params.Code != "" {
			write("code")
			write(params.Code)
		} else {
			write("text")
			write(params.Text)
		}
		return hex.EncodeToString(hash.Sum(nil))
	}
	if params.ProviderMessageID != "" {
		write("provider-message-id")
		write(params.ProviderMessageID)
		return hex.EncodeToString(hash.Sum(nil))
	}
	if productKind == domain.HeroSMSProductActivation {
		write("activation-cycle")
		write(strconv.Itoa(continuationCount))
		write(params.Code)
		write(params.Text)
		return hex.EncodeToString(hash.Sum(nil))
	}
	write("content-fallback")
	write(params.Code)
	write(params.Text)
	return hex.EncodeToString(hash.Sum(nil))
}

// A pending continuation target is already the durable boundary between the
// message cycle being advanced remotely and any message arriving concurrently.
// Use it before the provider call is acknowledged locally so a same-code
// callback from the next cycle cannot collide with or enrich the prior row.
func heroSMSMessageCycle(continuationCount, continuationPendingCount int) int {
	if continuationPendingCount > continuationCount {
		return continuationPendingCount
	}
	return continuationCount
}

func findExistingHeroSMSTaskMessage(
	ctx context.Context,
	tx pgx.Tx,
	params AppendHeroSMSTaskMessageParams,
	taskID sql.NullInt64,
	productKind string,
	continuationCount int,
) (domain.HeroSMSNumberMessage, bool, error) {
	if params.ProviderMessageID != "" {
		message, err := scanHeroSMSTaskMessage(tx.QueryRow(ctx,
			findHeroSMSMessageByProviderIDSQL,
			params.ProviderActivationID, params.ProviderMessageID,
		))
		if err == nil {
			return message, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberMessage{}, false, mapError(err)
		}
	}
	if taskID.Valid && domain.HeroSMSProductKind(productKind) == domain.HeroSMSProductActivation {
		message, err := scanHeroSMSTaskMessage(tx.QueryRow(ctx,
			findPendingHeroSMSActivationMessageSQL,
			taskID.Int64, continuationCount, params.Code, params.Text,
		))
		if err == nil {
			return message, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberMessage{}, false, mapError(err)
		}
		if shouldFindDelayedConfirmedHeroSMSActivationMessage(params) {
			message, err = scanHeroSMSTaskMessage(tx.QueryRow(ctx,
				findDelayedConfirmedHeroSMSActivationMessageSQL,
				taskID.Int64, continuationCount, params.Code, params.Text,
				params.ProviderReceivedAt, params.ProviderMessageID,
			))
			if err == nil {
				return message, true, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return domain.HeroSMSNumberMessage{}, false, mapError(err)
			}
		}
	}
	return domain.HeroSMSNumberMessage{}, false, nil
}

func shouldFindDelayedConfirmedHeroSMSActivationMessage(params AppendHeroSMSTaskMessageParams) bool {
	return params.Source == domain.HeroSMSMessageWebhook && params.ProviderReceivedAt != nil
}

func (s *PostgresStore) AppendHeroSMSTaskMessage(
	ctx context.Context,
	params AppendHeroSMSTaskMessageParams,
) (AppendHeroSMSTaskMessageResult, error) {
	params, err := normalizeAppendHeroSMSTaskMessageParams(params)
	if err != nil {
		return AppendHeroSMSTaskMessageResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AppendHeroSMSTaskMessageResult{}, fmt.Errorf("begin HeroSMS message append: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = lockProviderActivation(ctx, tx, "hero-sms", params.ProviderActivationID); err != nil {
		return AppendHeroSMSTaskMessageResult{}, err
	}

	var taskID sql.NullInt64
	var productKind string
	var continuationCount int
	var continuationPendingCount int
	if params.TaskID != nil {
		var storedProviderID sql.NullString
		err = tx.QueryRow(ctx, `SELECT provider_activation_id, product_kind,
			continuation_count, continuation_pending_count
			FROM hero_sms_number_tasks WHERE id=$1 FOR UPDATE`, *params.TaskID).
			Scan(&storedProviderID, &productKind, &continuationCount, &continuationPendingCount)
		if err != nil {
			return AppendHeroSMSTaskMessageResult{}, mapError(err)
		}
		if !storedProviderID.Valid || storedProviderID.String != params.ProviderActivationID {
			return AppendHeroSMSTaskMessageResult{}, ErrConflict
		}
		taskID = sql.NullInt64{Int64: *params.TaskID, Valid: true}
	} else {
		err = tx.QueryRow(ctx, `SELECT id, product_kind, continuation_count,
			continuation_pending_count FROM hero_sms_number_tasks
			WHERE provider='hero-sms' AND provider_activation_id=$1 FOR UPDATE`,
			params.ProviderActivationID).
			Scan(&taskID, &productKind, &continuationCount, &continuationPendingCount)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return AppendHeroSMSTaskMessageResult{}, mapError(err)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			taskID = sql.NullInt64{}
		}
	}
	messageCycle := heroSMSMessageCycle(continuationCount, continuationPendingCount)
	fingerprint := heroSMSTaskMessageFingerprintForTask(
		params, domain.HeroSMSProductKind(productKind), messageCycle,
	)

	message, duplicate, err := findExistingHeroSMSTaskMessage(
		ctx, tx, params, taskID, productKind, messageCycle,
	)
	if err != nil {
		return AppendHeroSMSTaskMessageResult{}, err
	}
	inserted := false
	if !duplicate {
		var scanErr error
		message, scanErr = scanHeroSMSTaskMessage(tx.QueryRow(ctx,
			`INSERT INTO hero_sms_number_messages(
				task_id, provider_activation_id, provider_message_id, source,
				code, message_text, provider_received_at, raw_payload, payload_fingerprint
			) VALUES($1,$2,$3,$4,$5,$6,$7,$8::json,$9)
			ON CONFLICT(payload_fingerprint) DO NOTHING
			RETURNING `+heroSMSTaskMessageColumns,
			nullableHeroSMSTaskID(taskID), params.ProviderActivationID,
			params.ProviderMessageID, string(params.Source), params.Code, params.Text,
			params.ProviderReceivedAt, params.RawPayload, fingerprint,
		))
		inserted = scanErr == nil
		if scanErr != nil && !errors.Is(scanErr, pgx.ErrNoRows) {
			return AppendHeroSMSTaskMessageResult{}, mapError(scanErr)
		}
	}
	attached := false
	if duplicate {
		attached = taskID.Valid && message.TaskID == nil
		message, err = enrichExistingHeroSMSTaskMessage(ctx, tx, message, taskID, params)
		if err != nil {
			return AppendHeroSMSTaskMessageResult{}, err
		}
	} else if !inserted {
		// The only remaining conflict is the permanent fingerprint key. Provider
		// identity is serialized by the advisory lock above, so the row is stable.
		message, err = scanHeroSMSTaskMessage(tx.QueryRow(ctx,
			`SELECT `+heroSMSTaskMessageColumns+`
			FROM hero_sms_number_messages WHERE payload_fingerprint=$1`, fingerprint))
		if err != nil {
			return AppendHeroSMSTaskMessageResult{}, mapError(err)
		}
		attached = taskID.Valid && message.TaskID == nil
		message, err = enrichExistingHeroSMSTaskMessage(ctx, tx, message, taskID, params)
		if err != nil {
			return AppendHeroSMSTaskMessageResult{}, err
		}
	}

	var task *domain.HeroSMSNumberTask
	if taskID.Valid {
		wakeAt := time.Now().UTC()
		if params.ProviderReceivedAt != nil && params.ProviderReceivedAt.Before(wakeAt) {
			wakeAt = *params.ProviderReceivedAt
		}
		if inserted || attached {
			if _, err = tx.Exec(ctx, wakeHeroSMSTaskSQL, taskID.Int64, wakeAt); err != nil {
				return AppendHeroSMSTaskMessageResult{}, mapError(err)
			}
		}
		updated, scanTaskErr := scanHeroSMSTask(tx.QueryRow(ctx,
			`SELECT `+heroSMSTaskColumns+` FROM hero_sms_number_tasks WHERE id=$1`, taskID.Int64))
		if scanTaskErr != nil {
			return AppendHeroSMSTaskMessageResult{}, mapError(scanTaskErr)
		}
		task = &updated
	}
	if err = tx.Commit(ctx); err != nil {
		return AppendHeroSMSTaskMessageResult{}, fmt.Errorf("%w: commit HeroSMS message: %v", ErrCommitUnknown, err)
	}
	return AppendHeroSMSTaskMessageResult{Message: message, Task: task, Inserted: inserted}, nil
}

func enrichExistingHeroSMSTaskMessage(
	ctx context.Context,
	tx pgx.Tx,
	message domain.HeroSMSNumberMessage,
	taskID sql.NullInt64,
	params AppendHeroSMSTaskMessageParams,
) (domain.HeroSMSNumberMessage, error) {
	updated, err := scanHeroSMSTaskMessage(tx.QueryRow(ctx,
		enrichExistingHeroSMSTaskMessageSQL,
		message.ID, nullableHeroSMSTaskID(taskID), params.ProviderActivationID,
		params.ProviderMessageID, params.ProviderReceivedAt,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.HeroSMSNumberMessage{}, ErrConflict
		}
		return domain.HeroSMSNumberMessage{}, mapError(err)
	}
	return updated, nil
}

func nullableHeroSMSTaskID(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}
