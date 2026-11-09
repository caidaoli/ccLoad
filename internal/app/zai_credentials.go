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

/**
 * @brief 标记在途刷新已被另一个站点的账号替换，当前请求必须停止以免向旧地址发送新凭证。
 */
var errZAICredentialSiteChanged = errors.New("z.ai credential site changed during re-resolution")

/**
 * @brief 管理按渠道与凭证快照隔离的 Coding Plan 凭证及并发刷新。
 * @details 付费 Key 无定时刷新；上游 401 或管理员强制刷新时重新解析凭证。
 */
type zaiCredentialManager struct {
	mu               sync.RWMutex                      ///< 保护缓存条目。
	entries          map[int64]zaiCredentialCacheEntry ///< 每渠道保留一个已解析快照。
	refreshes        singleflight.Group                ///< 合并相同身份的刷新。
	refreshTracker   *oauthCredentialRefreshTracker    ///< 跟踪服务生命周期内的刷新。
	store            storage.Store                     ///< 凭证持久存储。
	clientFor        func(*model.Config) *http.Client  ///< 渠道出口客户端。
	invalidateConfig func(int64)                       ///< 落库后失效配置缓存。
	now              func() time.Time                  ///< 刷新时间来源。
}

/**
 * @brief 绑定原始凭证快照，防止在途旧请求的回填被新配置复用。
 */
type zaiCredentialCacheEntry struct {
	source     string              ///< 对应的持久凭证 JSON，包含套餐偏好。
	credential *zaiauth.Credential ///< 只读缓存副本，对外返回前深拷贝。
}

/**
 * @brief 创建独立的渠道凭证缓存和刷新管理器。
 * @param store 凭证持久存储。
 * @param clientFor 渠道客户端工厂；nil 时使用默认客户端。
 * @param invalidate 刷新落库后的配置缓存失效回调；允许 nil。
 * @return 未加载凭证的管理器。
 */
func newZAICredentialManager(
	store storage.Store,
	clientFor func(*model.Config) *http.Client,
	invalidate func(int64),
) *zaiCredentialManager {
	return &zaiCredentialManager{
		entries: make(map[int64]zaiCredentialCacheEntry), store: store,
		clientFor: clientFor, invalidateConfig: invalidate, now: time.Now,
	}
}

