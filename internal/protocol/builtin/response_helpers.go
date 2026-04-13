package builtin

import (
	"fmt"

	"github.com/bytedance/sonic"
)

func marshalDataSSE(payload any) ([]byte, error) {
	body, err := sonic.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), append(body, []byte("\n\n")...)...), nil
}

func marshalEventSSE(event string, payload any) ([]byte, error) {
	body, err := sonic.Marshal(payload)
	if err != nil {
		return nil, err
	}
	prefix := []byte(fmt.Sprintf("event: %s\ndata: ", event))
	return append(prefix, append(body, []byte("\n\n")...)...), nil
}

func mapOpenAIFinishReasonToGemini(reason string) string {
	switch reason {
	case "":
		return ""
	case "length":
		return "MAX_TOKENS"
	default:
		return "STOP"
	}
}

func mapOpenAIFinishReasonToAnthropic(reason string) string {
	switch reason {
	case "", "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func mapAnthropicStopReasonToGemini(reason string) string {
	switch reason {
	case "max_tokens":
		return "MAX_TOKENS"
	case "":
		return ""
	default:
		return "STOP"
	}
}

func buildGeminiPayload(model, text, finishReason string, promptTokens, candidateTokens, totalTokens int64, includeUsage bool) map[string]any {
	candidate := map[string]any{
		"content": map[string]any{
			"role":  "model",
			"parts": make([]map[string]any, 0, 1),
		},
	}
	parts := candidate["content"].(map[string]any)["parts"].([]map[string]any)
	if text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	candidate["content"].(map[string]any)["parts"] = parts
	if finishReason != "" {
		candidate["finishReason"] = finishReason
	}
	payload := map[string]any{"candidates": []map[string]any{candidate}}
	if model != "" {
		payload["modelVersion"] = model
	}
	if includeUsage {
		payload["usageMetadata"] = map[string]any{
			"promptTokenCount":     promptTokens,
			"candidatesTokenCount": candidateTokens,
			"totalTokenCount":      totalTokens,
		}
	}
	return payload
}

func buildGeminiPayloadFromParts(model, responseID string, parts []geminiPart, finishReason string, promptTokens, candidateTokens, totalTokens int64, includeUsage bool) map[string]any {
	if parts == nil {
		parts = []geminiPart{}
	}
	candidate := map[string]any{
		"content": map[string]any{
			"role":  "model",
			"parts": parts,
		},
	}
	if finishReason != "" {
		candidate["finishReason"] = finishReason
	}
	payload := map[string]any{
		"candidates": []map[string]any{candidate},
	}
	if model != "" {
		payload["modelVersion"] = model
	}
	if responseID != "" {
		payload["responseId"] = responseID
	}
	if includeUsage {
		payload["usageMetadata"] = map[string]any{
			"promptTokenCount":     promptTokens,
			"candidatesTokenCount": candidateTokens,
			"totalTokenCount":      totalTokens,
		}
	}
	return payload
}

func geminiPartsFromConversationParts(parts []conversationPart) ([]geminiPart, error) {
	out := make([]geminiPart, 0, len(parts))
	for _, part := range parts {
		encoded, err := encodeGeminiPart(part)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded)
	}
	return out, nil
}

func decodeObjectSlice(value any) ([]map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	body, err := sonic.Marshal(value)
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	if err := sonic.Unmarshal(body, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func geminiPartsFromAnthropicContent(value any) ([]geminiPart, error) {
	blocks, err := decodeObjectSlice(value)
	if err != nil {
		return nil, err
	}
	parts := make([]conversationPart, 0, len(blocks))
	for _, block := range blocks {
		part, err := decodeAnthropicContentBlock(block)
		if err != nil {
			return nil, err
		}
		if part.Kind != "" {
			parts = append(parts, part)
		}
	}
	return geminiPartsFromConversationParts(parts)
}

func geminiPartsFromCodexOutput(value any) ([]geminiPart, error) {
	items, err := decodeObjectSlice(value)
	if err != nil {
		return nil, err
	}
	parts := make([]conversationPart, 0, len(items))
	for _, item := range items {
		switch normalizeRole(stringValue(item["type"])) {
		case "message":
			contentParts, err := extractCodexContentParts(item["content"])
			if err != nil {
				return nil, err
			}
			parts = append(parts, contentParts...)
		case "function_call":
			call, err := decodeCodexToolCall(item)
			if err != nil {
				return nil, err
			}
			parts = append(parts, conversationPart{Kind: partKindToolCall, ToolCall: &call})
		case "function_call_output":
			result, err := decodeCodexToolResult(item)
			if err != nil {
				return nil, err
			}
			parts = append(parts, conversationPart{Kind: partKindToolResult, ToolResult: &result})
		}
	}
	return geminiPartsFromConversationParts(parts)
}
