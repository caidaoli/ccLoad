package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/zaiauth"

	"github.com/gin-gonic/gin"
)

// Z.ai Coding Plan channel provisioning.
//
// Both entry points — the hosted ZCode authorization and a directly supplied
// Coding Plan key — end here, so every Z.ai channel is created with the same
// upstream contract.

const (
	maxZAICredentialImportBytes = 1 << 16
	// zaiCodingPlanName labels the plan in usage summaries. Z.ai exposes no
	// plan tier on the quota endpoint, so the label is the plan itself.
	zaiCodingPlanName = "Coding Plan"
)

var zaiChannelCreateMu sync.Mutex

/**
 * @brief 管理端导入的付费 Key、登录令牌及本次代理设置。
 */
type zaiCredentialImportRequest struct {
	oauthLoginStartRequest              ///< 本次登录代理设置。
	Site                   zaiauth.Site `json:"site"`         ///< 账号站点；缺省国际版。
	APIKey                 string       `json:"api_key"`      ///< 所选站点控制台的 Coding Plan Key。
	AccessToken            string       `json:"access_token"` ///< 所选站点账号令牌，用于派生付费 Key。
	JWTToken               string       `json:"jwt_token"`    ///< ZCode 登录 JWT，体验账号不要求付费 Key。
}

/**
 * @brief 就地清理导入输入，至少提供一种认证材料并校验代理。
 * @return 认证材料缺失、格式非法或代理无效时返回错误。
 */
func (r *zaiCredentialImportRequest) Validate() error {
	site, err := zaiauth.NormalizeSite(r.Site)
	if err != nil {
		return err
	}
	r.Site = site
	r.APIKey = strings.TrimSpace(r.APIKey)
	r.AccessToken = strings.TrimSpace(r.AccessToken)
	r.JWTToken = strings.TrimSpace(r.JWTToken)
	if r.APIKey == "" && r.AccessToken == "" && r.JWTToken == "" {
		return errors.New("api_key, access_token or jwt_token is required")
	}
	// 在上游校验或派生凭证之前拒绝非法认证材料。
	if strings.ContainsAny(r.APIKey, " \r\n\t") || strings.ContainsAny(r.AccessToken, " \r\n\t") || strings.ContainsAny(r.JWTToken, " \r\n\t") {
		return errors.New("credential contains invalid characters")
	}
	return r.oauthLoginStartRequest.Validate()
}

