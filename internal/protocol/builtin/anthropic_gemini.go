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
