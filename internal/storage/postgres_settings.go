package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mangobubu/gopay-autosms/internal/domain"
)

func (s *PostgresStore) GetSetting(ctx context.Context, key string) (domain.Setting, error) {
	var setting domain.Setting
	var value []byte
	err := s.pool.QueryRow(ctx, `SELECT key, value, updated_at FROM settings WHERE key=$1`, key).
		Scan(&setting.Key, &value, &setting.UpdatedAt)
	if err != nil {
		return domain.Setting{}, mapError(err)
	}
	setting.Value = cloneJSON(value)
	return setting, nil
}

func (s *PostgresStore) SetSetting(ctx context.Context, key string, value json.RawMessage) (domain.Setting, error) {
	key = strings.TrimSpace(key)
	if key == "" || !json.Valid(value) {
		return domain.Setting{}, ErrInvalidInput
	}
	var setting domain.Setting
	var stored []byte
	err := s.pool.QueryRow(ctx, `INSERT INTO settings(key, value)
		VALUES($1, $2::jsonb)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=now()
		RETURNING key, value, updated_at`, key, value).
		Scan(&setting.Key, &stored, &setting.UpdatedAt)
	if err != nil {
		return domain.Setting{}, mapError(err)
	}
	setting.Value = cloneJSON(stored)
	return setting, nil
}

func (s *PostgresStore) ListSettings(ctx context.Context) ([]domain.Setting, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value, updated_at FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Setting, 0)
	for rows.Next() {
		var setting domain.Setting
		var value []byte
		if err = rows.Scan(&setting.Key, &value, &setting.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		setting.Value = cloneJSON(value)
		result = append(result, setting)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate settings: %w", err)
	}
	return result, nil
}

func cloneJSON(value []byte) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(`{}`)
	}
	cloned := make([]byte, len(value))
	copy(cloned, value)
	return json.RawMessage(cloned)
}

func validJSONOrObject(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(`{}`)
	}
	return value
}
