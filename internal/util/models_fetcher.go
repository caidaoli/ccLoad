package util

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ModelsFetcher 模型列表获取器接口
// 不同渠道类型有不同的API实现
type ModelsFetcher interface {
	FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error)
}

// NewModelsFetcher 根据渠道类型创建对应的Fetcher
// [FIX] P2-9: 删除口号式注释，代码已经够清晰
func NewModelsFetcher(channelType string) ModelsFetcher {
	switch NormalizeChannelType(channelType) {
	case ChannelTypeAnthropic:
		return &AnthropicModelsFetcher{}
	case ChannelTypeOpenAI:
		return &OpenAIModelsFetcher{}
	case ChannelTypeGemini:
		return &GeminiModelsFetcher{}
	case ChannelTypeCodex:
		return &CodexModelsFetcher{}
	default:
		return &AnthropicModelsFetcher{} // 默认使用Anthropic格式
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

// doHTTPRequest 执行HTTP GET请求并返回响应体
// 封装公共的HTTP请求、错误处理、超时控制逻辑
func doHTTPRequest(client *http.Client, req *http.Request) ([]byte, error) {
	if client == nil {
		client = defaultModelsFetcherClient
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
		// [INFO] 修复：区分4xx和5xx错误，便于上层返回正确的HTTP状态码
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fmt.Errorf("上游配置错误 (HTTP %d): %s", resp.StatusCode, string(body))
		}
		return nil, fmt.Errorf("上游服务器错误 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// BuildModelsRequest builds the protocol-specific model catalog request.
// Callers that need channel-level transport and rate limiting can execute the
// returned request themselves instead of using a ModelsFetcher directly.
func BuildModelsRequest(ctx context.Context, channelType, baseURL, apiKey string) (*http.Request, error) {
	switch NormalizeChannelType(channelType) {
	case ChannelTypeOpenAI, ChannelTypeCodex:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
		if err != nil {
			return nil, fmt.Errorf("创建请求失败: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		return req, nil
	case ChannelTypeGemini:
		endpoint, err := url.Parse(baseURL + "/v1beta/models")
		if err != nil {
			return nil, fmt.Errorf("解析请求 URL 失败: %w", err)
		}
		query := endpoint.Query()
		query.Set("key", apiKey)
		endpoint.RawQuery = query.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("创建请求失败: %w", err)
		}
		return req, nil
	default:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
		if err != nil {
			return nil, fmt.Errorf("创建请求失败: %w", err)
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return req, nil
	}
}

// ParseModelsResponse parses the protocol-specific model catalog response.
func ParseModelsResponse(channelType string, body []byte) ([]string, error) {
	switch NormalizeChannelType(channelType) {
	case ChannelTypeOpenAI, ChannelTypeCodex:
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
	case ChannelTypeGemini:
		var result geminiModelsResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("解析响应失败: %w", err)
		}

		models := make([]string, 0, len(result.Models))
		for _, m := range result.Models {
			if len(m.Name) > 7 && m.Name[:7] == "models/" {
				models = append(models, m.Name[7:])
			} else {
				models = append(models, m.Name)
			}
		}
		return models, nil
	default:
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
}

func fetchModelsWithClient(ctx context.Context, channelType, baseURL, apiKey string, client *http.Client) ([]string, error) {
	req, err := BuildModelsRequest(ctx, channelType, baseURL, apiKey)
	if err != nil {
		return nil, err
	}

	body, err := doHTTPRequest(client, req)
	if err != nil {
		return nil, err
	}
	return ParseModelsResponse(channelType, body)
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

// FetchModels 从 Anthropic API 获取可用模型列表。
func (f *AnthropicModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error) {
	return fetchModelsWithClient(ctx, ChannelTypeAnthropic, baseURL, apiKey, f.client)
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

// FetchModels 从 OpenAI API 获取可用模型列表。
func (f *OpenAIModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error) {
	return fetchModelsWithClient(ctx, ChannelTypeOpenAI, baseURL, apiKey, f.client)
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

// FetchModels 从 Gemini API 获取可用模型列表。
func (f *GeminiModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error) {
	return fetchModelsWithClient(ctx, ChannelTypeGemini, baseURL, apiKey, f.client)
}

// CodexModelsFetcher 实现 Codex 渠道的模型列表获取。
type CodexModelsFetcher struct {
	client *http.Client
}

// FetchModels 从 Codex API 获取可用模型列表（使用 OpenAI 兼容接口）。
func (f *CodexModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error) {
	return fetchModelsWithClient(ctx, ChannelTypeCodex, baseURL, apiKey, f.client)
}

// ============================================================
// 预设模型列表（用于官方无Models API的渠道）
// ============================================================

var predefinedModelSets = map[string][]string{
	ChannelTypeAnthropic: {
		"claude-3-5-sonnet-20241022",
		"claude-3-5-sonnet-latest",
		"claude-3-5-haiku-20241022",
		"claude-3-5-haiku-latest",
		"claude-3-opus-20240229",
		"claude-3-opus-latest",
		"claude-3-sonnet-20240229",
		"claude-3-sonnet-latest",
		"claude-3-haiku-20240307",
		"claude-3-haiku-latest",
		"claude-2.1",
		"claude-2.0",
		"claude-instant-1.2",
	},
	ChannelTypeCodex: {
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

// PredefinedModels 返回给定渠道类型的预设模型列表
func PredefinedModels(channelType string) []string {
	ct := NormalizeChannelType(channelType)
	models, ok := predefinedModelSets[ct]
	if !ok {
		return nil
	}
	result := make([]string, len(models))
	copy(result, models)
	return result
}
