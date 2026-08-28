package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
	"github.com/mangobubu/gopay-autosms/internal/smsprovider"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

const (
	heroSMSInboxDrainLimit = 32
	heroSMSClockSkew       = 5 * time.Second
)

type heroSMSWebhookContextKey struct{}

type heroSMSClaimedCode struct {
	event domain.HeroSMSWebhookEvent
	code  string
}

type heroSMSClaimResult struct {
	code    *heroSMSClaimedCode
	yielded bool
}

type heroSMSVerificationPayload struct {
	Source         string          `json:"source"`
	WebhookEventID int64           `json:"webhook_event_id"`
	Payload        json.RawMessage `json:"payload"`
}

func (m *Manager) findHeroSMSVerificationEvent(
	ctx context.Context,
	activationID, eventID int64,
) (domain.VerificationCode, bool, error) {
	verifications, err := m.store.ListVerificationCodes(ctx, activationID)
	if err != nil {
		return domain.VerificationCode{}, false, err
	}
	for _, verification := range verifications {
		var payload heroSMSVerificationPayload
		if json.Unmarshal(verification.ProviderPayload, &payload) == nil &&
			payload.Source == smsprovider.HeroSMS && payload.WebhookEventID == eventID {
			return verification, true, nil
		}
	}
	return domain.VerificationCode{}, false, nil
}

func (m *Manager) findHeroSMSVerificationCycle(
	ctx context.Context,
	activationID int64,
	cycle int,
) (domain.VerificationCode, bool, error) {
	verifications, err := m.store.ListVerificationCodes(ctx, activationID)
	if err != nil {
		return domain.VerificationCode{}, false, err
	}
	for _, verification := range verifications {
		if verification.CycleNo == cycle {
			return verification, true, nil
		}
	}
	return domain.VerificationCode{}, false, nil
}

func isHeroSMSActivation(activation domain.Activation) bool {
	return activation.Provider == smsprovider.HeroSMS
}

func heroSMSInbox(store storage.Store) (storage.HeroSMSWebhookStore, error) {
	inbox, ok := store.(storage.HeroSMSWebhookStore)
	if !ok {
		return nil, fmt.Errorf("HeroSMS webhook inbox is not configured")
	}
	return inbox, nil
}

// claimHeroSMSCode drains audit-only and clearly stale callbacks before
// returning the next usable code. HeroSMS documents code as nullable, so text
// is deliberately not guessed or parsed as an OTP.
func (m *Manager) claimHeroSMSCode(
	ctx context.Context,
	activation domain.Activation,
	currentSentAt time.Time,
	dispatchCertain bool,
) (heroSMSClaimResult, error) {
	inbox, err := heroSMSInbox(m.store)
	if err != nil {
		return heroSMSClaimResult{}, err
	}
	for index := 0; index < heroSMSInboxDrainLimit; index++ {
		event, claimErr := inbox.ClaimNextHeroSMSWebhookEventOwned(
			ctx, activation.ID, activation.LeaseOwner, activation.LeaseVersion, time.Now().UTC(),
		)
		if errors.Is(claimErr, storage.ErrNotFound) {
			return heroSMSClaimResult{}, nil
		}
		if claimErr != nil {
			return heroSMSClaimResult{}, claimErr
		}
		var code string
		if event.Code != nil {
			code = strings.TrimSpace(*event.Code)
		}
		switch {
		case code == "":
			if err = inbox.IgnoreHeroSMSWebhookEventOwned(ctx, event.ID, activation.ID,
				activation.LeaseOwner, activation.LeaseVersion, "HeroSMS 回调未包含验证码"); err != nil {
				return heroSMSClaimResult{}, err
			}
		case !dispatchCertain:
			if err = inbox.IgnoreHeroSMSWebhookEventOwned(ctx, event.ID, activation.ID,
				activation.LeaseOwner, activation.LeaseVersion, "验证码到达时本地 OTP 派发结果未可靠保存"); err != nil {
				return heroSMSClaimResult{}, err
			}
		case heroSMSWebhookClearlyPredates(event, currentSentAt):
			if err = inbox.IgnoreHeroSMSWebhookEventOwned(ctx, event.ID, activation.ID,
				activation.LeaseOwner, activation.LeaseVersion, "验证码早于当前短信周期"); err != nil {
				return heroSMSClaimResult{}, err
			}
		default:
			return heroSMSClaimResult{code: &heroSMSClaimedCode{event: event, code: code}}, nil
		}
	}
	return heroSMSClaimResult{yielded: true}, nil
}

