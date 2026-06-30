// Package testutil 提供测试工具和辅助函数
package testutil

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"ccLoad/internal/version"

	"github.com/bytedance/sonic"
)

// patchMessagesInBody 将消息列表注入到已生成的请求体 JSON 中，替换指定字段。
// 用于多轮对话：先用模板生成基础请求体，再按协议替换 messages/contents 字段。
func patchMessagesInBody(body []byte, key string, messages any) ([]byte, error) {
	var obj map[string]any
	if err := sonic.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	obj[key] = messages
	return sonic.Marshal(obj)
}

func parseDataURLImage(dataURL string) (mimeType, data string, ok bool) {
	value := strings.TrimSpace(dataURL)
	if !strings.HasPrefix(value, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(value, "data:")
	parts := strings.SplitN(rest, ",", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	meta := strings.TrimSpace(parts[0])
	if !strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return "", "", false
	}
	mimeType = strings.TrimSpace(strings.TrimSuffix(meta, ";base64"))
	data = strings.TrimSpace(parts[1])
	if mimeType == "" || data == "" {
		return "", "", false
	}
	return mimeType, data, true
}

func chatMessageContentBlocks(msg ChatMessage) []chatContentBlock {
	if len(msg.ContentBlocks) > 0 {
		return msg.ContentBlocks
	}
	switch content := msg.Content.(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			return nil
		}
		return []chatContentBlock{{Type: "text", Text: content}}
	case []chatContentBlock:
		return content
	case []any:
		raw, err := sonic.Marshal(content)
		if err != nil {
			return nil
		}
		var blocks []chatContentBlock
		if err := sonic.Unmarshal(raw, &blocks); err != nil {
			return nil
		}
		return blocks
	default:
		return nil
	}
}

func openAIContentValue(msg ChatMessage) any {
	blocks := chatMessageContentBlocks(msg)
	if len(blocks) == 0 {
		return msg.Content
	}
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return blocks[0].Text
	}
	items := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch strings.TrimSpace(block.Type) {
		case "text":
			items = append(items, map[string]any{"type": "text", "text": block.Text})
		case "image_url":
			if block.ImageURL == nil || strings.TrimSpace(block.ImageURL.URL) == "" {
				continue
			}
			payload := map[string]any{"url": strings.TrimSpace(block.ImageURL.URL)}
			if detail := strings.TrimSpace(block.ImageURL.Detail); detail != "" {
				payload["detail"] = detail
			}
			items = append(items, map[string]any{"type": "image_url", "image_url": payload})
		}
	}
	if len(items) == 0 {
		if text, ok := msg.Content.(string); ok {
			return text
		}
		return ""
	}
	return items
}

func anthropicContentValue(msg ChatMessage) any {
	blocks := chatMessageContentBlocks(msg)
	if len(blocks) == 0 {
		if text, ok := msg.Content.(string); ok {
			return text
		}
		return ""
	}
	items := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch strings.TrimSpace(block.Type) {
		case "text":
			if strings.TrimSpace(block.Text) == "" {
				continue
			}
			items = append(items, map[string]any{"type": "text", "text": block.Text})
		case "image_url":
			if block.ImageURL == nil || strings.TrimSpace(block.ImageURL.URL) == "" {
				continue
			}
			url := strings.TrimSpace(block.ImageURL.URL)
			if mimeType, data, ok := parseDataURLImage(url); ok {
				items = append(items, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": mimeType,
						"data":       data,
					},
				})
				continue
			}
			items = append(items, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "url",
					"media_type": "image/*",
					"url":        url,
				},
			})
		}
	}
	return items
}

func codexContentValue(msg ChatMessage) []map[string]any {
	blocks := chatMessageContentBlocks(msg)
	if len(blocks) == 0 {
		text := ""
		if raw, ok := msg.Content.(string); ok {
			text = raw
		}
		partType := "input_text"
		if strings.TrimSpace(msg.Role) == "assistant" {
			partType = "output_text"
		}
		return []map[string]any{{"type": partType, "text": text}}
	}
	items := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch strings.TrimSpace(block.Type) {
		case "text":
			partType := "input_text"
			if strings.TrimSpace(msg.Role) == "assistant" {
				partType = "output_text"
			}
			items = append(items, map[string]any{"type": partType, "text": block.Text})
		case "image_url":
			if block.ImageURL == nil || strings.TrimSpace(block.ImageURL.URL) == "" {
				continue
			}
			items = append(items, map[string]any{"type": "input_image", "image_url": strings.TrimSpace(block.ImageURL.URL)})
		}
	}
	return items
}

