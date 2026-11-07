package app

import (
	"context"
	"errors"
	"time"

	"ccLoad/internal/accioauth"
	"ccLoad/internal/model"

	"golang.org/x/sync/singleflight"
)

// There is no second credential cache: channel snapshots already cache storage.
// Each refresh reloads storage and is bound to the token rejected by the caller.
type accioCredentialManager struct {
	server    *Server
	refreshes singleflight.Group
}

func (m *accioCredentialManager) credential(ctx context.Context, cfg *model.Config, force bool, rejected string) (*accioauth.Credential, error) {
	if m == nil || m.server == nil || m.server.accioService == nil || m.server.oauthCredentialRefreshes == nil || cfg == nil || !cfg.UsesAccioOAuth() {
		return nil, errors.New("accio credentials unavailable")
	}
	c, err := accioauth.ParseCredential([]byte(cfg.OAuthCredential))
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
		if !currentCfg.UsesAccioOAuth() {
			return nil, errors.New("accio channel authentication changed")
		}
		current, parseErr := accioauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, parseErr
		}
		if current.AccessToken != rejected {
			return current, nil
		}
		service := *s.accioService
		service.Client = s.getClientForChannel(currentCfg)
		refreshed, refreshErr := service.Refresh(refreshCtx, current)
		// Only identity/token changes indicate reauthorization. Usage
		// writes can change the JSON while this one-use refresh token rotates.
		for {
			if err := refreshCtx.Err(); err != nil {
				return nil, err
			}
			latest, latestErr := s.store.GetConfig(refreshCtx, cfg.ID)
			if latestErr != nil {
				return nil, latestErr
			}
			if !latest.UsesAccioOAuth() {
				return nil, errors.New("accio channel authentication changed")
			}
			latestCredential, parseErr := accioauth.ParseCredential([]byte(latest.OAuthCredential))
			if parseErr != nil {
				return nil, parseErr
			}
			if !sameAccioAuthentication(current, latestCredential) {
				return latestCredential, nil
			}
			if refreshErr != nil {
				return nil, newCodexCredentialRefreshError(latest, refreshErr)
			}
			// Keep the newest metadata and publish the rotated token pair together.
			merged := *latestCredential
			merged.AccessToken, merged.RefreshToken = refreshed.AccessToken, refreshed.RefreshToken
			if latestCredential.SGCookie == current.SGCookie {
				merged.SGCookie = refreshed.SGCookie
			}
			merged.ExpiresAt = refreshed.ExpiresAt
			if merged.UserID == "" {
				merged.UserID = refreshed.UserID
			}
			payload, encodeErr := merged.JSON()
			if encodeErr != nil {
				return nil, encodeErr
			}
			updated, persistErr := s.store.CompareAndSwapOAuthCredential(refreshCtx, cfg.ID, model.AuthTypeAccioOAuth, latest.OAuthCredential, payload)
			if persistErr != nil {
				return nil, persistErr
			}
			if !updated {
				continue
			}
			s.invalidateChannelRelatedCache(cfg.ID)
			s.InvalidateChannelListCache()
			return &merged, nil
		}
	})
	select {
	case <-ctx.Done():
		return c, ctx.Err()
	case result := <-ch:
		if result.Err != nil {
			return c, result.Err
		}
		copy := *result.Val.(*accioauth.Credential)
		return &copy, nil
	}
}

// sameAccioAuthentication deliberately excludes mutable account metadata.
func sameAccioAuthentication(a, b *accioauth.Credential) bool {
	return a.Type == b.Type && a.Region == b.Region && a.UserID == b.UserID &&
		a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken
}
