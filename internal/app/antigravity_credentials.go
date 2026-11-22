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
 * @brief 从 Antigravity 的 HTTP 或 SSE 错误中确认账号因 TOS 违规被拒绝。
 * @param cfg 实际发送请求使用的 Antigravity 凭证快照。
 * @param res HTTP 或 SSE 错误结果；调用方保证非 nil。
 * @return 仅当 403 PERMISSION_DENIED 包含 TOS_VIOLATION 的 ErrorInfo 时返回 true。
 */
func isAntigravityTOSViolation(cfg *model.Config, res *fwResult) bool {
	if !cfg.UsesAntigravityOAuth() {
		return false
	}
	if len(res.SSEErrorEvent) != 0 {
		return isAntigravityPermissionReason(http.StatusForbidden, res.SSEErrorEvent, "TOS_VIOLATION")
	}
	status := res.UpstreamStatus
	if status == 0 {
		status = res.Status
	}
	return isAntigravityPermissionReason(status, res.Body, "TOS_VIOLATION")
}

/**
 * @brief 识别 Antigravity 结构化权限拒绝。
 * @param status 实际 HTTP 或 SSE 错误状态。
 * @param body Google RPC 错误正文。
 * @param reason 要匹配的 ErrorInfo 原因。
 * @return 403 PERMISSION_DENIED 含匹配原因时为 true。
 */
func isAntigravityPermissionReason(status int, body []byte, reason string) bool {
	if status != http.StatusForbidden {
		return false
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
	for _, detail := range payload.Error.Details {
		if detail.Type == "type.googleapis.com/google.rpc.ErrorInfo" && detail.Reason == reason {
			return true
		}
	}
	return false
}

/**
 * @brief 停用被 Antigravity 明确以 TOS_VIOLATION 拒绝的账号对应渠道。
 * @param ctx 本次请求上下文；已取消时使用既有冷却写入预算持久化停用。
 * @param cfg 实际发送请求使用的 Antigravity 凭证快照。
 * @param res HTTP 或 SSE 错误结果；调用方保证非 nil。
 * @return 已处理 TOS 拒绝时返回 true，包括账号已替换或渠道已停用；非 TOS 或持久化失败返回 false。
 * @note 同一 refresh token 下的 access token 刷新不影响停用；更换账号后的旧结果不改动新账号。成功停用后失效凭证、渠道与冷却缓存。
 */
func (s *Server) disableAntigravityTOSCredential(ctx context.Context, cfg *model.Config, res *fwResult) bool {
	if !isAntigravityTOSViolation(cfg, res) {
		return false
	}
	return s.disableAntigravityAccount(ctx, cfg.ID, gjson.Get(cfg.OAuthCredential, "refresh_token").String())
}

/**
 * @brief 按实际被拒的刷新令牌条件停用渠道。
 * @param ctx 本次上游请求上下文。
 * @param channelID 保存该账号的渠道。
 * @param refreshToken 产生这次拒绝的账号刷新令牌。
 * @return 已停用、渠道原本停用或账号已更换时为 true；持久化失败为 false。
 * @note 同账号的 access token 轮转可以继续停用；停用后失效凭证、渠道与冷却缓存。
 */
func (s *Server) disableAntigravityAccount(ctx context.Context, channelID int64, refreshToken string) bool {
	disableCtx, cancel := cooldownWriteContext(ctx)
	defer cancel()
	for {
		current, err := s.store.GetConfig(disableCtx, channelID)
		if err != nil {
			log.Printf("[ERROR] 核对 Antigravity TOS 违规账号失败: channel_id=%d err=%v", channelID, err)
			return false
		}
		if !current.Enabled || !current.UsesAntigravityOAuth() ||
			gjson.Get(current.OAuthCredential, "refresh_token").String() != refreshToken {
			return true
		}
		disabled, err := s.store.DisableOAuthChannelIfCredentialMatches(disableCtx, channelID, model.AuthTypeAntigravityOAuth, current.OAuthCredential)
		if err != nil {
			log.Printf("[ERROR] 停用 Antigravity TOS 违规渠道失败: channel_id=%d err=%v", channelID, err)
			return false
		}
		if disabled {
			s.antigravityCredentials.invalidate(channelID)
			s.invalidateChannelRelatedCache(channelID)
			s.InvalidateChannelListCache()
			log.Printf("[DISABLED] Antigravity 账号被上游以 TOS_VIOLATION 停用: channel_id=%d", channelID)
			return true
		}
	}
}

/**
 * @brief 判断项目或订阅请求是否收到明确的账号 TOS 拒绝。
 * @param err 元数据查询错误，允许被包装。
 * @return 强类型元数据错误包含 TOS_VIOLATION 时为 true。
 */
func isAntigravityMetadataTOSViolation(err error) bool {
	metadataErr, ok := errors.AsType[*antigravityauth.MetadataError](err)
	return ok && isAntigravityPermissionReason(metadataErr.HTTPStatus, []byte(metadataErr.UpstreamResponseBody()), "TOS_VIOLATION")
}

type antigravityCredentialManager struct {
	mu                sync.RWMutex
	entries           map[int64]*antigravityauth.Credential
	refreshes         singleflight.Group
	metadata          singleflight.Group
	refreshTracker    *oauthCredentialRefreshTracker
	service           *antigravityauth.Service
	store             storage.Store
	clientFor         func(*model.Config) *http.Client
	invalidateConfig  func(int64)
	disableTOSAccount func(context.Context, int64, string) bool ///< 条件停用实际收到 TOS 拒绝的账号。
	now               func() time.Time
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
				if errors.Is(winnerErr, model.ErrChannelNotFound) {
					return nil, winnerErr
				}
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

/**
 * @brief 查询并保存 Antigravity 项目或订阅元数据。
 * @param ctx 当前等待者的上下文；取消时仅停止此等待者。
 * @param cfg 渠道配置。
 * @param credential 本次使用的凭证。
 * @param subscription 是否查询订阅和积分信息。
 * @return 当前凭证，可能包含已完成的部分更新。
 * @return 查询或保存失败，以及当前等待者的取消错误。
 * @note 共享任务拥有独立的查询和 SOCKS5 握手生命周期。
 */
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
		metadataCtx := context.WithValue(queryCtx, socks5DialTaskContextKey{}, queryCtx)
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
				project, err = service.FetchProjectID(metadataCtx, current.AccessToken)
				if err == nil {
					current, err = m.updateStoredMetadata(queryCtx, cfg.ID, current, func(latest *antigravityauth.Credential) { latest.ProjectID = project })
					if err != nil {
						return credential, err
					}
					project = current.ProjectID
				}
			}
			if err == nil && subscription {
				tier, credits, err = service.FetchSubscription(metadataCtx, current.AccessToken)
			}
			if antigravityauth.AccessTokenRejected(err) && attempt == 0 {
				current, err = m.resolveTokenCredential(queryCtx, cfg, true, current.AccessToken)
				if err != nil {
					return current, err
				}
				continue
			}
			if err != nil {
				if isAntigravityMetadataTOSViolation(err) && m.disableTOSAccount != nil {
					m.disableTOSAccount(queryCtx, cfg.ID, current.RefreshToken)
				}
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
