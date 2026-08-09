package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"golang.org/x/sync/singleflight"
)

type antigravityCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*antigravityauth.Credential
	refreshes        singleflight.Group
	service          *antigravityauth.Service
	store            storage.Store
	clientFor        func(*model.Config) *http.Client
	invalidateConfig func(int64)
	now              func() time.Time
}

func newAntigravityCredentialManager(
	service *antigravityauth.Service,
	store storage.Store,
	clientFor func(*model.Config) *http.Client,
	invalidate func(int64),
) *antigravityCredentialManager {
	return &antigravityCredentialManager{
		entries: make(map[int64]*antigravityauth.Credential), service: service,
		store: store, clientFor: clientFor, invalidateConfig: invalidate, now: time.Now,
	}
}

func (m *antigravityCredentialManager) credential(ctx context.Context, cfg *model.Config, forceRefresh bool) (*antigravityauth.Credential, error) {
	return m.resolveCredential(ctx, cfg, forceRefresh, false)
}

func (m *antigravityCredentialManager) credentialWithMetadata(ctx context.Context, cfg *model.Config) (*antigravityauth.Credential, error) {
	return m.resolveCredential(ctx, cfg, false, true)
}

func (m *antigravityCredentialManager) resolveCredential(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
	refreshMetadata bool,
) (*antigravityauth.Credential, error) {
	if m == nil || m.service == nil || m.store == nil || cfg == nil || !cfg.UsesAntigravityOAuth() {
		return nil, errors.New("credential manager: Antigravity is unavailable")
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	needsRefresh, err := credential.NeedsRefresh(m.now(), antigravityauth.CredentialRefreshLead)
	if err != nil {
		return nil, err
	}
	if !forceRefresh && !needsRefresh && !refreshMetadata && credential.ProjectID != "" && credential.Email != "" {
		return cloneAntigravityCredential(credential), nil
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
			return nil, fmt.Errorf("reload Antigravity credential before refresh: %w", getErr)
		}
		for attempt := 0; attempt < 2; attempt++ {
			current, parseErr := antigravityauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil {
				return nil, fmt.Errorf("parse Antigravity credential for channel %d: %w", currentCfg.ID, parseErr)
			}
			refreshNeeded, refreshErr := current.NeedsRefresh(m.now(), antigravityauth.CredentialRefreshLead)
			if refreshErr != nil {
				return nil, refreshErr
			}
			forceCurrent := forceRequested && current.AccessToken == forcedAccessToken
			if !forceCurrent && !refreshNeeded && !refreshMetadata && current.ProjectID != "" && current.Email != "" {
				m.cache(currentCfg.ID, current)
				return cloneAntigravityCredential(current), nil
			}

			service := *m.service
			if m.clientFor != nil {
				service.Client = m.clientFor(currentCfg)
			}
			merged := current
			tokenRefreshed := forceCurrent || refreshNeeded
			if tokenRefreshed {
				refreshed, err := service.Refresh(refreshCtx, current.RefreshToken)
				if err != nil {
					return nil, fmt.Errorf("refresh Antigravity credential for channel %d: %w", currentCfg.ID, err)
				}
				merged, err = current.MergeRefresh(refreshed)
				if err != nil {
					return nil, err
				}
			}
			if refreshMetadata || tokenRefreshed || merged.ProjectID == "" || merged.Email == "" {
				completed, err := service.CompleteCredential(refreshCtx, merged)
				if err != nil {
					return nil, fmt.Errorf("complete Antigravity credential for channel %d: %w", currentCfg.ID, err)
				}
				merged = completed
			}
			payload, err := merged.JSON()
			if err != nil {
				return nil, err
			}
			updated, err := m.store.CompareAndSwapOAuthCredential(
				refreshCtx, currentCfg.ID, model.AuthTypeAntigravityOAuth, currentCfg.OAuthCredential, payload,
			)
			if err != nil {
				return nil, err
			}
			if !updated {
				winner, getErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
				if getErr != nil {
					return nil, fmt.Errorf("reload Antigravity credential after concurrent refresh: %w", getErr)
				}
				if attempt == 1 {
					return nil, errors.New("antigravity credential changed during refresh retry")
				}
				currentCfg = winner
				refreshMetadata = false
				continue
			}
			m.cache(currentCfg.ID, merged)
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			return cloneAntigravityCredential(merged), nil
		}
		return nil, errors.New("antigravity credential refresh retry exhausted")
	})
	if err != nil {
		return nil, err
	}
	return value.(*antigravityauth.Credential), nil
}

func (m *antigravityCredentialManager) cache(channelID int64, credential *antigravityauth.Credential) {
	m.mu.Lock()
	m.entries[channelID] = cloneAntigravityCredential(credential)
	m.mu.Unlock()
}

func (m *antigravityCredentialManager) cachedOrParse(cfg *model.Config) (*antigravityauth.Credential, error) {
	m.mu.RLock()
	credential := m.entries[cfg.ID]
	m.mu.RUnlock()
	if credential != nil {
		return cloneAntigravityCredential(credential), nil
	}
	parsed, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse Antigravity credential for channel %d: %w", cfg.ID, err)
	}
	m.mu.Lock()
	if existing := m.entries[cfg.ID]; existing != nil {
		parsed = cloneAntigravityCredential(existing)
	} else {
		m.entries[cfg.ID] = cloneAntigravityCredential(parsed)
	}
	m.mu.Unlock()
	return parsed, nil
}

func (m *antigravityCredentialManager) invalidate(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	m.mu.Unlock()
}

func cloneAntigravityCredential(credential *antigravityauth.Credential) *antigravityauth.Credential {
	if credential == nil {
		return nil
	}
	clone := *credential
	if credential.PaidTier != nil {
		paidTier := *credential.PaidTier
		clone.PaidTier = &paidTier
	}
	clone.OAuthUsage = append([]byte(nil), credential.OAuthUsage...)
	return &clone
}
