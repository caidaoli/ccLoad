package builtin

import (
	"context"
	"strings"

	"github.com/bytedance/sonic"
)

type openAIToAnthropicStreamState struct {
	started bool
	model   string
	usage   struct {
		promptTokens             int64
		completionTokens         int64
		cachedTokens             int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
}

type anthropicToOpenAIStreamState struct {
	model string
	usage struct {
		inputTokens              int64
		outputTokens             int64
		totalTokens              int64
		cacheReadInputTokens     int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
	toolCallIndex      int
	toolID             string
	toolName           string
	toolInput          any
	toolJSON           string
	toolActive         bool
	reasoningActive    bool
	reasoningText      string
	reasoningSignature string
	reasoningData      string
}

func convertOpenAIRequestToAnthropic(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req openAIChatRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeAnthropicRequest(model, conv, stream)
}

func convertAnthropicRequestToOpenAI(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req anthropicMessagesRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeAnthropicConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeOpenAIRequest(model, conv, stream)
}

func convertAnthropicResponseToOpenAINonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp anthropicMessagesResponse
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	message, err := openAIMessageFromAnthropicBlocks(resp.Content)
	if err != nil {
		return nil, err
	}
	out := openAIChatCompletionResponse{
		ID:      "chatcmpl-proxy",
		Object:  "chat.completion",
		Created: 0,
		Model:   coalesceModel(model, resp.Model),
		Choices: []openAIChatCompletionChoice{{
			Index:        0,
			Message:      message,
			FinishReason: mapAnthropicStopReasonToOpenAI(resp.StopReason, len(message.ToolCalls) > 0),
		}},
		Usage: openAIUsageFromAnthropicUsage(resp.Usage),
	}
	return sonic.Marshal(out)
}

func convertOpenAIResponseToAnthropicNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	content := []map[string]any{}
	stopReason := "end_turn"
	if choices, _ := resp["choices"].([]any); len(choices) > 0 {
		if choice, _ := choices[0].(map[string]any); choice != nil {
			if message, _ := choice["message"].(map[string]any); message != nil {
				blocks, err := anthropicBlocksFromOpenAIMessage(message)
				if err != nil {
					return nil, err
				}
				content = blocks
			}
			stopReason = mapOpenAIFinishReasonToAnthropic(stringValue(choice["finish_reason"]))
		}
	}
	out := anthropicMessagesResponse{
		ID:         "msg-proxy",
		Type:       "message",
		Role:       "assistant",
		Content:    []anthropicResponseBlock{},
		Model:      coalesceModel(model, resp["model"]),
		StopReason: stopReason,
	}
	if len(content) > 0 {
		blocks, err := anthropicResponseBlocksFromMaps(content)
		if err != nil {
			return nil, err
		}
		out.Content = blocks
	}
	if usage := openAIUsageFromMap(resp["usage"]); usage != nil {
		inputTokens := usage.promptTokens - usage.cachedTokens
		if inputTokens < 0 {
			inputTokens = 0
		}
		out.Usage.InputTokens = inputTokens
		out.Usage.OutputTokens = usage.completionTokens
		out.Usage.CacheReadInputTokens = usage.cachedTokens
		out.Usage.CacheCreationInputTokens = usage.cacheCreationInputTokens
		out.Usage.ReasoningTokens = usage.reasoningTokens
	}
	return sonic.Marshal(out)
}

