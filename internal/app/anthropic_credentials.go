package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/storage"

	"golang.org/x/sync/singleflight"
)

type anthropicCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*anthropicauth.Credential
	refreshes        singleflight.Group
	refreshTracker   *oauthCredentialRefreshTracker
	service          *anthropicauth.Service
	store            storage.Store
	clientFor        func(*model.Config) *http.Client
	invalidateConfig func(int64)
	now              func() time.Time
}

type anthropicCredentialMetadata struct {
	PlanType              string
	PlanTypeSet           bool
	ClaudeCodeTrialEndsAt string
	TrialEndsAtSet        bool
}

type anthropicPassiveUsageUpdate struct {
	FiveHour                *anthropicauth.PassiveUsageWindow
	SevenDay                *anthropicauth.PassiveUsageWindow
	SevenDayOverageIncluded *anthropicauth.PassiveUsageWindow
	SampledAt               string
}

func newAnthropicCredentialManager(
	service *anthropicauth.Service,
	store storage.Store,
	clientFor func(*model.Config) *http.Client,
	invalidate func(int64),
) *anthropicCredentialManager {
	return &anthropicCredentialManager{
		entries: make(map[int64]*anthropicauth.Credential), service: service, store: store,
		clientFor: clientFor, invalidateConfig: invalidate, now: time.Now,
	}
}

func (m *anthropicCredentialManager) credential(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
) (*anthropicauth.Credential, error) {
	return m.credentialForRejectedAccessToken(ctx, cfg, forceRefresh, "")
}

func (m *anthropicCredentialManager) credentialAfterUnauthorized(
	ctx context.Context,
	cfg *model.Config,
	rejectedAccessToken string,
) (*anthropicauth.Credential, error) {
	if rejectedAccessToken == "" {
		return nil, errors.New("anthropic rejected access token is required")
	}
	return m.credentialForRejectedAccessToken(ctx, cfg, true, rejectedAccessToken)
}

func (m *anthropicCredentialManager) credentialForRejectedAccessToken(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
	rejectedAccessToken string,
) (*anthropicauth.Credential, error) {
	if m == nil || m.service == nil || m.store == nil || cfg == nil || !cfg.UsesAnthropicOAuth() {
		return nil, errors.New("anthropic credential manager is unavailable")
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	needsRefresh, err := credential.NeedsRefresh(m.now(), anthropicauth.CredentialRefreshLead)
	if err != nil {
		return nil, err
	}
	if !forceRefresh && !needsRefresh {
		return cloneAnthropicCredential(credential), nil
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
			return nil, fmt.Errorf("reload Anthropic credential before refresh: %w", getErr)
		}
		current, parseErr := anthropicauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, fmt.Errorf("parse Anthropic credential for channel %d: %w", currentCfg.ID, parseErr)
		}
		if current.AccessToken != forcedAccessToken {
			m.cache(currentCfg.ID, current)
			return oauthCredentialRefreshRedirect{}, nil
		}
		service := *m.service
		if m.clientFor != nil {
			service.Client = m.clientFor(currentCfg)
		}
		refreshed, refreshErr := service.Refresh(refreshCtx, current.RefreshToken)
		if refreshErr != nil {
			winner, getErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
			if getErr == nil && winner.OAuthCredential != currentCfg.OAuthCredential {
				// Another instance may already have consumed Anthropic's one-time
				// refresh token. Re-read its CAS winner before surfacing invalid_grant.
				winnerCredential, parseWinnerErr := anthropicauth.ParseCredential([]byte(winner.OAuthCredential))
				if parseWinnerErr == nil &&
					(winnerCredential.AccessToken != current.AccessToken || winnerCredential.RefreshToken != current.RefreshToken) {
					m.cache(winner.ID, winnerCredential)
					return cloneAnthropicCredential(winnerCredential), nil
				}
			}
			return nil, fmt.Errorf("refresh Anthropic credential for channel %d: %w", currentCfg.ID, refreshErr)
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
			return cloneAnthropicCredential(credential), ctx.Err()
		}
	}
	if result.Err != nil {
		return cloneAnthropicCredential(credential), result.Err
	}
	if _, redirected := result.Val.(oauthCredentialRefreshRedirect); redirected {
		winner, winnerErr := m.cachedOrParse(cfg)
		if winnerErr != nil {
			return nil, winnerErr
		}
		if rejectedAccessToken != "" {
			return cloneAnthropicCredential(winner), nil
		}
		return m.credentialForRejectedAccessToken(ctx, cfg, false, "")
	}
	return result.Val.(*anthropicauth.Credential), nil
}

