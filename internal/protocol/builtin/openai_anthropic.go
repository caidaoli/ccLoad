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
		promptTokens     int64
		completionTokens int64
		seen             bool
	}
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
	content := ""
	for _, block := range resp.Content {
		content += block.Text
	}
	out := openAIChatCompletionResponse{
		ID:      "chatcmpl-proxy",
		Object:  "chat.completion",
		Created: 0,
		Model:   model,
		Choices: []openAIChatCompletionChoice{{
			Index: 0,
			Message: openAIChatCompletionMessage{
				Role:    "assistant",
				Content: content,
			},
			FinishReason: "stop",
		}},
		Usage: openAIChatCompletionUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	if out.Model == "" {
		out.Model = resp.Model
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
		Content:    []anthropicTextBlock{},
		Model:      coalesceModel(model, resp["model"]),
		StopReason: stopReason,
	}
	if len(content) > 0 {
		payload := map[string]any{
			"id":          out.ID,
			"type":        out.Type,
			"role":        out.Role,
			"content":     content,
			"model":       out.Model,
			"stop_reason": out.StopReason,
		}
		if usage := openAIUsageFromMap(resp["usage"]); usage != nil {
			payload["usage"] = map[string]any{
				"input_tokens":  usage.promptTokens,
				"output_tokens": usage.completionTokens,
			}
		}
		return sonic.Marshal(payload)
	}
	if usage := openAIUsageFromMap(resp["usage"]); usage != nil {
		out.Usage.InputTokens = usage.promptTokens
		out.Usage.OutputTokens = usage.completionTokens
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
	return encodeAnthropicBlocks(parts)
}

func convertAnthropicResponseToOpenAIStream(_ context.Context, model string, _, _, rawJSON []byte, _ *any) ([][]byte, error) {
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
	if eventType == "message_stop" || func() bool { typ, _ := payload["type"].(string); return typ == "message_stop" }() {
		return [][]byte{[]byte("data: [DONE]\n\n")}, nil
	}
	if typ, _ := payload["type"].(string); typ == "content_block_delta" {
		if delta, ok := payload["delta"].(map[string]any); ok {
			if text, ok := delta["text"].(string); ok && text != "" {
				chunk := map[string]any{
					"id":      "chatcmpl-proxy",
					"object":  "chat.completion.chunk",
					"created": 0,
					"model":   model,
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
	if st != nil && st.usage.seen {
		inputTokens = st.usage.promptTokens
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
			"usage": map[string]any{
				"input_tokens":  inputTokens,
				"output_tokens": 0,
			},
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
	if st != nil && st.usage.seen {
		outputTokens = st.usage.completionTokens
	}
	messageDelta, err := marshalEventSSE("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": stopReason,
		},
		"usage": map[string]any{
			"output_tokens": outputTokens,
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
	}
	return outputs, nil
}
