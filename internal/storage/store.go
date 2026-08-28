package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
)

var (
	ErrNotFound           = errors.New("storage: not found")
	ErrConflict           = errors.New("storage: state conflict")
	ErrActiveBatchExists  = fmt.Errorf("当前已有运行中的任务，请先停止当前任务: %w", ErrConflict)
	ErrRetryable          = errors.New("storage: retryable transaction failure")
	ErrBatchCapacity      = errors.New("storage: batch quantity already reserved")
	ErrPurchaseInProgress = fmt.Errorf("购号请求正在处理中，请稍后再次停止任务: %w", ErrConflict)
	ErrInvalidInput       = errors.New("storage: invalid input")
	ErrCommitUnknown      = errors.New("storage: transaction commit outcome unknown")
)

type Page struct {
	Limit  int
	Offset int
}

type BatchFilter struct {
	Statuses []domain.BatchStatus
	Page     Page
}

type ActivationFilter struct {
	BatchID       *int64
	Statuses      []domain.ActivationStatus
	IncludeHidden bool
	PhoneContains string
	Page          Page
}

type AccountFilter struct {
	Statuses []domain.AccountStatus
	Page     Page
}

type CreateBatchParams struct {
	ServiceCode    string
	ServiceName    string
	CountryCode    string
	CountryName    string
	MaxPriceAmount string
	Currency       string
	TargetPINEnc   []byte
	// Config persists provider IDs, proxy selection and future batch options.
	Config   json.RawMessage
	Quantity int
}

type CreateActivationParams struct {
	PurchaseToken        string
	BatchID              int64
	Provider             string
	ProviderActivationID string
	PhoneNumber          string
	ServiceCode          string
	CountryCode          string
	Operator             string
	PurchasePriceAmount  string
	Currency             string
	ProviderPayload      json.RawMessage
	ProviderExpiresAt    *time.Time
	NextRunAt            time.Time
}

type CreateActivationResult struct {
	Activation domain.Activation
	// Duplicate is retained for lightweight store/mock source compatibility.
	// New activations are no longer classified by historical phone usage, so
	// production storage always leaves it false.
	Duplicate bool
}

type PurchaseAttemptState string

const (
	PurchaseAttemptReserved   PurchaseAttemptState = "reserved"
	PurchaseAttemptSent       PurchaseAttemptState = "sent"
	PurchaseAttemptCommitted  PurchaseAttemptState = "committed"
	PurchaseAttemptReleased   PurchaseAttemptState = "released"
	PurchaseAttemptUnknown    PurchaseAttemptState = "unknown"
	PurchaseAttemptConflicted PurchaseAttemptState = "conflicted"
)

type PurchaseCleanupAttempt struct {
	Token                string
	BatchID              int64
	Provider             string
	ProviderActivationID string
	LeaseOwner           string
	LeaseVersion         int64
}

type AppendVerificationParams struct {
	ActivationID       int64
	CycleNo            int
	Phase              domain.VerificationPhase
	Code               string
	ProviderPayload    json.RawMessage
	ProviderReceivedAt *time.Time
}

type AppendVerificationResult struct {
	Verification domain.VerificationCode
	Inserted     bool
}

type UpsertAccountParams struct {
	PhoneNumber    string
	Status         domain.AccountStatus
	BalanceRP      *float64
	CredentialsEnc []byte
	TargetPINEnc   []byte
	TokenState     json.RawMessage
	DeviceState    json.RawMessage
	Metadata       json.RawMessage
	LastLoginAt    *time.Time
}

// Store is split into small interfaces so job-manager and HTTP tests can mock
// only the capabilities they exercise.
type Store interface {
	SettingsStore
	BatchStore
	ActivationStore
	VerificationStore
	AccountStore
	Ping(context.Context) error
	Close()
}

type SettingsStore interface {
	GetSetting(context.Context, string) (domain.Setting, error)
	SetSetting(context.Context, string, json.RawMessage) (domain.Setting, error)
	ListSettings(context.Context) ([]domain.Setting, error)
}

type BatchStore interface {
	CreateBatch(context.Context, CreateBatchParams) (domain.Batch, error)
	GetBatch(context.Context, int64) (domain.Batch, error)
	ListBatches(context.Context, BatchFilter) ([]domain.Batch, error)
	// ReserveBatchPurchase persists the quota token before contacting the
	// provider. Only one unresolved remote purchase may exist per batch.
	ReserveBatchPurchase(context.Context, int64, string) error
	// MarkBatchPurchaseSent is the distributed cancellation fence immediately
	// before the provider request. A stopped batch cannot enter this state.
	MarkBatchPurchaseSent(context.Context, int64, string) error
	// ReleaseBatchPurchaseReservation is used only when the provider proves no
	// number was allocated. Repeating the same token is idempotent.
	ReleaseBatchPurchaseReservation(context.Context, int64, string, time.Time, string) error
	// FreezeBatchPurchase retains the quota after an ambiguous outcome and
	// stores any known provider identity for later reconciliation. Its returned
	// state is read under the same locks that resolve a concurrent commit.
	FreezeBatchPurchase(context.Context, int64, string, string, string, string) (PurchaseAttemptState, error)
	// RecoverBatchPurchaseOnStartup converts a request left in sent state by a
	// stopped process into an unknown retained slot before startup cancellation.
	RecoverBatchPurchaseOnStartup(context.Context, int64) error
	ClaimPurchaseCleanupAttempts(context.Context, string, time.Time, time.Duration, int) ([]PurchaseCleanupAttempt, error)
	CompletePurchaseCleanup(context.Context, string, string, int64) error
	RetryPurchaseCleanup(context.Context, string, string, int64, time.Time, string) error
	TransitionBatch(context.Context, int64, []domain.BatchStatus, domain.BatchStatus, string) (domain.Batch, error)
	CancelBatch(context.Context, int64) (domain.Batch, error)
	ScheduleBatchPurchase(context.Context, int64, time.Time, string) error
	UpdateBatchConfig(context.Context, int64, json.RawMessage) error
}

