package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"golang.org/x/sync/singleflight"
)

const (
	codexCredentialRefreshLead       = 5 * time.Minute
	codexVersion                     = codexauth.DefaultClientVersion
	codexOriginator                  = codexauth.DefaultOriginator
	codexUserAgent                   = codexauth.DefaultUserAgent
	codexQuotaOverdraftWriteAttempts = 3
)

var codexHTTPForwardHeaders = []string{
	"X-Codex-Beta-Features",
	"Version",
	"X-Codex-Turn-State",
	"X-Codex-Turn-Metadata",
	"X-Client-Request-Id",
	"User-Agent",
	"Session_id",
	"Session-Id",
	"Thread-Id",
	"Originator",
}

type codexCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*codexauth.Credential
	refreshes        singleflight.Group
	refreshTracker   *oauthCredentialRefreshTracker
	service          *codexauth.Service
	store            storage.Store
	clientFor        func(*model.Config) *http.Client
	invalidateConfig func(int64)
	now              func() time.Time
	passiveLocks     [64]sync.Mutex
	passiveSamples   map[int64]map[string]time.Time
}

// codexCredentialRefreshError keeps the exact persisted credential snapshot
// used by a failed refresh. Runtime rejection handling can then disable that
// snapshot atomically without touching a concurrently reauthorized channel.
type codexCredentialRefreshError struct {
	cause      error
	authType   string
	credential string
}

func (e *codexCredentialRefreshError) Error() string {
	if e == nil || e.cause == nil {
		return "Codex credential refresh failed"
	}
	return e.cause.Error()
}

