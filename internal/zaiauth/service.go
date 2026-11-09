package zaiauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

/**
 * @brief 访问 ZCode 公共控制面及所选站点的业务接口，不持久化凭证。
 */
type Service struct {
	Site                Site             ///< 已验证的账号站点。
	SubscriptionBaseURL string           ///< 付费订阅根地址；空值沿用 BizBaseURL。
	Client              *http.Client     ///< 请求出口。
	OAuthBaseURL        string           ///< 两站共用的 ZCode 控制面。
	BizBaseURL          string           ///< 账号项目与 Key 业务根地址。
	CodingModelsURL     string           ///< 付费 Coding Plan 模型目录。
	ModelsURL           string           ///< 通用模型目录及 Key 验证。
	CommunityURL        string           ///< 公共 models.dev 目录。
	QuotaLimitURL       string           ///< 付费额度窗口。
	Now                 func() time.Time ///< 可注入的查询时钟。
}

/**
 * @brief 创建国际站控制面服务；付费推理地址由站点固定配置提供。
 * @param client 请求出口；nil 使用默认 HTTP 客户端。
 * @return 独立服务配置，可用 ForSite 切换账号所属站点。
 */
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client: client, OAuthBaseURL: OAuthAPIBaseURL, BizBaseURL: BizBaseURL,
		CodingModelsURL: CodingModelsURL, ModelsURL: ModelsURL,
		CommunityURL:  CommunityCatalogURL,
		QuotaLimitURL: QuotaLimitURL, Now: time.Now,
	}
}

// Flow is one pending ZCode CLI authorization.
type Flow struct {
	FlowID          string `json:"flow_id"`
	AuthorizeURL    string `json:"authorize_url"`
	ExpiresAt       int64  `json:"expires_at"`
	PollIntervalSec int    `json:"poll_interval_sec"`
}

// PollStatus is the state of a pending authorization.
type PollStatus string

const (
	// PollPending means the user has not finished authorizing yet.
	PollPending PollStatus = "pending"
	// PollReady means the authorization produced a credential.
	PollReady PollStatus = "ready"
	// PollFailed means the authorization was rejected upstream.
	PollFailed PollStatus = "failed"
)

// PollResult is one ZCode CLI poll response.
type PollResult struct {
	Status      PollStatus
	AccessToken string
	JWTToken    string
	Identity    Identity
	Name        string
}

// ErrOAuthFlowUnavailable reports that ZCode's hosted CLI OAuth flow rejected
// the request outright. The endpoint is enabled per client release, so this is
// an upstream availability fact rather than a ccLoad misconfiguration.
var ErrOAuthFlowUnavailable = errors.New("z.ai CLI OAuth flow is unavailable upstream")

// GeneratePollToken returns the client-side bearer that binds one CLI flow.
func GeneratePollToken() (string, error) {
	random := make([]byte, pollTokenBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate z.ai poll token: %w", err)
	}
	return hex.EncodeToString(random), nil
}

/**
 * @brief 按站点启动 ZCode 授权并设置对应的浏览器回跳参数。
 * @param ctx 请求上下文。
 * @param pollToken 绑定本次登录的轮询令牌。
 * @return 上游登录会话。
 * @return 配置、网络或登录响应非法时返回错误。
 */