func geminiContentValue(msg ChatMessage) []map[string]any {
	blocks := chatMessageContentBlocks(msg)
	if len(blocks) == 0 {
		text := ""
		if raw, ok := msg.Content.(string); ok {
			text = raw
		}
		return []map[string]any{{"text": text}}
	}
	items := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch strings.TrimSpace(block.Type) {
		case "text":
			items = append(items, map[string]any{"text": block.Text})
		case "image_url":
			if block.ImageURL == nil || strings.TrimSpace(block.ImageURL.URL) == "" {
				continue
			}
			url := strings.TrimSpace(block.ImageURL.URL)
			if mimeType, data, ok := parseDataURLImage(url); ok {
				items = append(items, map[string]any{
					"inlineData": map[string]any{
						"mimeType": mimeType,
						"data":     data,
					},
				})
				continue
			}
			items = append(items, map[string]any{
				"fileData": map[string]any{
					"mimeType": "image/*",
					"fileUri":  url,
				},
			})
		}
	}
	return items
}

// toOpenAIMessages 将通用消息列表转换为 OpenAI/Anthropic 兼容格式。
func toOpenAIMessages(msgs []ChatMessage) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		out[i] = map[string]any{"role": m.Role, "content": openAIContentValue(m)}
	}
	return out
}

func toAnthropicMessages(msgs []ChatMessage) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		out[i] = map[string]any{"role": m.Role, "content": anthropicContentValue(m)}
	}
	return out
}

// toCodexInput 将通用消息列表转换为 Responses API input 格式。
func toCodexInput(msgs []ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			role = "user"
		}
		out = append(out, map[string]any{
			"type":    "message",
			"role":    role,
			"content": codexContentValue(m),
		})
	}
	return out
}

// toGeminiContents 将通用消息列表转换为 Gemini contents 格式
func toGeminiContents(msgs []ChatMessage) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		out[i] = map[string]any{
			"role":  role,
			"parts": geminiContentValue(m),
		}
	}
	return out
}

func patchBodyObject(body []byte, mutate func(map[string]any)) ([]byte, error) {
	var obj map[string]any
	if err := sonic.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	mutate(obj)
	return sonic.Marshal(obj)
}

func hasTestSamplingOptions(req *TestChannelRequest) bool {
	return req != nil && (req.Temperature != nil || req.TopP != nil || req.MaxTokens > 0 || strings.TrimSpace(req.SystemPrompt) != "")
}

func setOpenAILikeSampling(obj map[string]any, req *TestChannelRequest, maxTokensKey string) {
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		obj["top_p"] = *req.TopP
	}
	if req.MaxTokens > 0 {
		obj[maxTokensKey] = req.MaxTokens
	}
}

func appendOpenAISystemPrompt(obj map[string]any, prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	systemMessage := map[string]any{"role": "system", "content": prompt}
	messages, _ := obj["messages"].([]any)
	obj["messages"] = append([]any{systemMessage}, messages...)
}

func prependCodexDeveloperPrompt(obj map[string]any, prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	developerMessage := map[string]any{"role": "developer", "content": prompt}
	input, _ := obj["input"].([]any)
	obj["input"] = append([]any{developerMessage}, input...)
}

func setGeminiGenerationOption(generationConfig map[string]any, key string, value any) {
	if value != nil {
		generationConfig[key] = value
	}
}

func applyGeminiSamplingAndSystemPrompt(obj map[string]any, req *TestChannelRequest) {
	if req.Temperature != nil || req.TopP != nil || req.MaxTokens > 0 {
		generationConfig, _ := obj["generationConfig"].(map[string]any)
		if generationConfig == nil {
			generationConfig = map[string]any{}
		}
		if req.Temperature != nil {
			setGeminiGenerationOption(generationConfig, "temperature", *req.Temperature)
		}
		if req.TopP != nil {
			setGeminiGenerationOption(generationConfig, "topP", *req.TopP)
		}
		if req.MaxTokens > 0 {
			setGeminiGenerationOption(generationConfig, "maxOutputTokens", req.MaxTokens)
		}
		obj["generationConfig"] = generationConfig
	}

	if prompt := strings.TrimSpace(req.SystemPrompt); prompt != "" {
		obj["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": prompt}},
		}
	}
}

