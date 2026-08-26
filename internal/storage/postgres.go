package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

type PostgresStore struct {
	pool *pgxpool.Pool
}

// OpenPostgres establishes the pool, verifies connectivity and applies schema
// migrations. A successful return is therefore ready for workers and handlers.
func OpenPostgres(ctx context.Context, cfg PostgresConfig) (*PostgresStore, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("open postgres: %w: empty URL", ErrInvalidInput)
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres URL: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns >= 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	store := &PostgresStore{pool: pool}
	if err = store.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err = Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("ping: nil store")
	}
	return s.pool.Ping(ctx)
}

func (s *PostgresStore) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "40001", "40P01":
			return fmt.Errorf("%w: %s", ErrConflict, pgErr.Message)
		case "23503", "23514", "22P02":
			return fmt.Errorf("%w: %s", ErrInvalidInput, pgErr.Message)
		}
	}
	return err
}

func normalizePage(page Page) Page {
	if page.Limit <= 0 {
		page.Limit = 100
	}
	if page.Limit > 500 {
		page.Limit = 500
	}
	if page.Offset < 0 {
		page.Offset = 0
	}
	return page
}
