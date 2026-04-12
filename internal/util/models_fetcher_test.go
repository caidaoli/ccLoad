package util

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestModelsFetcherClient(fn roundTripFunc) *http.Client {
	return &http.Client{Transport: fn}
}

func newJSONResponse(status int, body string) *http.Response {
	resp := &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	resp.Header.Set("Content-Type", "application/json")
	return resp
}

// ============================================================
// 模型获取器工厂测试
// ============================================================

func TestNewModelsFetcher(t *testing.T) {
	tests := []struct {
		name         string
		channelType  string
		expectedType string
	}{
		{"Anthropic渠道", "anthropic", "*util.AnthropicModelsFetcher"},
		{"OpenAI渠道", "openai", "*util.OpenAIModelsFetcher"},
		{"Gemini渠道", "gemini", "*util.GeminiModelsFetcher"},
		{"Codex渠道", "codex", "*util.CodexModelsFetcher"},
		{"空值默认", "", "*util.AnthropicModelsFetcher"},
		{"未知类型默认", "unknown", "*util.AnthropicModelsFetcher"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetcher := NewModelsFetcher(tt.channelType)
			if fetcher == nil {
				t.Fatal("fetcher不应为nil")
			}
			// 类型断言验证
			typeName := getTypeName(fetcher)
			if typeName != tt.expectedType {
				t.Errorf("期望类型 %s, 实际类型 %s", tt.expectedType, typeName)
			}
		})
	}
}

// ============================================================
// Anthropic 模型获取器测试
// ============================================================

func TestAnthropicModelsFetcher(t *testing.T) {
	responseBody, err := json.Marshal(map[string]any{
		"data": []map[string]any{
			{
				"id":           "claude-3-5-sonnet-20241022",
				"display_name": "Claude 3.5 Sonnet",
				"type":         "model",
				"created_at":   "2024-10-22T00:00:00Z",
			},
			{
				"id":           "claude-3-opus-20240229",
				"display_name": "Claude 3 Opus",
				"type":         "model",
				"created_at":   "2024-02-29T00:00:00Z",
			},
			{
				"id":           "claude-3-sonnet-20240229",
				"display_name": "Claude 3 Sonnet",
				"type":         "model",
				"created_at":   "2024-02-29T00:00:00Z",
			},
		},
		"has_more": false,
		"first_id": "claude-3-5-sonnet-20241022",
		"last_id":  "claude-3-sonnet-20240229",
	})
	if err != nil {
		t.Fatalf("marshal 响应失败: %v", err)
	}

	fetcher := &AnthropicModelsFetcher{
		client: newTestModelsFetcherClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/v1/models" {
				t.Fatalf("期望路径 /v1/models, 实际 %s", r.URL.Path)
			}
			if r.Header.Get("x-api-key") == "" {
				return newJSONResponse(http.StatusUnauthorized, `{"error":"missing x-api-key"}`), nil
			}
			if r.Header.Get("anthropic-version") == "" {
				return newJSONResponse(http.StatusBadRequest, `{"error":"missing anthropic-version"}`), nil
			}
			return newJSONResponse(http.StatusOK, string(responseBody)), nil
		}),
	}
	ctx := context.Background()

	models, err := fetcher.FetchModels(ctx, "https://anthropic.test", "test-api-key")
	if err != nil {
		t.Fatalf("获取失败: %v", err)
	}

	if len(models) == 0 {
		t.Fatal("Anthropic应返回模型列表")
	}

	// 验证包含核心模型
	expectedModels := []string{
		"claude-3-5-sonnet-20241022",
		"claude-3-opus-20240229",
		"claude-3-sonnet-20240229",
	}

	if len(models) != len(expectedModels) {
		t.Errorf("期望 %d 个模型, 实际获取 %d 个", len(expectedModels), len(models))
	}

	for _, expected := range expectedModels {
		found := false
		for _, model := range models {
			if model == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("未找到期望的模型: %s", expected)
		}
	}
}

// ============================================================
// OpenAI 模型获取器测试
// ============================================================

func TestOpenAIModelsFetcher(t *testing.T) {
	fetcher := &OpenAIModelsFetcher{
		client: newTestModelsFetcherClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/v1/models" {
				t.Fatalf("期望路径 /v1/models, 实际 %s", r.URL.Path)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
				t.Fatalf("期望Authorization: Bearer test-key, 实际: %s", auth)
			}
			return newJSONResponse(http.StatusOK, `{
				"data": [
					{"id": "gpt-4o"},
					{"id": "gpt-4-turbo"},
					{"id": "gpt-3.5-turbo"}
				]
			}`), nil
		}),
	}
	ctx := context.Background()

	models, err := fetcher.FetchModels(ctx, "https://openai.test", "test-key")
	if err != nil {
		t.Fatalf("获取失败: %v", err)
	}

	expectedCount := 3
	if len(models) != expectedCount {
		t.Errorf("期望 %d 个模型, 实际 %d 个", expectedCount, len(models))
	}

	// 验证模型ID
	expectedModels := map[string]bool{
		"gpt-4o":        true,
		"gpt-4-turbo":   true,
		"gpt-3.5-turbo": true,
	}

	for _, model := range models {
		if !expectedModels[model] {
			t.Errorf("意外的模型: %s", model)
		}
	}
}

