package smsbower

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

// Client calls the SMSBower handler endpoint.
type Client struct {
	apiKey   string
	endpoint string
	http     HTTPDoer
}

var _ API = (*Client)(nil)

// NewClient constructs a client. The API key is checked when an API method is
// called, allowing the surrounding service to start before UI configuration is
// complete.
func NewClient(cfg Config) (*Client, error) {
	endpoint, err := normalizeEndpoint(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	doer := cfg.HTTPClient
	if doer == nil {
		doer = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		apiKey:   strings.TrimSpace(cfg.APIKey),
		endpoint: endpoint,
		http:     doer,
	}, nil
}

func normalizeEndpoint(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = DefaultBaseURL
	}
	if !strings.HasSuffix(strings.ToLower(strings.TrimRight(base, "/")), "handler_api.php") {
		base = strings.TrimRight(base, "/") + HandlerPath
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("smsbower: invalid base URL %q", base)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("smsbower: unsupported URL scheme %q", u.Scheme)
	}
	return u.String(), nil
}

// GetServicesList returns the provider's current service catalogue.
func (c *Client) GetServicesList(ctx context.Context) ([]Service, error) {
	body, err := c.call(ctx, "getServicesList", nil)
	if err != nil {
		return nil, err
	}
	return parseServices("getServicesList", body)
}

// GetCountries returns the provider's current country catalogue.
func (c *Client) GetCountries(ctx context.Context) ([]Country, error) {
	body, err := c.call(ctx, "getCountries", nil)
	if err != nil {
		return nil, err
	}
	return parseCountries("getCountries", body)
}

// GetPrices prefers V3 and falls back through V2 to the legacy catalogue.
func (c *Client) GetPrices(ctx context.Context, req PriceRequest) ([]Price, error) {
	params := priceParams(req)
	actions := [...]string{"getPricesV3", "getPricesV2", "getPrices"}
	errs := make([]error, 0, len(actions))
	for _, action := range actions {
		body, err := c.call(ctx, action, params)
		if err == nil {
			var prices []Price
			prices, err = parsePrices(action, body)
			if err == nil {
				return prices, nil
			}
		}
		if isContextError(err) {
			return nil, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", action, err))
	}
	return nil, fmt.Errorf("smsbower price catalogue failed: %w", errors.Join(errs...))
}

// GetNumber prefers the structured V2 action and falls back to ACCESS_NUMBER.
func (c *Client) GetNumber(ctx context.Context, req NumberRequest) (Activation, error) {
	if strings.TrimSpace(req.Service) == "" {
		return Activation{}, errors.New("smsbower: service is required")
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return Activation{}, errors.New("smsbower: API key is required")
	}
	params := numberParams(req)
	body, err := c.call(ctx, "getNumberV2", params)
	if err == nil {
		activation, parseErr := parseActivation("getNumberV2", body)
		if parseErr == nil {
			return activation, nil
		}
		err = parseErr
	}
	if !canFallbackNumber(err) {
		if isAmbiguousPurchaseError(err) {
			return Activation{}, unknownPurchase("getNumberV2", err)
		}
		return Activation{}, err
	}

	body, err = c.call(ctx, "getNumber", params)
	if err != nil {
		if isAmbiguousPurchaseError(err) {
			return Activation{}, unknownPurchase("getNumber", err)
		}
		return Activation{}, err
	}
	activation, err := parseActivation("getNumber", body)
	if err != nil {
		if isAmbiguousPurchaseError(err) {
			return Activation{}, unknownPurchase("getNumber", err)
		}
		return Activation{}, err
	}
	return activation, nil
}

// GetStatus performs one status read. Polling cadence belongs to the caller.
func (c *Client) GetStatus(ctx context.Context, activationID string) (ActivationStatus, error) {
	activationID = strings.TrimSpace(activationID)
	if activationID == "" {
		return ActivationStatus{}, errors.New("smsbower: activation ID is required")
	}
	body, err := c.call(ctx, "getStatus", map[string]string{"id": activationID})
	if err != nil {
		return ActivationStatus{}, err
	}
	return parseActivationStatus("getStatus", body)
}

// SetStatus sends status 3 (another SMS), 6 (complete), or 8 (cancel).
func (c *Client) SetStatus(ctx context.Context, activationID string, status SetStatus) (SetStatusResult, error) {
	activationID = strings.TrimSpace(activationID)
	if activationID == "" {
		return SetStatusResult{}, errors.New("smsbower: activation ID is required")
	}
	if !status.valid() {
		return SetStatusResult{}, fmt.Errorf("smsbower: unsupported setStatus value %d", status)
	}
	body, err := c.call(ctx, "setStatus", map[string]string{
		"id":     activationID,
		"status": strconv.Itoa(int(status)),
	})
	if err != nil {
		return SetStatusResult{}, err
	}
	return parseSetStatus("setStatus", body)
}

func priceParams(req PriceRequest) map[string]string {
	params := make(map[string]string, 5)
	if value := strings.TrimSpace(req.Service); value != "" {
		params["service"] = value
	}
	// Country zero is a real provider country ID. Send it whenever another
	// filter is present; an entirely zero request intentionally fetches all.
	if req.Country != 0 || req.Service != "" || req.MinPrice != "" || req.MaxPrice != "" || len(req.ProviderIDs) != 0 {
		params["country"] = strconv.Itoa(req.Country)
	}
	if value := strings.TrimSpace(req.MinPrice); value != "" {
		params["minPrice"] = value
	}
	if value := strings.TrimSpace(req.MaxPrice); value != "" {
		params["maxPrice"] = value
	}
	if value := joinInt64(req.ProviderIDs); value != "" {
		params["providerIds"] = value
	}
	return params
}

func numberParams(req NumberRequest) map[string]string {
	params := map[string]string{
		"service": strings.TrimSpace(req.Service),
		"country": strconv.Itoa(req.Country),
	}
	if value := strings.TrimSpace(req.MinPrice); value != "" {
		params["minPrice"] = value
	}
	if value := strings.TrimSpace(req.MaxPrice); value != "" {
		params["maxPrice"] = value
	}
	if value := joinInt64(req.ProviderIDs); value != "" {
		params["providerIds"] = value
	}
	if value := joinInt64(req.ExceptProviderIDs); value != "" {
		params["exceptProviderIds"] = value
	}
	if value := strings.TrimSpace(req.UserID); value != "" {
		params["userID"] = value
	}
	if len(req.PhoneException) != 0 {
		items := make([]string, 0, len(req.PhoneException))
		for _, item := range req.PhoneException {
			if item = strings.TrimSpace(item); item != "" {
				items = append(items, item)
			}
		}
		if len(items) != 0 {
			params["phoneException"] = strings.Join(items, ",")
		}
	}
	if value := strings.TrimSpace(req.Ref); value != "" {
		params["ref"] = value
	}
	return params
}

func joinInt64(values []int64) string {
	if len(values) == 0 {
		return ""
	}
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, strconv.FormatInt(value, 10))
	}
	return strings.Join(items, ",")
}

