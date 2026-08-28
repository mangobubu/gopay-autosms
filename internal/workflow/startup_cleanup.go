package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

const startupBatchPageSize = 500

// recoverStartupBatches runs synchronously before the scheduler starts. It
// preserves active batches so their durable activations can resume after a
// restart, while fencing any provider purchase that the stopped process left
// in an uncertain sent state.
func (m *Manager) recoverStartupBatches(ctx context.Context) error {
	batches := make([]domain.Batch, 0)
	for offset := 0; ; {
		page, err := m.store.ListBatches(ctx, storage.BatchFilter{
			Statuses: []domain.BatchStatus{domain.BatchStatusPending, domain.BatchStatusRunning},
			Page:     storage.Page{Limit: startupBatchPageSize, Offset: offset},
		})
		if err != nil {
			return fmt.Errorf("list unfinished batches during startup: %w", err)
		}
		batches = append(batches, page...)
		if len(page) < startupBatchPageSize {
			break
		}
		offset += len(page)
	}

	seen := make(map[int64]struct{}, len(batches))
	var recoveryErrors []error
	for _, batch := range batches {
		if batch.Status != domain.BatchStatusPending && batch.Status != domain.BatchStatusRunning {
			continue
		}
		if _, ok := seen[batch.ID]; ok {
			continue
		}
		seen[batch.ID] = struct{}{}
		if err := m.recoverStartupPurchase(ctx, batch.ID); err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("recover unfinished purchase for batch %d during startup: %w", batch.ID, err))
		}
	}
	return errors.Join(recoveryErrors...)
}

func (m *Manager) recoverStartupPurchase(ctx context.Context, batchID int64) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = m.store.RecoverBatchPurchaseOnStartup(ctx, batchID)
		if err == nil || (!errors.Is(err, storage.ErrCommitUnknown) && !errors.Is(err, storage.ErrRetryable)) {
			return err
		}
	}
	return err
}
