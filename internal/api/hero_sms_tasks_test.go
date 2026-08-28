package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/herosms"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type heroSMSCatalogFixture struct {
	services         []smsbower.Service
	countries        []smsbower.Country
	rentOffers       []herosms.Offer
	offers           map[herosms.VerificationType][]herosms.Offer
	serviceErr       error
	countryErr       error
	rentErr          error
	offerErr         error
	rentRequests     []herosms.RentAvailabilityRequest
	offerRequests    []herosms.OfferRequest
	serviceListCalls int
	countryListCalls int
}

func (fixture *heroSMSCatalogFixture) GetServicesList(context.Context) ([]smsbower.Service, error) {
	fixture.serviceListCalls++
	return fixture.services, fixture.serviceErr
}

func (fixture *heroSMSCatalogFixture) GetCountries(context.Context) ([]smsbower.Country, error) {
	fixture.countryListCalls++
	return fixture.countries, fixture.countryErr
}

func (fixture *heroSMSCatalogFixture) RentAvailability(_ context.Context, request herosms.RentAvailabilityRequest) ([]herosms.Offer, error) {
	fixture.rentRequests = append(fixture.rentRequests, request)
	return fixture.rentOffers, fixture.rentErr
}

func (fixture *heroSMSCatalogFixture) Offers(_ context.Context, request herosms.OfferRequest) ([]herosms.Offer, error) {
	fixture.offerRequests = append(fixture.offerRequests, request)
	return fixture.offers[request.VerificationType], fixture.offerErr
}

type heroSMSTaskControllerFixture struct {
	created      []domain.HeroSMSNumberTask
	listed       []domain.HeroSMSNumberTask
	actionTask   domain.HeroSMSNumberTask
	createErr    error
	listErr      error
	actionErr    error
	createInputs []HeroSMSCreateTasksInput
	listPages    []storage.Page
	startIDs     []int64
	stopIDs      []int64
}

func (fixture *heroSMSTaskControllerFixture) CreateTasks(_ context.Context, input HeroSMSCreateTasksInput) ([]domain.HeroSMSNumberTask, error) {
	fixture.createInputs = append(fixture.createInputs, input)
	return fixture.created, fixture.createErr
}

func (fixture *heroSMSTaskControllerFixture) ListTasks(_ context.Context, page storage.Page) ([]domain.HeroSMSNumberTask, error) {
	fixture.listPages = append(fixture.listPages, page)
	return fixture.listed, fixture.listErr
}

func (fixture *heroSMSTaskControllerFixture) GetTask(context.Context, int64) (domain.HeroSMSNumberTask, error) {
	return fixture.actionTask, fixture.actionErr
}

func (fixture *heroSMSTaskControllerFixture) StartTask(_ context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	fixture.startIDs = append(fixture.startIDs, id)
	return fixture.actionTask, fixture.actionErr
}

func (fixture *heroSMSTaskControllerFixture) StopTask(_ context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	fixture.stopIDs = append(fixture.stopIDs, id)
	return fixture.actionTask, fixture.actionErr
}

func newHeroSMSAPIRouter(handler *HeroSMSAPI) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler.RegisterHeroSMSRoutes(router.Group("/api"))
	return router
}

