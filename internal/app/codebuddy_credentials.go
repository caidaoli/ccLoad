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

/**
 * @brief 加载 CodeBuddy 凭证，轮换令牌时保留最新额度并优先使用并发完成的重新授权。
 * @param ctx 等待共享刷新结果的请求上下文。
 * @param cfg 请求使用的渠道快照。
 * @param force 是否强制刷新，无需检查到期时间。
 * @param rejected 上游拒绝的访问令牌，空值表示使用渠道快照中的令牌。
 * @return 可用凭证；刷新失败时返回原快照供调用方判断是否还能使用，快照不可用时为 nil。
 * @return 凭证无效、刷新或保存失败、渠道认证类型变化或请求取消时返回错误。
 * @note 刷新归服务器生命周期管理，同渠道上针对同一被拒令牌的刷新合并。
 */
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
		for {
			if err := refreshCtx.Err(); err != nil {
				return nil, err
			}
			latest, latestErr := s.store.GetConfig(refreshCtx, cfg.ID)
			if latestErr != nil {
				return nil, latestErr
			}
			if !latest.UsesCodeBuddyOAuth() {
				return nil, errors.New("CodeBuddy channel authentication changed")
			}
			latestCredential, parseErr := codebuddyauth.ParseCredential([]byte(latest.OAuthCredential))
			if parseErr != nil {
				return nil, parseErr
			}
			// Usage writes can change the JSON while the upstream refresh consumes the old token.
			if latestCredential.AccessToken != current.AccessToken || latestCredential.RefreshToken != current.RefreshToken ||
				latestCredential.UID != current.UID || latestCredential.EnterpriseID != current.EnterpriseID ||
				latestCredential.Domain != current.Domain || latestCredential.Endpoint() != current.Endpoint() {
				return latestCredential, nil
			}
			if refreshErr != nil {
				return nil, refreshErr
			}
			refreshed.OAuthUsage, refreshed.Nickname = latestCredential.OAuthUsage, latestCredential.Nickname
			payload, encodeErr := refreshed.JSON()
			if encodeErr != nil {
				return nil, encodeErr
			}
			updated, persistErr := s.store.CompareAndSwapOAuthCredential(refreshCtx, cfg.ID, model.AuthTypeCodeBuddyOAuth, latest.OAuthCredential, payload)
			if persistErr != nil {
				return nil, persistErr
			}
			if !updated {
				continue
			}
			s.invalidateChannelRelatedCache(cfg.ID)
			s.InvalidateChannelListCache()
			return refreshed, nil
		}
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
