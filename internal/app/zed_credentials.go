package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/zedauth"

	"golang.org/x/sync/singleflight"
)

const zedCredentialRefreshLead = time.Minute

type zedCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*zedauth.Credential
	refreshes        singleflight.Group
	refreshTracker   *oauthCredentialRefreshTracker
	service          *zedauth.Service
	store            storage.Store
	clientFor        func(*model.Config) *http.Client
	invalidateConfig func(int64)
	now              func() time.Time
}

func newZedCredentialManager(service *zedauth.Service, store storage.Store, clientFor func(*model.Config) *http.Client, invalidate func(int64)) *zedCredentialManager {
	return &zedCredentialManager{
		entries: make(map[int64]*zedauth.Credential), service: service, store: store,
		clientFor: clientFor, invalidateConfig: invalidate, now: time.Now,
	}
}

func (m *zedCredentialManager) credential(ctx context.Context, cfg *model.Config, forceRefresh bool) (*zedauth.Credential, error) {
	return m.credentialForRejectedAccessToken(ctx, cfg, forceRefresh, "")
}

func (m *zedCredentialManager) credentialAfterUnauthorized(ctx context.Context, cfg *model.Config, rejectedAccessToken string) (*zedauth.Credential, error) {
	if strings.TrimSpace(rejectedAccessToken) == "" {
		return nil, errors.New("zed rejected access token is required")
	}
	return m.credentialForRejectedAccessToken(ctx, cfg, true, strings.TrimSpace(rejectedAccessToken))
}

func (m *zedCredentialManager) credentialForRejectedAccessToken(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
	rejectedAccessToken string,
) (*zedauth.Credential, error) {
	if m == nil || m.service == nil || m.store == nil || cfg == nil || !cfg.UsesZedOAuth() {
		return nil, errors.New("zed credential manager is unavailable")
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	if !forceRefresh && !credential.NeedsRefresh(m.now(), zedCredentialRefreshLead) {
		return zedauth.CloneCredential(credential), nil
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
			return nil, fmt.Errorf("reload Zed credential before refresh: %w", getErr)
		}
		current, parseErr := zedauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, fmt.Errorf("parse Zed credential for channel %d: %w", currentCfg.ID, parseErr)
		}
		if current.AccessToken != forcedAccessToken {
			m.cache(currentCfg.ID, current)
			return oauthCredentialRefreshRedirect{}, nil
		}
		service := *m.service
		if m.clientFor != nil {
			service.Client = m.clientFor(currentCfg)
		}
		refreshed, refreshErr := service.MintLLMToken(refreshCtx, current)
		if refreshErr != nil {
			winnerCfg, winnerErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
			if winnerErr == nil && winnerCfg.OAuthCredential != currentCfg.OAuthCredential && winnerCfg.UsesZedOAuth() {
				winner, parseWinnerErr := zedauth.ParseCredential([]byte(winnerCfg.OAuthCredential))
				if parseWinnerErr == nil && winner.AccessToken != current.AccessToken {
					m.cache(winnerCfg.ID, winner)
					return zedauth.CloneCredential(winner), nil
				}
			}
			return nil, newCodexCredentialRefreshError(currentCfg, fmt.Errorf("refresh Zed credential for channel %d: %w", currentCfg.ID, refreshErr))
		}
		return m.persistRefresh(refreshCtx, currentCfg, current, refreshed)
	})
	var result singleflight.Result
	if ctx == nil {
		result = <-resultCh
	} else {
		select {
		case result = <-resultCh:
		case <-ctx.Done():
			return zedauth.CloneCredential(credential), ctx.Err()
		}
	}
	if result.Err != nil {
		return zedauth.CloneCredential(credential), result.Err
	}
	if _, redirected := result.Val.(oauthCredentialRefreshRedirect); redirected {
		winner, winnerErr := m.cachedOrParse(cfg)
		if winnerErr != nil {
			return nil, winnerErr
		}
		if rejectedAccessToken != "" {
			return zedauth.CloneCredential(winner), nil
		}
		return m.credentialForRejectedAccessToken(ctx, cfg, false, "")
	}
	return result.Val.(*zedauth.Credential), nil
}

func (m *zedCredentialManager) persistRefresh(ctx context.Context, cfg *model.Config, refreshedFrom, refreshed *zedauth.Credential) (*zedauth.Credential, error) {
	currentCfg := cfg
	current := refreshedFrom
	for {
		if current.AccessToken != refreshedFrom.AccessToken {
			m.cache(currentCfg.ID, current)
			return zedauth.CloneCredential(current), nil
		}
		merged, err := current.MergeRefresh(refreshed)
		if err != nil {
			return nil, err
		}
		payload, err := merged.JSON()
		if err != nil {
			return nil, err
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(ctx, currentCfg.ID, model.AuthTypeZedOAuth, currentCfg.OAuthCredential, payload)
		if err != nil {
			return nil, err
		}
		if updated {
			m.cache(currentCfg.ID, merged)
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			return zedauth.CloneCredential(merged), nil
		}
		currentCfg, err = m.store.GetConfig(ctx, currentCfg.ID)
		if err != nil {
			return nil, fmt.Errorf("reload Zed credential after concurrent refresh: %w", err)
		}
		if !currentCfg.UsesZedOAuth() {
			return nil, errors.New("zed credential changed provider during refresh persistence")
		}
		current, err = zedauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return nil, fmt.Errorf("parse Zed credential after concurrent refresh: %w", err)
		}
	}
}

func (m *zedCredentialManager) cachedOrParse(cfg *model.Config) (*zedauth.Credential, error) {
	m.mu.RLock()
	credential := m.entries[cfg.ID]
	m.mu.RUnlock()
	if credential != nil {
		return zedauth.CloneCredential(credential), nil
	}
	parsed, err := zedauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse Zed credential for channel %d: %w", cfg.ID, err)
	}
	m.mu.Lock()
	if existing := m.entries[cfg.ID]; existing != nil {
		parsed = zedauth.CloneCredential(existing)
	} else {
		m.entries[cfg.ID] = zedauth.CloneCredential(parsed)
	}
	m.mu.Unlock()
	return parsed, nil
}

func (m *zedCredentialManager) cache(channelID int64, credential *zedauth.Credential) {
	m.mu.Lock()
	m.entries[channelID] = zedauth.CloneCredential(credential)
	m.mu.Unlock()
}

func (m *zedCredentialManager) invalidate(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	m.mu.Unlock()
}