func heroSMSAPIRequest(t *testing.T, router http.Handler, method, path, body, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func decodeHeroSMSBody[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	return value
}

func TestHeroSMSCatalogInitialAndServiceDurationExpansion(t *testing.T) {
	fixture := &heroSMSCatalogFixture{
		services:  []smsbower.Service{{Code: "wa", Name: "WhatsApp"}},
		countries: []smsbower.Country{{ID: 6, Name: "Indonesia"}, {ID: 12, Name: "United States"}},
		rentOffers: []herosms.Offer{
			{Service: "wa", Country: 6, DurationHours: 24, VerificationType: herosms.VerificationSMS, Price: "0.61", Count: 3},
			{Service: "wa", Country: 6, DurationHours: 48, VerificationType: herosms.VerificationSMS, Price: "1.10", Count: 0},
		},
	}
	handler := NewHeroSMSAPI(nil, nil)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return fixture, nil }
	router := newHeroSMSAPIRouter(handler)

	response := heroSMSAPIRequest(t, router, http.MethodGet, "/api/hero-sms/catalog", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("initial status = %d; body = %s", response.Code, response.Body.String())
	}
	initial := decodeHeroSMSBody[heroSMSCatalogResponse](t, response)
	if len(initial.Services) != 1 || initial.Services[0].Code != "wa" {
		t.Fatalf("initial services = %#v", initial.Services)
	}
	if len(initial.Countries) != 2 || len(initial.Offers) != 0 || len(initial.Durations) != 0 {
		t.Fatalf("initial catalog = %#v", initial)
	}
	if len(fixture.rentRequests) != 0 || len(fixture.offerRequests) != 0 {
		t.Fatalf("initial catalog called dependent APIs: rent=%d offers=%d", len(fixture.rentRequests), len(fixture.offerRequests))
	}

	response = heroSMSAPIRequest(t, router, http.MethodGet, "/api/hero-sms/catalog?service=wa", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("service status = %d; body = %s", response.Code, response.Body.String())
	}
	serviceCatalog := decodeHeroSMSBody[heroSMSCatalogResponse](t, response)
	if len(fixture.rentRequests) != 1 || fixture.rentRequests[0].Service != "wa" {
		t.Fatalf("rent requests = %#v", fixture.rentRequests)
	}
	if len(serviceCatalog.Durations) != 2 || serviceCatalog.Durations[0].DurationHours != 24 || serviceCatalog.Durations[0].Label != "1 天" {
		t.Fatalf("durations = %#v", serviceCatalog.Durations)
	}
	if len(serviceCatalog.VerificationTypes) != 2 || serviceCatalog.VerificationTypes[0].Value != "sms" || serviceCatalog.VerificationTypes[1].Value != "call" {
		t.Fatalf("verification types = %#v", serviceCatalog.VerificationTypes)
	}
	if len(serviceCatalog.Offers) != 2 || serviceCatalog.Offers[0].RefundableWindowSeconds != 1200 || serviceCatalog.Offers[1].RefundableWindowSeconds != 1200 {
		t.Fatalf("rent offers must expose 20-minute refund window: %#v", serviceCatalog.Offers)
	}
	if serviceCatalog.Offers[1].Stock != 0 || serviceCatalog.Offers[1].Available {
		t.Fatalf("out-of-stock rent offer = %#v", serviceCatalog.Offers[1])
	}
}

func TestHeroSMSCatalogExactOffersAndNoInventoryPlaceholder(t *testing.T) {
	fixture := &heroSMSCatalogFixture{
		services:  []smsbower.Service{{Code: "wa", Name: "WhatsApp"}},
		countries: []smsbower.Country{{ID: 6, Name: "Indonesia"}},
		offers: map[herosms.VerificationType][]herosms.Offer{
			herosms.VerificationSMS: nil,
			herosms.VerificationCall: {{
				Service: "wa", Country: 6, VerificationType: herosms.VerificationCall,
				Price: "0.80", Count: 2,
			}},
		},
	}
	handler := NewHeroSMSAPI(nil, nil)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return fixture, nil }
	router := newHeroSMSAPIRouter(handler)

	response := heroSMSAPIRequest(t, router, http.MethodGet, "/api/hero-sms/catalog?service=wa&country=6", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	catalog := decodeHeroSMSBody[heroSMSCatalogResponse](t, response)
	if len(fixture.offerRequests) != 2 || fixture.offerRequests[0].VerificationType != herosms.VerificationSMS || fixture.offerRequests[1].VerificationType != herosms.VerificationCall {
		t.Fatalf("offer requests = %#v", fixture.offerRequests)
	}
	if len(catalog.Offers) != 2 {
		t.Fatalf("offers = %#v", catalog.Offers)
	}
	if catalog.Offers[0].VerificationType != herosms.VerificationSMS || catalog.Offers[0].Stock != 0 || catalog.Offers[0].Available {
		t.Fatalf("sms no-inventory placeholder = %#v", catalog.Offers[0])
	}
	if catalog.Offers[1].VerificationType != herosms.VerificationCall || catalog.Offers[1].Stock != 2 || !catalog.Offers[1].Available {
		t.Fatalf("call exact offer = %#v", catalog.Offers[1])
	}
	if catalog.Message != "" {
		t.Fatalf("message = %q, want empty while exact offer is available", catalog.Message)
	}

	fixture.offerRequests = nil
	fixture.offers[herosms.VerificationSMS] = nil
	response = heroSMSAPIRequest(t, router, http.MethodGet, "/api/hero-sms/catalog?service=wa&country=6&verification_type=sms", "", "")
	catalog = decodeHeroSMSBody[heroSMSCatalogResponse](t, response)
	if len(catalog.Offers) != 1 || catalog.Offers[0].Stock != 0 {
		t.Fatalf("single no-inventory offer = %#v", catalog.Offers)
	}
	if !strings.Contains(catalog.Message, "暂无可用号码") {
		t.Fatalf("no-inventory message = %q", catalog.Message)
	}
}

