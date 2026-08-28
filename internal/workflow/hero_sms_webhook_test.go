package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/smsprovider"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

type heroSMSWorkflowStore struct {
	*verificationResendFlowStore

	heroMu    sync.Mutex
	events    []domain.HeroSMSWebhookEvent
	ingested  []storage.IngestHeroSMSWebhookParams
	nextRuns  []time.Time
	completed []int64
	ignored   map[int64]string
	failed    map[int64]string
}

func (s *heroSMSWorkflowStore) IngestHeroSMSWebhook(
	_ context.Context,
	params storage.IngestHeroSMSWebhookParams,
) (storage.IngestHeroSMSWebhookResult, error) {
	s.heroMu.Lock()
	defer s.heroMu.Unlock()
	s.ingested = append(s.ingested, params)
	event := domain.HeroSMSWebhookEvent{
		ID:                   int64(len(s.events) + 1),
		ProviderActivationID: params.ProviderActivationID,
		Code:                 params.Code,
		Text:                 params.Text,
		PhoneNumber:          params.PhoneNumber,
		ServiceCode:          params.ServiceCode,
		CountryCode:          params.CountryCode,
		ProviderReceivedAt:   params.ProviderReceivedAt,
		RawPayload:           append(json.RawMessage(nil), params.RawPayload...),
		Status:               domain.HeroSMSWebhookEventReceived,
		NextAttemptAt:        time.Now().UTC().Add(-time.Second),
		ReceivedAt:           time.Now().UTC(),
	}
	s.events = append(s.events, event)
	return storage.IngestHeroSMSWebhookResult{Event: event, Inserted: true}, nil
}

func (s *heroSMSWorkflowStore) ClaimNextHeroSMSWebhookEventOwned(
	_ context.Context,
	activationID int64,
	owner string,
	leaseVersion int64,
	now time.Time,
) (domain.HeroSMSWebhookEvent, error) {
	s.heroMu.Lock()
	defer s.heroMu.Unlock()
	if activationID != s.activation.ID || owner != s.activation.LeaseOwner || leaseVersion != s.activation.LeaseVersion {
		return domain.HeroSMSWebhookEvent{}, storage.ErrConflict
	}
	for index := range s.events {
		event := &s.events[index]
		claimable := event.Status == domain.HeroSMSWebhookEventReceived && !event.NextAttemptAt.After(now)
		if event.Status == domain.HeroSMSWebhookEventProcessing &&
			(event.ClaimedLeaseOwner != owner || event.ClaimedLeaseVersion != leaseVersion) {
			claimable = true
		}
		if claimable {
			event.Status = domain.HeroSMSWebhookEventProcessing
			event.Attempts++
			event.ClaimedLeaseOwner = owner
			event.ClaimedLeaseVersion = leaseVersion
			id := activationID
			event.ActivationID = &id
			return *event, nil
		}
	}
	return domain.HeroSMSWebhookEvent{}, storage.ErrNotFound
}

func (s *heroSMSWorkflowStore) CompleteHeroSMSWebhookEventOwned(
	_ context.Context, eventID, activationID int64, owner string, leaseVersion int64,
) error {
	s.heroMu.Lock()
	defer s.heroMu.Unlock()
	event, err := s.ownedHeroEvent(eventID, activationID, owner, leaseVersion)
	if err != nil {
		return err
	}
	event.Status = domain.HeroSMSWebhookEventProcessed
	s.completed = append(s.completed, eventID)
	return nil
}

func (s *heroSMSWorkflowStore) IgnoreHeroSMSWebhookEventOwned(
	_ context.Context, eventID, activationID int64, owner string, leaseVersion int64, reason string,
) error {
	s.heroMu.Lock()
	defer s.heroMu.Unlock()
	event, err := s.ownedHeroEvent(eventID, activationID, owner, leaseVersion)
	if err != nil {
		return err
	}
	event.Status = domain.HeroSMSWebhookEventIgnored
	event.LastError = reason
	if s.ignored == nil {
		s.ignored = make(map[int64]string)
	}
	s.ignored[eventID] = reason
	return nil
}

func (s *heroSMSWorkflowStore) FailHeroSMSWebhookEventOwned(
	_ context.Context,
	eventID, activationID int64,
	owner string,
	leaseVersion int64,
	retryAt time.Time,
	reason string,
) error {
	s.heroMu.Lock()
	defer s.heroMu.Unlock()
	event, err := s.ownedHeroEvent(eventID, activationID, owner, leaseVersion)
	if err != nil {
		return err
	}
	event.Status = domain.HeroSMSWebhookEventReceived
	event.NextAttemptAt = retryAt
	event.LastError = reason
	if s.failed == nil {
		s.failed = make(map[int64]string)
	}
	s.failed[eventID] = reason
	return nil
}

