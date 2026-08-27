package storage

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mangobubu/gopay-autosms/internal/domain"
)

func TestNormalizePage(t *testing.T) {
	if got := normalizePage(Page{}); got.Limit != 100 || got.Offset != 0 {
		t.Fatalf("normalizePage(default) = %+v", got)
	}
	if got := normalizePage(Page{Limit: 999, Offset: -1}); got.Limit != 500 || got.Offset != 0 {
		t.Fatalf("normalizePage(cap) = %+v", got)
	}
}

func TestBuildActivationListQuery(t *testing.T) {
	batchID := int64(7)
	query, args := buildActivationListQuery(ActivationFilter{
		BatchID:       &batchID,
		Statuses:      []domain.ActivationStatus{domain.ActivationStatusActive},
		PhoneContains: "8123",
		Page:          Page{Limit: 20, Offset: 40},
	})
	for _, expected := range []string{
		"batch_id=$1",
		"status=ANY($2::text[])",
		"hidden_at IS NULL",
		"phone_number ILIKE $3",
		"LIMIT $4 OFFSET $5",
	} {
		if !strings.Contains(query, expected) {
			t.Fatalf("query %q does not contain %q", query, expected)
		}
	}
	if len(args) != 5 || args[0] != batchID || args[2] != "%8123%" || args[3] != 20 || args[4] != 40 {
		t.Fatalf("args = %#v", args)
	}
}

func TestBuildActivationListQueryCanIncludeHidden(t *testing.T) {
	query, _ := buildActivationListQuery(ActivationFilter{IncludeHidden: true})
	if strings.Contains(query, "hidden_at IS NULL") {
		t.Fatalf("query unexpectedly filters hidden rows: %s", query)
	}
}

func TestMigrationHasAtomicDedupAndRestartIndexes(t *testing.T) {
	var all strings.Builder
	for _, migration := range migrations {
		for _, statement := range migration.statements {
			all.WriteString(statement)
			all.WriteByte('\n')
		}
	}
	schema := all.String()
	for _, required := range []string{
		"phone_fingerprint char(64) PRIMARY KEY",
		"UNIQUE (activation_id, cycle_no)",
		"activations_runnable_idx",
		"hidden_at timestamptz",
		"balance_rp numeric(24,6)",
		"target_pin_enc bytea",
		"next_purchase_at timestamptz",
		"purchased_count integer NOT NULL DEFAULT 0",
		"config jsonb",
		"status_changed_at timestamptz NOT NULL DEFAULT clock_timestamp()",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("schema missing %q", required)
		}
	}
}

func TestStatusChangedAtMigrationAndTransitionPreserveStateEntryTime(t *testing.T) {
	var freshSchemaSQL, migrationSQL strings.Builder
	for _, migration := range migrations {
		if migration.version != 1 && migration.version != 6 {
			continue
		}
		for _, statement := range migration.statements {
			builder := &migrationSQL
			if migration.version == 1 {
				builder = &freshSchemaSQL
			}
			builder.WriteString(statement)
			builder.WriteByte('\n')
		}
	}
	if sql := freshSchemaSQL.String(); !strings.Contains(sql,
		"status_changed_at timestamptz NOT NULL DEFAULT clock_timestamp()",
	) {
		t.Fatalf("fresh activation schema is missing status_changed_at: %s", sql)
	}
	if sql := migrationSQL.String(); !strings.Contains(sql,
		"ADD COLUMN IF NOT EXISTS status_changed_at timestamptz NOT NULL DEFAULT clock_timestamp()",
	) {
		t.Fatalf("status timestamp migration is incomplete: %s", sql)
	}

	if !strings.Contains(activationColumns, "status, status_changed_at, failure_reason") {
		t.Fatalf("activation scan columns do not expose status_changed_at beside status: %s", activationColumns)
	}
	for name, sql := range map[string]string{
		"generic transition": transitionStatusChangedAtSQL,
		"duplicate write":    duplicateStatusChangedAtSQL,
	} {
		if !strings.Contains(sql, "IS DISTINCT FROM") ||
			!strings.Contains(sql, "THEN clock_timestamp()") ||
			!strings.Contains(sql, "ELSE status_changed_at") {
			t.Fatalf("%s does not update only on an actual status change: %s", name, sql)
		}
	}
}

func TestPurchaseCountMigrationBackfillsAndConvergesRunningBatches(t *testing.T) {
	var migrationSQL strings.Builder
	for _, migration := range migrations {
		if migration.version != 3 {
			continue
		}
		for _, statement := range migration.statements {
			migrationSQL.WriteString(statement)
			migrationSQL.WriteByte('\n')
		}
	}
	sql := migrationSQL.String()
	for _, required := range []string{
		"ADD COLUMN IF NOT EXISTS purchased_count",
		"COUNT(*)::integer FROM activations",
		"a.batch_id=b.id",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("purchase-count migration missing %q: %s", required, sql)
		}
	}
	if !strings.Contains(batchColumns, "quantity, purchased_count, purchase_reserved_count, fulfilled_count, inflight_count") {
		t.Fatalf("batch scan columns do not expose purchased_count: %s", batchColumns)
	}
	if strings.Contains(sql, "status='completed'") || strings.Contains(sql, "finished_at=COALESCE") {
		t.Fatalf("purchase-count migration must not finalize active batches: %s", sql)
	}
}

