package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/mangobubu/gopay-autosms/internal/domain"
)

const accountColumns = `id, phone_number, phone_fingerprint, status, balance_rp,
	credentials_enc, target_pin_enc, token_state, device_state, metadata,
	last_login_at, created_at, updated_at, sensitive_enc`

func (s *PostgresStore) scanAccount(row rowScanner) (domain.Account, error) {
	var account domain.Account
	var status string
	var balance sql.NullFloat64
	var lastLoginAt sql.NullTime
	var legacyPhone string
	var tokenState, deviceState, metadata, sensitive []byte
	err := row.Scan(
		&account.ID, &legacyPhone, &account.PhoneFingerprint, &status, &balance,
		&account.CredentialsEnc, &account.TargetPINEnc, &tokenState, &deviceState, &metadata,
		&lastLoginAt, &account.CreatedAt, &account.UpdatedAt, &sensitive,
	)
	if err != nil {
		return domain.Account{}, err
	}
	account.Status = domain.AccountStatus(status)
	account.PhoneNumber = legacyPhone
	if len(sensitive) > 0 {
		envelope, openErr := openAccountSensitive(s.protector, sensitive, account.PhoneFingerprint)
		if openErr != nil {
			return domain.Account{}, fmt.Errorf("decrypt account %d: %w", account.ID, openErr)
		}
		account.PhoneNumber = envelope.PhoneNumber
	}
	if balance.Valid {
		account.BalanceRP = &balance.Float64
	}
	if lastLoginAt.Valid {
		account.LastLoginAt = &lastLoginAt.Time
	}
	account.TokenState = cloneJSON(tokenState)
	account.DeviceState = cloneJSON(deviceState)
	account.Metadata = cloneJSON(metadata)
	return account, nil
}

func (s *PostgresStore) UpsertAccount(ctx context.Context, params UpsertAccountParams) (domain.Account, error) {
	normalized, err := domain.NormalizePhone(params.PhoneNumber)
	if err != nil || !params.Status.Valid() {
		return domain.Account{}, ErrInvalidInput
	}
	tokenState := validJSONOrObject(params.TokenState)
	deviceState := validJSONOrObject(params.DeviceState)
	metadata := validJSONOrObject(params.Metadata)
	if !json.Valid(tokenState) || !json.Valid(deviceState) || !json.Valid(metadata) {
		return domain.Account{}, fmt.Errorf("%w: malformed account JSON", ErrInvalidInput)
	}
	fingerprint, err := accountPhoneBlindIndex(s.protector, normalized)
	if err != nil {
		return domain.Account{}, err
	}
	sensitive, err := sealAccountSensitive(s.protector, fingerprint, normalized)
	if err != nil {
		return domain.Account{}, err
	}
	query := `INSERT INTO accounts(
		phone_number, phone_fingerprint, status, balance_rp, credentials_enc,
		target_pin_enc, token_state, device_state, metadata, last_login_at, sensitive_enc
	) VALUES('',$1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8::jsonb,$9,$10)
	ON CONFLICT(phone_fingerprint) DO UPDATE SET
		phone_number='',
		status=excluded.status,
		balance_rp=excluded.balance_rp,
		credentials_enc=excluded.credentials_enc,
		target_pin_enc=excluded.target_pin_enc,
		token_state=excluded.token_state,
		device_state=excluded.device_state,
		metadata=excluded.metadata,
		last_login_at=excluded.last_login_at,
		sensitive_enc=excluded.sensitive_enc,
		updated_at=now()
	RETURNING ` + accountColumns
	account, err := s.scanAccount(s.pool.QueryRow(ctx, query,
		fingerprint, string(params.Status), params.BalanceRP,
		params.CredentialsEnc, params.TargetPINEnc, tokenState, deviceState,
		metadata, params.LastLoginAt, sensitive,
	))
	if err != nil {
		return domain.Account{}, mapError(err)
	}
	return account, nil
}

func (s *PostgresStore) GetAccount(ctx context.Context, id int64) (domain.Account, error) {
	account, err := s.scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id=$1`, id))
	if err != nil {
		return domain.Account{}, mapError(err)
	}
	return account, nil
}

func (s *PostgresStore) GetAccountByPhone(ctx context.Context, phone string) (domain.Account, error) {
	normalized, err := domain.NormalizePhone(phone)
	if err != nil {
		return domain.Account{}, ErrInvalidInput
	}
	fingerprint, err := accountPhoneBlindIndex(s.protector, normalized)
	if err != nil {
		return domain.Account{}, err
	}
	account, err := s.scanAccount(s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE phone_fingerprint=$1`, fingerprint))
	if err != nil {
		return domain.Account{}, mapError(err)
	}
	return account, nil
}

func (s *PostgresStore) ListAccounts(ctx context.Context, filter AccountFilter) ([]domain.Account, error) {
	query, args := buildAccountListQuery(filter)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	accounts := make([]domain.Account, 0)
	for rows.Next() {
		account, scanErr := s.scanAccount(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan account: %w", scanErr)
		}
		accounts = append(accounts, account)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	return accounts, nil
}

func buildAccountListQuery(filter AccountFilter) (string, []any) {
	page := normalizePage(filter.Page)
	query := `SELECT ` + accountColumns + ` FROM accounts`
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
	query += ` ORDER BY updated_at DESC, id DESC LIMIT $` + strconv.Itoa(limitPos) + ` OFFSET $` + strconv.Itoa(limitPos+1)
	return query, args
}

func (s *PostgresStore) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus) error {
	if !status.Valid() {
		return ErrInvalidInput
	}
	result, err := s.pool.Exec(ctx, `UPDATE accounts SET status=$2, updated_at=now() WHERE id=$1`, id, string(status))
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

var _ AccountCredentialCASStore = (*PostgresStore)(nil)

// UpdateAccountCredentialsIfUnchanged persists a rotated GoPay session without
// replacing the account's business status, balance, PIN ciphertext, or
// metadata. The old ciphertext is an optimistic version: a worker that saved a
// newer session wins instead of being overwritten by a stale status probe.
func (s *PostgresStore) UpdateAccountCredentialsIfUnchanged(
	ctx context.Context,
	id int64,
	expectedCredentialsEnc []byte,
	nextCredentialsEnc []byte,
	deviceState json.RawMessage,
) error {
	if id <= 0 || len(expectedCredentialsEnc) == 0 || len(nextCredentialsEnc) == 0 {
		return ErrInvalidInput
	}
	device := validJSONOrObject(deviceState)
	if !json.Valid(device) {
		return fmt.Errorf("%w: malformed device JSON", ErrInvalidInput)
	}
	result, err := s.pool.Exec(ctx, `
		UPDATE accounts
		SET credentials_enc=$3, device_state=$4::jsonb, updated_at=now()
		WHERE id=$1 AND credentials_enc=$2`,
		id, expectedCredentialsEnc, nextCredentialsEnc, device,
	)
	if err != nil {
		return mapError(err)
	}
	if result.RowsAffected() == 0 {
		var exists bool
		if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, id).Scan(&exists); err != nil {
			return mapError(err)
		}
		if !exists {
			return ErrNotFound
		}
		return ErrConflict
	}
	return nil
}
