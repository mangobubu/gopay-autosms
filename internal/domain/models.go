package domain

import (
	"encoding/json"
	"time"
)

const MaxBatchQuantity = 100

// Setting is a JSON-backed application setting. Keeping the value as JSON lets
// callers evolve individual settings without requiring a database migration.
type Setting struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type BatchStatus string

const (
	BatchStatusPending   BatchStatus = "pending"
	BatchStatusRunning   BatchStatus = "running"
	BatchStatusCompleted BatchStatus = "completed"
	BatchStatusCancelled BatchStatus = "cancelled"
	BatchStatusFailed    BatchStatus = "failed"
)

type Batch struct {
	ID             int64           `json:"id"`
	Status         BatchStatus     `json:"status"`
	ServiceCode    string          `json:"service_code"`
	ServiceName    string          `json:"service_name"`
	CountryCode    string          `json:"country_code"`
	CountryName    string          `json:"country_name"`
	MaxPriceAmount string          `json:"max_price_amount"`
	Currency       string          `json:"currency"`
	TargetPINEnc   []byte          `json:"-"`
	Config         json.RawMessage `json:"-"`
	ProxyAvailable int             `json:"proxy_available"`
	ProxyTotal     int             `json:"proxy_total"`
	NextPurchaseAt time.Time       `json:"next_purchase_at"`
	Quantity       int             `json:"quantity"`
	// PurchasedCount is an audit counter for provider allocations persisted for
	// this batch. Failed activations are replaced, so it may exceed Quantity.
	PurchasedCount int `json:"purchased_count"`
	// PurchaseReservedCount is a durable pre-provider-call quota reservation.
	// Unknown provider outcomes retain this count so another instance cannot
	// consume the same success slot twice.
	PurchaseReservedCount int        `json:"purchase_reserved_count"`
	FulfilledCount        int        `json:"fulfilled_count"`
	InflightCount         int        `json:"inflight_count"`
	FailureReason         string     `json:"failure_reason,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	StartedAt             *time.Time `json:"started_at,omitempty"`
	FinishedAt            *time.Time `json:"finished_at,omitempty"`
}

type ActivationStatus string

const (
	ActivationStatusPurchased              ActivationStatus = "purchased"
	ActivationStatusDuplicate              ActivationStatus = "duplicate"
	ActivationStatusPhoneInUse             ActivationStatus = "phone_in_use"
	ActivationStatusAwaitingLoginCode      ActivationStatus = "awaiting_login_code"
	ActivationStatusLoggingIn              ActivationStatus = "logging_in"
	ActivationStatusPINRequired            ActivationStatus = "pin_required"
	ActivationStatusUnregistered           ActivationStatus = "unregistered"
	ActivationStatusLoginFailed            ActivationStatus = "login_failed"
	ActivationStatusLoginCodeTimeout       ActivationStatus = "login_code_timeout"
	ActivationStatusCheckingBalance        ActivationStatus = "checking_balance"
	ActivationStatusZeroBalanceUsed        ActivationStatus = "zero_balance_used"
	ActivationStatusSettingPIN             ActivationStatus = "setting_pin"
	ActivationStatusPINSubmissionBlocked   ActivationStatus = "pin_submission_blocked"
	ActivationStatusAwaitingPINCode        ActivationStatus = "awaiting_pin_code"
	ActivationStatusPINCodeTimeout         ActivationStatus = "pin_code_timeout"
	ActivationStatusPINChanged             ActivationStatus = "pin_changed"
	ActivationStatusAwaitingSubsequentCode ActivationStatus = "awaiting_subsequent_code"
	ActivationStatusActive                 ActivationStatus = "active"
	ActivationStatusSuccess                ActivationStatus = "success"
	ActivationStatusExpired                ActivationStatus = "expired"
	ActivationStatusCancelled              ActivationStatus = "cancelled"
	ActivationStatusFailed                 ActivationStatus = "failed"
)

// ControlAction is a durable user action consumed by a worker. Delete is the
// sole cancellation action exposed by the UI; it also hides the activation.
type ControlAction string

const (
	ControlActionNone    ControlAction = ""
	ControlActionSuccess ControlAction = "success"
	ControlActionDelete  ControlAction = "delete"
)

type Activation struct {
	ID                   int64            `json:"id"`
	BatchID              int64            `json:"batch_id"`
	AccountID            *int64           `json:"account_id,omitempty"`
	Provider             string           `json:"provider"`
	ProviderActivationID string           `json:"provider_activation_id"`
	PhoneNumber          string           `json:"phone_number"`
	PhoneFingerprint     string           `json:"phone_fingerprint"`
	ServiceCode          string           `json:"service_code"`
	CountryCode          string           `json:"country_code"`
	Operator             string           `json:"operator,omitempty"`
	PurchasePriceAmount  string           `json:"purchase_price_amount"`
	Currency             string           `json:"currency"`
	Status               ActivationStatus `json:"status"`
	StatusChangedAt      time.Time        `json:"status_changed_at"`
	FailureReason        string           `json:"failure_reason,omitempty"`
	BalanceRP            *float64         `json:"balance_rp,omitempty"`
	BalanceCheckedAt     *time.Time       `json:"balance_checked_at,omitempty"`
	EverFulfilled        bool             `json:"ever_fulfilled"`
	SlotReserved         bool             `json:"slot_reserved"`
	SMSCycle             int              `json:"sms_cycle"`
	NextRunAt            time.Time        `json:"next_run_at"`
	LeaseOwner           string           `json:"lease_owner,omitempty"`
	LeaseUntil           *time.Time       `json:"lease_until,omitempty"`
	LeaseVersion         int64            `json:"lease_version"`
	ControlAction        ControlAction    `json:"control_action,omitempty"`
	ProviderPayload      json.RawMessage  `json:"provider_payload,omitempty"`
	ProviderExpiresAt    *time.Time       `json:"provider_expires_at,omitempty"`
	LastPolledAt         *time.Time       `json:"last_polled_at,omitempty"`
	HiddenAt             *time.Time       `json:"hidden_at,omitempty"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
	FinishedAt           *time.Time       `json:"finished_at,omitempty"`
}

