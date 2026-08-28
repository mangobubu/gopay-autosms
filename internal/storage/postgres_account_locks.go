package storage

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mangobubu/gopay-autosms/internal/domain"
)

var _ AccountSessionLockStore = (*PostgresStore)(nil)

const accountSessionLockRetry = 75 * time.Millisecond

// AcquireAccountSessionLock takes a transaction-scoped PostgreSQL advisory
// lock keyed by normalized phone number. A try-lock loop avoids occupying pool
// connections while another instance owns the account's remote session lock.
func (s *PostgresStore) AcquireAccountSessionLock(ctx context.Context, phone string) (func(context.Context) error, error) {
	normalized, err := domain.NormalizePhone(phone)
	if err != nil {
		return nil, ErrInvalidInput
	}
	lockIndex, err := blindIndex(s.protector, accountSessionLockPurpose, []byte(normalized))
	if err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(lockIndex)
	if err != nil || len(digest) < 8 {
		return nil, fmt.Errorf("invalid account session lock index")
	}
	key := int64(binary.BigEndian.Uint64(digest[:8]))

	for {
		conn, err := s.pool.Acquire(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			conn.Release()
			return nil, mapError(err)
		}
		var locked bool
		err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1::bigint)`, key).Scan(&locked)
		if err != nil {
			_ = tx.Rollback(context.Background())
			conn.Release()
			return nil, mapError(err)
		}
		if locked {
			var once sync.Once
			var releaseErr error
			return func(releaseCtx context.Context) error {
				once.Do(func() {
					releaseErr = tx.Rollback(releaseCtx)
					if releaseErr != nil && releaseErr != pgx.ErrTxClosed {
						releaseErr = fmt.Errorf("release account session lock: %w", releaseErr)
					} else {
						releaseErr = nil
					}
					conn.Release()
				})
				return releaseErr
			}, nil
		}

		_ = tx.Rollback(context.Background())
		conn.Release()
		timer := time.NewTimer(accountSessionLockRetry)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		}
	}
}