func heroSMSWebhookClearlyPredates(event domain.HeroSMSWebhookEvent, sentAt time.Time) bool {
	return !sentAt.IsZero() && event.ProviderReceivedAt != nil &&
		event.ProviderReceivedAt.Before(sentAt.Add(-heroSMSClockSkew))
}

func withHeroSMSWebhookEvent(ctx context.Context, event domain.HeroSMSWebhookEvent) context.Context {
	return context.WithValue(ctx, heroSMSWebhookContextKey{}, event)
}

func verificationProviderMetadata(ctx context.Context, code string) (json.RawMessage, *time.Time) {
	if event, ok := ctx.Value(heroSMSWebhookContextKey{}).(domain.HeroSMSWebhookEvent); ok {
		payload, err := json.Marshal(heroSMSVerificationPayload{
			Source:         smsprovider.HeroSMS,
			WebhookEventID: event.ID,
			Payload:        append(json.RawMessage(nil), event.RawPayload...),
		})
		if err == nil {
			receivedAt := event.ReceivedAt.UTC()
			if event.ProviderReceivedAt != nil {
				receivedAt = event.ProviderReceivedAt.UTC()
			}
			return payload, &receivedAt
		}
	}
	payload, _ := json.Marshal(map[string]string{"code": code})
	now := time.Now().UTC()
	return payload, &now
}

func (m *Manager) completeHeroSMSCode(
	ctx context.Context,
	activation domain.Activation,
	claimed heroSMSClaimedCode,
) error {
	inbox, err := heroSMSInbox(m.store)
	if err != nil {
		return err
	}
	return inbox.CompleteHeroSMSWebhookEventOwned(ctx, claimed.event.ID, activation.ID,
		activation.LeaseOwner, activation.LeaseVersion)
}

// reconcileHeroSMSConsumedEvents closes the narrow crash window after GoPay
// and the session transition committed but before the inbox acknowledgement
// did. A webhook-specific verification record proves which event supplied the
// already-consumed code; unrelated callbacks are retained as ignored audit
// records instead of being replayed in the next protocol phase.
func (m *Manager) reconcileHeroSMSConsumedEvents(
	ctx context.Context,
	activation domain.Activation,
	phase domain.VerificationPhase,
) error {
	inbox, err := heroSMSInbox(m.store)
	if err != nil {
		return err
	}
	for index := 0; index < heroSMSInboxDrainLimit; index++ {
		claimed, claimErr := m.claimHeroSMSCode(ctx, activation, time.Time{}, true)
		if claimErr != nil {
			return claimErr
		}
		if claimed.code == nil {
			return nil
		}
		verification, found, findErr := m.findHeroSMSVerificationEvent(
			ctx, activation.ID, claimed.code.event.ID,
		)
		if findErr != nil {
			return m.failHeroSMSCode(ctx, activation, *claimed.code, findErr)
		}
		if found && verification.Phase == phase {
			if err = m.completeHeroSMSCode(ctx, activation, *claimed.code); err != nil {
				return err
			}
			continue
		}
		reason := "HeroSMS 回调与当前业务阶段不匹配"
		if found {
			reason = fmt.Sprintf("验证码已由 %s 阶段处理", verification.Phase)
		}
		if err = inbox.IgnoreHeroSMSWebhookEventOwned(ctx, claimed.code.event.ID, activation.ID,
			activation.LeaseOwner, activation.LeaseVersion, reason); err != nil {
			return err
		}
	}
	return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
}