func (e *codexCredentialRefreshError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newCodexCredentialRefreshError(cfg *model.Config, cause error) error {
	if cause == nil {
		return nil
	}
	if cfg == nil {
		return cause
	}
	return &codexCredentialRefreshError{
		cause: cause, authType: cfg.GetAuthType(), credential: cfg.OAuthCredential,
	}
}

type codexPassiveUsageUpdate struct {
	Windows   []codexauth.PassiveUsageWindow
	SampledAt string
}

func newCodexCredentialManager(
	service *codexauth.Service,
	store storage.Store,
	clientFor func(*model.Config) *http.Client,
	invalidate func(int64),
) *codexCredentialManager {
	return &codexCredentialManager{
		entries: make(map[int64]*codexauth.Credential), service: service,
		store: store, clientFor: clientFor, invalidateConfig: invalidate, now: time.Now,
		passiveSamples: make(map[int64]map[string]time.Time),
	}
}

func (m *codexCredentialManager) credential(ctx context.Context, cfg *model.Config, forceRefresh bool) (*codexauth.Credential, error) {
	return m.credentialForRejectedAccessToken(ctx, cfg, forceRefresh, "")
}

func (m *codexCredentialManager) credentialAfterUnauthorized(ctx context.Context, cfg *model.Config, rejectedAccessToken string) (*codexauth.Credential, error) {
	if rejectedAccessToken == "" {
		return nil, errors.New("codex rejected access token is required")
	}
	return m.credentialForRejectedAccessToken(ctx, cfg, true, rejectedAccessToken)
}

func (m *codexCredentialManager) credentialForRejectedAccessToken(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
	rejectedAccessToken string,
) (*codexauth.Credential, error) {
	if m == nil || m.service == nil || m.store == nil || cfg == nil || !cfg.UsesCodexOAuth() {
		return nil, errors.New("codex credential manager is unavailable")
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	if credential.IsPersonalAccessToken() {
		if forceRefresh {
			return cloneCodexCredential(credential), newCodexCredentialRefreshError(
				cfg, codexauth.ErrPersonalAccessTokenCannotRefresh,
			)
		}
		return cloneCodexCredential(credential), nil
	}
	needsRefresh, err := credential.NeedsRefresh(m.now(), codexCredentialRefreshLead)
	if err != nil {
		return nil, err
	}
	if !forceRefresh && !needsRefresh {
		return cloneCodexCredential(credential), nil
	}
	forcedAccessToken := credential.AccessToken
	if rejectedAccessToken != "" {
		forcedAccessToken = rejectedAccessToken
	}
	resultCh := m.refreshes.DoChan(oauthCredentialRefreshSingleflightKey(cfg.ID, forcedAccessToken, true), func() (any, error) {
		refreshCtx := context.Background()
		if m.refreshTracker != nil {
			trackedCtx, done, beginErr := m.refreshTracker.begin()
			if beginErr != nil {
				return nil, beginErr
			}
			defer done()
			refreshCtx = trackedCtx
		} else if ctx != nil {
			refreshCtx = context.WithoutCancel(ctx)
		}
		currentCfg, getErr := m.store.GetConfig(refreshCtx, cfg.ID)
		if getErr != nil {
			return nil, fmt.Errorf("reload Codex credential before refresh: %w", getErr)
		}
		current, parseErr := codexauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, fmt.Errorf("parse Codex credential for channel %d: %w", currentCfg.ID, parseErr)
		}
		if current.AccessToken != forcedAccessToken {
			winner, reconcileErr := applyCodexWinnerModelState(refreshCtx, m.store, currentCfg, "", current)
			if reconcileErr != nil {
				return nil, reconcileErr
			}
			m.cache(currentCfg.ID, winner)
			return oauthCredentialRefreshRedirect{}, nil
		}
		service := *m.service
		if m.clientFor != nil {
			service.Client = m.clientFor(currentCfg)
		}
		refreshed, refreshErr := service.Refresh(refreshCtx, current.RefreshToken)
		if refreshErr != nil {
			winnerCfg, winnerErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
			if winnerErr == nil && winnerCfg.OAuthCredential != currentCfg.OAuthCredential && winnerCfg.UsesCodexOAuth() {
				winner, parseWinnerErr := codexauth.ParseCredential([]byte(winnerCfg.OAuthCredential))
				if parseWinnerErr == nil &&
					(winner.AccessToken != current.AccessToken || winner.RefreshToken != current.RefreshToken) {
					winner, reconcileErr := applyCodexWinnerModelState(
						refreshCtx, m.store, winnerCfg, current.PlanType, winner,
					)
					if reconcileErr != nil {
						return nil, reconcileErr
					}
					m.cache(currentCfg.ID, winner)
					return cloneCodexCredential(winner), nil
				}
			}
			return nil, newCodexCredentialRefreshError(
				currentCfg,
				fmt.Errorf("refresh Codex credential for channel %d: %w", currentCfg.ID, refreshErr),
			)
		}
		return m.persistRefreshResult(refreshCtx, currentCfg, current, refreshed)
	})
	var result singleflight.Result
	if ctx == nil {
		result = <-resultCh
	} else {
		select {
		case result = <-resultCh:
		case <-ctx.Done():
			return cloneCodexCredential(credential), ctx.Err()
		}
	}
	if result.Err != nil {
		return cloneCodexCredential(credential), result.Err
	}
	if _, redirected := result.Val.(oauthCredentialRefreshRedirect); redirected {
		winner, winnerErr := m.cachedOrParse(cfg)
		if winnerErr != nil {
			return nil, winnerErr
		}
		if rejectedAccessToken != "" {
			return cloneCodexCredential(winner), nil
		}
		return m.credentialForRejectedAccessToken(ctx, cfg, false, "")
	}
	return result.Val.(*codexauth.Credential), nil
}

func (m *codexCredentialManager) persistRefreshResult(
	ctx context.Context,
	cfg *model.Config,
	refreshedFrom *codexauth.Credential,
	refreshed *codexauth.Credential,
) (*codexauth.Credential, error) {
	currentCfg := cfg
	current := refreshedFrom
	for {
		if current.AccessToken != refreshedFrom.AccessToken || current.RefreshToken != refreshedFrom.RefreshToken {
			winner, err := applyCodexWinnerModelState(
				ctx, m.store, currentCfg, refreshedFrom.PlanType, current,
			)
			if err != nil {
				return nil, err
			}
			m.cache(currentCfg.ID, winner)
			return cloneCodexCredential(winner), nil
		}
		merged, err := current.MergeRefresh(refreshed)
		if err != nil {
			return nil, err
		}
		payload, err := merged.JSON()
		if err != nil {
			return nil, err
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(
			ctx, currentCfg.ID, model.AuthTypeCodexOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return nil, err
		}
		if updated {
			persisted, persistErr := persistCodexModelState(
				ctx, m.store, currentCfg, current.PlanType, merged, payload,
			)
			if persistErr != nil {
				return nil, persistErr
			}
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			m.cache(currentCfg.ID, persisted)
			return cloneCodexCredential(persisted), nil
		}
		currentCfg, err = m.store.GetConfig(ctx, currentCfg.ID)
		if err != nil {
			return nil, fmt.Errorf("reload Codex credential after concurrent update: %w", err)
		}
		if !currentCfg.UsesCodexOAuth() {
			return nil, errors.New("codex credential changed provider during refresh persistence")
		}
		current, err = codexauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return nil, fmt.Errorf("parse Codex credential after concurrent update: %w", err)
		}
	}
}

func (m *codexCredentialManager) cache(channelID int64, credential *codexauth.Credential) {
	m.mu.Lock()
	m.entries[channelID] = cloneCodexCredential(credential)
	m.mu.Unlock()
}

func (m *codexCredentialManager) cachedOrParse(cfg *model.Config) (*codexauth.Credential, error) {
	m.mu.RLock()
	credential := m.entries[cfg.ID]
	m.mu.RUnlock()
	if credential != nil {
		return cloneCodexCredential(credential), nil
	}
	parsed, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse Codex credential for channel %d: %w", cfg.ID, err)
	}
	m.mu.Lock()
	if existing := m.entries[cfg.ID]; existing != nil {
		parsed = cloneCodexCredential(existing)
	} else {
		m.entries[cfg.ID] = cloneCodexCredential(parsed)
	}
	m.mu.Unlock()
	return parsed, nil
}

