package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
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
	return m.credentialForRejectedAccessToken(ctx, cfg, forceRefresh, "")
}

func (m *cursorCredentialManager) credentialAfterUnauthorized(
	ctx context.Context,
	cfg *model.Config,
	rejectedAccessToken string,
) (*cursorauth.Credential, error) {
	if rejectedAccessToken == "" {
		return nil, errors.New("cursor rejected access token is required")
	}
	return m.credentialForRejectedAccessToken(ctx, cfg, true, rejectedAccessToken)
}

func (m *cursorCredentialManager) credentialForRejectedAccessToken(
	ctx context.Context,
	cfg *model.Config,
	forceRefresh bool,
	rejectedAccessToken string,
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
		return credential.Clone(), nil
	}
	if rejectedAccessToken == "" && credential.APIKey == "" {
		return credential.Clone(), errors.New(
			"cursor session was rejected and cannot be re-minted without a stored API key",
		)
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
			return nil, fmt.Errorf("reload cursor credential before re-mint: %w", getErr)
		}
		current, parseErr := cursorauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if parseErr != nil {
			return nil, fmt.Errorf("parse cursor credential for channel %d: %w", currentCfg.ID, parseErr)
		}
		if current.AccessToken != forcedAccessToken {
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
			return credential.Clone(), ctx.Err()
		}
	}
	if result.Err != nil {
		return credential.Clone(), result.Err
	}
	if _, redirected := result.Val.(oauthCredentialRefreshRedirect); redirected {
		winner, winnerErr := m.cachedOrParse(cfg)
		if winnerErr != nil {
			return nil, winnerErr
		}
		return winner.Clone(), nil
	}
	return result.Val.(*cursorauth.Credential), nil
}

/**
 * @brief 写入重新签发的 Cursor 会话并保留同一 API Key 的额度和模型快照。
 * @param ctx 持久化上下文。
 * @param cfg 发起重新签发时的渠道配置。
 * @param resolvedFrom 重新签发所用的凭证。
 * @param resolved 上游返回的会话。
 * @return 保存后的凭证；并发换 Key 或重签时采用当前凭证。
 * @return 读取、编码或写入失败时返回相应错误。
 */
func (m *cursorCredentialManager) persistResolved(
	ctx context.Context,
	cfg *model.Config,
	resolvedFrom *cursorauth.Credential,
	resolved *cursorauth.Credential,
) (*cursorauth.Credential, error) {
	currentCfg := cfg
	current := resolvedFrom
	for {
		if current.AccessToken != resolvedFrom.AccessToken || current.APIKey != resolvedFrom.APIKey {
			m.cache(currentCfg.ID, current)
			return current.Clone(), nil
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
			return merged.Clone(), nil
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
	m.entries[channelID] = credential.Clone()
	m.mu.Unlock()
}

func (m *cursorCredentialManager) cachedOrParse(cfg *model.Config) (*cursorauth.Credential, error) {
	m.mu.RLock()
	credential := m.entries[cfg.ID]
	m.mu.RUnlock()
	if credential != nil {
		return credential.Clone(), nil
	}
	parsed, err := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse cursor credential for channel %d: %w", cfg.ID, err)
	}
	m.mu.Lock()
	if existing := m.entries[cfg.ID]; existing != nil {
		parsed = existing.Clone()
	} else {
		m.entries[cfg.ID] = parsed.Clone()
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

/**
 * @brief 同时保存同一 Cursor User API Key 最新发现的上下文上限与思考档位。
 * @param ctx 保存操作的取消与截止时间上下文。
 * @param channelID 模型发现使用的渠道 ID。
 * @param source 发起模型发现时的凭证快照。
 * @param catalog 实时 SDK 目录；空元数据表会移除对应的旧快照。
 * @return 写入失败，或凭证及模型快照已并发变更且与本次查询不一致时返回错误。
 * @note 条件写入保留并发更新的会话与额度，成功后失效凭证和渠道列表缓存。
 */
func (s *Server) persistCursorCatalog(ctx context.Context, channelID int64, source *cursorauth.Credential, catalog cursorauth.ModelCatalog) error {
	for attempt := 0; ; attempt++ {
		current, err := s.store.GetConfig(ctx, channelID)
		if err != nil {
			return err
		}
		if !current.UsesCursorOAuth() {
			return errors.New("渠道已改用其他认证类型，请重试")
		}
		credential, err := cursorauth.ParseCredential([]byte(current.OAuthCredential))
		if err != nil {
			return err
		}
		if credential.APIKey != source.APIKey {
			return errors.New("用于查询的 Cursor API Key 已变更，请重试")
		}
		if cursorCatalogMatches(credential, catalog.ContextWindows, catalog.EffortLevels, catalog.DefaultEfforts) {
			return nil
		}
		if !cursorCatalogMatches(credential, source.ContextWindows, source.EffortLevels, source.DefaultEfforts) {
			return errors.New("已保存的 Cursor 模型目录已并发更新，请重试")
		}
		credential.ContextWindows = catalog.ContextWindows
		credential.EffortLevels = catalog.EffortLevels
		credential.DefaultEfforts = catalog.DefaultEfforts
		payload, err := credential.JSON()
		if err != nil {
			return err
		}
		updated, err := s.store.CompareAndSwapOAuthCredential(ctx, channelID, model.AuthTypeCursorOAuth, current.OAuthCredential, payload)
		if err != nil {
			return err
		}
		if updated {
			s.cursorCredentials.invalidate(channelID)
			s.InvalidateChannelListCache()
			return nil
		}
		if err := waitOAuthCASRetry(ctx, attempt); err != nil {
			return err
		}
	}
}

/**
 * @brief 比较 Cursor 凭证中的目录快照，适配 JSON 省略空快照表的存储格式。
 * @param credential 需要验证的凭证。
 * @param windows 预期上下文上限。
 * @param levels 预期思考档位。
 * @param defaults 预期默认档位。
 * @return 所有目录元数据相等时返回 true。
 */
func cursorCatalogMatches(credential *cursorauth.Credential, windows map[string]int, levels map[string][]string, defaults map[string]string) bool {
	return maps.Equal(credential.ContextWindows, windows) && maps.EqualFunc(credential.EffortLevels, levels, slices.Equal) && maps.Equal(credential.DefaultEfforts, defaults)
}