func (s *heroSMSWorkflowStore) ownedHeroEvent(
	eventID, activationID int64, owner string, leaseVersion int64,
) (*domain.HeroSMSWebhookEvent, error) {
	if activationID != s.activation.ID || owner != s.activation.LeaseOwner || leaseVersion != s.activation.LeaseVersion {
		return nil, storage.ErrConflict
	}
	for index := range s.events {
		if s.events[index].ID == eventID && s.events[index].Status == domain.HeroSMSWebhookEventProcessing {
			return &s.events[index], nil
		}
	}
	return nil, storage.ErrConflict
}

func (s *heroSMSWorkflowStore) TouchActivationPoll(
	_ context.Context,
	id int64,
	owner string,
	_ time.Time,
	nextRunAt time.Time,
) error {
	if id != s.activation.ID || owner != s.activation.LeaseOwner {
		return storage.ErrConflict
	}
	s.heroMu.Lock()
	defer s.heroMu.Unlock()
	s.nextRuns = append(s.nextRuns, nextRunAt)
	s.activation.NextRunAt = nextRunAt
	return nil
}

func (s *heroSMSWorkflowStore) ListVerificationCodes(_ context.Context, activationID int64) ([]domain.VerificationCode, error) {
	if activationID != s.activation.ID {
		return nil, storage.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]domain.VerificationCode, 0, len(s.verifications))
	for index, params := range s.verifications {
		result = append(result, domain.VerificationCode{
			ID:                 int64(index + 1),
			ActivationID:       params.ActivationID,
			CycleNo:            params.CycleNo,
			Phase:              params.Phase,
			Code:               params.Code,
			ProviderPayload:    append(json.RawMessage(nil), params.ProviderPayload...),
			ProviderReceivedAt: params.ProviderReceivedAt,
		})
	}
	return result, nil
}

func configureHeroSMSWorkflow(
	t *testing.T,
	manager *Manager,
	base *verificationResendFlowStore,
	boxProviderURL string,
) *heroSMSWorkflowStore {
	t.Helper()
	base.setting.Key = appsettings.HeroSMSKey
	base.activation.Provider = smsprovider.HeroSMS
	store := &heroSMSWorkflowStore{verificationResendFlowStore: base}
	manager.store = store
	manager.settings = appsettings.New(store, manager.box, "https://smsbower.invalid", boxProviderURL)
	return store
}

func TestReceiveHeroSMSWebhookPersistsNullablePayloadAndRawBody(t *testing.T) {
	base := &verificationResendFlowStore{activation: domain.Activation{ID: 7, LeaseOwner: "worker", LeaseVersion: 1}}
	store := &heroSMSWorkflowStore{verificationResendFlowStore: base}
	manager := &Manager{store: store}
	text := "Your code is 1234"
	receivedAt := time.Date(2026, 8, 28, 1, 2, 3, 0, time.FixedZone("fixture", 7*60*60))
	raw := json.RawMessage(`{"activationId":"hero-7","text":"Your code is 1234","code":null}`)

	if err := manager.ReceiveHeroSMSWebhook(context.Background(), HeroSMSWebhookPayload{
		ActivationID: " hero-7 ", PhoneFrom: " 628123 ", Service: " go ", Text: &text,
		Code: nil, Country: 6, ReceivedAt: receivedAt, RawPayload: raw,
	}); err != nil {
		t.Fatal(err)
	}

	if len(store.ingested) != 1 {
		t.Fatalf("ingested callbacks = %d, want 1", len(store.ingested))
	}
	got := store.ingested[0]
	if got.ProviderActivationID != "hero-7" || got.Code != nil || got.Text == nil || *got.Text != text ||
		got.PhoneNumber != "628123" || got.ServiceCode != "go" || got.CountryCode != "6" {
		t.Fatalf("ingested callback = %#v", got)
	}
	if got.ProviderReceivedAt == nil || !got.ProviderReceivedAt.Equal(receivedAt.UTC()) {
		t.Fatalf("provider received at = %v, want %s", got.ProviderReceivedAt, receivedAt.UTC())
	}
	if string(got.RawPayload) != string(raw) {
		t.Fatalf("raw payload = %s, want %s", got.RawPayload, raw)
	}
}