func (m *anthropicCredentialManager) persistRefreshResult(
	ctx context.Context,
	cfg *model.Config,
	refreshedFrom *anthropicauth.Credential,
	refreshed *anthropicauth.Credential,
) (*anthropicauth.Credential, error) {
	currentCfg := cfg
	current := refreshedFrom
	for {
		if current.AccessToken != refreshedFrom.AccessToken || current.RefreshToken != refreshedFrom.RefreshToken {
			m.cache(currentCfg.ID, current)
			return cloneAnthropicCredential(current), nil
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
			ctx, currentCfg.ID, model.AuthTypeAnthropicOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return nil, err
		}
		if updated {
			m.cache(currentCfg.ID, merged)
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			return cloneAnthropicCredential(merged), nil
		}
		currentCfg, err = m.store.GetConfig(ctx, currentCfg.ID)
		if err != nil {
			return nil, fmt.Errorf("reload Anthropic credential after concurrent refresh: %w", err)
		}
		if !currentCfg.UsesAnthropicOAuth() {
			return nil, errors.New("anthropic credential changed provider during refresh persistence")
		}
		current, err = anthropicauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return nil, fmt.Errorf("parse Anthropic credential after concurrent refresh: %w", err)
		}
	}
}

func (m *anthropicCredentialManager) cachedOrParse(cfg *model.Config) (*anthropicauth.Credential, error) {
	m.mu.RLock()
	credential := m.entries[cfg.ID]
	m.mu.RUnlock()
	if credential != nil {
		return cloneAnthropicCredential(credential), nil
	}
	parsed, err := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse Anthropic credential for channel %d: %w", cfg.ID, err)
	}
	m.mu.Lock()
	if existing := m.entries[cfg.ID]; existing != nil {
		parsed = cloneAnthropicCredential(existing)
	} else {
		m.entries[cfg.ID] = cloneAnthropicCredential(parsed)
	}
	m.mu.Unlock()
	return parsed, nil
}

func (m *anthropicCredentialManager) cache(channelID int64, credential *anthropicauth.Credential) {
	m.mu.Lock()
	m.entries[channelID] = cloneAnthropicCredential(credential)
	m.mu.Unlock()
}

func (m *anthropicCredentialManager) invalidate(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	m.mu.Unlock()
}

func (m *anthropicCredentialManager) updateMetadata(
	ctx context.Context,
	cfg *model.Config,
	metadata anthropicCredentialMetadata,
) (bool, error) {
	if m == nil || m.store == nil || cfg == nil || !cfg.UsesAnthropicOAuth() {
		return false, errors.New("anthropic credential manager is unavailable")
	}
	metadata.PlanType = strings.TrimSpace(metadata.PlanType)
	metadata.ClaudeCodeTrialEndsAt = strings.TrimSpace(metadata.ClaudeCodeTrialEndsAt)
	if !metadata.PlanTypeSet && !metadata.TrialEndsAtSet {
		return false, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		currentCfg, err := m.store.GetConfig(ctx, cfg.ID)
		if err != nil {
			return false, fmt.Errorf("reload Anthropic credential metadata: %w", err)
		}
		if !currentCfg.UsesAnthropicOAuth() {
			return false, errors.New("anthropic credential changed provider")
		}
		current, err := anthropicauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return false, fmt.Errorf("parse Anthropic credential metadata: %w", err)
		}
		updatedCredential := *current
		if metadata.PlanTypeSet {
			updatedCredential.PlanType = metadata.PlanType
		}
		if metadata.TrialEndsAtSet {
			updatedCredential.ClaudeCodeTrialEndsAt = metadata.ClaudeCodeTrialEndsAt
		}
		if updatedCredential.PlanType == current.PlanType &&
			updatedCredential.ClaudeCodeTrialEndsAt == current.ClaudeCodeTrialEndsAt {
			m.cache(currentCfg.ID, current)
			return false, nil
		}
		payload, err := updatedCredential.JSON()
		if err != nil {
			return false, err
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(
			ctx, currentCfg.ID, model.AuthTypeAnthropicOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return false, err
		}
		if !updated {
			continue
		}
		m.cache(currentCfg.ID, &updatedCredential)
		if m.invalidateConfig != nil {
			m.invalidateConfig(currentCfg.ID)
		}
		return true, nil
	}
}

func cloneAnthropicCredential(credential *anthropicauth.Credential) *anthropicauth.Credential {
	if credential == nil {
		return nil
	}
	clone := *credential
	clone.PassiveUsage = anthropicauth.ClonePassiveUsage(credential.PassiveUsage)
	clone.OAuthUsage = append([]byte(nil), credential.OAuthUsage...)
	clone.QuotaCostUsage = oauthcost.Clone(credential.QuotaCostUsage)
	return &clone
}

