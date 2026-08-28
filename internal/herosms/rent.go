package herosms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/smsbower"
)

const maxHeroResponseBytes = 4 << 20

// ErrNoNumbers is returned by PurchaseOne when HeroSMS conclusively reports
// that the requested service/country currently has no matching numbers.
var ErrNoNumbers = errors.New("herosms: no numbers available")

// VerificationType selects the HeroSMS activation offer catalogue.
type VerificationType string

const (
	VerificationSMS  VerificationType = "sms"
	VerificationCall VerificationType = "call"
)

// OfferRequest selects either one short activation offer (DurationHours == 0)
// or rental offers for one exact duration (DurationHours > 0).
type OfferRequest struct {
	Service          string           `json:"service"`
	Country          int              `json:"country"`
	DurationHours    int              `json:"duration_hours,omitempty"`
	VerificationType VerificationType `json:"verification_type,omitempty"`
}

// RentAvailabilityRequest asks serviceCountRent for all currently advertised
// country/duration combinations for one service.
type RentAvailabilityRequest struct {
	Service  string `json:"service"`
	Operator string `json:"operator,omitempty"`
	Currency string `json:"currency,omitempty"`
}

// Offer is the common shape used for short activations and rentals. Prices are
// strings so a value selected in the UI can be sent back without float drift.
type Offer struct {
	Service           string           `json:"service"`
	Country           int              `json:"country"`
	DurationHours     int              `json:"duration_hours,omitempty"`
	VerificationType  VerificationType `json:"verification_type,omitempty"`
	Price             string           `json:"price"`
	RetailPrice       string           `json:"retail_price,omitempty"`
	Currency          string           `json:"currency,omitempty"`
	Count             int              `json:"count"`
	PhysicalCount     int              `json:"physical_count,omitempty"`
	DefaultPriceCount int              `json:"default_price_count,omitempty"`
	PriceCounts       map[string]int   `json:"price_counts,omitempty"`
	Operators         []string         `json:"operators,omitempty"`
	Raw               json.RawMessage  `json:"-"`
}

// PurchaseRequest buys exactly one activation. A positive DurationHours uses
// getRentNumber; zero uses the regular getNumberV2 flow.
type PurchaseRequest struct {
	Service          string           `json:"service"`
	Country          int              `json:"country"`
	DurationHours    int              `json:"duration_hours,omitempty"`
	VerificationType VerificationType `json:"verification_type,omitempty"`
	MaxPrice         string           `json:"max_price,omitempty"`
	Operator         string           `json:"operator,omitempty"`
	Currency         string           `json:"currency,omitempty"`
	Ref              string           `json:"ref,omitempty"`
	PhoneException   []string         `json:"phone_exception,omitempty"`
}

// Purchase preserves the lifecycle timestamps HeroSMS returns. ExpiresAt is
// activationEndTime and is therefore the authoritative provider countdown.
type Purchase struct {
	ActivationID     string           `json:"activation_id"`
	PhoneNumber      string           `json:"phone_number"`
	Service          string           `json:"service"`
	Country          int              `json:"country"`
	CountryPhoneCode string           `json:"country_phone_code,omitempty"`
	DurationHours    int              `json:"duration_hours,omitempty"`
	Rent             bool             `json:"rent"`
	Cost             string           `json:"cost,omitempty"`
	Currency         string           `json:"currency,omitempty"`
	CanGetAnotherSMS bool             `json:"can_get_another_sms,omitempty"`
	Operator         string           `json:"operator,omitempty"`
	VerificationType VerificationType `json:"verification_type,omitempty"`
	ActivatedAt      time.Time        `json:"activated_at,omitempty"`
	ExpiresAt        time.Time        `json:"expires_at,omitempty"`
	Raw              json.RawMessage  `json:"-"`
}

// Message is one getAllSms item, or the compatible representation of a code
// observed through getStatus for a short activation.
type Message struct {
	ID               string           `json:"id,omitempty"`
	PhoneFrom        string           `json:"phone_from,omitempty"`
	Code             string           `json:"code,omitempty"`
	Text             string           `json:"text,omitempty"`
	Service          string           `json:"service,omitempty"`
	VerificationType VerificationType `json:"verification_type,omitempty"`
	ReceivedAt       time.Time        `json:"received_at,omitempty"`
	Raw              json.RawMessage  `json:"-"`
}

