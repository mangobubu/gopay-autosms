package domain

import (
	"encoding/json"
	"time"
)

// HeroSMSProductKind identifies the provider product used by an independent
// receive-only number task. Activation products have the provider's default
// lifetime; rent products carry an explicit DurationHours value.
type HeroSMSProductKind string

const (
	HeroSMSProductActivation HeroSMSProductKind = "activation"
	HeroSMSProductRent       HeroSMSProductKind = "rent"
)

func (kind HeroSMSProductKind) Valid() bool {
	return kind == HeroSMSProductActivation || kind == HeroSMSProductRent
}

// HeroSMSNumberTaskStatus is deliberately independent from BatchStatus and
// ActivationStatus. Each row represents exactly one requested number slot.
type HeroSMSNumberTaskStatus string

const (
	HeroSMSTaskWaitingNumber   HeroSMSNumberTaskStatus = "waiting_number"
	HeroSMSTaskPurchasing      HeroSMSNumberTaskStatus = "purchasing"
	HeroSMSTaskActive          HeroSMSNumberTaskStatus = "active"
	HeroSMSTaskPurchaseUnknown HeroSMSNumberTaskStatus = "purchase_unknown"
	HeroSMSTaskSettling        HeroSMSNumberTaskStatus = "settling"
	HeroSMSTaskStopped         HeroSMSNumberTaskStatus = "stopped"
	HeroSMSTaskRefunded        HeroSMSNumberTaskStatus = "refunded"
	HeroSMSTaskSettled         HeroSMSNumberTaskStatus = "settled"
	HeroSMSTaskExpired         HeroSMSNumberTaskStatus = "expired"
)

func (status HeroSMSNumberTaskStatus) Valid() bool {
	switch status {
	case HeroSMSTaskWaitingNumber, HeroSMSTaskPurchasing, HeroSMSTaskActive, HeroSMSTaskPurchaseUnknown,
		HeroSMSTaskSettling, HeroSMSTaskStopped, HeroSMSTaskRefunded,
		HeroSMSTaskSettled, HeroSMSTaskExpired:
		return true
	default:
		return false
	}
}

func (status HeroSMSNumberTaskStatus) Terminal() bool {
	switch status {
	case HeroSMSTaskStopped, HeroSMSTaskRefunded, HeroSMSTaskSettled, HeroSMSTaskExpired:
		return true
	default:
		return false
	}
}

type HeroSMSRefundStatus string

const (
	HeroSMSRefundUnknown     HeroSMSRefundStatus = ""
	HeroSMSRefundRefundable  HeroSMSRefundStatus = "refundable"
	HeroSMSRefundRequested   HeroSMSRefundStatus = "requested"
	HeroSMSRefunded          HeroSMSRefundStatus = "refunded"
	HeroSMSRefundUnavailable HeroSMSRefundStatus = "unavailable"
	HeroSMSRefundSettled     HeroSMSRefundStatus = "settled"
)

func (status HeroSMSRefundStatus) Valid() bool {
	switch status {
	case HeroSMSRefundUnknown, HeroSMSRefundRefundable, HeroSMSRefundRequested,
		HeroSMSRefunded, HeroSMSRefundUnavailable, HeroSMSRefundSettled:
		return true
	default:
		return false
	}
}

type HeroSMSTaskControlAction string

const (
	HeroSMSTaskControlNone HeroSMSTaskControlAction = ""
	HeroSMSTaskControlStop HeroSMSTaskControlAction = "stop"
)

func (action HeroSMSTaskControlAction) Valid() bool {
	return action == HeroSMSTaskControlNone || action == HeroSMSTaskControlStop
}

type HeroSMSMessageSource string

const (
	HeroSMSMessageWebhook HeroSMSMessageSource = "webhook"
	HeroSMSMessagePoll    HeroSMSMessageSource = "poll"
)

func (source HeroSMSMessageSource) Valid() bool {
	return source == HeroSMSMessageWebhook || source == HeroSMSMessagePoll
}

