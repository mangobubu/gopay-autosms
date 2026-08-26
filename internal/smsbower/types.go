// Package smsbower implements the SMSBower handler_api.php client API.
package smsbower

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the public SMSBower API host. NewClient appends the
	// handler path when Config.BaseURL contains only the host.
	DefaultBaseURL = "https://smsbower.page"
	HandlerPath    = "/stubs/handler_api.php"
)

// ErrPurchaseUnknown marks a getNumber request whose outcome cannot be proven.
// Callers must reconcile the provider account/activation list instead of
// automatically buying another number, which could create a double purchase.
var ErrPurchaseUnknown = errors.New("smsbower: purchase result unknown")

// API is the boundary consumed by the activation/task manager. Keeping this
// interface free of transport details makes task flows straightforward to mock.
type API interface {
	GetServicesList(context.Context) ([]Service, error)
	GetCountries(context.Context) ([]Country, error)
	GetPrices(context.Context, PriceRequest) ([]Price, error)
	GetNumber(context.Context, NumberRequest) (Activation, error)
	GetStatus(context.Context, string) (ActivationStatus, error)
	SetStatus(context.Context, string, SetStatus) (SetStatusResult, error)
}

// HTTPDoer is implemented by http.Client and permits transport-level tests.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config configures a Client. BaseURL may be either a host URL or the complete
// handler_api.php endpoint. HTTPClient defaults to a client with a 30s timeout.
type Config struct {
	APIKey     string
	BaseURL    string
	HTTPClient HTTPDoer
}

// Service is one entry returned by getServicesList.
type Service struct {
	Code string          `json:"code"`
	Name string          `json:"name"`
	Raw  json.RawMessage `json:"-"`
}

// Country is one entry returned by getCountries. Some compatible gateways do
// not return every translated name, so Name contains the best available value.
type Country struct {
	ID           int             `json:"id"`
	Name         string          `json:"name"`
	EnglishName  string          `json:"eng,omitempty"`
	RussianName  string          `json:"rus,omitempty"`
	ChineseName  string          `json:"chn,omitempty"`
	Visible      bool            `json:"visible,omitempty"`
	Retry        bool            `json:"retry,omitempty"`
	Rent         bool            `json:"rent,omitempty"`
	MultiService bool            `json:"multiService,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

// PriceRequest filters the provider price catalogue. Country zero and an empty
// Service request the unfiltered catalogue. Price bounds are strings so values
// selected from V3 can be sent back byte-for-byte without floating-point loss.
type PriceRequest struct {
	Service     string
	Country     int
	MinPrice    string
	MaxPrice    string
	ProviderIDs []int64
}

// Price is a normalized V3/V2/legacy price entry.
type Price struct {
	Country    int             `json:"country"`
	Service    string          `json:"service"`
	ProviderID int64           `json:"providerId,omitempty"`
	Price      string          `json:"price"`
	Count      int             `json:"count"`
	Raw        json.RawMessage `json:"-"`
}

// NumberRequest contains documented getNumberV2/getNumber filters.
type NumberRequest struct {
	Service           string
	Country           int
	MinPrice          string
	MaxPrice          string
	ProviderIDs       []int64
	ExceptProviderIDs []int64
	UserID            string
	PhoneException    []string
	Ref               string
}

// Activation is a normalized successful getNumberV2/getNumber result.
type Activation struct {
	ActivationID     string          `json:"activationId"`
	PhoneNumber      string          `json:"phoneNumber"`
	Cost             string          `json:"activationCost,omitempty"`
	Currency         string          `json:"currency,omitempty"`
	CountryCode      string          `json:"countryCode,omitempty"`
	CanGetAnotherSMS bool            `json:"canGetAnotherSms,omitempty"`
	ActivatedAt      time.Time       `json:"activationTime,omitempty"`
	Operator         string          `json:"activationOperator,omitempty"`
	Raw              json.RawMessage `json:"-"`
}

// StatusKind describes getStatus without imposing a polling policy. The task
// manager owns the two-second interval and termination decisions.
type StatusKind string

const (
	StatusWaitCode   StatusKind = "wait_code"
	StatusWaitRetry  StatusKind = "wait_retry"
	StatusWaitResend StatusKind = "wait_resend"
	StatusOK         StatusKind = "ok"
	StatusCancel     StatusKind = "cancel"
	StatusUnknown    StatusKind = "unknown"
)

// ActivationStatus is one getStatus observation. Code is also preserved for
// STATUS_WAIT_RETRY so callers can label every code according to workflow step.
type ActivationStatus struct {
	Kind StatusKind      `json:"kind"`
	Code string          `json:"code,omitempty"`
	Raw  json.RawMessage `json:"-"`
}

// SetStatus is one of the values accepted by SMSBower's setStatus action.
type SetStatus int

const (
	SetStatusRequestAnother SetStatus = 3
	SetStatusComplete       SetStatus = 6
	SetStatusCancel         SetStatus = 8
)

func (s SetStatus) valid() bool {
	return s == SetStatusRequestAnother || s == SetStatusComplete || s == SetStatusCancel
}

// SetStatusResult preserves the provider acknowledgement.
type SetStatusResult struct {
	Code string          `json:"code"`
	Raw  json.RawMessage `json:"-"`
}

// APIError reports an error returned in either the legacy text response or a
// JSON response. Code is stable enough for errors.Is-style application logic.
type APIError struct {
	Action  string
	Code    string
	Message string
	Raw     string
}

// PurchaseUnknownError wraps an ambiguous purchase outcome. It is returned
// after transport failures and after a successful but unparseable purchase
// response. errors.Is(err, ErrPurchaseUnknown) is the intended test.
type PurchaseUnknownError struct {
	Action string
	Cause  error
}

func (e *PurchaseUnknownError) Error() string {
	if e == nil || e.Cause == nil {
		return ErrPurchaseUnknown.Error()
	}
	return fmt.Sprintf("%s (%s): %v", ErrPurchaseUnknown, e.Action, e.Cause)
}

func (e *PurchaseUnknownError) Unwrap() error { return e.Cause }

func (e *PurchaseUnknownError) Is(target error) bool {
	return target == ErrPurchaseUnknown
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	message := strings.TrimSpace(e.Message)
	if message == "" || strings.EqualFold(message, e.Code) {
		return fmt.Sprintf("smsbower %s: %s", e.Action, e.Code)
	}
	return fmt.Sprintf("smsbower %s: %s: %s", e.Action, e.Code, message)
}

// IsAPIError reports whether err (including a wrapped/joined error) contains
// an SMSBower API error with the requested code.
func IsAPIError(err error, code string) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && strings.EqualFold(apiErr.Code, code) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if IsAPIError(child, code) {
				return true
			}
		}
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsAPIError(wrapped.Unwrap(), code)
	}
	return false
}
