package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"golang.org/x/sync/singleflight"
)

const (
	codexCredentialRefreshLead = 5 * time.Minute
	codexUserAgent             = "codex-tui/0.146.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.146.0)"
)

var codexHTTPForwardHeaders = []string{
	"X-Codex-Beta-Features",
	"Version",
	"X-Codex-Turn-Metadata",
	"X-Client-Request-Id",
	"User-Agent",
	"Session_id",
	"Session-Id",
	"Originator",
}

type codexCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*codexauth.Credential
	refreshes        singleflight.Group
	service          *codexauth.Service
	store            storage.Store
	clientFor        func(*model.Config) *http.Client
	invalidateConfig func(int64)
	now              func() time.Time
	passiveLocks     [64]sync.Mutex
	passiveSamples   map[int64]map[string]time.Time
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
	if m == nil || m.service == nil || m.store == nil || cfg == nil || !cfg.UsesCodexOAuth() {
		return nil, errors.New("codex credential manager is unavailable")
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	needsRefresh, err := credential.NeedsRefresh(m.now(), codexCredentialRefreshLead)
	if err != nil {
		return nil, err
	}
	if !forceRefresh && !needsRefresh {
		return cloneCodexCredential(credential), nil
	}
	forcedAccessToken := credential.AccessToken
	forceRequested := forceRefresh

	value, err, _ := m.refreshes.Do(fmt.Sprintf("channel:%d", cfg.ID), func() (any, error) {
		refreshCtx := context.Background()
		if ctx != nil {
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
		refreshNeeded, refreshErr := current.NeedsRefresh(m.now(), codexCredentialRefreshLead)
		if refreshErr != nil {
			return nil, refreshErr
		}
		forceCurrent := forceRequested && current.AccessToken == forcedAccessToken
		if !forceCurrent && !refreshNeeded {
			winner, reconcileErr := applyCodexWinnerModelState(refreshCtx, m.store, currentCfg, "", current)
			if reconcileErr != nil {
				return nil, reconcileErr
			}
			m.cache(currentCfg.ID, winner)
			return cloneCodexCredential(winner), nil
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
			return nil, fmt.Errorf("refresh Codex credential for channel %d: %w", currentCfg.ID, refreshErr)
		}
		return m.persistRefreshResult(refreshCtx, currentCfg, current, refreshed)
	})
	if err != nil {
		return nil, err
	}
	return value.(*codexauth.Credential), nil
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
	return &clone
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
		if !changed {
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
		a.LimitWindowSeconds == b.LimitWindowSeconds && a.ResetAt == b.ResetAt
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
	req.Header.Set("Originator", "codex-tui")
	if cfg.UsesCodexOAuth() && req.Header.Get("Session_id") == "" && req.Header.Get("Session-Id") == "" {
		req.Header.Set("Session_id", util.NewUUIDv4())
	}
	if cfg.UsesCodexOAuth() && cfg.CodexAccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", cfg.CodexAccountID)
	} else {
		req.Header.Del("ChatGPT-Account-ID")
	}
}
