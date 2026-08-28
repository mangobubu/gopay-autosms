package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultDatabaseURL    = "postgres://autosms:autosms_local_password@127.0.0.1:5432/autosms?sslmode=disable"
	DefaultSMSBaseURL     = "https://smsbower.page/stubs/handler_api.php"
	DefaultHeroSMSBaseURL = "https://hero-sms.com/stubs/handler_api.php"
)

var webhookTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Config struct {
	HTTPAddr            string
	PublicURL           string
	AuthUsername        string
	AuthPassword        string
	HeroSMSWebhookToken string
	DatabaseURL         string
	SecretKey           string
	DataDir             string
	SMSBaseURL          string
	HeroSMSBaseURL      string
	GoPaySSOBaseURL     string
	GoPayBaseURL        string
	PollInterval        time.Duration
	LoginStatusTTL      time.Duration
	ActivationTTL       time.Duration
	PurchaseRetryWait   time.Duration
	MaxBatchAttempts    int
}

func Load() Config {
	return Config{
		HTTPAddr:            envString("AUTOSMS_HTTP_ADDR", ":8080"),
		PublicURL:           envString("AUTOSMS_PUBLIC_URL", ""),
		AuthUsername:        envString("AUTOSMS_AUTH_USERNAME", ""),
		AuthPassword:        envString("AUTOSMS_AUTH_PASSWORD", ""),
		HeroSMSWebhookToken: envString("AUTOSMS_HEROSMS_WEBHOOK_TOKEN", ""),
		DatabaseURL:         envString("DATABASE_URL", DefaultDatabaseURL),
		SecretKey:           envString("AUTOSMS_SECRET_KEY", ""),
		DataDir:             envString("AUTOSMS_DATA_DIR", "data"),
		SMSBaseURL:          envString("AUTOSMS_SMSBOWER_BASE_URL", DefaultSMSBaseURL),
		HeroSMSBaseURL:      envString("AUTOSMS_HEROSMS_BASE_URL", envString("AUTOSMS_HERO_SMS_BASE_URL", DefaultHeroSMSBaseURL)),
		GoPaySSOBaseURL:     envString("AUTOSMS_GOPAY_SSO_BASE_URL", "https://accounts.goto-products.com"),
		GoPayBaseURL:        envString("AUTOSMS_GOPAY_BASE_URL", "https://customer.gopayapi.com"),
		PollInterval:        envDuration("AUTOSMS_POLL_INTERVAL", 2*time.Second),
		LoginStatusTTL:      envDuration("AUTOSMS_GOPAY_LOGIN_STATUS_TTL", 4*time.Second),
		ActivationTTL:       envDuration("AUTOSMS_ACTIVATION_TTL", 20*time.Minute),
		PurchaseRetryWait:   envDuration("AUTOSMS_PURCHASE_RETRY_WAIT", 2*time.Second),
		MaxBatchAttempts:    envInt("AUTOSMS_MAX_BATCH_ATTEMPTS", 100),
	}
}

// ValidatePublicSecurity fails closed before an Internet-facing server is
// started. The webhook token is separate from the dashboard credentials
// because HeroSMS authenticates by calling a configured URL.
func ValidatePublicSecurity(cfg Config) error {
	missing := make([]string, 0, 4)
	if strings.TrimSpace(cfg.PublicURL) == "" {
		missing = append(missing, "AUTOSMS_PUBLIC_URL")
	}
	if strings.TrimSpace(cfg.AuthUsername) == "" {
		missing = append(missing, "AUTOSMS_AUTH_USERNAME")
	}
	if cfg.AuthPassword == "" {
		missing = append(missing, "AUTOSMS_AUTH_PASSWORD")
	}
	if cfg.HeroSMSWebhookToken == "" {
		missing = append(missing, "AUTOSMS_HEROSMS_WEBHOOK_TOKEN")
	}
	if len(missing) != 0 {
		return fmt.Errorf("required public security configuration is missing: %s", strings.Join(missing, ", "))
	}
	if len(cfg.AuthPassword) < 16 {
		return fmt.Errorf("AUTOSMS_AUTH_PASSWORD must contain at least 16 characters")
	}
	if strings.TrimSpace(cfg.AuthPassword) == "" || strings.ContainsAny(cfg.AuthUsername+cfg.AuthPassword, "\r\n") || strings.Contains(cfg.AuthUsername, ":") {
		return fmt.Errorf("HTTP Basic credentials must be non-blank, contain no newlines, and the username must not contain ':'")
	}
	if len(cfg.HeroSMSWebhookToken) < 32 || !webhookTokenPattern.MatchString(cfg.HeroSMSWebhookToken) {
		return fmt.Errorf("AUTOSMS_HEROSMS_WEBHOOK_TOKEN must contain at least 32 URL-safe characters (A-Z, a-z, 0-9, _ or -)")
	}
	if cfg.HeroSMSWebhookToken == cfg.AuthPassword {
		return fmt.Errorf("AUTOSMS_HEROSMS_WEBHOOK_TOKEN must be different from AUTOSMS_AUTH_PASSWORD")
	}
	publicURL, err := url.Parse(strings.TrimSpace(cfg.PublicURL))
	if err != nil || publicURL.Host == "" || publicURL.User != nil ||
		(publicURL.Path != "" && publicURL.Path != "/") || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		return fmt.Errorf("AUTOSMS_PUBLIC_URL must be an absolute service origin without credentials, query, fragment or path")
	}
	if publicURL.Scheme != "https" {
		hostname := publicURL.Hostname()
		loopback := strings.EqualFold(hostname, "localhost")
		if address := net.ParseIP(hostname); address != nil && address.IsLoopback() {
			loopback = true
		}
		if publicURL.Scheme != "http" || !loopback {
			return fmt.Errorf("AUTOSMS_PUBLIC_URL must use HTTPS except for loopback development addresses")
		}
	}
	return nil
}

// ResolveSecretKey keeps local (non-Docker) restarts able to decrypt the
// database while avoiding a predictable built-in encryption key. Docker
// supplies AUTOSMS_SECRET_KEY from its persistent runtime credentials file.
func ResolveSecretKey(cfg Config) (string, error) {
	if cfg.SecretKey != "" {
		return cfg.SecretKey, nil
	}
	path := os.Getenv("AUTOSMS_SECRET_FILE")
	if path == "" {
		path = filepath.Join(cfg.DataDir, "runtime", "secret.key")
	}
	if value, err := os.ReadFile(path); err == nil && len(value) > 0 {
		return string(value), nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read secret key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("create secret directory: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate secret key: %w", err)
	}
	value := []byte(hex.EncodeToString(raw))
	if err := os.WriteFile(path, value, 0600); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && len(existing) > 0 {
			return string(existing), nil
		}
		return "", fmt.Errorf("persist secret key: %w", err)
	}
	return string(value), nil
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