func TestHeroSMSLoginWaitUsesWebhookDeadlineWithoutProviderPoll(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	sentAt := time.Now().UTC().Add(-5 * time.Second)
	manager, base, _ := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-login-wait"),
		LoginStage: gopay.LoginStageAwaiting1FAOTP, LoginCodeSentAt: sentAt,
	}, domain.ActivationStatusAwaitingLoginCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)

	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("external calls = %v, want none", got)
	}
	want := sentAt.Add(loginVerificationCodeWait)
	if len(store.nextRuns) != 1 || !store.nextRuns[0].Equal(want) {
		t.Fatalf("next runs = %v, want [%s]", store.nextRuns, want)
	}
}

func TestHeroSMSLoginTimeoutResendsWithoutGetStatus(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-login-timeout"),
		VerificationID: "login-verification-1", Methods: []string{"otp_sms"},
		LoginStage:      gopay.LoginStageAwaiting1FAOTP,
		LoginCodeSentAt: time.Now().UTC().Add(-loginVerificationCodeWait - time.Second),
	}, domain.ActivationStatusAwaitingLoginCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)

	for step := 0; step < 3; step++ {
		if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
	}
	if got, want := events.snapshot(), []string{"provider:setStatus:3", "gopay:initiate:login_1fa"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageAwaiting1FAOTP || state.LoginCodeResends != 1 {
		t.Fatalf("login state = stage %q resends %d", state.LoginStage, state.LoginCodeResends)
	}
}

func TestHeroSMSPINWaitUsesEightySecondDeadlineWithoutProviderPoll(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	sentAt := time.Now().UTC().Add(-5 * time.Second)
	manager, base, _ := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-pin-wait"),
		AccessToken: "access-token", PINVerificationID: "pin-verification-1", PINOTPToken: "pin-token-1",
		PINStage: gopay.PINStageAwaiting, PINCodeSentAt: sentAt,
	}, domain.ActivationStatusAwaitingPINCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)

	if err := manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("external calls = %v, want none", got)
	}
	want := sentAt.Add(pinVerificationCodeWait)
	if len(store.nextRuns) != 1 || !store.nextRuns[0].Equal(want) {
		t.Fatalf("next runs = %v, want [%s]", store.nextRuns, want)
	}
}

func TestHeroSMSFollowupWaitOpensNextCycleThenSleepsUntilProviderExpiry(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-followup-wait"),
		AccessToken: "access-token", LoginStage: gopay.LoginStageAuthenticated, PINStage: gopay.PINStageComplete,
	}, domain.ActivationStatusAwaitingSubsequentCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	expiresAt := time.Now().UTC().Add(12 * time.Minute)
	store.activation.ProviderExpiresAt = &expiresAt
	store.verificationResendFlowStore.verifications = []storage.AppendVerificationParams{{
		ActivationID: store.activation.ID, CycleNo: 0, Phase: domain.VerificationPhasePIN, Code: "1111",
	}}

	if err := manager.pollFollowupCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got, want := events.snapshot(), []string{"provider:setStatus:3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("external calls = %v, want %v", got, want)
	}
	if state := store.session(t, box); state.SMSCycle != 1 {
		t.Fatalf("session cycle = %d, want 1", state.SMSCycle)
	}
	if len(store.nextRuns) != 1 || !store.nextRuns[0].Equal(expiresAt) {
		t.Fatalf("next runs = %v, want [%s]", store.nextRuns, expiresAt)
	}
}

func TestHeroSMSFirstFollowupCodeUsesFreshCycleAndIsNotLostToPINUniqueKey(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	manager, base, _ := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-followup-code"),
		AccessToken: "access-token", LoginStage: gopay.LoginStageAuthenticated, PINStage: gopay.PINStageComplete,
		SMSCycle: 1,
	}, domain.ActivationStatusAwaitingSubsequentCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	store.activation.SMSCycle = 1
	store.verificationResendFlowStore.verifications = []storage.AppendVerificationParams{{
		ActivationID: store.activation.ID, CycleNo: 0, Phase: domain.VerificationPhasePIN, Code: "1111",
	}}
	receivedAt := time.Now().UTC()
	store.events = []domain.HeroSMSWebhookEvent{{
		ID: 21, Code: stringPointer("2222"), Status: domain.HeroSMSWebhookEventReceived,
		NextAttemptAt: receivedAt.Add(-time.Second), ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt,
		RawPayload: json.RawMessage(`{"code":"2222"}`),
	}}

	if err := manager.pollFollowupCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	base.mu.Lock()
	verifications := append([]storage.AppendVerificationParams(nil), base.verifications...)
	base.mu.Unlock()
	if len(verifications) != 2 || verifications[1].CycleNo != 1 ||
		verifications[1].Phase != domain.VerificationPhaseSubsequent || verifications[1].Code != "2222" {
		t.Fatalf("verifications = %#v", verifications)
	}
	if !reflect.DeepEqual(store.completed, []int64{21}) {
		t.Fatalf("completed events = %v", store.completed)
	}
	if got, want := events.snapshot(), []string{"provider:setStatus:3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("external calls = %v, want %v", got, want)
	}
}

