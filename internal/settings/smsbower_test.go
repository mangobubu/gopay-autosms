package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type memorySettingsStore struct {
	values map[string]json.RawMessage
}

func (s *memorySettingsStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	value, ok := s.values[key]
	if !ok {
		return domain.Setting{}, storage.ErrNotFound
	}
	return domain.Setting{Key: key, Value: value}, nil
}

func (s *memorySettingsStore) SetSetting(_ context.Context, key string, value json.RawMessage) (domain.Setting, error) {
	if s.values == nil {
		s.values = make(map[string]json.RawMessage)
	}
	cloned := append(json.RawMessage(nil), value...)
	s.values[key] = cloned
	return domain.Setting{Key: key, Value: cloned}, nil
}

func (s *memorySettingsStore) ListSettings(context.Context) ([]domain.Setting, error) {
	return nil, nil
}

func TestProviderKeysAreEncryptedIsolatedAndMaskedSavePreservesEachKey(t *testing.T) {
	box, err := secure.New("provider-settings-isolation-test")
	if err != nil {
		t.Fatal(err)
	}
	store := &memorySettingsStore{}
	manager := New(store, box, "https://bower.test", "https://hero.test")
	ctx := context.Background()

	if _, err = manager.SetSMSBower(ctx, SMSBower{APIKey: "bower-key-123456"}); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.SetHeroSMS(ctx, HeroSMS{APIKey: "hero-key-987654"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(store.values[SMSBowerKey]), "bower-key-123456") || strings.Contains(string(store.values[HeroSMSKey]), "hero-key-987654") {
		t.Fatal("stored settings contain a plaintext API key")
	}
	if string(store.values[SMSBowerKey]) == string(store.values[HeroSMSKey]) {
		t.Fatal("provider settings unexpectedly share one ciphertext")
	}

	bower, err := manager.GetSMSBower(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hero, err := manager.GetHeroSMS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bower.APIKey != "bower-key-123456" || hero.APIKey != "hero-key-987654" {
		t.Fatalf("keys = bower:%q hero:%q", bower.APIKey, hero.APIKey)
	}
	if bower.BaseURL != "https://bower.test" || hero.BaseURL != "https://hero.test" {
		t.Fatalf("base URLs = bower:%q hero:%q", bower.BaseURL, hero.BaseURL)
	}

	if _, err = manager.SetHeroSMS(ctx, HeroSMS{APIKey: MaskAPIKey(hero.APIKey)}); err != nil {
		t.Fatal(err)
	}
	bower, _ = manager.GetSMSBower(ctx)
	hero, _ = manager.GetHeroSMS(ctx)
	if bower.APIKey != "bower-key-123456" || hero.APIKey != "hero-key-987654" {
		t.Fatalf("masked save changed provider keys: bower:%q hero:%q", bower.APIKey, hero.APIKey)
	}
}