func (m *Manager) failHeroSMSCode(
	ctx context.Context,
	activation domain.Activation,
	claimed heroSMSClaimedCode,
	processingErr error,
) error {
	inbox, err := heroSMSInbox(m.store)
	if err != nil {
		return errors.Join(processingErr, err)
	}
	if errors.Is(processingErr, gopay.ErrLoginFailed) {
		ignoreErr := inbox.IgnoreHeroSMSWebhookEventOwned(ctx, claimed.event.ID, activation.ID,
			activation.LeaseOwner, activation.LeaseVersion, "业务流程已终止: "+processingErr.Error())
		return errors.Join(processingErr, ignoreErr)
	}
	delay := m.cfg.PollInterval
	if delay < time.Second {
		delay = time.Second
	}
	// Back off repeated transient business failures without sleeping past the
	// short OTP validity window.
	for attempt := 1; attempt < claimed.event.Attempts && attempt < 4; attempt++ {
		delay *= 2
	}
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	reason := truncateUTF8Bytes(processingErr.Error(), 1024)
	failErr := inbox.FailHeroSMSWebhookEventOwned(ctx, claimed.event.ID, activation.ID,
		activation.LeaseOwner, activation.LeaseVersion, time.Now().UTC().Add(delay), reason)
	return errors.Join(processingErr, failErr)
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func (m *Manager) scheduleHeroSMSWait(ctx context.Context, activation domain.Activation, next time.Time) error {
	now := time.Now().UTC()
	if next.IsZero() {
		next = now.Add(m.cfg.ActivationTTL)
	}
	return m.store.TouchActivationPoll(ctx, activation.ID, activation.LeaseOwner, now, next)
}

func (m *Manager) heroSMSProviderDeadline(activation domain.Activation) time.Time {
	if activation.ProviderExpiresAt != nil {
		return activation.ProviderExpiresAt.UTC()
	}
	return time.Now().UTC().Add(m.cfg.ActivationTTL)
}

func earlierHeroSMSDeadline(first, second time.Time) time.Time {
	if first.IsZero() || (!second.IsZero() && second.Before(first)) {
		return second
	}
	return first
}

func (m *Manager) scheduleHeroSMSOTPWait(
	ctx context.Context,
	activation domain.Activation,
	sentAt time.Time,
	wait time.Duration,
) error {
	deadline := sentAt.Add(wait)
	deadline = earlierHeroSMSDeadline(deadline, m.heroSMSProviderDeadline(activation))
	return m.scheduleHeroSMSWait(ctx, activation, deadline)
}

func (m *Manager) consumeHeroSMSLoginCode(
	ctx context.Context,
	activation domain.Activation,
	client *gopay.Client,
	targetPIN string,
	claimed heroSMSClaimedCode,
) error {
	existing, alreadySubmitted, err := m.findHeroSMSVerificationEvent(ctx, activation.ID, claimed.event.ID)
	if err != nil {
		return m.failHeroSMSCode(ctx, activation, claimed, err)
	}
	if alreadySubmitted {
		if existing.Phase != domain.VerificationPhaseLogin {
			return m.ignoreHeroSMSCode(ctx, activation, claimed, "验证码已属于其他业务阶段")
		}
		return m.recoverUncertainHeroSMSLoginCode(ctx, activation, client, targetPIN, claimed)
	}
	if rejected, rejectErr := m.rejectHeroSMSCodeForOccupiedCycle(ctx, activation, claimed); rejected {
		return rejectErr
	}
	eventCtx := withHeroSMSWebhookEvent(ctx, claimed.event)
	if err := m.consumeLoginVerificationCode(eventCtx, activation, client, targetPIN, claimed.code); err != nil {
		return m.failHeroSMSCode(ctx, activation, claimed, err)
	}
	if err := m.completeHeroSMSCode(ctx, activation, claimed); err != nil {
		return err
	}
	return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
}

func (m *Manager) ignoreHeroSMSCode(
	ctx context.Context,
	activation domain.Activation,
	claimed heroSMSClaimedCode,
	reason string,
) error {
	inbox, err := heroSMSInbox(m.store)
	if err != nil {
		return err
	}
	return inbox.IgnoreHeroSMSWebhookEventOwned(ctx, claimed.event.ID, activation.ID,
		activation.LeaseOwner, activation.LeaseVersion, reason)
}

// rejectHeroSMSCodeForOccupiedCycle prevents a second, differently
// fingerprinted callback from reusing the verification row which already
// represents this provider cycle. PostgreSQL stores one verification per
// cycle; treating its conflict result as fresh would replay the first OTP.
func (m *Manager) rejectHeroSMSCodeForOccupiedCycle(
	ctx context.Context,
	activation domain.Activation,
	claimed heroSMSClaimedCode,
) (bool, error) {
	existing, found, err := m.findHeroSMSVerificationCycle(ctx, activation.ID, activation.SMSCycle)
	if err != nil {
		return true, m.failHeroSMSCode(ctx, activation, claimed, err)
	}
	if !found {
		return false, nil
	}
	reason := fmt.Sprintf("短信周期 %d 已由 %s 验证码处理", existing.CycleNo, existing.Phase)
	return true, m.ignoreHeroSMSCode(ctx, activation, claimed, reason)
}

func (m *Manager) recoverUncertainHeroSMSLoginCode(
	ctx context.Context,
	activation domain.Activation,
	client *gopay.Client,
	targetPIN string,
	claimed heroSMSClaimedCode,
) error {
	state := client.State()
	recoveryAlreadySaved := false
	switch state.LoginStage {
	case gopay.LoginStageAwaiting1FAOTP:
		if state.LoginCodeResends >= verificationCodeResends {
			if err := m.ignoreHeroSMSCode(ctx, activation, claimed, "验证码外部处理结果未知，重试次数已耗尽"); err != nil {
				return err
			}
			return m.cancelAndClassifyFrom(ctx, activation,
				[]domain.ActivationStatus{domain.ActivationStatusAwaitingLoginCode},
				domain.ActivationStatusLoginCodeTimeout,
				"登录验证码外部处理结果未知，重发 3 次后仍未完成")
		}
		state.LoginStage = gopay.LoginStageReady1FA
	case gopay.LoginStageAwaiting2FAOTP:
		if state.LoginCodeResends >= verificationCodeResends {
			if err := m.ignoreHeroSMSCode(ctx, activation, claimed, "验证码外部处理结果未知，重试次数已耗尽"); err != nil {
				return err
			}
			return m.cancelAndClassifyFrom(ctx, activation,
				[]domain.ActivationStatus{domain.ActivationStatusAwaitingLoginCode},
				domain.ActivationStatusLoginCodeTimeout,
				"登录验证码外部处理结果未知，重发 3 次后仍未完成")
		}
		state.LoginStage = gopay.LoginStageReady2FA
	case gopay.LoginStageReady1FA, gopay.LoginStageReady2FA:
		recoveryAlreadySaved = true
	default:
		return m.failHeroSMSCode(ctx, activation, claimed,
			fmt.Errorf("cannot recover uncertain login code from stage %q", state.LoginStage))
	}
	if !recoveryAlreadySaved {
		state.LoginCodeResends++
	}
	state.LoginCodeSentAt = time.Time{}
	state.LoginCodeDispatchUncertain = false
	state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	client.Restore(state)
	if _, err := m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil); err != nil {
		return m.failHeroSMSCode(ctx, activation, claimed, err)
	}
	if err := m.ignoreHeroSMSCode(ctx, activation, claimed, "验证码已提交但外部处理结果未知，改用新的短信周期"); err != nil {
		return err
	}
	return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
}

