package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

// AccountLoginStatusView is the public, credential-free representation of a
// GoPay session probe.  The encrypted session and all tokens stay in storage.
type AccountLoginStatusView struct {
	ID          int64             `json:"id"`
	PhoneNumber string            `json:"phone_number"`
	Status      gopay.LoginStatus `json:"login_status"`
	// State is a compatibility alias for clients that use a generic status
	// field on account rows. It mirrors Status and carries no business status.
	State     gopay.LoginStatus `json:"status"`
	Valid     bool              `json:"valid"`
	CheckedAt time.Time         `json:"checked_at"`
	Error     string            `json:"error,omitempty"`
	Refreshed bool              `json:"refreshed,omitempty"`
}

type loginStatusCacheEntry struct {
	view              AccountLoginStatusView
	expiresAt         time.Time
	credentialVersion credentialDigest
}

type credentialDigest [sha256.Size]byte

type loginStatusProbeOutcome struct {
	view              AccountLoginStatusView
	credentialVersion credentialDigest
	cacheable         bool
}

type loginStatusFlight struct {
	done              chan struct{}
	view              AccountLoginStatusView
	credentialVersion credentialDigest
}

const (
	loginStatusTransientCacheTTL = 5 * time.Second
	loginStatusPersistTimeout    = 5 * time.Second
	loginStatusPersistAttempts   = 3
	loginStatusProbeTimeout      = 45 * time.Second
)

// ListAccountLoginStatuses returns a snapshot for every saved GoPay account.
// Remote probes are cached briefly because the browser refreshes this endpoint
// frequently; concurrent requests for one account share a single in-flight
// probe and therefore cannot race a refresh-token rotation.
func (m *Manager) ListAccountLoginStatuses(ctx context.Context) ([]AccountLoginStatusView, error) {
	if m == nil || m.store == nil {
		return nil, fmt.Errorf("workflow: account login status manager is unavailable")
	}
	const pageSize = 500
	accounts := make([]domain.Account, 0, pageSize)
	for offset := 0; ; offset += pageSize {
		page, err := m.store.ListAccounts(ctx, storage.AccountFilter{Page: storage.Page{Limit: pageSize, Offset: offset}})
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, page...)
		if len(page) < pageSize {
			break
		}
	}
	views := make([]AccountLoginStatusView, len(accounts))
	workers := 8
	if len(accounts) < workers {
		workers = len(accounts)
	}
	if workers == 0 {
		return views, nil
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for index := range accounts {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				views[index] = loginStatusViewForContext(accounts[index], ctx.Err())
				return
			}
			defer func() { <-sem }()
			views[index] = m.accountLoginStatus(ctx, accounts[index])
		}()
	}
	wg.Wait()
	return views, nil
}

// RefreshAccountLoginStatuses invalidates the in-memory snapshot and probes
// immediately. It is used by the explicit “立即刷新” action; normal polling
// should call ListAccountLoginStatuses so the short remote cache is respected.
func (m *Manager) RefreshAccountLoginStatuses(ctx context.Context) ([]AccountLoginStatusView, error) {
	if m == nil || m.store == nil {
		return nil, fmt.Errorf("workflow: account login status manager is unavailable")
	}
	m.loginStatusMu.Lock()
	m.loginStatusCache = make(map[int64]loginStatusCacheEntry)
	m.loginStatusMu.Unlock()
	return m.ListAccountLoginStatuses(ctx)
}

// GetAccountLoginStatus probes one account and is used by callers that need a
// focused refresh without downloading the complete account list.
func (m *Manager) GetAccountLoginStatus(ctx context.Context, id int64) (AccountLoginStatusView, error) {
	if m == nil || m.store == nil {
		return AccountLoginStatusView{}, fmt.Errorf("workflow: account login status manager is unavailable")
	}
	if id <= 0 {
		return AccountLoginStatusView{}, storage.ErrInvalidInput
	}
	account, err := m.store.GetAccount(ctx, id)
	if err != nil {
		return AccountLoginStatusView{}, err
	}
	return m.accountLoginStatus(ctx, account), nil
}