// ProxyExhaustionStore is an optional capability used by the scheduler to
// atomically stop a one-shot proxy batch once no work remains in flight. It is
// separate from BatchStore so lightweight Store test doubles and integrations
// do not need to implement this optimization.
type ProxyExhaustionStore interface {
	FailBatchForExhaustedProxies(context.Context, int64, string) (domain.Batch, error)
}

type ActivationStore interface {
	// CreateActivationAtomically inserts the activation and consumes its durable
	// provider-purchase reservation in the same transaction.
	CreateActivationAtomically(context.Context, CreateActivationParams) (CreateActivationResult, error)
	GetActivation(context.Context, int64) (domain.Activation, error)
	GetActivationByProviderID(context.Context, string, string) (domain.Activation, error)
	ListActivations(context.Context, ActivationFilter) ([]domain.Activation, error)
	ListRecoverableActivations(context.Context, int) ([]domain.Activation, error)
	ClaimRunnableActivations(context.Context, string, time.Time, time.Duration, int) ([]domain.Activation, error)
	ReleaseActivationLease(context.Context, int64, string, time.Time) error
	TransitionActivation(context.Context, int64, []domain.ActivationStatus, domain.ActivationStatus, string) (domain.Activation, error)
	TransitionActivationOwned(context.Context, int64, []domain.ActivationStatus, domain.ActivationStatus, string, string, int64) (domain.Activation, error)
	FinalizeActivation(context.Context, int64, []domain.ActivationStatus) (domain.Activation, error)
	FinalizeActivationOwned(context.Context, int64, []domain.ActivationStatus, string, int64) (domain.Activation, error)
	SetActivationBalance(context.Context, int64, *float64, *time.Time) error
	SetActivationBalanceOwned(context.Context, int64, *float64, *time.Time, string, int64) error
	AttachActivationAccount(context.Context, int64, int64) error
	AttachActivationAccountOwned(context.Context, int64, int64, string, int64) error
	MarkActivationFulfilled(context.Context, int64) (bool, error)
	MarkActivationFulfilledOwned(context.Context, int64, string, int64) (bool, error)
	AdvanceSMSCycle(context.Context, int64, string, time.Time) (int, error)
	TouchActivationPoll(context.Context, int64, string, time.Time, time.Time) error
	SetControlAction(context.Context, int64, domain.ControlAction) error
	ClearControlAction(context.Context, int64, domain.ControlAction) error
	SoftDeleteActivation(context.Context, int64) error
	HideActivation(context.Context, int64) error
}

type VerificationStore interface {
	AppendVerificationCode(context.Context, AppendVerificationParams) (AppendVerificationResult, error)
	ListVerificationCodes(context.Context, int64) ([]domain.VerificationCode, error)
}

// OwnedVerificationStore is an optional capability used by workers. It keeps
// the lease owner/version check in the same transaction as the append while
// preserving source compatibility for API and lightweight non-worker stores.
type OwnedVerificationStore interface {
	AppendVerificationCodeOwned(context.Context, AppendVerificationParams, string, int64) (AppendVerificationResult, error)
}

type AccountStore interface {
	UpsertAccount(context.Context, UpsertAccountParams) (domain.Account, error)
	GetAccount(context.Context, int64) (domain.Account, error)
	GetAccountByPhone(context.Context, string) (domain.Account, error)
	ListAccounts(context.Context, AccountFilter) ([]domain.Account, error)
	UpdateAccountStatus(context.Context, int64, domain.AccountStatus) error
}

// AccountCredentialCASStore is an optional capability used when an operation
// rotates GoPay credentials outside the main account workflow. It deliberately
// is not embedded in Store so lightweight stores remain source compatible.
// Implementations must update only when the encrypted session still matches
// expectedCredentialsEnc, returning ErrConflict when another writer won.
type AccountCredentialCASStore interface {
	UpdateAccountCredentialsIfUnchanged(
		context.Context,
		int64,
		[]byte,
		[]byte,
		json.RawMessage,
	) error
}

// AccountSessionLockStore serializes remote session mutation and its durable
// write across service instances. The returned release function must always be
// called; implementations should make it idempotent.
type AccountSessionLockStore interface {
	AcquireAccountSessionLock(context.Context, string) (func(context.Context) error, error)
}
