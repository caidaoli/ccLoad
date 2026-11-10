package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

type anthropicOAuthCodeRequest struct {
	State string `json:"state"`
	Code  string `json:"code"`
}

func (r *anthropicOAuthCodeRequest) Validate() error {
	if strings.TrimSpace(r.State) == "" {
		return errors.New("state is required")
	}
	if strings.TrimSpace(r.Code) == "" {
		return errors.New("code is required")
	}
	return nil
}

/**
 * @brief 创建 Anthropic 登录管理器，新账号使用登录代理查询实时模型目录。
 * @param service OAuth 认证服务
 * @param store 渠道存储
 * @param baseURL 应用全局覆盖后的模型上游基础地址
 * @param invalidate 授权完成后的缓存失效回调
 * @return 登录会话管理器
 */
func newAnthropicOAuthManager(
	service *anthropicauth.Service,
	store storage.Store,
	baseURL string,
	invalidate func(int64),
) *codexOAuthManager {
	return &codexOAuthManager{
		provider: "Anthropic", redirectURI: anthropicauth.RedirectURI,
		timeout: 30 * time.Minute, now: time.Now,
		sessions: make(map[string]*codexOAuthSession), invalidate: invalidate,
		prepare: func(_ string, options oauthLoginOptions) (string, string, func(context.Context, string) (any, error), func(context.Context, any) (int64, error), error) {
			if service == nil || store == nil {
				return "", "", nil, nil, errors.New("oauth: Anthropic is unavailable")
			}
			state, err := anthropicauth.GenerateState()
			if err != nil {
				return "", "", nil, nil, err
			}
			pkce, err := anthropicauth.GeneratePKCE()
			if err != nil {
				return "", "", nil, nil, err
			}
			flowService := *service
			flowService.Client = options.clientOr(flowService.Client)
			authURL, err := flowService.AuthorizationLink(state, pkce)
			if err != nil {
				return "", "", nil, nil, err
			}
			exchange := func(ctx context.Context, code string) (any, error) {
				return flowService.ExchangeCode(ctx, code, state, pkce)
			}
			commit := func(ctx context.Context, raw any) (int64, error) {
				credential, ok := raw.(*anthropicauth.Credential)
				if !ok || credential == nil {
					return 0, errors.New("oauth: Anthropic exchange returned an invalid credential")
				}
				channel, _, err := createOrUpdateAnthropicChannel(ctx, store, flowService.Client, baseURL, credential, options.proxyURL)
				if err != nil {
					return 0, err
				}
				return channel.ID, nil
			}
			return state, authURL, exchange, commit, nil
		},
	}
}

/**
 * @brief 为新账号创建具有实时模型目录的渠道，或为同账号更新授权。
 * @param ctx 请求上下文
 * @param store 渠道存储
 * @param client 登录会话使用的 HTTP 客户端，包含所选代理
 * @param baseURL 应用全局覆盖后的上游基础地址
 * @param credential 授权交换取得的账号凭证
 * @param proxyURL 本次登录选择的代理地址
 * @return 创建或更新后的渠道
 * @return 新建渠道时返回 true，重新授权返回 false
 * @return 模型目录查询、凭证编码或存储失败时返回错误；目录失败不会创建渠道
 * @note 同账号重新授权保留已配置模型，不请求模型目录。
 */
func createOrUpdateAnthropicChannel(
	ctx context.Context,
	store storage.Store,
	client *http.Client,
	baseURL string,
	credential *anthropicauth.Credential,
	proxyURL string,
) (*model.Config, bool, error) {
	credentialJSON, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list channels for Anthropic credential: %w", err)
	}
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesAnthropicOAuth() || cfg.OAuthCredential == "" {
			continue
		}
		existing, parseErr := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil || !sameAnthropicIdentity(existing, credential) {
			continue
		}
		for {
			currentCfg, getErr := store.GetConfig(ctx, cfg.ID)
			if getErr != nil {
				return nil, false, getErr
			}
			current, parseErr := anthropicauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil || !currentCfg.UsesAnthropicOAuth() || !sameAnthropicIdentity(current, credential) {
				return nil, false, errors.New("anthropic credential changed identity during reauthorization")
			}
			merged, mergeErr := current.MergeRefresh(credential)
			if mergeErr != nil {
				return nil, false, mergeErr
			}
			mergedJSON, encodeErr := merged.JSON()
			if encodeErr != nil {
				return nil, false, encodeErr
			}
			updated, updateErr := store.CompareAndSwapOAuthCredential(
				ctx, currentCfg.ID, model.AuthTypeAnthropicOAuth, currentCfg.OAuthCredential, mergedJSON,
			)
			if updateErr != nil {
				return nil, false, updateErr
			}
			if !updated {
				continue
			}
			winner, reactivateErr := reactivateReauthorizedChannel(ctx, store, currentCfg.ID, proxyURL)
			return winner, false, reactivateErr
		}
	}
	models, err := requestAnthropicModels(ctx, client, baseURL, credential.AccessToken)
	if err != nil {
		return nil, false, err
	}
	channel := newAnthropicOAuthChannel(uniqueAnthropicChannelName(configs, credential), credentialJSON)
	channel.ModelEntries = models
	channel.ProxyURL = proxyURL
	created, err := store.CreateConfig(ctx, channel)
	if err != nil {
		return nil, false, fmt.Errorf("create Anthropic channel: %w", err)
	}
	return created, true, nil
}

