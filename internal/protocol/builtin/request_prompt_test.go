package builtin

import (
	"encoding/json"
	"testing"

	"ccLoad/internal/protocol"
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

func TestNormalizeGeminiConversation_StructuredContent(t *testing.T) {
	t.Parallel()

	req := geminiRequestPayload{
		SystemInstruction: &geminiSystemInstruction{
			Parts: []geminiPart{{Text: "be careful"}},
		},
		Tools: []geminiTool{{
			FunctionDeclarations: []geminiFunctionDeclaration{{
				Name:        "search",
				Description: "lookup",
				Parameters:  map[string]any{"type": "object"},
			}},
		}},
		ToolConfig: &geminiToolConfig{
			FunctionCallingConfig: geminiFunctionCallingConfig{
				Mode:                 "ANY",
				AllowedFunctionNames: []string{"search"},
			},
		},
		Contents: []geminiContent{
			{
				Role: "model",
				Parts: []geminiPart{
					{Text: "calling tool"},
					{FunctionCall: &geminiFunctionCall{Name: "search", Args: map[string]any{"query": "go"}}},
				},
			},
			{
				Role: "user",
				Parts: []geminiPart{
					{Text: "hello"},
					{FileData: &geminiFileData{MIMEType: "image/png", FileURI: "https://example.com/a.png"}},
					{InlineData: &geminiInlineData{MIMEType: "application/pdf", Data: "cGRm"}},
					{FunctionResponse: &geminiFunctionResponse{Name: "search", Response: map[string]any{"call_id": "call_1", "content": "done"}}},
				},
			},
		},
	}

	conv, err := normalizeGeminiConversation(req)
	if err != nil {
		t.Fatalf("normalizeGeminiConversation failed: %v", err)
	}
	if len(conv.Turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(conv.Turns))
	}
	if len(conv.Tools) != 1 || conv.Tools[0].Name != "search" {
		t.Fatalf("unexpected tools: %+v", conv.Tools)
	}
	if conv.ToolChoice.Mode != "named" || conv.ToolChoice.Name != "search" {
		t.Fatalf("unexpected tool choice: %+v", conv.ToolChoice)
	}
	assistantParts := conv.Turns[1].Parts
	if len(assistantParts) != 2 || assistantParts[1].Kind != partKindToolCall {
		t.Fatalf("unexpected assistant parts: %+v", assistantParts)
	}
	if call := assistantParts[1].ToolCall; call == nil || call.ID != "call_1" {
		t.Fatalf("expected matched tool call id call_1, got %+v", call)
	}
	userParts := conv.Turns[2].Parts
	if len(userParts) != 4 || userParts[1].Kind != partKindImage || userParts[2].Kind != partKindFile || userParts[3].Kind != partKindToolResult {
		t.Fatalf("unexpected user parts: %+v", userParts)
	}
	if toolResult := userParts[3].ToolResult; toolResult == nil || toolResult.CallID != "call_1" || toolResult.Name != "search" {
		t.Fatalf("unexpected gemini tool result: %+v", toolResult)
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

func TestNormalizeConversationCoverage_SupportedSources(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		protocol  protocol.Protocol
		normalize func() (conversation, error)
	}{
		{
			name:     "openai",
			protocol: protocol.OpenAI,
			normalize: func() (conversation, error) {
				return normalizeOpenAIConversation(openAIChatRequest{
					Model: "gpt-4o",
					Messages: []openAIChatMessage{{
						Role:    "user",
						Content: "hello",
					}},
				})
			},
		},
		{
			name:     "anthropic",
			protocol: protocol.Anthropic,
			normalize: func() (conversation, error) {
				return normalizeAnthropicConversation(anthropicMessagesRequest{
					Model: "claude-3-5-sonnet",
					Messages: []anthropicMessageContent{{
						Role:    "user",
						Content: []any{map[string]any{"type": "text", "text": "hello"}},
					}},
				})
			},
		},
		{
			name:     "codex",
			protocol: protocol.Codex,
			normalize: func() (conversation, error) {
				return normalizeCodexConversation(codexRequest{
					Model: "gpt-5-codex",
					Input: []json.RawMessage{
						json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`),
					},
				})
			},
		},
	}

	if len(cases) != 3 {
		t.Fatalf("expected exactly three covered request-source normalizers, got %d", len(cases))
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			conv, err := tc.normalize()
			if err != nil {
				t.Fatalf("%s normalize failed: %v", tc.protocol, err)
			}
			if len(conv.Turns) != 1 {
				t.Fatalf("%s normalize expected 1 turn, got %d", tc.protocol, len(conv.Turns))
			}
			if conv.Turns[0].Role != "user" {
				t.Fatalf("%s normalize expected user role, got %s", tc.protocol, conv.Turns[0].Role)
			}
			if len(conv.Turns[0].Parts) != 1 || conv.Turns[0].Parts[0].Kind != partKindText || conv.Turns[0].Parts[0].Text != "hello" {
				t.Fatalf("%s normalize expected one text part, got %+v", tc.protocol, conv.Turns[0].Parts)
			}
		})
	}
}

func TestNormalizeConversationCoverage_GeminiSourceHasRequestTransforms(t *testing.T) {
	t.Parallel()

	if got := protocol.DetectRequestFamily("/v1beta/models/gemini-2.5-pro:generateContent"); got != protocol.RequestFamilyGenerateContent {
		t.Fatalf("expected generate_content family for Gemini request, got %s", got)
	}

	for _, upstream := range []protocol.Protocol{protocol.OpenAI, protocol.Anthropic, protocol.Codex} {
		if !protocol.SupportsTransform(protocol.Gemini, upstream) {
			t.Fatalf("expected Gemini source to be supported for upstream %s", upstream)
		}
		if !protocol.SupportsTransformFamily(protocol.Gemini, upstream, protocol.RequestFamilyGenerateContent) {
			t.Fatalf("expected Gemini generate_content source family to be supported for upstream %s", upstream)
		}
	}
}
