package storage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
)

var _ HeroSMSTaskStore = (*PostgresStore)(nil)

func TestHeroSMSTaskMigrationIsIndependentAndAppendOnly(t *testing.T) {
	var builder strings.Builder
	for _, migration := range migrations {
		if migration.version != 11 {
			continue
		}
		for _, statement := range migration.statements {
			builder.WriteString(statement)
			builder.WriteByte('\n')
		}
	}
	schema := builder.String()
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS hero_sms_task_submissions",
		"request_fingerprint char(64) NOT NULL",
		"CREATE TABLE IF NOT EXISTS hero_sms_number_tasks",
		"submission_id text NOT NULL REFERENCES hero_sms_task_submissions(id) ON DELETE RESTRICT",
		"'waiting_number','purchasing','active','purchase_unknown','settling','stopped','refunded','settled','expired'",
		"product_kind IN ('activation','rent')",
		"verification_type text NOT NULL",
		"duration_hours integer",
		"activation_cost numeric(18,6)",
		"expires_at timestamptz",
		"refundable_until timestamptz",
		"message_count integer NOT NULL DEFAULT 0",
		"continuation_count integer NOT NULL DEFAULT 0",
		"CHECK (continuation_count <= message_count)",
		"lease_version bigint NOT NULL DEFAULT 0",
		"stop_requested boolean NOT NULL DEFAULT false",
		"webhook_wakeup_at timestamptz",
		"hero_sms_number_tasks_provider_activation_idx",
		"WHERE provider_activation_id IS NOT NULL",
		"CREATE TABLE IF NOT EXISTS hero_sms_number_messages",
		"task_id bigint REFERENCES hero_sms_number_tasks(id) ON DELETE SET NULL",
		"payload_fingerprint char(64) NOT NULL UNIQUE",
		"hero_sms_number_messages_provider_id_idx",
		"source IN ('webhook','poll')",
		"WHERE task_id IS NULL",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("HeroSMS task migration missing %q: %s", required, schema)
		}
	}
	for _, coupled := range []string{"REFERENCES batches", "REFERENCES activations", "ON DELETE CASCADE"} {
		if strings.Contains(schema, coupled) {
			t.Fatalf("independent HeroSMS schema contains %q: %s", coupled, schema)
		}
	}
}

func TestHeroSMSTaskV12UpgradesEarlyV11Draft(t *testing.T) {
	var builder strings.Builder
	for _, migration := range migrations {
		if migration.version != 12 {
			continue
		}
		for _, statement := range migration.statements {
			builder.WriteString(statement)
			builder.WriteByte('\n')
		}
	}
	schema := builder.String()
	for _, required := range []string{
		"ADD COLUMN IF NOT EXISTS",
		"continuation_count integer NOT NULL DEFAULT 0",
		"UPDATE hero_sms_number_tasks SET",
		"continuation_count=GREATEST(0, LEAST(COALESCE(continuation_count, 0), message_count))",
		"continuation_count IS NULL",
		"continuation_count < 0",
		"continuation_count > message_count",
		"ALTER COLUMN continuation_count SET DEFAULT 0",
		"ALTER COLUMN continuation_count SET NOT NULL",
		"IF NOT EXISTS",
		"FROM pg_constraint",
		"conrelid='hero_sms_number_tasks'::regclass",
		"hero_sms_number_tasks_continuation_count_nonnegative_chk",
		"CHECK (continuation_count >= 0) NOT VALID",
		"VALIDATE CONSTRAINT hero_sms_number_tasks_continuation_count_nonnegative_chk",
		"hero_sms_number_tasks_continuation_count_lte_chk",
		"CHECK (continuation_count <= message_count) NOT VALID",
		"VALIDATE CONSTRAINT hero_sms_number_tasks_continuation_count_lte_chk",
		"WITH duplicate_provider_messages AS",
		"row_number() OVER",
		"PARTITION BY provider_activation_id, provider_message_id",
		"ORDER BY created_at ASC, id ASC",
		"WHERE duplicate_rank > 1",
		"UPDATE hero_sms_number_messages AS message SET",
		"provider_message_id=''",
		"CREATE UNIQUE INDEX IF NOT EXISTS hero_sms_number_messages_provider_id_idx",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("HeroSMS v12 compatibility migration missing %q: %s", required, schema)
		}
	}
	counterRepairAt := strings.Index(schema, "UPDATE hero_sms_number_tasks SET")
	setDefaultAt := strings.Index(schema, "ALTER COLUMN continuation_count SET DEFAULT 0")
	setNotNullAt := strings.Index(schema, "ALTER COLUMN continuation_count SET NOT NULL")
	nonnegativeConstraintAt := strings.Index(schema, "ADD CONSTRAINT hero_sms_number_tasks_continuation_count_nonnegative_chk")
	nonnegativeValidateAt := strings.Index(schema, "VALIDATE CONSTRAINT hero_sms_number_tasks_continuation_count_nonnegative_chk")
	upperConstraintAt := strings.Index(schema, "ADD CONSTRAINT hero_sms_number_tasks_continuation_count_lte_chk")
	upperValidateAt := strings.Index(schema, "VALIDATE CONSTRAINT hero_sms_number_tasks_continuation_count_lte_chk")
	if counterRepairAt < 0 || setDefaultAt < 0 || setNotNullAt < 0 || nonnegativeConstraintAt < 0 ||
		nonnegativeValidateAt < 0 || upperConstraintAt < 0 || upperValidateAt < 0 ||
		counterRepairAt >= setDefaultAt || setDefaultAt >= setNotNullAt ||
		setNotNullAt >= nonnegativeConstraintAt || nonnegativeConstraintAt >= nonnegativeValidateAt ||
		nonnegativeValidateAt >= upperConstraintAt || upperConstraintAt >= upperValidateAt {
		t.Fatalf("HeroSMS v12 must repair and normalize continuation_count before validating its constraints: %s", schema)
	}
	duplicateRepairAt := strings.Index(schema, "WITH duplicate_provider_messages AS")
	providerIndexAt := strings.Index(schema, "CREATE UNIQUE INDEX IF NOT EXISTS hero_sms_number_messages_provider_id_idx")
	if duplicateRepairAt < 0 || providerIndexAt < 0 || duplicateRepairAt >= providerIndexAt {
		t.Fatalf("HeroSMS v12 must clear duplicate provider message IDs before creating the unique index: %s", schema)
	}
	if strings.Contains(schema, "DELETE FROM hero_sms_number_messages") {
		t.Fatalf("HeroSMS v12 must retain duplicate message audit rows: %s", schema)
	}
}

