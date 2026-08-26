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

const SMSBowerKey = "smsbower"

type SMSBower struct {
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url"`
}

type storedSMSBower struct {
	APIKeyEncrypted string `json:"api_key_encrypted"`
}

type Manager struct {
	store   storage.SettingsStore
	box     *secure.Box
	baseURL string
}

func New(store storage.SettingsStore, box *secure.Box, baseURL string) *Manager {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = config.DefaultSMSBaseURL
	}
	return &Manager{store: store, box: box, baseURL: baseURL}
}

func (m *Manager) GetSMSBower(ctx context.Context) (SMSBower, error) {
	setting, err := m.store.GetSetting(ctx, SMSBowerKey)
	if errors.Is(err, storage.ErrNotFound) {
		return SMSBower{BaseURL: m.baseURL}, nil
	}
	if err != nil {
		return SMSBower{}, err
	}
	var stored storedSMSBower
	if err := json.Unmarshal(setting.Value, &stored); err != nil {
		return SMSBower{}, err
	}
	result := SMSBower{BaseURL: m.baseURL}
	if stored.APIKeyEncrypted != "" {
		ciphertext, err := base64.StdEncoding.DecodeString(stored.APIKeyEncrypted)
		if err != nil {
			return SMSBower{}, err
		}
		plain, err := m.box.Open(ciphertext)
		if err != nil {
			return SMSBower{}, err
		}
		result.APIKey = string(plain)
	}
	return result, nil
}

func (m *Manager) SetSMSBower(ctx context.Context, value SMSBower) (SMSBower, error) {
	value.APIKey = strings.TrimSpace(value.APIKey)
	// The endpoint is service-owned. UI input is deliberately ignored so the
	// API key can never be redirected to an arbitrary host.
	value.BaseURL = m.baseURL
	// The settings endpoint returns a masked value. Treat that display value as
	// "unchanged" so saving another field never overwrites the real API key.
	if value.APIKey == "" || strings.Contains(value.APIKey, "*") {
		current, err := m.GetSMSBower(ctx)
		if err != nil {
			return SMSBower{}, err
		}
		value.APIKey = current.APIKey
	}
	ciphertext, err := m.box.Seal([]byte(value.APIKey))
	if err != nil {
		return SMSBower{}, err
	}
	payload, err := json.Marshal(storedSMSBower{
		APIKeyEncrypted: base64.StdEncoding.EncodeToString(ciphertext),
	})
	if err != nil {
		return SMSBower{}, err
	}
	if _, err = m.store.SetSetting(ctx, SMSBowerKey, payload); err != nil {
		return SMSBower{}, err
	}
	return value, nil
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