type heroTransport struct {
	apiKey          string
	handlerEndpoint string
	restEndpoint    string
	http            smsbower.HTTPDoer
}

func newHeroTransport(baseURL, apiKey string, doer smsbower.HTTPDoer) (*heroTransport, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("herosms: invalid base URL %q", baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("herosms: unsupported URL scheme %q", parsed.Scheme)
	}

	handler := *parsed
	handler.Fragment = ""
	if !strings.HasSuffix(strings.ToLower(strings.TrimRight(handler.Path, "/")), "handler_api.php") {
		handler.Path = strings.TrimRight(handler.Path, "/") + smsbower.HandlerPath
	}

	// The activation offers endpoint is a native REST endpoint. Derive it from
	// the configured handler host rather than appending to handler_api.php.
	rest := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: "/api/v1"}
	if doer == nil {
		doer = &http.Client{Timeout: 30 * time.Second}
	}
	return &heroTransport{
		apiKey: strings.TrimSpace(apiKey), handlerEndpoint: handler.String(),
		restEndpoint: rest.String(), http: doer,
	}, nil
}

// Offers returns the currently purchasable offers for one selection. Rental
// offers come from getRentServicesAndCountries; short activation offers come
// from the native /api/v1 catalogue.
func (c *Client) Offers(ctx context.Context, req OfferRequest) ([]Offer, error) {
	if c == nil || c.transport == nil {
		return nil, errors.New("herosms: client is not initialized")
	}
	if req.DurationHours < 0 {
		return nil, errors.New("herosms: duration hours must not be negative")
	}
	if req.DurationHours > 0 {
		return c.rentOffers(ctx, req)
	}
	verificationType, err := normalizeVerificationType(req.VerificationType)
	if err != nil {
		return nil, err
	}
	params := map[string]string{"countries": strconv.Itoa(req.Country)}
	if service := strings.TrimSpace(req.Service); service != "" {
		params["services"] = service
	}
	body, err := c.transport.restCall(ctx, "activations/offers/"+string(verificationType), params)
	if err != nil {
		return nil, markNoNumbers(err)
	}
	offers, err := parseActivationOffers(body, req.Service, req.Country, verificationType)
	if err != nil {
		return nil, err
	}
	return offers, nil
}

func (c *Client) rentOffers(ctx context.Context, req OfferRequest) ([]Offer, error) {
	params := map[string]string{
		"country":  strconv.Itoa(req.Country),
		"duration": strconv.Itoa(req.DurationHours),
	}
	body, err := c.transport.handlerCall(ctx, "getRentServicesAndCountries", params, false)
	if err != nil {
		return nil, err
	}
	return parseRentOffers(body, req)
}

// RentAvailability returns every country/duration bucket advertised for a
// service. It is useful for building the duration selector before Offers is
// called for the user's exact country and duration.
func (c *Client) RentAvailability(ctx context.Context, req RentAvailabilityRequest) ([]Offer, error) {
	if c == nil || c.transport == nil {
		return nil, errors.New("herosms: client is not initialized")
	}
	service := strings.TrimSpace(req.Service)
	if service == "" {
		return nil, errors.New("herosms: service is required")
	}
	params := map[string]string{"service": service}
	if operator := strings.TrimSpace(req.Operator); operator != "" {
		params["operator"] = operator
	}
	if currency := strings.TrimSpace(req.Currency); currency != "" {
		params["currency"] = currency
	}
	body, err := c.transport.handlerCall(ctx, "serviceCountRent", params, false)
	if err != nil {
		return nil, err
	}
	return parseRentAvailability(body, req)
}

