package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mangobubu/gopay-autosms/internal/storage"
	"github.com/mangobubu/gopay-autosms/internal/workflow"
)

const (
	heroSMSWebhookBodyLimit = 64 << 10
	heroSMSWebhookTimeout   = 2 * time.Second
)

type HeroSMSWebhookReceiver interface {
	ReceiveHeroSMSWebhook(context.Context, workflow.HeroSMSWebhookPayload) error
}

type nullableString struct {
	Present bool
	Value   *string
}

type activationID string

func (value *activationID) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		*value = activationID(text)
		return nil
	}
	if len(raw) == 0 {
		return fmt.Errorf("activationId is empty")
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return fmt.Errorf("activationId must be a string or integer")
		}
	}
	*value = activationID(string(raw))
	return nil
}

func (value *nullableString) UnmarshalJSON(raw []byte) error {
	value.Present = true
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		value.Value = nil
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	value.Value = &text
	return nil
}

type heroSMSWebhookRequest struct {
	ActivationID activationID   `json:"activationId"`
	PhoneFrom    string         `json:"phoneFrom"`
	Service      string         `json:"service"`
	Text         nullableString `json:"text"`
	Code         *string        `json:"code"`
	Country      *int           `json:"country"`
	ReceivedAt   string         `json:"receivedAt"`
}

func (s *Server) receiveHeroSMSWebhook(c *gin.Context) {
	if !constantTimeEqual(c.Param("token"), s.webhookToken) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook token"})
		return
	}
	if s.webhookReceiver == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook receiver unavailable"})
		return
	}

	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "Content-Type must be application/json"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, heroSMSWebhookBodyLimit)
	rawPayload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "webhook payload exceeds 64 KiB"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read webhook payload"})
		return
	}

	var request heroSMSWebhookRequest
	decoder := json.NewDecoder(bytes.NewReader(rawPayload))
	if err = decoder.Decode(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid webhook JSON"})
		return
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "webhook body must contain one JSON object"})
		return
	}
	payload, err := request.payload(rawPayload)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), heroSMSWebhookTimeout)
	defer cancel()
	if err = s.webhookReceiver.ReceiveHeroSMSWebhook(ctx, payload); err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, storage.ErrInvalidInput) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": "webhook was not persisted"})
		return
	}
	c.Status(http.StatusOK)
}

func (request heroSMSWebhookRequest) payload(rawPayload []byte) (workflow.HeroSMSWebhookPayload, error) {
	request.ActivationID = activationID(strings.TrimSpace(string(request.ActivationID)))
	request.PhoneFrom = strings.TrimSpace(request.PhoneFrom)
	request.Service = strings.TrimSpace(request.Service)
	request.ReceivedAt = strings.TrimSpace(request.ReceivedAt)
	switch {
	case request.ActivationID == "":
		return workflow.HeroSMSWebhookPayload{}, fmt.Errorf("activationId is required")
	case request.Service == "":
		return workflow.HeroSMSWebhookPayload{}, fmt.Errorf("service is required")
	case len(request.Service) < 2 || len(request.Service) > 4:
		return workflow.HeroSMSWebhookPayload{}, fmt.Errorf("service must contain 2 to 4 characters")
	case !request.Text.Present:
		return workflow.HeroSMSWebhookPayload{}, fmt.Errorf("text is required")
	case request.Country == nil:
		return workflow.HeroSMSWebhookPayload{}, fmt.Errorf("country is required")
	case *request.Country < 0 || *request.Country > 999:
		return workflow.HeroSMSWebhookPayload{}, fmt.Errorf("country must be between 0 and 999")
	case request.ReceivedAt == "":
		return workflow.HeroSMSWebhookPayload{}, fmt.Errorf("receivedAt is required")
	}
	receivedAt, err := time.Parse(time.RFC3339, request.ReceivedAt)
	if err != nil {
		receivedAt, err = time.ParseInLocation("2006-01-02 15:04:05", request.ReceivedAt, time.UTC)
	}
	if err != nil {
		return workflow.HeroSMSWebhookPayload{}, fmt.Errorf("receivedAt must use RFC3339 or YYYY-MM-DD HH:MM:SS")
	}
	return workflow.HeroSMSWebhookPayload{
		ActivationID: string(request.ActivationID),
		PhoneFrom:    request.PhoneFrom,
		Service:      request.Service,
		Text:         request.Text.Value,
		Code:         request.Code,
		Country:      *request.Country,
		ReceivedAt:   receivedAt,
		RawPayload:   append(json.RawMessage(nil), rawPayload...),
	}, nil
}