func (m *Manager) accountLoginStatus(ctx context.Context, account domain.Account) AccountLoginStatusView {
	key := account.ID
	if err := ctx.Err(); err != nil {
		return loginStatusViewForContext(account, err)
	}
	version := accountCredentialDigest(account.CredentialsEnc)
	now := time.Now().UTC()

	m.loginStatusMu.Lock()
	if m.loginStatusCache == nil {
		m.loginStatusCache = make(map[int64]loginStatusCacheEntry)
	}
	if m.loginStatusFlights == nil {
		m.loginStatusFlights = make(map[int64]*loginStatusFlight)
	}
	if cached, ok := m.loginStatusCache[key]; ok && now.Before(cached.expiresAt) && cached.credentialVersion == version {
		view := cached.view
		m.loginStatusMu.Unlock()
		return view
	}
	flight := m.loginStatusFlights[key]
	if flight == nil {
		flight = &loginStatusFlight{done: make(chan struct{})}
		m.loginStatusFlights[key] = flight
		// The shared remote probe is service-owned rather than request-owned. A
		// browser disconnect can stop waiting without abandoning a token refresh
		// that may already have committed remotely.
		go m.runAccountLoginStatusProbe(key, account, flight)
	}
	m.loginStatusMu.Unlock()

	select {
	case <-flight.done:
		// A worker can save a newer session in the narrow window after the
		// service-owned probe releases its account lock and before this flight is
		// published. Do not hand that stale result to a caller which already read
		// the newer durable credentials.
		if flight.credentialVersion == version {
			return flight.view
		}
		latest, err := m.store.GetAccount(ctx, account.ID)
		if err != nil {
			return loginStatusViewForContext(account, err)
		}
		if accountCredentialDigest(latest.CredentialsEnc) == flight.credentialVersion {
			return flight.view
		}
		m.loginStatusMu.Lock()
		if m.loginStatusFlights[account.ID] == flight {
			delete(m.loginStatusFlights, account.ID)
		}
		m.loginStatusMu.Unlock()
		return m.accountLoginStatus(ctx, latest)
	case <-ctx.Done():
		return loginStatusViewForContext(account, ctx.Err())
	}
}

func (m *Manager) runAccountLoginStatusProbe(key int64, account domain.Account, flight *loginStatusFlight) {
	probeCtx, cancel := context.WithTimeout(context.Background(), loginStatusProbeTimeout)
	defer cancel()
	outcome := m.probeAccountLoginStatus(probeCtx, account)
	cacheTTL := m.cfg.LoginStatusTTL
	if cacheTTL <= 0 {
		cacheTTL = 4 * time.Second
	}
	// Unknown results are transient, and an invalid result can become stale
	// immediately after a login on another instance. Recheck both promptly;
	// only a confirmed valid session receives the longer cache TTL.
	if outcome.view.Status != gopay.LoginStatusValid && cacheTTL > loginStatusTransientCacheTTL {
		cacheTTL = loginStatusTransientCacheTTL
	}

	m.loginStatusMu.Lock()
	flight.view = outcome.view
	flight.credentialVersion = outcome.credentialVersion
	if outcome.cacheable {
		// Start the cache window at the actual probe time. Starting it after the
		// remote request completes makes a five-second browser poll reuse the
		// result once more and stretches the effective remote check interval to
		// almost ten seconds.
		expiresAt := outcome.view.CheckedAt.Add(cacheTTL)
		if outcome.view.CheckedAt.IsZero() {
			expiresAt = time.Now().UTC().Add(cacheTTL)
		}
		m.loginStatusCache[key] = loginStatusCacheEntry{
			view:              outcome.view,
			expiresAt:         expiresAt,
			credentialVersion: outcome.credentialVersion,
		}
	} else {
		delete(m.loginStatusCache, key)
	}
	if m.loginStatusFlights[key] == flight {
		delete(m.loginStatusFlights, key)
	}
	close(flight.done)
	m.loginStatusMu.Unlock()
}