func (m *codexCredentialManager) invalidate(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	delete(m.passiveSamples, channelID)
	m.mu.Unlock()
}

func (m *codexCredentialManager) invalidateCredentialCache(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	m.mu.Unlock()
}

func cloneCodexCredential(credential *codexauth.Credential) *codexauth.Credential {
	if credential == nil {
		return nil
	}
	clone := *credential
	clone.PassiveUsage = codexauth.ClonePassiveUsage(credential.PassiveUsage)
	clone.OAuthUsage = append([]byte(nil), credential.OAuthUsage...)
	clone.QuotaCostUsage = oauthcost.Clone(credential.QuotaCostUsage)
	clone.QuotaOverdraft = codexauth.CloneQuotaOverdraft(credential.QuotaOverdraft)
	return &clone
}

func (m *codexCredentialManager) updateQuotaOverdraft(
	ctx context.Context,
	channelID int64,
	mutate func(*codexauth.QuotaOverdraft, bool, *codexauth.Credential) (bool, error),
) (*codexauth.QuotaOverdraft, error) {
	if m == nil || m.store == nil || mutate == nil {
		return nil, errors.New("codex credential manager is unavailable")
	}
	transientFailures := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		currentCfg, err := m.store.GetConfig(ctx, channelID)
		if err != nil {
			transientFailures++
			if transientFailures < codexQuotaOverdraftWriteAttempts && isRetryableCodexCredentialStoreError(err) {
				if err := waitCodexQuotaOverdraftRetry(ctx, transientFailures); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("reload Codex quota overdraft: %w", err)
		}
		if !currentCfg.UsesCodexOAuth() {
			return nil, errors.New("codex credential changed provider")
		}
		current, err := codexauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return nil, fmt.Errorf("parse Codex quota overdraft: %w", err)
		}
		existed := current.QuotaOverdraft != nil
		next := codexauth.CloneQuotaOverdraft(current.QuotaOverdraft)
		if next == nil {
			next = &codexauth.QuotaOverdraft{}
		}
		changed, err := mutate(next, existed, current)
		if err != nil {
			return nil, err
		}
		if !changed {
			return codexauth.CloneQuotaOverdraft(next), nil
		}
		updatedCredential := *current
		updatedCredential.QuotaOverdraft = next
		payload, err := updatedCredential.JSON()
		if err != nil {
			return nil, err
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(
			ctx, currentCfg.ID, model.AuthTypeCodexOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			transientFailures++
			if transientFailures < codexQuotaOverdraftWriteAttempts && isRetryableCodexCredentialStoreError(err) {
				if err := waitCodexQuotaOverdraftRetry(ctx, transientFailures); err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}
		if !updated {
			continue
		}
		m.invalidateCredentialCache(currentCfg.ID)
		if m.invalidateConfig != nil {
			m.invalidateConfig(currentCfg.ID)
		}
		return codexauth.CloneQuotaOverdraft(next), nil
	}
}

func waitCodexQuotaOverdraftRetry(ctx context.Context, transientFailures int) error {
	timer := time.NewTimer(time.Duration(transientFailures) * 25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isRetryableCodexCredentialStoreError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, pattern := range []string{
		"database is locked",
		"database is deadlocked",
		"database table is locked",
		"sqlite_busy",
		"sqlite_locked",
		"deadlock found",
		"lock wait timeout",
		"serialization failure",
		"could not serialize access",
		"try restarting transaction",
	} {
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

func (m *codexCredentialManager) setQuotaOverdraftEnabled(
	ctx context.Context,
	channelID int64,
	enabled bool,
) (*codexauth.QuotaOverdraft, error) {
	return m.updateQuotaOverdraft(ctx, channelID, func(overdraft *codexauth.QuotaOverdraft, existed bool, _ *codexauth.Credential) (bool, error) {
		changed := !existed || overdraft.Enabled != enabled
		overdraft.Enabled = enabled
		if !enabled && overdraft.ActiveUntil != 0 {
			overdraft.ActiveUntil = 0
			changed = true
		}
		if !changed {
			return false, nil
		}
		return true, nil
	})
}

func (m *codexCredentialManager) clearQuotaOverdraftWindow(ctx context.Context, channelID int64) error {
	_, err := m.updateQuotaOverdraft(ctx, channelID, func(
		overdraft *codexauth.QuotaOverdraft,
		existed bool,
		_ *codexauth.Credential,
	) (bool, error) {
		if !existed || overdraft.ActiveUntil == 0 {
			return false, nil
		}
		overdraft.ActiveUntil = 0
		return true, nil
	})
	return err
}

func (m *codexCredentialManager) recordQuotaOverdraftSuccess(
	ctx context.Context,
	channelID int64,
	costMicroUSD int64,
	replayed bool,
	activeUntil int64,
) (*codexauth.QuotaOverdraft, bool, error) {
	if costMicroUSD < 0 {
		return nil, false, errors.New("codex quota overdraft cost cannot be negative")
	}
	recorded := false
	stats, err := m.updateQuotaOverdraft(ctx, channelID, func(
		overdraft *codexauth.QuotaOverdraft,
		_ bool,
		credential *codexauth.Credential,
	) (bool, error) {
		// updateQuotaOverdraft may invoke the mutation again after a CAS miss.
		// The returned decision must describe the winning snapshot only.
		recorded = false
		if !overdraft.Enabled {
			return false, nil
		}
		now := time.Now().Unix()
		if replayed && activeUntil > now && activeUntil > overdraft.ActiveUntil {
			overdraft.ActiveUntil = activeUntil
		}
		if !replayed && overdraft.ActiveUntil <= now {
			if overdraft.ActiveUntil != 0 || overdraft.SuccessfulRequests <= 0 {
				return false, nil
			}
			// Legacy credentials created before active_until existed can recover an
			// already-confirmed cycle from the latest persisted primary quota window.
			overdraft.ActiveUntil = legacyCodexQuotaOverdraftActiveUntil(credential, now)
			if overdraft.ActiveUntil <= now {
				return false, nil
			}
		}
		if overdraft.SuccessfulRequests == math.MaxInt64 || costMicroUSD > math.MaxInt64-overdraft.CostMicroUSD {
			return false, errors.New("codex quota overdraft statistics overflow")
		}
		overdraft.SuccessfulRequests++
		overdraft.CostMicroUSD += costMicroUSD
		recorded = true
		return true, nil
	})
	return stats, recorded, err
}

func legacyCodexQuotaOverdraftActiveUntil(credential *codexauth.Credential, now int64) int64 {
	if credential == nil || credential.PassiveUsage == nil {
		return 0
	}
	var activeUntil int64
	for _, window := range credential.PassiveUsage.Windows {
		if strings.EqualFold(strings.TrimSpace(window.Scope), codexauth.ChannelType) &&
			strings.EqualFold(strings.TrimSpace(window.Kind), "primary") &&
			window.UsedPercent >= 100 && window.ResetAt > now && window.ResetAt > activeUntil {
			activeUntil = window.ResetAt
		}
	}
	return activeUntil
}

func (m *codexCredentialManager) updatePassiveUsage(
	ctx context.Context,
	cfg *model.Config,
	update codexPassiveUsageUpdate,
) (bool, error) {
	if m == nil || m.store == nil || cfg == nil || !cfg.UsesCodexOAuth() {
		return false, errors.New("codex credential manager is unavailable")
	}
	if len(update.Windows) == 0 {
		return false, nil
	}
	updateTime, err := time.Parse(time.RFC3339, strings.TrimSpace(update.SampledAt))
	if err != nil {
		return false, errors.New("codex passive usage has invalid sample time")
	}
	usageLock := &m.passiveLocks[uint64(cfg.ID)%uint64(len(m.passiveLocks))]
	usageLock.Lock()
	defer usageLock.Unlock()
	update.Windows = m.observePassiveUsageWindows(cfg.ID, update.Windows, updateTime)
	if len(update.Windows) == 0 {
		return false, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		currentCfg, err := m.store.GetConfig(ctx, cfg.ID)
		if err != nil {
			return false, fmt.Errorf("reload Codex passive usage: %w", err)
		}
		if !currentCfg.UsesCodexOAuth() {
			return false, errors.New("codex credential changed provider")
		}
		current, err := codexauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return false, fmt.Errorf("parse Codex passive usage: %w", err)
		}
		updatedCredential := *current
		var changed bool
		updatedCredential.PassiveUsage, changed = mergeCodexPassiveUsage(current.PassiveUsage, update.Windows, updateTime)
		nextQuotaCostUsage := reconcileOAuthQuotaCostUsage(
			current.QuotaCostUsage, codexPassiveUsageSummary(&updatedCredential), updateTime,
		)
		quotaCostChanged := !reflect.DeepEqual(current.QuotaCostUsage, nextQuotaCostUsage)
		updatedCredential.QuotaCostUsage = nextQuotaCostUsage
		if !changed && !quotaCostChanged {
			return false, nil
		}
		payload, err := updatedCredential.JSON()
		if err != nil {
			return false, err
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(
			ctx, currentCfg.ID, model.AuthTypeCodexOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return false, err
		}
		if !updated {
			continue
		}
		// A concurrent token refresh may have committed and cached a newer
		// credential after this CAS. Dropping the cache is always safe; caching the
		// local snapshot here could resurrect the old access token in memory.
		m.invalidateCredentialCache(currentCfg.ID)
		return true, nil
	}
}

func (m *codexCredentialManager) observePassiveUsageWindows(
	channelID int64,
	windows []codexauth.PassiveUsageWindow,
	fallbackTime time.Time,
) []codexauth.PassiveUsageWindow {
	m.mu.Lock()
	defer m.mu.Unlock()
	observed := m.passiveSamples[channelID]
	if observed == nil {
		observed = make(map[string]time.Time)
		m.passiveSamples[channelID] = observed
	}
	accepted := make([]codexauth.PassiveUsageWindow, 0, len(windows))
	for _, window := range windows {
		sampledAt, err := time.Parse(time.RFC3339, strings.TrimSpace(window.SampledAt))
		if err != nil {
			sampledAt = fallbackTime
			window.SampledAt = sampledAt.UTC().Format(time.RFC3339Nano)
		}
		key := codexPassiveUsageWindowKey(window)
		if previous, ok := observed[key]; ok && !sampledAt.After(previous) {
			continue
		}
		observed[key] = sampledAt
		accepted = append(accepted, window)
	}
	return accepted
}

func mergeCodexPassiveUsage(
	current *codexauth.PassiveUsage,
	windows []codexauth.PassiveUsageWindow,
	fallbackTime time.Time,
) (*codexauth.PassiveUsage, bool) {
	usage := codexauth.ClonePassiveUsage(current)
	if usage == nil {
		usage = &codexauth.PassiveUsage{}
	}
	indexes := make(map[string]int, len(usage.Windows))
	for i, window := range usage.Windows {
		indexes[codexPassiveUsageWindowKey(window)] = i
	}
	changed := false
	latest := time.Time{}
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(usage.SampledAt)); err == nil {
		latest = parsed
	}
	for _, window := range windows {
		windowTime, err := time.Parse(time.RFC3339, strings.TrimSpace(window.SampledAt))
		if err != nil {
			windowTime = fallbackTime
			window.SampledAt = windowTime.UTC().Format(time.RFC3339Nano)
		}
		key := codexPassiveUsageWindowKey(window)
		if index, ok := indexes[key]; ok {
			currentWindow := usage.Windows[index]
			currentTime, currentErr := time.Parse(time.RFC3339, strings.TrimSpace(currentWindow.SampledAt))
			if currentErr == nil && !windowTime.After(currentTime) {
				continue
			}
			if codexPassiveUsageWindowValueEqual(currentWindow, window) {
				continue
			}
			usage.Windows[index] = window
		} else {
			indexes[key] = len(usage.Windows)
			usage.Windows = append(usage.Windows, window)
		}
		changed = true
		if windowTime.After(latest) {
			latest = windowTime
		}
	}
	if !changed {
		return usage, false
	}
	if latest.IsZero() {
		latest = fallbackTime
	}
	usage.SampledAt = latest.UTC().Format(time.RFC3339Nano)
	return usage, true
}

func codexPassiveUsageWindowKey(window codexauth.PassiveUsageWindow) string {
	limitName := strings.ToLower(strings.TrimSpace(window.LimitName))
	if limitName == "" {
		limitName = strings.ToLower(strings.TrimSpace(window.Scope))
	}
	return limitName + "\x00" +
		strings.ToLower(strings.TrimSpace(window.Kind))
}

func codexPassiveUsageWindowValueEqual(a, b codexauth.PassiveUsageWindow) bool {
	return strings.EqualFold(strings.TrimSpace(a.Scope), strings.TrimSpace(b.Scope)) &&
		a.LimitName == b.LimitName && a.Kind == b.Kind && a.UsedPercent == b.UsedPercent &&
		a.LimitWindowSeconds == b.LimitWindowSeconds && codexPassiveResetSamePeriod(a, b)
}

// codexPassiveResetSamePeriod 判断两次采样的 reset 时间是否指向同一个上游周期。
// 同一个周期有两种精度的表达：响应头给绝对 reset-at，SSE rate_limits 事件只给
// resets_in_seconds（换算成 sampledAt+n，每次都不同）。逐秒比较会把这种抖动当成
// 真实变化，于是几乎每个请求都要重写一次凭证。周期滚动会把 reset 整整推进一个
// 窗口时长，容差取半个窗口足以把它和秒级噪声区分开。
func codexPassiveResetSamePeriod(a, b codexauth.PassiveUsageWindow) bool {
	if a.ResetAt == b.ResetAt {
		return true
	}
	if a.ResetAt <= 0 || b.ResetAt <= 0 || a.LimitWindowSeconds <= 0 {
		return false
	}
	delta := a.ResetAt - b.ResetAt
	if delta < 0 {
		delta = -delta
	}
	return delta*2 < a.LimitWindowSeconds
}

func copyCodexHTTPHeaders(dst, src http.Header) {
	if dst == nil {
		return
	}
	for _, name := range codexHTTPForwardHeaders {
		if value := strings.TrimSpace(src.Get(name)); value != "" {
			dst.Set(name, value)
		}
	}
}

func injectCodexHeaders(req *http.Request, cfg *model.Config, apiKey string, streaming bool) {
	if req == nil || cfg == nil {
		return
	}
	token := apiKey
	if cfg.UsesCodexOAuth() {
		token = cfg.CodexAccessToken
	}
	req.Header.Del("X-Api-Key")
	req.Header.Del("x-goog-api-key")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("Originator", codexOriginator)
	req.Header.Set("Version", codexVersion)
	if cfg.UsesCodexOAuth() && req.Header.Get("Session_id") == "" && req.Header.Get("Session-Id") == "" {
		req.Header.Set("Session_id", util.NewUUIDv4())
	}
	if cfg.UsesCodexOAuth() && cfg.CodexAccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", cfg.CodexAccountID)
	} else {
		req.Header.Del("ChatGPT-Account-ID")
	}
	if cfg.UsesCodexOAuth() && cfg.CodexAccountFedRAMP {
		req.Header.Set("X-OpenAI-FedRAMP", "true")
	} else {
		req.Header.Del("X-OpenAI-FedRAMP")
	}
}
