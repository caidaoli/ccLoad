package util

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ccLoad/internal/version"
)

/**
 * @brief 按上游协议获取模型目录。
 */
type ModelsFetcher interface {
	/**
	 * @brief 以渠道身份读取上游模型目录。
	 * @param ctx 请求上下文，取消时中止请求
	 * @param baseURL 上游基础地址
	 * @param apiKey 上游 API Key
	 * @param prepareRequest 可选请求改写，在默认头和认证头设置后调用
	 * @return 上游返回的模型 ID，允许为空
	 * @return 请求或解析失败时返回错误；HTTP 失败可通过 errors.As 取得 ModelsHTTPError
	 */
	FetchModels(ctx context.Context, baseURL string, apiKey string, prepareRequest func(*http.Request)) ([]string, error)
}

// NewModelsFetcher 根据上游协议创建对应的 Fetcher。
// client 为空时使用包内默认客户端；渠道代理由调用方传入已配置好的客户端。
func NewModelsFetcher(upstreamProtocol string, client *http.Client) ModelsFetcher {
	switch NormalizeProtocol(upstreamProtocol) {
	case ProtocolAnthropic:
		return &AnthropicModelsFetcher{client: client}
	case ProtocolOpenAI:
		return &OpenAIModelsFetcher{client: client}
	case ProtocolGemini:
		return &GeminiModelsFetcher{client: client}
	case ProtocolCodex:
		return &CodexModelsFetcher{client: client}
	case ProtocolSystemOne:
		return &SystemOneModelsFetcher{client: client}
	default:
		return &AnthropicModelsFetcher{client: client}
	}
}

// ============================================================
// 公共辅助函数 - 避免重复HTTP请求逻辑
// ============================================================

// 全局复用的 HTTP Client（连接池化，避免每次请求创建新客户端）
// [FIX] P2-8: 使用全局 HTTP Client，复用连接池
var defaultModelsFetcherClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// SetModelsFetcherHTTPClientForTesting 覆盖默认模型抓取 HTTP client。
// 仅供测试使用，用于在受限环境下替换掉真实网络访问。
func SetModelsFetcherHTTPClientForTesting(client *http.Client) {
	if client == nil {
		return
	}
	defaultModelsFetcherClient = client
}

/**
 * @brief 保留模型发现失败响应，供管理端分类并生成安全提示。
 */
type ModelsHTTPError struct {
	StatusCode int         ///< 上游 HTTP 状态码
	Header     http.Header ///< 上游响应头，用于识别 Cloudflare 验证
	Body       []byte      ///< 上游错误正文
}

/**
 * @brief 格式化模型发现失败，Cloudflare 验证不回显 HTML 正文。
 * @return 包含状态码及错误原因的描述
 */