func TestHeroSMSCatalogExactRentalUsesSMSOnly(t *testing.T) {
	fixture := &heroSMSCatalogFixture{
		services:  []smsbower.Service{{Code: "wa", Name: "WhatsApp"}},
		countries: []smsbower.Country{{ID: 6, Name: "Indonesia"}},
		rentOffers: []herosms.Offer{{
			Service: "wa", Country: 6, DurationHours: 24,
			VerificationType: herosms.VerificationSMS, Price: "0.61", Count: 0,
		}},
		offers: map[herosms.VerificationType][]herosms.Offer{
			herosms.VerificationSMS: {{
				Service: "wa", Country: 6, DurationHours: 24,
				VerificationType: herosms.VerificationSMS, Price: "0.61", Count: 4,
			}},
		},
	}
	handler := NewHeroSMSAPI(nil, nil)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return fixture, nil }
	router := newHeroSMSAPIRouter(handler)
	response := heroSMSAPIRequest(t, router, http.MethodGet,
		"/api/hero-sms/catalog?service=wa&country=6&duration_hours=24", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	if len(fixture.offerRequests) != 1 || fixture.offerRequests[0].VerificationType != herosms.VerificationSMS || fixture.offerRequests[0].DurationHours != 24 {
		t.Fatalf("rental offer requests = %#v", fixture.offerRequests)
	}
	catalog := decodeHeroSMSBody[heroSMSCatalogResponse](t, response)
	if len(catalog.Offers) != 1 || catalog.Offers[0].ProductKind != domain.HeroSMSProductRent || catalog.Offers[0].Stock != 4 {
		t.Fatalf("rental offers = %#v", catalog.Offers)
	}
}

func TestHeroSMSCatalogCallSkipsRentalAvailability(t *testing.T) {
	fixture := &heroSMSCatalogFixture{
		services:  []smsbower.Service{{Code: "wa", Name: "WhatsApp"}},
		countries: []smsbower.Country{{ID: 6, Name: "Indonesia"}},
		rentErr:   errors.New("rental catalogue unavailable"),
		offers: map[herosms.VerificationType][]herosms.Offer{
			herosms.VerificationCall: {{
				Service: "wa", Country: 6, VerificationType: herosms.VerificationCall,
				Price: "0.8", Count: 2,
			}},
		},
	}
	handler := NewHeroSMSAPI(nil, nil)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return fixture, nil }
	router := newHeroSMSAPIRouter(handler)
	response := heroSMSAPIRequest(t, router, http.MethodGet,
		"/api/hero-sms/catalog?service=wa&country=6&verification_type=call", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	if len(fixture.rentRequests) != 0 {
		t.Fatalf("call catalogue made rental requests: %#v", fixture.rentRequests)
	}
	catalog := decodeHeroSMSBody[heroSMSCatalogResponse](t, response)
	if len(catalog.Offers) != 1 || !catalog.Offers[0].Available || catalog.Offers[0].VerificationType != herosms.VerificationCall {
		t.Fatalf("call offers = %#v", catalog.Offers)
	}
}