func TestPurchaseTokenMigrationProvidesDurableQuotaLedger(t *testing.T) {
	var migrationSQL strings.Builder
	for _, migration := range migrations {
		if migration.version != 5 {
			continue
		}
		for _, statement := range migration.statements {
			migrationSQL.WriteString(statement)
			migrationSQL.WriteByte('\n')
		}
	}
	sql := migrationSQL.String()
	for _, required := range []string{
		"purchase_reserved_count integer NOT NULL DEFAULT 0",
		"purchase_protocol_version integer NOT NULL DEFAULT 0",
		"CREATE TABLE IF NOT EXISTS batch_purchase_attempts",
		"state IN ('reserved','sent','committed','released','unknown','conflicted')",
		"activation_id bigint UNIQUE",
		"cleanup_state text NOT NULL DEFAULT ''",
		"cleanup_lease_version bigint NOT NULL DEFAULT 0",
		"batch_purchase_attempts_cleanup_idx",
		"batch_purchase_attempts_provider_activation_idx",
		"batch_purchase_attempts_one_unresolved_idx",
		"WHERE state IN ('reserved','sent','unknown','conflicted')",
		"SELECT 'legacy:' || a.id::text",
		"status='failed'",
		"购买协议升级",
		"batches_purchase_reserved_max_one_chk",
		"purchased_count+purchase_reserved_count <= quantity",
		"batches_purchase_protocol_v1_chk",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("purchase-token migration missing %q: %s", required, sql)
		}
	}
}

func TestPurchaseTokenSQLMaintainsQuotaInvariant(t *testing.T) {
	for _, required := range []string{
		"purchase_reserved_count=purchase_reserved_count+1",
		"purchase_reserved_count=0",
		"purchased_count+purchase_reserved_count < quantity",
	} {
		if !strings.Contains(reserveBatchPurchaseSQL, required) {
			t.Fatalf("purchase reservation SQL missing %q: %s", required, reserveBatchPurchaseSQL)
		}
	}
	for _, required := range []string{
		"purchase_reserved_count=purchase_reserved_count-1",
		"purchase_reserved_count > 0",
	} {
		if !strings.Contains(releaseBatchPurchaseSQL, required) {
			t.Fatalf("purchase release SQL missing %q: %s", required, releaseBatchPurchaseSQL)
		}
		if !strings.Contains(recordBatchPurchaseSQL, required) {
			t.Fatalf("purchase commit SQL missing %q: %s", required, recordBatchPurchaseSQL)
		}
	}
	for _, required := range []string{
		"purchased_count=purchased_count+1",
		"inflight_count=inflight_count+1",
		"status=CASE WHEN status IN ('pending','running') THEN 'running' ELSE status END",
	} {
		if !strings.Contains(recordBatchPurchaseSQL, required) {
			t.Fatalf("purchase commit SQL missing %q: %s", required, recordBatchPurchaseSQL)
		}
	}
	if strings.Contains(freezeBatchPurchaseSQL, "purchase_reserved_count=") {
		t.Fatalf("unknown purchase must retain its quota: %s", freezeBatchPurchaseSQL)
	}
}

func TestFreezePurchaseResolvesPartialProviderIdentityBeforeOwnershipCheck(t *testing.T) {
	tests := []struct {
		name                                   string
		storedProvider, storedID, provider, id string
		wantProvider, wantID                   string
		wantConflict                           bool
	}{
		{name: "补齐供应商号码", storedProvider: "smsbower", id: "remote-1", wantProvider: "smsbower", wantID: "remote-1"},
		{name: "补齐供应商", storedID: "remote-1", provider: "smsbower", wantProvider: "smsbower", wantID: "remote-1"},
		{name: "沿用完整归属", storedProvider: "smsbower", storedID: "remote-1", wantProvider: "smsbower", wantID: "remote-1"},
		{name: "供应商冲突", storedProvider: "smsbower", provider: "other", wantConflict: true},
		{name: "供应商号码冲突", storedID: "remote-1", id: "remote-2", wantConflict: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, id, err := resolveProviderActivationIdentity(
				tt.storedProvider, tt.storedID, tt.provider, tt.id,
			)
			if (err != nil) != tt.wantConflict {
				t.Fatalf("resolveProviderActivationIdentity() error = %v, wantConflict=%v", err, tt.wantConflict)
			}
			if err == nil && (provider != tt.wantProvider || id != tt.wantID) {
				t.Fatalf("resolveProviderActivationIdentity() = (%q, %q), want (%q, %q)",
					provider, id, tt.wantProvider, tt.wantID)
			}
		})
	}
}

