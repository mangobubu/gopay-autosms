package main

import (
	"context"
	"errors"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/herotask"
	"github.com/mangobubu/gopay-autosms/internal/storage"
	"github.com/mangobubu/gopay-autosms/internal/workflow"
)

type legacyHeroSMSWebhookReceiver interface {
	ReceiveHeroSMSWebhook(context.Context, workflow.HeroSMSWebhookPayload) error
}

type heroSMSNumberMessageReceiver interface {
	ReceiveHeroSMSMessage(context.Context, herotask.ReceiveMessageInput) (storage.AppendHeroSMSTaskMessageResult, error)
}

// heroSMSWebhookFanout persists an authenticated callback in both independent
// domains. Both writes are idempotent, so returning either error asks HeroSMS
// to retry without duplicating the side which already committed.
type heroSMSWebhookFanout struct {
	legacy      legacyHeroSMSWebhookReceiver
	numberTasks heroSMSNumberMessageReceiver
}

func (receiver heroSMSWebhookFanout) ReceiveHeroSMSWebhook(
	ctx context.Context,
	payload workflow.HeroSMSWebhookPayload,
) error {
	var legacyErr error
	if receiver.legacy != nil {
		legacyErr = receiver.legacy.ReceiveHeroSMSWebhook(ctx, payload)
	}
	var numberTaskErr error
	if receiver.numberTasks != nil {
		_, numberTaskErr = receiver.numberTasks.ReceiveHeroSMSMessage(ctx, herotask.ReceiveMessageInput{
			ProviderActivationID: payload.ActivationID,
			Source:               domain.HeroSMSMessageWebhook,
			Code:                 payload.Code,
			Text:                 payload.Text,
			ProviderReceivedAt:   &payload.ReceivedAt,
			RawPayload:           payload.RawPayload,
		})
	}
	return errors.Join(legacyErr, numberTaskErr)
}