func TestHeroSMSLoginConsumesInboxCodeAndPersistsRawMetadata(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	sentAt := time.Now().UTC().Add(-time.Second)
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-login-code"),
		VerificationID: "login-verification-1", OTPToken: "login-token-1",
		LoginStage: gopay.LoginStageAwaiting1FAOTP, LoginCodeSentAt: sentAt,
	}, domain.ActivationStatusAwaitingLoginCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	receivedAt := time.Now().UTC()
	store.events = []domain.HeroSMSWebhookEvent{{
		ID: 12, Code: stringPointer("1234"), Text: stringPointer("Your code is 1234"), RawPayload: json.RawMessage(`{"code":"1234","text":"Your code is 1234"}`),
		Status: domain.HeroSMSWebhookEventReceived, NextAttemptAt: receivedAt.Add(-time.Second),
		ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt,
	}}

	if err := manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"gopay:verify:login_1fa:1234", "gopay:accountList", "gopay:token:cvs"}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("external calls = %v, want %v", got, wantCalls)
	}
	if !reflect.DeepEqual(store.completed, []int64{12}) {
		t.Fatalf("completed events = %v", store.completed)
	}
	if state := store.session(t, box); state.LoginStage != gopay.LoginStageAuthenticated {
		t.Fatalf("login stage = %q, want authenticated", state.LoginStage)
	}
	base.mu.Lock()
	defer base.mu.Unlock()
	if len(base.verifications) != 1 {
		t.Fatalf("verification count = %d, want 1", len(base.verifications))
	}
	var metadata heroSMSVerificationPayload
	if err := json.Unmarshal(base.verifications[0].ProviderPayload, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Source != smsprovider.HeroSMS || metadata.WebhookEventID != 12 ||
		string(metadata.Payload) != string(store.events[0].RawPayload) {
		t.Fatalf("verification metadata = %#v", metadata)
	}
}