func TestHeroSMSTaskV15AddsContinuationCrashState(t *testing.T) {
	var builder strings.Builder
	found := 0
	for _, migration := range migrations {
		if migration.version != 15 {
			continue
		}
		found++
		for _, statement := range migration.statements {
			builder.WriteString(statement)
			builder.WriteByte('\n')
		}
	}
	if found != 1 {
		t.Fatalf("HeroSMS continuation migration count = %d, want 1", found)
	}
	schema := builder.String()
	for _, required := range []string{
		"continuation_pending_count integer NOT NULL DEFAULT 0",
		"supports_continuation boolean NOT NULL DEFAULT false",
		"UPDATE hero_sms_number_tasks SET supports_continuation=false",
		"supports_continuation IS NULL",
		"UPDATE hero_sms_number_tasks SET supports_continuation=true",
		"product_kind='activation' AND verification_type='sms'",
		"provider_payload::jsonb -> 'canGetAnotherSms' = 'true'::jsonb",
		"provider_payload::jsonb -> 'can_get_another_sms' = 'true'::jsonb",
		"UPDATE hero_sms_number_tasks SET continuation_pending_count=0",
		"ALTER COLUMN continuation_pending_count SET DEFAULT 0",
		"ALTER COLUMN continuation_pending_count SET NOT NULL",
		"ALTER COLUMN supports_continuation SET DEFAULT false",
		"ALTER COLUMN supports_continuation SET NOT NULL",
		"hero_sms_number_tasks_continuation_pending_state_chk",
		"CHECK (continuation_pending_count=0 OR (",
		"status='active'",
		"product_kind='activation'",
		"supports_continuation=true",
		"continuation_count < continuation_pending_count",
		"continuation_pending_count <= message_count",
		"NOT VALID",
		"VALIDATE CONSTRAINT hero_sms_number_tasks_continuation_pending_state_chk",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("HeroSMS v15 continuation migration missing %q: %s", required, schema)
		}
	}
	addPendingAt := strings.Index(schema, "continuation_pending_count integer NOT NULL DEFAULT 0")
	addSupportAt := strings.Index(schema, "supports_continuation boolean NOT NULL DEFAULT false")
	repairSupportAt := strings.Index(schema, "UPDATE hero_sms_number_tasks SET supports_continuation=false")
	backfillAt := strings.Index(schema, "UPDATE hero_sms_number_tasks SET supports_continuation=true")
	repairAt := strings.Index(schema, "UPDATE hero_sms_number_tasks SET continuation_pending_count=0")
	constraintAt := strings.Index(schema, "ADD CONSTRAINT hero_sms_number_tasks_continuation_pending_state_chk")
	validateAt := strings.Index(schema, "VALIDATE CONSTRAINT hero_sms_number_tasks_continuation_pending_state_chk")
	if addPendingAt < 0 || addSupportAt < 0 || repairSupportAt < 0 || backfillAt < 0 || repairAt < 0 ||
		constraintAt < 0 || validateAt < 0 || addPendingAt >= repairSupportAt ||
		addSupportAt >= repairSupportAt || repairSupportAt >= backfillAt || backfillAt >= repairAt ||
		repairAt >= constraintAt || constraintAt >= validateAt {
		t.Fatalf("HeroSMS v15 must add, backfill, repair, add, then validate continuation state: %s", schema)
	}
}