func (s *Service) InitFlow(ctx context.Context, pollToken string) (*Flow, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(pollToken) == "" {
		return nil, errors.New("z.ai poll token is required")
	}
	site, err := NormalizeSite(s.Site)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]string{"provider": string(site)})
	if err != nil {
		return nil, fmt.Errorf("encode z.ai OAuth init request: %w", err)
	}
	data, err := s.envelopeRequest(ctx, http.MethodPost,
		strings.TrimRight(s.OAuthBaseURL, "/")+"/oauth/cli/init", pollToken, payload, "OAuth init")
	if err != nil {
		return nil, err
	}
	var flow struct {
		FlowID          string `json:"flow_id"`
		PollToken       string `json:"poll_token"`
		AuthorizeURL    string `json:"authorize_url"`
		ExpiresAt       int64  `json:"expires_at"`
		PollIntervalSec int    `json:"poll_interval_sec"`
	}
	if err := json.Unmarshal(data, &flow); err != nil {
		return nil, fmt.Errorf("decode z.ai OAuth init response: %w", err)
	}
	if strings.TrimSpace(flow.FlowID) == "" || strings.TrimSpace(flow.AuthorizeURL) == "" {
		return nil, errors.New("z.ai OAuth init response is incomplete")
	}
	authorizeURL, err := url.Parse(flow.AuthorizeURL)
	if err != nil || authorizeURL.Scheme != "https" || authorizeURL.Host == "" {
		return nil, errors.New("z.ai OAuth init response has an invalid authorize_url")
	}
	back := "https://zcode.z.ai/app/oauth/login?" + url.Values{"redirect": {"zcode://oauth/callback"}, "app_version": {AppVersion}}.Encode()
	redirectParam := "redirect_uri"
	if site == SiteBigModel {
		redirectParam = "redirect"
	}
	query := authorizeURL.Query()
	query.Set(redirectParam, back)
	authorizeURL.RawQuery = query.Encode()
	return &Flow{
		FlowID: flow.FlowID, AuthorizeURL: authorizeURL.String(),
		ExpiresAt: flow.ExpiresAt, PollIntervalSec: flow.PollIntervalSec,
	}, nil
}

/**
 * @brief 读取登录轮询状态，ready 可以仅包含体验 JWT。
 * @param ctx 轮询上下文。
 * @param flowID 上游登录会话 ID。
 * @param pollToken 会话轮询令牌。
 * @return 当前登录状态与认证材料。
 * @return 网络、响应格式或 ready 缺少全部认证材料时返回错误。
 */
