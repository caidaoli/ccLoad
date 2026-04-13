package builtin

import (
	"context"
	"strings"

	"github.com/bytedance/sonic"
)

type codexToAnthropicStreamState struct {
	started    bool
	blockIndex int
	model      string
	usage      struct {
		inputTokens              int64
		outputTokens             int64
		cachedTokens             int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
}

type anthropicToCodexStreamState struct {
	model      string
	responseID string
	usage      struct {
		inputTokens              int64
		outputTokens             int64
		totalTokens              int64
		cacheReadInputTokens     int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
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

func convertCodexRequestToAnthropic(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req codexRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeCodexConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeAnthropicRequest(model, conv, stream)
}

func convertAnthropicRequestToCodex(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req anthropicMessagesRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeAnthropicConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeCodexRequest(model, conv, stream)
}

func convertAnthropicResponseToCodexNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp anthropicMessagesResponse
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	output, err := codexOutputItemsFromAnthropicBlocks(resp.Content)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"id":     "resp-proxy",
		"object": "response",
		"status": "completed",
		"model":  coalesceModel(model, resp.Model),
		"output": output,
	}
	if usage := codexUsageFromAnthropicUsage(resp.Usage); usage != nil {
		out["usage"] = codexUsagePayload(usage)
	}
	return sonic.Marshal(out)
}

func convertCodexResponseToAnthropicNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	message, err := openAIMessageFromCodexOutput(resp["output"])
	if err != nil {
		return nil, err
	}
	blocks, err := anthropicBlocksFromOpenAIMessage(message)
	if err != nil {
		return nil, err
	}
	stopReason := "end_turn"
	if rawToolCalls, ok := message["tool_calls"].([]map[string]any); ok && len(rawToolCalls) > 0 {
		stopReason = "tool_use"
	} else if rawToolCalls, ok := message["tool_calls"].([]any); ok && len(rawToolCalls) > 0 {
		stopReason = "tool_use"
	}
	out := map[string]any{
		"id":          "msg-proxy",
		"type":        "message",
		"role":        "assistant",
		"content":     blocks,
		"model":       coalesceModel(model, resp["model"]),
		"stop_reason": stopReason,
	}
	if usage := codexUsageFromMap(resp["usage"]); usage != nil {
		inputTokens := max(usage.inputTokens-usage.cachedTokens, 0)

		out["usage"] = map[string]any{
			"input_tokens":                inputTokens,
			"output_tokens":               usage.outputTokens,
			"cache_read_input_tokens":     usage.cachedTokens,
			"cache_creation_input_tokens": usage.cacheCreationInputTokens,
			"reasoning_tokens":            usage.reasoningTokens,
		}
	}
	return sonic.Marshal(out)
}

func convertAnthropicResponseToCodexStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &anthropicToCodexStreamState{model: model, responseID: "resp-proxy"}
	}
	st := (*param).(*anthropicToCodexStreamState)
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
			if messageID := stringValue(message["id"]); messageID != "" {
				st.responseID = messageID
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
		done := map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     st.responseID,
				"object": "response",
				"status": "completed",
				"model":  st.model,
			},
		}
		if st.usage.seen {
			done["response"].(map[string]any)["usage"] = codexUsagePayload(&codexUsage{
				inputTokens:              st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens,
				outputTokens:             st.usage.outputTokens,
				totalTokens:              st.usage.totalTokens,
				cachedTokens:             st.usage.cacheReadInputTokens,
				cacheCreationInputTokens: st.usage.cacheCreationInputTokens,
				reasoningTokens:          st.usage.reasoningTokens,
			})
		}
		body, err := sonic.Marshal(done)
		if err != nil {
			return nil, err
		}
		return [][]byte{append([]byte("event: response.completed\ndata: "), append(body, []byte("\n\n")...)...)}, nil
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
				st.reasoningText += stringValue(delta["thinking"])
				return nil, nil
			}
			if stringValue(delta["type"]) == "signature_delta" && st.reasoningActive {
				st.reasoningSignature += stringValue(delta["signature"])
				return nil, nil
			}
			if text := stringValue(delta["text"]); text != "" {
				chunk := map[string]any{
					"type":  "response.output_text.delta",
					"delta": text,
				}
				body, err := sonic.Marshal(chunk)
				if err != nil {
					return nil, err
				}
				return [][]byte{append([]byte("event: response.output_text.delta\ndata: "), append(body, []byte("\n\n")...)...)}, nil
			}
		}
	}
	if typ := stringValue(payload["type"]); typ == "content_block_stop" && st.reasoningActive {
		chunk := map[string]any{
			"type": "response.output_item.done",
			"item": codexReasoningItem(st.reasoningText, firstNonEmptyString(map[string]any{
				"signature": st.reasoningSignature,
				"data":      st.reasoningData,
			}, "signature", "data")),
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		st.reasoningActive = false
		st.reasoningText = ""
		st.reasoningSignature = ""
		st.reasoningData = ""
		return [][]byte{append([]byte("event: response.output_item.done\ndata: "), append(body, []byte("\n\n")...)...)}, nil
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
			"type": "response.output_item.done",
			"item": map[string]any{
				"type":      "function_call",
				"call_id":   st.toolID,
				"name":      st.toolName,
				"arguments": jsonStringOrObject(arguments),
			},
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		st.toolID = ""
		st.toolName = ""
		st.toolInput = nil
		st.toolJSON = ""
		st.toolActive = false
		return [][]byte{append([]byte("event: response.output_item.done\ndata: "), append(body, []byte("\n\n")...)...)}, nil
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
	}
	return nil, nil
}

func convertCodexResponseToAnthropicStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &codexToAnthropicStreamState{model: model}
	}
	st := (*param).(*codexToAnthropicStreamState)
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
	if response, _ := payload["response"].(map[string]any); response != nil {
		if responseModel := stringValue(response["model"]); responseModel != "" {
			st.model = responseModel
		}
		if usage := codexUsageFromMap(response["usage"]); usage != nil {
			st.usage.inputTokens = usage.inputTokens
			st.usage.outputTokens = usage.outputTokens
			st.usage.cachedTokens = usage.cachedTokens
			st.usage.cacheCreationInputTokens = usage.cacheCreationInputTokens
			st.usage.reasoningTokens = usage.reasoningTokens
			st.usage.seen = true
		}
	}
	if eventType == "response.output_item.done" || stringValue(payload["type"]) == "response.output_item.done" {
		item, _ := payload["item"].(map[string]any)
		if item == nil {
			return nil, nil
		}
		itemType := stringValue(item["type"])
		outputs := make([][]byte, 0, 6)

		if !st.started {
			msgStart, err := codexAnthropicMessageStartChunk(st)
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, msgStart)
			st.started = true
		}

		switch itemType {
		case "reasoning":
			encContent := stringValue(item["encrypted_content"])
			if encContent != "" {
				// redacted_thinking block: start + stop, no delta
				blockStart, err := marshalEventSSE("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": st.blockIndex,
					"content_block": map[string]any{
						"type": "redacted_thinking",
						"data": "",
					},
				})
				if err != nil {
					return nil, err
				}
				blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": st.blockIndex,
				})
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, blockStart, blockStop)
				st.blockIndex++
			} else {
				// Extract summary text
				summaryText := ""
				if summaries, ok := item["summary"].([]any); ok {
					for _, s := range summaries {
						if sm, ok := s.(map[string]any); ok {
							if stringValue(sm["type"]) == "summary_text" {
								summaryText += stringValue(sm["text"])
							}
						}
					}
				}
				if summaryText == "" {
					return outputs, nil
				}
				blockStart, err := marshalEventSSE("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": st.blockIndex,
					"content_block": map[string]any{
						"type":     "thinking",
						"thinking": "",
					},
				})
				if err != nil {
					return nil, err
				}
				thinkingDelta, err := marshalEventSSE("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.blockIndex,
					"delta": map[string]any{
						"type":     "thinking_delta",
						"thinking": summaryText,
					},
				})
				if err != nil {
					return nil, err
				}
				blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": st.blockIndex,
				})
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, blockStart, thinkingDelta, blockStop)
				st.blockIndex++
			}

		case "function_call":
			toolID := stringValue(item["id"])
			toolName := stringValue(item["name"])
			arguments := stringValue(item["arguments"])
			partialJSON := arguments
			// Normalize: arguments may already be a JSON string; pass as-is
			if partialJSON == "" {
				partialJSON = "{}"
			}
			blockStart, err := marshalEventSSE("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": st.blockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    toolID,
					"name":  toolName,
					"input": map[string]any{},
				},
			})
			if err != nil {
				return nil, err
			}
			inputDelta, err := marshalEventSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": st.blockIndex,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": partialJSON,
				},
			})
			if err != nil {
				return nil, err
			}
			blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": st.blockIndex,
			})
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, blockStart, inputDelta, blockStop)
			st.blockIndex++
		}
		return outputs, nil
	}
	if eventType == "response.output_text.delta" || stringValue(payload["type"]) == "response.output_text.delta" {
		if content := stringValue(payload["delta"]); content != "" {
			outputs := make([][]byte, 0, 3)
			if !st.started {
				msgStart, err := codexAnthropicMessageStartChunk(st)
				if err != nil {
					return nil, err
				}
				textBlockStart, err := marshalEventSSE("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": st.blockIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, msgStart, textBlockStart)
				st.started = true
			}
			deltaChunk, err := marshalEventSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": st.blockIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": content,
				},
			})
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, deltaChunk)
			return outputs, nil
		}
	}
	if eventType == "response.completed" || stringValue(payload["type"]) == "response.completed" {
		return codexAnthropicStopChunks(st)
	}
	return nil, nil
}

// codexAnthropicMessageStartChunk emits only the message_start SSE frame.
// Callers are responsible for emitting the appropriate content_block_start
// depending on whether the first block is text, thinking, or tool_use.
func codexAnthropicMessageStartChunk(st *codexToAnthropicStreamState) ([]byte, error) {
	inputTokens := int64(0)
	cacheReadTokens := int64(0)
	cacheCreationTokens := int64(0)
	if st != nil && st.usage.seen {
		inputTokens = max(st.usage.inputTokens-st.usage.cachedTokens, 0)
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
	return marshalEventSSE("message_start", map[string]any{
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
}

// codexAnthropicStartChunks emits message_start + text content_block_start.
// Used when the response begins directly with text (no leading reasoning/tool blocks).
func codexAnthropicStartChunks(st *codexToAnthropicStreamState) ([][]byte, error) {
	msgStart, err := codexAnthropicMessageStartChunk(st)
	if err != nil {
		return nil, err
	}
	blockStart, err := marshalEventSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": st.blockIndex,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
	if err != nil {
		return nil, err
	}
	return [][]byte{msgStart, blockStart}, nil
}

func codexAnthropicStopChunks(st *codexToAnthropicStreamState) ([][]byte, error) {
	outputs := make([][]byte, 0, 5)
	if st != nil && !st.started {
		// No content was emitted at all: synthesize message_start + empty text block.
		start, err := codexAnthropicStartChunks(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, start...)
		st.started = true
	}
	blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": st.blockIndex,
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
		outputTokens = st.usage.outputTokens
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
			"stop_reason": "end_turn",
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