func TestHeroSMSCatalogBadRentServiceStillReturnsActivationOffers(t *testing.T) {
	fixture := &heroSMSCatalogFixture{
		services:  []smsbower.Service{{Code: "wa", Name: "WhatsApp"}},
		countries: []smsbower.Country{{ID: 6, Name: "Indonesia"}},
		rentErr: &smsbower.APIError{
			Provider: "HeroSMS", Action: "serviceCountRent", Code: "BAD_SERVICE",
		},
		offers: map[herosms.VerificationType][]herosms.Offer{
			herosms.VerificationSMS: {{
				Service: "wa", Country: 6, VerificationType: herosms.VerificationSMS,
				Price: "0.45", Count: 3,
			}},
		},
	}
	handler := NewHeroSMSAPI(nil, nil)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return fixture, nil }
	router := newHeroSMSAPIRouter(handler)

	response := heroSMSAPIRequest(t, router, http.MethodGet,
		"/api/hero-sms/catalog?service=wa&country=6&verification_type=sms", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	if len(fixture.rentRequests) != 1 || len(fixture.offerRequests) != 1 {
		t.Fatalf("requests: rent=%#v activation=%#v", fixture.rentRequests, fixture.offerRequests)
	}
	catalog := decodeHeroSMSBody[heroSMSCatalogResponse](t, response)
	if len(catalog.Durations) != 0 {
		t.Fatalf("durations = %#v, want no rental products", catalog.Durations)
	}
	if len(catalog.Offers) != 1 || catalog.Offers[0].ProductKind != domain.HeroSMSProductActivation ||
		!catalog.Offers[0].Available || catalog.Offers[0].Stock != 3 {
		t.Fatalf("activation offers = %#v", catalog.Offers)
	}
	if catalog.Message != "" {
		t.Fatalf("message = %q while activation inventory is available", catalog.Message)
	}
}

func TestHeroSMSCatalogOtherRentalErrorStillFails(t *testing.T) {
	fixture := &heroSMSCatalogFixture{
		services:  []smsbower.Service{{Code: "wa", Name: "WhatsApp"}},
		countries: []smsbower.Country{{ID: 6, Name: "Indonesia"}},
		rentErr:   errors.New("rental catalogue transport failure"),
		offers: map[herosms.VerificationType][]herosms.Offer{
			herosms.VerificationSMS: {{
				Service: "wa", Country: 6, VerificationType: herosms.VerificationSMS,
				Price: "0.45", Count: 3,
			}},
		},
	}
	handler := NewHeroSMSAPI(nil, nil)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return fixture, nil }
	router := newHeroSMSAPIRouter(handler)

	response := heroSMSAPIRequest(t, router, http.MethodGet,
		"/api/hero-sms/catalog?service=wa&country=6&verification_type=sms", "", "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", response.Code, response.Body.String())
	}
	if len(fixture.rentRequests) != 1 || len(fixture.offerRequests) != 0 {
		t.Fatalf("requests after rental failure: rent=%#v activation=%#v", fixture.rentRequests, fixture.offerRequests)
	}
}

func TestHeroSMSCatalogDoesNotClaimNoInventoryWhenRentalIsAvailable(t *testing.T) {
	fixture := &heroSMSCatalogFixture{
		services:  []smsbower.Service{{Code: "wa", Name: "WhatsApp"}},
		countries: []smsbower.Country{{ID: 6, Name: "Indonesia"}},
		rentOffers: []herosms.Offer{{
			Service: "wa", Country: 6, DurationHours: 24,
			VerificationType: herosms.VerificationSMS, Price: "0.61", Count: 4,
		}},
		offers: map[herosms.VerificationType][]herosms.Offer{
			herosms.VerificationSMS:  nil,
			herosms.VerificationCall: nil,
		},
	}
	handler := NewHeroSMSAPI(nil, nil)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return fixture, nil }
	router := newHeroSMSAPIRouter(handler)
	response := heroSMSAPIRequest(t, router, http.MethodGet,
		"/api/hero-sms/catalog?service=wa&country=6", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	catalog := decodeHeroSMSBody[heroSMSCatalogResponse](t, response)
	if catalog.Message != "" {
		t.Fatalf("message = %q while rental inventory is available; offers=%#v", catalog.Message, catalog.Offers)
	}
}

func TestHeroSMSCatalogRejectsInvalidFilterCombinations(t *testing.T) {
	fixture := &heroSMSCatalogFixture{}
	handler := NewHeroSMSAPI(nil, nil)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return fixture, nil }
	router := newHeroSMSAPIRouter(handler)

	for _, path := range []string{
		"/api/hero-sms/catalog?country=6",
		"/api/hero-sms/catalog?service=wa&verification_type=sms",
		"/api/hero-sms/catalog?service=wa&country=bad",
		"/api/hero-sms/catalog?service=wa&country=6&verification_type=email",
		"/api/hero-sms/catalog?service=wa&country=6&duration_hours=-1",
		"/api/hero-sms/catalog?service=wa&country=6&verification_type=call&duration_hours=24",
	} {
		response := heroSMSAPIRequest(t, router, http.MethodGet, path, "", "")
		if response.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400; body = %s", path, response.Code, response.Body.String())
		}
	}
	if fixture.serviceListCalls != 0 || fixture.countryListCalls != 0 {
		t.Fatalf("invalid filters reached provider: services=%d countries=%d", fixture.serviceListCalls, fixture.countryListCalls)
	}
}