type zaiCredentialImportResponse struct {
	ChannelID   int64  `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Created     bool   `json:"created"`
	Email       string `json:"email,omitempty"`
	UpstreamURL string `json:"upstream_url"`
}

// HandleImportZAICredential provisions a Coding Plan channel from a key or a
// ZCode account authorization.
func (s *Server) HandleImportZAICredential(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxZAICredentialImportBytes)
	var request zaiCredentialImportRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.store == nil || s.zaiService == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "z.ai credential import is unavailable")
		return
	}
	options, err := s.oauthLoginOptions(request.oauthLoginStartRequest, false)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	ctx := c.Request.Context()
	credential, err := s.buildZAICredential(ctx, request, options)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	cfg, created, err := s.commitZAICredential(ctx, credential, options)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	upstream := ""
	if len(cfg.URLs) > 0 {
		upstream = cfg.URLs[0].URL
	}
	RespondJSON(c, http.StatusOK, zaiCredentialImportResponse{
		ChannelID: cfg.ID, ChannelName: cfg.Name, Created: created,
		Email: credential.Email, UpstreamURL: upstream,
	})
}

// HandleRefreshZAICredential re-resolves the Coding Plan key from the stored
// ZCode account authorization. Key-only imports cannot be refreshed.
func (s *Server) HandleRefreshZAICredential(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}
	if !cfg.UsesZAIOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use Z.ai OAuth")
		return
	}
	stored, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if stored.AccessToken == "" {
		RespondErrorMsg(c, http.StatusConflict, "z.ai Coding Plan key cannot be re-resolved without an account authorization")
		return
	}
	if s.zaiCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Z.ai credential refresh is unavailable")
		return
	}
	credential, err := s.zaiCredentials.credential(c.Request.Context(), cfg, true)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}

/**
 * @brief 为登录派生固定站点及出口的服务，不修改全局实例。
 * @param options 登录出口配置。
 * @param site 已验证的账号站点。
 * @return 独立服务实例。
 */
func (s *Server) zaiLoginService(options oauthLoginOptions, site zaiauth.Site) *zaiauth.Service {
	return s.zaiService.ForSite(site, options.client)
}

/**
 * @brief 校验导入材料并尽可能派生付费 Key；JWT 允许独立接入体验套餐。
 * @param ctx 导入上下文。
 * @param request 已校验的管理端输入。
 * @param options 账号请求的代理设置。
 * @return 已归一的账号凭证。
 * @return 没有可用认证材料或凭证校验失败时返回错误。
 */
func (s *Server) buildZAICredential(
	ctx context.Context,
	request zaiCredentialImportRequest,
	options oauthLoginOptions,
) (*zaiauth.Credential, error) {
	service := s.zaiLoginService(options, request.Site)
	credential := &zaiauth.Credential{
		Site: request.Site,
		Type: zaiauth.ChannelType, APIKey: request.APIKey, AccessToken: request.AccessToken,
		JWTToken:    request.JWTToken,
		LastRefresh: time.Now().UTC().Format(time.RFC3339),
	}
	validateKey := credential.APIKey != ""
	if request.AccessToken != "" {
		apiKey, identity, err := service.ResolveCodingPlanAPIKey(ctx, request.AccessToken)
		if err != nil && credential.JWTToken == "" && credential.APIKey == "" {
			return nil, err
		}
		// 派生失败保留显式 Key，随后独立验证，不能以空结果覆盖用户材料。
		if err == nil {
			credential.APIKey = apiKey
			credential.UserID, credential.Email = identity.UserID, identity.Email
			validateKey = false
		}
	}
	if validateKey {
		if err := service.ValidateAPIKey(ctx, credential.APIKey); err != nil {
			return nil, err
		}
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	if credential.JWTToken != "" {
		// Key 只证明付费凭证有效，不能替代 JWT 验证；空套餐仍是有效登录。
		plans, err := s.zaiAccountPlans(ctx, &model.Config{ProxyURL: options.proxyURL}, credential, false)
		if err != nil {
			return nil, err
		}
		if !plans.GiftKnown {
			return nil, fmt.Errorf("ZCode 导入凭证暂无法验证：%s", strings.Join(plans.Warnings, "；"))
		}
	}
	return credential, nil
}

/**
 * @brief 保存已授权账号并尽力追加实际套餐模型；暂无模型不阻断授权。
 * @param ctx 操作上下文。
 * @param credential 已归一的登录材料。
 * @param options 登录代理设置。
 * @return 已保存的渠道；失败时为 nil。
 * @return 是否新建渠道。
 * @return 上下文取消、存储或并发身份变更时返回错误。
 * @note 写入后失效凭证、套餐及渠道列表缓存。
 */
func (s *Server) commitZAICredential(
	ctx context.Context,
	credential *zaiauth.Credential,
	options oauthLoginOptions,
) (*model.Config, bool, error) {
	var names []string
	if credential.JWTToken != "" {
		var err error
		names, err = s.zaiAvailableModels(ctx, &model.Config{ProxyURL: options.proxyURL}, credential)
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			// 登录身份已由授权流程确认；套餐发现失败不应丢弃授权结果。
			log.Printf("[WARN] ZCode 已授权账号暂未获取模型，继续保存渠道: %v", err)
		}
	} else {
		names = s.zaiChannelModels(ctx, credential, options.proxyURL)
	}
	cfg, created, err := createOrUpdateZAIChannel(
		ctx, s.store, credential,
		credential.Site.CodingPlanBaseURL(), names,
		options.proxyURL,
	)
	if err != nil {
		return nil, false, err
	}
	if !created {
		committed, parseErr := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			return nil, false, parseErr
		}
		for attempt := 0; ; attempt++ {
			next := model.CloneModelEntries(cfg.ModelEntries)
			seen := make(map[string]bool)
			for _, entry := range next {
				seen[strings.ToLower(entry.Model)] = true
			}
			for _, name := range names {
				if !seen[strings.ToLower(name)] {
					next = append(next, model.ModelEntry{Model: name})
					seen[strings.ToLower(name)] = true
				}
			}
			if len(next) == len(cfg.ModelEntries) {
				break
			}
			matched, updateErr := s.store.UpdateModelStateIfSnapshotMatches(ctx, cfg, next, cfg.ScheduledCheckModel, nil)
			if updateErr != nil {
				return nil, false, updateErr
			}
			latest, loadErr := s.store.GetConfig(ctx, cfg.ID)
			if loadErr != nil {
				return nil, false, loadErr
			}
			cfg = latest
			if matched {
				break
			}
			current, parseErr := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
			if parseErr != nil || current.Site != committed.Site || current.JWTToken != committed.JWTToken || current.APIKey != committed.APIKey {
				return nil, false, errors.New("ZCode credential changed while updating models")
			}
			if retryErr := waitOAuthCASRetry(ctx, attempt); retryErr != nil {
				return nil, false, retryErr
			}
		}
	}
	s.zaiCredentials.invalidate(cfg.ID)
	s.invalidateZAIPlans(cfg.ID)
	s.invalidateChannelRelatedCache(cfg.ID)
	s.InvalidateChannelListCache()
	return cfg, created, nil
}

/**
 * @brief 按账号站点查询付费目录，所有目录不可用时使用内置列表。
 * @param ctx 查询上下文。
 * @param credential 账号认证材料及站点。
 * @param proxyURL 登录使用的代理。
 * @return 实际目录或内置兜底列表。
 */
func (s *Server) zaiChannelModels(ctx context.Context, credential *zaiauth.Credential, proxyURL string) []string {
	models, err := s.zaiCodingPlanModels(ctx, &model.Config{ProxyURL: proxyURL}, credential)
	if err != nil || len(models) == 0 {
		if err != nil {
			log.Printf("[WARN] Z.ai 模型目录不可用，新渠道使用内置列表: %v", err)
		}
		return zaiauth.DefaultModels
	}
	return models
}

func createOrUpdateZAIChannel(
	ctx context.Context,
	store storage.Store,
	credential *zaiauth.Credential,
	baseURL string,
	models []string,
	proxyURL string,
) (*model.Config, bool, error) {
	if store == nil || credential == nil {
		return nil, false, errors.New("persist z.ai credential: unavailable")
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}

	zaiChannelCreateMu.Lock()
	defer zaiChannelCreateMu.Unlock()
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list channels for z.ai credential: %w", err)
	}
	if existing, found, updateErr := updateExistingZAIChannel(ctx, store, configs, credential, proxyURL); found || updateErr != nil {
		return existing, false, updateErr
	}
	channel := newZAIOAuthChannel(uniqueZAIChannelName(configs, zaiChannelBaseName(credential)), credentialJSON, baseURL, models)
	channel.ProxyURL = proxyURL
	created, err := store.CreateConfig(ctx, channel)
	if err != nil {
		return nil, false, fmt.Errorf("create z.ai channel: %w", err)
	}
	return created, true, nil
}

/**
 * @brief 按账号身份匹配已有渠道并 CAS 合并授权，不用空 Key 匹配体验账号。
 * @param ctx 操作上下文。
 * @param store 渠道存储。
 * @param configs 待匹配的渠道快照。
 * @param credential 新登录材料。
 * @param proxyURL 非空时更新渠道代理。
 * @return 已更新渠道；未匹配或失败时为 nil。
 * @return 是否匹配到原渠道。
 * @return 合并、CAS 冲突或存储失败时返回错误。
 */
func updateExistingZAIChannel(
	ctx context.Context,
	store storage.Store,
	configs []*model.Config,
	credential *zaiauth.Credential,
	proxyURL string,
) (*model.Config, bool, error) {
	identity := credential.Identity()
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesZAIOAuth() || strings.TrimSpace(cfg.OAuthCredential) == "" {
			continue
		}
		existing, parseErr := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			continue
		}
		// 两站同邮箱或 UID 不代表同一登录，不能混合 Key、JWT 或额度。
		if existing.Site != credential.Site {
			continue
		}
		// Without an account identity the API key itself is the only stable
		// handle, so re-importing the same key updates in place.
		matched := (credential.APIKey != "" && existing.APIKey == credential.APIKey) ||
			(credential.JWTToken != "" && existing.JWTToken == credential.JWTToken)
		if !identity.IsZero() && !existing.Identity().IsZero() {
			matched = existing.Identity().Matches(identity)
		}
		if !matched {
			continue
		}
		merged, mergeErr := existing.MergeRefresh(credential)
		if mergeErr != nil {
			return nil, true, mergeErr
		}
		mergedJSON, encodeErr := merged.JSON()
		if encodeErr != nil {
			return nil, true, encodeErr
		}
		updated, updateErr := store.CompareAndSwapOAuthCredential(
			ctx, cfg.ID, model.AuthTypeZAIOAuth, cfg.OAuthCredential, mergedJSON,
		)
		if updateErr != nil {
			return nil, true, updateErr
		}
		if !updated {
			return nil, true, errors.New("z.ai credential changed during reauthorization")
		}
		persisted, reactivateErr := reactivateReauthorizedChannel(ctx, store, cfg.ID, proxyURL)
		return persisted, true, reactivateErr
	}
	return nil, false, nil
}

/**
 * @brief 生成可区分国内外账号的渠道默认名。
 * @param credential 已归一凭证；nil 使用国际版默认名。
 * @return 站点前缀加邮箱或 CodingPlan。
 */
func zaiChannelBaseName(credential *zaiauth.Credential) string {
	if credential != nil {
		if email := strings.TrimSpace(credential.Email); email != "" {
			return credential.Site.Label() + "-" + email
		}
		return credential.Site.Label() + "-CodingPlan"
	}
	return "Z.ai-CodingPlan"
}

func uniqueZAIChannelName(configs []*model.Config, base string) string {
	used := make(map[string]struct{}, len(configs))
	for _, cfg := range configs {
		if cfg != nil {
			used[strings.ToLower(strings.TrimSpace(cfg.Name))] = struct{}{}
		}
	}
	if _, exists := used[strings.ToLower(base)]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s (%d)", base, suffix)
		if _, exists := used[strings.ToLower(candidate)]; !exists {
			return candidate
		}
	}
}

/**
 * @brief 创建账号渠道；空目录保留为空，不为尚无套餐模型的账号填入付费目录。
 * @param name 渠道名称。
 * @param credentialJSON 已验证的凭证 JSON。
 * @param baseURL 上游地址；空值使用默认地址。
 * @param modelNames 实际发现的模型；付费目录回退由调用者处理。
 * @return 尚未持久化的渠道配置。
 */
func newZAIOAuthChannel(name, credentialJSON, baseURL string, modelNames []string) *model.Config {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = zaiauth.CodingPlanAPIBaseURL
		if credential, err := zaiauth.ParseCredential([]byte(credentialJSON)); err == nil {
			baseURL = credential.Site.CodingPlanBaseURL()
		}
	}
	return &model.Config{
		Name: name, AuthType: model.AuthTypeZAIOAuth, OAuthCredential: credentialJSON,
		URLs:                  model.ChannelURLs{{URL: baseURL, Protocols: []string{"anthropic"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0,
		Enabled:               true,
		CostMultiplier:        1,
		ModelEntries:          oauthModelEntries(modelNames),
	}
}

/**
 * @brief ZAI 导入时分别验证付费 Key 与体验登录，不修改公共 CSV 策略。
 * @param ctx 校验上下文，取消时中止上游验证。
 * @param existing 现有渠道快照，用于取得代理等出站配置。
 * @param raw 待导入的凭证 JSON。
 * @return 规范化凭证 JSON；失败时为空字符串。
 * @return 解析、付费 Key 或 JWT 登录验证失败时返回错误；成功时为 nil。
 * @note 访问上游但不要求套餐已发放；不合并旧字段，不持久化凭证。
 */
func (s *Server) validateImportedZAICredential(ctx context.Context, existing *model.Config, raw string) (string, error) {
	credential, err := zaiauth.ParseCredential([]byte(raw))
	if err != nil {
		return "", err
	}
	service := s.zaiPlanService(existing, credential)
	if credential.APIKey != "" {
		if err := service.ValidateAPIKey(ctx, credential.APIKey); err != nil {
			return "", err
		}
	}
	if credential.JWTToken != "" {
		// 独立验证体验登录，防止有效 Key 掩盖失效 JWT；不要求已发放套餐。
		plans, err := service.FetchPlans(ctx, credential)
		if err != nil {
			return "", err
		}
		if !plans.GiftKnown {
			return "", errors.New("无法验证 ZCode 登录凭证")
		}
	}
	return credential.JSON()
}
