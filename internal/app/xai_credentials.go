package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/xaiauth"

	"golang.org/x/sync/singleflight"
)

type xaiCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*xaiauth.Credential
	refreshes        singleflight.Group
	store            storage.Store
	clientFor        func(*model.Config) *http.Client
	invalidateConfig func(int64)
	now              func() time.Time
}

func newXAICredentialManager(
	store storage.Store,
	clientFor func(*model.Config) *http.Client,
	invalidate func(int64),
) *xaiCredentialManager {
	return &xaiCredentialManager{
		entries: make(map[int64]*xaiauth.Credential), store: store,
		clientFor: clientFor, invalidateConfig: invalidate, now: time.Now,
	}
}

func (m *xaiCredentialManager) credential(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
) (*xaiauth.Credential, error) {
	if m == nil || m.store == nil || cfg == nil || !cfg.UsesXAIOAuth() {
		return nil, errors.New("xAI credential manager is unavailable")
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	needsRefresh, err := credential.NeedsRefresh(m.now(), xaiauth.RefreshLead)
	if err != nil {
		return nil, err
	}
	if !forceRefresh && !needsRefresh {
		return cloneXAICredential(credential), nil
	}

	value, err, _ := m.refreshes.Do(fmt.Sprintf("channel:%d", cfg.ID), func() (any, error) {
		refreshCtx := context.Background()
		if ctx != nil {
			refreshCtx = context.WithoutCancel(ctx)
		}
		currentCfg, getErr := m.store.GetConfig(refreshCtx, cfg.ID)
		if getErr != nil {
			return nil, fmt.Errorf("reload xAI credential before refresh: %w", getErr)
		}
		for attempt := 0; attempt < 2; attempt++ {
			current, parseErr := xaiauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil {
				return nil, fmt.Errorf("parse xAI credential for channel %d: %w", currentCfg.ID, parseErr)
			}
			// A rejected request only proves the access token used by that request
			// is stale. If another refresher already persisted a different token,
			// consume that CAS winner; a stale cfg snapshot is not sufficient proof.
			if attempt == 0 && forceRefresh && current.AccessToken != credential.AccessToken {
				forceRefresh = false
			}
			refreshNeeded, refreshErr := current.NeedsRefresh(m.now(), xaiauth.RefreshLead)
			if refreshErr != nil {
				return nil, refreshErr
			}
			if (attempt > 0 || !forceRefresh) && !refreshNeeded {
				m.cache(currentCfg.ID, current)
				return cloneXAICredential(current), nil
			}

			client := http.DefaultClient
			if m.clientFor != nil {
				client = m.clientFor(currentCfg)
			}
			refreshed, refreshErr := xaiauth.NewService(client).Refresh(refreshCtx, current)
			if refreshErr != nil {
				return nil, fmt.Errorf("refresh xAI credential for channel %d: %w", currentCfg.ID, refreshErr)
			}
			payload, encodeErr := refreshed.JSON()
			if encodeErr != nil {
				return nil, encodeErr
			}
			updated, updateErr := m.store.CompareAndSwapOAuthCredential(
				refreshCtx, currentCfg.ID, model.AuthTypeXAIOAuth, currentCfg.OAuthCredential, payload,
			)
			if updateErr != nil {
				return nil, updateErr
			}
			if !updated {
				winner, getErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
				if getErr != nil {
					return nil, fmt.Errorf("reload xAI credential after concurrent refresh: %w", getErr)
				}
				if attempt == 1 {
					return nil, errors.New("xAI credential changed during refresh retry")
				}
				currentCfg = winner
				forceRefresh = false
				continue
			}

			m.cache(currentCfg.ID, refreshed)
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			return cloneXAICredential(refreshed), nil
		}
		return nil, errors.New("xAI credential refresh retry exhausted")
	})
	if err != nil {
		return nil, err
	}
	return value.(*xaiauth.Credential), nil
}

func (m *xaiCredentialManager) cache(channelID int64, credential *xaiauth.Credential) {
	m.mu.Lock()
	m.entries[channelID] = cloneXAICredential(credential)
	m.mu.Unlock()
}

func (m *xaiCredentialManager) cachedOrParse(cfg *model.Config) (*xaiauth.Credential, error) {
	m.mu.RLock()
	credential := m.entries[cfg.ID]
	m.mu.RUnlock()
	if credential != nil {
		return cloneXAICredential(credential), nil
	}
	parsed, err := xaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse xAI credential for channel %d: %w", cfg.ID, err)
	}
	m.mu.Lock()
	if existing := m.entries[cfg.ID]; existing != nil {
		parsed = cloneXAICredential(existing)
	} else {
		m.entries[cfg.ID] = cloneXAICredential(parsed)
	}
	m.mu.Unlock()
	return parsed, nil
}

func (m *xaiCredentialManager) invalidate(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	m.mu.Unlock()
}

func cloneXAICredential(credential *xaiauth.Credential) *xaiauth.Credential {
	if credential == nil {
		return nil
	}
	clone := *credential
	clone.OAuthUsage = append([]byte(nil), credential.OAuthUsage...)
	return &clone
}