func TestOpenAIModelsFetcher_APIError(t *testing.T) {
	fetcher := &OpenAIModelsFetcher{
		client: newTestModelsFetcherClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/v1/models" {
				t.Fatalf("期望路径 /v1/models, 实际 %s", r.URL.Path)
			}
			return newJSONResponse(http.StatusUnauthorized, `{"error": {"message": "Invalid API key"}}`), nil
		}),
	}
	ctx := context.Background()

	_, err := fetcher.FetchModels(ctx, "https://openai.test", "invalid-key")
	if err == nil {
		t.Fatal("期望返回错误，但成功了")
	}

	// 验证错误信息包含状态码
	if !containsString(err.Error(), "401") {
		t.Errorf("错误信息应包含HTTP 401: %v", err)
	}
}

// ============================================================
// Gemini 模型获取器测试
// ============================================================

func TestGeminiModelsFetcher(t *testing.T) {
	fetcher := &GeminiModelsFetcher{
		client: newTestModelsFetcherClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/v1beta/models" {
				t.Fatalf("期望路径 /v1beta/models, 实际 %s", r.URL.Path)
			}
			if r.URL.Query().Get("key") != "test-key" {
				t.Fatalf("URL应包含API key参数, 实际 query=%s", r.URL.RawQuery)
			}
			return newJSONResponse(http.StatusOK, `{
				"models": [
					{"name": "models/gemini-1.5-flash"},
					{"name": "models/gemini-1.5-pro"},
					{"name": "models/gemini-1.0-pro"}
				]
			}`), nil
		}),
	}
	ctx := context.Background()

	models, err := fetcher.FetchModels(ctx, "https://gemini.test", "test-key")
	if err != nil {
		t.Fatalf("获取失败: %v", err)
	}

	expectedCount := 3
	if len(models) != expectedCount {
		t.Errorf("期望 %d 个模型, 实际 %d 个", expectedCount, len(models))
	}

	// 验证模型名称已去除"models/"前缀
	expectedModels := map[string]bool{
		"gemini-1.5-flash": true,
		"gemini-1.5-pro":   true,
		"gemini-1.0-pro":   true,
	}

	for _, model := range models {
		if !expectedModels[model] {
			t.Errorf("意外的模型: %s", model)
		}
		// 确保没有"models/"前缀
		if containsString(model, "models/") {
			t.Errorf("模型名称不应包含'models/'前缀: %s", model)
		}
	}
}

// ============================================================
// Codex 模型获取器测试
// ============================================================

func TestCodexModelsFetcher(t *testing.T) {
	responseBody, err := json.Marshal(map[string]any{
		"data": []map[string]any{
			{"id": "gpt-4"},
			{"id": "gpt-3.5-turbo"},
			{"id": "text-davinci-003"},
		},
	})
	if err != nil {
		t.Fatalf("marshal 响应失败: %v", err)
	}

	fetcher := &CodexModelsFetcher{
		client: newTestModelsFetcherClient(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/v1/models" {
				t.Fatalf("期望路径 /v1/models, 实际 %s", r.URL.Path)
			}
			return newJSONResponse(http.StatusOK, string(responseBody)), nil
		}),
	}
	ctx := context.Background()

	models, err := fetcher.FetchModels(ctx, "https://codex.test", "dummy-key")
	if err != nil {
		t.Fatalf("获取失败: %v", err)
	}

	if len(models) == 0 {
		t.Fatal("Codex应返回模型列表")
	}

	// 验证返回的模型
	expectedModels := []string{"gpt-4", "gpt-3.5-turbo", "text-davinci-003"}
	if len(models) != len(expectedModels) {
		t.Errorf("期望 %d 个模型, 实际获取 %d 个", len(expectedModels), len(models))
	}

	for _, expected := range expectedModels {
		found := false
		for _, model := range models {
			if model == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("未找到期望的模型: %s", expected)
		}
	}
}

// ============================================================
// 辅助函数
// ============================================================

func getTypeName(v any) string {
	return fmt.Sprintf("%T", v)
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
