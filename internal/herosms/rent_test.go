package herosms

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/smsbower"
)

func TestActivationOffersUseNativeRESTHostAndVerificationType(t *testing.T) {
	var requestURL *url.URL
	var authorization string
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestURL = request.URL
		authorization = request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"data":{"wa":{"6":{"prices":{"default":0.03,"retail":0.04,"min":0.02},
			"counts":{"total":19,"physical":11,"defaultPrice":13},"map":{"0.0300":17}}}},
			"meta":{"filters":{"services":["wa"],"countries":["6"]}}
		}`))
	})

	client := newRentTestClient(t, "http://hero.test/fixture/handler_api.php", handler)
	offers, err := client.Offers(context.Background(), OfferRequest{
		Service: "wa", Country: 6, VerificationType: VerificationCall,
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestURL.Path != "/api/v1/activations/offers/call" {
		t.Fatalf("path = %q, want native REST offers path", requestURL.Path)
	}
	if got := requestURL.Query().Get("services"); got != "wa" {
		t.Fatalf("services = %q", got)
	}
	if got := requestURL.Query().Get("countries"); got != "6" {
		t.Fatalf("countries = %q", got)
	}
	if requestURL.Query().Get("api_key") != "" {
		t.Fatalf("REST request leaked query API key: %s", requestURL.RawQuery)
	}
	if authorization != "ApiKey hero-secret" {
		t.Fatalf("Authorization = %q", authorization)
	}
	if len(offers) != 1 {
		t.Fatalf("offers = %#v", offers)
	}
	offer := offers[0]
	if offer.Service != "wa" || offer.Country != 6 || offer.VerificationType != VerificationCall ||
		offer.Price != "0.03" || offer.RetailPrice != "0.04" || offer.Count != 19 ||
		offer.PhysicalCount != 11 || offer.DefaultPriceCount != 13 || offer.PriceCounts["0.0300"] != 17 {
		t.Fatalf("offer = %#v", offer)
	}
}

func TestRentOffersUseCountryDurationAndExposeOperators(t *testing.T) {
	var query url.Values
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query = request.URL.Query()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"operators":{"2":"three","1":"tmobile"},
			"services":{"tg":{"quantity":2,"price":0.6,"retail_price":0.7},
			"wa":{"quantity":4,"price":1.2,"retail_price":1.3}}
		}`))
	})

	client := newRentTestClient(t, "http://hero.test", handler)
	offers, err := client.Offers(context.Background(), OfferRequest{
		Service: "tg", Country: 6, DurationHours: 72,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertQuery(t, query, map[string]string{
		"api_key": "hero-secret", "action": "getRentServicesAndCountries",
		"country": "6", "duration": "72",
	})
	if len(offers) != 1 {
		t.Fatalf("offers = %#v", offers)
	}
	if got := offers[0]; got.Service != "tg" || got.Country != 6 || got.DurationHours != 72 ||
		got.Price != "0.6" || got.RetailPrice != "0.7" || got.Count != 2 ||
		!reflect.DeepEqual(got.Operators, []string{"three", "tmobile"}) {
		t.Fatalf("offer = %#v", got)
	}
}

func TestRentAvailabilityParsesCountryDurationBuckets(t *testing.T) {
	var query url.Values
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query = request.URL.Query()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"6":{"12":{"count":400,"price":0.4568,"retail_price":0.5},
			"2":{"count":25370,"price":0.18,"retail_price":0.2}},
			"33":{"48":{"count":8,"price":1.25,"retail_price":1.4}}
		}`))
	})

	client := newRentTestClient(t, "http://hero.test", handler)
	offers, err := client.RentAvailability(context.Background(), RentAvailabilityRequest{
		Service: "tg", Operator: "telkomsel", Currency: "840",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertQuery(t, query, map[string]string{
		"api_key": "hero-secret", "action": "serviceCountRent", "service": "tg",
		"operator": "telkomsel", "currency": "840",
	})
	if len(offers) != 3 {
		t.Fatalf("offers = %#v", offers)
	}
	if offers[0].Country != 6 || offers[0].DurationHours != 2 || offers[0].Price != "0.18" ||
		offers[0].Count != 25370 || offers[0].Currency != "840" {
		t.Fatalf("first offer = %#v", offers[0])
	}
	if offers[1].Country != 6 || offers[1].DurationHours != 12 || offers[2].Country != 33 {
		t.Fatalf("offer order = %#v", offers)
	}
}

func TestRentAvailabilityAcceptsOfficialEncodedEmptyObject(t *testing.T) {
	client, err := NewClient(Config{
		APIKey: "hero-secret", BaseURL: "http://hero.test",
		HTTPClient: responseDoer(http.StatusOK, `"{}"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	offers, err := client.RentAvailability(context.Background(), RentAvailabilityRequest{Service: "tg"})
	if err != nil {
		t.Fatal(err)
	}
	if offers == nil || len(offers) != 0 {
		t.Fatalf("offers = %#v, want non-nil empty list", offers)
	}
}

