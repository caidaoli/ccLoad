package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
			merged, mergeErr := current.MergeRefresh(refreshed)
			if mergeErr != nil {
				return nil, mergeErr
			}
			payload, encodeErr := merged.JSON()
			if encodeErr != nil {
				return nil, encodeErr
			}
			updated, updateErr := m.store.CompareAndSwapOAuthCredential(
				refreshCtx, currentCfg.ID, model.AuthTypeAnthropicOAuth, currentCfg.OAuthCredential, payload,
			)
			if updateErr != nil {
				return nil, updateErr
			}
			if !updated {
				winner, getErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
				if getErr != nil {
					return nil, fmt.Errorf("reload Anthropic credential after concurrent refresh: %w", getErr)
				}
				if attempt == 1 {
					return nil, errors.New("anthropic credential changed during refresh retry")
				}
				currentCfg = winner
				continue
			}
			m.cache(currentCfg.ID, merged)
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			return cloneAnthropicCredential(merged), nil
		}
		return nil, errors.New("anthropic credential refresh retry exhausted")
	})
	if err != nil {
		return nil, err
	}
	return value.(*anthropicauth.Credential), nil
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

func cloneAnthropicCredential(credential *anthropicauth.Credential) *anthropicauth.Credential {
	if credential == nil {
		return nil
	}
	clone := *credential
	return &clone
}
