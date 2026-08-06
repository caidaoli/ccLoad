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

const antigravityCredentialRefreshLead = 5 * time.Minute

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
	if m == nil || m.service == nil || m.store == nil || cfg == nil || !cfg.UsesAntigravityOAuth() {
		return nil, errors.New("credential manager: Antigravity is unavailable")
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	needsRefresh, err := credential.NeedsRefresh(m.now(), antigravityCredentialRefreshLead)
	if err != nil {
		return nil, err
	}
	if !forceRefresh && !needsRefresh && credential.ProjectID != "" {
		return cloneAntigravityCredential(credential), nil
	}

	value, err, _ := m.refreshes.Do(fmt.Sprintf("channel:%d", cfg.ID), func() (any, error) {
		current, currentErr := m.cachedOrParse(cfg)
		if currentErr != nil {
			return nil, currentErr
		}
		refreshNeeded, refreshErr := current.NeedsRefresh(m.now(), antigravityCredentialRefreshLead)
		if refreshErr != nil {
			return nil, refreshErr
		}

		service := *m.service
		if m.clientFor != nil {
			service.Client = m.clientFor(cfg)
		}
		refreshCtx := context.Background()
		if ctx != nil {
			refreshCtx = context.WithoutCancel(ctx)
		}
		merged := current
		if forceRefresh || refreshNeeded {
			refreshed, err := service.Refresh(refreshCtx, current.RefreshToken)
			if err != nil {
				return nil, fmt.Errorf("refresh Antigravity credential for channel %d: %w", cfg.ID, err)
			}
			merged, err = current.MergeRefresh(refreshed)
			if err != nil {
				return nil, err
			}
		}
		if merged.ProjectID == "" || merged.Email == "" {
			completed, err := service.CompleteCredential(refreshCtx, merged)
			if err != nil {
				return nil, fmt.Errorf("complete Antigravity credential for channel %d: %w", cfg.ID, err)
			}
			merged = completed
		}
		payload, err := merged.JSON()
		if err != nil {
			return nil, err
		}
		if err := m.store.UpdateOAuthCredential(refreshCtx, cfg.ID, payload); err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.entries[cfg.ID] = cloneAntigravityCredential(merged)
		m.mu.Unlock()
		if m.invalidateConfig != nil {
			m.invalidateConfig(cfg.ID)
		}
		return cloneAntigravityCredential(merged), nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*antigravityauth.Credential), nil
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
	return &clone
}