// HeroSMSNumberTask is a single independently scheduled number. SubmissionID
// only records which create request produced the row; it has no scheduling or
// lifecycle semantics.
type HeroSMSNumberTask struct {
	ID                       int64                   `json:"id"`
	SubmissionID             string                  `json:"-"`
	Status                   HeroSMSNumberTaskStatus `json:"status"`
	ProductKind              HeroSMSProductKind      `json:"product_kind"`
	ServiceCode              string                  `json:"service_code"`
	ServiceName              string                  `json:"service_name"`
	CountryCode              string                  `json:"country_code"`
	CountryName              string                  `json:"country_name"`
	VerificationType         string                  `json:"verification_type"`
	DurationHours            *int                    `json:"duration_hours,omitempty"`
	MaxPriceAmount           string                  `json:"-"`
	Provider                 string                  `json:"-"`
	PurchaseToken            string                  `json:"-"`
	ProviderActivationID     string                  `json:"provider_activation_id,omitempty"`
	PhoneNumber              string                  `json:"phone_number,omitempty"`
	Operator                 string                  `json:"operator,omitempty"`
	ActivationCost           string                  `json:"activation_cost,omitempty"`
	Currency                 string                  `json:"currency"`
	PurchasedAt              *time.Time              `json:"purchased_at,omitempty"`
	ExpiresAt                *time.Time              `json:"expires_at,omitempty"`
	RefundableUntil          *time.Time              `json:"refundable_until,omitempty"`
	RefundStatus             HeroSMSRefundStatus     `json:"refund_status,omitempty"`
	MessageCount             int                     `json:"message_count"`
	ContinuationCount        int                     `json:"-"`
	ContinuationPendingCount int                     `json:"-"`
	SupportsContinuation     bool                    `json:"supports_continuation"`
	FirstMessageAt           *time.Time              `json:"first_message_at,omitempty"`
	NextRunAt                time.Time               `json:"next_run_at"`
	LeaseOwner               string                  `json:"-"`
	LeaseUntil               *time.Time              `json:"-"`
	LeaseVersion             int64                   `json:"-"`
	StopRequested            bool                    `json:"-"`
	RetryCount               int                     `json:"retry_count,omitempty"`
	LastError                string                  `json:"last_error,omitempty"`
	LastPolledAt             *time.Time              `json:"last_polled_at,omitempty"`
	WebhookWakeupAt          *time.Time              `json:"-"`
	ProviderPayload          json.RawMessage         `json:"-"`
	CreatedAt                time.Time               `json:"created_at"`
	UpdatedAt                time.Time               `json:"updated_at"`
	FinishedAt               *time.Time              `json:"finished_at,omitempty"`
	Messages                 []HeroSMSNumberMessage  `json:"messages"`
}

// Short aliases keep manager and API code readable while retaining the more
// explicit public JSON model name.
type HeroSMSTask = HeroSMSNumberTask
type HeroSMSTaskStatus = HeroSMSNumberTaskStatus

// HeroSMSNumberMessage is append-only. TaskID is nullable while a provider
// callback races the local purchase commit; the commit attaches matching
// orphan rows without rewriting their audit payload.
type HeroSMSNumberMessage struct {
	ID                   int64                `json:"id"`
	TaskID               *int64               `json:"task_id,omitempty"`
	ProviderActivationID string               `json:"provider_activation_id"`
	ProviderMessageID    string               `json:"provider_message_id,omitempty"`
	Source               HeroSMSMessageSource `json:"source"`
	Code                 string               `json:"code,omitempty"`
	Text                 string               `json:"text,omitempty"`
	ProviderReceivedAt   *time.Time           `json:"provider_received_at,omitempty"`
	RawPayload           json.RawMessage      `json:"-"`
	PayloadFingerprint   string               `json:"-"`
	CreatedAt            time.Time            `json:"created_at"`
}