func (c *Client) call(ctx context.Context, action string, params map[string]string) ([]byte, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, errors.New("smsbower: API key is required")
	}
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, fmt.Errorf("smsbower %s: invalid endpoint: %w", action, err)
	}
	query := u.Query()
	query.Set("api_key", c.apiKey)
	query.Set("action", action)
	for key, value := range params {
		query.Set(key, value)
	}
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("smsbower %s: create request: %w", action, err)
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.8")
	req.Header.Set("User-Agent", "gopay-autosms/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("smsbower %s request: %w", action, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return nil, fmt.Errorf("smsbower %s response: %w", action, readErr)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("smsbower %s: response exceeds %d bytes", action, maxResponseBytes)
	}
	body = trimResponse(body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		code := "HTTP_" + strconv.Itoa(resp.StatusCode)
		return nil, &APIError{Action: action, Code: code, Message: http.StatusText(resp.StatusCode), Raw: string(body)}
	}
	if len(body) == 0 {
		return nil, &APIError{Action: action, Code: "EMPTY_RESPONSE", Message: "provider returned an empty response"}
	}
	return body, nil
}

func trimResponse(body []byte) []byte {
	value := strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff"))
	return []byte(value)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func canFallbackNumber(err error) bool {
	// These responses prove that the V2 action was rejected before allocation.
	for _, code := range []string{
		"BAD_ACTION", "WRONG_ACTION", "INVALID_ACTION", "UNKNOWN_ACTION",
		"ACTION_NOT_FOUND", "UNSUPPORTED_ACTION",
	} {
		if IsAPIError(err, code) {
			return true
		}
	}
	return false
}

func isAmbiguousPurchaseError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		// Transport/context failures do not prove whether the remote side acted.
		return true
	}
	code := strings.ToUpper(apiErr.Code)
	return strings.HasPrefix(code, "HTTP_") || code == "EMPTY_RESPONSE"
}

func unknownPurchase(action string, cause error) error {
	return &PurchaseUnknownError{Action: action, Cause: cause}
}