func appendAnthropicSystemPrompt(obj map[string]any, prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	system, _ := obj["system"].([]any)
	obj["system"] = append(system, map[string]any{"type": "text", "text": prompt})
}

func normalizeTestThinkingEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "":
		return ""
	case "none", "minimal", "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(effort))
	case "max", "xhigh":
		return "xhigh"
	case "auto":
		return "medium"
	default:
		return "medium"
	}
}

func testThinkingBudget(effort string) int {
	switch normalizeTestThinkingEffort(effort) {
	case "minimal", "low":
		return 1024
	case "medium":
		return 4096
	case "high", "xhigh":
		return 16384
	default:
		return 0
	}
}

func testCodexReasoningEffort(effort string) string {
	switch normalizeTestThinkingEffort(effort) {
	case "minimal", "low":
		return "low"
	case "high":
		return "high"
	case "xhigh":
		return "xhigh"
	case "medium":
		return "medium"
	default:
		return ""
	}
}

func testAnthropicOutputEffort(effort string) string {
	switch normalizeTestThinkingEffort(effort) {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "max"
	default:
		return ""
	}
}

func testGeminiUsesThinkingLevel(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "gemini-3")
}

func testGeminiThinkingLevel(effort string) string {
	switch normalizeTestThinkingEffort(effort) {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh":
		return "high"
	default:
		return ""
	}
}

func appendTestTool(obj map[string]any, tool map[string]any) {
	tools, _ := obj["tools"].([]any)
	obj["tools"] = append(tools, tool)
}

func applyOpenAITestOptions(body []byte, req *TestChannelRequest) ([]byte, error) {
	effort := normalizeTestThinkingEffort(req.ThinkingEffort)
	if effort == "" && !req.BuiltinSearch && !hasTestSamplingOptions(req) {
		return body, nil
	}
	return patchBodyObject(body, func(obj map[string]any) {
		setOpenAILikeSampling(obj, req, "max_tokens")
		appendOpenAISystemPrompt(obj, req.SystemPrompt)
		if effort == "none" {
			delete(obj, "reasoning_effort")
		} else if effort != "" {
			obj["reasoning_effort"] = effort
		}
		if req.BuiltinSearch {
			obj["web_search_options"] = map[string]any{}
		}
	})
}

func applyCodexTestOptions(body []byte, req *TestChannelRequest) ([]byte, error) {
	effort := normalizeTestThinkingEffort(req.ThinkingEffort)
	if effort == "" && !req.BuiltinSearch && !hasTestSamplingOptions(req) {
		return body, nil
	}
	return patchBodyObject(body, func(obj map[string]any) {
		setOpenAILikeSampling(obj, req, "max_output_tokens")
		prependCodexDeveloperPrompt(obj, req.SystemPrompt)
		if effort == "none" {
			delete(obj, "reasoning")
			delete(obj, "include")
		} else if effort != "" {
			obj["reasoning"] = map[string]any{
				"effort":  testCodexReasoningEffort(effort),
				"summary": "auto",
			}
			obj["include"] = []any{"reasoning.encrypted_content"}
		}
		if req.BuiltinSearch {
			appendTestTool(obj, map[string]any{"type": "web_search"})
			obj["tool_choice"] = "auto"
		}
	})
}

func applyGeminiTestOptions(body []byte, req *TestChannelRequest) ([]byte, error) {
	effort := normalizeTestThinkingEffort(req.ThinkingEffort)
	if effort == "" && !req.BuiltinSearch && !hasTestSamplingOptions(req) {
		return body, nil
	}
	return patchBodyObject(body, func(obj map[string]any) {
		applyGeminiSamplingAndSystemPrompt(obj, req)
		if effort != "" {
			generationConfig, _ := obj["generationConfig"].(map[string]any)
			if generationConfig == nil {
				generationConfig = map[string]any{}
			}
			thinkingConfig := map[string]any{}
			if testGeminiUsesThinkingLevel(req.Model) && effort != "none" {
				thinkingConfig["thinkingLevel"] = testGeminiThinkingLevel(effort)
				thinkingConfig["includeThoughts"] = true
			} else {
				thinkingConfig["thinkingBudget"] = testThinkingBudget(effort)
				if effort != "none" {
					thinkingConfig["includeThoughts"] = true
				}
			}
			generationConfig["thinkingConfig"] = thinkingConfig
			obj["generationConfig"] = generationConfig
		}
		if req.BuiltinSearch {
			appendTestTool(obj, map[string]any{"googleSearch": map[string]any{}})
		}
	})
}