func TestHeroSMSLoginIgnoresDifferentCallbackWhenCycleAlreadyOccupied(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-login-cycle-occupied"),
		VerificationID: "login-verification-1", OTPToken: "login-token-1",
		LoginStage: gopay.LoginStageAwaiting1FAOTP, LoginCodeSentAt: time.Now().UTC().Add(-time.Second),
	}, domain.ActivationStatusAwaitingLoginCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	receivedAt := time.Now().UTC()
	metadata, err := json.Marshal(heroSMSVerificationPayload{
		Source: smsprovider.HeroSMS, WebhookEventID: 61, Payload: json.RawMessage(`{"code":"1234"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	base.verifications = []storage.AppendVerificationParams{{
		ActivationID: base.activation.ID, CycleNo: 0, Phase: domain.VerificationPhaseLogin,
		Code: "1234", ProviderPayload: metadata, ProviderReceivedAt: &receivedAt,
	}}
	store.events = []domain.HeroSMSWebhookEvent{{
		ID: 62, Code: stringPointer("1234"), Status: domain.HeroSMSWebhookEventReceived,
		NextAttemptAt: receivedAt.Add(-time.Second), ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt,
	}}

	if err = manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("external calls = %v, want no OTP replay", got)
	}
	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageAwaiting1FAOTP || state.LoginCodeResends != 0 {
		t.Fatalf("login state = stage %q resends %d", state.LoginStage, state.LoginCodeResends)
	}
	if store.events[0].Status != domain.HeroSMSWebhookEventIgnored || store.ignored[62] == "" {
		t.Fatalf("duplicate event = %#v, ignored = %v", store.events[0], store.ignored)
	}
}

func TestHeroSMSPINIgnoresDifferentCallbackWhenCycleAlreadyOccupied(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-pin-cycle-occupied"),
		AccessToken: "access-token", PINVerificationID: "pin-verification-1", PINOTPToken: "pin-token-1",
		PINStage: gopay.PINStageAwaiting, PINCodeSentAt: time.Now().UTC().Add(-time.Second),
	}, domain.ActivationStatusAwaitingPINCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	receivedAt := time.Now().UTC()
	metadata, err := json.Marshal(heroSMSVerificationPayload{
		Source: smsprovider.HeroSMS, WebhookEventID: 71, Payload: json.RawMessage(`{"code":"5678"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	base.verifications = []storage.AppendVerificationParams{{
		ActivationID: base.activation.ID, CycleNo: 0, Phase: domain.VerificationPhasePIN,
		Code: "5678", ProviderPayload: metadata, ProviderReceivedAt: &receivedAt,
	}}
	store.events = []domain.HeroSMSWebhookEvent{{
		ID: 72, Code: stringPointer("5678"), Status: domain.HeroSMSWebhookEventReceived,
		NextAttemptAt: receivedAt.Add(-time.Second), ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt,
	}}

	if err = manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("external calls = %v, want no OTP replay", got)
	}
	state := store.session(t, box)
	if state.PINStage != gopay.PINStageAwaiting || state.PINCodeResends != 0 {
		t.Fatalf("PIN state = stage %q resends %d", state.PINStage, state.PINCodeResends)
	}
	if store.events[0].Status != domain.HeroSMSWebhookEventIgnored || store.ignored[72] == "" {
		t.Fatalf("duplicate event = %#v, ignored = %v", store.events[0], store.ignored)
	}
}

func TestHeroSMSFollowupWaitsForOriginalOccupiedCycleEvent(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-followup-cycle-occupied"),
		AccessToken: "access-token", LoginStage: gopay.LoginStageAuthenticated, PINStage: gopay.PINStageComplete,
		SMSCycle: 1,
	}, domain.ActivationStatusAwaitingSubsequentCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	store.activation.SMSCycle = 1
	receivedAt := time.Now().UTC()
	metadata, err := json.Marshal(heroSMSVerificationPayload{
		Source: smsprovider.HeroSMS, WebhookEventID: 81, Payload: json.RawMessage(`{"code":"1111"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	base.verifications = []storage.AppendVerificationParams{{
		ActivationID: base.activation.ID, CycleNo: 1, Phase: domain.VerificationPhaseSubsequent,
		Code: "1111", ProviderPayload: metadata, ProviderReceivedAt: &receivedAt,
	}}
	store.events = []domain.HeroSMSWebhookEvent{
		{ID: 81, Code: stringPointer("1111"), Status: domain.HeroSMSWebhookEventReceived,
			NextAttemptAt: receivedAt.Add(time.Minute), ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt},
		{ID: 82, Code: stringPointer("1111"), Status: domain.HeroSMSWebhookEventReceived,
			NextAttemptAt: receivedAt.Add(-time.Second), ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt},
	}

	if err = manager.pollFollowupCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("external calls after duplicate = %v, want none", got)
	}
	if store.events[1].Status != domain.HeroSMSWebhookEventIgnored || store.activation.SMSCycle != 1 {
		t.Fatalf("duplicate event = %#v, cycle = %d", store.events[1], store.activation.SMSCycle)
	}

	store.events[0].NextAttemptAt = time.Now().UTC().Add(-time.Second)
	if err = manager.pollFollowupCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got, want := events.snapshot(), []string{"provider:setStatus:3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("external calls = %v, want %v", got, want)
	}
	if store.activation.SMSCycle != 2 || store.events[0].Status != domain.HeroSMSWebhookEventProcessed {
		t.Fatalf("cycle = %d, original event = %#v", store.activation.SMSCycle, store.events[0])
	}
	if state := store.session(t, box); state.SMSCycle != 2 {
		t.Fatalf("session cycle = %d, want 2", state.SMSCycle)
	}
}

func TestHeroSMSInboxIgnoresNullAndStaleEventsBeforeReturningCurrentCode(t *testing.T) {
	now := time.Now().UTC()
	activation := domain.Activation{ID: 4, Provider: smsprovider.HeroSMS, LeaseOwner: "worker", LeaseVersion: 2}
	base := &verificationResendFlowStore{activation: activation}
	store := &heroSMSWorkflowStore{verificationResendFlowStore: base, events: []domain.HeroSMSWebhookEvent{
		{ID: 1, Status: domain.HeroSMSWebhookEventReceived, NextAttemptAt: now.Add(-time.Second), ReceivedAt: now.Add(-time.Minute)},
		{ID: 2, Code: stringPointer("1111"), Status: domain.HeroSMSWebhookEventReceived, NextAttemptAt: now.Add(-time.Second), ProviderReceivedAt: timePointer(now.Add(-time.Minute)), ReceivedAt: now.Add(-time.Minute)},
		{ID: 3, Code: stringPointer(" 2222 "), Status: domain.HeroSMSWebhookEventReceived, NextAttemptAt: now.Add(-time.Second), ProviderReceivedAt: timePointer(now), ReceivedAt: now},
	}}
	manager := &Manager{store: store, cfg: Config{PollInterval: 2 * time.Second}}

	claimed, err := manager.claimHeroSMSCode(context.Background(), activation, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.code == nil || claimed.code.event.ID != 3 || claimed.code.code != "2222" {
		t.Fatalf("claimed = %#v, want event 3", claimed)
	}
	if len(store.ignored) != 2 || store.events[0].Status != domain.HeroSMSWebhookEventIgnored ||
		store.events[1].Status != domain.HeroSMSWebhookEventIgnored {
		t.Fatalf("ignored = %v, events = %#v", store.ignored, store.events)
	}
}

func TestHeroSMSLoginCodeFailureReturnsEventToInbox(t *testing.T) {
	now := time.Now().UTC()
	activation := domain.Activation{ID: 4, Provider: smsprovider.HeroSMS, LeaseOwner: "worker", LeaseVersion: 2}
	base := &verificationResendFlowStore{activation: activation}
	store := &heroSMSWorkflowStore{verificationResendFlowStore: base, events: []domain.HeroSMSWebhookEvent{
		{ID: 8, Code: stringPointer("1234"), Status: domain.HeroSMSWebhookEventProcessing, Attempts: 1, ReceivedAt: now},
	}}
	manager := &Manager{store: store, cfg: Config{PollInterval: 2 * time.Second}}
	processingErr := errors.New("temporary GoPay failure")

	err := manager.failHeroSMSCode(context.Background(), activation,
		heroSMSClaimedCode{event: store.events[0], code: "1234"}, processingErr)
	if !errors.Is(err, processingErr) {
		t.Fatalf("error = %v, want processing error", err)
	}
	if store.events[0].Status != domain.HeroSMSWebhookEventReceived || store.failed[8] == "" ||
		!store.events[0].NextAttemptAt.After(now) {
		t.Fatalf("retried event = %#v, failures = %v", store.events[0], store.failed)
	}
}

func TestHeroSMSFailureReasonTruncatesUTF8Safely(t *testing.T) {
	now := time.Now().UTC()
	activation := domain.Activation{ID: 4, Provider: smsprovider.HeroSMS, LeaseOwner: "worker", LeaseVersion: 2}
	base := &verificationResendFlowStore{activation: activation}
	store := &heroSMSWorkflowStore{verificationResendFlowStore: base, events: []domain.HeroSMSWebhookEvent{
		{ID: 18, Code: stringPointer("1234"), Status: domain.HeroSMSWebhookEventProcessing, Attempts: 1, ReceivedAt: now},
	}}
	manager := &Manager{store: store, cfg: Config{PollInterval: 2 * time.Second}}
	processingErr := errors.New(strings.Repeat("错🙂", 400))

	err := manager.failHeroSMSCode(context.Background(), activation,
		heroSMSClaimedCode{event: store.events[0], code: "1234"}, processingErr)
	if !errors.Is(err, processingErr) {
		t.Fatalf("error = %v, want processing error", err)
	}
	reason := store.failed[18]
	if len(reason) > 1024 || !utf8.ValidString(reason) {
		t.Fatalf("failure reason bytes = %d, valid UTF-8 = %v", len(reason), utf8.ValidString(reason))
	}
}

func TestHeroSMSLoginDoesNotReplayCodeAfterUncertainExternalConsumption(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	sentAt := time.Now().UTC().Add(-time.Second)
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-login-uncertain"),
		VerificationID: "login-verification-1", OTPToken: "login-token-1",
		LoginStage: gopay.LoginStageAwaiting1FAOTP, LoginCodeSentAt: sentAt,
	}, domain.ActivationStatusAwaitingLoginCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	receivedAt := time.Now().UTC()
	metadata, err := json.Marshal(heroSMSVerificationPayload{
		Source: smsprovider.HeroSMS, WebhookEventID: 31, Payload: json.RawMessage(`{"code":"1234"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	base.verifications = []storage.AppendVerificationParams{{
		ActivationID: base.activation.ID, CycleNo: 0, Phase: domain.VerificationPhaseLogin,
		Code: "1234", ProviderPayload: metadata, ProviderReceivedAt: &receivedAt,
	}}
	store.events = []domain.HeroSMSWebhookEvent{{
		ID: 31, Code: stringPointer("1234"), Status: domain.HeroSMSWebhookEventProcessing,
		Attempts: 1, ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt,
		ClaimedLeaseOwner: "old-worker", ClaimedLeaseVersion: 1,
	}}

	if err = manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("external calls = %v, want no OTP replay", got)
	}
	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageReady1FA || state.LoginCodeResends != 1 {
		t.Fatalf("recovered login state = stage %q resends %d", state.LoginStage, state.LoginCodeResends)
	}
	if store.events[0].Status != domain.HeroSMSWebhookEventIgnored || store.ignored[31] == "" {
		t.Fatalf("event = %#v, ignored = %v", store.events[0], store.ignored)
	}
}

func TestHeroSMSPINDoesNotReplayCodeAfterUncertainExternalConsumption(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	sentAt := time.Now().UTC().Add(-time.Second)
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-pin-uncertain"),
		AccessToken: "access-token", PINVerificationID: "pin-verification-1", PINOTPToken: "pin-token-1",
		PINStage: gopay.PINStageAwaiting, PINCodeSentAt: sentAt,
	}, domain.ActivationStatusAwaitingPINCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	receivedAt := time.Now().UTC()
	metadata, err := json.Marshal(heroSMSVerificationPayload{
		Source: smsprovider.HeroSMS, WebhookEventID: 32, Payload: json.RawMessage(`{"code":"5678"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	base.verifications = []storage.AppendVerificationParams{{
		ActivationID: base.activation.ID, CycleNo: 0, Phase: domain.VerificationPhasePIN,
		Code: "5678", ProviderPayload: metadata, ProviderReceivedAt: &receivedAt,
	}}
	store.events = []domain.HeroSMSWebhookEvent{{
		ID: 32, Code: stringPointer("5678"), Status: domain.HeroSMSWebhookEventProcessing,
		Attempts: 1, ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt,
		ClaimedLeaseOwner: "old-worker", ClaimedLeaseVersion: 1,
	}}

	if err = manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("external calls = %v, want no OTP replay", got)
	}
	state := store.session(t, box)
	if state.PINStage != gopay.PINStageReadyCycle || state.PINCodeResends != 1 {
		t.Fatalf("recovered PIN state = stage %q resends %d", state.PINStage, state.PINCodeResends)
	}
	if store.events[0].Status != domain.HeroSMSWebhookEventIgnored || store.ignored[32] == "" {
		t.Fatalf("event = %#v, ignored = %v", store.events[0], store.ignored)
	}
}

func TestHeroSMSUncertainLoginRecoveryCheckpointDoesNotSpendResendTwice(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-login-recover-checkpoint"),
		VerificationID: "login-verification-1", OTPToken: "login-token-1",
		LoginStage: gopay.LoginStageReady1FA, LoginCodeSentAt: time.Now().UTC().Add(-time.Second), LoginCodeResends: 1,
	}, domain.ActivationStatusAwaitingLoginCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	receivedAt := time.Now().UTC()
	metadata, err := json.Marshal(heroSMSVerificationPayload{Source: smsprovider.HeroSMS, WebhookEventID: 41})
	if err != nil {
		t.Fatal(err)
	}
	base.verifications = []storage.AppendVerificationParams{{
		ActivationID: base.activation.ID, CycleNo: 0, Phase: domain.VerificationPhaseLogin,
		Code: "1234", ProviderPayload: metadata, ProviderReceivedAt: &receivedAt,
	}}
	store.events = []domain.HeroSMSWebhookEvent{{
		ID: 41, Code: stringPointer("1234"), Status: domain.HeroSMSWebhookEventProcessing,
		Attempts: 2, ClaimedLeaseOwner: "old-worker", ClaimedLeaseVersion: 2,
		ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt,
	}}

	if err = manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("external calls = %v, want no replay or resend yet", got)
	}
	state := store.session(t, box)
	if state.LoginStage != gopay.LoginStageReady1FA || state.LoginCodeResends != 1 {
		t.Fatalf("checkpoint state = stage %q resends %d", state.LoginStage, state.LoginCodeResends)
	}
	if store.events[0].Status != domain.HeroSMSWebhookEventIgnored {
		t.Fatalf("event = %#v, want ignored", store.events[0])
	}
}

func TestHeroSMSUncertainPINRecoveryCheckpointDoesNotSpendResendTwice(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	manager, base, box := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-pin-recover-checkpoint"),
		AccessToken: "access-token", PINStage: gopay.PINStageResetReadyCycle,
		PINCodeSentAt: time.Now().UTC().Add(-time.Second), PINCodeResends: 1,
	}, domain.ActivationStatusAwaitingPINCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	receivedAt := time.Now().UTC()
	metadata, err := json.Marshal(heroSMSVerificationPayload{Source: smsprovider.HeroSMS, WebhookEventID: 42})
	if err != nil {
		t.Fatal(err)
	}
	base.verifications = []storage.AppendVerificationParams{{
		ActivationID: base.activation.ID, CycleNo: 0, Phase: domain.VerificationPhasePIN,
		Code: "5678", ProviderPayload: metadata, ProviderReceivedAt: &receivedAt,
	}}
	store.events = []domain.HeroSMSWebhookEvent{{
		ID: 42, Code: stringPointer("5678"), Status: domain.HeroSMSWebhookEventProcessing,
		Attempts: 2, ClaimedLeaseOwner: "old-worker", ClaimedLeaseVersion: 2,
		ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt,
	}}

	if err = manager.pollPINCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("external calls = %v, want no replay or resend yet", got)
	}
	state := store.session(t, box)
	if state.PINStage != gopay.PINStageResetReadyCycle || state.PINCodeResends != 1 {
		t.Fatalf("checkpoint state = stage %q resends %d", state.PINStage, state.PINCodeResends)
	}
	if store.events[0].Status != domain.HeroSMSWebhookEventIgnored {
		t.Fatalf("event = %#v, want ignored", store.events[0])
	}
}

func TestHeroSMSUncertainLoginAtResendLimitTimesOutWithoutFourthRequest(t *testing.T) {
	events := &verificationResendEventLog{}
	provider := newVerificationResendProvider(t, events)
	defer provider.Close()
	goPay := newVerificationResendGoPay(t, events)
	defer goPay.Close()
	manager, base, _ := newVerificationResendFlowManager(t, provider.URL, goPay.URL, gopay.Session{
		Phone: "+6281234567890", CountryCode: "+62", Device: gopay.GenerateDeviceIdentity("hero-login-recover-limit"),
		VerificationID: "login-verification-1", OTPToken: "login-token-1",
		LoginStage: gopay.LoginStageAwaiting1FAOTP, LoginCodeResends: verificationCodeResends,
	}, domain.ActivationStatusAwaitingLoginCode)
	store := configureHeroSMSWorkflow(t, manager, base, provider.URL)
	receivedAt := time.Now().UTC()
	metadata, err := json.Marshal(heroSMSVerificationPayload{Source: smsprovider.HeroSMS, WebhookEventID: 43})
	if err != nil {
		t.Fatal(err)
	}
	base.verifications = []storage.AppendVerificationParams{{
		ActivationID: base.activation.ID, CycleNo: 0, Phase: domain.VerificationPhaseLogin,
		Code: "1234", ProviderPayload: metadata,
	}}
	store.events = []domain.HeroSMSWebhookEvent{{
		ID: 43, Code: stringPointer("1234"), Status: domain.HeroSMSWebhookEventProcessing,
		ClaimedLeaseOwner: "old-worker", ClaimedLeaseVersion: 2, ProviderReceivedAt: &receivedAt, ReceivedAt: receivedAt,
	}}

	if err = manager.pollLoginCode(context.Background(), store.activationSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); len(got) != 1 || got[0] != "provider:setStatus:8" {
		t.Fatalf("external calls = %v, want cancellation only", got)
	}
	if base.activation.Status != domain.ActivationStatusLoginCodeTimeout {
		t.Fatalf("activation status = %q, want login_code_timeout", base.activation.Status)
	}
	if store.events[0].Status != domain.HeroSMSWebhookEventIgnored {
		t.Fatalf("event = %#v, want ignored", store.events[0])
	}
}

func TestHeroSMSReconcilesConsumedEventAfterAcknowledgementFailure(t *testing.T) {
	now := time.Now().UTC()
	activation := domain.Activation{ID: 4, Provider: smsprovider.HeroSMS, LeaseOwner: "worker", LeaseVersion: 2}
	payload, err := json.Marshal(heroSMSVerificationPayload{
		Source: smsprovider.HeroSMS, WebhookEventID: 9, Payload: json.RawMessage(`{"code":"1234"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := &verificationResendFlowStore{
		activation: activation,
		verifications: []storage.AppendVerificationParams{{
			ActivationID: activation.ID, CycleNo: 0, Phase: domain.VerificationPhaseLogin,
			Code: "1234", ProviderPayload: payload, ProviderReceivedAt: &now,
		}},
	}
	store := &heroSMSWorkflowStore{verificationResendFlowStore: base, events: []domain.HeroSMSWebhookEvent{
		{ID: 9, Code: stringPointer("1234"), Status: domain.HeroSMSWebhookEventProcessing, Attempts: 1,
			ClaimedLeaseOwner: "old-worker", ClaimedLeaseVersion: 1, ReceivedAt: now},
	}}
	manager := &Manager{store: store, cfg: Config{PollInterval: 2 * time.Second}}

	if err = manager.reconcileHeroSMSConsumedEvents(context.Background(), activation, domain.VerificationPhaseLogin); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.completed, []int64{9}) || store.events[0].Status != domain.HeroSMSWebhookEventProcessed {
		t.Fatalf("completed = %v, event = %#v", store.completed, store.events[0])
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func stringPointer(value string) *string { return &value }
