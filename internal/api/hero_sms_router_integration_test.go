package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestNewRouterWithConfigHeroSMSTaskRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("configured for both API prefixes", func(t *testing.T) {
		controller := &heroSMSTaskControllerFixture{}
		spaHits := 0
		spa := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			spaHits++
			writer.Header().Set("X-Test-SPA-Fallback", "true")
			writer.WriteHeader(http.StatusTeapot)
		})
		router := NewRouterWithConfig(securityStore{}, nil, nil, spa, RouterConfig{
			HeroSMSTasks: controller,
		})

		registered := make(map[string]bool)
		for _, route := range router.Routes() {
			registered[route.Method+" "+route.Path] = true
		}
		paths := []string{"/api/hero-sms/tasks", "/api/v1/hero-sms/tasks"}
		for index, path := range paths {
			if !registered[http.MethodGet+" "+path] {
				t.Fatalf("GET %s is not registered through NewRouterWithConfig", path)
			}
			response := heroSMSAPIRequest(t, router, http.MethodGet, path, "", "")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200; body = %s", path, response.Code, response.Body.String())
			}
			if response.Header().Get("X-Test-SPA-Fallback") != "" {
				t.Fatalf("GET %s was handled by the SPA fallback", path)
			}
			if len(controller.listPages) != index+1 {
				t.Fatalf("GET %s controller calls = %d, want %d", path, len(controller.listPages), index+1)
			}
		}
		if spaHits != 0 {
			t.Fatalf("SPA fallback hits = %d, want 0", spaHits)
		}
	})

	t.Run("unconfigured routes stay private to the SPA fallback", func(t *testing.T) {
		spaHits := 0
		spa := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			spaHits++
			writer.Header().Set("X-Test-SPA-Fallback", "true")
			writer.WriteHeader(http.StatusTeapot)
		})
		router := NewRouterWithConfig(securityStore{}, nil, nil, spa, RouterConfig{})

		for _, route := range router.Routes() {
			if strings.Contains(route.Path, "/hero-sms/tasks") {
				t.Fatalf("task route is registered without HeroSMSTasks: %s %s", route.Method, route.Path)
			}
		}
		for _, path := range []string{"/api/hero-sms/tasks", "/api/v1/hero-sms/tasks"} {
			response := heroSMSAPIRequest(t, router, http.MethodGet, path, "", "")
			if response.Code != http.StatusTeapot || response.Header().Get("X-Test-SPA-Fallback") != "true" {
				t.Fatalf("GET %s status = %d headers = %v, want SPA fallback", path, response.Code, response.Header())
			}
		}
		if spaHits != 2 {
			t.Fatalf("SPA fallback hits = %d, want 2", spaHits)
		}
	})
}
