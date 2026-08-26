package smsbower

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGetNumberParsesAccessNumber(t *testing.T) {
	client, requests := testClient(t, func(values url.Values) string {
		if values.Get("action") != "getNumberV2" {
			return "ACCESS_NUMBER:123456:628123456789"
		}
		return "BAD_ACTION"
	})

	activation, err := client.GetNumber(context.Background(), NumberRequest{
		Service:           "ni",
		Country:           6,
		MinPrice:          "1.00",
		MaxPrice:          "1.00",
		ProviderIDs:       []int64{17, 23},
		ExceptProviderIDs: []int64{99},
		UserID:            "job-1",
		PhoneException:    []string{"+620", "6288"},
		Ref:               "autosms",
	})
	if err != nil {
		t.Fatalf("GetNumber() error = %v", err)
	}
	if activation.ActivationID != "123456" || activation.PhoneNumber != "+628123456789" {
		t.Fatalf("GetNumber() = %#v", activation)
	}
	if len(*requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(*requests))
	}
	query := (*requests)[1]
	wants := map[string]string{
		"action": "getNumber", "api_key": "secret", "service": "ni", "country": "6",
		"minPrice": "1.00", "maxPrice": "1.00", "providerIds": "17,23",
		"exceptProviderIds": "99", "userID": "job-1", "phoneException": "+620,6288", "ref": "autosms",
	}
	for key, want := range wants {
		if got := query.Get(key); got != want {
			t.Errorf("query %q = %q, want %q", key, got, want)
		}
	}
}

func TestGetNumberParsesV2JSON(t *testing.T) {
	client, _ := testClient(t, func(url.Values) string {
		return `{
          "activationId": 9007199254740993,
          "phoneNumber": 628111222333,
          "activationCost": "1.25",
          "currency": "USD",
          "countryCode": "62",
          "canGetAnotherSms": true,
          "activationTime": "2026-08-26 12:34:56",
          "activationOperator": "telkomsel"
        }`
	})

	activation, err := client.GetNumber(context.Background(), NumberRequest{Service: "ni", Country: 6})
	if err != nil {
		t.Fatalf("GetNumber() error = %v", err)
	}
	if activation.ActivationID != "9007199254740993" {
		t.Errorf("ActivationID = %q", activation.ActivationID)
	}
	if activation.PhoneNumber != "+628111222333" || activation.Cost != "1.25" || activation.Currency != "USD" {
		t.Errorf("activation = %#v", activation)
	}
	if !activation.CanGetAnotherSMS || activation.Operator != "telkomsel" {
		t.Errorf("activation = %#v", activation)
	}
	if want := time.Date(2026, 8, 26, 12, 34, 56, 0, time.UTC); !activation.ActivatedAt.Equal(want) {
		t.Errorf("ActivatedAt = %v, want %v", activation.ActivatedAt, want)
	}
}

func TestGetStatusVariants(t *testing.T) {
	tests := []struct {
		body string
		kind StatusKind
		code string
	}{
		{"STATUS_OK:123456", StatusOK, "123456"},
		{"STATUS_WAIT_RETRY:654321", StatusWaitRetry, "654321"},
		{"STATUS_WAIT_CODE", StatusWaitCode, ""},
		{"STATUS_CANCEL", StatusCancel, ""},
		{`{"status":"STATUS_OK","code":"7777"}`, StatusOK, "7777"},
	}
	for _, tt := range tests {
		t.Run(strings.ReplaceAll(tt.body, ":", "_"), func(t *testing.T) {
			client, _ := testClient(t, func(url.Values) string { return tt.body })
			status, err := client.GetStatus(context.Background(), "100")
			if err != nil {
				t.Fatalf("GetStatus() error = %v", err)
			}
			if status.Kind != tt.kind || status.Code != tt.code {
				t.Fatalf("GetStatus() = %#v, want kind=%s code=%q", status, tt.kind, tt.code)
			}
		})
	}
}

