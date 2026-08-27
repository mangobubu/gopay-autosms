package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/smsprovider"
	"github.com/mangobubu/gopay-autosms/internal/storage"
	"github.com/mangobubu/gopay-autosms/internal/workflow"
)

type stopBatchStore struct {
	storage.Store
	result  domain.Batch
	err     error
	callIDs []int64
}

type catalogAPI struct {
	smsbower.API
	prices []smsbower.Price
}

func (a catalogAPI) GetPrices(context.Context, smsbower.PriceRequest) ([]smsbower.Price, error) {
	return a.prices, nil
}

func TestCatalogProviderValidationDispatchAndHeroTierRemoval(t *testing.T) {
	gin.SetMode(gin.TestMode)
	called := ""
	server := &Server{smsClientFactory: func(_ context.Context, provider string) (smsbower.API, error) {
		called = provider
		return catalogAPI{prices: []smsbower.Price{{
			Country: 6, Service: "go", ProviderID: 17, Price: "0.8", Count: 10, Tier: "Gold",
		}}}, nil
	}}
	router := gin.New()
	router.GET("/catalog/prices", server.listPrices)

	request := httptest.NewRequest(http.MethodGet, "/catalog/prices?country=6&service=go&sms_provider=hero-sms", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	if called != smsprovider.HeroSMS {
		t.Fatalf("provider dispatch = %q, want %q", called, smsprovider.HeroSMS)
	}
	if strings.Contains(response.Body.String(), `"tier"`) || strings.Contains(response.Body.String(), `"providerId"`) {
		t.Fatalf("HeroSMS price response exposes provider metadata: %s", response.Body.String())
	}

	called = ""
	request = httptest.NewRequest(http.MethodGet, "/catalog/prices?country=6&service=go&sms_provider=unknown", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider status = %d, want 400; body = %s", response.Code, response.Body.String())
	}
	if called != "" {
		t.Fatalf("factory called for unknown provider: %q", called)
	}
}

func TestHeroSMSSettingsRoutesRegisteredForBothAPIPrefixes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	box, err := secure.New("hero-settings-routes-test")
	if err != nil {
		t.Fatal(err)
	}
	store := &createBatchCaptureStore{settings: map[string]domain.Setting{}}
	settingsManager := appsettings.New(store, box, "https://bower.test", "https://hero.test")
	router := NewRouter(store, settingsManager, nil, nil)
	routes := make(map[string]bool)
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, prefix := range []string{"/api", "/api/v1"} {
		for method, suffix := range map[string]string{
			http.MethodGet:  "/settings/hero-sms",
			http.MethodPut:  "/settings/hero-sms",
			http.MethodPost: "/settings/hero-sms/test",
		} {
			key := method + " " + prefix + suffix
			if !routes[key] {
				t.Errorf("route %s is not registered", key)
			}
		}
	}
	for _, path := range []string{"/api/settings/hero-sms", "/api/v1/settings/hero-sms"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200; body = %s", path, response.Code, response.Body.String())
		}
		if got := response.Body.String(); !strings.Contains(got, `"configured":false`) {
			t.Errorf("GET %s body = %s, want unconfigured HeroSMS settings", path, got)
		}
	}
}

func (s *stopBatchStore) CancelBatch(_ context.Context, id int64) (domain.Batch, error) {
	s.callIDs = append(s.callIDs, id)
	return s.result, s.err
}