func (e *ModelsHTTPError) Error() string {
	if ClassifyHTTPResponseWithMeta(e.StatusCode, e.Header, e.Body).ChannelCooldownReason == "cloudflare_challenge" {
		return fmt.Sprintf("上游触发 Cloudflare 验证 (HTTP %d)", e.StatusCode)
	}
	if e.StatusCode >= 400 && e.StatusCode < 500 {
		return fmt.Sprintf("上游配置错误 (HTTP %d): %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("上游服务器错误 (HTTP %d): %s", e.StatusCode, e.Body)
}

/**
 * @brief 应用模型发现默认 UA 和渠道改写，然后读取上游响应。
 * @param client 渠道客户端；nil 使用共享模型发现客户端
 * @param req 已初始化认证头的请求
 * @param prepareRequest 可选请求改写，在默认 UA 设置后调用
 * @return 完整响应正文；失败时为 nil
 * @return 网络、读取失败或可通过 errors.As 获取的 ModelsHTTPError
 * @note 改写 req 的请求头；始终关闭上游响应体。
 */
func doHTTPRequest(client *http.Client, req *http.Request, prepareRequest func(*http.Request)) ([]byte, error) {
	if client == nil {
		client = defaultModelsFetcherClient
	}
	req.Header.Set("User-Agent", "ccLoad/"+version.Version)
	if prepareRequest != nil {
		prepareRequest(req)
	}
	if req.Header.Get("User-Agent") == "" {
		// 显式空值阻止 net/http 在渠道删除 UA 后补回 Go 默认值。
		req.Header.Set("User-Agent", "")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &ModelsHTTPError{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
	}

	return body, nil
}

// AnthropicModelsFetcher 实现 Anthropic/Claude Code 渠道的模型列表获取。
type AnthropicModelsFetcher struct {
	client *http.Client
}

type anthropicModelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
		CreatedAt   string `json:"created_at"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	FirstID string `json:"first_id"`
	LastID  string `json:"last_id"`
}

/**
 * @brief 获取 Anthropic 上游模型目录。
 * @param ctx 请求上下文，取消时中止请求
 * @param baseURL 上游基础地址
 * @param apiKey 上游 API Key
 * @param prepareRequest 可选请求改写，在默认头和认证头设置后调用
 * @return 上游返回的模型 ID，允许为空
 * @return 请求或解析失败时返回错误；HTTP 失败可通过 errors.As 取得 ModelsHTTPError
 */
func (f *AnthropicModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string, prepareRequest func(*http.Request)) ([]string, error) {
	// Anthropic Models API: https://docs.claude.com/en/api/models-list
	endpoint := baseURL + "/v1/models"

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// 同时设置两个认证头，与代理转发保持一致
	// 官方API使用x-api-key，第三方中转通常使用Authorization Bearer
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	// 使用公共HTTP请求函数 (ctx已包含在req中)
	body, err := doHTTPRequest(f.client, req, prepareRequest)
	if err != nil {
		return nil, err
	}

	var result anthropicModelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}

	return models, nil
}

// OpenAIModelsFetcher 实现 OpenAI 渠道的模型列表获取。
type OpenAIModelsFetcher struct {
	client *http.Client
}

type openAIModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

/**
 * @brief 获取 OpenAI 上游模型目录。
 * @param ctx 请求上下文，取消时中止请求
 * @param baseURL 上游基础地址
 * @param apiKey 上游 API Key
 * @param prepareRequest 可选请求改写，在默认头和认证头设置后调用
 * @return 上游返回的模型 ID，允许为空
 * @return 请求或解析失败时返回错误；HTTP 失败可通过 errors.As 取得 ModelsHTTPError
 */
func (f *OpenAIModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string, prepareRequest func(*http.Request)) ([]string, error) {
	// OpenAI Models API: https://platform.openai.com/docs/api-reference/models/list
	endpoint := baseURL + "/v1/models"

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)

	// 使用公共HTTP请求函数 (ctx已包含在req中)
	body, err := doHTTPRequest(f.client, req, prepareRequest)
	if err != nil {
		return nil, err
	}

	var result openAIModelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}

	return models, nil
}

// GeminiModelsFetcher 实现 Google Gemini 渠道的模型列表获取。
type GeminiModelsFetcher struct {
	client *http.Client
}

type geminiModelsResponse struct {
	Models []struct {
		Name string `json:"name"` // 格式: "models/gemini-1.5-flash"
	} `json:"models"`
}

/**
 * @brief 获取 Gemini 上游模型目录。
 * @param ctx 请求上下文，取消时中止请求
 * @param baseURL 上游基础地址
 * @param apiKey 上游 API Key
 * @param prepareRequest 可选请求改写，在默认头和认证头设置后调用
 * @return 上游返回的模型 ID，允许为空
 * @return 请求或解析失败时返回错误；HTTP 失败可通过 errors.As 取得 ModelsHTTPError
 */
func (f *GeminiModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string, prepareRequest func(*http.Request)) ([]string, error) {
	// Gemini Models API: https://ai.google.dev/api/rest/v1beta/models/list
	endpoint, err := url.Parse(baseURL + "/v1beta/models")
	if err != nil {
		return nil, fmt.Errorf("解析请求 URL 失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("X-Goog-Api-Key", apiKey)

	// 使用公共HTTP请求函数 (ctx已包含在req中)
	body, err := doHTTPRequest(f.client, req, prepareRequest)
	if err != nil {
		return nil, err
	}

	var result geminiModelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	models := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		// 提取模型名称（去掉"models/"前缀）
		if len(m.Name) > 7 && m.Name[:7] == "models/" {
			models = append(models, m.Name[7:])
		} else {
			models = append(models, m.Name)
		}
	}

	return models, nil
}

// CodexModelsFetcher 实现 Codex 渠道的模型列表获取。
type CodexModelsFetcher struct {
	client *http.Client
}

/**
 * @brief 获取 Codex 上游模型目录。
 * @param ctx 请求上下文，取消时中止请求
 * @param baseURL 上游基础地址
 * @param apiKey 上游 API Key
 * @param prepareRequest 可选请求改写，在默认头和认证头设置后调用
 * @return 上游返回的模型 ID，允许为空
 * @return 请求或解析失败时返回错误；HTTP 失败可通过 errors.As 取得 ModelsHTTPError
 */
func (f *CodexModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string, prepareRequest func(*http.Request)) ([]string, error) {
	openAIFetcher := &OpenAIModelsFetcher{client: f.client}
	return openAIFetcher.FetchModels(ctx, baseURL, apiKey, prepareRequest)
}

// SystemOneModelsFetcher 实现 TypeSafe System One 决策渠道的模型列表获取。
type SystemOneModelsFetcher struct {
	client *http.Client
}

type systemOneModelsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

/**
 * @brief 获取 SystemOne 上游模型目录。
 * @param ctx 请求上下文，取消时中止请求
 * @param baseURL 上游基础地址
 * @param apiKey 上游 API Key
 * @param prepareRequest 可选请求改写，在默认头和认证头设置后调用
 * @return 上游返回的模型 ID，允许为空
 * @return 请求或解析失败时返回错误；HTTP 失败可通过 errors.As 取得 ModelsHTTPError
 */
func (f *SystemOneModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string, prepareRequest func(*http.Request)) ([]string, error) {
	// System One Models API: https://docs.typesafe.ai/models
	// 与决策调用一致：基础 URL 已以 /v1 结尾时只追加 /models。
	endpoint := strings.TrimRight(strings.TrimSuffix(baseURL, "#"), "/")
	if strings.HasSuffix(endpoint, "/v1") {
		endpoint += "/models"
	} else {
		endpoint += "/v1/models"
	}

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	body, err := doHTTPRequest(f.client, req, prepareRequest)
	if err != nil {
		return nil, err
	}

	var result systemOneModelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	models := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		if m.Name != "" {
			models = append(models, m.Name)
		}
	}
	return models, nil
}

// ============================================================
// 预设模型列表（用于官方无Models API的渠道）
// ============================================================

/**
 * @brief 保留尚需静态目录的协议模型；Anthropic 通过上游接口发现。
 */
var predefinedModelSets = map[string][]string{
	ProtocolCodex: {
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4.1-preview",
		"gpt-4o",
		"gpt-4o-mini",
		"gpt-4o-mini-2024-07-18",
		"gpt-4-turbo",
		"gpt-4",
		"gpt-3.5-turbo",
	},
}

// PredefinedModels 返回给定协议的预设模型列表。
func PredefinedModels(upstreamProtocol string) []string {
	ct := NormalizeProtocol(upstreamProtocol)
	models, ok := predefinedModelSets[ct]
	if !ok {
		return nil
	}
	result := make([]string, len(models))
	copy(result, models)
	return result
}
