package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type batchDraftAPIStore struct {
	values map[string]json.RawMessage
}

func (s *batchDraftAPIStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	value, ok := s.values[key]
	if !ok {
		return domain.Setting{}, storage.ErrNotFound
	}
	return domain.Setting{Key: key, Value: append(json.RawMessage(nil), value...)}, nil
}

func (s *batchDraftAPIStore) SetSetting(_ context.Context, key string, value json.RawMessage) (domain.Setting, error) {
	if s.values == nil {
		s.values = make(map[string]json.RawMessage)
	}
	cloned := append(json.RawMessage(nil), value...)
	s.values[key] = cloned
	return domain.Setting{Key: key, Value: cloned}, nil
}

func (s *batchDraftAPIStore) ListSettings(context.Context) ([]domain.Setting, error) {
	return nil, nil
}

func newBatchDraftRouter(t *testing.T) (*gin.Engine, *batchDraftAPIStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	box, err := secure.New("batch-draft-api-test")
	if err != nil {
		t.Fatal(err)
	}
	store := &batchDraftAPIStore{}
	server := &Server{settings: appsettings.New(store, box, "https://bower.test", "https://hero.test")}
	router := gin.New()
	router.GET("/settings/batch-draft", server.getBatchDraft)
	router.PUT("/settings/batch-draft", server.putBatchDraft)
	return router, store
}

func TestBatchDraftRoutesRegisteredForBothAPIPrefixes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	server := &Server{}
	server.register(router.Group("/api"))
	server.register(router.Group("/api/v1"))

	routes := make(map[string]bool)
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, prefix := range []string{"/api", "/api/v1"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			key := method + " " + prefix + "/settings/batch-draft"
			if !routes[key] {
				t.Errorf("route %s is not registered", key)
			}
		}
	}
}

func TestBatchDraftAPIEncryptedPutAndPlaintextGet(t *testing.T) {
	router, store := newBatchDraftRouter(t)
	body := `{
		"sms_provider":" HERO-SMS ","service":"go","country":"6",
		"price_key":"offer-1","quantity":2,"pin":"123",
		"proxy":"http://user:pass@proxy.test:8080",
		"price_snapshot":{"value":"offer-1","price":0.82}
	}`

	put := httptest.NewRequest(http.MethodPut, "/settings/batch-draft", strings.NewReader(body))
	put.Header.Set("Content-Type", "application/json")
	putResponse := httptest.NewRecorder()
	router.ServeHTTP(putResponse, put)
	if putResponse.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; body = %s", putResponse.Code, putResponse.Body.String())
	}
	if !strings.Contains(putResponse.Body.String(), `"sms_provider":"hero-sms"`) ||
		!strings.Contains(putResponse.Body.String(), `"pin":"123"`) {
		t.Fatalf("PUT response did not return normalized plaintext draft: %s", putResponse.Body.String())
	}
	persisted := string(store.values[appsettings.BatchDraftKey])
	for _, plaintext := range []string{"offer-1", `"pin":"123"`, "proxy.test", "price_snapshot"} {
		if strings.Contains(persisted, plaintext) {
			t.Fatalf("persisted draft contains plaintext %q: %s", plaintext, persisted)
		}
	}

	get := httptest.NewRequest(http.MethodGet, "/settings/batch-draft", nil)
	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body = %s", getResponse.Code, getResponse.Body.String())
	}
	if !strings.Contains(getResponse.Body.String(), `"proxy":"http://user:pass@proxy.test:8080"`) ||
		!strings.Contains(getResponse.Body.String(), `"price_snapshot":{"value":"offer-1","price":0.82}`) {
		t.Fatalf("GET response did not return plaintext draft: %s", getResponse.Body.String())
	}
}

func TestBatchDraftAPIGetMissingReturnsEmptyDraft(t *testing.T) {
	router, _ := newBatchDraftRouter(t)
	request := httptest.NewRequest(http.MethodGet, "/settings/batch-draft", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body = %s", response.Code, response.Body.String())
	}
	if got := strings.TrimSpace(response.Body.String()); got != `{"sms_provider":"","service":"","country":"","price_key":"","quantity":0,"pin":"","proxy":"","price_snapshot":null}` {
		t.Fatalf("empty draft response = %s", got)
	}
}

func TestBatchDraftAPIRejectsInvalidDraftFields(t *testing.T) {
	router, _ := newBatchDraftRouter(t)
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "provider", body: `{"sms_provider":"unknown"}`},
		{name: "quantity", body: `{"quantity":101}`},
		{name: "pin", body: `{"pin":"12a"}`},
		{name: "invalid JSON", body: `{"pin":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/settings/batch-draft", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("PUT status = %d, want 400; body = %s", response.Code, response.Body.String())
			}
		})
	}
}