func (m *Manager) probeAccountLoginStatus(ctx context.Context, account domain.Account) loginStatusProbeOutcome {
	releaseAccount, lockErr := m.acquireAccountSessionLock(ctx, account.PhoneNumber)
	if lockErr != nil {
		view := loginStatusViewForContext(account, lockErr)
		return loginStatusProbeOutcome{
			view: view, credentialVersion: accountCredentialDigest(account.CredentialsEnc), cacheable: !contextError(lockErr),
		}
	}
	defer releaseAccount()

	// The account may have changed while this request waited for a business
	// worker. Start only from the latest durable encrypted session.
	latest, err := m.store.GetAccount(ctx, account.ID)
	if err != nil {
		view := loginStatusViewForContext(account, err)
		return loginStatusProbeOutcome{view: view, credentialVersion: accountCredentialDigest(account.CredentialsEnc), cacheable: ctx.Err() == nil}
	}
	account = latest
	version := accountCredentialDigest(account.CredentialsEnc)
	view := AccountLoginStatusView{
		ID: account.ID, PhoneNumber: account.PhoneNumber,
		Status: gopay.LoginStatusUnknown, State: gopay.LoginStatusUnknown,
		CheckedAt: time.Now().UTC(),
	}
	if len(account.CredentialsEnc) == 0 {
		view.Error = "账号尚未完成登录，暂时没有可检查的会话"
		return loginStatusProbeOutcome{view: view, credentialVersion: version, cacheable: true}
	}
	if m.box == nil {
		view.Error = "服务端加密组件未就绪"
		return loginStatusProbeOutcome{view: view, credentialVersion: version, cacheable: true}
	}
	raw, err := m.box.Open(account.CredentialsEnc)
	if err != nil {
		view.Error = "账号凭据读取失败"
		return loginStatusProbeOutcome{view: view, credentialVersion: version, cacheable: true}
	}
	session, err := gopay.ParseSession(raw)
	if err != nil {
		view.Error = "账号会话数据无效"
		return loginStatusProbeOutcome{view: view, credentialVersion: version, cacheable: true}
	}
	if strings.TrimSpace(session.AccessToken) == "" && strings.TrimSpace(session.RefreshToken) == "" {
		view.Error = "账号尚未完成登录，暂时没有可检查的会话"
		return loginStatusProbeOutcome{view: view, credentialVersion: version, cacheable: true}
	}
	client, err := gopay.NewClientForPhone(account.PhoneNumber, gopay.Config{
		SSOBaseURL: m.cfg.SSOBaseURL, GoPayBaseURL: m.cfg.GoPayBaseURL,
		ProxyURL: session.ProxyURL, Session: &session,
	})
	if err != nil {
		view.Error = "账号会话客户端初始化失败"
		return loginStatusProbeOutcome{view: view, credentialVersion: version, cacheable: true}
	}
	result, probeErr := client.CheckLoginStatus(ctx)
	view.Status = result.Status
	view.State = result.Status
	view.Valid = result.Valid()
	view.Refreshed = result.Refreshed
	if result.Refreshed {
		persistedVersion, persistErr := m.persistRotatedSession(ctx, account, client.State())
		if persistErr != nil {
			view.Status = gopay.LoginStatusUnknown
			view.State = gopay.LoginStatusUnknown
			view.Valid = false
			if errors.Is(persistErr, storage.ErrConflict) {
				view.Error = "账号登录凭据刚刚发生变化，系统将重新检查"
			} else {
				view.Error = "登录凭据已刷新，但保存失败，系统将自动重试"
			}
			return loginStatusProbeOutcome{view: view, credentialVersion: version, cacheable: ctx.Err() == nil && !contextError(probeErr)}
		}
		version = persistedVersion
	}
	if probeErr != nil {
		if errors.Is(probeErr, gopay.ErrLoginFailed) {
			view.Error = "登录失败"
		} else if result.Status == gopay.LoginStatusInvalid {
			view.Error = "登录已失效，请重新登录"
		} else if view.Error == "" {
			view.Error = publicProbeError(probeErr)
		}
	}

	// A different process may have replaced the encrypted session while this
	// probe was running. Never cache a stale invalid result against that newer
	// credential version.
	if view.Status == gopay.LoginStatusInvalid && !result.Refreshed && ctx.Err() == nil {
		if current, currentErr := m.store.GetAccount(ctx, account.ID); currentErr == nil {
			currentVersion := accountCredentialDigest(current.CredentialsEnc)
			if currentVersion != version {
				view.Status = gopay.LoginStatusUnknown
				view.State = gopay.LoginStatusUnknown
				view.Valid = false
				view.Error = "账号登录凭据刚刚发生变化，系统将重新检查"
				version = currentVersion
			}
		}
	}
	return loginStatusProbeOutcome{view: view, credentialVersion: version, cacheable: ctx.Err() == nil && !contextError(probeErr)}
}

