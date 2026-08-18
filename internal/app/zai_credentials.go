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
	"ccLoad/internal/zaiauth"

	"golang.org/x/sync/singleflight"
)

// zaiCredentialManager owns the Coding Plan key of every Z.ai channel.
//
// Coding Plan keys do not expire, so there is no scheduled refresh: the only
// re-resolution happens after the upstream rejects a key, and it replays the
// ZCode key derivation with the stored account access token.
type zaiCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*zaiauth.Credential
	refreshes        singleflight.Group
	refreshTracker   *oauthCredentialRefreshTracker
	store            storage.Store
	clientFor        func(*model.Config) *http.Client
	invalidateConfig func(int64)
	now              func() time.Time
}

func newZAICredentialManager(
	store storage.Store,
	clientFor func(*model.Config) *http.Client,
	invalidate func(int64),
) *zaiCredentialManager {
	return &zaiCredentialManager{
		entries: make(map[int64]*zaiauth.Credential), store: store,
		clientFor: clientFor, invalidateConfig: invalidate, now: time.Now,
	}
}

// credential returns the channel's Coding Plan credential. forceRefresh
// re-resolves the API key from the stored account access token.
func (m *zaiCredentialManager) credential(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
) (*zaiauth.Credential, error) {
	if m == nil || m.store == nil || cfg == nil || !cfg.UsesZAIOAuth() {
		return nil, errors.New("z.ai credential manager is unavailable")
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	if !forceRefresh {
		return cloneZAICredential(credential), nil
	}
	if credential.AccessToken == "" {
		return cloneZAICredential(credential), errors.New(
			"z.ai Coding Plan key was rejected and cannot be re-resolved without an account authorization",
		)
	}
	rejectedKey := credential.APIKey

	resultCh := m.refreshes.DoChan(oauthCredentialRefreshSingleflightKey(cfg.ID, rejectedKey, true), func() (any, error) {
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
			return nil, fmt.Errorf("reload z.ai credential before re-resolution: %w", getErr)
		}
		current, parseErr := zaiauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, fmt.Errorf("parse z.ai credential for channel %d: %w", currentCfg.ID, parseErr)
		}
		// One re-resolution flight is bound to one rejected key. If the stored
		// key already changed, consume that winner instead of minting another.
		if current.APIKey != rejectedKey {
			m.cache(currentCfg.ID, current)
			return oauthCredentialRefreshRedirect{}, nil
		}
		if current.AccessToken == "" {
			return nil, errors.New("z.ai credential has no account authorization to re-resolve with")
		}
		client := http.DefaultClient
		if m.clientFor != nil {
			client = m.clientFor(currentCfg)
		}
		apiKey, identity, resolveErr := zaiauth.NewService(client).ResolveCodingPlanAPIKey(refreshCtx, current.AccessToken)
		if resolveErr != nil {
			return nil, fmt.Errorf("re-resolve z.ai Coding Plan key for channel %d: %w", currentCfg.ID, resolveErr)
		}
		resolved := &zaiauth.Credential{
			APIKey: apiKey, UserID: identity.UserID, Email: identity.Email,
			LastRefresh: m.now().UTC().Format(time.RFC3339),
		}
		return m.persistResolvedKey(refreshCtx, currentCfg, current, resolved)
	})

	var result singleflight.Result
	if ctx == nil {
		result = <-resultCh
	} else {
		select {
		case result = <-resultCh:
		case <-ctx.Done():
			return cloneZAICredential(credential), ctx.Err()
		}
	}
	if result.Err != nil {
		return cloneZAICredential(credential), result.Err
	}
	if _, redirected := result.Val.(oauthCredentialRefreshRedirect); redirected {
		winner, winnerErr := m.cachedOrParse(cfg)
		if winnerErr != nil {
			return nil, winnerErr
		}
		return cloneZAICredential(winner), nil
	}
	return result.Val.(*zaiauth.Credential), nil
}

func (m *zaiCredentialManager) persistResolvedKey(
	ctx context.Context,
	cfg *model.Config,
	resolvedFrom *zaiauth.Credential,
	resolved *zaiauth.Credential,
) (*zaiauth.Credential, error) {
	currentCfg := cfg
	current := resolvedFrom
	for {
		if current.APIKey != resolvedFrom.APIKey {
			m.cache(currentCfg.ID, current)
			return cloneZAICredential(current), nil
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
			ctx, currentCfg.ID, model.AuthTypeZAIOAuth, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return nil, err
		}
		if updated {
			m.cache(currentCfg.ID, merged)
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			return cloneZAICredential(merged), nil
		}
		currentCfg, err = m.store.GetConfig(ctx, currentCfg.ID)
		if err != nil {
			return nil, fmt.Errorf("reload z.ai credential after concurrent re-resolution: %w", err)
		}
		if !currentCfg.UsesZAIOAuth() {
			return nil, errors.New("z.ai credential changed provider during persistence")
		}
		current, err = zaiauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return nil, fmt.Errorf("parse z.ai credential after concurrent re-resolution: %w", err)
		}
	}
}

func (m *zaiCredentialManager) cache(channelID int64, credential *zaiauth.Credential) {
	m.mu.Lock()
	m.entries[channelID] = cloneZAICredential(credential)
	m.mu.Unlock()
}

func (m *zaiCredentialManager) cachedOrParse(cfg *model.Config) (*zaiauth.Credential, error) {
	m.mu.RLock()
	credential := m.entries[cfg.ID]
	m.mu.RUnlock()
	if credential != nil {
		return cloneZAICredential(credential), nil
	}
	parsed, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse z.ai credential for channel %d: %w", cfg.ID, err)
	}
	m.mu.Lock()
	if existing := m.entries[cfg.ID]; existing != nil {
		parsed = cloneZAICredential(existing)
	} else {
		m.entries[cfg.ID] = cloneZAICredential(parsed)
	}
	m.mu.Unlock()
	return parsed, nil
}

func (m *zaiCredentialManager) invalidate(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	m.mu.Unlock()
}

func cloneZAICredential(credential *zaiauth.Credential) *zaiauth.Credential {
	if credential == nil {
		return nil
	}
	clone := *credential
	clone.OAuthUsage = append([]byte(nil), credential.OAuthUsage...)
	return &clone
}