func TestHeroSMSCreateTasksValidatesAndForwardsIdempotency(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	controller := &heroSMSTaskControllerFixture{created: []domain.HeroSMSNumberTask{
		{ID: 101, Status: domain.HeroSMSTaskWaitingNumber, ProductKind: domain.HeroSMSProductRent, ServiceCode: "wa", CountryCode: "6", CreatedAt: now},
		{ID: 102, Status: domain.HeroSMSTaskWaitingNumber, ProductKind: domain.HeroSMSProductRent, ServiceCode: "wa", CountryCode: "6", CreatedAt: now},
	}}
	handler := NewHeroSMSAPI(nil, controller)
	handler.now = func() time.Time { return now }
	client := &heroSMSCatalogFixture{offers: map[herosms.VerificationType][]herosms.Offer{
		herosms.VerificationSMS: {{
			Service: "wa", Country: 6, DurationHours: 24, VerificationType: herosms.VerificationSMS,
			Price: "0.6125", Currency: "USD", Count: 5, Operators: []string{"telkomsel"},
		}},
	}}
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return client, nil }
	router := newHeroSMSAPIRouter(handler)
	body := `{"service":"wa","country":"6","verification_type":"sms","duration_hours":24,"quantity":2}`

	for _, key := range []string{"", "bad key"} {
		response := heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks", body, key)
		if response.Code != http.StatusBadRequest {
			t.Errorf("idempotency key %q status = %d, want 400; body = %s", key, response.Code, response.Body.String())
		}
	}
	response := heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks", body, "purchase-uuid-001")
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	if len(controller.createInputs) != 1 {
		t.Fatalf("create calls = %d, want 1", len(controller.createInputs))
	}
	input := controller.createInputs[0]
	if input.SubmissionID != "purchase-uuid-001" || input.ProductKind != domain.HeroSMSProductRent || input.ServiceCode != "wa" || input.CountryCode != "6" || input.Quantity != 2 {
		t.Fatalf("create input = %#v", input)
	}
	if input.MaxPriceAmount != "0.6125" || input.Currency != "USD" || input.Operator != "" {
		t.Fatalf("offer-enriched create input = %#v", input)
	}
	if input.DurationHours == nil || *input.DurationHours != 24 {
		t.Fatalf("duration = %#v", input.DurationHours)
	}
	envelope := decodeHeroSMSBody[heroSMSTasksResponse](t, response)
	if len(envelope.Tasks) != 2 || !envelope.Tasks[0].Running || !envelope.Tasks[0].Capabilities.Stop {
		t.Fatalf("created task envelope = %#v", envelope)
	}
	if !envelope.ServerNow.Equal(now) {
		t.Fatalf("server_now = %s, want %s", envelope.ServerNow, now)
	}
	// Catalogue metadata is deliberately outside the durable submission
	// fingerprint. Replaying the same key still reaches the manager when the
	// price changes or the catalogue has a transient failure.
	client.offers[herosms.VerificationSMS][0].Price = "0.7000"
	response = heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks", body, "purchase-uuid-001")
	if response.Code != http.StatusCreated || len(controller.createInputs) != 2 || controller.createInputs[1].SubmissionID != "purchase-uuid-001" {
		t.Fatalf("price-change replay status=%d inputs=%#v body=%s", response.Code, controller.createInputs, response.Body.String())
	}
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) {
		return nil, errors.New("temporary catalogue outage")
	}
	response = heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks", body, "purchase-uuid-001")
	if response.Code != http.StatusCreated || len(controller.createInputs) != 3 || controller.createInputs[2].MaxPriceAmount != "" {
		t.Fatalf("catalogue-outage replay status=%d inputs=%#v body=%s", response.Code, controller.createInputs, response.Body.String())
	}

	response = heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks", `{"service":"wa","country":"6","verification_type":"sms","quantity":101}`, "purchase-uuid-002")
	if response.Code != http.StatusBadRequest || len(controller.createInputs) != 3 {
		t.Fatalf("invalid quantity status = %d calls = %d; body = %s", response.Code, len(controller.createInputs), response.Body.String())
	}
	response = heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks", `{"service":"wa","country":"6","verification_type":"call","duration_hours":24,"quantity":1}`, "purchase-rent-call")
	if response.Code != http.StatusBadRequest || len(controller.createInputs) != 3 {
		t.Fatalf("unsupported rent call status = %d calls = %d; body = %s", response.Code, len(controller.createInputs), response.Body.String())
	}
}

