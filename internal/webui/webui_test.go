package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndexAndSPAFallback(t *testing.T) {
	handler := Handler()

	for _, route := range []string{"/", "/batches/current"} {
		t.Run(route, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, route, nil)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("GET %s returned %d", route, response.Code)
			}
			if !strings.Contains(response.Body.String(), `<div id="app"></div>`) {
				t.Fatalf("GET %s did not serve the Vue index", route)
			}
		})
	}
}

func TestHandlerServesEmbeddedAsset(t *testing.T) {
	entries, err := fs.ReadDir(Assets(), "assets")
	if err != nil {
		t.Fatalf("read assets: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("production build contains no assets")
	}

	route := "/assets/" + entries[0].Name()
	request := httptest.NewRequest(http.MethodGet, route, nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("GET %s returned %d", route, response.Code)
	}
	if got := response.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("unexpected cache header %q", got)
	}
}

func TestHandlerDoesNotFallbackForAPIOrMissingAsset(t *testing.T) {
	for _, route := range []string{"/api/health", "/assets/missing.js"} {
		t.Run(route, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, route, nil)
			response := httptest.NewRecorder()
			Handler().ServeHTTP(response, request)

			if response.Code != http.StatusNotFound {
				t.Fatalf("GET %s returned %d, want 404", route, response.Code)
			}
		})
	}
}
