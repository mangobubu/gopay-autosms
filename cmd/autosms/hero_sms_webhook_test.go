package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/herotask"
	"github.com/mangobubu/gopay-autosms/internal/storage"
	"github.com/mangobubu/gopay-autosms/internal/workflow"
)

type legacyWebhookStub struct {
	payload workflow.HeroSMSWebhookPayload
	err     error
}

func (stub *legacyWebhookStub) ReceiveHeroSMSWebhook(_ context.Context, payload workflow.HeroSMSWebhookPayload) error {
	stub.payload = payload
	return stub.err
}

type numberWebhookStub struct {
	input herotask.ReceiveMessageInput
	err   error
}

func (stub *numberWebhookStub) ReceiveHeroSMSMessage(_ context.Context, input herotask.ReceiveMessageInput) (storage.AppendHeroSMSTaskMessageResult, error) {
	stub.input = input
	return storage.AppendHeroSMSTaskMessageResult{}, stub.err
}

func TestHeroSMSWebhookFanoutPersistsBothDomains(t *testing.T) {
	receivedAt := time.Date(2026, 8, 28, 12, 30, 0, 0, time.UTC)
	code, text := "123456", "Your code is 123456"
	payload := workflow.HeroSMSWebhookPayload{
		ActivationID: "hero-42", Code: &code, Text: &text,
		ReceivedAt: receivedAt, RawPayload: json.RawMessage(`{"activationId":"hero-42"}`),
	}
	legacy, number := &legacyWebhookStub{}, &numberWebhookStub{}
	receiver := heroSMSWebhookFanout{legacy: legacy, numberTasks: number}

	if err := receiver.ReceiveHeroSMSWebhook(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if legacy.payload.ActivationID != payload.ActivationID {
		t.Fatalf("legacy activation = %q", legacy.payload.ActivationID)
	}
	if number.input.ProviderActivationID != payload.ActivationID || number.input.Code == nil || *number.input.Code != code {
		t.Fatalf("number-task input = %+v", number.input)
	}
	if number.input.ProviderReceivedAt == nil || !number.input.ProviderReceivedAt.Equal(receivedAt) {
		t.Fatalf("number-task timestamp = %v", number.input.ProviderReceivedAt)
	}
}

func TestHeroSMSWebhookFanoutAttemptsBothAndJoinsErrors(t *testing.T) {
	legacyErr := errors.New("legacy write")
	numberErr := errors.New("number write")
	legacy := &legacyWebhookStub{err: legacyErr}
	number := &numberWebhookStub{err: numberErr}
	receiver := heroSMSWebhookFanout{legacy: legacy, numberTasks: number}

	err := receiver.ReceiveHeroSMSWebhook(context.Background(), workflow.HeroSMSWebhookPayload{
		ActivationID: "hero-42", ReceivedAt: time.Now(), RawPayload: json.RawMessage(`{}`),
	})
	if !errors.Is(err, legacyErr) || !errors.Is(err, numberErr) {
		t.Fatalf("joined error = %v", err)
	}
	if number.input.ProviderActivationID != "hero-42" {
		t.Fatal("number-task receiver was not attempted after legacy failure")
	}
}
