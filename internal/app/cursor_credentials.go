package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"golang.org/x/sync/singleflight"
)

// cursorCredentialManager owns the session token of every Cursor channel.
//
// CLI session JWTs last weeks. There is no documented refresh endpoint for
// this flow; forceRefresh re-mints the pair from a stored user API key, or
// returns the cached token when the channel was authorized in the browser.
type cursorCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*cursorauth.Credential
	refreshes        singleflight.Group
	refreshTracker   *oauthCredentialRefreshTracker
	store            storage.Store
	clientFor        func(*model.Config) *http.Client
	invalidateConfig func(int64)
	now              func() time.Time
}

func newCursorCredentialManager(
	store storage.Store,
	clientFor func(*model.Config) *http.Client,
	invalidate func(int64),
) *cursorCredentialManager {
	return &cursorCredentialManager{
		entries: make(map[int64]*cursorauth.Credential), store: store,
		clientFor: clientFor, invalidateConfig: invalidate, now: time.Now,
	}
}

func (m *cursorCredentialManager) credential(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
) (*cursorauth.Credential, error) {
	if m == nil || m.store == nil || cfg == nil || !cfg.UsesCursorOAuth() {
		return nil, errors.New("cursor credential manager is unavailable")
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	if !forceRefresh {
		return cloneCursorCredential(credential), nil
	}
	if credential.APIKey == "" {
		return cloneCursorCredential(credential), errors.New(
			"cursor session was rejected and cannot be re-minted without a stored API key",
		)
	}
	rejectedToken := credential.AccessToken
	resultCh := m.refreshes.DoChan(oauthCredentialRefreshSingleflightKey(cfg.ID, rejectedToken, true), func() (any, error) {
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
			return nil, fmt.Errorf("reload cursor credential before re-mint: %w", getErr)
		}
		current, parseErr := cursorauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, fmt.Errorf("parse cursor credential for channel %d: %w", currentCfg.ID, parseErr)
		}
		if current.AccessToken != rejectedToken {
			m.cache(currentCfg.ID, current)
			return oauthCredentialRefreshRedirect{}, nil
		}
		if current.APIKey == "" {
			return nil, errors.New("cursor credential has no API key to re-mint with")
		}
		client := http.DefaultClient
		if m.clientFor != nil {
			client = m.clientFor(currentCfg)
		}
		pair, exchangeErr := cursorauth.NewService(client).ExchangeAPIKey(refreshCtx, current.APIKey)
		if exchangeErr != nil {
			return nil, fmt.Errorf("re-mint cursor session for channel %d: %w", currentCfg.ID, exchangeErr)
		}
		resolved := &cursorauth.Credential{
			AccessToken: pair.AccessToken, RefreshToken: pair.RefreshToken,
			LastRefresh: m.now().UTC().Format(time.RFC3339),
		}
		return m.persistResolved(refreshCtx, currentCfg, current, resolved)
	})

	var result singleflight.Result
	if ctx == nil {
		result = <-resultCh
	} else {
		select {
		case result = <-resultCh:
		case <-ctx.Done():
			return cloneCursorCredential(credential), ctx.Err()
		}
	}
	if result.Err != nil {
		return cloneCursorCredential(credential), result.Err
	}
	if _, redirected := result.Val.(oauthCredentialRefreshRedirect); redirected {
		winner, winnerErr := m.cachedOrParse(cfg)
		if winnerErr != nil {
			return nil, winnerErr
		}
		return cloneCursorCredential(winner), nil
	}
	return result.Val.(*cursorauth.Credential), nil
}

func (m *cursorCredentialManager) persistResolved(
	ctx context.Context,
	cfg *model.Config,
	resolvedFrom *cursorauth.Credential,
	resolved *cursorauth.Credential,
) (*cursorauth.Credential, error) {
	currentCfg := cfg
	current := resolvedFrom
	for {
		if current.AccessToken != resolvedFrom.AccessToken {
			m.cache(currentCfg.ID, current)
			return cloneCursorCredential(current), nil
		}
		merged, err := current.MergeRefresh(resolved)
		if err != nil {
			return nil, err
		}
		payload, err := merged.JSON()
		if err != nil {
			return nil, err
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(
			ctx, currentCfg.ID, model.AuthTypeCursorOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return nil, err
		}
		if updated {
			m.cache(currentCfg.ID, merged)
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			return cloneCursorCredential(merged), nil
		}
		currentCfg, err = m.store.GetConfig(ctx, currentCfg.ID)
		if err != nil {
			return nil, fmt.Errorf("reload cursor credential after concurrent re-mint: %w", err)
		}
		if !currentCfg.UsesCursorOAuth() {
			return nil, errors.New("cursor credential changed provider during persistence")
		}
		current, err = cursorauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return nil, fmt.Errorf("parse cursor credential after concurrent re-mint: %w", err)
		}
	}
}

func (m *cursorCredentialManager) cache(channelID int64, credential *cursorauth.Credential) {
	m.mu.Lock()
	m.entries[channelID] = cloneCursorCredential(credential)
	m.mu.Unlock()
}

func (m *cursorCredentialManager) cachedOrParse(cfg *model.Config) (*cursorauth.Credential, error) {
	m.mu.RLock()
	credential := m.entries[cfg.ID]
	m.mu.RUnlock()
	if credential != nil {
		return cloneCursorCredential(credential), nil
	}
	parsed, err := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse cursor credential for channel %d: %w", cfg.ID, err)
	}
	m.mu.Lock()
	if existing := m.entries[cfg.ID]; existing != nil {
		parsed = cloneCursorCredential(existing)
	} else {
		m.entries[cfg.ID] = cloneCursorCredential(parsed)
	}
	m.mu.Unlock()
	return parsed, nil
}

func (m *cursorCredentialManager) invalidate(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	m.mu.Unlock()
}

func cloneCursorCredential(credential *cursorauth.Credential) *cursorauth.Credential {
	if credential == nil {
		return nil
	}
	clone := *credential
	clone.OAuthUsage = append([]byte(nil), credential.OAuthUsage...)
	return &clone
}
