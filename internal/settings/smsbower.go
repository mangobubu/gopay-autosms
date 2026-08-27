package settings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mangobubu/gopay-autosms/internal/config"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

const (
	SMSBowerKey = "smsbower"
	HeroSMSKey  = "hero-sms"
)

type SMSBower struct {
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url"`
}

type HeroSMS struct {
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url"`
}

type storedSMSBower struct {
	APIKeyEncrypted string `json:"api_key_encrypted"`
}

type Manager struct {
	store           storage.SettingsStore
	box             *secure.Box
	smsBowerBaseURL string
	heroSMSBaseURL  string
}

// New preserves the original three-argument construction while accepting an
// optional HeroSMS endpoint for application wiring and HTTP fixture tests.
func New(store storage.SettingsStore, box *secure.Box, baseURL string, heroBaseURL ...string) *Manager {
	baseURL = defaultBaseURL(baseURL, config.DefaultSMSBaseURL)
	heroURL := ""
	if len(heroBaseURL) != 0 {
		heroURL = heroBaseURL[0]
	}
	heroURL = defaultBaseURL(heroURL, config.DefaultHeroSMSBaseURL)
	return &Manager{store: store, box: box, smsBowerBaseURL: baseURL, heroSMSBaseURL: heroURL}
}

func (m *Manager) GetSMSBower(ctx context.Context) (SMSBower, error) {
	value, err := m.getSMSProvider(ctx, SMSBowerKey, m.smsBowerBaseURL)
	return SMSBower(value), err
}

func (m *Manager) GetHeroSMS(ctx context.Context) (HeroSMS, error) {
	value, err := m.getSMSProvider(ctx, HeroSMSKey, m.heroSMSBaseURL)
	return HeroSMS(value), err
}

type smsProviderSetting struct {
	APIKey  string
	BaseURL string
}

func (m *Manager) getSMSProvider(ctx context.Context, key, baseURL string) (smsProviderSetting, error) {
	setting, err := m.store.GetSetting(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return smsProviderSetting{BaseURL: baseURL}, nil
	}
	if err != nil {
		return smsProviderSetting{}, err
	}
	var stored storedSMSBower
	if err := json.Unmarshal(setting.Value, &stored); err != nil {
		return smsProviderSetting{}, err
	}
	result := smsProviderSetting{BaseURL: baseURL}
	if stored.APIKeyEncrypted != "" {
		ciphertext, err := base64.StdEncoding.DecodeString(stored.APIKeyEncrypted)
		if err != nil {
			return smsProviderSetting{}, err
		}
		plain, err := m.box.Open(ciphertext)
		if err != nil {
			return smsProviderSetting{}, err
		}
		result.APIKey = string(plain)
	}
	return result, nil
}

func (m *Manager) SetSMSBower(ctx context.Context, value SMSBower) (SMSBower, error) {
	updated, err := m.setSMSProvider(ctx, SMSBowerKey, m.smsBowerBaseURL, smsProviderSetting(value))
	return SMSBower(updated), err
}

func (m *Manager) SetHeroSMS(ctx context.Context, value HeroSMS) (HeroSMS, error) {
	updated, err := m.setSMSProvider(ctx, HeroSMSKey, m.heroSMSBaseURL, smsProviderSetting(value))
	return HeroSMS(updated), err
}

func (m *Manager) setSMSProvider(ctx context.Context, key, baseURL string, value smsProviderSetting) (smsProviderSetting, error) {
	value.APIKey = strings.TrimSpace(value.APIKey)
	// The endpoint is service-owned. UI input is deliberately ignored so the
	// API key can never be redirected to an arbitrary host.
	value.BaseURL = baseURL
	// The settings endpoint returns a masked value. Treat that display value as
	// "unchanged" so saving another field never overwrites the real API key.
	if value.APIKey == "" || strings.Contains(value.APIKey, "*") {
		current, err := m.getSMSProvider(ctx, key, baseURL)
		if err != nil {
			return smsProviderSetting{}, err
		}
		value.APIKey = current.APIKey
	}
	ciphertext, err := m.box.Seal([]byte(value.APIKey))
	if err != nil {
		return smsProviderSetting{}, err
	}
	payload, err := json.Marshal(storedSMSBower{
		APIKeyEncrypted: base64.StdEncoding.EncodeToString(ciphertext),
	})
	if err != nil {
		return smsProviderSetting{}, err
	}
	if _, err = m.store.SetSetting(ctx, key, payload); err != nil {
		return smsProviderSetting{}, err
	}
	return value, nil
}

func defaultBaseURL(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func MaskAPIKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return strings.Repeat("*", len(value))
	}
	return value[:4] + strings.Repeat("*", len(value)-8) + value[len(value)-4:]
}
