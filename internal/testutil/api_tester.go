package testutil

import (
	"net/http"
	"strings"

	"ccLoad/internal/model"

	"github.com/bytedance/sonic"
)

// ChannelTester 定义不同渠道类型的测试协议（OCP：新增类型无需修改调用方）
type ChannelTester interface {
	// Build 构造完整请求：URL、基础请求头、请求体
	// apiKey: 实际使用的API Key字符串（由调用方从数据库查询）
	Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (fullURL string, headers http.Header, body []byte, err error)
	// Parse 解析响应体，返回通用结果字段（如 response_text、usage、api_response/api_error/raw_response）
	Parse(statusCode int, respBody []byte) map[string]any
}

// === 泛型类型安全工具函数 ===

// getTypedValue 从map中安全获取指定类型的值（消除类型断言嵌套）
func getTypedValue[T any](m map[string]any, key string) (T, bool) {
	var zero T
	v, ok := m[key]
	if !ok {
		return zero, false
	}
	typed, ok := v.(T)
	return typed, ok
}

// getSliceItem 从切片中安全获取指定索引的指定类型元素
func getSliceItem[T any](slice []any, index int) (T, bool) {
	var zero T
	if index < 0 || index >= len(slice) {
		return zero, false
	}
	typed, ok := slice[index].(T)
	return typed, ok
}

// CodexTester 兼容 Codex 风格（渠道类型: codex）
type CodexTester struct{}

func (t *CodexTester) Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (string, http.Header, []byte, error) {
	testContent := req.Content
	if strings.TrimSpace(testContent) == "" {
		testContent = "test"
	}

	msg := map[string]any{
		"model":        req.Model,
		"stream":       req.Stream,
		"instructions": "You are Codex, based on GPT-5. You are running as a coding agent in the Codex CLI on a user's computer.",
		"input": []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{
						"type": "input_text",
						"text": testContent,
					},
				},
			},
		},
	}

	body, err := sonic.Marshal(msg)
	if err != nil {
		return "", nil, nil, err
	}

	baseURL := strings.TrimRight(cfg.URL, "/")
	fullURL := baseURL + "/v1/responses"

	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", "codex_cli_rs/0.41.0 (Mac OS 26.0.0; arm64) iTerm.app/3.6.1")
	h.Set("Openai-Beta", "responses=experimental")
	h.Set("Originator", "codex_cli_rs")
	if req.Stream {
		h.Set("Accept", "text/event-stream")
	}

	return fullURL, h, body, nil
}

// extractCodexResponseText 从Codex响应中提取文本（消除6层嵌套）
func extractCodexResponseText(apiResp map[string]any) (string, bool) {
	output, ok := getTypedValue[[]any](apiResp, "output")
	if !ok {
		return "", false
	}

	for _, item := range output {
		outputItem, ok := item.(map[string]any)
		if !ok {
			continue
		}

		outputType, ok := getTypedValue[string](outputItem, "type")
		if !ok || outputType != "message" {
			continue
		}

		content, ok := getTypedValue[[]any](outputItem, "content")
		if !ok || len(content) == 0 {
			continue
		}

		textBlock, ok := getSliceItem[map[string]any](content, 0)
		if !ok {
			continue
		}

		text, ok := getTypedValue[string](textBlock, "text")
		if ok {
			return text, true
		}
	}
	return "", false
}

func (t *CodexTester) Parse(statusCode int, respBody []byte) map[string]any {
	out := map[string]any{}
	var apiResp map[string]any
	if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
		// 提取文本（使用辅助函数）
		if text, ok := extractCodexResponseText(apiResp); ok {
			out["response_text"] = text
		}

		// 提取usage（使用泛型工具）
		if usage, ok := getTypedValue[map[string]any](apiResp, "usage"); ok {
			out["usage"] = usage
		}

		out["api_response"] = apiResp
		return out
	}
	out["raw_response"] = string(respBody)
	return out
}

// OpenAITester 标准OpenAI API格式（渠道类型: openai）
type OpenAITester struct{}

func (t *OpenAITester) Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (string, http.Header, []byte, error) {
	testContent := req.Content
	if strings.TrimSpace(testContent) == "" {
		testContent = "test"
	}

	// 标准OpenAI Chat Completions格式
	msg := map[string]any{
		"model": req.Model,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": testContent,
			},
		},
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
	}

	body, err := sonic.Marshal(msg)
	if err != nil {
		return "", nil, nil, err
	}

	// 使用标准OpenAI API路径
	baseURL := strings.TrimRight(cfg.URL, "/")
	fullURL := baseURL + "/v1/chat/completions"

	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	if req.Stream {
		h.Set("Accept", "text/event-stream")
	}

	return fullURL, h, body, nil
}

func (t *OpenAITester) Parse(statusCode int, respBody []byte) map[string]any {
	out := map[string]any{}
	var apiResp map[string]any
	if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
		// 提取choices[0].message.content
		if choices, ok := getTypedValue[[]any](apiResp, "choices"); ok && len(choices) > 0 {
			if choice, ok := getSliceItem[map[string]any](choices, 0); ok {
				if message, ok := getTypedValue[map[string]any](choice, "message"); ok {
					if content, ok := getTypedValue[string](message, "content"); ok {
						out["response_text"] = content
					}
				}
			}
		}

		// 提取usage
		if usage, ok := getTypedValue[map[string]any](apiResp, "usage"); ok {
			out["usage"] = usage
		}

		out["api_response"] = apiResp
		return out
	}
	out["raw_response"] = string(respBody)
	return out
}

