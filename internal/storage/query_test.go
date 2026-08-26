package storage

import (
	"strings"
	"testing"

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
		"config jsonb",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("schema missing %q", required)
		}
	}
}