func TestStopBatch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		path       string
		storeErr   error
		wantStatus int
		wantCallID int64
	}{
		{
			name:       "成功停止批次",
			path:       "/api/batches/42/stop",
			wantStatus: http.StatusAccepted,
			wantCallID: 42,
		},
		{
			name:       "非法批次ID",
			path:       "/api/batches/not-a-number/stop",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "批次不存在",
			path:       "/api/batches/42/stop",
			storeErr:   storage.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCallID: 42,
		},
		{
			name:       "批次状态冲突",
			path:       "/api/batches/42/stop",
			storeErr:   storage.ErrConflict,
			wantStatus: http.StatusConflict,
			wantCallID: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stopBatchStore{
				result: domain.Batch{ID: 42, Status: domain.BatchStatusCancelled},
				err:    tt.storeErr,
			}
			manager := workflow.New(store, nil, nil, workflow.Config{}, nil)
			router := NewRouter(store, nil, manager, nil)
			request := httptest.NewRequest(http.MethodPost, tt.path, nil)
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d；响应体：%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if tt.wantCallID == 0 {
				if len(store.callIDs) != 0 {
					t.Fatalf("CancelBatch 调用次数 = %d，期望 0", len(store.callIDs))
				}
				return
			}
			if len(store.callIDs) != 1 {
				t.Fatalf("CancelBatch 调用次数 = %d，期望 1", len(store.callIDs))
			}
			if store.callIDs[0] != tt.wantCallID {
				t.Fatalf("CancelBatch ID = %d，期望 %d", store.callIDs[0], tt.wantCallID)
			}
		})
	}
}

type createBatchConflictStore struct {
	storage.Store
	setting     domain.Setting
	createCalls int
}

func (s *createBatchConflictStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	if key != s.setting.Key {
		return domain.Setting{}, storage.ErrNotFound
	}
	return s.setting, nil
}

func (s *createBatchConflictStore) CreateBatch(context.Context, storage.CreateBatchParams) (domain.Batch, error) {
	s.createCalls++
	return domain.Batch{}, storage.ErrActiveBatchExists
}