func TestHeroSMSCreateActivationNormalizesZeroDurationAndAllowsNoInventory(t *testing.T) {
	controller := &heroSMSTaskControllerFixture{created: []domain.HeroSMSNumberTask{{
		ID: 201, Status: domain.HeroSMSTaskWaitingNumber, ProductKind: domain.HeroSMSProductActivation,
	}}}
	client := &heroSMSCatalogFixture{offers: map[herosms.VerificationType][]herosms.Offer{}}
	handler := NewHeroSMSAPI(nil, controller)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) { return client, nil }
	router := newHeroSMSAPIRouter(handler)
	response := heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks",
		`{"service":"wa","country":"6","verification_type":"sms","duration_hours":0,"quantity":1}`,
		"activation-zero-duration")
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	if len(controller.createInputs) != 1 {
		t.Fatalf("create calls = %d", len(controller.createInputs))
	}
	input := controller.createInputs[0]
	if input.ProductKind != domain.HeroSMSProductActivation || input.DurationHours != nil || input.MaxPriceAmount != "" {
		t.Fatalf("activation input = %#v", input)
	}
	if len(client.offerRequests) != 1 || client.offerRequests[0].DurationHours != 0 {
		t.Fatalf("offer requests = %#v", client.offerRequests)
	}

	client.offers[herosms.VerificationCall] = []herosms.Offer{{
		Service: "wa", Country: 6, VerificationType: herosms.VerificationCall, Price: "0.9", Count: 1,
	}}
	response = heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks",
		`{"service":"wa","country":"6","verification_type":"call","quantity":1}`,
		"activation-call")
	if response.Code != http.StatusCreated || len(controller.createInputs) != 2 {
		t.Fatalf("call activation status=%d inputs=%#v body=%s", response.Code, controller.createInputs, response.Body.String())
	}
	callInput := controller.createInputs[1]
	if callInput.VerificationType != "call" || callInput.ProductKind != domain.HeroSMSProductActivation || callInput.MaxPriceAmount != "0.9" {
		t.Fatalf("call activation input = %#v", callInput)
	}
}