func anthropicBlocksFromOpenAIMessage(message map[string]any) ([]map[string]any, error) {
	parts, err := extractOpenAIContentParts(message["content"])
	if err != nil {
		return nil, err
	}
	if rawCalls, ok := message["tool_calls"]; ok {
		var calls []openAIChatToolCall
		callBytes, err := sonic.Marshal(rawCalls)
		if err != nil {
			return nil, err
		}
		if err := sonic.Unmarshal(callBytes, &calls); err != nil {
			return nil, err
		}
		toolParts, err := extractOpenAIToolCallParts(calls)
		if err != nil {
			return nil, err
		}
		parts = append(parts, toolParts...)
	}
	blocks, err := encodeAnthropicBlocks(parts)
	if err != nil {
		return nil, err
	}
	reasoningAdded := false
	if rawReasoning, ok := message["reasoning"].([]any); ok {
		for _, item := range rawReasoning {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch normalizeRole(stringValue(entry["type"])) {
			case "redacted_thinking":
				blocks = append(blocks, map[string]any{
					"type": "redacted_thinking",
					"data": stringValue(entry["data"]),
				})
				reasoningAdded = true
			default:
				if text := stringValue(entry["text"]); text != "" {
					block := map[string]any{
						"type":     "thinking",
						"thinking": text,
					}
					if signature := stringValue(entry["signature"]); signature != "" {
						block["signature"] = signature
					}
					blocks = append(blocks, block)
					reasoningAdded = true
				}
			}
		}
	}
	if !reasoningAdded {
		if text := stringValue(message["reasoning_content"]); text != "" {
			blocks = append(blocks, map[string]any{
				"type":     "thinking",
				"thinking": text,
			})
		}
	}
	return blocks, nil
}

func convertAnthropicResponseToOpenAIStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &anthropicToOpenAIStreamState{model: model}
	}
	st := (*param).(*anthropicToOpenAIStreamState)
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
				st.usage.inputTokens = int64Value(usage["input_tokens"])
				st.usage.outputTokens = int64Value(usage["output_tokens"])
				st.usage.cacheReadInputTokens = int64Value(usage["cache_read_input_tokens"])
				st.usage.cacheCreationInputTokens = int64Value(usage["cache_creation_input_tokens"])
				st.usage.reasoningTokens = int64Value(usage["reasoning_tokens"])
				st.usage.totalTokens = st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens + st.usage.outputTokens
				st.usage.seen = st.usage.inputTokens != 0 || st.usage.outputTokens != 0 || st.usage.cacheReadInputTokens != 0 || st.usage.cacheCreationInputTokens != 0 || st.usage.reasoningTokens != 0
			}
		}
		return nil, nil
	}
	if eventType == "message_stop" || func() bool { typ, _ := payload["type"].(string); return typ == "message_stop" }() {
		return [][]byte{[]byte("data: [DONE]\n\n")}, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_start" {
		if block, _ := payload["content_block"].(map[string]any); block != nil {
			switch stringValue(block["type"]) {
			case "tool_use":
				st.toolID = stringValue(block["id"])
				st.toolName = stringValue(block["name"])
				st.toolInput = block["input"]
				st.toolJSON = ""
				st.toolActive = true
			case "thinking", "redacted_thinking":
				st.reasoningActive = true
				st.reasoningText = ""
				st.reasoningSignature = ""
				st.reasoningData = stringValue(block["data"])
			}
		}
		return nil, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_delta" {
		if delta, ok := payload["delta"].(map[string]any); ok {
			if stringValue(delta["type"]) == "input_json_delta" && st.toolActive {
				st.toolJSON += stringValue(delta["partial_json"])
				return nil, nil
			}
			if stringValue(delta["type"]) == "thinking_delta" && st.reasoningActive {
				text := stringValue(delta["thinking"])
				if text == "" {
					return nil, nil
				}
				st.reasoningText += text
				chunk := map[string]any{
					"id":      "chatcmpl-proxy",
					"object":  "chat.completion.chunk",
					"created": 0,
					"model":   st.model,
					"choices": []map[string]any{{
						"index":         0,
						"delta":         map[string]any{"reasoning_content": text},
						"finish_reason": nil,
					}},
				}
				body, err := sonic.Marshal(chunk)
				if err != nil {
					return nil, err
				}
				return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
			}
			if stringValue(delta["type"]) == "signature_delta" && st.reasoningActive {
				st.reasoningSignature += stringValue(delta["signature"])
				return nil, nil
			}
			if text := stringValue(delta["text"]); text != "" {
				chunk := map[string]any{
					"id":      "chatcmpl-proxy",
					"object":  "chat.completion.chunk",
					"created": 0,
					"model":   st.model,
					"choices": []map[string]any{{
						"index":         0,
						"delta":         map[string]any{"content": text},
						"finish_reason": nil,
					}},
				}
				body, err := sonic.Marshal(chunk)
				if err != nil {
					return nil, err
				}
				return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
			}
		}
	}
	if typ := stringValue(payload["type"]); typ == "content_block_stop" && st.reasoningActive {
		if st.reasoningSignature == "" && st.reasoningData == "" {
			st.reasoningActive = false
			st.reasoningText = ""
			return nil, nil
		}
		reasoning := map[string]any{"type": "thinking"}
		if st.reasoningSignature != "" {
			reasoning["signature"] = st.reasoningSignature
		}
		if st.reasoningData != "" {
			reasoning["type"] = "redacted_thinking"
			reasoning["data"] = st.reasoningData
		}
		chunk := map[string]any{
			"id":      "chatcmpl-proxy",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   st.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"reasoning": reasoning},
				"finish_reason": nil,
			}},
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		st.reasoningActive = false
		st.reasoningText = ""
		st.reasoningSignature = ""
		st.reasoningData = ""
		return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_stop" && st.toolActive {
		arguments := strings.TrimSpace(st.toolJSON)
		if arguments == "" {
			if raw, err := sonic.Marshal(st.toolInput); err == nil && len(raw) > 0 {
				arguments = string(raw)
			}
		}
		if arguments == "" {
			arguments = "{}"
		}
		chunk := map[string]any{
			"id":      "chatcmpl-proxy",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   st.model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": st.toolCallIndex,
						"id":    st.toolID,
						"type":  "function",
						"function": map[string]any{
							"name":      st.toolName,
							"arguments": arguments,
						},
					}},
				},
				"finish_reason": nil,
			}},
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		st.toolCallIndex++
		st.toolID = ""
		st.toolName = ""
		st.toolInput = nil
		st.toolJSON = ""
		st.toolActive = false
		return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
	}
	if typ := stringValue(payload["type"]); typ == "message_delta" {
		if usage, _ := payload["usage"].(map[string]any); usage != nil {
			if val := int64Value(usage["input_tokens"]); val != 0 {
				st.usage.inputTokens = val
			}
			if val := int64Value(usage["output_tokens"]); val != 0 {
				st.usage.outputTokens = val
			}
			if val := int64Value(usage["cache_read_input_tokens"]); val != 0 {
				st.usage.cacheReadInputTokens = val
			}
			if val := int64Value(usage["cache_creation_input_tokens"]); val != 0 {
				st.usage.cacheCreationInputTokens = val
			}
			if val := int64Value(usage["reasoning_tokens"]); val != 0 {
				st.usage.reasoningTokens = val
			}
			st.usage.totalTokens = st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens + st.usage.outputTokens
			st.usage.seen = true
		}
		finishReason := any(nil)
		if delta, _ := payload["delta"].(map[string]any); delta != nil {
			if reason := stringValue(delta["stop_reason"]); reason != "" {
				finishReason = mapAnthropicStopReasonToOpenAI(reason, reason == "tool_use")
			}
		}
		if finishReason == nil && !st.usage.seen {
			return nil, nil
		}
		chunk := map[string]any{
			"id":      "chatcmpl-proxy",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   st.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			}},
		}
		if st.usage.seen {
			chunk["usage"] = openAIUsagePayload(&openAIUsage{
				promptTokens:             st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens,
				completionTokens:         st.usage.outputTokens,
				totalTokens:              st.usage.totalTokens,
				cachedTokens:             st.usage.cacheReadInputTokens,
				cacheCreationInputTokens: st.usage.cacheCreationInputTokens,
				reasoningTokens:          st.usage.reasoningTokens,
			})
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
	}
	return nil, nil
}

func convertOpenAIResponseToAnthropicStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &openAIToAnthropicStreamState{model: model}
	}
	st := (*param).(*openAIToAnthropicStreamState)
	if st.model == "" {
		st.model = model
	}

	line := strings.TrimSpace(string(rawJSON))
	if line == "" {
		return nil, nil
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "[DONE]" {
		return openAIAnthropicStopChunks(st, "end_turn")
	}

	var chunk map[string]any
	if err := sonic.Unmarshal([]byte(line), &chunk); err != nil {
		return nil, err
	}
	if chunkModel := stringValue(chunk["model"]); chunkModel != "" {
		st.model = chunkModel
	}
	if usage := openAIUsageFromMap(chunk["usage"]); usage != nil {
		st.usage.promptTokens = usage.promptTokens
		st.usage.completionTokens = usage.completionTokens
		st.usage.cachedTokens = usage.cachedTokens
		st.usage.cacheCreationInputTokens = usage.cacheCreationInputTokens
		st.usage.reasoningTokens = usage.reasoningTokens
		st.usage.seen = true
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil, nil
	}

	outputs := make([][]byte, 0, 4)
	if delta, _ := choice["delta"].(map[string]any); delta != nil {
		if content := stringValue(delta["content"]); content != "" {
			if !st.started {
				start, err := openAIAnthropicStartChunks(st)
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, start...)
				st.started = true
			}
			deltaChunk, err := marshalEventSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{
					"type": "text_delta",
					"text": content,
				},
			})
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, deltaChunk)
		}
	}
	if finishReasonRaw, ok := choice["finish_reason"]; ok && finishReasonRaw != nil {
		done, err := openAIAnthropicStopChunks(st, mapOpenAIFinishReasonToAnthropic(stringValue(finishReasonRaw)))
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, done...)
	}
	if len(outputs) == 0 {
		return nil, nil
	}
	return outputs, nil
}

func openAIAnthropicStartChunks(st *openAIToAnthropicStreamState) ([][]byte, error) {
	inputTokens := int64(0)
	cacheReadTokens := int64(0)
	cacheCreationTokens := int64(0)
	if st != nil && st.usage.seen {
		inputTokens = st.usage.promptTokens - st.usage.cachedTokens
		if inputTokens < 0 {
			inputTokens = 0
		}
		cacheReadTokens = st.usage.cachedTokens
		cacheCreationTokens = st.usage.cacheCreationInputTokens
	}
	usage := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": 0,
	}
	if cacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = cacheReadTokens
	}
	if cacheCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = cacheCreationTokens
	}
	start, err := marshalEventSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          "msg-proxy",
			"type":        "message",
			"role":        "assistant",
			"content":     []any{},
			"model":       st.model,
			"stop_reason": nil,
			"usage":       usage,
		},
	})
	if err != nil {
		return nil, err
	}
	blockStart, err := marshalEventSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
	if err != nil {
		return nil, err
	}
	return [][]byte{start, blockStart}, nil
}

func openAIAnthropicStopChunks(st *openAIToAnthropicStreamState, stopReason string) ([][]byte, error) {
	if stopReason == "" {
		stopReason = "end_turn"
	}
	outputs := make([][]byte, 0, 5)
	if st != nil && !st.started {
		start, err := openAIAnthropicStartChunks(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, start...)
	}
	blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, blockStop)
	outputTokens := int64(0)
	cacheReadTokens := int64(0)
	cacheCreationTokens := int64(0)
	reasoningTokens := int64(0)
	if st != nil && st.usage.seen {
		outputTokens = st.usage.completionTokens
		cacheReadTokens = st.usage.cachedTokens
		cacheCreationTokens = st.usage.cacheCreationInputTokens
		reasoningTokens = st.usage.reasoningTokens
	}
	usage := map[string]any{
		"output_tokens": outputTokens,
	}
	if cacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = cacheReadTokens
	}
	if cacheCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = cacheCreationTokens
	}
	if reasoningTokens > 0 {
		usage["reasoning_tokens"] = reasoningTokens
	}
	messageDelta, err := marshalEventSSE("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": stopReason,
		},
		"usage": usage,
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
	}
	return outputs, nil
}
