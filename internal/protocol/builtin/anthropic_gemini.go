package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
)

type anthropicMessagesRequest struct {
	Model      string                    `json:"model"`
	Messages   []anthropicMessageContent `json:"messages"`
	Stream     bool                      `json:"stream,omitempty"`
	System     any                       `json:"system,omitempty"`
	Tools      json.RawMessage           `json:"tools,omitempty"`
	ToolChoice json.RawMessage           `json:"tool_choice,omitempty"`
}

type anthropicMessageContent struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicMessagesResponse struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Role       string                 `json:"role"`
	Content    []anthropicTextBlock   `json:"content"`
	Model      string                 `json:"model"`
	StopReason string                 `json:"stop_reason"`
	Usage      anthropicMessagesUsage `json:"usage"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicMessagesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type anthropicToGeminiStreamState struct {
	model      string
	toolName   string
	toolJSON   string
	toolActive bool
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

	content := ""
	if len(resp.Candidates) > 0 {
		for _, part := range resp.Candidates[0].Content.Parts {
			content += part.Text
		}
	}

	out := anthropicMessagesResponse{
		ID:         "msg-proxy",
		Type:       "message",
		Role:       "assistant",
		Content:    []anthropicTextBlock{{Type: "text", Text: content}},
		Model:      model,
		StopReason: "end_turn",
		Usage: anthropicMessagesUsage{
			InputTokens:  resp.UsageMetadata.PromptTokenCount,
			OutputTokens: resp.UsageMetadata.CandidatesTokenCount,
		},
	}
	if out.Model == "" {
		out.Model = resp.ModelVersion
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
	started bool
}

func convertGeminiResponseToAnthropicStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if *param == nil {
		*param = &anthropicStreamState{}
	}
	st := (*param).(*anthropicStreamState)

	line := strings.TrimSpace(string(rawJSON))
	if line == "" {
		return nil, nil
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "[DONE]" {
		if !st.started {
			return [][]byte{
				[]byte(fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-proxy\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":%q,\"stop_reason\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}\n\n", model)),
				[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"),
				[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"),
				[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\n"),
				[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			}, nil
		}
		return [][]byte{
			[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"),
			[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\n"),
			[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
		}, nil
	}

	var resp geminiResponse
	if err := sonic.Unmarshal([]byte(line), &resp); err != nil {
		return nil, err
	}
	content := ""
	if len(resp.Candidates) > 0 {
		for _, part := range resp.Candidates[0].Content.Parts {
			content += part.Text
		}
	}
	if content == "" {
		return nil, nil
	}

	delta := []byte(fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", content))
	if st.started {
		return [][]byte{delta}, nil
	}
	st.started = true
	return [][]byte{
		[]byte(fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-proxy\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":%q,\"stop_reason\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n", model)),
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"),
		bytes.Clone(delta),
	}, nil
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
		}
		return nil, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_start" {
		if block, _ := payload["content_block"].(map[string]any); block != nil && stringValue(block["type"]) == "tool_use" {
			st.toolName = stringValue(block["name"])
			st.toolJSON = ""
			st.toolActive = true
		}
		return nil, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_delta" {
		if delta, _ := payload["delta"].(map[string]any); delta != nil {
			if stringValue(delta["type"]) == "input_json_delta" && st.toolActive {
				st.toolJSON += stringValue(delta["partial_json"])
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
	if typ := stringValue(payload["type"]); typ == "content_block_stop" && st.toolActive {
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
	if typ := stringValue(payload["type"]); typ == "message_delta" {
		usage, _ := payload["usage"].(map[string]any)
		outputTokens := int64Value(usage["output_tokens"])
		inputTokens := int64Value(usage["input_tokens"])
		totalTokens := inputTokens + outputTokens
		if totalTokens == 0 && outputTokens > 0 {
			totalTokens = outputTokens
		}
		finishReason := ""
		if delta, _ := payload["delta"].(map[string]any); delta != nil {
			finishReason = mapAnthropicStopReasonToGemini(stringValue(delta["stop_reason"]))
		}
		body, err := marshalDataSSE(buildGeminiPayloadFromParts(st.model, "", nil, finishReason, inputTokens, outputTokens, totalTokens, usage != nil))
		if err != nil {
			return nil, err
		}
		return [][]byte{body}, nil
	}
	return nil, nil
}
