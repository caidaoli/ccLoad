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
	refreshTracker   *oauthCredentialRefreshTracker
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
	return m.resolveCredential(ctx, cfg, forceRefresh, false, "")
}

func (m *antigravityCredentialManager) credentialAfterUnauthorized(ctx context.Context, cfg *model.Config, rejectedAccessToken string) (*antigravityauth.Credential, error) {
	if rejectedAccessToken == "" {
		return nil, errors.New("antigravity rejected access token is required")
	}
	return m.resolveCredential(ctx, cfg, true, false, rejectedAccessToken)
}

func (m *antigravityCredentialManager) credentialWithMetadata(ctx context.Context, cfg *model.Config) (*antigravityauth.Credential, error) {
	return m.resolveCredential(ctx, cfg, false, true, "")
}

func (m *antigravityCredentialManager) resolveCredential(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
	refreshMetadata bool,
	rejectedAccessToken string,
) (*antigravityauth.Credential, error) {
	if m == nil || m.service == nil || m.store == nil || cfg == nil || !cfg.UsesAntigravityOAuth() {
		return nil, errors.New("credential manager: Antigravity is unavailable")
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
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
	if rejectedAccessToken != "" {
		forcedAccessToken = rejectedAccessToken
	}
	tokenRefreshRequested := forceRefresh || needsRefresh

	resultCh := m.refreshes.DoChan(oauthCredentialRefreshSingleflightKey(cfg.ID, forcedAccessToken, tokenRefreshRequested), func() (any, error) {
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
			return nil, fmt.Errorf("reload Antigravity credential before refresh: %w", getErr)
		}
		current, parseErr := antigravityauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, fmt.Errorf("parse Antigravity credential for channel %d: %w", currentCfg.ID, parseErr)
		}
		if current.AccessToken != forcedAccessToken {
			m.cache(currentCfg.ID, current)
			return oauthCredentialRefreshRedirect{}, nil
		}
		refreshNeeded, refreshErr := current.NeedsRefresh(m.now(), antigravityauth.CredentialRefreshLead)
		if refreshErr != nil {
			return nil, refreshErr
		}
		if !tokenRefreshRequested && refreshNeeded {
			m.cache(currentCfg.ID, current)
			return oauthCredentialRefreshRedirect{}, nil
		}
		if !tokenRefreshRequested && !refreshMetadata && current.ProjectID != "" && current.Email != "" {
			m.cache(currentCfg.ID, current)
			return cloneAntigravityCredential(current), nil
		}

		service := *m.service
		if m.clientFor != nil {
			service.Client = m.clientFor(currentCfg)
		}
		merged := current
		tokenRefreshed := tokenRefreshRequested
		if tokenRefreshed {
			refreshed, err := service.Refresh(refreshCtx, current.RefreshToken)
			if err != nil {
				winnerCfg, winnerErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
				if winnerErr == nil && winnerCfg.OAuthCredential != currentCfg.OAuthCredential && winnerCfg.UsesAntigravityOAuth() {
					winner, parseWinnerErr := antigravityauth.ParseCredential([]byte(winnerCfg.OAuthCredential))
					if parseWinnerErr == nil &&
						(winner.AccessToken != current.AccessToken || winner.RefreshToken != current.RefreshToken) {
						m.cache(winnerCfg.ID, winner)
						return cloneAntigravityCredential(winner), nil
					}
				}
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
		return m.persistResolvedCredential(refreshCtx, currentCfg, current, merged)
	})
	var result singleflight.Result
	if ctx == nil {
		result = <-resultCh
	} else {
		select {
		case result = <-resultCh:
		case <-ctx.Done():
			return cloneAntigravityCredential(credential), ctx.Err()
		}
	}
	if result.Err != nil {
		return cloneAntigravityCredential(credential), result.Err
	}
	if _, redirected := result.Val.(oauthCredentialRefreshRedirect); redirected {
		winner, winnerErr := m.cachedOrParse(cfg)
		if winnerErr != nil {
			return nil, winnerErr
		}
		if rejectedAccessToken != "" {
			return cloneAntigravityCredential(winner), nil
		}
		return m.resolveCredential(ctx, cfg, false, refreshMetadata, "")
	}
	return result.Val.(*antigravityauth.Credential), nil
}

func (m *antigravityCredentialManager) persistResolvedCredential(
	ctx context.Context,
	cfg *model.Config,
	updatedFrom *antigravityauth.Credential,
	desired *antigravityauth.Credential,
) (*antigravityauth.Credential, error) {
	currentCfg := cfg
	current := updatedFrom
	for {
		if current.AccessToken != updatedFrom.AccessToken || current.RefreshToken != updatedFrom.RefreshToken {
			m.cache(currentCfg.ID, current)
			return cloneAntigravityCredential(current), nil
		}
		merged, err := current.MergeRefresh(desired)
		if err != nil {
			return nil, err
		}
		payload, err := merged.JSON()
		if err != nil {
			return nil, err
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(
			ctx, currentCfg.ID, model.AuthTypeAntigravityOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return nil, err
		}
		if updated {
			m.cache(currentCfg.ID, merged)
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			return cloneAntigravityCredential(merged), nil
		}
		currentCfg, err = m.store.GetConfig(ctx, currentCfg.ID)
		if err != nil {
			return nil, fmt.Errorf("reload Antigravity credential after concurrent update: %w", err)
		}
		if !currentCfg.UsesAntigravityOAuth() {
			return nil, errors.New("antigravity credential changed provider during update persistence")
		}
		current, err = antigravityauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return nil, fmt.Errorf("parse Antigravity credential after concurrent update: %w", err)
		}
	}
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
