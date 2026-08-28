package config

import (
	"strings"
	"testing"
)

func TestLoadReadsPublicSecurityConfiguration(t *testing.T) {
	t.Setenv("AUTOSMS_PUBLIC_URL", "https://sms.example.com")
	t.Setenv("AUTOSMS_AUTH_USERNAME", "operator")
	t.Setenv("AUTOSMS_AUTH_PASSWORD", "a-long-dashboard-password")
	t.Setenv("AUTOSMS_HEROSMS_WEBHOOK_TOKEN", strings.Repeat("a", 32))

	cfg := Load()
	if cfg.PublicURL != "https://sms.example.com" || cfg.AuthUsername != "operator" || cfg.AuthPassword != "a-long-dashboard-password" || cfg.HeroSMSWebhookToken != strings.Repeat("a", 32) {
		t.Fatalf("public security config not loaded: %#v", cfg)
	}
	if err := ValidatePublicSecurity(cfg); err != nil {
		t.Fatalf("valid public security config rejected: %v", err)
	}
}

func TestValidatePublicSecurityFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "all missing", cfg: Config{}, want: "AUTOSMS_AUTH_USERNAME"},
		{name: "short password", cfg: Config{PublicURL: "https://sms.example.com", AuthUsername: "operator", AuthPassword: "short", HeroSMSWebhookToken: strings.Repeat("a", 32)}, want: "at least 16"},
		{name: "blank password", cfg: Config{PublicURL: "https://sms.example.com", AuthUsername: "operator", AuthPassword: strings.Repeat(" ", 16), HeroSMSWebhookToken: strings.Repeat("a", 32)}, want: "non-blank"},
		{name: "colon username", cfg: Config{PublicURL: "https://sms.example.com", AuthUsername: "bad:name", AuthPassword: "a-long-dashboard-password", HeroSMSWebhookToken: strings.Repeat("a", 32)}, want: "must not contain ':'"},
		{name: "short token", cfg: Config{PublicURL: "https://sms.example.com", AuthUsername: "operator", AuthPassword: "a-long-dashboard-password", HeroSMSWebhookToken: "short"}, want: "at least 32"},
		{name: "path-unsafe token", cfg: Config{PublicURL: "https://sms.example.com", AuthUsername: "operator", AuthPassword: "a-long-dashboard-password", HeroSMSWebhookToken: strings.Repeat("a", 31) + "/"}, want: "URL-safe"},
		{name: "reused password and token", cfg: Config{PublicURL: "https://sms.example.com", AuthUsername: "operator", AuthPassword: strings.Repeat("a", 32), HeroSMSWebhookToken: strings.Repeat("a", 32)}, want: "must be different"},
		{name: "public plain HTTP", cfg: Config{PublicURL: "http://sms.example.com", AuthUsername: "operator", AuthPassword: "a-long-dashboard-password", HeroSMSWebhookToken: strings.Repeat("a", 32)}, want: "HTTPS"},
		{name: "public URL path", cfg: Config{PublicURL: "https://sms.example.com/admin", AuthUsername: "operator", AuthPassword: "a-long-dashboard-password", HeroSMSWebhookToken: strings.Repeat("a", 32)}, want: "origin"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePublicSecurity(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestValidatePublicSecurityAllowsLoopbackHTTP(t *testing.T) {
	cfg := Config{
		PublicURL: "http://127.0.0.1:8080", AuthUsername: "operator",
		AuthPassword:        "a-long-dashboard-password",
		HeroSMSWebhookToken: strings.Repeat("a", 32),
	}
	if err := ValidatePublicSecurity(cfg); err != nil {
		t.Fatalf("loopback development URL rejected: %v", err)
	}
}