func TestHeroSMSTaskViewsDeriveCountdownEligibilityAndCapabilities(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	purchasedAt := now.Add(-time.Minute)
	expiresAt := purchasedAt.Add(48 * time.Hour)
	refundableUntil := now.Add(time.Minute)
	atDeadline := now
	messageTaskID := int64(2)
	controller := &heroSMSTaskControllerFixture{listed: []domain.HeroSMSNumberTask{
		{
			ID: 1, Status: domain.HeroSMSTaskActive, ProviderActivationID: "a-1", ProductKind: domain.HeroSMSProductRent,
			PurchasedAt: &purchasedAt, ExpiresAt: &expiresAt, RefundableUntil: &refundableUntil,
			RefundStatus: domain.HeroSMSRefundRefundable,
		},
		{
			ID: 2, Status: domain.HeroSMSTaskActive, ProviderActivationID: "a-2", ProductKind: domain.HeroSMSProductRent,
			RefundableUntil: &refundableUntil, RefundStatus: domain.HeroSMSRefundRefundable,
			Messages: []domain.HeroSMSNumberMessage{{ID: 9, TaskID: &messageTaskID, Code: "123456"}},
		},
		{
			ID: 3, Status: domain.HeroSMSTaskActive, ProviderActivationID: "a-3", ProductKind: domain.HeroSMSProductActivation,
			RefundableUntil: &atDeadline, RefundStatus: domain.HeroSMSRefundRefundable,
		},
		{ID: 4, Status: domain.HeroSMSTaskWaitingNumber, ProductKind: domain.HeroSMSProductActivation},
		{ID: 5, Status: domain.HeroSMSTaskStopped, ProductKind: domain.HeroSMSProductActivation},
		{ID: 6, Status: domain.HeroSMSTaskExpired, ProductKind: domain.HeroSMSProductRent},
		{ID: 7, Status: domain.HeroSMSTaskStopped, ProductKind: domain.HeroSMSProductActivation, PurchaseToken: "unknown-outcome-token"},
		{ID: 8, Status: domain.HeroSMSTaskWaitingNumber, ProductKind: domain.HeroSMSProductActivation, StopRequested: true},
		{ID: 9, Status: domain.HeroSMSTaskActive, ProductKind: domain.HeroSMSProductActivation, ProviderActivationID: "expired-by-clock", ExpiresAt: &now},
	}}
	handler := NewHeroSMSAPI(nil, controller)
	handler.now = func() time.Time { return now }
	router := newHeroSMSAPIRouter(handler)
	response := heroSMSAPIRequest(t, router, http.MethodGet, "/api/hero-sms/tasks?limit=999", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	envelope := decodeHeroSMSBody[heroSMSTasksResponse](t, response)
	if len(controller.listPages) != 1 || controller.listPages[0] != (storage.Page{Limit: 500}) {
		t.Fatalf("list pages = %#v", controller.listPages)
	}
	if len(envelope.Tasks) != 9 {
		t.Fatalf("tasks = %#v", envelope.Tasks)
	}
	refundable := envelope.Tasks[0]
	if !refundable.Refundable || !refundable.Capabilities.Cancel || refundable.Capabilities.Settle || refundable.Capabilities.Stop {
		t.Fatalf("refundable capabilities = %#v", refundable)
	}
	if refundable.EffectiveDurationSeconds == nil || *refundable.EffectiveDurationSeconds != int64(48*time.Hour/time.Second) {
		t.Fatalf("effective duration = %#v", refundable.EffectiveDurationSeconds)
	}
	withMessage := envelope.Tasks[1]
	if withMessage.Refundable || withMessage.Capabilities.Cancel || !withMessage.Capabilities.Settle {
		t.Fatalf("message capabilities = %#v", withMessage)
	}
	atBoundary := envelope.Tasks[2]
	if atBoundary.Refundable || !atBoundary.Capabilities.Settle {
		t.Fatalf("refund boundary capabilities = %#v", atBoundary)
	}
	if !envelope.Tasks[3].Capabilities.Stop || !envelope.Tasks[3].Running {
		t.Fatalf("waiting task = %#v", envelope.Tasks[3])
	}
	if !envelope.Tasks[4].Capabilities.Start || envelope.Tasks[4].Running {
		t.Fatalf("stopped task = %#v", envelope.Tasks[4])
	}
	if envelope.Tasks[5].Running || envelope.Tasks[5].Capabilities != (heroSMSTaskCapabilities{}) {
		t.Fatalf("expired task = %#v", envelope.Tasks[5])
	}
	if envelope.Tasks[6].Running || envelope.Tasks[6].Capabilities != (heroSMSTaskCapabilities{}) {
		t.Fatalf("stopped unknown-purchase task must not restart = %#v", envelope.Tasks[6])
	}
	if !envelope.Tasks[7].Running || envelope.Tasks[7].Capabilities != (heroSMSTaskCapabilities{}) {
		t.Fatalf("stop-requested task must not expose another action = %#v", envelope.Tasks[7])
	}
	if envelope.Tasks[8].Running || envelope.Tasks[8].Capabilities != (heroSMSTaskCapabilities{}) {
		t.Fatalf("clock-expired active task must not expose an action = %#v", envelope.Tasks[8])
	}
	if envelope.Tasks[0].Messages == nil {
		t.Fatal("messages must serialize as an empty array")
	}
}

func TestHeroSMSTaskListCursorKeepsMoreThanOnePageReachable(t *testing.T) {
	controller := &heroSMSTaskControllerFixture{listed: make([]domain.HeroSMSNumberTask, 2)}
	controller.listed[0] = domain.HeroSMSNumberTask{ID: 501, Status: domain.HeroSMSTaskWaitingNumber}
	controller.listed[1] = domain.HeroSMSNumberTask{ID: 500, Status: domain.HeroSMSTaskWaitingNumber}
	handler := NewHeroSMSAPI(nil, controller)
	router := newHeroSMSAPIRouter(handler)

	response := heroSMSAPIRequest(t, router, http.MethodGet, "/api/hero-sms/tasks?limit=2&cursor=500", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	if len(controller.listPages) != 1 || controller.listPages[0] != (storage.Page{Limit: 2, Offset: 500}) {
		t.Fatalf("list pages = %#v", controller.listPages)
	}
	envelope := decodeHeroSMSBody[heroSMSTasksResponse](t, response)
	if envelope.NextCursor != "502" {
		t.Fatalf("next_cursor = %q, want 502", envelope.NextCursor)
	}

	response = heroSMSAPIRequest(t, router, http.MethodGet, "/api/hero-sms/tasks?cursor=invalid", "", "")
	if response.Code != http.StatusBadRequest || len(controller.listPages) != 1 {
		t.Fatalf("invalid cursor status=%d calls=%#v body=%s", response.Code, controller.listPages, response.Body.String())
	}
}

func TestHeroSMSTaskActionsAreIndependentAndStopDecidesFinalization(t *testing.T) {
	controller := &heroSMSTaskControllerFixture{actionTask: domain.HeroSMSNumberTask{
		ID: 42, Status: domain.HeroSMSTaskWaitingNumber, ProductKind: domain.HeroSMSProductActivation,
	}}
	handler := NewHeroSMSAPI(nil, controller)
	router := newHeroSMSAPIRouter(handler)

	response := heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks/42/start", "", "")
	if response.Code != http.StatusAccepted || len(controller.startIDs) != 1 || controller.startIDs[0] != 42 {
		t.Fatalf("start status=%d IDs=%#v body=%s", response.Code, controller.startIDs, response.Body.String())
	}
	for _, action := range []string{"stop", "cancel", "settle"} {
		response = heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks/42/"+action, "", "")
		if response.Code != http.StatusAccepted {
			t.Errorf("%s status = %d; body = %s", action, response.Code, response.Body.String())
		}
	}
	if len(controller.stopIDs) != 3 {
		t.Fatalf("stop IDs = %#v, want one per stop/cancel/settle endpoint", controller.stopIDs)
	}
	response = heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks/not-an-id/stop", "", "")
	if response.Code != http.StatusBadRequest || len(controller.stopIDs) != 3 {
		t.Fatalf("invalid ID status=%d IDs=%#v body=%s", response.Code, controller.stopIDs, response.Body.String())
	}

	controller.actionErr = storage.ErrNotFound
	response = heroSMSAPIRequest(t, router, http.MethodPost, "/api/hero-sms/tasks/404/stop", "", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("not found status = %d; body = %s", response.Code, response.Body.String())
	}
}

func TestHeroSMSRoutesAndUnavailableController(t *testing.T) {
	handler := NewHeroSMSAPI(nil, nil)
	handler.newClient = func(context.Context) (heroSMSCatalogClient, error) {
		return nil, errors.New("catalog unavailable")
	}
	router := newHeroSMSAPIRouter(handler)
	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for method, path := range map[string]string{
		"catalog": http.MethodGet + " /api/hero-sms/catalog",
		"create":  http.MethodPost + " /api/hero-sms/tasks",
		"list":    http.MethodGet + " /api/hero-sms/tasks",
		"start":   http.MethodPost + " /api/hero-sms/tasks/:id/start",
		"stop":    http.MethodPost + " /api/hero-sms/tasks/:id/stop",
		"cancel":  http.MethodPost + " /api/hero-sms/tasks/:id/cancel",
		"settle":  http.MethodPost + " /api/hero-sms/tasks/:id/settle",
	} {
		if !registered[path] {
			t.Errorf("%s route %s is not registered", method, path)
		}
	}
	for _, request := range []struct {
		method string
		path   string
		body   string
		key    string
	}{
		{method: http.MethodPost, path: "/api/hero-sms/tasks", body: `{}`, key: "valid-key"},
		{method: http.MethodGet, path: "/api/hero-sms/tasks"},
		{method: http.MethodPost, path: "/api/hero-sms/tasks/1/start"},
	} {
		response := heroSMSAPIRequest(t, router, request.method, request.path, request.body, request.key)
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s status = %d, want 503; body = %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
}