func (m *Manager) waitHeroSMSLoginCode(ctx context.Context, activation domain.Activation) error {
	batch, _, client, err := m.restoreGoPayClient(ctx, activation)
	if err != nil {
		return err
	}
	targetPIN, err := m.targetPIN(batch)
	if err != nil {
		return err
	}
	state := client.State()
	if state.LoginStage == gopay.LoginStageAuthenticated ||
		(state.LoginStage == gopay.LoginStageReady2FA && state.LoginCodeSentAt.IsZero()) {
		if err = m.reconcileHeroSMSConsumedEvents(ctx, activation, domain.VerificationPhaseLogin); err != nil {
			return err
		}
	}
	switch state.LoginStage {
	case gopay.LoginStageAuthenticated:
		_, err = m.store.TransitionActivationOwned(ctx, activation.ID,
			[]domain.ActivationStatus{domain.ActivationStatusAwaitingLoginCode},
			domain.ActivationStatusCheckingBalance, "", activation.LeaseOwner, activation.LeaseVersion)
		return err
	case gopay.LoginStageReady1FA, gopay.LoginStageReady2FA:
		if !state.LoginCodeSentAt.IsZero() {
			claimed, claimErr := m.claimHeroSMSCode(ctx, activation, state.LoginCodeSentAt, !state.LoginCodeDispatchUncertain)
			if claimErr != nil {
				return claimErr
			}
			if claimed.code != nil {
				return m.consumeHeroSMSLoginCode(ctx, activation, client, targetPIN, *claimed.code)
			}
			if claimed.yielded {
				return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
			}
			if !verificationCodeWaitTimedOut(state.LoginCodeSentAt, time.Now().UTC(), loginVerificationCodeWait) {
				return m.scheduleHeroSMSOTPWait(ctx, activation, state.LoginCodeSentAt, loginVerificationCodeWait)
			}
		}
		cycle := activation.SMSCycle
		if state.SMSCycle >= activation.SMSCycle {
			cycle, state, err = m.advanceVerificationSMSCycle(ctx, activation, state, func(checkpoint gopay.Session) error {
				_, saveErr := m.saveSession(ctx, activation.PhoneNumber, checkpoint, targetPIN, domain.AccountStatusPending, nil)
				return saveErr
			})
			if err != nil {
				return err
			}
		}
		if state.LoginStage == gopay.LoginStageReady1FA {
			state.LoginStage = gopay.LoginStageCycleReady1FA
		} else {
			state.LoginStage = gopay.LoginStageCycleReady2FA
		}
		state.SMSCycle = cycle
		state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil); err != nil {
			return err
		}
		return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
	case gopay.LoginStageCycleReady1FA, gopay.LoginStageCycleReady2FA:
		pending := state
		if state.LoginStage == gopay.LoginStageCycleReady1FA {
			pending.LoginStage = gopay.LoginStageAwaiting1FAOTP
		} else {
			pending.LoginStage = gopay.LoginStageAwaiting2FAOTP
		}
		pending.LoginCodeDispatchUncertain = true
		pending.LoginCodeSentAt = time.Time{}
		if _, err = m.saveSession(ctx, activation.PhoneNumber, pending, targetPIN, domain.AccountStatusPending, nil); err != nil {
			return err
		}
		if state.LoginStage == gopay.LoginStageCycleReady1FA {
			_, err = client.StartLogin(ctx)
		} else {
			_, err = client.StartNextLoginOTP(ctx)
		}
		if err != nil {
			return err
		}
		state = client.State()
		state.LoginCodeSentAt = time.Now().UTC()
		state.LoginCodeDispatchUncertain = false
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil); err != nil {
			return err
		}
		return m.scheduleHeroSMSOTPWait(ctx, activation, state.LoginCodeSentAt, loginVerificationCodeWait)
	}

	claimed, err := m.claimHeroSMSCode(ctx, activation, state.LoginCodeSentAt, !state.LoginCodeDispatchUncertain)
	if err != nil {
		return err
	}
	if claimed.code != nil {
		return m.consumeHeroSMSLoginCode(ctx, activation, client, targetPIN, *claimed.code)
	}
	if claimed.yielded {
		return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
	}
	now := time.Now().UTC()
	if state.LoginCodeSentAt.IsZero() {
		state.LoginCodeSentAt = now
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil); err != nil {
			return err
		}
	}
	if !verificationCodeWaitTimedOut(state.LoginCodeSentAt, now, loginVerificationCodeWait) {
		return m.scheduleHeroSMSOTPWait(ctx, activation, state.LoginCodeSentAt, loginVerificationCodeWait)
	}
	if state.LoginCodeResends >= verificationCodeResends {
		return m.cancelAndClassifyFrom(ctx, activation,
			[]domain.ActivationStatus{domain.ActivationStatusAwaitingLoginCode},
			domain.ActivationStatusLoginCodeTimeout,
			"登录验证码重发 3 次后仍未收到")
	}
	switch state.LoginStage {
	case gopay.LoginStageAwaiting1FAOTP:
		state.LoginStage = gopay.LoginStageReady1FA
	case gopay.LoginStageAwaiting2FAOTP:
		state.LoginStage = gopay.LoginStageReady2FA
	default:
		return fmt.Errorf("unexpected login OTP wait stage %q", state.LoginStage)
	}
	state.LoginCodeResends++
	client.Restore(state)
	if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPending, nil); err != nil {
		return err
	}
	return m.scheduleHeroSMSWait(ctx, activation, now)
}

