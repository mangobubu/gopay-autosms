package smsbower

import (
	"context"
	"encoding/json"
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
		"getCountries":    `{"countries":[{"id":6,"rus":"Индонезия","eng":"Indonesia","chn":"印度尼西亚","iso":" id "},{"id":"1003","eng":"Bermuda","countryCode":"BM"},{"id":999,"eng":"Example","countryCode":"62"}]}`,
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
	if len(countries) != 3 || countries[0].ID != 6 || countries[0].Name != "Indonesia" || countries[2].ID != 1003 {
		t.Errorf("countries = %#v", countries)
	}
	if countries[0].ISOCode != "ID" || countries[1].ISOCode != "" || countries[2].ISOCode != "BM" {
		t.Errorf("country ISO codes = %q, %q, %q", countries[0].ISOCode, countries[1].ISOCode, countries[2].ISOCode)
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

func TestCountryISOCodeFallback(t *testing.T) {
	if len(countryISOByID) != 205 {
		t.Fatalf("country ISO catalogue has %d entries, want 205", len(countryISOByID))
	}
	tests := map[int]string{
		4: "PH", 6: "ID", 12: "US", 16: "GB", 18: "CD",
		139: "NE", 150: "CG", 187: "US", 203: "XK", 204: "NU",
	}
	for id, want := range tests {
		if got := countryISOCode(id); got != want {
			t.Errorf("countryISOCode(%d) = %q, want %q", id, got, want)
		}
	}
	for _, id := range []int{-1, 205, 360, 999} {
		if got := countryISOCode(id); got != "" {
			t.Errorf("countryISOCode(%d) = %q, want empty", id, got)
		}
	}
}

func TestParseCountriesAddsISOFromProviderID(t *testing.T) {
	countries, err := parseCountries("getCountries", []byte(`{
		"countries": [
			{"id": 12, "eng": "USA (virtual)"},
			{"id": 18, "eng": "Congo (Dem. Republic)"},
			{"id": 204, "eng": "Niue"}
		]
	}`))
	if err != nil {
		t.Fatalf("parseCountries() error = %v", err)
	}
	want := []string{"US", "CD", "NU"}
	for index, country := range countries {
		if country.ISOCode != want[index] {
			t.Errorf("countries[%d].ISOCode = %q, want %q", index, country.ISOCode, want[index])
		}
	}
}

func TestParseCountriesAcceptsISOAliasesAndRejectsCallingCodes(t *testing.T) {
	countries, err := parseCountries("getCountries", []byte(`{
		"countries": [
			{"id": 998, "eng": "Code alias", "code": " zz "},
			{"id": 999, "eng": "Country alias", "country": "PH"},
			{"id": 1000, "eng": "Calling code", "countryCode": "62"}
		]
	}`))
	if err != nil {
		t.Fatalf("parseCountries() error = %v", err)
	}
	want := []string{"ZZ", "PH", ""}
	for index, country := range countries {
		if country.ISOCode != want[index] {
			t.Errorf("countries[%d].ISOCode = %q, want %q", index, country.ISOCode, want[index])
		}
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

func TestGetPricesParsesProviderTiersAndSortsByNumericPrice(t *testing.T) {
	client, _ := testClient(t, func(values url.Values) string {
		if values.Get("action") != "getPricesV3" {
			t.Fatalf("action = %q, want getPricesV3", values.Get("action"))
		}
		return `{
			"6": {
				"Bronze": {
					"ni": {
						"5": {"count": 2, "price": "2", "provider_id": 5}
					}
				},
				"ni": {
					"Gold": {
						"7": {"count": 1, "price": "10", "provider_id": 7, "rank": 1}
					},
					"3": {"count": 3, "price": "1,25", "provider_id": 3, "level": "SILVER"}
				}
			}
		}`
	})

	prices, err := client.GetPrices(context.Background(), PriceRequest{Service: "ni", Country: 6})
	if err != nil {
		t.Fatalf("GetPrices() error = %v", err)
	}
	if len(prices) != 3 {
		t.Fatalf("prices = %#v", prices)
	}
	wantPrices := []string{"1,25", "2", "10"}
	wantProviders := []int64{3, 5, 7}
	wantTiers := []string{"Silver", "Bronze", "Gold"}
	for index := range prices {
		if prices[index].Price != wantPrices[index] || prices[index].ProviderID != wantProviders[index] || prices[index].Tier != wantTiers[index] {
			t.Errorf("prices[%d] = %#v, want price=%q provider=%d tier=%q", index, prices[index], wantPrices[index], wantProviders[index], wantTiers[index])
		}
	}

	encoded, err := json.Marshal(prices)
	if err != nil {
		t.Fatalf("json.Marshal(prices) error = %v", err)
	}
	for _, tier := range wantTiers {
		if !strings.Contains(string(encoded), `"tier":"`+tier+`"`) {
			t.Errorf("JSON %s does not expose tier %q", encoded, tier)
		}
	}
}

func TestParsePricesSortsUnknownPricesAfterNumericPrices(t *testing.T) {
	prices, err := parsePrices("getPricesV3", []byte(`{
		"6": {
			"ni": {
				"3": {"price": "unknown", "count": 1, "provider_id": 3},
				"2": {"price": "10", "count": 1, "provider_id": 2},
				"1": {"price": "2", "count": 1, "provider_id": 1}
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parsePrices() error = %v", err)
	}
	if got := []string{prices[0].Price, prices[1].Price, prices[2].Price}; !slices.Equal(got, []string{"2", "10", "unknown"}) {
		t.Fatalf("price order = %v, want numeric prices first from low to high", got)
	}
}

func TestParsePricesAcceptsNumericAndObjectProviderRanks(t *testing.T) {
	prices, err := parsePrices("getPricesV3", []byte(`{
		"6": {
			"ni": {
				"101": {"price": "1", "count": 1, "provider_id": 101, "rank": 1},
				"102": {"price": "2", "count": 1, "provider_id": 102, "tier": "", "provider_rank": {"id": "2"}},
				"103": {"price": "3", "count": 1, "provider_id": 103, "tier": null, "rank": {"id": 3, "description": "bronze"}}
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parsePrices() error = %v", err)
	}
	if len(prices) != 3 {
		t.Fatalf("prices = %#v, want 3 provider-ranked offers", prices)
	}
	if got := []string{prices[0].Tier, prices[1].Tier, prices[2].Tier}; !slices.Equal(got, []string{"Gold", "Silver", "Bronze"}) {
		t.Fatalf("provider tiers = %v, want numeric/object ranks normalized", got)
	}
}

func TestProviderErrorsAndSetStatus(t *testing.T) {
	t.Run("text error", func(t *testing.T) {
		client, _ := testClient(t, func(url.Values) string { return "NO_NUMBERS" })
		_, err := client.GetNumber(context.Background(), NumberRequest{Service: "ni", Country: 6})
		if !IsAPIError(err, "NO_NUMBERS") {
			t.Fatalf("error = %v, want NO_NUMBERS", err)
		}
		if got := err.Error(); !strings.HasPrefix(got, "smsbower getNumberV2: NO_NUMBERS") {
			t.Fatalf("error text = %q, want default smsbower prefix", got)
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

func TestTransportErrorRedactsAPIKey(t *testing.T) {
	const apiKey = "transport-secret-key"
	client, err := NewClient(Config{
		APIKey:  apiKey,
		BaseURL: "https://example.test",
		HTTPClient: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("fixture dial failure")}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetStatus(context.Background(), "100")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("transport error exposed API key: %v", err)
	}
	if !strings.Contains(err.Error(), "api_key=REDACTED") {
		t.Fatalf("transport error did not retain a redacted URL: %v", err)
	}
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