// PurchaseOne buys one regular activation or rental and returns the provider's
// activationEndTime for the effective-number countdown.
func (c *Client) PurchaseOne(ctx context.Context, req PurchaseRequest) (Purchase, error) {
	if c == nil || c.transport == nil {
		return Purchase{}, errors.New("herosms: client is not initialized")
	}
	service := strings.TrimSpace(req.Service)
	if service == "" {
		return Purchase{}, errors.New("herosms: service is required")
	}
	if req.DurationHours < 0 {
		return Purchase{}, errors.New("herosms: duration hours must not be negative")
	}
	verificationType, err := normalizeVerificationType(req.VerificationType)
	if err != nil {
		return Purchase{}, err
	}
	req.VerificationType = verificationType
	if req.DurationHours > 0 && verificationType != VerificationSMS {
		return Purchase{}, errors.New("herosms: rental numbers support SMS verification only")
	}
	if req.DurationHours == 0 {
		params := map[string]string{
			"service": service, "country": strconv.Itoa(req.Country),
			"activationType": activationType(verificationType),
		}
		if maxPrice := strings.TrimSpace(req.MaxPrice); maxPrice != "" {
			params["maxPrice"] = maxPrice
		}
		if operator := strings.TrimSpace(req.Operator); operator != "" {
			params["operator"] = operator
		}
		if ref := strings.TrimSpace(req.Ref); ref != "" {
			params["ref"] = ref
		}
		if exceptions := joinHeroStrings(req.PhoneException); exceptions != "" {
			params["phoneException"] = exceptions
		}
		body, err := c.transport.handlerCall(ctx, "getNumberV2", params, false)
		if err != nil {
			if IsNoNumbers(err) {
				return Purchase{}, markNoNumbers(err)
			}
			if conclusivePurchaseError(err) {
				return Purchase{}, err
			}
			return Purchase{}, unknownHeroPurchase("getNumberV2", err)
		}
		purchase, parseErr := parsePurchase(body, req)
		if parseErr != nil {
			return Purchase{}, unknownHeroPurchase("getNumberV2", parseErr)
		}
		return purchase, nil
	}

	params := map[string]string{
		"service": service, "country": strconv.Itoa(req.Country),
		"duration": strconv.Itoa(req.DurationHours),
	}
	if operator := strings.TrimSpace(req.Operator); operator != "" {
		params["operator"] = operator
	}
	if currency := strings.TrimSpace(req.Currency); currency != "" {
		params["currency"] = currency
	}
	if ref := strings.TrimSpace(req.Ref); ref != "" {
		params["ref"] = ref
	}
	body, err := c.transport.handlerCall(ctx, "getRentNumber", params, false)
	if err != nil {
		if IsNoNumbers(err) {
			return Purchase{}, markNoNumbers(err)
		}
		if conclusivePurchaseError(err) {
			return Purchase{}, err
		}
		return Purchase{}, unknownHeroPurchase("getRentNumber", err)
	}
	purchase, err := parsePurchase(body, req)
	if err != nil {
		return Purchase{}, unknownHeroPurchase("getRentNumber", err)
	}
	if purchase.ExpiresAt.IsZero() {
		return Purchase{}, unknownHeroPurchase("getRentNumber", errors.New("herosms getRentNumber: response lacks activationEndTime"))
	}
	return purchase, nil
}

// GetMessages performs one provider read. Rental numbers use getAllSms;
// regular activations use getStatus as a low-frequency fallback when a webhook
// is delayed or not delivered. Scheduling and de-duplication belong to the
// independent task manager.
func (c *Client) GetMessages(ctx context.Context, activationID string, rent bool) ([]Message, error) {
	if c == nil || c.transport == nil {
		return nil, errors.New("herosms: client is not initialized")
	}
	activationID = strings.TrimSpace(activationID)
	if activationID == "" {
		return nil, errors.New("herosms: activation ID is required")
	}
	if !rent {
		status, err := c.GetStatus(ctx, activationID)
		if err != nil {
			return nil, err
		}
		code := strings.TrimSpace(status.Code)
		if status.Kind != smsbower.StatusOK || code == "" {
			return []Message{}, nil
		}
		return []Message{{
			Code: code, VerificationType: VerificationSMS,
			Raw: cloneHeroRaw(status.Raw),
		}}, nil
	}

	body, err := c.transport.handlerCall(ctx, "getAllSms", map[string]string{"id": activationID}, false)
	if err != nil {
		return nil, err
	}
	return parseMessages(body)
}

// Finish settles an activation successfully. Rental endpoints acknowledge with
// HTTP 204; regular activations use setStatus=6.
func (c *Client) Finish(ctx context.Context, activationID string, rent bool) error {
	return c.finalize(ctx, activationID, rent, true)
}

// Cancel stops an activation and requests a refund when HeroSMS still permits
// one. Rental endpoints use cancelActivation; regular activations use status 8.
func (c *Client) Cancel(ctx context.Context, activationID string, rent bool) error {
	return c.finalize(ctx, activationID, rent, false)
}

