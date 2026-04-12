package builtin

import (
	"context"
	"strings"

	"github.com/bytedance/sonic"
)

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