func TestHeroSMSTaskClaimUsesLeaseFenceAndFreezesStalePurchases(t *testing.T) {
	for _, required := range []string{
		"FOR UPDATE SKIP LOCKED",
		"status='purchasing' AND lease_until <= $2",
		"status=CASE WHEN task.status='purchasing' THEN 'purchase_unknown'",
		"lease_owner=$1 || ':' || task.id || ':' || (task.lease_version+1)",
		"lease_version=task.lease_version+1",
		"status IN ('waiting_number','active')",
		"status='settling'",
		"next_run_at <= $2 OR webhook_wakeup_at <= $2",
		"status='purchase_unknown' AND stop_requested",
	} {
		if !strings.Contains(claimDueHeroSMSTasksSQL, required) {
			t.Fatalf("claim SQL missing %q: %s", required, claimDueHeroSMSTasksSQL)
		}
	}
	if strings.Contains(claimDueHeroSMSTasksSQL, "status IN ('waiting_number','active','purchase_unknown'") {
		t.Fatalf("unreconciled purchase_unknown task would be repeatedly claimed: %s", claimDueHeroSMSTasksSQL)
	}
}

func TestHeroSMSContinuationBeginSnapshotsTargetUnderOwnedLease(t *testing.T) {
	for _, required := range []string{
		"continuation_pending_count=message_count",
		"lease_owner=$2",
		"lease_version=$3",
		"lease_until > $4",
		"status='active'",
		"product_kind='activation'",
		"supports_continuation=true",
		"continuation_pending_count=0",
		"continuation_count < message_count",
		"NOT stop_requested",
		"finished_at IS NULL",
		"expires_at IS NULL OR expires_at > $4",
	} {
		if !strings.Contains(beginHeroSMSContinuationOwnedSQL, required) {
			t.Fatalf("continuation begin missing %q: %s", required, beginHeroSMSContinuationOwnedSQL)
		}
	}
	if strings.Contains(beginHeroSMSContinuationOwnedSQL, "lease_owner=''") ||
		strings.Contains(beginHeroSMSContinuationOwnedSQL, "lease_until=NULL") {
		t.Fatalf("continuation begin releases its owned lease: %s", beginHeroSMSContinuationOwnedSQL)
	}
}

func TestHeroSMSContinuationCompletesOnlyPendingTarget(t *testing.T) {
	for _, required := range []string{
		"continuation_count=$5",
		"continuation_pending_count=0",
		"continuation_pending_count=$5",
		"message_count>$5",
		"COALESCE(webhook_wakeup_at,now())",
		"continuation_count < $5",
		"$5 <= message_count",
		"lease_owner=$2",
		"lease_version=$3",
		"lease_until > $4",
		"product_kind='activation'",
		"supports_continuation=true",
	} {
		if !strings.Contains(completeHeroSMSContinuationOwnedSQL, required) {
			t.Fatalf("continuation completion missing %q: %s", required, completeHeroSMSContinuationOwnedSQL)
		}
	}
	for _, required := range []string{
		"continuation_pending_count>0", "THEN $6", "webhook_wakeup_at=NULL",
		"AND (continuation_pending_count=0 OR $5='active')",
	} {
		if !strings.Contains(scheduleHeroSMSTaskOwnedSQL, required) {
			t.Fatalf("continuation retry loses durable delta/backoff (%q): %s", required, scheduleHeroSMSTaskOwnedSQL)
		}
	}
	setClause := strings.SplitN(scheduleHeroSMSTaskOwnedSQL, "\n\tWHERE", 2)[0]
	if strings.Contains(setClause, "continuation_pending_count=") {
		t.Fatalf("uncertain continuation schedule clears pending target: %s", scheduleHeroSMSTaskOwnedSQL)
	}
}

func TestHeroSMSSettlingRetryRespectsBackoffAfterStop(t *testing.T) {
	for _, required := range []string{
		"status='settling'",
		"next_run_at <= $2 OR webhook_wakeup_at <= $2",
		"stop_requested AND status<>'settling'",
		"stop_requested AND $5<>'settling'",
	} {
		if !strings.Contains(claimDueHeroSMSTasksSQL+scheduleHeroSMSTaskOwnedSQL, required) {
			t.Fatalf("settling retry bypasses backoff (%q): claim=%s schedule=%s",
				required, claimDueHeroSMSTasksSQL, scheduleHeroSMSTaskOwnedSQL)
		}
	}
	if strings.Contains(claimDueHeroSMSTasksSQL,
		"status IN ('waiting_number','active','settling')") {
		t.Fatalf("settling task is still claimed solely by stop_requested: %s", claimDueHeroSMSTasksSQL)
	}
}