func (m *anthropicCredentialManager) updatePassiveUsage(
	ctx context.Context,
	cfg *model.Config,
	update anthropicPassiveUsageUpdate,
) (bool, error) {
	if m == nil || m.store == nil || cfg == nil || !cfg.UsesAnthropicOAuth() {
		return false, errors.New("anthropic credential manager is unavailable")
	}
	if update.FiveHour == nil && update.SevenDay == nil && update.SevenDayOverageIncluded == nil {
		return false, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		currentCfg, err := m.store.GetConfig(ctx, cfg.ID)
		if err != nil {
			return false, fmt.Errorf("reload Anthropic passive usage: %w", err)
		}
		if !currentCfg.UsesAnthropicOAuth() {
			return false, errors.New("anthropic credential changed provider")
		}
		current, err := anthropicauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return false, fmt.Errorf("parse Anthropic passive usage: %w", err)
		}
		updateSampledAt, updateSampledAtErr := time.Parse(time.RFC3339, update.SampledAt)
		stampAnthropicPassiveWindow(update.FiveHour, update.SampledAt)
		stampAnthropicPassiveWindow(update.SevenDay, update.SampledAt)
		stampAnthropicPassiveWindow(update.SevenDayOverageIncluded, update.SampledAt)
		updatedCredential := *current
		usage := anthropicauth.ClonePassiveUsage(current.PassiveUsage)
		if usage == nil {
			usage = &anthropicauth.PassiveUsage{}
		}
		migrateAnthropicPassiveWindowSampleTimes(usage)
		currentSampledAt := usage.SampledAt
		var changed bool
		usage.FiveHour, changed = mergeAnthropicPassiveWindow(usage.FiveHour, update.FiveHour, currentSampledAt)
		usageChanged := changed
		usage.SevenDay, changed = mergeAnthropicPassiveWindow(usage.SevenDay, update.SevenDay, currentSampledAt)
		usageChanged = usageChanged || changed
		usage.SevenDayOverageIncluded, changed = mergeAnthropicPassiveWindow(
			usage.SevenDayOverageIncluded, update.SevenDayOverageIncluded, currentSampledAt,
		)
		usageChanged = usageChanged || changed
		if !usageChanged {
			m.cache(currentCfg.ID, current)
			return false, nil
		}
		if updateSampledAtErr == nil {
			if currentAt, err := time.Parse(time.RFC3339, currentSampledAt); err != nil || updateSampledAt.After(currentAt) {
				usage.SampledAt = updateSampledAt.UTC().Format(time.RFC3339Nano)
			}
		}
		updatedCredential.PassiveUsage = usage
		updatedCredential.QuotaCostUsage = reconcileOAuthQuotaCostUsage(
			current.QuotaCostUsage, anthropicPassiveUsageSummary(&updatedCredential), updateSampledAt,
		)
		payload, err := updatedCredential.JSON()
		if err != nil {
			return false, err
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(
			ctx, currentCfg.ID, model.AuthTypeAnthropicOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return false, err
		}
		if !updated {
			continue
		}
		m.cache(currentCfg.ID, &updatedCredential)
		if m.invalidateConfig != nil {
			m.invalidateConfig(currentCfg.ID)
		}
		return true, nil
	}
}

func mergeAnthropicPassiveWindow(
	current, update *anthropicauth.PassiveUsageWindow,
	currentFallbackSampledAt string,
) (*anthropicauth.PassiveUsageWindow, bool) {
	if update == nil {
		return current, false
	}
	updateSampledAt, updateSampledAtErr := time.Parse(time.RFC3339, strings.TrimSpace(update.SampledAt))
	currentSampledAt := ""
	if current != nil {
		currentSampledAt = strings.TrimSpace(current.SampledAt)
	}
	if currentSampledAt == "" {
		currentSampledAt = strings.TrimSpace(currentFallbackSampledAt)
	}
	if sampledAt, err := time.Parse(time.RFC3339, currentSampledAt); err == nil &&
		(updateSampledAtErr != nil || !updateSampledAt.After(sampledAt)) {
		return current, false
	}
	if current != nil && update.ResetAt != nil && current.ResetAt != nil && *update.ResetAt != *current.ResetAt &&
		updateSampledAtErr == nil && !time.Unix(*current.ResetAt, 0).After(updateSampledAt) {
		// 这个槽位已自然进入新周期，只丢它自己的旧利用率，不能清掉其他周期窗口。
		current = nil
	}
	merged := &anthropicauth.PassiveUsageWindow{}
	if current != nil {
		*merged = *current
	}
	if update.Utilization != nil {
		value := *update.Utilization
		merged.Utilization = &value
	}
	if update.ResetAt != nil {
		value := *update.ResetAt
		merged.ResetAt = &value
	}
	if update.Utilization != nil {
		merged.SampledAt = strings.TrimSpace(update.SampledAt)
		merged.UtilizationStale = false
	} else {
		// reset-only 样本可以更新边界，但没有资格把保留的旧利用率伪装成新样本。
		merged.SampledAt = ""
		merged.UtilizationStale = true
	}
	return merged, true
}

func migrateAnthropicPassiveWindowSampleTimes(usage *anthropicauth.PassiveUsage) {
	if usage == nil {
		return
	}
	fallback := strings.TrimSpace(usage.SampledAt)
	if fallback == "" {
		return
	}
	for _, window := range []*anthropicauth.PassiveUsageWindow{
		usage.FiveHour, usage.SevenDay, usage.SevenDayOverageIncluded,
	} {
		if window != nil && !window.UtilizationStale && strings.TrimSpace(window.SampledAt) == "" {
			window.SampledAt = fallback
		}
	}
}