func applyAnthropicTestOptions(body []byte, req *TestChannelRequest) ([]byte, error) {
	effort := normalizeTestThinkingEffort(req.ThinkingEffort)
	if effort == "" && !req.BuiltinSearch && !hasTestSamplingOptions(req) {
		return body, nil
	}
	return patchBodyObject(body, func(obj map[string]any) {
		setOpenAILikeSampling(obj, req, "max_tokens")
		appendAnthropicSystemPrompt(obj, req.SystemPrompt)
		if effort == "none" {
			obj["thinking"] = map[string]any{"type": "disabled"}
		} else if effort != "" {
			obj["thinking"] = map[string]any{"type": "adaptive"}
			obj["output_config"] = map[string]any{"effort": testAnthropicOutputEffort(effort)}
		}
		if req.BuiltinSearch {
			appendTestTool(obj, map[string]any{
				"type": "web_search_20250305",
				"name": "web_search",
			})
		}
	})
}

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

func extractStructuredAPIError(apiResp map[string]any) (string, bool) {
	if errInfo, ok := getTypedValue[map[string]any](apiResp, "error"); ok {
		if msg, ok := getTypedValue[string](errInfo, "message"); ok && strings.TrimSpace(msg) != "" {
			return msg, true
		}
		if typeStr, ok := getTypedValue[string](errInfo, "type"); ok && strings.TrimSpace(typeStr) != "" {
			return typeStr, true
		}
		if code, ok := getTypedValue[string](errInfo, "code"); ok && strings.TrimSpace(code) != "" {
			return code, true
		}
		return "上游返回结构化错误", true
	}

	objectType, hasObjectType := getTypedValue[string](apiResp, "object")
	status, hasStatus := getTypedValue[string](apiResp, "status")
	if hasObjectType && objectType == "response" && hasStatus {
		normalizedStatus := strings.ToLower(strings.TrimSpace(status))
		if normalizedStatus != "" && normalizedStatus != "completed" {
			if details, ok := getTypedValue[map[string]any](apiResp, "incomplete_details"); ok {
				if reason, ok := getTypedValue[string](details, "reason"); ok && strings.TrimSpace(reason) != "" {
					return "响应未完成: " + reason, true
				}
			}
			return "响应状态为 " + status, true
		}
	}

	return "", false
}

func finalizeParsedAPIResponse(out map[string]any, apiResp map[string]any) map[string]any {
	out["api_response"] = apiResp
	if errorMsg, ok := extractStructuredAPIError(apiResp); ok {
		out["success"] = false
		out["error"] = errorMsg
		out["api_error"] = apiResp
	}
	return out
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
		return finalizeParsedAPIResponse(out, apiResp)
	}
	out["raw_response"] = string(respBody)
	return out
}

func buildTesterURL(baseURL, endpointSuffix string) string {
	if model.HasExactUpstreamURLMarker(baseURL) {
		return model.StripExactUpstreamURLMarker(baseURL)
	}
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + endpointSuffix
}

// CodexTester 兼容 Codex 风格（渠道类型: codex）
type CodexTester struct{}

// Build 构建 Codex 格式的 API 请求
func (t *CodexTester) Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (string, http.Header, []byte, error) {
	testContent := req.Content
	if strings.TrimSpace(testContent) == "" && len(req.Messages) == 0 {
		testContent = "test"
	}
	sessionID := newTestSessionID()
	turnID := newTestSessionID()
	windowID := sessionID + ":0"
	turnMetadata, err := newCodexTurnMetadata(sessionID, turnID, windowID)
	if err != nil {
		return "", nil, nil, err
	}

	body, err := buildRequestFromTemplate("codex", map[string]any{
		"MODEL":           req.Model,
		"STREAM":          req.Stream,
		"CONTENT":         testContent,
		"SESSION_ID":      sessionID,
		"INSTALLATION_ID": newTestSessionID(),
	})
	if err != nil {
		return "", nil, nil, err
	}

	if len(req.Messages) > 0 {
		body, err = patchMessagesInBody(body, "input", toCodexInput(req.Messages))
		if err != nil {
			return "", nil, nil, err
		}
	}
	body, err = applyCodexTestOptions(body, req)
	if err != nil {
		return "", nil, nil, err
	}

	fullURL := buildTesterURL(cfg.GetURLs()[0], "/v1/responses")

	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("X-Api-Key", apiKey)
	h.Set("User-Agent", "codex-tui/0.137.0 (Mac OS 26.5.1; arm64) iTerm.app/3.7.0beta3 (codex-tui; 0.137.0)")
	h.Set("Originator", "codex-tui")
	h.Set("Session-Id", sessionID)
	h.Set("Thread-Id", sessionID)
	h.Set("X-Client-Request-Id", sessionID)
	h.Set("X-Codex-Beta-Features", "terminal_resize_reflow")
	h.Set("X-Codex-Turn-Metadata", turnMetadata)
	h.Set("X-Codex-Window-Id", windowID)
	if req.Stream {
		h.Set("Accept", "text/event-stream")
	}

	return fullURL, h, body, nil
}