func TestHeroSMSPollErrorPreservesConcurrentWebhookWake(t *testing.T) {
	for _, required := range []string{
		"$5='active' AND btrim($7)<>'' AND continuation_pending_count>0",
		"ELSE LEAST($6,COALESCE(webhook_wakeup_at,$6))",
	} {
		if !strings.Contains(scheduleHeroSMSTaskOwnedSQL, required) {
			t.Fatalf("poll error loses concurrent webhook wake (%q): %s",
				required, scheduleHeroSMSTaskOwnedSQL)
		}
	}
	if strings.Contains(scheduleHeroSMSTaskOwnedSQL,
		"btrim($7)<>'' AND message_count>continuation_count") {
		t.Fatalf("ordinary webhook delta is incorrectly forced into error backoff: %s",
			scheduleHeroSMSTaskOwnedSQL)
	}
}

func TestHeroSMSContinuationAbortClearsOnlyMatchingPendingTarget(t *testing.T) {
	for _, required := range []string{
		"continuation_pending_count=0",
		"continuation_pending_count=$5",
		"lease_owner=$2",
		"lease_version=$3",
		"lease_until > $4",
		"status='active'",
		"product_kind='activation'",
		"supports_continuation=true",
		"next_run_at=CASE WHEN stop_requested THEN now()",
		"WHEN message_count>$5",
		"LEAST(next_run_at,COALESCE(webhook_wakeup_at,now())) ELSE $6",
		"THEN COALESCE(webhook_wakeup_at,now()) ELSE NULL",
		"lease_owner='', lease_until=NULL",
		"retry_count=retry_count+1",
		"last_error=$7",
	} {
		if !strings.Contains(abortHeroSMSContinuationOwnedSQL, required) {
			t.Fatalf("continuation abort missing %q: %s", required, abortHeroSMSContinuationOwnedSQL)
		}
	}
	setClause := strings.SplitN(abortHeroSMSContinuationOwnedSQL, "\n\tWHERE", 2)[0]
	if strings.Contains(setClause, "continuation_count=") {
		t.Fatalf("continuation abort acknowledges messages: %s", abortHeroSMSContinuationOwnedSQL)
	}
}

func TestHeroSMSContinuationCompletionPreservesConcurrentStopUrgency(t *testing.T) {
	stopBranchAt := strings.Index(completeHeroSMSContinuationOwnedSQL,
		"next_run_at=CASE WHEN stop_requested THEN now()")
	pendingMessageBranchAt := strings.Index(completeHeroSMSContinuationOwnedSQL,
		"WHEN message_count>$5")
	if stopBranchAt < 0 || pendingMessageBranchAt < 0 || stopBranchAt >= pendingMessageBranchAt {
		t.Fatalf("continuation completion does not prioritize a concurrent stop: %s",
			completeHeroSMSContinuationOwnedSQL)
	}
	for _, required := range []string{
		"webhook_wakeup_at=CASE WHEN stop_requested THEN webhook_wakeup_at",
		"lease_owner='', lease_until=NULL",
	} {
		if !strings.Contains(completeHeroSMSContinuationOwnedSQL, required) {
			t.Fatalf("continuation completion loses stop urgency %q: %s",
				required, completeHeroSMSContinuationOwnedSQL)
		}
	}
	if strings.Contains(completeHeroSMSContinuationOwnedSQL, "stop_requested=false") {
		t.Fatalf("continuation completion clears a concurrent stop: %s",
			completeHeroSMSContinuationOwnedSQL)
	}
}

func TestHeroSMSContinuationAbortPreservesConcurrentStopUrgency(t *testing.T) {
	stopBranchAt := strings.Index(abortHeroSMSContinuationOwnedSQL,
		"next_run_at=CASE WHEN stop_requested THEN now()")
	retryBranchAt := strings.Index(abortHeroSMSContinuationOwnedSQL,
		"WHEN message_count>$5")
	if stopBranchAt < 0 || retryBranchAt < 0 || stopBranchAt >= retryBranchAt {
		t.Fatalf("continuation abort does not prioritize a concurrent stop: %s",
			abortHeroSMSContinuationOwnedSQL)
	}
	if strings.Contains(abortHeroSMSContinuationOwnedSQL, "stop_requested=false") {
		t.Fatalf("continuation abort clears a concurrent stop: %s", abortHeroSMSContinuationOwnedSQL)
	}
}

func TestHeroSMSSuccessfulOperationsResetConsecutiveRetryCount(t *testing.T) {
	for name, statement := range map[string]string{
		"continuation": completeHeroSMSContinuationOwnedSQL,
	} {
		if !strings.Contains(statement, "retry_count=0") {
			t.Fatalf("%s success does not reset retry_count: %s", name, statement)
		}
	}
	for _, required := range []string{
		"retry_count=CASE WHEN btrim($7)<>'' THEN retry_count+1 ELSE 0 END",
		"last_error=$7",
	} {
		if !strings.Contains(scheduleHeroSMSTaskOwnedSQL, required) {
			t.Fatalf("schedule does not model consecutive failures %q: %s",
				required, scheduleHeroSMSTaskOwnedSQL)
		}
	}
	if !strings.Contains(persistHeroSMSPurchaseOutcomeSQL, "retry_count=0") {
		t.Fatalf("purchase success does not reset retry_count: %s",
			persistHeroSMSPurchaseOutcomeSQL)
	}
}

