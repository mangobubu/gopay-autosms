package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	heroSMSWebhookRoute       = "/api/webhooks/hero-sms/:token"
	maxRequestBodyBytes int64 = 2 << 20
)

// RouterConfig contains credentials that protect the Internet-facing HTTP
// surface. Health checks are intentionally public and the HeroSMS callback has
// its own URL token because the provider does not send HTTP Basic credentials.
type RouterConfig struct {
	AuthUsername           string
	AuthPassword           string
	HeroSMSWebhookToken    string
	PublicURL              string
	HeroSMSWebhookReceiver HeroSMSWebhookReceiver
	HeroSMSTasks           HeroSMSTaskController
}

func accessLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		startedAt := time.Now()
		c.Next()
		path := c.FullPath()
		if path == "" {
			// Logging an unmatched raw URL could expose credentials accidentally
			// placed in a path or query string.
			path = "/<unmatched>"
		}
		slog.InfoContext(c.Request.Context(), "HTTP request",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency", time.Since(startedAt),
		)
	}
}

func safeRecovery() gin.HandlerFunc {
	// Gin's default debug recovery dumps the raw request URI, which would reveal
	// the webhook token stored in its path. Suppress that dump and log only the
	// matched route template.
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) {
		path := c.FullPath()
		if path == "" {
			path = "/<unmatched>"
		}
		slog.ErrorContext(c.Request.Context(), "HTTP handler panic recovered",
			"method", c.Request.Method, "path", path)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
	})
}

func basicAuth(cfg RouterConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isPublicRoute(c) || (cfg.AuthUsername == "" && cfg.AuthPassword == "") {
			c.Next()
			return
		}

		username, password, ok := c.Request.BasicAuth()
		usernameMatches := constantTimeEqual(username, cfg.AuthUsername)
		passwordMatches := constantTimeEqual(password, cfg.AuthPassword)
		if !ok || !usernameMatches || !passwordMatches {
			c.Header("WWW-Authenticate", `Basic realm="AutoSMS", charset="UTF-8"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.Next()
	}
}

func isPublicRoute(c *gin.Context) bool {
	path := c.FullPath()
	return path == "/healthz" || path == "/readyz" || path == heroSMSWebhookRoute
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "same-origin")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Header("Cache-Control", "no-store")
		}
		c.Next()
	}
}

// limitRequestBodies bounds every Internet-facing request before a handler
// starts decoding it. The webhook applies its stricter 64 KiB limit on top.
func limitRequestBodies() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
		}
		c.Next()
	}
}

func csrfProtection(cfg RouterConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isPublicRoute(c) || !isUnsafeMethod(c.Request.Method) {
			c.Next()
			return
		}
		if strings.EqualFold(strings.TrimSpace(c.GetHeader("Sec-Fetch-Site")), "cross-site") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "cross-site request rejected"})
			return
		}
		originText := strings.TrimSpace(c.GetHeader("Origin"))
		if originText == "" {
			fetchSite := strings.ToLower(strings.TrimSpace(c.GetHeader("Sec-Fetch-Site")))
			if fetchSite == "same-origin" || strings.TrimSpace(cfg.PublicURL) == "" {
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "request origin is required"})
			return
		}
		origin, err := url.Parse(originText)
		if err != nil || origin.Scheme == "" || origin.Host == "" || origin.User != nil || origin.Path != "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "invalid request origin"})
			return
		}
		expectedScheme, expectedHost := expectedOrigin(c.Request, cfg.PublicURL)
		if !strings.EqualFold(origin.Scheme, expectedScheme) || !strings.EqualFold(origin.Host, expectedHost) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "request origin does not match this service"})
			return
		}
		c.Next()
	}
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func expectedOrigin(request *http.Request, publicURL string) (scheme, host string) {
	if parsed, err := url.Parse(strings.TrimSpace(publicURL)); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Scheme, parsed.Host
	}
	scheme = firstForwardedValue(request.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host = firstForwardedValue(request.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = request.Host
	}
	return scheme, host
}

func firstForwardedValue(value string) string {
	value, _, _ = strings.Cut(value, ",")
	return strings.TrimSpace(value)
}

// Hashing to a fixed width keeps comparisons constant-time even when the
// supplied credential has a different length from the configured credential.
func constantTimeEqual(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}