func (s *Service) Poll(ctx context.Context, flowID, pollToken string) (*PollResult, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(flowID) == "" || strings.TrimSpace(pollToken) == "" {
		return nil, errors.New("z.ai flow id and poll token are required")
	}
	target := strings.TrimRight(s.OAuthBaseURL, "/") + "/oauth/cli/poll/" + url.PathEscape(flowID)
	data, err := s.envelopeRequest(ctx, http.MethodGet, target, pollToken, nil, "OAuth poll")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Status string `json:"status"`
		Token  string `json:"token"`
		User   struct {
			UserID string `json:"user_id"`
			Email  string `json:"email"`
			Name   string `json:"name"`
		} `json:"user"`
		ZAI struct {
			AccessToken string `json:"access_token"`
		} `json:"zai"`
		BigModel struct {
			AccessToken string `json:"access_token"` ///< 国内授权令牌。
			Alias       string `json:"accessToken"`  ///< 国内接口的驼峰别名。
		} `json:"bigmodel"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode z.ai OAuth poll response: %w", err)
	}
	accessToken := strings.TrimSpace(payload.ZAI.AccessToken)
	if s.Site == SiteBigModel {
		accessToken = strings.TrimSpace(payload.BigModel.AccessToken)
		if accessToken == "" {
			accessToken = strings.TrimSpace(payload.BigModel.Alias)
		}
	}
	switch PollStatus(strings.TrimSpace(payload.Status)) {
	case PollPending:
		return &PollResult{Status: PollPending}, nil
	case PollFailed:
		return &PollResult{Status: PollFailed}, nil
	case PollReady:
		if accessToken == "" && strings.TrimSpace(payload.Token) == "" {
			return nil, errors.New("z.ai OAuth poll response is missing access_token and token")
		}
		return &PollResult{
			Status:      PollReady,
			AccessToken: accessToken,
			JWTToken:    strings.TrimSpace(payload.Token),
			Identity: Identity{
				UserID: strings.TrimSpace(payload.User.UserID),
				Email:  strings.TrimSpace(payload.User.Email),
			},
			Name: strings.TrimSpace(payload.User.Name),
		}, nil
	default:
		return nil, errors.New("z.ai OAuth poll response has an unknown status")
	}
}

// ResolveCodingPlanAPIKey exchanges a ZCode access token for the account's
// Coding Plan API key, creating ZCode's key on first use exactly like the
// official client does.
func (s *Service) ResolveCodingPlanAPIKey(ctx context.Context, accessToken string) (string, Identity, error) {
	if err := s.validate(); err != nil {
		return "", Identity{}, err
	}
	if strings.TrimSpace(accessToken) == "" {
		return "", Identity{}, errors.New("z.ai access token is required")
	}
	bizToken, err := s.bizToken(ctx, accessToken)
	if err != nil {
		return "", Identity{}, err
	}
	customer, identity, err := s.customerInfo(ctx, bizToken)
	if err != nil {
		return "", Identity{}, err
	}
	keysURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys",
		strings.TrimRight(s.BizBaseURL, "/"), url.PathEscape(customer.organizationID), url.PathEscape(customer.projectID))
	apiKeyID, err := s.findOrCreateAPIKey(ctx, bizToken, keysURL)
	if err != nil {
		return "", identity, err
	}
	secret, err := s.copyAPIKeySecret(ctx, bizToken, keysURL, apiKeyID)
	if err != nil {
		return "", identity, err
	}
	return apiKeyID + "." + secret, identity, nil
}

// ListModels returns the model ids the account can call, newest catalog first.
//
// The Coding Plan keeps its own catalog: models reach it before the general API
// lists them (glm-5.3 did), so the plan endpoint is authoritative and the
// general one only fills in what it misses. Discovery is live on purpose — the
// lineup changes without a ccLoad release.
func (s *Service) ListModels(ctx context.Context, apiKey string) ([]string, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("z.ai API key is required")
	}
	models := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	var lastErr error
	for _, catalog := range []string{s.CodingModelsURL, s.ModelsURL} {
		if strings.TrimSpace(catalog) == "" {
			continue
		}
		listed, err := s.listCatalogModels(ctx, catalog, apiKey)
		if err != nil {
			lastErr = err
			continue
		}
		for _, id := range listed {
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			models = append(models, id)
		}
	}
	if len(models) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("z.ai model catalog is empty")
	}
	return models, nil
}

func (s *Service) listCatalogModels(ctx context.Context, catalogURL, apiKey string) ([]string, error) {
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build z.ai model list request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	ApplySourceHeaders(request.Header)
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("z.ai model list request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read z.ai model list response: %w", err)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, errors.New("z.ai rejected the Coding Plan API key")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("z.ai model list returned HTTP %d", response.StatusCode)
	}
	return parseModelCatalog(body)
}

// parseModelCatalog accepts the OpenAI-shaped catalog and the `{code,data}`
// envelope the Z.ai business APIs use, with either a list of objects or a plain
// list of ids.
func parseModelCatalog(body []byte) ([]string, error) {
	raw := body
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Data) > 0 {
		raw = envelope.Data
	}
	var entries []struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		ModelID string `json:"modelId"`
	}
	models := make([]string, 0, 8)
	if json.Unmarshal(raw, &entries) == nil {
		for _, entry := range entries {
			for _, candidate := range []string{entry.ID, entry.Model, entry.ModelID} {
				if id := strings.TrimSpace(candidate); id != "" {
					models = append(models, id)
					break
				}
			}
		}
		if len(models) > 0 {
			return models, nil
		}
	}
	var ids []string
	if json.Unmarshal(raw, &ids) == nil {
		for _, id := range ids {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				models = append(models, trimmed)
			}
		}
	}
	if len(models) == 0 {
		return nil, errors.New("z.ai model list response has no models")
	}
	return models, nil
}

/**
 * @brief 按账号站点读取无需 Key 的 models.dev Coding Plan 目录。
 * @param ctx 查询上下文。
 * @return 按发布日期排序的模型名称。
 * @return 请求失败、响应非法或所选站点目录为空时返回错误。
 */
func (s *Service) ListCommunityModels(ctx context.Context) ([]string, error) {
	if s == nil || s.Client == nil || strings.TrimSpace(s.CommunityURL) == "" {
		return nil, errors.New("z.ai community catalog is unavailable")
	}
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, s.CommunityURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build z.ai community catalog request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("z.ai community catalog request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCommunityCatalogSize))
	if err != nil {
		return nil, fmt.Errorf("read z.ai community catalog: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("z.ai community catalog returned HTTP %d", response.StatusCode)
	}
	return parseCommunityCatalog(body, s.Site.config().catalog)
}

// parseCommunityCatalog returns the provider's models newest first, so a
// channel seeded from it lists the current flagship before the older ones.
func parseCommunityCatalog(body []byte, provider string) ([]string, error) {
	var catalog map[string]struct {
		Models map[string]struct {
			ReleaseDate string `json:"release_date"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode z.ai community catalog: %w", err)
	}
	entry, ok := catalog[provider]
	if !ok || len(entry.Models) == 0 {
		return nil, fmt.Errorf("z.ai community catalog has no provider %q", provider)
	}
	type catalogModel struct {
		id          string
		releaseDate string
	}
	models := make([]catalogModel, 0, len(entry.Models))
	for id, meta := range entry.Models {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			models = append(models, catalogModel{id: trimmed, releaseDate: strings.TrimSpace(meta.ReleaseDate)})
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("z.ai community catalog provider %q has no models", provider)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].releaseDate != models[j].releaseDate {
			return models[i].releaseDate > models[j].releaseDate
		}
		return models[i].id < models[j].id
	})
	ids := make([]string, len(models))
	for i, entry := range models {
		ids[i] = entry.id
	}
	return ids, nil
}