func (m *Manager) consumeHeroSMSPINCode(
	ctx context.Context,
	activation domain.Activation,
	client *gopay.Client,
	targetPIN string,
	claimed heroSMSClaimedCode,
) error {
	existing, alreadySubmitted, err := m.findHeroSMSVerificationEvent(ctx, activation.ID, claimed.event.ID)
	if err != nil {
		return m.failHeroSMSCode(ctx, activation, claimed, err)
	}
	if alreadySubmitted {
		if existing.Phase != domain.VerificationPhasePIN {
			return m.ignoreHeroSMSCode(ctx, activation, claimed, "验证码已属于其他业务阶段")
		}
		return m.recoverUncertainHeroSMSPINCode(ctx, activation, client, targetPIN, claimed)
	}
	if rejected, rejectErr := m.rejectHeroSMSCodeForOccupiedCycle(ctx, activation, claimed); rejected {
		return rejectErr
	}
	eventCtx := withHeroSMSWebhookEvent(ctx, claimed.event)
	if err := m.consumePINVerificationCode(eventCtx, activation, client, targetPIN, claimed.code); err != nil {
		return m.failHeroSMSCode(ctx, activation, claimed, err)
	}
	return m.completeHeroSMSCode(ctx, activation, claimed)
}

func (m *Manager) recoverUncertainHeroSMSPINCode(
	ctx context.Context,
	activation domain.Activation,
	client *gopay.Client,
	targetPIN string,
	claimed heroSMSClaimedCode,
) error {
	state := client.State()
	recoveryAlreadySaved := false
	switch state.PINStage {
	case gopay.PINStageAwaiting:
		if state.PINCodeResends >= verificationCodeResends {
			if err := m.ignoreHeroSMSCode(ctx, activation, claimed, "验证码外部处理结果未知，重试次数已耗尽"); err != nil {
				return err
			}
			return m.cancelAndClassifyFrom(ctx, activation,
				[]domain.ActivationStatus{domain.ActivationStatusAwaitingPINCode},
				domain.ActivationStatusPINCodeTimeout,
				"改 PIN 验证码外部处理结果未知，重发 3 次后仍未完成")
		}
		state.PINStage = gopay.PINStageReadyCycle
	case gopay.PINStageResetAwaiting:
		if state.PINCodeResends >= verificationCodeResends {
			if err := m.ignoreHeroSMSCode(ctx, activation, claimed, "验证码外部处理结果未知，重试次数已耗尽"); err != nil {
				return err
			}
			return m.cancelAndClassifyFrom(ctx, activation,
				[]domain.ActivationStatus{domain.ActivationStatusAwaitingPINCode},
				domain.ActivationStatusPINCodeTimeout,
				"改 PIN 验证码外部处理结果未知，重发 3 次后仍未完成")
		}
		state.PINStage = gopay.PINStageResetReadyCycle
	case gopay.PINStageReadyCycle, gopay.PINStageResetReadyCycle:
		recoveryAlreadySaved = true
	default:
		return m.failHeroSMSCode(ctx, activation, claimed,
			fmt.Errorf("cannot recover uncertain PIN code from stage %q", state.PINStage))
	}
	if !recoveryAlreadySaved {
		state.PINCodeResends++
	}
	state.PINCodeSentAt = time.Time{}
	state.PINCodeDispatchUncertain = false
	state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	client.Restore(state)
	if _, err := m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); err != nil {
		return m.failHeroSMSCode(ctx, activation, claimed, err)
	}
	if err := m.ignoreHeroSMSCode(ctx, activation, claimed, "验证码已提交但外部处理结果未知，改用新的短信周期"); err != nil {
		return err
	}
	return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
}