// RequestAnother keeps a regular activation open for the next SMS by sending
// the SMS-Activate-compatible setStatus=3 transition.
func (c *Client) RequestAnother(ctx context.Context, activationID string) error {
	if c == nil || c.transport == nil {
		return errors.New("herosms: client is not initialized")
	}
	_, err := c.SetStatus(ctx, activationID, smsbower.SetStatusRequestAnother)
	return err
}

func (c *Client) finalize(ctx context.Context, activationID string, rent, finish bool) error {
	if c == nil || c.transport == nil {
		return errors.New("herosms: client is not initialized")
	}
	activationID = strings.TrimSpace(activationID)
	if activationID == "" {
		return errors.New("herosms: activation ID is required")
	}
	if rent {
		action := "cancelActivation"
		if finish {
			action = "finishActivation"
		}
		_, err := c.transport.handlerCall(ctx, action, map[string]string{"id": activationID}, true)
		return mapMissingRentActivation(err)
	}
	status := smsbower.SetStatusCancel
	if finish {
		status = smsbower.SetStatusComplete
	}
	_, err := c.SetStatus(ctx, activationID, status)
	return err
}

func mapMissingRentActivation(err error) error {
	var apiErr *smsbower.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	code := strings.ToUpper(strings.TrimSpace(apiErr.Code))
	if code != "HTTP_404" && code != "NOT_FOUND" && code != "ACTIVATION_NOT_FOUND" && code != "NO_ACTIVATION" {
		return err
	}
	mapped := *apiErr
	mapped.Provider = "HeroSMS"
	mapped.Code = "NO_ACTIVATION"
	mapped.Message = ""
	return &mapped
}

// IsNoNumbers recognizes both HeroSMS's NO_NUMBERS token and its documented
// HTTP 404 purchase response. PurchaseOne additionally marks these errors with
// ErrNoNumbers so errors.Is can be used directly.
func IsNoNumbers(err error) bool {
	return errors.Is(err, ErrNoNumbers) || smsbower.IsAPIError(err, "NO_NUMBERS") ||
		smsbower.IsAPIError(err, "HTTP_404")
}

type noNumbersError struct{ cause error }

func (e *noNumbersError) Error() string { return e.cause.Error() }
func (e *noNumbersError) Unwrap() error { return e.cause }
func (e *noNumbersError) Is(target error) bool {
	return target == ErrNoNumbers
}

func markNoNumbers(err error) error {
	if err == nil || errors.Is(err, ErrNoNumbers) {
		return err
	}
	if smsbower.IsAPIError(err, "NO_NUMBERS") || smsbower.IsAPIError(err, "HTTP_404") {
		return &noNumbersError{cause: err}
	}
	return err
}

func conclusivePurchaseError(err error) bool {
	var apiErr *smsbower.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToUpper(strings.TrimSpace(apiErr.Code))
	if code == "EMPTY_RESPONSE" {
		return false
	}
	if strings.HasPrefix(code, "ERROR") || strings.HasPrefix(code, "SERVER_") ||
		strings.HasPrefix(code, "EXCEPTION_") || strings.HasPrefix(code, "INTERNAL_") {
		return false
	}
	if strings.HasPrefix(code, "HTTP_") {
		status, parseErr := strconv.Atoi(strings.TrimPrefix(code, "HTTP_"))
		return parseErr == nil && status >= 400 && status < 500
	}
	// A provider business token in a successful HTTP response means the rent
	// request was rejected before allocation.
	return code != ""
}

func unknownHeroPurchase(action string, cause error) error {
	return &smsbower.PurchaseUnknownError{Action: action, Cause: cause}
}

func activationType(verificationType VerificationType) string {
	if verificationType == VerificationCall {
		return "1"
	}
	return "0"
}

func joinHeroStrings(values []string) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			items = append(items, value)
		}
	}
	return strings.Join(items, ",")
}

func normalizeVerificationType(value VerificationType) (VerificationType, error) {
	switch VerificationType(strings.ToLower(strings.TrimSpace(string(value)))) {
	case "", VerificationSMS:
		return VerificationSMS, nil
	case VerificationCall:
		return VerificationCall, nil
	default:
		return "", fmt.Errorf("herosms: unsupported verification type %q", value)
	}
}