func TestPurchaseOneRegularPreservesActivationEndTime(t *testing.T) {
	var query url.Values
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query = request.URL.Query()
		if query.Get("action") != "getNumberV2" {
			http.Error(writer, "unexpected action", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"activationId":"635468024","serviceCode":"tg","phoneNumber":"+628123456789",
			"activationCost":0.15,"currency":840,"countryCode":6,"countryPhoneCode":62,
			"canGetAnotherSms":true,"activationTime":"2026-02-18T16:11:33Z",
			"activationEndTime":"2026-02-18T16:31:33Z","activationOperator":"telkomsel",
			"verificationType":"sms"
		}`))
	})

	client := newRentTestClient(t, "http://hero.test", handler)
	purchase, err := client.PurchaseOne(context.Background(), PurchaseRequest{
		Service: "tg", Country: 6, MaxPrice: "0.15", Ref: "autosms",
		PhoneException: []string{"6280", "6289"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertQuery(t, query, map[string]string{
		"api_key": "hero-secret", "action": "getNumberV2", "service": "tg", "country": "6",
		"activationType": "0", "maxPrice": "0.15", "ref": "autosms", "phoneException": "6280,6289",
	})
	if purchase.Rent || purchase.ActivationID != "635468024" || purchase.PhoneNumber != "+628123456789" ||
		purchase.Service != "tg" || purchase.Cost != "0.15" || purchase.Currency != "USD" ||
		purchase.Country != 6 || purchase.CountryPhoneCode != "62" || !purchase.CanGetAnotherSMS ||
		purchase.Operator != "telkomsel" || purchase.VerificationType != VerificationSMS {
		t.Fatalf("purchase = %#v", purchase)
	}
	if got, want := purchase.ExpiresAt, time.Date(2026, 2, 18, 16, 31, 33, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", got, want)
	}
	if len(purchase.Raw) == 0 {
		t.Fatal("raw response was not preserved")
	}
}

func TestPurchaseOneCallMapsActivationTypeAndPreservesSelection(t *testing.T) {
	var query url.Values
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query = request.URL.Query()
		writer.Header().Set("Content-Type", "application/json")
		// Omit verificationType deliberately: the client must preserve the
		// requested type when the compatible response does not echo it.
		_, _ = writer.Write([]byte(`{
			"activationId":"call-1","serviceCode":"wa","phoneNumber":"628123456789",
			"activationCost":0.2,"currency":840,"countryCode":6,
			"activationTime":"2026-02-18T16:11:33Z"
		}`))
	})

	client := newRentTestClient(t, "http://hero.test", handler)
	purchase, err := client.PurchaseOne(context.Background(), PurchaseRequest{
		Service: "wa", Country: 6, VerificationType: VerificationCall, Operator: "telkomsel",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertQuery(t, query, map[string]string{
		"api_key": "hero-secret", "action": "getNumberV2", "service": "wa", "country": "6",
		"activationType": "1", "operator": "telkomsel",
	})
	if purchase.Rent || purchase.ActivationID != "call-1" || purchase.PhoneNumber != "+628123456789" ||
		purchase.VerificationType != VerificationCall {
		t.Fatalf("purchase = %#v", purchase)
	}
}

func TestPurchaseOneRentUsesRentActionAndRequiresEndTime(t *testing.T) {
	var query url.Values
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query = request.URL.Query()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"activationId":635468024,"serviceCode":"wa","phoneNumber":"+79584123456",
			"activationCost":12.5,"currency":978,"countryCode":2,"countryPhoneCode":7,
			"canGetAnotherSms":true,"activationTime":"2026-02-18T16:11:33+00:00",
			"activationEndTime":"2026-02-18T18:11:23+00:00","activationOperator":"mts",
			"verificationType":"sms"
		}`))
	})

	client := newRentTestClient(t, "http://hero.test", handler)
	purchase, err := client.PurchaseOne(context.Background(), PurchaseRequest{
		Service: "wa", Country: 2, DurationHours: 2, Operator: "mts", Currency: "978", Ref: "buyer-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertQuery(t, query, map[string]string{
		"api_key": "hero-secret", "action": "getRentNumber", "service": "wa",
		"country": "2", "duration": "2", "operator": "mts", "currency": "978", "ref": "buyer-1",
	})
	if !purchase.Rent || purchase.DurationHours != 2 || purchase.ActivationID != "635468024" ||
		purchase.PhoneNumber != "+79584123456" || purchase.Currency != "EUR" || purchase.Cost != "12.5" ||
		purchase.Service != "wa" || purchase.Operator != "mts" {
		t.Fatalf("purchase = %#v", purchase)
	}
	if got, want := purchase.ExpiresAt, time.Date(2026, 2, 18, 18, 11, 23, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", got, want)
	}
}

func TestPurchaseOneMapsNoNumbersResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantCode   string
	}{
		{name: "HTTP 404", statusCode: http.StatusNotFound, body: `{"message":"gone"}`, wantCode: "HTTP_404"},
		{name: "provider token", statusCode: http.StatusOK, body: "NO_NUMBERS", wantCode: "NO_NUMBERS"},
		{name: "structured code", statusCode: http.StatusNotFound, body: `{"title":"NO_NUMBERS","details":"Numbers Gone"}`, wantCode: "NO_NUMBERS"},
		{name: "structured message", statusCode: http.StatusOK, body: `{"message":"NO_NUMBERS: Numbers Gone"}`, wantCode: "NO_NUMBERS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.statusCode)
				_, _ = writer.Write([]byte(test.body))
			})

			client := newRentTestClient(t, "http://hero.test", handler)
			_, err := client.PurchaseOne(context.Background(), PurchaseRequest{
				Service: "tg", Country: 6, DurationHours: 2,
			})
			if !errors.Is(err, ErrNoNumbers) || !IsNoNumbers(err) {
				t.Fatalf("error = %v, want ErrNoNumbers", err)
			}
			if !smsbower.IsAPIError(err, test.wantCode) {
				t.Fatalf("error = %v, want API code %s", err, test.wantCode)
			}
		})
	}
}