func (m *Manager) waitHeroSMSPINCode(ctx context.Context, activation domain.Activation) error {
	batch, _, client, err := m.restoreGoPayClient(ctx, activation)
	if err != nil {
		return err
	}
	targetPIN, err := m.targetPIN(batch)
	if err != nil {
		return err
	}
	state := client.State()
	switch state.PINStage {
	case gopay.PINStageReadyCycle, gopay.PINStageResetReadyCycle:
		if !state.PINCodeSentAt.IsZero() {
			claimed, claimErr := m.claimHeroSMSCode(ctx, activation, state.PINCodeSentAt, !state.PINCodeDispatchUncertain)
			if claimErr != nil {
				return claimErr
			}
			if claimed.code != nil {
				return m.consumeHeroSMSPINCode(ctx, activation, client, targetPIN, *claimed.code)
			}
			if claimed.yielded {
				return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
			}
			if !verificationCodeWaitTimedOut(state.PINCodeSentAt, time.Now().UTC(), pinVerificationCodeWait) {
				return m.scheduleHeroSMSOTPWait(ctx, activation, state.PINCodeSentAt, pinVerificationCodeWait)
			}
		}
		cycle := activation.SMSCycle
		if state.SMSCycle >= activation.SMSCycle {
			cycle, state, err = m.advanceVerificationSMSCycle(ctx, activation, state, func(checkpoint gopay.Session) error {
				_, saveErr := m.saveSession(ctx, activation.PhoneNumber, checkpoint, targetPIN, domain.AccountStatusPINPending, activation.BalanceRP)
				return saveErr
			})
			if err != nil {
				return err
			}
		}
		if state.PINStage == gopay.PINStageReadyCycle {
			state.PINStage = gopay.PINStageCycleReady
		} else {
			state.PINStage = gopay.PINStageResetCycleReady
		}
		state.SMSCycle = cycle
		state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); err != nil {
			return err
		}
		return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
	case gopay.PINStageCycleReady:
		if err = m.savePINDispatchCheckpoint(ctx, activation, state, targetPIN, gopay.PINStageAwaiting); err != nil {
			return err
		}
		if _, err = client.StartPINSetup(ctx, targetPIN); err != nil {
			if isHTTPStatus(err, 401) {
				if refreshErr := client.Refresh(ctx); refreshErr != nil {
					err = refreshErr
				} else if checkpointErr := m.savePINDispatchCheckpoint(ctx, activation, client.State(), targetPIN, gopay.PINStageAwaiting); checkpointErr != nil {
					err = checkpointErr
				} else {
					_, err = client.StartPINSetup(ctx, targetPIN)
				}
			}
			if errors.Is(err, gopay.ErrPINAlreadySet) {
				return m.preparePINReset(ctx, activation, client, targetPIN)
			}
			if err != nil {
				return err
			}
		}
		state = client.State()
		state.PINCodeSentAt = time.Now().UTC()
		state.PINCodeDispatchUncertain = false
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); err != nil {
			return err
		}
		return m.scheduleHeroSMSOTPWait(ctx, activation, state.PINCodeSentAt, pinVerificationCodeWait)
	case gopay.PINStageResetCycleReady:
		if err = m.savePINDispatchCheckpoint(ctx, activation, state, targetPIN, gopay.PINStageResetAwaiting); err != nil {
			return err
		}
		if _, err = client.StartPINReset(ctx, targetPIN); err != nil {
			if isHTTPStatus(err, 401) {
				if refreshErr := client.Refresh(ctx); refreshErr != nil {
					err = refreshErr
				} else if checkpointErr := m.savePINDispatchCheckpoint(ctx, activation, client.State(), targetPIN, gopay.PINStageResetAwaiting); checkpointErr != nil {
					err = checkpointErr
				} else {
					_, err = client.StartPINReset(ctx, targetPIN)
				}
			}
			if err != nil {
				return err
			}
		}
		state = client.State()
		state.PINCodeSentAt = time.Now().UTC()
		state.PINCodeDispatchUncertain = false
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); err != nil {
			return err
		}
		return m.scheduleHeroSMSOTPWait(ctx, activation, state.PINCodeSentAt, pinVerificationCodeWait)
	case gopay.PINStageSetupVerified, gopay.PINStageResetVerified, gopay.PINStageComplete:
		return m.publishPINSettingState(ctx, activation)
	}

	claimed, err := m.claimHeroSMSCode(ctx, activation, state.PINCodeSentAt, !state.PINCodeDispatchUncertain)
	if err != nil {
		return err
	}
	if claimed.code != nil {
		return m.consumeHeroSMSPINCode(ctx, activation, client, targetPIN, *claimed.code)
	}
	if claimed.yielded {
		return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
	}
	now := time.Now().UTC()
	if state.PINCodeSentAt.IsZero() {
		state.PINCodeSentAt = now
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); err != nil {
			return err
		}
	}
	if !verificationCodeWaitTimedOut(state.PINCodeSentAt, now, pinVerificationCodeWait) {
		return m.scheduleHeroSMSOTPWait(ctx, activation, state.PINCodeSentAt, pinVerificationCodeWait)
	}
	if state.PINCodeResends >= verificationCodeResends {
		return m.cancelAndClassifyFrom(ctx, activation,
			[]domain.ActivationStatus{domain.ActivationStatusAwaitingPINCode},
			domain.ActivationStatusPINCodeTimeout,
			"改 PIN 验证码重发 3 次后仍未收到")
	}
	switch state.PINStage {
	case gopay.PINStageAwaiting:
		state.PINStage = gopay.PINStageReadyCycle
	case gopay.PINStageResetAwaiting:
		state.PINStage = gopay.PINStageResetReadyCycle
	default:
		return fmt.Errorf("unexpected PIN OTP wait stage %q", state.PINStage)
	}
	state.PINCodeResends++
	client.Restore(state)
	if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusPINPending, activation.BalanceRP); err != nil {
		return err
	}
	return m.scheduleHeroSMSWait(ctx, activation, now)
}

