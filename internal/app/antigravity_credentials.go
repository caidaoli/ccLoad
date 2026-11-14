package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/storage"

	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

/**
 * @brief 停用被 Antigravity 明确以 TOS_VIOLATION 拒绝的账号对应渠道。
 * @param ctx 本次请求上下文；已取消时使用既有冷却写入预算持久化停用。
 * @param cfg 实际发送请求使用的 Antigravity 凭证快照。
 * @param res HTTP 或 SSE 错误结果；调用方保证非 nil。
 * @return 已处理 TOS 拒绝时返回 true，包括账号已替换或渠道已停用；非 TOS 或持久化失败返回 false。
 * @note 同一 refresh token 下的 access token 刷新不影响停用；更换账号后的旧结果不改动新账号。成功停用后失效凭证、渠道与冷却缓存。
 */
func (s *Server) disableAntigravityTOSCredential(ctx context.Context, cfg *model.Config, res *fwResult) bool {
	if !cfg.UsesAntigravityOAuth() {
		return false
	}
	body := res.SSEErrorEvent
	if len(body) == 0 {
		status := res.UpstreamStatus
		if status == 0 {
			status = res.Status
		}
		if status != http.StatusForbidden {
			return false
		}
		body = res.Body
	}
	var payload struct {
		Error struct {
			Code    int    `json:"code"`   ///< Google RPC 错误对应的 HTTP 状态。
			Status  string `json:"status"` ///< Google RPC 标准状态名。
			Details []struct {
				Type   string `json:"@type"`  ///< Google RPC 详情的类型标识。
				Reason string `json:"reason"` ///< ErrorInfo 的结构化拒绝原因。
			} `json:"details"` ///< 供精确识别服务级账号停用的错误详情。
		} `json:"error"` ///< 上游原始错误，普通消息文本不参与识别。
	}
	if json.Unmarshal(body, &payload) != nil || payload.Error.Code != http.StatusForbidden || payload.Error.Status != "PERMISSION_DENIED" {
		return false
	}
	matched := false
	for _, detail := range payload.Error.Details {
		if detail.Type == "type.googleapis.com/google.rpc.ErrorInfo" && detail.Reason == "TOS_VIOLATION" {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}

	disableCtx, cancel := cooldownWriteContext(ctx)
	defer cancel()
	expected := cfg.OAuthCredential
	refreshToken := gjson.Get(expected, "refresh_token").String()
	for {
		disabled, err := s.store.DisableOAuthChannelIfCredentialMatches(disableCtx, cfg.ID, model.AuthTypeAntigravityOAuth, expected)
		if err != nil {
			log.Printf("[ERROR] 停用 Antigravity TOS 违规渠道失败: channel_id=%d err=%v", cfg.ID, err)
			return false
		}
		if disabled {
			s.antigravityCredentials.invalidate(cfg.ID)
			s.invalidateChannelRelatedCache(cfg.ID)
			s.InvalidateChannelListCache()
			log.Printf("[DISABLED] Antigravity 账号被上游以 TOS_VIOLATION 停用: channel_id=%d", cfg.ID)
			return true
		}
		current, err := s.store.GetConfig(disableCtx, cfg.ID)
		if err != nil {
			log.Printf("[ERROR] 核对 Antigravity TOS 违规账号失败: channel_id=%d err=%v", cfg.ID, err)
			return false
		}
		if !current.Enabled || !current.UsesAntigravityOAuth() ||
			gjson.Get(current.OAuthCredential, "refresh_token").String() != refreshToken {
			return true
		}
		expected = current.OAuthCredential
	}
}

type antigravityCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*antigravityauth.Credential
	refreshes        singleflight.Group
	metadata         singleflight.Group
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
	credential, err := m.resolveTokenCredential(ctx, cfg, forceRefresh, rejectedAccessToken)
	if err != nil || credential == nil {
		return credential, err
	}
	if !refreshMetadata && credential.ProjectID != "" {
		return credential, nil
	}
	return m.completeStoredMetadata(ctx, cfg, credential, refreshMetadata)
}

func (m *antigravityCredentialManager) resolveTokenCredential(ctx context.Context, cfg *model.Config, forceRefresh bool, rejectedAccessToken string) (*antigravityauth.Credential, error) {
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
	if !forceRefresh && !needsRefresh {
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
		if !tokenRefreshRequested {
			m.cache(currentCfg.ID, current)
			return cloneAntigravityCredential(current), nil
		}

		service := *m.service
		if m.clientFor != nil {
			service.Client = m.clientFor(currentCfg)
		}
		if tokenRefreshRequested {
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
				return nil, newCodexCredentialRefreshError(currentCfg, fmt.Errorf("refresh Antigravity credential for channel %d: %w", currentCfg.ID, err))
			}
			return m.persistResolvedCredential(refreshCtx, currentCfg, current, refreshed)
		}
		return cloneAntigravityCredential(current), nil
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
		return m.resolveTokenCredential(ctx, cfg, false, "")
	}
	return result.Val.(*antigravityauth.Credential), nil
}

