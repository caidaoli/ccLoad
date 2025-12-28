package testutil

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

func parseAPIResponse(respBody []byte, extractText func(map[string]any) (string, bool), usageKey string) map[string]any {
	out := map[string]any{}
	var apiResp map[string]any
	if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
		if extractText != nil {
			if text, ok := extractText(apiResp); ok {
				out["response_text"] = text
			}
		}
		if usageKey != "" {
			if usage, ok := getTypedValue[map[string]any](apiResp, usageKey); ok {
				out["usage"] = usage
			}
		}
		out["api_response"] = apiResp
		return out
	}
	out["raw_response"] = string(respBody)
	return out
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
	return parseAPIResponse(respBody, extractCodexResponseText, "usage")
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
		"stream": req.Stream,
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
	return parseAPIResponse(respBody, extractGeminiResponseText, "usageMetadata")
}

// AnthropicTester 实现 Anthropic 测试协议
type AnthropicTester struct{}

func newClaudeCLIUserID() string {
	// 格式示例：
	// user_<64hex>_account__session_<uuid>
	userBytes := make([]byte, 32)
	if _, err := rand.Read(userBytes); err != nil {
		return "user_0000000000000000000000000000000000000000000000000000000000000000_account__session_00000000-0000-0000-0000-000000000000"
	}

	uuidBytes := make([]byte, 16)
	if _, err := rand.Read(uuidBytes); err != nil {
		return "user_" + hex.EncodeToString(userBytes) + "_account__session_00000000-0000-0000-0000-000000000000"
	}

	// RFC 4122 UUID v4
	uuidBytes[6] = (uuidBytes[6] & 0x0f) | 0x40
	uuidBytes[8] = (uuidBytes[8] & 0x3f) | 0x80
	u := fmt.Sprintf("%x-%x-%x-%x-%x", uuidBytes[0:4], uuidBytes[4:6], uuidBytes[6:8], uuidBytes[8:10], uuidBytes[10:16])

	return "user_" + hex.EncodeToString(userBytes) + "_account__session_" + u
}

func (t *AnthropicTester) Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (string, http.Header, []byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 32000
	}
	testContent := req.Content

	msg := map[string]any{
		"model": req.Model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "text",
						"text": testContent,
					},
				},
			},
		},
		"system": []map[string]any{
			{
				"type": "text",
				"text": "You are Claude Code, Anthropic's official CLI for Claude.",
			},
		},
		"tools":      []any{},
		"metadata":   map[string]any{"user_id": newClaudeCLIUserID()},
		"max_tokens": maxTokens,
		"stream":     req.Stream,
	}

	body, err := sonic.Marshal(msg)
	if err != nil {
		return "", nil, nil, err
	}

	baseURL := strings.TrimRight(cfg.URL, "/")
	fullURL := baseURL + "/v1/messages?beta=true"

	h := make(http.Header)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	// Claude Code CLI headers
	h.Set("User-Agent", "claude-cli/2.0.76 (external, cli)")
	h.Set("x-app", "cli")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("anthropic-beta", "interleaved-thinking-2025-05-14,advanced-tool-use-2025-11-20")
	h.Set("anthropic-dangerous-direct-browser-access", "true")
	// x-stainless-* headers
	h.Set("x-stainless-arch", "arm64")
	h.Set("x-stainless-lang", "js")
	h.Set("x-stainless-os", "MacOS")
	h.Set("x-stainless-package-version", "0.70.0")
	h.Set("x-stainless-retry-count", "0")
	h.Set("x-stainless-runtime", "node")
	h.Set("x-stainless-runtime-version", "v24.3.0")
	h.Set("x-stainless-timeout", "600")
	if req.Stream {
		h.Set("x-stainless-helper-method", "stream")
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
