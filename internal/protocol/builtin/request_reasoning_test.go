package builtin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRequestReasoning(t *testing.T) {
	t.Run("anthropic assistant thinking becomes reasoning part", func(t *testing.T) {
		req := anthropicMessagesRequest{
			Model: "claude-3-5-sonnet",
			Messages: []anthropicMessageContent{{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "thinking", "thinking": "step by step"},
					map[string]any{"type": "redacted_thinking", "data": "enc_1"},
					map[string]any{"type": "text", "text": "hello"},
				},
			}},
		}

		conv, err := normalizeAnthropicConversation(req)
		if err != nil {
			t.Fatalf("normalizeAnthropicConversation failed: %v", err)
		}
		if len(conv.Turns) != 1 || len(conv.Turns[0].Parts) != 3 {
			t.Fatalf("unexpected turns: %+v", conv.Turns)
		}
		if got := conv.Turns[0].Parts[0].Kind; got != "reasoning" {
			t.Fatalf("expected first part to be reasoning, got %q", got)
		}

		reasoning := reflect.ValueOf(conv.Turns[0].Parts[0]).FieldByName("Reasoning")
		if !reasoning.IsValid() || reasoning.IsNil() {
			t.Fatalf("expected reasoning payload on first part, got %+v", conv.Turns[0].Parts[0])
		}
		if got := reasoning.Elem().FieldByName("Subtype"); !got.IsValid() || got.String() != "thinking" {
			t.Fatalf("expected thinking subtype, got %+v", reasoning.Interface())
		}

		redacted := reflect.ValueOf(conv.Turns[0].Parts[1]).FieldByName("Reasoning")
		if !redacted.IsValid() || redacted.IsNil() {
			t.Fatalf("expected reasoning payload on second part, got %+v", conv.Turns[0].Parts[1])
		}
		if got := redacted.Elem().FieldByName("Subtype"); !got.IsValid() || got.String() != "redacted_thinking" {
			t.Fatalf("expected redacted_thinking subtype, got %+v", redacted.Interface())
		}
	})

	t.Run("openai request flattens thinking into reasoning_content string only", func(t *testing.T) {
		conv := conversation{
			Turns: []conversationTurn{{
				Role: "assistant",
				Parts: []conversationPart{
					newReasoningPart("thinking", "first thought", "", ""),
					newReasoningPart("redacted_thinking", "", "", "enc_blob"),
					newReasoningPart("thinking", "second thought", "sig_xyz", ""),
					{Kind: partKindText, Text: "final answer"},
				},
			}},
		}

		raw, err := encodeOpenAIRequest("gpt-x", conv, false)
		if err != nil {
			t.Fatalf("encodeOpenAIRequest failed: %v", err)
		}
		body := string(raw)
		if strings.Contains(body, `"reasoning":[`) {
			t.Fatalf("openai request must not emit reasoning array, got: %s", body)
		}
		if strings.Contains(body, "redacted_thinking") || strings.Contains(body, "enc_blob") || strings.Contains(body, "sig_xyz") {
			t.Fatalf("openai request must drop redacted_thinking and signatures, got: %s", body)
		}
		if !strings.Contains(body, `"reasoning_content":"first thought\n\nsecond thought"`) {
			t.Fatalf("expected reasoning_content to join thinking texts with blank line, got: %s", body)
		}
		if !strings.Contains(body, `"final answer"`) {
			t.Fatalf("expected sibling text preserved, got: %s", body)
		}
	})

	t.Run("gemini request drops reasoning but keeps sibling text and tool content", func(t *testing.T) {
		conv := conversation{
			Turns: []conversationTurn{{
				Role: "assistant",
				Parts: []conversationPart{
					{Kind: "reasoning"},
					{Kind: partKindText, Text: "public"},
					{Kind: partKindToolCall, ToolCall: &conversationToolCall{
						ID:        "call_1",
						Name:      "lookup",
						Arguments: json.RawMessage(`{"q":"go"}`),
					}},
				},
			}},
		}

		raw, err := encodeGeminiRequest(conv)
		if err != nil {
			t.Fatalf("encodeGeminiRequest failed: %v", err)
		}
		body := string(raw)
		if !strings.Contains(body, `"public"`) || !strings.Contains(body, `"lookup"`) {
			t.Fatalf("unexpected gemini request body: %s", body)
		}
		if strings.Contains(body, `"reasoning"`) {
			t.Fatalf("expected gemini request to drop reasoning content, got %s", body)
		}
	})
}
