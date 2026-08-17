package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/storage"
	"ccLoad/internal/xaiauth"

	"golang.org/x/sync/singleflight"
)

type xaiCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*xaiauth.Credential
	refreshes        singleflight.Group
	refreshTracker   *oauthCredentialRefreshTracker
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
	return m.credentialForRejectedAccessToken(ctx, cfg, forceRefresh, "")
}

func (m *xaiCredentialManager) credentialAfterUnauthorized(
	ctx context.Context,
	cfg *model.Config,
	rejectedAccessToken string,
) (*xaiauth.Credential, error) {
	if rejectedAccessToken == "" {
		return nil, errors.New("xAI rejected access token is required")
	}
	return m.credentialForRejectedAccessToken(ctx, cfg, true, rejectedAccessToken)
}

func (m *xaiCredentialManager) credentialForRejectedAccessToken(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
	rejectedAccessToken string,
) (*xaiauth.Credential, error) {
	if m == nil || m.store == nil || cfg == nil || !cfg.UsesXAIOAuth() {
		return nil, errors.New("xAI credential manager is unavailable")
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
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
			return nil, fmt.Errorf("reload xAI credential before refresh: %w", getErr)
		}
		current, parseErr := xaiauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, fmt.Errorf("parse xAI credential for channel %d: %w", currentCfg.ID, parseErr)
		}
		// A refresh flight is bound to one access token. If the persisted token
		// changed, consume that CAS winner; never refresh it under the old key.
		if current.AccessToken != forcedAccessToken {
			m.cache(currentCfg.ID, current)
			return oauthCredentialRefreshRedirect{}, nil
		}
		client := http.DefaultClient
		if m.clientFor != nil {
			client = m.clientFor(currentCfg)
		}
		refreshed, refreshErr := xaiauth.NewService(client).Refresh(refreshCtx, current)
		if refreshErr != nil {
			winnerCfg, winnerErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
			if winnerErr == nil && winnerCfg.OAuthCredential != currentCfg.OAuthCredential && winnerCfg.UsesXAIOAuth() {
				winner, parseWinnerErr := xaiauth.ParseCredential([]byte(winnerCfg.OAuthCredential))
				if parseWinnerErr == nil &&
					(winner.AccessToken != current.AccessToken || winner.RefreshToken != current.RefreshToken) {
					m.cache(winnerCfg.ID, winner)
					return cloneXAICredential(winner), nil
				}
			}
			return nil, fmt.Errorf("refresh xAI credential for channel %d: %w", currentCfg.ID, refreshErr)
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
			return cloneXAICredential(credential), ctx.Err()
		}
	}
	if result.Err != nil {
		return cloneXAICredential(credential), result.Err
	}
	if _, redirected := result.Val.(oauthCredentialRefreshRedirect); redirected {
		winner, winnerErr := m.cachedOrParse(cfg)
		if winnerErr != nil {
			return nil, winnerErr
		}
		if rejectedAccessToken != "" {
			return cloneXAICredential(winner), nil
		}
		return m.credentialForRejectedAccessToken(ctx, cfg, false, "")
	}
	return result.Val.(*xaiauth.Credential), nil
}

func (m *xaiCredentialManager) persistRefreshResult(
	ctx context.Context,
	cfg *model.Config,
	refreshedFrom *xaiauth.Credential,
	refreshed *xaiauth.Credential,
) (*xaiauth.Credential, error) {
	currentCfg := cfg
	current := refreshedFrom
	for {
		if current.AccessToken != refreshedFrom.AccessToken || current.RefreshToken != refreshedFrom.RefreshToken {
			m.cache(currentCfg.ID, current)
			return cloneXAICredential(current), nil
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
			ctx, currentCfg.ID, model.AuthTypeXAIOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return nil, err
		}
		if updated {
			m.cache(currentCfg.ID, merged)
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			return cloneXAICredential(merged), nil
		}
		currentCfg, err = m.store.GetConfig(ctx, currentCfg.ID)
		if err != nil {
			return nil, fmt.Errorf("reload xAI credential after concurrent refresh: %w", err)
		}
		if !currentCfg.UsesXAIOAuth() {
			return nil, errors.New("xAI credential changed provider during refresh persistence")
		}
		current, err = xaiauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return nil, fmt.Errorf("parse xAI credential after concurrent refresh: %w", err)
		}
	}
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
	clone.QuotaCostUsage = oauthcost.Clone(credential.QuotaCostUsage)
	return &clone
}