func (m *Manager) persistRotatedSession(requestCtx context.Context, account domain.Account, session gopay.Session) (credentialDigest, error) {
	if m.box == nil || account.ID <= 0 {
		return credentialDigest{}, fmt.Errorf("account credentials cannot be persisted")
	}
	updater, ok := m.store.(storage.AccountCredentialCASStore)
	if !ok {
		return credentialDigest{}, fmt.Errorf("account credential CAS persistence is unavailable")
	}
	// A refresh token may already have rotated remotely when the browser closes
	// its request. Give the narrow durable write an independent, bounded window.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), loginStatusPersistTimeout)
	defer cancel()
	raw, err := session.Marshal()
	if err != nil {
		return credentialDigest{}, err
	}
	encrypted, err := m.box.Seal(raw)
	if err != nil {
		return credentialDigest{}, err
	}
	deviceState, err := json.Marshal(session.Device)
	if err != nil {
		return credentialDigest{}, err
	}
	nextVersion := accountCredentialDigest(encrypted)
	var lastErr error
	for attempt := 0; attempt < loginStatusPersistAttempts; attempt++ {
		lastErr = updater.UpdateAccountCredentialsIfUnchanged(
			ctx, account.ID, account.CredentialsEnc, encrypted, deviceState,
		)
		if lastErr == nil {
			return nextVersion, nil
		}

		// Resolve both a real CAS conflict and an uncertain write outcome. If the
		// exact ciphertext is durable, the rotation succeeded; if the expected
		// ciphertext changed, another session writer won and this probe is stale.
		current, getErr := m.store.GetAccount(ctx, account.ID)
		if getErr == nil {
			switch {
			case bytes.Equal(current.CredentialsEnc, encrypted):
				return nextVersion, nil
			case !bytes.Equal(current.CredentialsEnc, account.CredentialsEnc):
				return credentialDigest{}, storage.ErrConflict
			}
		}
		if errors.Is(lastErr, storage.ErrConflict) || errors.Is(lastErr, storage.ErrNotFound) || ctx.Err() != nil {
			return credentialDigest{}, lastErr
		}

		delay := time.Duration(attempt+1) * 75 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return credentialDigest{}, ctx.Err()
		}
	}
	return credentialDigest{}, lastErr
}

func accountCredentialDigest(credentials []byte) credentialDigest {
	return credentialDigest(sha256.Sum256(credentials))
}

func contextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func loginStatusViewForContext(account domain.Account, err error) AccountLoginStatusView {
	view := AccountLoginStatusView{
		ID: account.ID, PhoneNumber: account.PhoneNumber,
		Status: gopay.LoginStatusUnknown, State: gopay.LoginStatusUnknown, CheckedAt: time.Now().UTC(),
	}
	if err != nil {
		view.Error = publicProbeError(err)
	}
	return view
}

func publicProbeError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "登录状态检查已取消，系统将自动重试"
	case errors.Is(err, context.DeadlineExceeded):
		return "登录状态检查超时，系统将自动重试"
	case errors.Is(err, gopay.ErrLoginExpired):
		return "登录已失效，请重新登录"
	case errors.Is(err, gopay.ErrLoginFailed):
		return "登录失败"
	}
	var httpErr *gopay.HTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.StatusCode == http.StatusTooManyRequests:
			return "GoPay 请求频率受限，系统将稍后重试"
		case httpErr.StatusCode == http.StatusForbidden:
			return "GoPay 暂时拒绝此次状态检查，系统将稍后重试"
		case httpErr.StatusCode >= http.StatusInternalServerError:
			return "GoPay 服务暂时异常，系统将稍后重试"
		default:
			return "GoPay 登录状态检查暂时异常，系统将自动重试"
		}
	}
	// Upstream errors can contain echoed headers or request data. Public status
	// responses intentionally use a closed set of local messages instead.
	return "GoPay 登录状态检查暂时异常，系统将自动重试"
}