// ValidateAPIKey confirms a Coding Plan key is accepted without spending quota.
func (s *Service) ValidateAPIKey(ctx context.Context, apiKey string) error {
	if err := s.validate(); err != nil {
		return err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return errors.New("z.ai API key is required")
	}
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, s.ModelsURL, nil)
	if err != nil {
		return fmt.Errorf("build z.ai model list request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	ApplySourceHeaders(request.Header)
	response, err := s.Client.Do(request)
	if err != nil {
		return fmt.Errorf("z.ai model list request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseSize))
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return errors.New("z.ai rejected the Coding Plan API key")
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return fmt.Errorf("z.ai model list returned HTTP %d", response.StatusCode)
	default:
		return nil
	}
}

type customerSelection struct {
	organizationID string
	projectID      string
}

/**
 * @brief 国际版交换业务令牌，国内版直接使用授权返回的业务令牌。
 * @param ctx 请求上下文。
 * @param accessToken 已验证的账号授权材料。
 * @return 未加认证前缀的业务令牌。
 * @return 国际版交换失败或响应缺少令牌时返回错误。
 */
func (s *Service) bizToken(ctx context.Context, accessToken string) (string, error) {
	if s.Site == SiteBigModel {
		return strings.TrimSpace(accessToken), nil
	}
	payload, err := json.Marshal(map[string]string{"token": accessToken})
	if err != nil {
		return "", fmt.Errorf("encode z.ai business login request: %w", err)
	}
	data, err := s.bizRequest(ctx, http.MethodPost,
		strings.TrimRight(s.BizBaseURL, "/")+"/api/auth/z/login", "", payload, "business login")
	if err != nil {
		return "", err
	}
	var token struct {
		AccessToken string `json:"access_token"`
		Alias       string `json:"accessToken"`
	}
	if err := json.Unmarshal(data, &token); err != nil {
		return "", fmt.Errorf("decode z.ai business login response: %w", err)
	}
	resolved := strings.TrimSpace(token.AccessToken)
	if resolved == "" {
		resolved = strings.TrimSpace(token.Alias)
	}
	if resolved == "" {
		return "", errors.New("z.ai business login response is missing access_token")
	}
	return resolved, nil
}

/**
 * @brief 查找个人账号项目；团队项目不能作为个人 Coding Plan 的 Key 来源。
 * @param ctx 请求上下文。
 * @param bizToken 所选站点的业务令牌。
 * @return 默认个人项目，缺省时使用首个可用个人项目。
 * @return 上游确认的账号身份。
 * @return 业务查询失败、响应非法或没有个人项目时返回错误。
 */
func (s *Service) customerInfo(ctx context.Context, bizToken string) (customerSelection, Identity, error) {
	data, err := s.bizRequest(ctx, http.MethodGet,
		strings.TrimRight(s.BizBaseURL, "/")+"/api/biz/customer/getCustomerInfo", bizToken, nil, "customer info")
	if err != nil {
		return customerSelection{}, Identity{}, err
	}
	var payload struct {
		UserID        string `json:"userId"`
		Email         string `json:"email"`
		Organizations []struct {
			OrganizationID   string `json:"organizationId"`
			OrganizationName string `json:"organizationName"`
			Projects         []struct {
				ProjectID   string          `json:"projectId"`
				ProjectName string          `json:"projectName"`
				ProjectType json.RawMessage `json:"projectType"` ///< 2 表示团队项目，兼容数字或字符串。
			} `json:"projects"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return customerSelection{}, Identity{}, fmt.Errorf("decode z.ai customer info response: %w", err)
	}
	var selection customerSelection
	for _, organization := range payload.Organizations {
		organizationID := strings.TrimSpace(organization.OrganizationID)
		if organizationID == "" {
			continue
		}
		projectID := ""
		for _, project := range organization.Projects {
			id := strings.TrimSpace(project.ProjectID)
			if id == "" || strings.Trim(string(project.ProjectType), "\" \t\r\n") == "2" {
				continue
			}
			isDefault := strings.Contains(project.ProjectName, defaultProjectName)
			if projectID == "" || isDefault {
				projectID = id
			}
			if isDefault {
				break
			}
		}
		if projectID == "" {
			continue
		}
		isDefault := strings.Contains(organization.OrganizationName, defaultOrganizationName)
		if selection.organizationID == "" || isDefault {
			selection = customerSelection{organizationID: organizationID, projectID: projectID}
		}
		if isDefault {
			break
		}
	}
	if selection.projectID == "" {
		return customerSelection{}, Identity{}, errors.New("ZCode account has no personal project for an API key")
	}
	identity := Identity{UserID: strings.TrimSpace(payload.UserID), Email: strings.TrimSpace(payload.Email)}
	return selection, identity, nil
}

func (s *Service) findOrCreateAPIKey(ctx context.Context, bizToken, keysURL string) (string, error) {
	data, err := s.bizRequest(ctx, http.MethodGet, keysURL, bizToken, nil, "API key list")
	if err != nil {
		return "", err
	}
	var existing []struct {
		Name   string `json:"name"`
		APIKey string `json:"apiKey"`
	}
	if len(data) > 0 && json.Unmarshal(data, &existing) == nil {
		for _, item := range existing {
			if item.Name == codingPlanAPIKeyName && strings.TrimSpace(item.APIKey) != "" {
				return strings.TrimSpace(item.APIKey), nil
			}
		}
	}
	payload, err := json.Marshal(map[string]string{"name": codingPlanAPIKeyName})
	if err != nil {
		return "", fmt.Errorf("encode z.ai API key request: %w", err)
	}
	created, err := s.bizRequest(ctx, http.MethodPost, keysURL, bizToken, payload, "API key creation")
	if err != nil {
		return "", err
	}
	var key struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(created, &key); err != nil {
		return "", fmt.Errorf("decode z.ai API key response: %w", err)
	}
	if strings.TrimSpace(key.APIKey) == "" {
		return "", errors.New("z.ai API key response is missing apiKey")
	}
	return strings.TrimSpace(key.APIKey), nil
}

func (s *Service) copyAPIKeySecret(ctx context.Context, bizToken, keysURL, apiKeyID string) (string, error) {
	target := strings.TrimRight(keysURL, "/") + "/copy/" + url.PathEscape(apiKeyID)
	data, err := s.bizRequest(ctx, http.MethodGet, target, bizToken, nil, "API key secret")
	if err != nil {
		return "", err
	}
	var payload struct {
		SecretKey string `json:"secretKey"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("decode z.ai API key secret response: %w", err)
	}
	if strings.TrimSpace(payload.SecretKey) == "" {
		return "", errors.New("z.ai API key secret response is missing secretKey")
	}
	return strings.TrimSpace(payload.SecretKey), nil
}

/**
 * @brief 使用 Bearer 轮询令牌访问两站共用的 ZCode 登录接口。
 * @param ctx 请求上下文。
 * @param method HTTP 方法。
 * @param target 完整地址。
 * @param bearer 无前缀的轮询令牌。
 * @param payload 请求 JSON；nil 表示无请求体。
 * @param operation 错误上下文。
 * @return 信封中的 data。
 * @return 网络或上游拒绝时返回错误，接口不可用可匹配 ErrOAuthFlowUnavailable。
 */
func (s *Service) envelopeRequest(
	ctx context.Context,
	method, target, bearer string,
	payload []byte,
	operation string,
) (json.RawMessage, error) {
	body, status, err := s.do(ctx, method, target, "Bearer "+strings.TrimSpace(bearer), payload, operation)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		if status == http.StatusNotFound {
			return nil, fmt.Errorf("z.ai %s: %w", operation, ErrOAuthFlowUnavailable)
		}
		return nil, fmt.Errorf("decode z.ai %s response: %w", operation, err)
	}
	if err := checkEnvelopeCode(envelope.Code, envelope.Msg, status, operation); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

/**
 * @brief 按站点认证契约访问业务接口，国内令牌不加 Bearer。
 * @param ctx 请求上下文。
 * @param method HTTP 方法。
 * @param target 完整地址。
 * @param bearer 原始业务令牌；空值表示无需认证。
 * @param payload 请求 JSON；nil 表示无请求体。
 * @param operation 错误上下文。
 * @return 信封中的 data。
 * @return 网络、响应解析或上游拒绝时返回错误。
 */
func (s *Service) bizRequest(
	ctx context.Context,
	method, target, bearer string,
	payload []byte,
	operation string,
) (json.RawMessage, error) {
	authorization := strings.TrimSpace(bearer)
	if authorization != "" && s.Site != SiteBigModel {
		authorization = "Bearer " + authorization
	}
	body, status, err := s.do(ctx, method, target, authorization, payload, operation)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode z.ai %s response: %w", operation, err)
	}
	if err := checkEnvelopeCode(envelope.Code, envelope.Msg, status, operation); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

/**
 * @brief 发送完整认证值，避免把国内业务认证与公共 OAuth 认证混用。
 * @param ctx 请求上下文。
 * @param method HTTP 方法。
 * @param target 完整地址。
 * @param authorization 已按接口约定设置前缀的认证值；空值不发送。
 * @param payload 请求 JSON；nil 表示无请求体。
 * @param operation 错误上下文。
 * @return 响应正文。
 * @return HTTP 状态码；未取得响应时为零。
 * @return 构造请求、网络或读取失败时返回错误。
 */
func (s *Service) do(
	ctx context.Context,
	method, target, authorization string,
	payload []byte,
	operation string,
) ([]byte, int, error) {
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, target, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build z.ai %s request: %w", operation, err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	ApplySourceHeaders(request.Header)
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("z.ai %s request: %w", operation, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read z.ai %s response: %w", operation, err)
	}
	if response.StatusCode == http.StatusNotFound && len(bytes.TrimSpace(body)) == 0 {
		return nil, response.StatusCode, fmt.Errorf("z.ai %s: %w", operation, ErrOAuthFlowUnavailable)
	}
	return body, response.StatusCode, nil
}

func checkEnvelopeCode(code json.RawMessage, msg string, status int, operation string) error {
	message := strings.TrimSpace(msg)
	if !envelopeCodeSucceeded(code) {
		if message == "" {
			message = fmt.Sprintf("business error %s", strings.TrimSpace(string(code)))
		}
		return fmt.Errorf("z.ai %s failed: %s", operation, message)
	}
	if status < 200 || status >= 300 {
		if message == "" {
			message = fmt.Sprintf("HTTP %d", status)
		}
		return fmt.Errorf("z.ai %s failed: %s", operation, message)
	}
	return nil
}

func envelopeCodeSucceeded(code json.RawMessage) bool {
	value := strings.Trim(strings.TrimSpace(string(code)), `"`)
	switch value {
	case "", "null", "0", "200":
		return true
	default:
		return false
	}
}

/**
 * @brief 拒绝不完整的服务配置及未知站点。
 * @return 缺少客户端、端点或站点非法时返回错误。
 */
func (s *Service) validate() error {
	if s == nil || s.Client == nil || strings.TrimSpace(s.OAuthBaseURL) == "" ||
		strings.TrimSpace(s.BizBaseURL) == "" || strings.TrimSpace(s.ModelsURL) == "" {
		return errors.New("z.ai service is unavailable")
	}
	_, err := NormalizeSite(s.Site)
	return err
}

func requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, RequestTimeout)
}