func newCodexTurnMetadata(sessionID, turnID, windowID string) (string, error) {
	payload := map[string]any{
		"session_id":              sessionID,
		"thread_id":               sessionID,
		"thread_source":           "user",
		"turn_id":                 turnID,
		"workspaces":              map[string]any{},
		"sandbox":                 "none",
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
		"request_kind":            "turn",
		"window_id":               windowID,
	}
	data, err := sonic.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal codex turn metadata: %w", err)
	}
	return string(data), nil
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

// Parse 解析 Codex 格式的 API 响应
func (t *CodexTester) Parse(_ int, respBody []byte) map[string]any {
	return parseAPIResponse(respBody, extractCodexResponseText, "usage")
}

// OpenAITester 标准OpenAI API格式（渠道类型: openai）
type OpenAITester struct{}

// Build 构建 OpenAI 格式的 API 请求
func (t *OpenAITester) Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (string, http.Header, []byte, error) {
	testContent := req.Content
	if strings.TrimSpace(testContent) == "" && len(req.Messages) == 0 {
		testContent = "test"
	}
	sessionID := newTestSessionID()

	body, err := buildRequestFromTemplate("openai", map[string]any{
		"MODEL":      req.Model,
		"STREAM":     req.Stream,
		"CONTENT":    testContent,
		"SESSION_ID": sessionID,
	})
	if err != nil {
		return "", nil, nil, err
	}

	if len(req.Messages) > 0 {
		body, err = patchMessagesInBody(body, "messages", toOpenAIMessages(req.Messages))
		if err != nil {
			return "", nil, nil, err
		}
	}
	body, err = applyOpenAITestOptions(body, req)
	if err != nil {
		return "", nil, nil, err
	}

	fullURL := buildTesterURL(cfg.GetURLs()[0], "/v1/chat/completions")

	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("Session_id", sessionID)
	h.Set("User-Agent", version.OutboundUserAgent())
	if req.Stream {
		h.Set("Accept", "text/event-stream")
	}

	return fullURL, h, body, nil
}

// Parse 解析 OpenAI 格式的 API 响应
func (t *OpenAITester) Parse(_ int, respBody []byte) map[string]any {
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

		return finalizeParsedAPIResponse(out, apiResp)
	}
	out["raw_response"] = string(respBody)
	return out
}

// GeminiTester 实现 Google Gemini 测试协议
type GeminiTester struct{}