// GeminiTester 实现 Google Gemini 测试协议
type GeminiTester struct{}

func (t *GeminiTester) Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (string, http.Header, []byte, error) {
	testContent := req.Content
	if strings.TrimSpace(testContent) == "" {
		testContent = "test"
	}

	msg := map[string]any{
		"contents": []map[string]any{
			{
				"parts": []map[string]any{
					{
						"text": testContent,
					},
				},
			},
		},
	}

	body, err := sonic.Marshal(msg)
	if err != nil {
		return "", nil, nil, err
	}

	baseURL := strings.TrimRight(cfg.URL, "/")
	// Gemini API 路径格式: /v1beta/models/{model}:generateContent
	fullURL := baseURL + "/v1beta/models/" + req.Model + ":generateContent"

	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("x-goog-api-key", apiKey)

	return fullURL, h, body, nil
}

// extractGeminiResponseText 从Gemini响应中提取文本（消除5层嵌套）
func extractGeminiResponseText(apiResp map[string]any) (string, bool) {
	candidates, ok := getTypedValue[[]any](apiResp, "candidates")
	if !ok || len(candidates) == 0 {
		return "", false
	}

	candidate, ok := getSliceItem[map[string]any](candidates, 0)
	if !ok {
		return "", false
	}

	content, ok := getTypedValue[map[string]any](candidate, "content")
	if !ok {
		return "", false
	}

	parts, ok := getTypedValue[[]any](content, "parts")
	if !ok || len(parts) == 0 {
		return "", false
	}

	part, ok := getSliceItem[map[string]any](parts, 0)
	if !ok {
		return "", false
	}

	text, ok := getTypedValue[string](part, "text")
	return text, ok
}

func (t *GeminiTester) Parse(statusCode int, respBody []byte) map[string]any {
	out := map[string]any{}
	var apiResp map[string]any
	if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
		// 提取文本响应（使用辅助函数）
		if text, ok := extractGeminiResponseText(apiResp); ok {
			out["response_text"] = text
		}

		// 提取usage信息（使用泛型工具）
		if usageMetadata, ok := getTypedValue[map[string]any](apiResp, "usageMetadata"); ok {
			out["usage"] = usageMetadata
		}

		out["api_response"] = apiResp
		return out
	}
	out["raw_response"] = string(respBody)
	return out
}

// AnthropicTester 实现 Anthropic 测试协议
type AnthropicTester struct{}

func (t *AnthropicTester) Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (string, http.Header, []byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	testContent := req.Content
	if strings.TrimSpace(testContent) == "" {
		testContent = "test"
	}

	msg := map[string]any{
		"system": []map[string]any{
			{
				"type":          "text",
				"text":          "You are Claude Code, Anthropic's official CLI for Claude.",
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		},
		"stream": req.Stream,
		"messages": []map[string]any{
			{
				"content": []map[string]any{
					{
						"type": "text",
						"text": "<system-reminder>\nThis is a reminder that your todo list is currently empty. DO NOT mention this to the user explicitly because they are already aware. If you are working on tasks that would benefit from a todo list please use the TodoWrite tool to create one. If not, please feel free to ignore. Again do not mention this message to the user.\n</system-reminder>",
					},
					{
						"type":          "text",
						"text":          testContent,
						"cache_control": map[string]any{"type": "ephemeral"},
					},
				},
				"role": "user",
			},
		},
		"model":      req.Model,
		"max_tokens": maxTokens,
		"metadata":   map[string]any{"user_id": "test"},
	}

	body, err := sonic.Marshal(msg)
	if err != nil {
		return "", nil, nil, err
	}

	baseURL := strings.TrimRight(cfg.URL, "/")
	fullURL := baseURL + "/v1/messages?beta=true"

	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", "claude-cli/1.0.110 (external, cli)")
	h.Set("x-app", "cli")
	h.Set("anthropic-version", "2023-06-01")
	if req.Stream {
		h.Set("Accept", "text/event-stream")
	}

	return fullURL, h, body, nil
}

// extractAnthropicResponseText 从Anthropic响应中提取文本（消除3层嵌套）
func extractAnthropicResponseText(apiResp map[string]any) (string, bool) {
	content, ok := getTypedValue[[]any](apiResp, "content")
	if !ok || len(content) == 0 {
		return "", false
	}

	textBlock, ok := getSliceItem[map[string]any](content, 0)
	if !ok {
		return "", false
	}

	text, ok := getTypedValue[string](textBlock, "text")
	return text, ok
}

func (t *AnthropicTester) Parse(statusCode int, respBody []byte) map[string]any {
	out := map[string]any{}
	var apiResp map[string]any
	if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
		// 提取文本响应（使用辅助函数）
		if text, ok := extractAnthropicResponseText(apiResp); ok {
			out["response_text"] = text
		}

		out["api_response"] = apiResp
		return out
	}
	out["raw_response"] = string(respBody)
	return out
}
