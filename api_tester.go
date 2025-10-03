package main

import (
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
)

// ChannelTester 定义不同渠道类型的测试协议（OCP：新增类型无需修改调用方）
type ChannelTester interface {
	// Build 构造完整请求：URL、基础请求头、请求体
	Build(cfg *Config, req *TestChannelRequest) (fullURL string, headers http.Header, body []byte, err error)
	// Parse 解析响应体，返回通用结果字段（如 response_text、usage、api_response/api_error/raw_response）
	Parse(statusCode int, respBody []byte) map[string]any
}

// OpenAITester 兼容 Codex 风格（归一化为 openai）
type OpenAITester struct{}

func (t *OpenAITester) Build(cfg *Config, req *TestChannelRequest) (string, http.Header, []byte, error) {
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
	h.Set("Authorization", "Bearer "+cfg.APIKey)
	h.Set("User-Agent", "codex_cli_rs/0.41.0 (Mac OS 26.0.0; arm64) iTerm.app/3.6.1")
	h.Set("Openai-Beta", "responses=experimental")
	h.Set("Originator", "codex_cli_rs")

	return fullURL, h, body, nil
}

func (t *OpenAITester) Parse(statusCode int, respBody []byte) map[string]any {
	out := map[string]any{}
	var apiResp map[string]any
	if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
		// 提取文本
		if output, ok := apiResp["output"].([]any); ok {
			for _, item := range output {
				if outputItem, ok := item.(map[string]any); ok {
					if outputType, ok := outputItem["type"].(string); ok && outputType == "message" {
						if content, ok := outputItem["content"].([]any); ok && len(content) > 0 {
							if textBlock, ok := content[0].(map[string]any); ok {
								if text, ok := textBlock["text"].(string); ok {
									out["response_text"] = text
									break
								}
							}
						}
					}
				}
			}
		}
		// usage
		if usage, ok := apiResp["usage"].(map[string]any); ok {
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

func (t *GeminiTester) Build(cfg *Config, req *TestChannelRequest) (string, http.Header, []byte, error) {
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
	h.Set("x-goog-api-key", cfg.APIKey)

	return fullURL, h, body, nil
}

func (t *GeminiTester) Parse(statusCode int, respBody []byte) map[string]any {
	out := map[string]any{}
	var apiResp map[string]any
	if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
		// 提取文本响应
		if candidates, ok := apiResp["candidates"].([]any); ok && len(candidates) > 0 {
			if candidate, ok := candidates[0].(map[string]any); ok {
				if content, ok := candidate["content"].(map[string]any); ok {
					if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
						if part, ok := parts[0].(map[string]any); ok {
							if text, ok := part["text"].(string); ok {
								out["response_text"] = text
							}
						}
					}
				}
			}
		}
		// usage 信息
		if usageMetadata, ok := apiResp["usageMetadata"].(map[string]any); ok {
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

func (t *AnthropicTester) Build(cfg *Config, req *TestChannelRequest) (string, http.Header, []byte, error) {
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
	h.Set("Authorization", "Bearer "+cfg.APIKey)
	h.Set("User-Agent", "claude-cli/1.0.110 (external, cli)")
	h.Set("x-app", "cli")
	h.Set("anthropic-version", "2023-06-01")

	return fullURL, h, body, nil
}

func (t *AnthropicTester) Parse(statusCode int, respBody []byte) map[string]any {
	out := map[string]any{}
	var apiResp map[string]any
	if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
		if content, ok := apiResp["content"].([]any); ok && len(content) > 0 {
			if textBlock, ok := content[0].(map[string]any); ok {
				if text, ok := textBlock["text"].(string); ok {
					out["response_text"] = text
				}
			}
		}
		out["api_response"] = apiResp
		return out
	}
	out["raw_response"] = string(respBody)
	return out
}
