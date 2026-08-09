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
	"ccLoad/internal/storage"

	"golang.org/x/sync/singleflight"
)

type anthropicCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*anthropicauth.Credential
	refreshes        singleflight.Group
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
	if m == nil || m.service == nil || m.store == nil || cfg == nil || !cfg.UsesAnthropicOAuth() {
		return nil, errors.New("anthropic credential manager is unavailable")
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
	forceRequested := forceRefresh

	value, err, _ := m.refreshes.Do(fmt.Sprintf("channel:%d", cfg.ID), func() (any, error) {
		refreshCtx := context.Background()
		if ctx != nil {
			refreshCtx = context.WithoutCancel(ctx)
		}
		currentCfg, getErr := m.store.GetConfig(refreshCtx, cfg.ID)
		if getErr != nil {
			return nil, fmt.Errorf("reload Anthropic credential before refresh: %w", getErr)
		}
		for attempt := 0; attempt < 2; attempt++ {
			current, parseErr := anthropicauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil {
				return nil, fmt.Errorf("parse Anthropic credential for channel %d: %w", currentCfg.ID, parseErr)
			}
			refreshNeeded, refreshErr := current.NeedsRefresh(m.now(), anthropicauth.CredentialRefreshLead)
			if refreshErr != nil {
				return nil, refreshErr
			}
			forceCurrent := forceRequested && current.AccessToken == forcedAccessToken
			if !forceCurrent && !refreshNeeded {
				m.cache(currentCfg.ID, current)
				return cloneAnthropicCredential(current), nil
			}

			service := *m.service
			if m.clientFor != nil {
				service.Client = m.clientFor(currentCfg)
			}
			refreshed, refreshErr := service.Refresh(refreshCtx, current.RefreshToken)
			if refreshErr != nil {
				winner, getErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
				if getErr == nil && winner.OAuthCredential != currentCfg.OAuthCredential && attempt == 0 {
					// Another instance may already have consumed Anthropic's one-time
					// refresh token. Re-read its CAS winner before surfacing invalid_grant.
					currentCfg = winner
					continue
				}
				return nil, fmt.Errorf("refresh Anthropic credential for channel %d: %w", currentCfg.ID, refreshErr)
			}
			return m.persistRefreshResult(refreshCtx, currentCfg, current, refreshed)
		}
		return nil, errors.New("anthropic credential refresh retry exhausted")
	})
	if err != nil {
		return nil, err
	}
	return value.(*anthropicauth.Credential), nil
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
		if current.PassiveUsage != nil && current.PassiveUsage.SampledAt != "" {
			currentSampledAt, currentErr := time.Parse(time.RFC3339, current.PassiveUsage.SampledAt)
			if currentErr == nil && updateSampledAtErr == nil && !updateSampledAt.After(currentSampledAt) {
				m.cache(currentCfg.ID, current)
				return false, nil
			}
		}
		updatedCredential := *current
		usage := anthropicauth.ClonePassiveUsage(current.PassiveUsage)
		if usage == nil {
			usage = &anthropicauth.PassiveUsage{}
		}
		if update.FiveHour != nil && update.FiveHour.ResetAt != nil && usage.FiveHour != nil &&
			usage.FiveHour.ResetAt != nil && *update.FiveHour.ResetAt != *usage.FiveHour.ResetAt {
			currentResetAt := time.Unix(*usage.FiveHour.ResetAt, 0)
			if updateSampledAtErr == nil && !currentResetAt.After(updateSampledAt) {
				usage = &anthropicauth.PassiveUsage{}
			}
		}
		if update.FiveHour != nil {
			usage.FiveHour = mergeAnthropicPassiveWindow(usage.FiveHour, update.FiveHour)
		}
		if update.SevenDay != nil {
			usage.SevenDay = mergeAnthropicPassiveWindow(usage.SevenDay, update.SevenDay)
		}
		if update.SevenDayOverageIncluded != nil {
			usage.SevenDayOverageIncluded = mergeAnthropicPassiveWindow(usage.SevenDayOverageIncluded, update.SevenDayOverageIncluded)
		}
		usage.SampledAt = strings.TrimSpace(update.SampledAt)
		updatedCredential.PassiveUsage = usage
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

func mergeAnthropicPassiveWindow(current, update *anthropicauth.PassiveUsageWindow) *anthropicauth.PassiveUsageWindow {
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
	return merged
}
