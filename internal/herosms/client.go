// Package herosms adapts HeroSMS's SMS-Activate-compatible handler API to the
// normalized SMSBower boundary used by the workflow.
package herosms

import (
	"context"
	"errors"
	"strings"

	"github.com/mangobubu/gopay-autosms/internal/smsbower"
)

const DefaultBaseURL = "https://hero-sms.com/stubs/handler_api.php"

// Config configures the HeroSMS adapter. BaseURL is normally supplied by the
// application configuration so tests can use a local fixture endpoint.
type Config struct {
	APIKey     string
	BaseURL    string
	HTTPClient smsbower.HTTPDoer
}

// Client exposes HeroSMS through the common SMS provider API. HeroSMS supports
// only the legacy getPrices catalogue action, so the embedded client is
// configured not to probe getPricesV3/getPricesV2 first.
type Client struct {
	*smsbower.Client
}

var _ smsbower.API = (*Client)(nil)

func NewClient(cfg Config) (*Client, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client, err := smsbower.NewClient(smsbower.Config{
		APIKey:       cfg.APIKey,
		BaseURL:      baseURL,
		HTTPClient:   cfg.HTTPClient,
		PriceActions: []string{"getPrices"},
	})
	if err != nil {
		return nil, err
	}
	return &Client{Client: client}, nil
}

// GetPrices deliberately removes SMSBower-specific provider metadata.
// HeroSMS's legacy price catalogue exposes one price/stock bucket per
// country-service pair, not SMSBower's provider IDs or Bronze/Silver/Gold
// offer dimension.
func (c *Client) GetPrices(ctx context.Context, req smsbower.PriceRequest) ([]smsbower.Price, error) {
	prices, err := c.Client.GetPrices(ctx, req)
	if err != nil {
		return nil, err
	}
	for index := range prices {
		prices[index].ProviderID = 0
		prices[index].Tier = ""
	}
	return prices, nil
}

// GetNumber removes SMSBower-only filters before calling HeroSMS. HeroSMS's
// compatible getNumberV2 accepts service/country/maxPrice/ref/phoneException,
// but rejects provider IDs, a minimum price, and the reseller userID field.
func (c *Client) GetNumber(ctx context.Context, req smsbower.NumberRequest) (smsbower.Activation, error) {
	req.MinPrice = ""
	req.ProviderIDs = nil
	req.ExceptProviderIDs = nil
	req.UserID = ""
	activation, err := c.Client.GetNumber(ctx, req)
	if err == nil {
		activation.Currency = normalizeCurrency(activation.Currency)
	}
	if errors.Is(err, smsbower.ErrPurchaseUnknown) {
		var apiErr *smsbower.APIError
		if errors.As(err, &apiErr) && conclusivePurchaseHTTP(apiErr.Code) {
			// HeroSMS documents these 4xx responses as request rejections (for
			// example, 404 means no matching numbers), so no allocation occurred.
			return smsbower.Activation{}, apiErr
		}
	}
	return activation, err
}

func normalizeCurrency(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	switch value {
	case "840":
		return "USD"
	case "978":
		return "EUR"
	case "156":
		return "CNY"
	case "643":
		return "RUB"
	default:
		return value
	}
}

func conclusivePurchaseHTTP(code string) bool {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "HTTP_400", "HTTP_401", "HTTP_402", "HTTP_403", "HTTP_404", "HTTP_422":
		return true
	default:
		return false
	}
}

// SetStatus translates HeroSMS's documented missing-activation response into
// the stable compatible code understood by the workflow's idempotent
// finalizer. Other HTTP failures (including the early-cancel HTTP 409) retain
// their original code so the workflow can retry instead of falsely concluding
// that the remote activation was cancelled.
func (c *Client) SetStatus(ctx context.Context, activationID string, status smsbower.SetStatus) (smsbower.SetStatusResult, error) {
	result, err := c.Client.SetStatus(ctx, activationID, status)
	if smsbower.IsAPIError(err, "HTTP_404") {
		return result, &smsbower.APIError{Action: "setStatus", Code: "NO_ACTIVATION"}
	}
	return result, err
}
