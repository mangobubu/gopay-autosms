package settings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mangobubu/gopay-autosms/internal/smsprovider"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

const BatchDraftKey = "batch-draft"

// BatchDraft is the server-side copy of the unfinished batch form. The whole
// value is encrypted before it is written to the settings table because PIN
// and proxy entries are sensitive.
type BatchDraft struct {
	SMSProvider   string          `json:"sms_provider"`
	Service       string          `json:"service"`
	Country       string          `json:"country"`
	PriceKey      string          `json:"price_key"`
	Quantity      int             `json:"quantity"`
	PIN           string          `json:"pin"`
	Proxy         string          `json:"proxy"`
	PriceSnapshot json.RawMessage `json:"price_snapshot"`
}

type storedBatchDraft struct {
	DraftEncrypted string `json:"draft_encrypted"`
}

// GetBatchDraft returns an empty draft when the user has not saved one yet.
func (m *Manager) GetBatchDraft(ctx context.Context) (BatchDraft, error) {
	setting, err := m.store.GetSetting(ctx, BatchDraftKey)
	if errors.Is(err, storage.ErrNotFound) {
		return BatchDraft{}, nil
	}
	if err != nil {
		return BatchDraft{}, err
	}

	var stored storedBatchDraft
	if err := json.Unmarshal(setting.Value, &stored); err != nil {
		return BatchDraft{}, fmt.Errorf("decode batch draft setting: %w", err)
	}
	if strings.TrimSpace(stored.DraftEncrypted) == "" {
		return BatchDraft{}, fmt.Errorf("decode batch draft setting: encrypted draft is empty")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(stored.DraftEncrypted)
	if err != nil {
		return BatchDraft{}, fmt.Errorf("decode batch draft ciphertext: %w", err)
	}
	plain, err := m.box.Open(ciphertext)
	if err != nil {
		return BatchDraft{}, err
	}

	var draft BatchDraft
	if err := json.Unmarshal(plain, &draft); err != nil {
		return BatchDraft{}, fmt.Errorf("decode batch draft: %w", err)
	}
	return normalizeBatchDraft(draft)
}

// SetBatchDraft validates and persists the complete form as one encrypted
// value. Partial drafts are valid: quantity may be zero and PIN may contain
// zero to six digits while the user is still typing.
func (m *Manager) SetBatchDraft(ctx context.Context, draft BatchDraft) (BatchDraft, error) {
	normalized, err := normalizeBatchDraft(draft)
	if err != nil {
		return BatchDraft{}, err
	}
	plain, err := json.Marshal(normalized)
	if err != nil {
		return BatchDraft{}, fmt.Errorf("encode batch draft: %w", err)
	}
	ciphertext, err := m.box.Seal(plain)
	if err != nil {
		return BatchDraft{}, err
	}
	payload, err := json.Marshal(storedBatchDraft{
		DraftEncrypted: base64.StdEncoding.EncodeToString(ciphertext),
	})
	if err != nil {
		return BatchDraft{}, fmt.Errorf("encode encrypted batch draft: %w", err)
	}
	if _, err := m.store.SetSetting(ctx, BatchDraftKey, payload); err != nil {
		return BatchDraft{}, err
	}
	return normalized, nil
}

func normalizeBatchDraft(draft BatchDraft) (BatchDraft, error) {
	provider := strings.TrimSpace(draft.SMSProvider)
	if provider != "" {
		normalized, err := smsprovider.Normalize(provider)
		if err != nil {
			return BatchDraft{}, fmt.Errorf("%w: %v", storage.ErrInvalidInput, err)
		}
		draft.SMSProvider = normalized
	} else {
		draft.SMSProvider = ""
	}

	if draft.Quantity < 0 || draft.Quantity > 100 {
		return BatchDraft{}, fmt.Errorf("%w: quantity must be empty or between 1 and 100", storage.ErrInvalidInput)
	}
	if !validDraftPIN(draft.PIN) {
		return BatchDraft{}, fmt.Errorf("%w: pin must contain at most 6 digits", storage.ErrInvalidInput)
	}
	if len(draft.PriceSnapshot) != 0 && !json.Valid(draft.PriceSnapshot) {
		return BatchDraft{}, fmt.Errorf("%w: price_snapshot must be valid JSON", storage.ErrInvalidInput)
	}
	if len(draft.PriceSnapshot) != 0 {
		draft.PriceSnapshot = append(json.RawMessage(nil), draft.PriceSnapshot...)
	}
	return draft, nil
}

func validDraftPIN(pin string) bool {
	if len(pin) > 6 {
		return false
	}
	for _, digit := range pin {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