func (t *heroTransport) handlerCall(
	ctx context.Context,
	action string,
	params map[string]string,
	allowEmpty bool,
) ([]byte, error) {
	return t.call(ctx, t.handlerEndpoint, action, params, false, allowEmpty)
}

func (t *heroTransport) restCall(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	endpoint := strings.TrimRight(t.restEndpoint, "/") + "/" + strings.TrimLeft(path, "/")
	return t.call(ctx, endpoint, path, params, true, false)
}

func (t *heroTransport) call(
	ctx context.Context,
	endpoint, action string,
	params map[string]string,
	rest, allowEmpty bool,
) ([]byte, error) {
	if strings.TrimSpace(t.apiKey) == "" {
		return nil, errors.New("herosms: API key is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("herosms %s: invalid endpoint: %w", action, err)
	}
	query := parsed.Query()
	if !rest {
		query.Set("api_key", t.apiKey)
		query.Set("action", action)
	}
	for key, value := range params {
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("herosms %s: create request: %w", action, err)
	}
	request.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.8")
	request.Header.Set("User-Agent", "gopay-autosms/1.0")
	if rest {
		request.Header.Set("Authorization", "ApiKey "+t.apiKey)
	}
	response, err := t.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("herosms %s request: %w", action, smsbower.RedactRequestError(err))
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxHeroResponseBytes+1))
	if readErr != nil {
		return nil, fmt.Errorf("herosms %s response: %w", action, readErr)
	}
	if len(body) > maxHeroResponseBytes {
		return nil, fmt.Errorf("herosms %s: response exceeds %d bytes", action, maxHeroResponseBytes)
	}
	body = trimHeroResponse(body)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		code, message := heroErrorDetails(body)
		if code == "" {
			code = "HTTP_" + strconv.Itoa(response.StatusCode)
			message = http.StatusText(response.StatusCode)
		}
		return nil, &smsbower.APIError{
			Provider: "HeroSMS", Action: action, Code: code, Message: message, Raw: string(body),
		}
	}
	if len(body) == 0 {
		if allowEmpty {
			return []byte{}, nil
		}
		return nil, &smsbower.APIError{
			Provider: "HeroSMS", Action: action, Code: "EMPTY_RESPONSE",
			Message: "provider returned an empty response",
		}
	}
	if code, message := heroErrorDetails(body); isHeroErrorCode(code) {
		return nil, &smsbower.APIError{
			Provider: "HeroSMS", Action: action, Code: code, Message: message, Raw: string(body),
		}
	}
	return body, nil
}

func trimHeroResponse(body []byte) []byte {
	return []byte(strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff")))
}

func heroErrorDetails(body []byte) (string, string) {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "", ""
	}
	if text[0] != '{' && text[0] != '[' {
		code, message := splitHeroError(text)
		if isHeroErrorCode(code) {
			return code, message
		}
		return "", ""
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return "", ""
	}
	var code string
	for _, key := range []string{"title", "errorCode", "error", "code", "status"} {
		if value, ok := lookupHeroRaw(object, key); ok {
			candidate := strings.ToUpper(strings.TrimSpace(rawScalar(value)))
			if isHeroErrorCode(candidate) {
				code = candidate
				break
			}
		}
	}
	message := ""
	for _, key := range []string{"details", "message", "description", "errorMessage"} {
		if value, ok := lookupHeroRaw(object, key); ok {
			message = strings.TrimSpace(rawScalar(value))
			if message != "" {
				break
			}
		}
	}
	if code == "" && message != "" {
		candidate, detail := splitHeroError(message)
		if isHeroErrorCode(candidate) {
			code = candidate
			if detail != "" {
				message = detail
			}
		}
	}
	return code, message
}

func splitHeroError(text string) (string, string) {
	text = strings.TrimSpace(text)
	for _, separator := range []string{":", " ", "\t", "\n"} {
		if index := strings.Index(text, separator); index > 0 {
			return strings.ToUpper(strings.TrimSpace(text[:index])), strings.TrimSpace(text[index+len(separator):])
		}
	}
	return strings.ToUpper(text), ""
}

