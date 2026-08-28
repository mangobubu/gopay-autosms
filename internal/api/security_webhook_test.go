package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mangobubu/gopay-autosms/internal/storage"
	"github.com/mangobubu/gopay-autosms/internal/workflow"
)

type securityStore struct {
	storage.Store
	pingErr error
}

func (store securityStore) Ping(context.Context) error { return store.pingErr }

type webhookCapture struct {
	payloads       []workflow.HeroSMSWebhookPayload
	hasDeadline    bool
	deadlineWindow time.Duration
	err            error
	panicValue     any
}

func (receiver *webhookCapture) ReceiveHeroSMSWebhook(ctx context.Context, payload workflow.HeroSMSWebhookPayload) error {
	if receiver.panicValue != nil {
		panic(receiver.panicValue)
	}
	receiver.payloads = append(receiver.payloads, payload)
	if deadline, ok := ctx.Deadline(); ok {
		receiver.hasDeadline = true
		receiver.deadlineWindow = time.Until(deadline)
	}
	return receiver.err
}

func TestRouterBasicAuthProtectsSPAAndAPIButNotHealth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := NewRouterWithConfig(securityStore{}, nil, nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}), RouterConfig{AuthUsername: "admin", AuthPassword: "correct horse", HeroSMSWebhookToken: "hook-secret"})

	for _, path := range []string{"/healthz", "/readyz"} {
		response := serveRequest(router, http.MethodGet, path, "", "", "")
		if response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200; body = %s", path, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/", "/api/settings/hero-sms"} {
		response := serveRequest(router, http.MethodGet, path, "", "", "")
		if response.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s status = %d, want 401", path, response.Code)
		}
		if got := response.Header().Get("WWW-Authenticate"); !strings.Contains(got, `Basic realm="AutoSMS"`) {
			t.Errorf("GET %s WWW-Authenticate = %q", path, got)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("admin", "correct horse")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("authenticated SPA status = %d, want 204", response.Code)
	}
	if response.Header().Get("X-Frame-Options") != "DENY" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers missing: %v", response.Header())
	}

	notReadyRouter := NewRouterWithConfig(securityStore{pingErr: errors.New("secret database detail")}, nil, nil, nil, RouterConfig{
		AuthUsername: "admin", AuthPassword: "correct horse", HeroSMSWebhookToken: "hook-secret",
	})
	response = serveRequest(notReadyRouter, http.MethodGet, "/readyz", "", "", "")
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "secret database detail") {
		t.Fatalf("not-ready response status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestCSRFProtectionRejectsCrossSiteAndMismatchedOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := RouterConfig{AuthUsername: "admin", AuthPassword: "password", PublicURL: "https://sms.example.com"}
	router := gin.New()
	router.Use(basicAuth(cfg), csrfProtection(cfg))
	router.POST("/mutate", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	tests := []struct {
		name       string
		origin     string
		fetchSite  string
		wantStatus int
	}{
		{name: "same origin", origin: "https://sms.example.com", fetchSite: "same-origin", wantStatus: http.StatusNoContent},
		{name: "same origin metadata only", origin: "", fetchSite: "same-origin", wantStatus: http.StatusNoContent},
		{name: "same site metadata only", origin: "", fetchSite: "same-site", wantStatus: http.StatusForbidden},
		{name: "cross site metadata", origin: "https://sms.example.com", fetchSite: "cross-site", wantStatus: http.StatusForbidden},
		{name: "different origin", origin: "https://attacker.example", fetchSite: "same-site", wantStatus: http.StatusForbidden},
		{name: "missing browser metadata", origin: "", fetchSite: "", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/mutate", nil)
			request.SetBasicAuth("admin", "password")
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestHeroSMSWebhookUsesIndependentTokenAndForwardsOfficialPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	receiver := &webhookCapture{}
	router := NewRouterWithConfig(securityStore{}, nil, nil, nil, RouterConfig{
		AuthUsername: "admin", AuthPassword: "password",
		HeroSMSWebhookToken: "hook-secret", HeroSMSWebhookReceiver: receiver,
	})
	raw := `{"activationId":"635468024","phoneFrom":"GoPay","service":"go","text":null,"code":"12345","country":6,"receivedAt":"2026-08-28T12:34:56+08:00","futureField":true}`
	response := serveRequest(router, http.MethodPost, "/api/webhooks/hero-sms/hook-secret", raw, "application/json; charset=utf-8", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if len(receiver.payloads) != 1 {
		t.Fatalf("receiver calls = %d, want 1", len(receiver.payloads))
	}
	if !receiver.hasDeadline || receiver.deadlineWindow <= 0 || receiver.deadlineWindow > heroSMSWebhookTimeout {
		t.Errorf("receiver deadline window = %s, configured timeout = %s", receiver.deadlineWindow, heroSMSWebhookTimeout)
	}
	payload := receiver.payloads[0]
	if payload.ActivationID != "635468024" || payload.PhoneFrom != "GoPay" || payload.Service != "go" || payload.Country != 6 {
		t.Fatalf("payload identity fields = %#v", payload)
	}
	if payload.Text != nil || payload.Code == nil || *payload.Code != "12345" {
		t.Fatalf("payload nullable fields: text=%v code=%v", payload.Text, payload.Code)
	}
	wantReceivedAt, _ := time.Parse(time.RFC3339, "2026-08-28T12:34:56+08:00")
	if !payload.ReceivedAt.Equal(wantReceivedAt) {
		t.Errorf("received_at = %s, want %s", payload.ReceivedAt, wantReceivedAt)
	}
	if !bytes.Equal(payload.RawPayload, []byte(raw)) {
		t.Errorf("raw payload changed: %s", payload.RawPayload)
	}
	var saved map[string]any
	if err := json.Unmarshal(payload.RawPayload, &saved); err != nil || saved["futureField"] != true {
		t.Errorf("raw future fields not retained: value=%v err=%v", saved, err)
	}

	withoutCode := `{"activationId":"635468025","phoneFrom":"GoPay","service":"go","text":"message without parsed code","country":6,"receivedAt":"2026-08-28T12:35:56Z"}`
	response = serveRequest(router, http.MethodPost, "/api/webhooks/hero-sms/hook-secret", withoutCode, "application/json", "")
	if response.Code != http.StatusOK || len(receiver.payloads) != 2 {
		t.Fatalf("optional-code callback status = %d calls = %d; body = %s", response.Code, len(receiver.payloads), response.Body.String())
	}
	if receiver.payloads[1].Code != nil {
		t.Errorf("omitted code = %q, want nil", *receiver.payloads[1].Code)
	}

	legacyCompatible := `{"activationId":635468026,"service":"go","text":"legacy callback","country":6,"receivedAt":"2026-08-28 04:36:56"}`
	response = serveRequest(router, http.MethodPost, "/api/webhooks/hero-sms/hook-secret", legacyCompatible, "application/json", "")
	if response.Code != http.StatusOK || len(receiver.payloads) != 3 {
		t.Fatalf("legacy-compatible callback status = %d calls = %d; body = %s", response.Code, len(receiver.payloads), response.Body.String())
	}
	legacyPayload := receiver.payloads[2]
	if legacyPayload.ActivationID != "635468026" || legacyPayload.PhoneFrom != "" || !legacyPayload.ReceivedAt.Equal(time.Date(2026, 8, 28, 4, 36, 56, 0, time.UTC)) {
		t.Errorf("legacy-compatible payload = %#v", legacyPayload)
	}
}

func TestHeroSMSWebhookRejectsBadTokenBodyAndFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	receiver := &webhookCapture{}
	router := NewRouterWithConfig(securityStore{}, nil, nil, nil, RouterConfig{
		HeroSMSWebhookToken: "hook-secret", HeroSMSWebhookReceiver: receiver,
	})
	valid := `{"activationId":"635468024","phoneFrom":"GoPay","service":"go","text":"OTP 12345","country":6,"receivedAt":"2026-08-28T12:34:56Z"}`

	tests := []struct {
		name        string
		path        string
		body        string
		contentType string
		wantStatus  int
	}{
		{name: "wrong token", path: "/api/webhooks/hero-sms/wrong", body: valid, contentType: "application/json", wantStatus: http.StatusUnauthorized},
		{name: "wrong media type", path: "/api/webhooks/hero-sms/hook-secret", body: valid, contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{name: "trailing JSON", path: "/api/webhooks/hero-sms/hook-secret", body: valid + `{}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "decimal activation ID", path: "/api/webhooks/hero-sms/hook-secret", body: strings.Replace(valid, `"635468024"`, `635468024.5`, 1), contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "missing text", path: "/api/webhooks/hero-sms/hook-secret", body: strings.Replace(valid, `,"text":"OTP 12345"`, "", 1), contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "bad service", path: "/api/webhooks/hero-sms/hook-secret", body: strings.Replace(valid, `"go"`, `"g"`, 1), contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "bad country", path: "/api/webhooks/hero-sms/hook-secret", body: strings.Replace(valid, `"country":6`, `"country":1000`, 1), contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "bad timestamp", path: "/api/webhooks/hero-sms/hook-secret", body: strings.Replace(valid, `2026-08-28T12:34:56Z`, `08/28/2026 12:34:56`, 1), contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "oversize", path: "/api/webhooks/hero-sms/hook-secret", body: strings.Repeat("x", heroSMSWebhookBodyLimit+1), contentType: "application/json", wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveRequest(router, http.MethodPost, test.path, test.body, test.contentType, "")
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
	if len(receiver.payloads) != 0 {
		t.Fatalf("invalid requests reached receiver %d times", len(receiver.payloads))
	}
}

func TestHeroSMSWebhookReturnsRetryableFailureWithoutLeakingTokenToLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	receiver := &webhookCapture{err: errors.New("database temporarily unavailable")}
	router := NewRouterWithConfig(securityStore{}, nil, nil, nil, RouterConfig{
		HeroSMSWebhookToken: "super-secret-hook-token", HeroSMSWebhookReceiver: receiver,
	})
	body := `{"activationId":"635468024","phoneFrom":"GoPay","service":"go","text":"OTP","country":6,"receivedAt":"2026-08-28T12:34:56Z"}`
	response := serveRequest(router, http.MethodPost, "/api/webhooks/hero-sms/super-secret-hook-token", body, "application/json", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), "super-secret-hook-token") {
		t.Fatalf("access log leaked webhook token: %s", logs.String())
	}
	if !strings.Contains(logs.String(), heroSMSWebhookRoute) {
		t.Fatalf("access log did not use redacted route template: %s", logs.String())
	}

	logs.Reset()
	receiver.err = nil
	receiver.panicValue = "panic detail"
	response = serveRequest(router, http.MethodPost, "/api/webhooks/hero-sms/super-secret-hook-token", body, "application/json", "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d, want 500; body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), "super-secret-hook-token") {
		t.Fatalf("panic log leaked webhook token: %s", logs.String())
	}
	if !strings.Contains(logs.String(), heroSMSWebhookRoute) {
		t.Fatalf("panic log did not use redacted route template: %s", logs.String())
	}
}

func serveRequest(handler http.Handler, method, path, body, contentType, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
