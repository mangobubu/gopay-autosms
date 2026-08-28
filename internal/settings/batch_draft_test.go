package settings

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mangobubu/gopay-autosms/internal/secure"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

func TestBatchDraftEncryptedRoundTrip(t *testing.T) {
	box, err := secure.New("batch-draft-round-trip-test")
	if err != nil {
		t.Fatal(err)
	}
	store := &memorySettingsStore{}
	manager := New(store, box, "https://bower.test", "https://hero.test")
	draft := BatchDraft{
		SMSProvider: " HERO-SMS ",
		Service:     "go-secret-service",
		Country:     "6",
		PriceKey:    "price-key-17",
		Quantity:    3,
		PIN:         "123456",
		Proxy:       "socks5://user:pass@proxy.example:1080",
		PriceSnapshot: json.RawMessage(`{
			"value":"price-key-17","price":1.25
		}`),
	}

	storedDraft, err := manager.SetBatchDraft(context.Background(), draft)
	if err != nil {
		t.Fatal(err)
	}
	if storedDraft.SMSProvider != "hero-sms" {
		t.Fatalf("normalized provider = %q, want hero-sms", storedDraft.SMSProvider)
	}
	persisted := string(store.values[BatchDraftKey])
	for _, plaintext := range []string{
		"go-secret-service", "price-key-17", `"pin":"123456"`, "proxy.example", "price_snapshot",
	} {
		if strings.Contains(persisted, plaintext) {
			t.Fatalf("stored batch draft contains plaintext %q: %s", plaintext, persisted)
		}
	}

	loaded, err := manager.GetBatchDraft(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SMSProvider != "hero-sms" || loaded.Service != draft.Service ||
		loaded.Country != draft.Country || loaded.PriceKey != draft.PriceKey ||
		loaded.Quantity != draft.Quantity || loaded.PIN != draft.PIN || loaded.Proxy != draft.Proxy {
		t.Fatalf("loaded draft = %#v; want %#v", loaded, draft)
	}
	if !jsonEqual(loaded.PriceSnapshot, draft.PriceSnapshot) {
		t.Fatalf("loaded price snapshot = %s; want %s", loaded.PriceSnapshot, draft.PriceSnapshot)
	}
}

func TestGetBatchDraftReturnsEmptyDraftWhenMissing(t *testing.T) {
	box, err := secure.New("missing-batch-draft-test")
	if err != nil {
		t.Fatal(err)
	}
	manager := New(&memorySettingsStore{}, box, "https://bower.test")

	draft, err := manager.GetBatchDraft(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if draft.SMSProvider != "" || draft.Service != "" || draft.Country != "" ||
		draft.PriceKey != "" || draft.Quantity != 0 || draft.PIN != "" ||
		draft.Proxy != "" || len(draft.PriceSnapshot) != 0 {
		t.Fatalf("missing draft = %#v, want empty draft", draft)
	}
}

func TestSetBatchDraftValidatesPartialFields(t *testing.T) {
	box, err := secure.New("batch-draft-validation-test")
	if err != nil {
		t.Fatal(err)
	}
	manager := New(&memorySettingsStore{}, box, "https://bower.test")
	ctx := context.Background()

	for _, valid := range []BatchDraft{
		{},
		{SMSProvider: "smsbower", Quantity: 1, PIN: "1"},
		{SMSProvider: "hero-sms", Quantity: 100, PIN: "012345"},
	} {
		if _, err := manager.SetBatchDraft(ctx, valid); err != nil {
			t.Errorf("SetBatchDraft(%#v) = %v, want success", valid, err)
		}
	}

	for _, test := range []struct {
		name  string
		draft BatchDraft
	}{
		{name: "unknown provider", draft: BatchDraft{SMSProvider: "unknown"}},
		{name: "negative quantity", draft: BatchDraft{Quantity: -1}},
		{name: "quantity too large", draft: BatchDraft{Quantity: 101}},
		{name: "non-digit pin", draft: BatchDraft{PIN: "12a"}},
		{name: "pin too long", draft: BatchDraft{PIN: "1234567"}},
		{name: "invalid snapshot JSON", draft: BatchDraft{PriceSnapshot: json.RawMessage(`{"price":`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := manager.SetBatchDraft(ctx, test.draft); !errors.Is(err, storage.ErrInvalidInput) {
				t.Fatalf("SetBatchDraft(%#v) error = %v, want ErrInvalidInput", test.draft, err)
			}
		})
	}
}

func jsonEqual(left, right []byte) bool {
	var leftValue any
	var rightValue any
	return json.Unmarshal(left, &leftValue) == nil &&
		json.Unmarshal(right, &rightValue) == nil &&
		strings.TrimSpace(string(left)) != "" &&
		strings.TrimSpace(string(right)) != "" &&
		reflect.DeepEqual(leftValue, rightValue)
}