func TestCreateBatchReturnsConflictWhenAnotherBatchIsActive(t *testing.T) {
	gin.SetMode(gin.TestMode)

	box, err := secure.New("create-batch-conflict-test")
	if err != nil {
		t.Fatal(err)
	}
	apiKeyCiphertext, err := box.Seal([]byte("fixture-api-key"))
	if err != nil {
		t.Fatal(err)
	}
	settingValue, err := json.Marshal(map[string]string{
		"api_key_encrypted": base64.StdEncoding.EncodeToString(apiKeyCiphertext),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &createBatchConflictStore{setting: domain.Setting{
		Key: appsettings.SMSBowerKey, Value: settingValue,
	}}
	manager := workflow.New(store, appsettings.New(store, box, "http://sms.test"), box, workflow.Config{}, nil)
	router := NewRouter(store, appsettings.New(store, box, "http://sms.test"), manager, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/batches", strings.NewReader(`{
		"service":"go","country":"6","max_price":"1.25","quantity":1,"pin":"123456"
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 %d；响应体：%s", response.Code, http.StatusConflict, response.Body.String())
	}
	if store.createCalls != 1 {
		t.Fatalf("CreateBatch 调用次数 = %d，期望 1", store.createCalls)
	}
}

type createBatchCaptureStore struct {
	storage.Store
	settings    map[string]domain.Setting
	settingKeys []string
	params      []storage.CreateBatchParams
}

func (s *createBatchCaptureStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	s.settingKeys = append(s.settingKeys, key)
	setting, ok := s.settings[key]
	if !ok {
		return domain.Setting{}, storage.ErrNotFound
	}
	return setting, nil
}

func (s *createBatchCaptureStore) CreateBatch(_ context.Context, params storage.CreateBatchParams) (domain.Batch, error) {
	s.params = append(s.params, params)
	return domain.Batch{ID: 91, Status: domain.BatchStatusPending}, nil
}

func TestCreateBatchPersistsHeroSMSProviderAndValidatesOnlyHeroKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	box, err := secure.New("create-hero-batch-test")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Seal([]byte("hero-fixture-key"))
	if err != nil {
		t.Fatal(err)
	}
	settingValue, err := json.Marshal(map[string]string{
		"api_key_encrypted": base64.StdEncoding.EncodeToString(ciphertext),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &createBatchCaptureStore{settings: map[string]domain.Setting{
		appsettings.HeroSMSKey: {Key: appsettings.HeroSMSKey, Value: settingValue},
	}}
	settingsManager := appsettings.New(store, box, "https://bower.test", "https://hero.test")
	manager := workflow.New(store, settingsManager, box, workflow.Config{}, nil)
	router := NewRouter(store, settingsManager, manager, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/batches", strings.NewReader(`{
		"service":"go","country":"6","max_price":"1.25","currency":"USD",
		"sms_provider":"hero-sms","quantity":1,"pin":"123456"
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", response.Code, response.Body.String())
	}
	if len(store.settingKeys) != 1 || store.settingKeys[0] != appsettings.HeroSMSKey {
		t.Fatalf("settings read = %v, want only %q", store.settingKeys, appsettings.HeroSMSKey)
	}
	if len(store.params) != 1 {
		t.Fatalf("CreateBatch calls = %d, want 1", len(store.params))
	}
	var options workflow.BatchOptions
	if err = json.Unmarshal(store.params[0].Config, &options); err != nil {
		t.Fatal(err)
	}
	if options.SMSProvider != smsprovider.HeroSMS {
		t.Fatalf("sms_provider = %q, want %q", options.SMSProvider, smsprovider.HeroSMS)
	}
	if len(options.ProviderIDs) != 0 {
		t.Fatalf("internal provider IDs = %v, want none for HeroSMS", options.ProviderIDs)
	}
	if store.params[0].Currency != "" {
		t.Fatalf("HeroSMS batch currency = %q, want unknown until allocation", store.params[0].Currency)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/batches", strings.NewReader(`{
		"service":"go","country":"6","max_price":"1.25",
		"sms_provider":" HERO-SMS ","source":"hero-sms","quantity":1,"pin":"123456"
	}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("matching aliases status = %d, want 201; body = %s", response.Code, response.Body.String())
	}
	if len(store.params) != 2 {
		t.Fatalf("CreateBatch calls after matching aliases = %d, want 2", len(store.params))
	}
	if err = json.Unmarshal(store.params[1].Config, &options); err != nil {
		t.Fatal(err)
	}
	if options.SMSProvider != smsprovider.HeroSMS {
		t.Fatalf("matching aliases sms_provider = %q, want %q", options.SMSProvider, smsprovider.HeroSMS)
	}
	if store.params[1].Currency != "" {
		t.Fatalf("default HeroSMS batch currency = %q, want unknown until allocation", store.params[1].Currency)
	}
}

func TestCreateBatchRejectsHeroSMSProviderFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, filter := range map[string]string{
		"legacy provider": `"provider":"17"`,
		"provider IDs":    `"provider_ids":[17]`,
	} {
		t.Run(name, func(t *testing.T) {
			store := &createBatchCaptureStore{}
			router := NewRouter(store, nil, nil, nil)
			body := fmt.Sprintf(`{
				"service":"go","country":"6","max_price":"1.25",%s,
				"sms_provider":"hero-sms","quantity":1,"pin":"123456"
			}`, filter)
			request := httptest.NewRequest(http.MethodPost, "/api/batches", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
			}
			if len(store.params) != 0 {
				t.Fatalf("CreateBatch calls = %d, want 0", len(store.params))
			}
		})
	}
}

func TestCreateBatchRejectsUnknownSMSProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &createBatchCaptureStore{}
	router := NewRouter(store, nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/batches", strings.NewReader(`{
		"service":"go","country":"6","max_price":"1.25",
		"sms_provider":"unknown","quantity":1,"pin":"123456"
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
	}
	if len(store.params) != 0 {
		t.Fatalf("CreateBatch calls = %d, want 0", len(store.params))
	}
}

func TestCreateBatchRejectsConflictingSMSProviderAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &createBatchCaptureStore{}
	router := NewRouter(store, nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/batches", strings.NewReader(`{
		"service":"go","country":"6","max_price":"1.25",
		"sms_provider":"hero-sms","source":"smsbower","quantity":1,"pin":"123456"
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
	}
	if len(store.params) != 0 {
		t.Fatalf("CreateBatch calls = %d, want 0", len(store.params))
	}
}