func isHeroErrorCode(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, prefix := range []string{
		"BAD_", "NO_", "NOT_", "ERROR", "WRONG_", "INVALID_", "EARLY_",
		"ACCOUNT_", "BANNED", "MAX_", "CANNOT_", "EXCEPTION_", "HTTP_",
		"ACTION_", "ACTIVATION_", "CHANNELS_", "FREE_", "FREEZE_", "NEW_",
		"OTP_", "SERVER_", "SERVICE_", "SIM_", "UNPROCESSABLE_",
		"FORBIDDEN", "UNAUTHORIZED",
	} {
		if strings.HasPrefix(code, prefix) {
			return true
		}
	}
	return false
}

func parseActivationOffers(
	body []byte,
	serviceFilter string,
	countryFilter int,
	verificationType VerificationType,
) ([]Offer, error) {
	var envelope struct {
		Data map[string]map[string]json.RawMessage `json:"data"`
	}
	if err := decodeHeroJSON(body, &envelope); err != nil {
		return nil, fmt.Errorf("herosms activations/offers/%s: decode response: %w", verificationType, err)
	}
	offers := make([]Offer, 0)
	serviceFilter = strings.TrimSpace(serviceFilter)
	for service, countries := range envelope.Data {
		if serviceFilter != "" && service != serviceFilter {
			continue
		}
		for countryText, raw := range countries {
			country, err := strconv.Atoi(countryText)
			if err != nil || country != countryFilter {
				continue
			}
			var item struct {
				Prices struct {
					Default json.RawMessage `json:"default"`
					Retail  json.RawMessage `json:"retail"`
					Min     json.RawMessage `json:"min"`
				} `json:"prices"`
				Counts struct {
					Total        json.RawMessage `json:"total"`
					Physical     json.RawMessage `json:"physical"`
					DefaultPrice json.RawMessage `json:"defaultPrice"`
				} `json:"counts"`
				Map map[string]int `json:"map"`
			}
			if err = json.Unmarshal(raw, &item); err != nil {
				return nil, fmt.Errorf("herosms activations/offers/%s: decode offer: %w", verificationType, err)
			}
			price := rawScalar(item.Prices.Default)
			if price == "" {
				price = rawScalar(item.Prices.Min)
			}
			offers = append(offers, Offer{
				Service: service, Country: country, VerificationType: verificationType,
				Price: price, RetailPrice: rawScalar(item.Prices.Retail),
				Count: rawInt(item.Counts.Total), PhysicalCount: rawInt(item.Counts.Physical),
				DefaultPriceCount: rawInt(item.Counts.DefaultPrice), PriceCounts: item.Map,
				Raw: cloneHeroRaw(raw),
			})
		}
	}
	sortOffers(offers)
	return offers, nil
}