/**
 * @brief 构造 Anthropic OAuth 渠道基础配置，模型由调用方从上游目录填充。
 * @param name 渠道名称
 * @param credentialJSON 已校验的凭证 JSON
 * @return 尚未持久化的渠道配置
 */
func newAnthropicOAuthChannel(name, credentialJSON string) *model.Config {
	return &model.Config{
		Name: name, AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON,
		URLs:                  model.ChannelURLs{{URL: anthropicauth.DefaultUpstreamURL, Protocols: []string{"anthropic"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0, Enabled: true, CostMultiplier: 1,
	}
}

func sameAnthropicIdentity(a, b *anthropicauth.Credential) bool {
	if a == nil || b == nil {
		return false
	}
	if a.AccountUUID != "" && b.AccountUUID != "" {
		return a.AccountUUID == b.AccountUUID
	}
	if a.EmailAddress != "" && b.EmailAddress != "" {
		return strings.EqualFold(a.EmailAddress, b.EmailAddress)
	}
	return false
}

func uniqueAnthropicChannelName(configs []*model.Config, credential *anthropicauth.Credential) string {
	identity := strings.TrimSpace(credential.EmailAddress)
	if identity == "" {
		identity = strings.TrimSpace(credential.AccountUUID)
	}
	if identity == "" {
		identity = strings.TrimSpace(credential.OrgUUID)
	}
	if identity == "" {
		identity = "OAuth"
	}
	base := "Anthropic-" + identity
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

// HandleStartAnthropicOAuth starts one hosted Anthropic PKCE login.
func (s *Server) HandleStartAnthropicOAuth(c *gin.Context) {
	if s.anthropicOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic OAuth is unavailable")
		return
	}
	var request oauthLoginStartRequest
	if !bindOAuthLoginStart(c, &request) {
		return
	}
	options, err := s.oauthLoginOptions(request, false)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	authURL, state, err := s.anthropicOAuth.start(options)
	if err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"url": authURL, "state": state, "status": "pending"})
}

// HandleAnthropicOAuthStatus returns one Anthropic login state.
func (s *Server) HandleAnthropicOAuthStatus(c *gin.Context) {
	if s.anthropicOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic OAuth is unavailable")
		return
	}
	status, ok := s.anthropicOAuth.status(c.Query("state"))
	if !ok {
		RespondErrorMsg(c, http.StatusNotFound, "Anthropic OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelAnthropicOAuth cancels one pending Anthropic login.
func (s *Server) HandleCancelAnthropicOAuth(c *gin.Context) {
	var request codexOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.anthropicOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic OAuth is unavailable")
		return
	}
	if err := s.anthropicOAuth.cancel(request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": strings.TrimSpace(request.State), "status": "cancelled"})
}

// HandleSubmitAnthropicOAuthCode accepts the code displayed by Anthropic.
func (s *Server) HandleSubmitAnthropicOAuthCode(c *gin.Context) {
	var request anthropicOAuthCodeRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.anthropicOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic OAuth is unavailable")
		return
	}
	state, err := s.anthropicOAuth.submitAuthorizationCode(request.State, request.Code)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": state, "status": "accepted"})
}

// HandleRefreshAnthropicCredential forces the channel's atomic refresh path.
func (s *Server) HandleRefreshAnthropicCredential(c *gin.Context) {
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
	if !cfg.UsesAnthropicOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use Anthropic OAuth")
		return
	}
	if s.anthropicCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic credential refresh is unavailable")
		return
	}
	credential, err := s.anthropicCredentials.credential(c.Request.Context(), cfg, true)
	if err != nil {
		RespondError(c, http.StatusBadGateway, anthropicCredentialRefreshError(err))
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}

func anthropicCredentialRefreshError(err error) error {
	var upstreamErr interface{ UpstreamResponseBody() string }
	if !errors.As(err, &upstreamErr) {
		return err
	}
	body := strings.TrimSpace(upstreamErr.UpstreamResponseBody())
	if body == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, body)
}