func TestFreezeConflictSQLDoesNotClaimExistingProviderIdentity(t *testing.T) {
	for _, required := range []string{
		"state='conflicted'",
		"cleanup_state=''",
		"cleanup_lease_version=cleanup_lease_version+1",
	} {
		if !strings.Contains(conflictBatchPurchaseAttemptSQL, required) {
			t.Fatalf("purchase conflict SQL missing %q: %s", required, conflictBatchPurchaseAttemptSQL)
		}
	}
	for _, forbidden := range []string{"provider=", "provider_activation_id="} {
		if strings.Contains(conflictBatchPurchaseAttemptSQL, forbidden) {
			t.Fatalf("purchase conflict SQL must not copy an existing ownership key: %s", conflictBatchPurchaseAttemptSQL)
		}
	}
}

func TestPurchaseAccountingSQLKeepsBatchRunningUntilExplicitStop(t *testing.T) {
	if !strings.Contains(recordBatchPurchaseSQL, "status=CASE WHEN status IN ('pending','running') THEN 'running' ELSE status END") {
		t.Fatalf("recording the last purchase must keep the batch running: %s", recordBatchPurchaseSQL)
	}
	if strings.Contains(recordBatchPurchaseSQL, "'completed'") || strings.Contains(recordBatchPurchaseSQL, "finished_at") {
		t.Fatalf("recording a purchase must not complete the batch: %s", recordBatchPurchaseSQL)
	}
	for _, required := range []string{
		"fulfilled_count=fulfilled_count+CASE WHEN $2::boolean THEN 1 ELSE 0 END",
		"inflight_count=GREATEST(inflight_count-1, 0)",
	} {
		if !strings.Contains(releaseBatchSlotSQL, required) {
			t.Fatalf("slot release SQL missing %q: %s", required, releaseBatchSlotSQL)
		}
	}
	if strings.Contains(releaseBatchSlotSQL, "'completed'") || strings.Contains(releaseBatchSlotSQL, "finished_at") {
		t.Fatalf("slot release must not complete a batch before explicit stop: %s", releaseBatchSlotSQL)
	}
}

func TestCancelBatchActivationsSQLInvalidatesLeaseAndQueuesDelete(t *testing.T) {
	for _, required := range []string{
		"control_action='delete'",
		"lease_owner=''",
		"lease_until=NULL",
		"lease_version=lease_version+1",
		"next_run_at=now()",
		"WHERE batch_id=$1 AND finished_at IS NULL",
	} {
		if !strings.Contains(cancelBatchActivationsSQL, required) {
			t.Fatalf("batch cancellation SQL missing %q: %s", required, cancelBatchActivationsSQL)
		}
	}
}

func TestOwnedVerificationWriteFencesStoppedWorkers(t *testing.T) {
	for _, required := range []string{
		"lease_owner=$2",
		"lease_version=$3",
		"finished_at IS NULL",
		"FOR UPDATE",
	} {
		if !strings.Contains(ownedVerificationLeaseSQL, required) {
			t.Fatalf("owned verification lease query missing %q: %s", required, ownedVerificationLeaseSQL)
		}
	}
}

func TestSingleActiveBatchMigrationReconcilesHistoryAndAddsAtomicGuard(t *testing.T) {
	var migrationSQL strings.Builder
	for _, migration := range migrations {
		if migration.version != 4 {
			continue
		}
		for _, statement := range migration.statements {
			migrationSQL.WriteString(statement)
			migrationSQL.WriteByte('\n')
		}
	}
	sql := migrationSQL.String()
	for _, required := range []string{
		"LOCK TABLE batches IN SHARE ROW EXCLUSIVE MODE",
		"row_number() OVER (ORDER BY created_at DESC, id DESC)",
		"active_rank > 1",
		"control_action='delete'",
		"lease_owner=''",
		"lease_version=a.lease_version+1",
		"status='cancelled'",
		"CREATE UNIQUE INDEX IF NOT EXISTS " + singleActiveBatchIndexName,
		"ON batches ((true)) WHERE status IN ('pending','running')",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("single-active migration missing %q: %s", required, sql)
		}
	}
}

func TestMapErrorIdentifiesActiveBatchConflict(t *testing.T) {
	err := mapError(&pgconn.PgError{
		Code:           "23505",
		ConstraintName: singleActiveBatchIndexName,
		Message:        "duplicate key value violates unique constraint",
	})
	if !errors.Is(err, ErrActiveBatchExists) {
		t.Fatalf("mapError() = %v, want ErrActiveBatchExists", err)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("active-batch error must remain compatible with ErrConflict: %v", err)
	}
	if !strings.Contains(err.Error(), "当前已有运行中的任务") {
		t.Fatalf("active-batch error is not explicit: %v", err)
	}
}