func TestRentPurchaseUnknownProtectsAgainstDuplicatePurchase(t *testing.T) {
	tests := []struct {
		name string
		doer smsbower.HTTPDoer
	}{
		{
			name: "transport failure",
			doer: httpDoerFunc(func(*http.Request) (*http.Response, error) {
				return nil, io.ErrUnexpectedEOF
			}),
		},
		{
			name: "server failure",
			doer: responseDoer(http.StatusInternalServerError, `{"title":"SERVER_ERROR","details":"try later"}`),
		},
		{
			name: "successful but malformed response",
			doer: responseDoer(http.StatusOK, `{"activationId":"allocated-but-phone-missing"}`),
		},
		{
			name: "successful response missing expiry",
			doer: responseDoer(http.StatusOK, `{"activationId":"allocated","phoneNumber":"6281234"}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient(Config{
				APIKey: "hero-secret", BaseURL: "http://hero.test", HTTPClient: test.doer,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.PurchaseOne(context.Background(), PurchaseRequest{
				Service: "tg", Country: 6, DurationHours: 2,
			})
			if !errors.Is(err, smsbower.ErrPurchaseUnknown) {
				t.Fatalf("error = %v, want ErrPurchaseUnknown", err)
			}
		})
	}
}

func TestRegularPurchaseUnknownProtectsAgainstDuplicatePurchase(t *testing.T) {
	for _, doer := range []smsbower.HTTPDoer{
		responseDoer(http.StatusInternalServerError, `{"title":"SERVER_ERROR","details":"try later"}`),
		responseDoer(http.StatusOK, `{"activationId":"allocated-but-phone-missing"}`),
	} {
		client, err := NewClient(Config{
			APIKey: "hero-secret", BaseURL: "http://hero.test", HTTPClient: doer,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.PurchaseOne(context.Background(), PurchaseRequest{
			Service: "tg", Country: 6, VerificationType: VerificationSMS,
		})
		if !errors.Is(err, smsbower.ErrPurchaseUnknown) {
			t.Fatalf("error = %v, want ErrPurchaseUnknown", err)
		}
	}
}

func TestRentPurchaseRejectsCallWithoutSendingRequest(t *testing.T) {
	client := newRentTestClient(t, "http://hero.test", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid rental verification type should not send a request")
	}))
	_, err := client.PurchaseOne(context.Background(), PurchaseRequest{
		Service: "tg", Country: 6, DurationHours: 2, VerificationType: VerificationCall,
	})
	if err == nil || !strings.Contains(err.Error(), "SMS verification only") {
		t.Fatalf("error = %v", err)
	}
}

func TestRentPurchaseBusinessRejectionIsConclusive(t *testing.T) {
	client, err := NewClient(Config{
		APIKey: "hero-secret", BaseURL: "http://hero.test",
		HTTPClient: responseDoer(http.StatusBadRequest, `{"title":"BAD_SERVICE","details":"unknown service"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PurchaseOne(context.Background(), PurchaseRequest{
		Service: "missing", Country: 6, DurationHours: 2,
	})
	if !smsbower.IsAPIError(err, "BAD_SERVICE") || errors.Is(err, smsbower.ErrPurchaseUnknown) {
		t.Fatalf("error = %v, want conclusive BAD_SERVICE", err)
	}
}

func TestHandlerTransportErrorRedactsAPIKey(t *testing.T) {
	const apiKey = "hero-transport-secret"
	client, err := NewClient(Config{
		APIKey: apiKey, BaseURL: "https://hero.test",
		HTTPClient: httpDoerFunc(func(request *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Get", URL: request.URL.String(), Err: errors.New("fixture dial failure")}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RentAvailability(context.Background(), RentAvailabilityRequest{Service: "tg"})
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

func TestGetMessagesUsesRentListAndRegularStatusFallback(t *testing.T) {
	var actions []string
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		action := request.URL.Query().Get("action")
		actions = append(actions, action)
		switch action {
		case "getAllSms":
			_, _ = writer.Write([]byte(`{"data":[
				{"id":"2","phoneFrom":"Telegram","code":"2222","text":"code 2222","service":"tg","date":"2026-02-16T12:37:59+03:00","type":"sms"},
				{"id":"1","phoneFrom":"Telegram","code":null,"text":null,"service":"tg","date":"2026-02-16T12:36:59+03:00","type":"call"}
			],"meta":{"total":2}}`))
		case "getStatus":
			if request.URL.Query().Get("id") == "short-retry" {
				_, _ = writer.Write([]byte("STATUS_WAIT_RETRY:4321"))
				return
			}
			_, _ = writer.Write([]byte("STATUS_OK:4321"))
		default:
			http.Error(writer, "unexpected action", http.StatusBadRequest)
		}
	})

	client := newRentTestClient(t, "http://hero.test", handler)
	rentMessages, err := client.GetMessages(context.Background(), "rent-1", true)
	if err != nil {
		t.Fatal(err)
	}
	regularMessages, err := client.GetMessages(context.Background(), "short-1", false)
	if err != nil {
		t.Fatal(err)
	}
	retryMessages, err := client.GetMessages(context.Background(), "short-retry", false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actions, []string{"getAllSms", "getStatus", "getStatus"}) {
		t.Fatalf("actions = %v", actions)
	}
	if len(rentMessages) != 2 || rentMessages[0].ID != "1" || rentMessages[0].Code != "" ||
		rentMessages[0].VerificationType != VerificationCall || rentMessages[1].Code != "2222" {
		t.Fatalf("rent messages = %#v", rentMessages)
	}
	if len(regularMessages) != 1 || regularMessages[0].Code != "4321" || regularMessages[0].VerificationType != VerificationSMS {
		t.Fatalf("regular messages = %#v", regularMessages)
	}
	if len(retryMessages) != 0 {
		t.Fatalf("wait-retry messages = %#v, want none", retryMessages)
	}
}

func TestFinishAndCancelSelectLifecycleEndpoints(t *testing.T) {
	type lifecycleCall struct {
		action string
		id     string
		status string
	}
	var calls []lifecycleCall
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		calls = append(calls, lifecycleCall{action: query.Get("action"), id: query.Get("id"), status: query.Get("status")})
		if query.Get("action") == "finishActivation" || query.Get("action") == "cancelActivation" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = writer.Write([]byte("ACCESS_ACTIVATION"))
	})

	client := newRentTestClient(t, "http://hero.test", handler)
	for _, operation := range []func() error{
		func() error { return client.Finish(context.Background(), "rent-finish", true) },
		func() error { return client.Cancel(context.Background(), "rent-cancel", true) },
		func() error { return client.Finish(context.Background(), "short-finish", false) },
		func() error { return client.Cancel(context.Background(), "short-cancel", false) },
		func() error { return client.RequestAnother(context.Background(), "short-another") },
	} {
		if err := operation(); err != nil {
			t.Fatal(err)
		}
	}
	want := []lifecycleCall{
		{action: "finishActivation", id: "rent-finish"},
		{action: "cancelActivation", id: "rent-cancel"},
		{action: "setStatus", id: "short-finish", status: "6"},
		{action: "setStatus", id: "short-cancel", status: "8"},
		{action: "setStatus", id: "short-another", status: "3"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestRentFinalizersMapMissingActivationToIdempotentCode(t *testing.T) {
	for _, body := range []string{
		`{"message":"missing"}`,
		`{"title":"NOT_FOUND","details":"Activation Not Found"}`,
	} {
		client, err := NewClient(Config{
			APIKey: "hero-secret", BaseURL: "http://hero.test",
			HTTPClient: responseDoer(http.StatusNotFound, body),
		})
		if err != nil {
			t.Fatal(err)
		}
		err = client.Finish(context.Background(), "rent-missing", true)
		if !smsbower.IsAPIError(err, "NO_ACTIVATION") {
			t.Fatalf("body %s: error = %v, want NO_ACTIVATION", body, err)
		}
		var apiErr *smsbower.APIError
		if !errors.As(err, &apiErr) || apiErr.Action != "finishActivation" || apiErr.Raw != body {
			t.Fatalf("body %s: mapped error lost metadata: %#v", body, apiErr)
		}
	}
}

func TestVerificationTypeValidation(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid verification type should not send a request")
	})
	client := newRentTestClient(t, "http://hero.test", handler)
	_, err := client.Offers(context.Background(), OfferRequest{
		Service: "tg", Country: 6, VerificationType: "email",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported verification type") {
		t.Fatalf("error = %v", err)
	}
}

func newRentTestClient(t *testing.T, baseURL string, handler http.Handler) *Client {
	t.Helper()
	client, err := NewClient(Config{
		APIKey: "hero-secret", BaseURL: baseURL,
		HTTPClient: httpDoerFunc(func(request *http.Request) (*http.Response, error) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			response := recorder.Result()
			response.Request = request
			return response, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func responseDoer(status int, body string) smsbower.HTTPDoer {
	return httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		recorder.WriteHeader(status)
		_, _ = recorder.WriteString(body)
		response := recorder.Result()
		response.Request = request
		return response, nil
	})
}

func assertQuery(t *testing.T, query url.Values, expected map[string]string) {
	t.Helper()
	for key, want := range expected {
		if got := query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
}