func TestHeroSMSPurchasePersistsContinuationCapabilityAndClearsPending(t *testing.T) {
	for _, required := range []string{
		"supports_continuation=$11",
		"continuation_pending_count=0",
		"provider_payload=$12::json",
		"next_run_at=$13",
	} {
		if !strings.Contains(persistHeroSMSPurchaseOutcomeSQL, required) {
			t.Fatalf("purchase persistence missing %q: %s", required, persistHeroSMSPurchaseOutcomeSQL)
		}
	}
	params := CommitHeroSMSPurchaseParams{
		PurchaseToken: "purchase-1", ProviderActivationID: "activation-1",
		PhoneNumber: "+15550000001", SupportsContinuation: true,
		ProviderPayload: json.RawMessage(`{}`), RefundStatus: domain.HeroSMSRefundUnavailable,
	}
	normalized, err := normalizeCommitHeroSMSPurchaseParams(params)
	if err != nil {
		t.Fatalf("normalize purchase capability: %v", err)
	}
	if !normalized.SupportsContinuation {
		t.Fatal("purchase normalization dropped continuation capability")
	}
}

func TestHeroSMSStateTransitionsClearContinuationPendingTarget(t *testing.T) {
	for name, statement := range map[string]string{
		"prepare settlement": prepareHeroSMSTaskSettlementOwnedSQL,
		"finish":             finishHeroSMSTaskOwnedSQL,
		"restart":            restartHeroSMSTaskSQL,
		"purchase":           persistHeroSMSPurchaseOutcomeSQL,
	} {
		setClause := strings.SplitN(statement, "\n\tWHERE", 2)[0]
		if !strings.Contains(setClause, "continuation_pending_count=0") {
			t.Fatalf("%s leaves continuation pending across state change: %s", name, statement)
		}
	}
}

func TestHeroSMSRestartCannotRepurchaseUnknownOutcome(t *testing.T) {
	for _, required := range []string{
		"status='stopped'", "provider_activation_id IS NULL", "purchase_token=''",
	} {
		if !strings.Contains(restartHeroSMSTaskSQL, required) {
			t.Fatalf("restart fence missing %q: %s", required, restartHeroSMSTaskSQL)
		}
	}
}

func TestHeroSMSStopImmediatelyFencesClaimedWaitingTask(t *testing.T) {
	for _, required := range []string{
		"stop_requested=true", "status='waiting_number'", "lease_owner=CASE",
		"THEN ''", "lease_until=CASE", "THEN NULL",
		"lease_version=lease_version+CASE WHEN status='waiting_number' THEN 1 ELSE 0 END",
	} {
		if !strings.Contains(requestHeroSMSTaskStopSQL, required) {
			t.Fatalf("stop fence missing %q: %s", required, requestHeroSMSTaskStopSQL)
		}
	}
	for _, required := range []string{"lease_owner=$2", "lease_version=$3", "NOT stop_requested"} {
		if !strings.Contains(beginHeroSMSPurchaseOwnedSQL, required) {
			t.Fatalf("stale purchase begin is not fenced by stop (%q): %s", required, beginHeroSMSPurchaseOwnedSQL)
		}
	}
}

func TestHeroSMSConfirmedPurchaseCanRecoverAfterLeaseExpiry(t *testing.T) {
	for _, required := range []string{
		"purchase_token=$2",
		"status IN ('purchasing','purchase_unknown','active','stopped')",
		"finished_at IS NULL OR status='stopped'",
		"FOR UPDATE",
	} {
		if !strings.Contains(recoverHeroSMSPurchaseOutcomeSQL, required) {
			t.Fatalf("confirmed purchase recovery missing %q: %s", required, recoverHeroSMSPurchaseOutcomeSQL)
		}
	}
	for _, forbidden := range []string{"lease_owner", "lease_version", "lease_until"} {
		if strings.Contains(recoverHeroSMSPurchaseOutcomeSQL, forbidden) {
			t.Fatalf("confirmed outcome recovery incorrectly depends on expired lease %q: %s", forbidden, recoverHeroSMSPurchaseOutcomeSQL)
		}
	}
}

