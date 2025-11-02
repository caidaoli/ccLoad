package util

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ModelsFetcher 模型列表获取器接口（Strategy Pattern - 策略模式）
// 不同渠道类型有不同的API实现
type ModelsFetcher interface {
	FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error)
}

// ModelsFetcherFactory 工厂方法（Factory Pattern - 工厂模式）
// 根据渠道类型创建对应的Fetcher
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
// 公共辅助函数 (DRY原则 - 避免重复HTTP请求逻辑)
// ============================================================

// doHTTPRequest 执行HTTP GET请求并返回响应体
// 封装公共的HTTP请求、错误处理、超时控制逻辑
func doHTTPRequest(req *http.Request) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API返回错误 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// ============================================================
// Anthropic/Claude Code 渠道适配器
// ============================================================
type AnthropicModelsFetcher struct{}

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

func (f *AnthropicModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error) {
	// Anthropic Models API: https://docs.claude.com/en/api/models-list
	endpoint := baseURL + "/v1/models"

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// Anthropic要求的请求头
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	// 使用公共HTTP请求函数 (ctx已包含在req中)
	body, err := doHTTPRequest(req)
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

// ============================================================
// OpenAI 渠道适配器
// ============================================================
type OpenAIModelsFetcher struct{}

type openAIModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (f *OpenAIModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error) {
	// OpenAI Models API: https://platform.openai.com/docs/api-reference/models/list
	endpoint := baseURL + "/v1/models"

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)

	// 使用公共HTTP请求函数 (ctx已包含在req中)
	body, err := doHTTPRequest(req)
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

// ============================================================
// Google Gemini 渠道适配器
// ============================================================
type GeminiModelsFetcher struct{}

type geminiModelsResponse struct {
	Models []struct {
		Name string `json:"name"` // 格式: "models/gemini-1.5-flash"
	} `json:"models"`
}

func (f *GeminiModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error) {
	// Gemini Models API: https://ai.google.dev/api/rest/v1beta/models/list
	endpoint := baseURL + "/v1beta/models?key=" + apiKey

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// 使用公共HTTP请求函数 (ctx已包含在req中)
	body, err := doHTTPRequest(req)
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

// ============================================================
// Codex 渠道适配器
// ============================================================
type CodexModelsFetcher struct{}

func (f *CodexModelsFetcher) FetchModels(ctx context.Context, baseURL string, apiKey string) ([]string, error) {
	// Codex使用与OpenAI相同的标准接口 /v1/models
	openAIFetcher := &OpenAIModelsFetcher{}
	return openAIFetcher.FetchModels(ctx, baseURL, apiKey)
}