func (m *Manager) waitHeroSMSFollowupCode(ctx context.Context, activation domain.Activation) error {
	claimed, err := m.claimHeroSMSCode(ctx, activation, time.Time{}, true)
	if err != nil {
		return err
	}
	if claimed.code == nil {
		if claimed.yielded {
			return m.scheduleHeroSMSWait(ctx, activation, time.Now().UTC())
		}
		// A verification in the current cycle means its webhook event was
		// durably appended but has not yet finished the non-idempotent provider
		// advance. Leave the cycle in place until that event becomes claimable;
		// advancing here would let another callback masquerade as the next SMS.
		if verification, occupied, cycleErr := m.findHeroSMSVerificationCycle(
			ctx, activation.ID, activation.SMSCycle,
		); cycleErr != nil {
			return cycleErr
		} else if occupied && verification.Phase == domain.VerificationPhaseSubsequent {
			return m.scheduleHeroSMSWait(ctx, activation, m.heroSMSProviderDeadline(activation))
		}
		activation, err = m.ensureHeroSMSFollowupCycle(ctx, activation)
		if err != nil {
			return err
		}
		return m.scheduleHeroSMSWait(ctx, activation, m.heroSMSProviderDeadline(activation))
	}

	batch, _, client, err := m.restoreGoPayClient(ctx, activation)
	if err != nil {
		return m.failHeroSMSCode(ctx, activation, *claimed.code, err)
	}
	targetPIN, err := m.targetPIN(batch)
	if err != nil {
		return m.failHeroSMSCode(ctx, activation, *claimed.code, err)
	}
	state := client.State()
	existing, alreadyAppended, err := m.findHeroSMSVerificationEvent(ctx, activation.ID, claimed.code.event.ID)
	if err != nil {
		return m.failHeroSMSCode(ctx, activation, *claimed.code, err)
	}
	if alreadyAppended && activation.SMSCycle > existing.CycleNo {
		// The previous attempt advanced the provider and database cycle but lost
		// its inbox acknowledgement. Finish the local checkpoint without issuing
		// setStatus=3 a second time.
		state.SMSCycle = activation.SMSCycle
		state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
		client.Restore(state)
		if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusActive, activation.BalanceRP); err != nil {
			return m.failHeroSMSCode(ctx, activation, *claimed.code, err)
		}
		if err = m.completeHeroSMSCode(ctx, activation, *claimed.code); err != nil {
			return err
		}
		return m.scheduleHeroSMSWait(ctx, activation, m.heroSMSProviderDeadline(activation))
	}
	if !alreadyAppended {
		if rejected, rejectErr := m.rejectHeroSMSCodeForOccupiedCycle(ctx, activation, *claimed.code); rejected {
			return rejectErr
		}
		eventCtx := withHeroSMSWebhookEvent(ctx, claimed.code.event)
		if _, err = m.appendCode(eventCtx, activation, domain.VerificationPhaseSubsequent, claimed.code.code); err != nil {
			return m.failHeroSMSCode(ctx, activation, *claimed.code, err)
		}
	}
	cycle, state, err := m.advanceVerificationSMSCycle(ctx, activation, state, func(checkpoint gopay.Session) error {
		_, saveErr := m.saveSession(ctx, activation.PhoneNumber, checkpoint, targetPIN, domain.AccountStatusActive, activation.BalanceRP)
		return saveErr
	})
	if err != nil {
		return m.failHeroSMSCode(ctx, activation, *claimed.code, err)
	}
	state.SMSCycle = cycle
	state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	client.Restore(state)
	if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusActive, activation.BalanceRP); err != nil {
		return m.failHeroSMSCode(ctx, activation, *claimed.code, err)
	}
	if err = m.completeHeroSMSCode(ctx, activation, *claimed.code); err != nil {
		return err
	}
	return m.scheduleHeroSMSWait(ctx, activation, m.heroSMSProviderDeadline(activation))
}