// Build 构建 Gemini 格式的 API 请求
func (t *GeminiTester) Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (string, http.Header, []byte, error) {
	testContent := req.Content
	if strings.TrimSpace(testContent) == "" && len(req.Messages) == 0 {
		testContent = "test"
	}

	body, err := buildRequestFromTemplate("gemini", map[string]any{
		"CONTENT": testContent,
	})
	if err != nil {
		return "", nil, nil, err
	}

	if len(req.Messages) > 0 {
		body, err = patchMessagesInBody(body, "contents", toGeminiContents(req.Messages))
		if err != nil {
			return "", nil, nil, err
		}
	}
	body, err = applyGeminiTestOptions(body, req)
	if err != nil {
		return "", nil, nil, err
	}

	// Gemini API: 流式用 :streamGenerateContent?alt=sse，非流式用 :generateContent
	action := ":generateContent"
	if req.Stream {
		action = ":streamGenerateContent?alt=sse"
	}
	fullURL := buildTesterURL(cfg.GetURLs()[0], "/v1beta/models/"+req.Model+action)

	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("x-goog-api-key", apiKey)
	h.Set("User-Agent", version.OutboundUserAgent())

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

// Parse 解析 Gemini 格式的 API 响应
func (t *GeminiTester) Parse(_ int, respBody []byte) map[string]any {
	return parseAPIResponse(respBody, extractGeminiResponseText, "usageMetadata")
}

func newTestSessionID() string {
	return util.NewUUIDv4()
}

// AnthropicTester 实现 Anthropic 测试协议
type AnthropicTester struct{}

// newClaudeCLIUserID 生成 Claude CLI 用户ID
func newClaudeCLIUserID() string {
	// Claude Code 真实格式：metadata.user_id 是一个 JSON 字符串
	// 例如：{"device_id":"76efe6...","account_uuid":"","session_id":"ce6c5d34-..."}
	deviceID := make([]byte, 32)
	if _, err := rand.Read(deviceID); err != nil {
		return `{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","account_uuid":"","session_id":"00000000-0000-0000-0000-000000000000"}`
	}

	return fmt.Sprintf(`{"device_id":"%s","account_uuid":"","session_id":"%s"}`, hex.EncodeToString(deviceID), newTestSessionID())
}

// Build 构建 Anthropic 格式的 API 请求
func (t *AnthropicTester) Build(cfg *model.Config, apiKey string, req *TestChannelRequest) (string, http.Header, []byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 32000
	}
	testContent := req.Content

	body, err := buildRequestFromTemplate("anthropic", map[string]any{
		"MODEL":      req.Model,
		"STREAM":     req.Stream,
		"CONTENT":    testContent,
		"MAX_TOKENS": maxTokens,
		"USER_ID":    newClaudeCLIUserID(),
	})
	if err != nil {
		return "", nil, nil, err
	}

	if len(req.Messages) > 0 {
		body, err = patchMessagesInBody(body, "messages", toAnthropicMessages(req.Messages))
		if err != nil {
			return "", nil, nil, err
		}
	}
	body, err = applyAnthropicTestOptions(body, req)
	if err != nil {
		return "", nil, nil, err
	}

	fullURL := buildTesterURL(cfg.GetURLs()[0], "/v1/messages?beta=true")

	h := make(http.Header)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	// Claude Code CLI headers
	h.Set("User-Agent", "claude-cli/2.1.97 (external, cli)")
	h.Set("x-app", "cli")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("anthropic-beta", "claude-code-20250219,context-1m-2025-08-07,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advanced-tool-use-2025-11-20,effort-2025-11-24")
	h.Set("anthropic-dangerous-direct-browser-access", "true")
	// x-stainless-* headers
	h.Set("x-stainless-arch", "arm64")
	h.Set("x-stainless-lang", "js")
	h.Set("x-stainless-os", "MacOS")
	h.Set("x-stainless-package-version", "0.81.0")
	h.Set("x-stainless-retry-count", "0")
	h.Set("x-stainless-runtime", "node")
	h.Set("x-stainless-runtime-version", "v24.3.0")
	h.Set("x-stainless-timeout", "300")
	h.Set("X-Claude-Code-Session-Id", newTestSessionID())
	if req.Stream {
		h.Set("x-stainless-helper-method", "stream")
	}

	return fullURL, h, body, nil
}

// extractAnthropicResponseText 从Anthropic响应中提取文本
// 遍历content数组，跳过thinking block，取第一个type=text的block
func extractAnthropicResponseText(apiResp map[string]any) (string, bool) {
	content, ok := getTypedValue[[]any](apiResp, "content")
	if !ok || len(content) == 0 {
		return "", false
	}

	for i := range content {
		block, ok := getSliceItem[map[string]any](content, i)
		if !ok {
			continue
		}
		// 优先匹配 type=text 的 block
		if blockType, ok := getTypedValue[string](block, "type"); ok && blockType != "text" {
			continue
		}
		if text, ok := getTypedValue[string](block, "text"); ok {
			return text, true
		}
	}
	return "", false
}

// Parse 解析 Anthropic 格式的 API 响应
func (t *AnthropicTester) Parse(_ int, respBody []byte) map[string]any {
	out := map[string]any{}
	var apiResp map[string]any
	if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
		// 提取文本响应（使用辅助函数）
		if text, ok := extractAnthropicResponseText(apiResp); ok {
			out["response_text"] = text
		}

		// 提取usage（与其他Tester保持一致，便于上层统一处理）
		if usage, ok := getTypedValue[map[string]any](apiResp, "usage"); ok {
			out["usage"] = usage
		}

		return finalizeParsedAPIResponse(out, apiResp)
	}
	out["raw_response"] = string(respBody)
	return out
}
