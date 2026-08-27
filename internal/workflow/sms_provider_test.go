package workflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/herosms"
	"github.com/mangobubu/gopay-autosms/internal/secure"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/smsprovider"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type smsProviderSettingsStore struct {
	storage.Store
	settings map[string]domain.Setting
	reads    []string
}

func (s *smsProviderSettingsStore) GetSetting(_ context.Context, key string) (domain.Setting, error) {
	s.reads = append(s.reads, key)
	setting, ok := s.settings[key]
	if !ok {
		return domain.Setting{}, storage.ErrNotFound
	}
	return setting, nil
}

func TestSMSClientDispatchesByPersistedProviderAndLegacyEmptyDefaultsToSMSBower(t *testing.T) {
	box, err := secure.New("workflow-provider-dispatch-test")
	if err != nil {
		t.Fatal(err)
	}
	stored := make(map[string]domain.Setting)
	for key, plain := range map[string]string{
		appsettings.SMSBowerKey: "bower-key",
		appsettings.HeroSMSKey:  "hero-key",
	} {
		ciphertext, sealErr := box.Seal([]byte(plain))
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		value, marshalErr := json.Marshal(map[string]string{
			"api_key_encrypted": base64.StdEncoding.EncodeToString(ciphertext),
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		stored[key] = domain.Setting{Key: key, Value: value}
	}
	store := &smsProviderSettingsStore{settings: stored}
	settingsManager := appsettings.New(store, box, "https://bower.test", "https://hero.test")
	manager := New(store, settingsManager, box, Config{}, nil)

	legacyClient, err := manager.smsClient(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := legacyClient.(*smsbower.Client); !ok {
		t.Fatalf("legacy empty provider client = %T, want *smsbower.Client", legacyClient)
	}
	heroClient, err := manager.smsClient(context.Background(), smsprovider.HeroSMS)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := heroClient.(*herosms.Client); !ok {
		t.Fatalf("HeroSMS provider client = %T, want *herosms.Client", heroClient)
	}
	if len(store.reads) != 2 || store.reads[0] != appsettings.SMSBowerKey || store.reads[1] != appsettings.HeroSMSKey {
		t.Fatalf("settings reads = %v, want isolated provider keys", store.reads)
	}
}