func (m *antigravityCredentialManager) completeStoredMetadata(ctx context.Context, cfg *model.Config, credential *antigravityauth.Credential, subscription bool) (*antigravityauth.Credential, error) {
	key := fmt.Sprintf("%s:%t", oauthCredentialRefreshSingleflightKey(cfg.ID, credential.AccessToken, false), subscription)
	ch := m.metadata.DoChan(key, func() (any, error) {
		queryCtx := context.Background()
		if ctx != nil {
			queryCtx = context.WithoutCancel(ctx)
		}
		if m.refreshTracker != nil {
			tracked, done, err := m.refreshTracker.begin()
			if err != nil {
				return nil, err
			}
			defer done()
			queryCtx = tracked
		}
		timeout := 30 * time.Second
		if subscription {
			timeout = 5 * time.Second
		}
		queryCtx, cancel := context.WithTimeout(queryCtx, timeout)
		defer cancel()
		current := cloneAntigravityCredential(credential)
		for attempt := range 2 {
			service := *m.service
			if m.clientFor != nil {
				service.Client = m.clientFor(cfg)
			}
			project := current.ProjectID
			var tier *antigravityauth.PaidTier
			var credits *antigravityauth.Credits
			var err error
			if project == "" {
				project, err = service.FetchProjectID(queryCtx, current.AccessToken)
				if err == nil {
					current, err = m.updateStoredMetadata(queryCtx, cfg.ID, current, func(latest *antigravityauth.Credential) { latest.ProjectID = project })
					if err != nil {
						return credential, err
					}
					project = current.ProjectID
				}
			}
			if err == nil && subscription {
				tier, credits, err = service.FetchSubscription(queryCtx, current.AccessToken)
			}
			if antigravityauth.AccessTokenRejected(err) && attempt == 0 {
				current, err = m.resolveTokenCredential(queryCtx, cfg, true, current.AccessToken)
				if err != nil {
					return current, err
				}
				continue
			}
			if err != nil {
				if current.ProjectID == "" {
					err = fmt.Errorf("%w: %w", antigravityauth.ErrProjectUnavailable, err)
				}
				return current, err
			}
			return m.updateStoredMetadata(queryCtx, cfg.ID, current, func(latest *antigravityauth.Credential) {
				latest.ProjectID = project
				if subscription {
					if latest.Credits == nil || (!latest.Credits.SampledAt.After(credits.SampledAt) && latest.Credits.UnavailableAt <= credits.SampledAt.UnixMilli()) {
						latest.PaidTier = tier
						latest.Credits = credits.Clone()
					}
				}
			})
		}
		return current, errors.New("antigravity metadata retry exhausted")
	})
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case result := <-ch:
		if result.Val != nil {
			return cloneAntigravityCredential(result.Val.(*antigravityauth.Credential)), result.Err
		}
		return credential, result.Err
	case <-ctx.Done():
		return credential, ctx.Err()
	}
}

// Metadata patches never replay token fields or cost snapshots over a concurrent write.
func (m *antigravityCredentialManager) updateStoredMetadata(ctx context.Context, channelID int64, source *antigravityauth.Credential, update func(*antigravityauth.Credential)) (*antigravityauth.Credential, error) {
	for {
		cfg, err := m.store.GetConfig(ctx, channelID)
		if err != nil {
			return nil, err
		}
		if !cfg.UsesAntigravityOAuth() {
			return nil, errors.New("antigravity credential changed provider")
		}
		current, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil, err
		}
		if current.AccessToken != source.AccessToken || current.RefreshToken != source.RefreshToken {
			m.cache(channelID, current)
			return current, nil
		}
		update(current)
		payload, err := current.JSON()
		if err != nil {
			return nil, err
		}
		updated, err := m.store.CompareAndSwapOAuthCredential(ctx, channelID, model.AuthTypeAntigravityOAuth, cfg.OAuthCredential, payload)
		if err != nil {
			return nil, err
		}
		if updated {
			if m.invalidateConfig != nil {
				m.invalidateConfig(channelID)
			}
			m.cache(channelID, current)
			return current, nil
		}
	}
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
	clone.QuotaCostUsage = oauthcost.Clone(credential.QuotaCostUsage)
	clone.Credits = credential.Credits.Clone()
	clone.StandardQuota = maps.Clone(credential.StandardQuota)
	return &clone
}
