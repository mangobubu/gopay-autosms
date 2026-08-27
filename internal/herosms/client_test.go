package herosms

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mangobubu/gopay-autosms/internal/smsbower"
)

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (f httpDoerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func TestClientUsesHeroEndpointAndLegacyPriceActionWithoutTiers(t *testing.T) {
	var requests []*http.Request
	client, err := NewClient(Config{
		APIKey: "hero-secret",
		HTTPClient: httpDoerFunc(func(request *http.Request) (*http.Response, error) {
			requests = append(requests, request)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"prices":[{"country":6,"service":"go","providerId":17,"price":"0.8","count":10,"tier":"Gold"}]}`,
				)),
				Request: request,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	prices, err := client.GetPrices(context.Background(), smsbower.PriceRequest{Service: "go", Country: 6})
	if err != nil {
		t.Fatalf("GetPrices() error = %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	request := requests[0]
	if got := request.URL.String(); !strings.HasPrefix(got, DefaultBaseURL+"?") {
		t.Fatalf("endpoint = %q, want %q", got, DefaultBaseURL)
	}
	if got := request.URL.Query().Get("action"); got != "getPrices" {
		t.Fatalf("action = %q, want getPrices", got)
	}
	if got := request.URL.Query().Get("api_key"); got != "hero-secret" {
		t.Fatalf("api_key = %q", got)
	}
	if len(prices) != 1 || prices[0].Price != "0.8" || prices[0].ProviderID != 0 || prices[0].Tier != "" {
		t.Fatalf("prices = %#v, want one provider- and tier-free HeroSMS price", prices)
	}
}

func TestGetNumberUsesHeroSupportedParamsAndCountryPhoneCode(t *testing.T) {
	var query url.Values
	client, err := NewClient(Config{
		APIKey: "hero-secret",
		HTTPClient: httpDoerFunc(func(request *http.Request) (*http.Response, error) {
			query = request.URL.Query()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"activationId":"hero-1","phoneNumber":"628123456789","currency":840,"countryCode":6,"countryPhoneCode":62}`,
				)),
				Request: request,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := client.GetNumber(context.Background(), smsbower.NumberRequest{
		Service: "go", Country: 6, MinPrice: "0.80", MaxPrice: "1.00",
		ProviderIDs: []int64{1}, ExceptProviderIDs: []int64{2}, UserID: "buyer-1",
		PhoneException: []string{"6280"}, Ref: "autosms",
	})
	if err != nil {
		t.Fatal(err)
	}
	if activation.CountryCode != "6" || activation.CountryPhoneCode != "62" {
		t.Fatalf("activation country codes = %#v", activation)
	}
	if activation.Currency != "USD" {
		t.Fatalf("activation currency = %q, want USD", activation.Currency)
	}
	for _, unsupported := range []string{"minPrice", "providerIds", "exceptProviderIds", "userID"} {
		if got := query.Get(unsupported); got != "" {
			t.Errorf("unsupported query %s = %q", unsupported, got)
		}
	}
	for key, want := range map[string]string{
		"action": "getNumberV2", "service": "go", "country": "6", "maxPrice": "1.00",
		"phoneException": "6280", "ref": "autosms",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
}

func TestNormalizeHeroCurrency(t *testing.T) {
	for input, want := range map[string]string{
		"840": "USD", "978": "EUR", "156": "CNY", "643": "RUB",
		" usd ": "USD", "": "",
	} {
		if got := normalizeCurrency(input); got != want {
			t.Errorf("normalizeCurrency(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHeroHTTPPurchaseRejectionIsConclusive(t *testing.T) {
	client, err := NewClient(Config{
		APIKey: "hero-secret",
		HTTPClient: httpDoerFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"message":"no numbers"}`)),
				Request:    request,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetNumber(context.Background(), smsbower.NumberRequest{Service: "go", Country: 6})
	if !smsbower.IsAPIError(err, "HTTP_404") || errors.Is(err, smsbower.ErrPurchaseUnknown) {
		t.Fatalf("error = %v, want conclusive HTTP_404", err)
	}
}

func TestHeroSetStatusMapsNotFoundToNoActivation(t *testing.T) {
	client, err := NewClient(Config{
		APIKey: "hero-secret",
		HTTPClient: httpDoerFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNotFound, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"message":"missing"}`)), Request: request,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SetStatus(context.Background(), "hero-1", smsbower.SetStatusCancel)
	if !smsbower.IsAPIError(err, "NO_ACTIVATION") {
		t.Fatalf("error = %v, want NO_ACTIVATION", err)
	}
}

func TestHeroSetStatusPreservesConflictForRetry(t *testing.T) {
	client, err := NewClient(Config{
		APIKey: "hero-secret",
		HTTPClient: httpDoerFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusConflict, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"title":"EARLY_CANCEL_DENIED","details":"Activation cannot be cancelled yet","info":{"minActivationTime":120}}`,
				)), Request: request,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SetStatus(context.Background(), "hero-1", smsbower.SetStatusCancel)
	if !smsbower.IsAPIError(err, "HTTP_409") {
		t.Fatalf("error = %v, want original HTTP_409", err)
	}
	if smsbower.IsAPIError(err, "BAD_STATUS") {
		t.Fatalf("error = %v, must not conclude BAD_STATUS", err)
	}
}
