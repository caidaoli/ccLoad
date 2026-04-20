package builtin

import (
	"encoding/json"
	"testing"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
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

func TestEncodeCodexRequest_DropsAnthropicToolResultIsError(t *testing.T) {
	t.Parallel()

	req := anthropicMessagesRequest{
		Model: "gpt-5-codex",
		Messages: []anthropicMessageContent{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "lookup",
					"input": map[string]any{"query": "go"},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"is_error":    true,
					"content":     "quota exceeded",
				},
			}},
		},
	}

	conv, err := normalizeAnthropicConversation(req)
	if err != nil {
		t.Fatalf("normalizeAnthropicConversation failed: %v", err)
	}

	raw, err := encodeCodexRequest("gpt-5-codex", conv, false)
	if err != nil {
		t.Fatalf("encodeCodexRequest failed: %v", err)
	}

	var encoded codexRequest
	if err := sonic.Unmarshal(raw, &encoded); err != nil {
		t.Fatalf("unmarshal codex request failed: %v", err)
	}
	if len(encoded.Input) != 2 {
		t.Fatalf("expected 2 input items, got %d", len(encoded.Input))
	}

	var toolResult map[string]any
	if err := sonic.Unmarshal(encoded.Input[1], &toolResult); err != nil {
		t.Fatalf("unmarshal tool result item failed: %v", err)
	}
	if toolResult["type"] != "function_call_output" {
		t.Fatalf("expected function_call_output, got %+v", toolResult)
	}
	if _, ok := toolResult["is_error"]; ok {
		t.Fatalf("expected codex tool result without is_error, got %+v", toolResult)
	}
	if toolResult["output"] != "quota exceeded" {
		t.Fatalf("unexpected tool result output: %+v", toolResult)
	}
}

func TestEncodeCodexRequest_AssistantTextUsesOutputText(t *testing.T) {
	t.Parallel()

	conv := conversation{
		Turns: []conversationTurn{{
			Role: "assistant",
			Parts: []conversationPart{{
				Kind: partKindText,
				Text: "hello",
			}},
		}},
	}

	raw, err := encodeCodexRequest("gpt-5-codex", conv, false)
	if err != nil {
		t.Fatalf("encodeCodexRequest failed: %v", err)
	}

	var encoded codexRequest
	if err := sonic.Unmarshal(raw, &encoded); err != nil {
		t.Fatalf("unmarshal codex request failed: %v", err)
	}
	if len(encoded.Input) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(encoded.Input))
	}

	var message map[string]any
	if err := sonic.Unmarshal(encoded.Input[0], &message); err != nil {
		t.Fatalf("unmarshal assistant message failed: %v", err)
	}
	if message["type"] != "message" || message["role"] != "assistant" {
		t.Fatalf("unexpected assistant message: %+v", message)
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("unexpected assistant content: %+v", message["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["type"] != "output_text" || part["text"] != "hello" {
		t.Fatalf("unexpected assistant content part: %+v", content[0])
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

func TestNormalizeConversation_BuiltinToolsAndChoices(t *testing.T) {
	t.Parallel()

	openAIConv, err := normalizeOpenAIConversation(openAIChatRequest{
		Model:      "gpt-4o",
		Tools:      json.RawMessage(`[{"type":"web_search","search_context_size":"high"}]`),
		ToolChoice: json.RawMessage(`{"type":"web_search"}`),
		Messages: []openAIChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("normalizeOpenAIConversation failed: %v", err)
	}
	if len(openAIConv.Tools) != 1 || openAIConv.Tools[0].Type != "web_search" || openAIConv.Tools[0].Options["search_context_size"] != "high" {
		t.Fatalf("unexpected openai builtin tools: %+v", openAIConv.Tools)
	}
	if openAIConv.ToolChoice.Mode != "named" || openAIConv.ToolChoice.ToolType != "web_search" {
		t.Fatalf("unexpected openai builtin tool choice: %+v", openAIConv.ToolChoice)
	}

	codexConv, err := normalizeCodexConversation(codexRequest{
		Model:      "gpt-5-codex",
		Tools:      json.RawMessage(`[{"type":"web_search","user_location":{"type":"approximate","country":"US"}}]`),
		ToolChoice: json.RawMessage(`{"type":"web_search"}`),
		Input: []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`),
		},
	})
	if err != nil {
		t.Fatalf("normalizeCodexConversation failed: %v", err)
	}
	if len(codexConv.Tools) != 1 || codexConv.Tools[0].Type != "web_search" {
		t.Fatalf("unexpected codex builtin tools: %+v", codexConv.Tools)
	}
	location, ok := codexConv.Tools[0].Options["user_location"].(map[string]any)
	if !ok || location["country"] != "US" {
		t.Fatalf("unexpected codex builtin tool options: %+v", codexConv.Tools[0].Options)
	}
	if codexConv.ToolChoice.Mode != "named" || codexConv.ToolChoice.ToolType != "web_search" {
		t.Fatalf("unexpected codex builtin tool choice: %+v", codexConv.ToolChoice)
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

// 覆盖 bug：OpenAI→Codex 转换时 tool_choice="auto"/"none"/"required" 必须以字符串形式传给
// Responses API；若包装为 {"type":"auto"} 对象会被上游拒绝（Responses API 对 tool_choice.type
// 的对象形态只接受 builtin 工具类型如 file_search）。
func TestEncodeCodexRequest_ToolChoiceStringModes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mode string
	}{
		{"auto", "auto"},
		{"none", "none"},
		{"required", "required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conv := conversation{
				Turns:      []conversationTurn{{Role: "user", Parts: []conversationPart{{Kind: partKindText, Text: "hi"}}}},
				ToolChoice: conversationToolChoice{Mode: tc.mode},
			}
			raw, err := encodeCodexRequest("gpt-5-codex", conv, false)
			if err != nil {
				t.Fatalf("encodeCodexRequest failed: %v", err)
			}
			var out map[string]any
			if err := sonic.Unmarshal(raw, &out); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			got, ok := out["tool_choice"].(string)
			if !ok {
				t.Fatalf("expected tool_choice to be string %q, got %#v", tc.mode, out["tool_choice"])
			}
			if got != tc.mode {
				t.Fatalf("expected tool_choice %q, got %q", tc.mode, got)
			}
		})
	}
}

func TestEncodeCodexRequest_ToolChoiceNamedFunctionRemainsObject(t *testing.T) {
	t.Parallel()

	conv := conversation{
		Turns: []conversationTurn{{Role: "user", Parts: []conversationPart{{Kind: partKindText, Text: "hi"}}}},
		Tools: []conversationTool{{Name: "get_weather", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: conversationToolChoice{
			Mode:     "named",
			Name:     "get_weather",
			ToolType: "function",
		},
	}
	raw, err := encodeCodexRequest("gpt-5-codex", conv, false)
	if err != nil {
		t.Fatalf("encodeCodexRequest failed: %v", err)
	}
	var out map[string]any
	if err := sonic.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	choice, ok := out["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("expected tool_choice to be object, got %#v", out["tool_choice"])
	}
	if choice["type"] != "function" || choice["name"] != "get_weather" {
		t.Fatalf("unexpected named tool_choice: %#v", choice)
	}
}