func TestHeroSMSTaskPurchaseAndSettlementWritesAreFenced(t *testing.T) {
	for name, statement := range map[string]string{
		"begin":   beginHeroSMSPurchaseOwnedSQL,
		"release": releaseHeroSMSPurchaseOwnedSQL,
		"prepare": prepareHeroSMSTaskSettlementOwnedSQL,
		"finish":  finishHeroSMSTaskOwnedSQL,
	} {
		for _, required := range []string{
			"lease_owner=$2", "lease_version=$3", "lease_until > $4", "finished_at IS NULL",
		} {
			if !strings.Contains(statement, required) {
				t.Fatalf("%s SQL missing lease fence %q: %s", name, required, statement)
			}
		}
	}
	for _, required := range []string{
		"status='purchasing'", "purchase_token=$5", "status='waiting_number'",
		"purchase_token=''", "lease_owner=''", "lease_until=NULL",
	} {
		if !strings.Contains(beginHeroSMSPurchaseOwnedSQL+releaseHeroSMSPurchaseOwnedSQL, required) {
			t.Fatalf("conclusive no-number release is incomplete (%q)", required)
		}
	}
	for _, required := range []string{
		"status='settling'", "message_count=0", "refundable_until > $4",
		"expires_at IS NULL OR expires_at > $4", "refund_status='refundable'", "THEN 'requested'",
	} {
		if !strings.Contains(prepareHeroSMSTaskSettlementOwnedSQL, required) {
			t.Fatalf("settlement preparation missing %q: %s", required, prepareHeroSMSTaskSettlementOwnedSQL)
		}
	}
	for _, required := range []string{
		"$5='refunded'", "message_count=0", "refund_status='requested'",
	} {
		if !strings.Contains(finishHeroSMSTaskOwnedSQL, required) {
			t.Fatalf("refund finish CAS missing %q: %s", required, finishHeroSMSTaskOwnedSQL)
		}
	}
}

func TestHeroSMSTaskWakeupSurvivesWorkerAndExpiresRefund(t *testing.T) {
	for _, required := range []string{
		"webhook_wakeup_at=CASE WHEN finished_at IS NOT NULL",
		"LEAST(COALESCE(webhook_wakeup_at,$2),$2)",
		"message_count=(SELECT count(*)::integer",
		"refund_status=CASE WHEN status='active' THEN 'unavailable'",
	} {
		if !strings.Contains(wakeHeroSMSTaskSQL, required) {
			t.Fatalf("message wake SQL missing %q: %s", required, wakeHeroSMSTaskSQL)
		}
	}
	for _, required := range []string{
		"LEAST($6,COALESCE(webhook_wakeup_at,$6))",
		"webhook_wakeup_at=NULL",
		"refundable_until <= now()",
		"THEN 'unavailable'",
	} {
		if !strings.Contains(scheduleHeroSMSTaskOwnedSQL, required) {
			t.Fatalf("schedule SQL missing %q: %s", required, scheduleHeroSMSTaskOwnedSQL)
		}
	}
}

func TestHeroSMSTaskMessageFingerprintDedupesWebhookAndPoll(t *testing.T) {
	receivedAt := time.Date(2026, 8, 28, 1, 2, 3, 0, time.UTC)
	webhook := AppendHeroSMSTaskMessageParams{
		ProviderActivationID: "activation-1",
		Source:               domain.HeroSMSMessageWebhook,
		Code:                 "123456",
		Text:                 "Your code is 123456",
		ProviderReceivedAt:   &receivedAt,
		RawPayload:           json.RawMessage(`{"source":"webhook"}`),
	}
	poll := webhook
	poll.Source = domain.HeroSMSMessagePoll
	poll.ProviderMessageID = "message-1"
	poll.RawPayload = json.RawMessage(`{"source":"poll"}`)
	if got, want := heroSMSTaskMessageFingerprint(webhook), heroSMSTaskMessageFingerprint(poll); got != want {
		t.Fatalf("same provider message differs across transports: %q != %q", got, want)
	}
	providerIDOnly := webhook
	providerIDOnly.ProviderMessageID = "message-1"
	providerIDOnly.ProviderReceivedAt = nil
	changed := providerIDOnly
	changed.Code = "654321"
	changed.Text = "payload variant"
	if heroSMSTaskMessageFingerprint(providerIDOnly) != heroSMSTaskMessageFingerprint(changed) {
		t.Fatal("same provider message ID did not dedupe a payload variant")
	}
	secondMessage := providerIDOnly
	secondMessage.ProviderMessageID = "message-2"
	if heroSMSTaskMessageFingerprint(providerIDOnly) == heroSMSTaskMessageFingerprint(secondMessage) {
		t.Fatal("different provider message IDs share a fingerprint")
	}
	later := webhook
	later.ProviderMessageID = "message-2"
	laterReceivedAt := receivedAt.Add(time.Minute)
	later.ProviderReceivedAt = &laterReceivedAt
	if heroSMSTaskMessageFingerprint(webhook) == heroSMSTaskMessageFingerprint(later) {
		t.Fatal("same content from distinct provider timestamps was collapsed")
	}
	otherActivation := webhook
	otherActivation.ProviderActivationID = "activation-2"
	if heroSMSTaskMessageFingerprint(webhook) == heroSMSTaskMessageFingerprint(otherActivation) {
		t.Fatal("same content from distinct activations was collapsed")
	}
}

