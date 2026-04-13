package builtin

import (
	"context"
	"strings"

	"github.com/bytedance/sonic"
)

type codexToAnthropicStreamState struct {
	started bool
	model   string
	usage   struct {
		inputTokens  int64
		outputTokens int64
		seen         bool
	}
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
	content := ""
	for _, block := range resp.Content {
		content += block.Text
	}
	out := codexResponse{
		ID:     "resp-proxy",
		Object: "response",
		Status: "completed",
		Model:  model,
		Output: []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}{
			{
				Type: "message",
				Role: "assistant",
				Content: []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{
					{Type: "output_text", Text: content},
				},
			},
		},
	}
	out.Usage.InputTokens = resp.Usage.InputTokens
	out.Usage.OutputTokens = resp.Usage.OutputTokens
	out.Usage.TotalTokens = resp.Usage.InputTokens + resp.Usage.OutputTokens
	if out.Model == "" {
		out.Model = resp.Model
	}
	return sonic.Marshal(out)
}

func convertCodexResponseToAnthropicNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	content, toolCalls, err := openAIMessageFromCodexOutput(resp["output"])
	if err != nil {
		return nil, err
	}
	blocks, err := anthropicBlocksFromOpenAIMessage(map[string]any{
		"content":    content,
		"tool_calls": toolCalls,
	})
	if err != nil {
		return nil, err
	}
	stopReason := "end_turn"
	if len(toolCalls) > 0 {
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
		out["usage"] = map[string]any{
			"input_tokens":  usage.inputTokens,
			"output_tokens": usage.outputTokens,
		}
	}
	return sonic.Marshal(out)
}

func convertAnthropicResponseToCodexStream(_ context.Context, model string, _, _, rawJSON []byte, _ *any) ([][]byte, error) {
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
		done := map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp-proxy",
				"object": "response",
				"status": "completed",
				"model":  model,
			},
		}
		body, err := sonic.Marshal(done)
		if err != nil {
			return nil, err
		}
		return [][]byte{append([]byte("event: response.completed\ndata: "), append(body, []byte("\n\n")...)...)}, nil
	}
	if typ, _ := payload["type"].(string); typ == "content_block_delta" {
		if delta, ok := payload["delta"].(map[string]any); ok {
			if text, ok := delta["text"].(string); ok && text != "" {
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
			st.usage.seen = true
		}
	}
	if eventType == "response.output_text.delta" || stringValue(payload["type"]) == "response.output_text.delta" {
		if content := stringValue(payload["delta"]); content != "" {
			outputs := make([][]byte, 0, 3)
			if !st.started {
				start, err := codexAnthropicStartChunks(st)
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
			return outputs, nil
		}
	}
	if eventType == "response.completed" || stringValue(payload["type"]) == "response.completed" {
		return codexAnthropicStopChunks(st)
	}
	return nil, nil
}

func codexAnthropicStartChunks(st *codexToAnthropicStreamState) ([][]byte, error) {
	inputTokens := int64(0)
	if st != nil && st.usage.seen {
		inputTokens = st.usage.inputTokens
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

func codexAnthropicStopChunks(st *codexToAnthropicStreamState) ([][]byte, error) {
	outputs := make([][]byte, 0, 5)
	if st != nil && !st.started {
		start, err := codexAnthropicStartChunks(st)
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
		outputTokens = st.usage.outputTokens
	}
	messageDelta, err := marshalEventSSE("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": "end_turn",
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