/**
 * @brief 读取渠道凭证，强制刷新时使用原站点账号重新派生 Key。
 * @param ctx 等待上下文，取消不会中断其他等待者的刷新。
 * @param cfg 当前渠道快照。
 * @param forceRefresh 是否重新派生 Key。
 * @return 独立凭证；刷新失败时尽量返回旧凭证，站点已变更时为 nil。
 * @return 配置缺失、取消或上游派生失败时返回错误；站点变更返回可由 errors.Is 识别的 errZAICredentialSiteChanged。
 * @note 同渠道同站点的刷新合并，并以 CAS 防止覆盖新凭证。
 */
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
	rejectedKey := credential.APIKey

	resultCh := m.refreshes.DoChan(oauthCredentialRefreshSingleflightKey(cfg.ID, string(credential.Site)+"/"+rejectedKey, true), func() (any, error) {
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
		if current.Site != credential.Site || current.APIKey != rejectedKey || current.AccessToken != credential.AccessToken {
			m.cache(currentCfg.ID, currentCfg.OAuthCredential, current)
			return current, nil
		}
		if current.AccessToken == "" {
			return nil, errors.New("z.ai Coding Plan key was rejected and cannot be re-resolved without an account authorization")
		}
		client := http.DefaultClient
		if m.clientFor != nil {
			client = m.clientFor(currentCfg)
		}
		apiKey, identity, resolveErr := zaiauth.NewService(client).ForSite(current.Site, client).ResolveCodingPlanAPIKey(refreshCtx, current.AccessToken)
		if resolveErr != nil {
			return nil, fmt.Errorf("re-resolve z.ai Coding Plan key for channel %d: %w", currentCfg.ID, resolveErr)
		}
		resolved := &zaiauth.Credential{
			Site:   current.Site,
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
	resolved := result.Val.(*zaiauth.Credential)
	if resolved.Site != credential.Site {
		return nil, errZAICredentialSiteChanged
	}
	// singleflight 的结果由等待者共享，运行时编码会就地归一，必须先隔离副本。
	return cloneZAICredential(resolved), nil
}

/**
 * @brief 以 CAS 保存派生结果，站点或账号已变更时返回最新凭证。
 * @param ctx 存储操作上下文。
 * @param cfg 派生时的渠道快照。
 * @param resolvedFrom 派生所用的认证材料。
 * @param resolved 新派生结果。
 * @return 实际生效的独立凭证。
 * @return 合并、存储或凭证解析失败时返回错误。
 * @note 成功写入后更新凭证缓存并失效渠道缓存。
 */
func (m *zaiCredentialManager) persistResolvedKey(
	ctx context.Context,
	cfg *model.Config,
	resolvedFrom *zaiauth.Credential,
	resolved *zaiauth.Credential,
) (*zaiauth.Credential, error) {
	currentCfg := cfg
	current := resolvedFrom
	for {
		if current.Site != resolvedFrom.Site || current.APIKey != resolvedFrom.APIKey || current.AccessToken != resolvedFrom.AccessToken {
			m.cache(currentCfg.ID, currentCfg.OAuthCredential, current)
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
			m.cache(currentCfg.ID, payload, merged)
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

/**
 * @brief 缓存独立凭证副本并绑定其来源快照。
 * @param channelID 渠道 ID。
 * @param source 与凭证对应的持久 JSON。
 * @param credential 已解析的凭证。
 * @note 持锁替换条目；旧快照回填不会命中新快照的读取。
 */
func (m *zaiCredentialManager) cache(channelID int64, source string, credential *zaiauth.Credential) {
	m.mu.Lock()
	m.entries[channelID] = zaiCredentialCacheEntry{source: source, credential: cloneZAICredential(credential)}
	m.mu.Unlock()
}

/**
 * @brief 读取与请求配置完全匹配的凭证，快照变化时重新解析。
 * @param cfg 请求持有的渠道快照。
 * @return 独立凭证，不共享缓存中的可变字段。
 * @return 快照中的凭证 JSON 非法时返回解析错误。
 * @note 并发安全；缓存只复用相同 JSON，包含套餐偏好和站点。
 */
func (m *zaiCredentialManager) cachedOrParse(cfg *model.Config) (*zaiauth.Credential, error) {
	m.mu.RLock()
	entry, ok := m.entries[cfg.ID]
	m.mu.RUnlock()
	if ok && entry.source == cfg.OAuthCredential {
		return cloneZAICredential(entry.credential), nil
	}
	parsed, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse z.ai credential for channel %d: %w", cfg.ID, err)
	}
	m.cache(cfg.ID, cfg.OAuthCredential, parsed)
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

/**
 * @brief 深拷贝可变偏好与额度快照，避免修改缓存中的凭证。
 * @param credential 来源凭证，允许 nil。
 * @return 独立副本；输入 nil 时为 nil。
 */
func cloneZAICredential(credential *zaiauth.Credential) *zaiauth.Credential {
	if credential == nil {
		return nil
	}
	clone := *credential
	if credential.FreeFirst != nil {
		value := *credential.FreeFirst
		clone.FreeFirst = &value
	}
	clone.OAuthUsage = append([]byte(nil), credential.OAuthUsage...)
	return &clone
}

/**
 * @brief 将实际凭证投影到请求配置，供代理与管理检测共用。
 * @param cfg 原始渠道快照，不修改。
 * @param credential 已归一的实际凭证。
 * @return 包含设备身份及凭证的配置副本；编码失败时为 nil。
 * @return 优先 API Key，否则使用 JWT。
 * @return 凭证无法编码时返回错误。
 */
func zaiRuntimeConfig(cfg *model.Config, credential *zaiauth.Credential) (*model.Config, string, error) {
	raw, err := credential.JSON()
	if err != nil {
		return nil, "", err
	}
	runtimeCfg := cfg.Clone()
	runtimeCfg.ZAIDeviceID = credential.DeviceID
	runtimeCfg.OAuthCredential = raw
	key := credential.APIKey
	if key == "" {
		key = credential.JWTToken
	}
	return runtimeCfg, key, nil
}
