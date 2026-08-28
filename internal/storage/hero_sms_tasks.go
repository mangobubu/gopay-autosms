package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
)

const MaxHeroSMSTaskQuantity = 100

type CreateHeroSMSTasksParams struct {
	SubmissionID     string
	ProductKind      domain.HeroSMSProductKind
	ServiceCode      string
	ServiceName      string
	CountryCode      string
	CountryName      string
	VerificationType string
	DurationHours    *int
	MaxPriceAmount   string
	Currency         string
	ProviderPayload  json.RawMessage
	NextRunAt        time.Time
	Quantity         int
}

type HeroSMSTaskFilter struct {
	Statuses []domain.HeroSMSNumberTaskStatus
	Page     Page
}

type CommitHeroSMSPurchaseParams struct {
	PurchaseToken        string
	ProviderActivationID string
	PhoneNumber          string
	Operator             string
	ActivationCost       string
	Currency             string
	PurchasedAt          time.Time
	ExpiresAt            *time.Time
	RefundableUntil      *time.Time
	RefundStatus         domain.HeroSMSRefundStatus
	SupportsContinuation bool
	ProviderPayload      json.RawMessage
	NextRunAt            time.Time
}

type MarkHeroSMSPurchaseUnknownParams struct {
	PurchaseToken        string
	ProviderActivationID string
	ProviderPayload      json.RawMessage
	NextRunAt            time.Time
	LastError            string
}

type AppendHeroSMSTaskMessageParams struct {
	TaskID               *int64
	ProviderActivationID string
	ProviderMessageID    string
	Source               domain.HeroSMSMessageSource
	Code                 string
	Text                 string
	ProviderReceivedAt   *time.Time
	RawPayload           json.RawMessage
}

type AppendHeroSMSTaskMessageResult struct {
	Message  domain.HeroSMSNumberMessage
	Task     *domain.HeroSMSNumberTask
	Inserted bool
}

// HeroSMSTaskStore is independent from Store because the receive-only page has
// no batch, GoPay account, or legacy activation lifecycle. PostgresStore
// implements both interfaces, while existing lightweight mocks stay unchanged.
type HeroSMSTaskStore interface {
	CreateHeroSMSTasks(context.Context, CreateHeroSMSTasksParams) ([]domain.HeroSMSNumberTask, error)
	GetHeroSMSTask(context.Context, int64) (domain.HeroSMSNumberTask, error)
	ListHeroSMSTasks(context.Context, HeroSMSTaskFilter) ([]domain.HeroSMSNumberTask, error)
	ClaimDueHeroSMSTasks(context.Context, string, time.Time, time.Duration, int) ([]domain.HeroSMSNumberTask, error)
	BeginHeroSMSPurchaseOwned(context.Context, int64, string, int64, string) (domain.HeroSMSNumberTask, error)
	ReleaseHeroSMSPurchaseOwned(context.Context, int64, string, int64, string, time.Time, string) (domain.HeroSMSNumberTask, error)
	ScheduleHeroSMSTaskOwned(context.Context, int64, string, int64, domain.HeroSMSNumberTaskStatus, time.Time, string) (domain.HeroSMSNumberTask, error)
	CommitHeroSMSPurchaseOwned(context.Context, int64, string, int64, CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error)
	RecoverHeroSMSPurchaseOutcome(context.Context, int64, string, CommitHeroSMSPurchaseParams) (domain.HeroSMSNumberTask, error)
	MarkHeroSMSPurchaseUnknownOwned(context.Context, int64, string, int64, MarkHeroSMSPurchaseUnknownParams) (domain.HeroSMSNumberTask, error)
	RequestHeroSMSTaskStop(context.Context, int64) (domain.HeroSMSNumberTask, error)
	RestartHeroSMSTask(context.Context, int64, time.Time) (domain.HeroSMSNumberTask, error)
	PrepareHeroSMSTaskSettlementOwned(context.Context, int64, string, int64, time.Time) (domain.HeroSMSNumberTask, error)
	BeginHeroSMSContinuationOwned(context.Context, int64, string, int64, time.Time) (domain.HeroSMSNumberTask, error)
	CompleteHeroSMSContinuationOwned(context.Context, int64, string, int64, int, time.Time) (domain.HeroSMSNumberTask, error)
	AbortHeroSMSContinuationOwned(context.Context, int64, string, int64, int, time.Time, string) (domain.HeroSMSNumberTask, error)
	FinishHeroSMSTaskOwned(context.Context, int64, string, int64, domain.HeroSMSNumberTaskStatus, domain.HeroSMSRefundStatus, string) (domain.HeroSMSNumberTask, error)
	AppendHeroSMSTaskMessage(context.Context, AppendHeroSMSTaskMessageParams) (AppendHeroSMSTaskMessageResult, error)
	ListHeroSMSTaskMessages(context.Context, int64) ([]domain.HeroSMSNumberMessage, error)
}