func TestHeroSMSActivationAsymmetricWebhookPollUsesPendingCycle(t *testing.T) {
	for _, required := range []string{
		"row_number() OVER (ORDER BY message.id)",
		"pending.message_ordinal > $2",
		"$3<>'' AND pending.code=$3",
		"pending.message_text=$4",
	} {
		if !strings.Contains(findPendingHeroSMSActivationMessageSQL, required) {
			t.Fatalf("activation cross-source candidate missing %q: %s", required, findPendingHeroSMSActivationMessageSQL)
		}
	}
	webhookReceivedAt := time.Date(2026, 8, 28, 1, 2, 3, 0, time.UTC)
	webhook := AppendHeroSMSTaskMessageParams{
		ProviderActivationID: "activation-1", Source: domain.HeroSMSMessageWebhook,
		Code: "123456", Text: "Your code is 123456", ProviderReceivedAt: &webhookReceivedAt,
		RawPayload: json.RawMessage(`{"source":"webhook"}`),
	}
	poll := webhook
	poll.Source = domain.HeroSMSMessagePoll
	poll.Text = "Your code is 123456"
	poll.ProviderReceivedAt = nil
	if got, want := heroSMSTaskMessageFingerprintForTask(webhook, domain.HeroSMSProductActivation, 0),
		heroSMSTaskMessageFingerprintForTask(webhook, domain.HeroSMSProductActivation, 1); got != want {
		t.Fatalf("stable activation webhook changed after continuation: %q != %q", got, want)
	}
	if heroSMSTaskMessageFingerprintForTask(poll, domain.HeroSMSProductActivation, 0) ==
		heroSMSTaskMessageFingerprintForTask(poll, domain.HeroSMSProductActivation, 1) {
		t.Fatal("same activation code in a confirmed new continuation cycle was collapsed")
	}
	laterWebhook := webhook
	laterReceivedAt := webhookReceivedAt.Add(time.Minute)
	laterWebhook.ProviderReceivedAt = &laterReceivedAt
	if heroSMSTaskMessageFingerprintForTask(webhook, domain.HeroSMSProductActivation, 0) ==
		heroSMSTaskMessageFingerprintForTask(laterWebhook, domain.HeroSMSProductActivation, 0) {
		t.Fatal("distinct timestamped activation webhooks share a fingerprint")
	}
}

func TestHeroSMSDelayedRichWebhookFoldsConfirmedMetadataFreePoll(t *testing.T) {
	for _, required := range []string{
		"row_number() OVER (ORDER BY message.id)",
		"confirmed.message_ordinal <= $2",
		"confirmed.source='poll'",
		"$3<>'' AND confirmed.code=$3",
		"confirmed.message_text=$4",
		"confirmed.provider_message_id='' AND confirmed.provider_received_at IS NULL",
		"$5::timestamptz <= confirmed.created_at",
		"ORDER BY confirmed.created_at, confirmed.id",
	} {
		if !strings.Contains(findDelayedConfirmedHeroSMSActivationMessageSQL, required) {
			t.Fatalf("delayed confirmed-poll fold missing %q: %s",
				required, findDelayedConfirmedHeroSMSActivationMessageSQL)
		}
	}
	if !strings.Contains(findPendingHeroSMSActivationMessageSQL, "pending.message_ordinal > $2") {
		t.Fatalf("current pending-cycle lookup lost its precedence boundary: %s",
			findPendingHeroSMSActivationMessageSQL)
	}
	receivedAt := time.Now().UTC()
	webhook := AppendHeroSMSTaskMessageParams{
		Source: domain.HeroSMSMessageWebhook, ProviderReceivedAt: &receivedAt,
	}
	if !shouldFindDelayedConfirmedHeroSMSActivationMessage(webhook) {
		t.Fatal("rich webhook did not enable delayed confirmed-poll folding")
	}
	poll := webhook
	poll.Source = domain.HeroSMSMessagePoll
	if shouldFindDelayedConfirmedHeroSMSActivationMessage(poll) {
		t.Fatal("rich poll incorrectly enabled delayed confirmed-poll folding")
	}
	webhook.ProviderReceivedAt = nil
	if shouldFindDelayedConfirmedHeroSMSActivationMessage(webhook) {
		t.Fatal("metadata-free webhook incorrectly enabled delayed confirmed-poll folding")
	}
}