func parseRentOffers(body []byte, req OfferRequest) ([]Offer, error) {
	var payload struct {
		Operators map[string]string          `json:"operators"`
		Services  map[string]json.RawMessage `json:"services"`
	}
	if err := decodeHeroJSON(body, &payload); err != nil {
		return nil, fmt.Errorf("herosms getRentServicesAndCountries: decode response: %w", err)
	}
	operators := make([]string, 0, len(payload.Operators))
	for _, operator := range payload.Operators {
		if operator = strings.TrimSpace(operator); operator != "" {
			operators = append(operators, operator)
		}
	}
	sort.Strings(operators)
	serviceFilter := strings.TrimSpace(req.Service)
	offers := make([]Offer, 0, len(payload.Services))
	for service, raw := range payload.Services {
		if serviceFilter != "" && service != serviceFilter {
			continue
		}
		var item struct {
			Quantity    json.RawMessage `json:"quantity"`
			Price       json.RawMessage `json:"price"`
			RetailPrice json.RawMessage `json:"retail_price"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("herosms getRentServicesAndCountries: decode offer: %w", err)
		}
		offers = append(offers, Offer{
			Service: service, Country: req.Country, DurationHours: req.DurationHours,
			VerificationType: VerificationSMS, Price: rawScalar(item.Price),
			RetailPrice: rawScalar(item.RetailPrice), Count: rawInt(item.Quantity),
			Operators: append([]string(nil), operators...), Raw: cloneHeroRaw(raw),
		})
	}
	sortOffers(offers)
	return offers, nil
}

func parseRentAvailability(body []byte, req RentAvailabilityRequest) ([]Offer, error) {
	// The official empty example is encoded as the JSON string "{}", while
	// populated responses are JSON objects. Accept both without treating an
	// empty catalogue as an API failure.
	var encoded string
	if json.Unmarshal(body, &encoded) == nil && strings.HasPrefix(strings.TrimSpace(encoded), "{") {
		body = []byte(encoded)
	}
	var countries map[string]json.RawMessage
	if err := decodeHeroJSON(body, &countries); err != nil {
		return nil, fmt.Errorf("herosms serviceCountRent: decode response: %w", err)
	}
	if data, ok := lookupHeroRaw(countries, "data"); ok && len(data) != 0 && data[0] == '{' {
		if err := json.Unmarshal(data, &countries); err != nil {
			return nil, fmt.Errorf("herosms serviceCountRent: decode data: %w", err)
		}
	}
	offers := make([]Offer, 0)
	for countryText, durationsRaw := range countries {
		country, err := strconv.Atoi(countryText)
		if err != nil {
			continue
		}
		var durations map[string]json.RawMessage
		if err = json.Unmarshal(durationsRaw, &durations); err != nil {
			return nil, fmt.Errorf("herosms serviceCountRent: decode country %d: %w", country, err)
		}
		for durationText, raw := range durations {
			duration, parseErr := strconv.Atoi(durationText)
			if parseErr != nil {
				continue
			}
			var item struct {
				Price       json.RawMessage `json:"price"`
				RetailPrice json.RawMessage `json:"retail_price"`
				Count       json.RawMessage `json:"count"`
			}
			if err = json.Unmarshal(raw, &item); err != nil {
				return nil, fmt.Errorf("herosms serviceCountRent: decode duration %d: %w", duration, err)
			}
			offer := Offer{
				Service: strings.TrimSpace(req.Service), Country: country, DurationHours: duration,
				VerificationType: VerificationSMS, Price: rawScalar(item.Price),
				RetailPrice: rawScalar(item.RetailPrice), Currency: strings.TrimSpace(req.Currency),
				Count: rawInt(item.Count), Raw: cloneHeroRaw(raw),
			}
			if operator := strings.TrimSpace(req.Operator); operator != "" {
				offer.Operators = []string{operator}
			}
			offers = append(offers, offer)
		}
	}
	sortOffers(offers)
	return offers, nil
}

func parsePurchase(body []byte, req PurchaseRequest) (Purchase, error) {
	var payload struct {
		ActivationID     json.RawMessage `json:"activationId"`
		PhoneNumber      json.RawMessage `json:"phoneNumber"`
		ServiceCode      json.RawMessage `json:"serviceCode"`
		ActivationCost   json.RawMessage `json:"activationCost"`
		Currency         json.RawMessage `json:"currency"`
		CountryCode      json.RawMessage `json:"countryCode"`
		CountryPhoneCode json.RawMessage `json:"countryPhoneCode"`
		CanGetAnotherSMS bool            `json:"canGetAnotherSms"`
		ActivationTime   json.RawMessage `json:"activationTime"`
		ActivationEnd    json.RawMessage `json:"activationEndTime"`
		Operator         json.RawMessage `json:"activationOperator"`
		VerificationType json.RawMessage `json:"verificationType"`
	}
	if err := decodeHeroJSON(body, &payload); err != nil {
		return Purchase{}, fmt.Errorf("herosms getRentNumber: decode response: %w", err)
	}
	activationID := strings.TrimSpace(rawScalar(payload.ActivationID))
	phoneNumber := normalizeHeroPhone(rawScalar(payload.PhoneNumber))
	if activationID == "" || phoneNumber == "" {
		return Purchase{}, errors.New("herosms getRentNumber: response lacks activationId or phoneNumber")
	}
	country := req.Country
	if parsed, err := strconv.Atoi(rawScalar(payload.CountryCode)); err == nil {
		country = parsed
	}
	verificationType := req.VerificationType
	if verificationType == "" {
		verificationType = VerificationSMS
	}
	if value := strings.TrimSpace(rawScalar(payload.VerificationType)); value != "" {
		verificationType = VerificationType(value)
	}
	service := strings.TrimSpace(rawScalar(payload.ServiceCode))
	if service == "" {
		service = strings.TrimSpace(req.Service)
	}
	return Purchase{
		ActivationID: activationID, PhoneNumber: phoneNumber, Service: service,
		Country: country, CountryPhoneCode: rawScalar(payload.CountryPhoneCode),
		DurationHours: req.DurationHours, Rent: req.DurationHours > 0,
		Cost: rawScalar(payload.ActivationCost), Currency: normalizeCurrency(rawScalar(payload.Currency)),
		CanGetAnotherSMS: payload.CanGetAnotherSMS, Operator: rawScalar(payload.Operator),
		VerificationType: verificationType, ActivatedAt: parseHeroTime(rawScalar(payload.ActivationTime)),
		ExpiresAt: parseHeroTime(rawScalar(payload.ActivationEnd)), Raw: cloneHeroRaw(body),
	}, nil
}

func parseMessages(body []byte) ([]Message, error) {
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := decodeHeroJSON(body, &envelope); err != nil {
		var direct []json.RawMessage
		if directErr := decodeHeroJSON(body, &direct); directErr != nil {
			return nil, fmt.Errorf("herosms getAllSms: decode response: %w", err)
		}
		envelope.Data = direct
	}
	messages := make([]Message, 0, len(envelope.Data))
	for _, raw := range envelope.Data {
		var item struct {
			ID        json.RawMessage `json:"id"`
			PhoneFrom json.RawMessage `json:"phoneFrom"`
			Code      json.RawMessage `json:"code"`
			Text      json.RawMessage `json:"text"`
			Service   json.RawMessage `json:"service"`
			Date      json.RawMessage `json:"date"`
			Type      json.RawMessage `json:"type"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("herosms getAllSms: decode message: %w", err)
		}
		messages = append(messages, Message{
			ID: rawScalar(item.ID), PhoneFrom: rawScalar(item.PhoneFrom),
			Code: rawScalar(item.Code), Text: rawScalar(item.Text), Service: rawScalar(item.Service),
			VerificationType: VerificationType(rawScalar(item.Type)), ReceivedAt: parseHeroTime(rawScalar(item.Date)),
			Raw: cloneHeroRaw(raw),
		})
	}
	sort.SliceStable(messages, func(i, j int) bool {
		if !messages[i].ReceivedAt.Equal(messages[j].ReceivedAt) {
			return messages[i].ReceivedAt.Before(messages[j].ReceivedAt)
		}
		return messages[i].ID < messages[j].ID
	})
	return messages, nil
}

func decodeHeroJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("unexpected trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected trailing JSON: %w", err)
	}
	return nil
}

func lookupHeroRaw(object map[string]json.RawMessage, wanted string) (json.RawMessage, bool) {
	wanted = normalizeHeroKey(wanted)
	for key, value := range object {
		if normalizeHeroKey(key) == wanted {
			return value, true
		}
	}
	return nil, false
}

func normalizeHeroKey(value string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(value))
}

func rawScalar(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return ""
	}
	switch value := value.(type) {
	case json.Number:
		return value.String()
	case bool:
		return strconv.FormatBool(value)
	default:
		return ""
	}
}

func rawInt(raw json.RawMessage) int {
	value := rawScalar(raw)
	if parsed, err := strconv.Atoi(value); err == nil {
		return parsed
	}
	parsed, _ := strconv.ParseFloat(value, 64)
	return int(parsed)
}

func parseHeroTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if unixSeconds, err := strconv.ParseInt(value, 10, 64); err == nil && unixSeconds > 0 {
		return time.Unix(unixSeconds, 0).UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func normalizeHeroPhone(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "+") {
		return value
	}
	return "+" + value
}

func cloneHeroRaw(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func sortOffers(offers []Offer) {
	sort.SliceStable(offers, func(i, j int) bool {
		if offers[i].Service != offers[j].Service {
			return offers[i].Service < offers[j].Service
		}
		if offers[i].Country != offers[j].Country {
			return offers[i].Country < offers[j].Country
		}
		if offers[i].DurationHours != offers[j].DurationHours {
			return offers[i].DurationHours < offers[j].DurationHours
		}
		left, leftErr := strconv.ParseFloat(offers[i].Price, 64)
		right, rightErr := strconv.ParseFloat(offers[j].Price, 64)
		if leftErr == nil && rightErr == nil && left != right {
			return left < right
		}
		return offers[i].Price < offers[j].Price
	})
}