type VerificationPhase string

const (
	VerificationPhaseLogin      VerificationPhase = "login"
	VerificationPhasePIN        VerificationPhase = "pin"
	VerificationPhaseSubsequent VerificationPhase = "subsequent"
)

type VerificationCode struct {
	ID                 int64             `json:"id"`
	ActivationID       int64             `json:"activation_id"`
	CycleNo            int               `json:"cycle_no"`
	Phase              VerificationPhase `json:"phase"`
	Ordinal            int               `json:"ordinal"`
	Code               string            `json:"code"`
	ProviderPayload    json.RawMessage   `json:"provider_payload,omitempty"`
	ProviderReceivedAt *time.Time        `json:"provider_received_at,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
}

// HeroSMSWebhookEvent is an append-only audit record for one HeroSMS callback.
// ActivationID is nullable because HeroSMS may deliver the callback before the
// provider purchase and its local activation have committed.
type HeroSMSWebhookEvent struct {
	ID                   int64                     `json:"id"`
	ActivationID         *int64                    `json:"activation_id,omitempty"`
	ProviderActivationID string                    `json:"provider_activation_id"`
	Code                 *string                   `json:"code,omitempty"`
	Text                 *string                   `json:"text,omitempty"`
	PhoneNumber          string                    `json:"phone_number,omitempty"`
	ServiceCode          string                    `json:"service_code,omitempty"`
	CountryCode          string                    `json:"country_code,omitempty"`
	ProviderReceivedAt   *time.Time                `json:"provider_received_at,omitempty"`
	RawPayload           json.RawMessage           `json:"raw_payload"`
	PayloadFingerprint   string                    `json:"payload_fingerprint"`
	Status               HeroSMSWebhookEventStatus `json:"status"`
	Attempts             int                       `json:"attempts"`
	LastError            string                    `json:"last_error,omitempty"`
	NextAttemptAt        time.Time                 `json:"next_attempt_at"`
	ClaimedLeaseOwner    string                    `json:"claimed_lease_owner,omitempty"`
	ClaimedLeaseVersion  int64                     `json:"claimed_lease_version,omitempty"`
	ReceivedAt           time.Time                 `json:"received_at"`
	ProcessedAt          *time.Time                `json:"processed_at,omitempty"`
}

type HeroSMSWebhookEventStatus string

const (
	HeroSMSWebhookEventReceived   HeroSMSWebhookEventStatus = "received"
	HeroSMSWebhookEventProcessing HeroSMSWebhookEventStatus = "processing"
	HeroSMSWebhookEventProcessed  HeroSMSWebhookEventStatus = "processed"
	HeroSMSWebhookEventIgnored    HeroSMSWebhookEventStatus = "ignored"
)

type AccountStatus string

const (
	AccountStatusAuthenticated AccountStatus = "authenticated"
	AccountStatusPending       AccountStatus = "pending"
	AccountStatusPINPending    AccountStatus = "pin_pending"
	AccountStatusActive        AccountStatus = "active"
	AccountStatusDisabled      AccountStatus = "disabled"
	AccountStatusError         AccountStatus = "error"
)

// Account persists the complete GoPay session as JSON. CredentialsEnc and
// TargetPINEnc are opaque ciphertext produced by the service's encryption
// boundary; the storage package never needs the encryption key.
type Account struct {
	ID               int64           `json:"id"`
	PhoneNumber      string          `json:"phone_number"`
	PhoneFingerprint string          `json:"phone_fingerprint"`
	Status           AccountStatus   `json:"status"`
	BalanceRP        *float64        `json:"balance_rp,omitempty"`
	CredentialsEnc   []byte          `json:"-"`
	TargetPINEnc     []byte          `json:"-"`
	TokenState       json.RawMessage `json:"token_state"`
	DeviceState      json.RawMessage `json:"device_state"`
	Metadata         json.RawMessage `json:"metadata"`
	LastLoginAt      *time.Time      `json:"last_login_at,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}
