package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	DefaultDatabaseURL    = "postgres://autosms:autosms_local_password@127.0.0.1:5432/autosms?sslmode=disable"
	DefaultSMSBaseURL     = "https://smsbower.page/stubs/handler_api.php"
	DefaultHeroSMSBaseURL = "https://hero-sms.com/stubs/handler_api.php"
)

type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	SecretKey         string
	DataDir           string
	SMSBaseURL        string
	HeroSMSBaseURL    string
	GoPaySSOBaseURL   string
	GoPayBaseURL      string
	PollInterval      time.Duration
	LoginStatusTTL    time.Duration
	ActivationTTL     time.Duration
	PurchaseRetryWait time.Duration
	MaxBatchAttempts  int
}

func Load() Config {
	return Config{
		HTTPAddr:          envString("AUTOSMS_HTTP_ADDR", ":8080"),
		DatabaseURL:       envString("DATABASE_URL", DefaultDatabaseURL),
		SecretKey:         envString("AUTOSMS_SECRET_KEY", ""),
		DataDir:           envString("AUTOSMS_DATA_DIR", "data"),
		SMSBaseURL:        envString("AUTOSMS_SMSBOWER_BASE_URL", DefaultSMSBaseURL),
		HeroSMSBaseURL:    envString("AUTOSMS_HEROSMS_BASE_URL", envString("AUTOSMS_HERO_SMS_BASE_URL", DefaultHeroSMSBaseURL)),
		GoPaySSOBaseURL:   envString("AUTOSMS_GOPAY_SSO_BASE_URL", "https://accounts.goto-products.com"),
		GoPayBaseURL:      envString("AUTOSMS_GOPAY_BASE_URL", "https://customer.gopayapi.com"),
		PollInterval:      envDuration("AUTOSMS_POLL_INTERVAL", 2*time.Second),
		LoginStatusTTL:    envDuration("AUTOSMS_GOPAY_LOGIN_STATUS_TTL", 4*time.Second),
		ActivationTTL:     envDuration("AUTOSMS_ACTIVATION_TTL", 20*time.Minute),
		PurchaseRetryWait: envDuration("AUTOSMS_PURCHASE_RETRY_WAIT", 2*time.Second),
		MaxBatchAttempts:  envInt("AUTOSMS_MAX_BATCH_ATTEMPTS", 100),
	}
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
