package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
)

func convertOpenAIRequestToAnthropic(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req openAIChatRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	out := anthropicMessagesRequest{
		Model:    model,
		Messages: make([]anthropicMessageContent, 0, len(req.Messages)),
		Stream:   stream,
	}
	prompt, err := normalizeOpenAITextPrompt(req)
	if err != nil {
		return nil, err
	}
	if prompt.System != "" {
		out.System = []anthropicTextBlock{{Type: "text", Text: prompt.System}}
	}
	for _, msg := range prompt.Messages {
		out.Messages = append(out.Messages, anthropicMessageContent{
			Role: msg.Role,
			Content: []anthropicTextBlock{{
				Type: "text",
				Text: msg.Text,
			}},
		})
	}
	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("no convertible openai messages")
	}
	return sonic.Marshal(out)
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
