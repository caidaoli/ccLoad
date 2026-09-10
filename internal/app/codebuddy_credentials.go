package app

import (
	"context"
	"errors"
	"time"

	"ccLoad/internal/codebuddyauth"
	"ccLoad/internal/model"

	"golang.org/x/sync/singleflight"
)

// There is no second credential cache: channel snapshots already cache storage.
// Each refresh reloads storage and is bound to the token rejected by the caller.
type codeBuddyCredentialManager struct {
	server    *Server
	refreshes singleflight.Group
}

func (m *codeBuddyCredentialManager) credential(ctx context.Context, cfg *model.Config, force bool, rejected string) (*codebuddyauth.Credential, error) {
	if m == nil || m.server == nil || m.server.codeBuddyService == nil || m.server.oauthCredentialRefreshes == nil || cfg == nil || !cfg.UsesCodeBuddyOAuth() {
		return nil, errors.New("CodeBuddy credentials unavailable")
	}
	c, err := codebuddyauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, err
	}
	if !force && !c.NeedsRefresh(time.Now()) {
		return c, nil
	}
	if rejected == "" {
		rejected = c.AccessToken
	}
	s := m.server
	ch := m.refreshes.DoChan(oauthCredentialRefreshSingleflightKey(cfg.ID, rejected, true), func() (any, error) {
		refreshCtx, done, beginErr := s.oauthCredentialRefreshes.begin()
		if beginErr != nil {
			return nil, beginErr
		}
		defer done()
		currentCfg, getErr := s.store.GetConfig(refreshCtx, cfg.ID)
		if getErr != nil {
			return nil, getErr
		}
		if !currentCfg.UsesCodeBuddyOAuth() {
			return nil, errors.New("CodeBuddy channel authentication changed")
		}
		current, parseErr := codebuddyauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, parseErr
		}
		if current.AccessToken != rejected {
			return current, nil
		}
		service := *s.codeBuddyService
		service.Client = s.getClientForChannel(currentCfg)
		refreshed, refreshErr := service.Refresh(refreshCtx, current)
		// Reauthorization wins even when the old refresh request was rejected.
		latest, latestErr := s.store.GetConfig(refreshCtx, cfg.ID)
		if latestErr != nil {
			return nil, latestErr
		}
		if !latest.UsesCodeBuddyOAuth() {
			return nil, errors.New("CodeBuddy channel authentication changed")
		}
		if latest.OAuthCredential != currentCfg.OAuthCredential {
			return codebuddyauth.ParseCredential([]byte(latest.OAuthCredential))
		}
		if refreshErr != nil {
			return nil, refreshErr
		}
		payload, encodeErr := refreshed.JSON()
		if encodeErr != nil {
			return nil, encodeErr
		}
		updated, persistErr := s.store.CompareAndSwapOAuthCredential(refreshCtx, cfg.ID, model.AuthTypeCodeBuddyOAuth, currentCfg.OAuthCredential, payload)
		if persistErr != nil {
			return nil, persistErr
		}
		if !updated {
			winner, winnerErr := s.store.GetConfig(refreshCtx, cfg.ID)
			if winnerErr != nil {
				return nil, winnerErr
			}
			if !winner.UsesCodeBuddyOAuth() {
				return nil, errors.New("CodeBuddy channel authentication changed")
			}
			return codebuddyauth.ParseCredential([]byte(winner.OAuthCredential))
		}
		s.invalidateChannelRelatedCache(cfg.ID)
		s.InvalidateChannelListCache()
		return refreshed, nil
	})
	select {
	case <-ctx.Done():
		return c, ctx.Err()
	case result := <-ch:
		if result.Err != nil {
			return c, result.Err
		}
		copy := *result.Val.(*codebuddyauth.Credential)
		return &copy, nil
	}
}
