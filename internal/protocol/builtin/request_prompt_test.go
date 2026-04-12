package builtin

import (
	"encoding/json"
	"testing"
)

func TestNormalizeOpenAIConversation_StructuredContent(t *testing.T) {
	t.Parallel()

	req := openAIChatRequest{
		Model: "gpt-4o",
		Messages: []openAIChatMessage{
			{Role: "system", Content: "be careful"},
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png", "detail": "high"}},
			}},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "calling tool"}}, ToolCalls: []openAIChatToolCall{{ID: "call_1", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "search", Arguments: `{"query":"go"}`}}}},
			{Role: "tool", ToolCallID: "call_1", Content: "done"},
		},
		Tools:      json.RawMessage(`[{"type":"function","function":{"name":"search","description":"lookup","parameters":{"type":"object"}}}]`),
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"search"}}`),
	}

	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		t.Fatalf("normalizeOpenAIConversation failed: %v", err)
	}
	if len(conv.Tools) != 1 || conv.Tools[0].Name != "search" {
		t.Fatalf("unexpected tools: %+v", conv.Tools)
	}
	if conv.ToolChoice.Mode != "named" || conv.ToolChoice.Name != "search" {
		t.Fatalf("unexpected tool choice: %+v", conv.ToolChoice)
	}
	if len(conv.Turns) != 4 {
		t.Fatalf("expected 4 turns, got %d", len(conv.Turns))
	}
	if len(conv.Turns[1].Parts) != 2 || conv.Turns[1].Parts[1].Kind != partKindImage {
		t.Fatalf("expected user image part, got %+v", conv.Turns[1].Parts)
	}
	if got := conv.Turns[2].Parts[1].Kind; got != partKindToolCall {
		t.Fatalf("expected assistant tool_call part, got %s", got)
	}
	toolResult := conv.Turns[3].Parts[0].ToolResult
	if toolResult == nil || toolResult.CallID != "call_1" || toolResult.Name != "search" {
		t.Fatalf("unexpected tool result: %+v", toolResult)
	}
}

func TestNormalizeAnthropicConversation_StructuredContent(t *testing.T) {
	t.Parallel()

	req := anthropicMessagesRequest{
		Model:  "claude-3-5-sonnet",
		System: []any{map[string]any{"type": "text", "text": "keep format"}},
		Messages: []anthropicMessageContent{
			{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"query": "go"}}}},
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png", "media_type": "image/png"}},
				map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "cGRm"}, "title": "doc.pdf"},
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"},
			}},
		},
		Tools:      json.RawMessage(`[{"name":"lookup","description":"lookup","input_schema":{"type":"object"}}]`),
		ToolChoice: json.RawMessage(`{"type":"tool","name":"lookup"}`),
	}

	conv, err := normalizeAnthropicConversation(req)
	if err != nil {
		t.Fatalf("normalizeAnthropicConversation failed: %v", err)
	}
	if len(conv.Turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(conv.Turns))
	}
	userParts := conv.Turns[2].Parts
	if len(userParts) != 4 || userParts[1].Kind != partKindImage || userParts[2].Kind != partKindFile || userParts[3].Kind != partKindToolResult {
		t.Fatalf("unexpected user parts: %+v", userParts)
	}
	if toolResult := userParts[3].ToolResult; toolResult == nil || toolResult.Name != "lookup" {
		t.Fatalf("expected resolved tool result name, got %+v", toolResult)
	}
}

func TestNormalizeCodexConversation_StructuredContent(t *testing.T) {
	t.Parallel()

	req := codexRequest{
		Model:        "gpt-5-codex",
		Instructions: "be careful",
		Input: []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"https://example.com/a.png"},{"type":"input_file","file_id":"file_123","filename":"doc.pdf"}]}`),
			json.RawMessage(`{"type":"function_call","call_id":"call_1","name":"search","arguments":{"query":"go"}}`),
			json.RawMessage(`{"type":"function_call_output","call_id":"call_1","output":"done"}`),
		},
		Tools:      json.RawMessage(`[{"type":"function","name":"search","description":"lookup","parameters":{"type":"object"}}]`),
		ToolChoice: json.RawMessage(`"required"`),
	}

	conv, err := normalizeCodexConversation(req)
	if err != nil {
		t.Fatalf("normalizeCodexConversation failed: %v", err)
	}
	if len(conv.Turns) != 4 {
		t.Fatalf("expected 4 turns, got %d", len(conv.Turns))
	}
	if len(conv.Turns[1].Parts) != 3 || conv.Turns[1].Parts[1].Kind != partKindImage || conv.Turns[1].Parts[2].Kind != partKindFile {
		t.Fatalf("unexpected codex user parts: %+v", conv.Turns[1].Parts)
	}
	if conv.Turns[2].Parts[0].Kind != partKindToolCall {
		t.Fatalf("expected tool_call turn, got %+v", conv.Turns[2].Parts)
	}
	if toolResult := conv.Turns[3].Parts[0].ToolResult; toolResult == nil || toolResult.Name != "search" {
		t.Fatalf("expected resolved tool result, got %+v", toolResult)
	}
}

func TestNormalizeConversation_RejectsUnknownBlockType(t *testing.T) {
	t.Parallel()

	_, err := normalizeOpenAIConversation(openAIChatRequest{
		Model: "gpt-4o",
		Messages: []openAIChatMessage{{
			Role:    "user",
			Content: []any{map[string]any{"type": "mystery", "value": true}},
		}},
	})
	if err == nil {
		t.Fatal("expected unsupported block error")
	}
}