// ensureHeroSMSFollowupCycle opens a fresh provider cycle after the PIN OTP (or
// previous follow-up) consumed the current one. The verification row is the
// durable boundary: once activation.SMSCycle is greater than the newest stored
// code's cycle, setStatus=3 has already been accepted and must not be repeated.
func (m *Manager) ensureHeroSMSFollowupCycle(
	ctx context.Context,
	activation domain.Activation,
) (domain.Activation, error) {
	verifications, err := m.store.ListVerificationCodes(ctx, activation.ID)
	if err != nil {
		return activation, err
	}
	latestCodeCycle := -1
	for _, verification := range verifications {
		if verification.CycleNo > latestCodeCycle {
			latestCodeCycle = verification.CycleNo
		}
	}
	batch, _, client, err := m.restoreGoPayClient(ctx, activation)
	if err != nil {
		return activation, err
	}
	targetPIN, err := m.targetPIN(batch)
	if err != nil {
		return activation, err
	}
	state := client.State()
	if latestCodeCycle < activation.SMSCycle {
		if state.SMSCycle != activation.SMSCycle || state.VerificationCycleRequest != gopay.VerificationCycleRequestNone {
			state.SMSCycle = activation.SMSCycle
			state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
			client.Restore(state)
			_, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusActive, activation.BalanceRP)
		}
		return activation, err
	}
	cycle, state, err := m.advanceVerificationSMSCycle(ctx, activation, state, func(checkpoint gopay.Session) error {
		_, saveErr := m.saveSession(ctx, activation.PhoneNumber, checkpoint, targetPIN, domain.AccountStatusActive, activation.BalanceRP)
		return saveErr
	})
	if err != nil {
		return activation, err
	}
	state.SMSCycle = cycle
	state.VerificationCycleRequest = gopay.VerificationCycleRequestNone
	client.Restore(state)
	if _, err = m.saveSession(ctx, activation.PhoneNumber, client.State(), targetPIN, domain.AccountStatusActive, activation.BalanceRP); err != nil {
		return activation, err
	}
	activation.SMSCycle = cycle
	return activation, nil
}
