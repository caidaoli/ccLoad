package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

type anthropicMessagesRequest struct {
	Model         string                    `json:"model"`
	Messages      []anthropicMessageContent `json:"messages"`
	Stream        util.FlexibleBool         `json:"stream,omitempty"`
	System        any                       `json:"system,omitempty"`
	Tools         json.RawMessage           `json:"tools"`
	ToolChoice    json.RawMessage           `json:"tool_choice,omitempty"`
	MaxTokens     int                       `json:"max_tokens"`
	Metadata      map[string]string         `json:"metadata"`
	Thinking      *anthropicThinkingConfig  `json:"thinking,omitempty"`
	Temperature   *float64                  `json:"temperature,omitempty"`
	TopP          *float64                  `json:"top_p,omitempty"`
	TopK          *int                      `json:"top_k,omitempty"`
	StopSequences []string                  `json:"stop_sequences,omitempty"`
}

type anthropicThinkingConfig struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicMessageContent struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicMessagesResponse struct {
	ID         string                   `json:"id"`
	Type       string                   `json:"type"`
	Role       string                   `json:"role"`
	Content    []anthropicResponseBlock `json:"content"`
	Model      string                   `json:"model"`
	StopReason string                   `json:"stop_reason"`
	Usage      anthropicMessagesUsage   `json:"usage"`
}

type anthropicResponseBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	Source    any    `json:"source,omitempty"`
	Title     string `json:"title,omitempty"`
	Content   any    `json:"content,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicMessagesUsage struct {
	InputTokens              int64                   `json:"input_tokens"`
	OutputTokens             int64                   `json:"output_tokens"`
	CacheReadInputTokens     int64                   `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64                   `json:"cache_creation_input_tokens,omitempty"`
	ReasoningTokens          int64                   `json:"reasoning_tokens,omitempty"`
	CacheCreation            *anthropicCacheCreation `json:"cache_creation,omitempty"`
}

type anthropicCacheCreation struct {
	Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens,omitempty"`
}

type anthropicToGeminiStreamState struct {
	model          string
	toolName       string
	toolJSON       string
	toolActive     bool
	thinkingActive bool
	inputTokens    int64
	outputTokens   int64
	blockIgnored   bool // for redacted_thinking and future block types that should be silently ignored
}

func convertAnthropicRequestToGemini(_ string, rawJSON []byte, _ bool) ([]byte, error) {
	var req anthropicMessagesRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}

	conv, err := normalizeAnthropicConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeGeminiRequest(conv)
}

func convertGeminiRequestToAnthropic(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req geminiRequestPayload
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeGeminiConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeAnthropicRequest(model, conv, stream)
}

func convertGeminiResponseToAnthropicNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp geminiResponse
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}

	blocks := make([]anthropicResponseBlock, 0)
	stopReason := "end_turn"
	if len(resp.Candidates) > 0 {
		parts, err := conversationPartsFromGeminiParts(resp.Candidates[0].Content.Parts)
		if err != nil {
			return nil, err
		}
		encodedBlocks, err := encodeAnthropicBlocks(parts)
		if err != nil {
			return nil, err
		}
		blocks, err = anthropicResponseBlocksFromMaps(encodedBlocks)
		if err != nil {
			return nil, err
		}
		stopReason = mapGeminiFinishReasonToAnthropic(resp.Candidates[0].FinishReason, hasConversationToolCalls(parts))
	}

	out := anthropicMessagesResponse{
		ID:         "msg-proxy",
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      coalesceModel(model, resp.ModelVersion),
		StopReason: stopReason,
		Usage: anthropicMessagesUsage{
			InputTokens:  resp.UsageMetadata.PromptTokenCount,
			OutputTokens: resp.UsageMetadata.CandidatesTokenCount,
		},
	}
	return sonic.Marshal(out)
}

func convertAnthropicResponseToGeminiNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	parts, err := geminiPartsFromAnthropicContent(resp["content"])
	if err != nil {
		return nil, err
	}
	usageMap, _ := resp["usage"].(map[string]any)
	inputTokens := int64Value(usageMap["input_tokens"])
	outputTokens := int64Value(usageMap["output_tokens"])
	return sonic.Marshal(buildGeminiPayloadFromParts(
		coalesceModel(model, resp["model"]),
		stringValue(resp["id"]),
		parts,
		mapAnthropicStopReasonToGemini(stringValue(resp["stop_reason"])),
		inputTokens,
		outputTokens,
		inputTokens+outputTokens,
		len(usageMap) > 0,
	))
}

type anthropicStreamState struct {
	started            bool
	done               bool
	model              string
	responseID         string
	nextIndex          int
	openTextIndex      int
	pendingToolCallIDs []string
	nextToolCallID     int
	inputTokens        int64
	outputTokens       int64
	stopReason         string
}

func convertGeminiResponseToAnthropicStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &anthropicStreamState{model: model, openTextIndex: -1, nextToolCallID: 1}
	}
	st := (*param).(*anthropicStreamState)
	if st.model == "" {
		st.model = model
	}
	if st.nextToolCallID == 0 {
		st.nextToolCallID = 1
	}

	line := strings.TrimSpace(string(rawJSON))
	if line == "" {
		return nil, nil
	}
	if after, ok := strings.CutPrefix(line, "data:"); ok {
		line = strings.TrimSpace(after)
	}
	if line == "[DONE]" {
		if st.done {
			return nil, nil
		}
		return geminiAnthropicStopChunks(st, "")
	}

	var resp geminiResponse
	if err := sonic.Unmarshal([]byte(line), &resp); err != nil {
		return nil, err
	}
	if resp.ModelVersion != "" {
		st.model = resp.ModelVersion
	}
	if resp.UsageMetadata.PromptTokenCount != 0 || resp.UsageMetadata.CandidatesTokenCount != 0 {
		st.inputTokens = resp.UsageMetadata.PromptTokenCount
		st.outputTokens = resp.UsageMetadata.CandidatesTokenCount
	}
	if len(resp.Candidates) == 0 {
		return nil, nil
	}
	if st.done {
		return nil, nil
	}
	parts, err := extractGeminiParts(resp.Candidates[0].Content.Parts, &st.pendingToolCallIDs, &st.nextToolCallID)
	if err != nil {
		return nil, err
	}

	outputs := make([][]byte, 0, len(parts)*3+3)
	for _, part := range parts {
		switch part.Kind {
		case partKindText:
			if part.Text == "" {
				continue
			}
			if !st.started {
				start, err := geminiAnthropicStartChunks(st)
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, start...)
				st.started = true
			}
			if st.openTextIndex < 0 {
				blockStart, err := marshalEventSSE("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": st.nextIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, blockStart)
				st.openTextIndex = st.nextIndex
				st.nextIndex++
			}
			deltaChunk, err := marshalEventSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": st.openTextIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": part.Text,
				},
			})
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, deltaChunk)
			st.stopReason = "end_turn"
		case partKindToolCall:
			if part.ToolCall == nil {
				return nil, fmt.Errorf("missing gemini tool call content")
			}
			if !st.started {
				start, err := geminiAnthropicStartChunks(st)
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, start...)
				st.started = true
			}
			if st.openTextIndex >= 0 {
				textStop, err := marshalEventSSE("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": st.openTextIndex,
				})
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, textStop)
				st.openTextIndex = -1
			}
			blockIndex := st.nextIndex
			st.nextIndex++
			startChunk, err := marshalEventSSE("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    part.ToolCall.ID,
					"name":  part.ToolCall.Name,
					"input": map[string]any{},
				},
			})
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, startChunk)
			arguments := strings.TrimSpace(string(part.ToolCall.Arguments))
			if arguments != "" && arguments != "{}" {
				deltaChunk, err := marshalEventSSE("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": blockIndex,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": arguments,
					},
				})
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, deltaChunk)
			}
			stopChunk, err := marshalEventSSE("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": blockIndex,
			})
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, stopChunk)
			st.stopReason = "tool_use"
		default:
			return nil, fmt.Errorf("unsupported gemini response part kind %q", part.Kind)
		}
	}
	if resp.Candidates[0].FinishReason != "" {
		stopChunks, err := geminiAnthropicStopChunks(st, mapGeminiFinishReasonToAnthropic(resp.Candidates[0].FinishReason, hasConversationToolCalls(parts)))
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, stopChunks...)
	}
	if len(outputs) == 0 {
		return nil, nil
	}
	return outputs, nil
}

func geminiAnthropicStartChunks(st *anthropicStreamState) ([][]byte, error) {
	msgID := st.responseID
	if msgID == "" {
		msgID = "msg-proxy"
	}
	start, err := marshalEventSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         st.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  st.inputTokens,
				"output_tokens": 0,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	return [][]byte{start}, nil
}

func geminiAnthropicStopChunks(st *anthropicStreamState, stopReason string) ([][]byte, error) {
	if stopReason == "" {
		stopReason = st.stopReason
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}
	outputs := make([][]byte, 0, 4)
	if st != nil && !st.started {
		start, err := geminiAnthropicStartChunks(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, start...)
		st.started = true
	}
	if st != nil && st.openTextIndex >= 0 {
		blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": st.openTextIndex,
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, blockStop)
		st.openTextIndex = -1
	}
	messageDelta, err := marshalEventSSE("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":  st.inputTokens,
			"output_tokens": st.outputTokens,
		},
	})
	if err != nil {
		return nil, err
	}
	messageStop, err := marshalEventSSE("message_stop", map[string]any{"type": "message_stop"})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, messageDelta, messageStop)
	if st != nil {
		st.started = false
		st.done = true
		st.nextIndex = 0
		st.stopReason = ""
	}
	return outputs, nil
}

func convertAnthropicResponseToGeminiStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &anthropicToGeminiStreamState{model: model}
	}
	st := (*param).(*anthropicToGeminiStreamState)
	if st.model == "" {
		st.model = model
	}

	raw := strings.TrimSpace(string(rawJSON))
	if raw == "" {
		return nil, nil
	}
	eventType, line := parseSSEEventBlock(raw)
	if line == "" {
		return nil, nil
	}

	var payload map[string]any
	if err := sonic.Unmarshal([]byte(line), &payload); err != nil {
		return nil, err
	}
	if eventType == "message_start" {
		if message, _ := payload["message"].(map[string]any); message != nil {
			if messageModel := stringValue(message["model"]); messageModel != "" {
				st.model = messageModel
			}
			if usage, _ := message["usage"].(map[string]any); usage != nil {
				if raw, ok := usage["input_tokens"]; ok {
					st.inputTokens = int64Value(raw)
				}
				if raw, ok := usage["output_tokens"]; ok {
					st.outputTokens = int64Value(raw)
				}
			}
		}
		return nil, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_start" {
		if block, _ := payload["content_block"].(map[string]any); block != nil {
			switch stringValue(block["type"]) {
			case "tool_use":
				st.toolName = stringValue(block["name"])
				st.toolJSON = ""
				st.toolActive = true
			case "thinking":
				st.thinkingActive = true
			case "redacted_thinking":
				st.blockIgnored = true
			}
		}
		return nil, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_delta" {
		if delta, _ := payload["delta"].(map[string]any); delta != nil {
			deltaType := stringValue(delta["type"])
			if deltaType == "input_json_delta" && st.toolActive {
				st.toolJSON += stringValue(delta["partial_json"])
				return nil, nil
			}
			if deltaType == "thinking_delta" && st.thinkingActive {
				// Gemini 不支持 thinking，静默消费，不输出
				return nil, nil
			}
			if deltaType == "signature_delta" && st.thinkingActive {
				// thinking 签名，静默忽略
				return nil, nil
			}
			if text := stringValue(delta["text"]); text != "" {
				body, err := marshalDataSSE(buildGeminiPayload(st.model, text, "", 0, 0, 0, false))
				if err != nil {
					return nil, err
				}
				return [][]byte{body}, nil
			}
		}
	}
	if typ := stringValue(payload["type"]); typ == "content_block_stop" {
		if st.blockIgnored {
			st.blockIgnored = false
			return nil, nil
		}
		if st.thinkingActive {
			st.thinkingActive = false
			return nil, nil
		}
		if st.toolActive {
			args := any(map[string]any{})
			if strings.TrimSpace(st.toolJSON) != "" {
				if err := sonic.Unmarshal([]byte(st.toolJSON), &args); err != nil {
					return nil, err
				}
			}
			body, err := marshalDataSSE(buildGeminiPayloadFromParts(st.model, "", []geminiPart{{
				FunctionCall: &geminiFunctionCall{Name: st.toolName, Args: args},
			}}, "", 0, 0, 0, false))
			if err != nil {
				return nil, err
			}
			st.toolName = ""
			st.toolJSON = ""
			st.toolActive = false
			return [][]byte{body}, nil
		}
		// text block stop is a no-op for Gemini; only tool and thinking blocks need flushing.
	}
	if typ := stringValue(payload["type"]); typ == "message_delta" {
		usage, _ := payload["usage"].(map[string]any)
		if usage != nil {
			if raw, ok := usage["input_tokens"]; ok {
				st.inputTokens = int64Value(raw)
			}
			if raw, ok := usage["output_tokens"]; ok {
				st.outputTokens = int64Value(raw)
			}
		}
		totalTokens := st.inputTokens + st.outputTokens
		finishReason := ""
		if delta, _ := payload["delta"].(map[string]any); delta != nil {
			finishReason = mapAnthropicStopReasonToGemini(stringValue(delta["stop_reason"]))
		}
		includeUsage := usage != nil || st.inputTokens != 0 || st.outputTokens != 0
		body, err := marshalDataSSE(buildGeminiPayloadFromParts(st.model, "", nil, finishReason, st.inputTokens, st.outputTokens, totalTokens, includeUsage))
		if err != nil {
			return nil, err
		}
		return [][]byte{body}, nil
	}
	return nil, nil
}