func TestCatalogParsing(t *testing.T) {
	responses := map[string]string{
		"getServicesList": `{"status":"success","services":[{"code":"ni","name":"GoPay"},{"code":"kt","name":"KakaoTalk"}]}`,
		"getCountries":    `{"countries":[{"id":6,"rus":"Индонезия","eng":"Indonesia","chn":"印度尼西亚"},{"id":"1003","eng":"Bermuda"}]}`,
		"getPricesV3":     `{"6":{"ni":{"17":{"count":4,"price":"1.25","provider_id":17},"23":{"count":"2","price":1.5,"providerId":"23"}}}}`,
	}
	client, _ := testClient(t, func(values url.Values) string { return responses[values.Get("action")] })

	services, err := client.GetServicesList(context.Background())
	if err != nil {
		t.Fatalf("GetServicesList() error = %v", err)
	}
	if got := []string{services[0].Code, services[1].Code}; !slices.Equal(got, []string{"kt", "ni"}) {
		t.Errorf("service codes = %v", got)
	}
	countries, err := client.GetCountries(context.Background())
	if err != nil {
		t.Fatalf("GetCountries() error = %v", err)
	}
	if len(countries) != 2 || countries[0].ID != 6 || countries[0].Name != "Indonesia" || countries[1].ID != 1003 {
		t.Errorf("countries = %#v", countries)
	}
	prices, err := client.GetPrices(context.Background(), PriceRequest{
		Service: "ni", Country: 6, MinPrice: "1.25", MaxPrice: "1.50", ProviderIDs: []int64{17, 23},
	})
	if err != nil {
		t.Fatalf("GetPrices() error = %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("prices = %#v", prices)
	}
	if prices[0].Country != 6 || prices[0].Service != "ni" || prices[0].ProviderID != 17 || prices[0].Price != "1.25" || prices[0].Count != 4 {
		t.Errorf("prices[0] = %#v", prices[0])
	}
}

func TestGetPricesFallback(t *testing.T) {
	client, requests := testClient(t, func(values url.Values) string {
		switch values.Get("action") {
		case "getPricesV3":
			return "BAD_ACTION"
		case "getPricesV2":
			return "ERROR_SQL"
		default:
			return `{"6":{"ni":{"cost":0.8,"count":10}}}`
		}
	})
	prices, err := client.GetPrices(context.Background(), PriceRequest{Service: "ni", Country: 6})
	if err != nil {
		t.Fatalf("GetPrices() error = %v", err)
	}
	if len(prices) != 1 || prices[0].Price != "0.8" || prices[0].Count != 10 {
		t.Fatalf("GetPrices() = %#v", prices)
	}
	got := make([]string, 0, len(*requests))
	for _, query := range *requests {
		got = append(got, query.Get("action"))
	}
	if !slices.Equal(got, []string{"getPricesV3", "getPricesV2", "getPrices"}) {
		t.Errorf("actions = %v", got)
	}
}

func TestGetPricesV2IntegerPriceAndUnknownJSONFallback(t *testing.T) {
	t.Run("integer price", func(t *testing.T) {
		client, requests := testClient(t, func(values url.Values) string {
			if values.Get("action") == "getPricesV3" {
				return "BAD_ACTION"
			}
			return `{"6":{"ni":{"1":10}}}`
		})
		prices, err := client.GetPrices(context.Background(), PriceRequest{Service: "ni", Country: 6})
		if err != nil {
			t.Fatalf("GetPrices() error = %v", err)
		}
		if len(prices) != 1 || prices[0].Price != "1" || prices[0].Count != 10 {
			t.Fatalf("GetPrices() = %#v", prices)
		}
		if len(*requests) != 2 || (*requests)[1].Get("action") != "getPricesV2" {
			t.Fatalf("requests = %#v", *requests)
		}
	})

	t.Run("unknown JSON", func(t *testing.T) {
		client, requests := testClient(t, func(values url.Values) string {
			switch values.Get("action") {
			case "getPricesV3", "getPricesV2":
				return `{"status":"success"}`
			default:
				return `{"6":{"ni":{"cost":"2","count":3}}}`
			}
		})
		prices, err := client.GetPrices(context.Background(), PriceRequest{Service: "ni", Country: 6})
		if err != nil {
			t.Fatalf("GetPrices() error = %v", err)
		}
		if len(prices) != 1 || prices[0].Price != "2" {
			t.Fatalf("GetPrices() = %#v", prices)
		}
		if len(*requests) != 3 {
			t.Fatalf("request count = %d, want 3", len(*requests))
		}
	})
}

func TestProviderErrorsAndSetStatus(t *testing.T) {
	t.Run("text error", func(t *testing.T) {
		client, _ := testClient(t, func(url.Values) string { return "NO_NUMBERS" })
		_, err := client.GetNumber(context.Background(), NumberRequest{Service: "ni", Country: 6})
		if !IsAPIError(err, "NO_NUMBERS") {
			t.Fatalf("error = %v, want NO_NUMBERS", err)
		}
	})
	t.Run("JSON error", func(t *testing.T) {
		client, _ := testClient(t, func(url.Values) string {
			return `{"status":"error","error":"BAD_KEY","message":"invalid API key"}`
		})
		_, err := client.GetCountries(context.Background())
		if !IsAPIError(err, "BAD_KEY") {
			t.Fatalf("error = %v, want BAD_KEY", err)
		}
	})
	t.Run("JSON message error", func(t *testing.T) {
		client, _ := testClient(t, func(url.Values) string {
			return `{"success":false,"error":{"message":"BAD_SERVICE"}}`
		})
		_, err := client.GetServicesList(context.Background())
		if !IsAPIError(err, "BAD_SERVICE") {
			t.Fatalf("error = %v, want BAD_SERVICE", err)
		}
	})
	t.Run("set statuses", func(t *testing.T) {
		client, requests := testClient(t, func(values url.Values) string {
			switch values.Get("status") {
			case "3":
				return "ACCESS_RETRY_GET"
			case "6":
				return `{"status":"ACCESS_ACTIVATION"}`
			default:
				return "ACCESS_CANCEL"
			}
		})
		for _, status := range []SetStatus{SetStatusRequestAnother, SetStatusComplete, SetStatusCancel} {
			if _, err := client.SetStatus(context.Background(), "100", status); err != nil {
				t.Errorf("SetStatus(%d) error = %v", status, err)
			}
		}
		if len(*requests) != 3 {
			t.Fatalf("request count = %d", len(*requests))
		}
		if _, err := client.SetStatus(context.Background(), "100", 7); err == nil {
			t.Error("SetStatus(7) error = nil")
		}
	})
}

func TestGetNumberDoesNotFallbackWhenOutcomeIsUnknown(t *testing.T) {
	t.Run("transport failure", func(t *testing.T) {
		calls := 0
		client, err := NewClient(Config{
			APIKey: "secret",
			HTTPClient: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("connection reset after request")
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.GetNumber(context.Background(), NumberRequest{Service: "ni", Country: 6})
		if !errors.Is(err, ErrPurchaseUnknown) {
			t.Fatalf("error = %v, want ErrPurchaseUnknown", err)
		}
		if calls != 1 {
			t.Fatalf("request count = %d, want 1", calls)
		}
	})

	t.Run("successful malformed response", func(t *testing.T) {
		client, requests := testClient(t, func(url.Values) string { return `{"success":true}` })
		_, err := client.GetNumber(context.Background(), NumberRequest{Service: "ni", Country: 6})
		if !errors.Is(err, ErrPurchaseUnknown) {
			t.Fatalf("error = %v, want ErrPurchaseUnknown", err)
		}
		if len(*requests) != 1 {
			t.Fatalf("request count = %d, want 1", len(*requests))
		}
	})

	t.Run("empty successful response", func(t *testing.T) {
		client, requests := testClient(t, func(url.Values) string { return "" })
		_, err := client.GetNumber(context.Background(), NumberRequest{Service: "ni", Country: 6})
		if !errors.Is(err, ErrPurchaseUnknown) {
			t.Fatalf("error = %v, want ErrPurchaseUnknown", err)
		}
		if len(*requests) != 1 {
			t.Fatalf("request count = %d, want 1", len(*requests))
		}
	})

	t.Run("semantic rejection", func(t *testing.T) {
		client, requests := testClient(t, func(url.Values) string { return "NO_NUMBERS" })
		_, err := client.GetNumber(context.Background(), NumberRequest{Service: "ni", Country: 6})
		if !IsAPIError(err, "NO_NUMBERS") || errors.Is(err, ErrPurchaseUnknown) {
			t.Fatalf("error = %v, want unambiguous NO_NUMBERS", err)
		}
		if len(*requests) != 1 {
			t.Fatalf("request count = %d, want 1", len(*requests))
		}
	})

	t.Run("generic unsupported must not fall back", func(t *testing.T) {
		client, requests := testClient(t, func(url.Values) string { return "NOT_SUPPORTED" })
		_, err := client.GetNumber(context.Background(), NumberRequest{Service: "ni", Country: 6})
		if !IsAPIError(err, "NOT_SUPPORTED") || errors.Is(err, ErrPurchaseUnknown) {
			t.Fatalf("error = %v, want unambiguous NOT_SUPPORTED", err)
		}
		if len(*requests) != 1 {
			t.Fatalf("request count = %d, want 1", len(*requests))
		}
	})

	t.Run("HTTP outcome", func(t *testing.T) {
		calls := 0
		client, err := NewClient(Config{
			APIKey: "secret",
			HTTPClient: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusGatewayTimeout,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("upstream timeout")),
					Request:    req,
				}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.GetNumber(context.Background(), NumberRequest{Service: "ni", Country: 6})
		if !errors.Is(err, ErrPurchaseUnknown) {
			t.Fatalf("error = %v, want ErrPurchaseUnknown", err)
		}
		if calls != 1 {
			t.Fatalf("request count = %d, want 1", calls)
		}
	})
}

func TestHTTPAndValidationErrors(t *testing.T) {
	t.Run("HTTP", func(t *testing.T) {
		client, err := NewClient(Config{
			APIKey:  "secret",
			BaseURL: "https://example.test",
			HTTPClient: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("slow down")),
					Request:    req,
				}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.GetStatus(context.Background(), "100")
		if !IsAPIError(err, "HTTP_429") {
			t.Fatalf("error = %v, want HTTP_429", err)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		client, err := NewClient(Config{BaseURL: "https://example.test"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.GetServicesList(context.Background())
		if err == nil || !strings.Contains(err.Error(), "API key") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("context", func(t *testing.T) {
		client, err := NewClient(Config{APIKey: "secret", HTTPClient: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = client.GetStatus(ctx, "100")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func testClient(t *testing.T, response func(url.Values) string) (*Client, *[]url.Values) {
	t.Helper()
	requests := make([]url.Values, 0)
	doer := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		requests = append(requests, query)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(response(query))),
			Request:    req,
		}, nil
	})
	client, err := NewClient(Config{APIKey: "secret", BaseURL: "https://example.test", HTTPClient: doer})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client, &requests
}