func TestHeroSMSRepeatedDelayedRichWebhookFindsEnrichedPoll(t *testing.T) {
	for _, required := range []string{
		"confirmed.provider_received_at=$5",
		"confirmed.provider_message_id=''",
		"btrim($6)=''",
		"confirmed.provider_message_id=$6",
	} {
		if !strings.Contains(findDelayedConfirmedHeroSMSActivationMessageSQL, required) {
			t.Fatalf("repeated delayed webhook lookup missing %q: %s",
				required, findDelayedConfirmedHeroSMSActivationMessageSQL)
		}
	}
	for _, required := range []string{
		"provider_message_id=CASE WHEN provider_message_id=''",
		"provider_received_at=COALESCE(provider_received_at,$5)",
	} {
		if !strings.Contains(enrichExistingHeroSMSTaskMessageSQL, required) {
			t.Fatalf("delayed webhook enrichment missing %q", required)
		}
	}
}

func TestHeroSMSNextCycleSameCodeOneSecondLaterRemainsDistinct(t *testing.T) {
	receivedAt := time.Date(2026, 8, 28, 1, 2, 3, 0, time.UTC)
	laterReceivedAt := receivedAt.Add(time.Second)
	first := AppendHeroSMSTaskMessageParams{
		ProviderActivationID: "activation-1", Source: domain.HeroSMSMessageWebhook,
		Code: "123456", Text: "Your code is 123456", ProviderReceivedAt: &receivedAt,
	}
	later := first
	later.ProviderReceivedAt = &laterReceivedAt
	if heroSMSTaskMessageFingerprintForTask(first, domain.HeroSMSProductActivation, 1) ==
		heroSMSTaskMessageFingerprintForTask(later, domain.HeroSMSProductActivation, 1) {
		t.Fatal("same code outside delayed fold window shares a stable webhook fingerprint")
	}
	if !strings.Contains(findDelayedConfirmedHeroSMSActivationMessageSQL,
		"$5::timestamptz <= confirmed.created_at") {
		t.Fatalf("delayed fold has no strict poll-time upper bound: %s",
			findDelayedConfirmedHeroSMSActivationMessageSQL)
	}
	if strings.Contains(findDelayedConfirmedHeroSMSActivationMessageSQL, "confirmed.created_at +") {
		t.Fatalf("delayed fold can consume a later-cycle message through positive tolerance: %s",
			findDelayedConfirmedHeroSMSActivationMessageSQL)
	}
}

func TestHeroSMSPendingContinuationFencesConcurrentNextCycleMessage(t *testing.T) {
	if got := heroSMSMessageCycle(0, 1); got != 1 {
		t.Fatalf("message cycle during pending continuation = %d, want 1", got)
	}
	if got := heroSMSMessageCycle(2, 0); got != 2 {
		t.Fatalf("message cycle without pending continuation = %d, want 2", got)
	}
	message := AppendHeroSMSTaskMessageParams{
		ProviderActivationID: "activation-1", Source: domain.HeroSMSMessageWebhook,
		Code: "123456", Text: "Your code is 123456",
	}
	oldCycle := heroSMSTaskMessageFingerprintForTask(
		message, domain.HeroSMSProductActivation, heroSMSMessageCycle(0, 0),
	)
	nextCycleBeforeComplete := heroSMSTaskMessageFingerprintForTask(
		message, domain.HeroSMSProductActivation, heroSMSMessageCycle(0, 1),
	)
	if oldCycle == nextCycleBeforeComplete {
		t.Fatal("pending continuation did not fence a same-code next-cycle fingerprint")
	}
}

func TestNormalizeCreateHeroSMSTasksDurationRules(t *testing.T) {
	base := CreateHeroSMSTasksParams{
		SubmissionID:     "request-1",
		ProductKind:      domain.HeroSMSProductActivation,
		ServiceCode:      "wa",
		CountryCode:      "6",
		VerificationType: "sms",
		Quantity:         2,
	}
	if _, err := normalizeCreateHeroSMSTasksParams(base); err != nil {
		t.Fatalf("normalize activation: %v", err)
	}
	duration := 72
	invalidActivation := base
	invalidActivation.DurationHours = &duration
	if _, err := normalizeCreateHeroSMSTasksParams(invalidActivation); err != ErrInvalidInput {
		t.Fatalf("activation duration error = %v, want ErrInvalidInput", err)
	}
	rent := base
	rent.ProductKind = domain.HeroSMSProductRent
	if _, err := normalizeCreateHeroSMSTasksParams(rent); err != ErrInvalidInput {
		t.Fatalf("rent without duration error = %v, want ErrInvalidInput", err)
	}
	rent.DurationHours = &duration
	if _, err := normalizeCreateHeroSMSTasksParams(rent); err != nil {
		t.Fatalf("normalize rent: %v", err)
	}
	first, _ := normalizeCreateHeroSMSTasksParams(rent)
	second := first
	second.NextRunAt = first.NextRunAt.Add(time.Hour)
	second.ServiceName = "new display name"
	second.CountryName = "new country display name"
	second.MaxPriceAmount = "99.99"
	second.Currency = "USD"
	if heroSMSTaskSubmissionFingerprint(first) != heroSMSTaskSubmissionFingerprint(second) {
		t.Fatal("derived offer fields unexpectedly change create idempotency fingerprint")
	}
}
