package gemini

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestConvertGeminiRequestToClaude_PreservesCustomToolIDs(t *testing.T) {
	tests := []struct {
		name          string
		callField     string
		responseField string
		want          string
	}{
		{
			name:          "id",
			callField:     `"id":"call_gateway_id"`,
			responseField: `"id":"call_gateway_id"`,
			want:          "call_gateway_id",
		},
		{
			name:          "call_id",
			callField:     `"call_id":"call_gateway_call_id"`,
			responseField: `"call_id":"call_gateway_call_id"`,
			want:          "call_gateway_call_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{
				"contents": [
					{
						"role": "model",
						"parts": [
							{"functionCall": {"name": "lookup", %s, "args": {"query": "status"}}}
						]
					},
					{
						"role": "user",
						"parts": [
							{"functionResponse": {"name": "lookup", %s, "response": {"result": "ok"}}}
						]
					}
				]
			}`, tt.callField, tt.responseField))

			out, _ := ConvertGeminiRequestToClaude("claude-sonnet-4", raw, false)

			gotCallID := ojson.GetBytesNoCopy(out, "messages.0.content.0.id").String()
			if gotCallID != tt.want {
				t.Fatalf("expected tool_use id %q, got %q; output=%s", tt.want, gotCallID, string(out))
			}

			gotResultID := ojson.GetBytesNoCopy(out, "messages.1.content.0.tool_use_id").String()
			if gotResultID != tt.want {
				t.Fatalf("expected tool_result tool_use_id %q, got %q; output=%s", tt.want, gotResultID, string(out))
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_GroupsConsecutiveRoleTurns(t *testing.T) {
	raw := []byte(`{
		"contents":[
			{"role":"model","parts":[{"text":"answer"}]},
			{"role":"model","parts":[{"functionCall":{"name":"first","id":"call_1","args":{}}}]},
			{"role":"model","parts":[{"functionCall":{"name":"second","id":"call_2","args":{}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"first","id":"call_1","response":{"result":"one"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"second","id":"call_2","response":{"result":"two"}}}]}
		]
	}`)

	out, _ := ConvertGeminiRequestToClaude("claude-test", raw, false)
	messages := ojson.GetBytesNoCopy(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), string(out))
	}
	assistantContent := messages[0].Get("content").Array()
	wantAssistantTypes := []string{"text", "tool_use", "tool_use"}
	if len(assistantContent) != len(wantAssistantTypes) {
		t.Fatalf("assistant content count = %d, want %d. Output: %s", len(assistantContent), len(wantAssistantTypes), string(out))
	}
	for i, wantType := range wantAssistantTypes {
		if got := assistantContent[i].Get("type").String(); got != wantType {
			t.Fatalf("assistant content[%d].type = %q, want %q", i, got, wantType)
		}
	}
	userContent := messages[1].Get("content").Array()
	if len(userContent) != 2 {
		t.Fatalf("user content count = %d, want 2. Output: %s", len(userContent), string(out))
	}
	for i, wantID := range []string{"call_1", "call_2"} {
		if got := userContent[i].Get("type").String(); got != "tool_result" {
			t.Fatalf("user content[%d].type = %q, want tool_result", i, got)
		}
		if got := userContent[i].Get("tool_use_id").String(); got != wantID {
			t.Fatalf("user content[%d].tool_use_id = %q, want %q", i, got, wantID)
		}
	}
}

func TestConvertGeminiRequestToClaude_KeepsSystemInstructionUserSeparate(t *testing.T) {
	raw := []byte(`{
		"system_instruction":{"parts":[{"text":"system rule"}]},
		"contents":[{"role":"user","parts":[{"text":"question"}]}]
	}`)
	out, _ := ConvertGeminiRequestToClaude("claude-test", raw, false)
	messages := ojson.GetBytesNoCopy(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), string(out))
	}
	if got := messages[0].Get("content.0.text").String(); got != "system rule" {
		t.Fatalf("system user text = %q, want system rule", got)
	}
	if got := messages[1].Get("content.0.text").String(); got != "question" {
		t.Fatalf("ordinary user text = %q, want question", got)
	}
}

func TestConvertGeminiRequestToClaude_SupportsCamelCaseSystemInstruction(t *testing.T) {
	raw := []byte(`{
		"systemInstruction":{"parts":[{"text":"system rule in camelCase"}]},
		"contents":[{"role":"user","parts":[{"text":"question"}]}]
	}`)
	out, _ := ConvertGeminiRequestToClaude("claude-test", raw, false)
	messages := ojson.GetBytesNoCopy(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2. Output: %s", len(messages), string(out))
	}
	if got := messages[0].Get("content.0.text").String(); got != "system rule in camelCase" {
		t.Fatalf("system user text = %q, want system rule in camelCase", got)
	}
	if got := messages[1].Get("content.0.text").String(); got != "question" {
		t.Fatalf("ordinary user text = %q, want question", got)
	}
}

func TestConvertGeminiRequestToClaude_DropsTemperature(t *testing.T) {
	raw := []byte(`{
		"generationConfig": {
			"temperature": 0.2,
			"topP": 0.8
		},
		"contents": [
			{
				"role": "user",
				"parts": [{"text": "hi"}]
			}
		]
	}`)

	out, _ := ConvertGeminiRequestToClaude("claude-sonnet-5", raw, false)

	if ojson.GetBytesNoCopy(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
	if got := ojson.GetBytesNoCopy(out, "top_p").Float(); got != 0.8 {
		t.Fatalf("top_p = %v, want 0.8", got)
	}
}

func TestConvertGeminiRequestToClaude_AcceptsCamelInlineData(t *testing.T) {
	out, _ := ConvertGeminiRequestToClaude("claude-sonnet-4", []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}]}`), false)
	if got := ojson.GetBytesNoCopy(out, "messages.0.content.0.type").String(); got != "image" {
		t.Fatalf("content type = %q, want image. Output: %s", got, string(out))
	}
	if got := ojson.GetBytesNoCopy(out, "messages.0.content.0.source.media_type").String(); got != "image/png" {
		t.Fatalf("media_type = %q, want image/png. Output: %s", got, string(out))
	}
}

func TestConvertGeminiRequestToClaude_SplitsNonImageInlineDataByMIME(t *testing.T) {
	out, _ := ConvertGeminiRequestToClaude("claude-sonnet-4", []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}},{"inlineData":{"mimeType":"video/mp4","data":"AAAAIGZ0eXA="}},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}}]}]}`), false)

	// A user attachment Claude cannot read is dropped rather than replaced by placeholder text.
	if got := ojson.GetBytesNoCopy(out, "messages.0.content.#").Int(); got != 1 {
		t.Fatalf("user content has %d blocks, want only the document. Output: %s", got, string(out))
	}
	if got := ojson.GetBytesNoCopy(out, "messages.0.content.0.type").String(); got != "document" {
		t.Fatalf("document content type = %q, want document. Output: %s", got, string(out))
	}
	if ojson.GetBytesNoCopy(out, "messages.0.content.#(type==\"image\")").Exists() {
		t.Fatalf("non-image inlineData must not be converted to image. Output: %s", string(out))
	}
	if strings.Contains(string(out), "Media content") {
		t.Fatalf("user attachment was replaced by placeholder text. Output: %s", string(out))
	}
}

func TestConvertGeminiRequestToClaude_DropsHiddenThoughtParts(t *testing.T) {
	t.Run("thought-only turn", func(t *testing.T) {
		out, _ := ConvertGeminiRequestToClaude("claude-test", []byte(`{
			"contents":[
				{"role":"model","parts":[{"thought":true,"text":"internal reasoning","thoughtSignature":"opaque-provider-state"}]},
				{"role":"user","parts":[{"text":"continue"}]}
			]
		}`), false)

		messages := ojson.GetBytesNoCopy(out, "messages").Array()
		if len(messages) != 1 || messages[0].Get("role").String() != "user" || messages[0].Get("content.0.text").String() != "continue" {
			t.Fatalf("hidden thought turn was not dropped. Output: %s", string(out))
		}
	})

	t.Run("mixed turn", func(t *testing.T) {
		out, _ := ConvertGeminiRequestToClaude("claude-test", []byte(`{
			"contents":[{"role":"model","parts":[
				{"thought":true,"text":"internal reasoning","thoughtSignature":"opaque-provider-state"},
				{"text":"visible answer"}
			]}]
		}`), false)

		content := ojson.GetBytesNoCopy(out, "messages.0.content").Array()
		if len(content) != 1 || content[0].Get("type").String() != "text" || content[0].Get("text").String() != "visible answer" {
			t.Fatalf("hidden thought was not dropped independently of visible text. Output: %s", string(out))
		}
	})
}

func TestConvertGeminiRequestToClaude_DeterministicToolIDs(t *testing.T) {
	raw := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "first_tool", "args": {"q": "one"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "first_tool", "response": {"result": "ok1"}}}
				]
			},
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "second_tool", "args": {"q": "two"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "second_tool", "response": {"result": "ok2"}}}
				]
			}
		]
	}`)

	out1, _ := ConvertGeminiRequestToClaude("claude-sonnet-4", raw, false)
	out2, _ := ConvertGeminiRequestToClaude("claude-sonnet-4", raw, false)

	if string(out1) != string(out2) {
		t.Fatalf("expected deterministic output across multiple conversions, got different outputs:\nout1=%s\nout2=%s", string(out1), string(out2))
	}

	wantID1 := "toolu_gemini_0000000000000001"
	wantID2 := "toolu_gemini_0000000000000002"

	gotCall1 := ojson.GetBytesNoCopy(out1, "messages.0.content.0.id").String()
	gotResp1 := ojson.GetBytesNoCopy(out1, "messages.1.content.0.tool_use_id").String()
	gotCall2 := ojson.GetBytesNoCopy(out1, "messages.2.content.0.id").String()
	gotResp2 := ojson.GetBytesNoCopy(out1, "messages.3.content.0.tool_use_id").String()

	if gotCall1 != wantID1 || gotResp1 != wantID1 {
		t.Fatalf("expected first tool pair to have id %q, got call=%q, resp=%q", wantID1, gotCall1, gotResp1)
	}
	if gotCall2 != wantID2 || gotResp2 != wantID2 {
		t.Fatalf("expected second tool pair to have id %q, got call=%q, resp=%q", wantID2, gotCall2, gotResp2)
	}
}

func TestConvertGeminiRequestToClaude_PreservesCallerSuppliedMetadataUserID(t *testing.T) {
	testCases := []struct {
		name     string
		rawJSON  string
		expected string
	}{
		{
			name:     "plain string",
			rawJSON:  `{"model":"claude-test","metadata":{"user_id":"custom-gemini-user-123"},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			expected: "custom-gemini-user-123",
		},
		{
			name:     "special characters and json string",
			rawJSON:  `{"model":"claude-test","metadata":{"user_id":"foo\"bar\nbaz\\qux"},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			expected: "foo\"bar\nbaz\\qux",
		},
		{
			name:     "claude code json format",
			rawJSON:  `{"model":"claude-test","metadata":{"user_id":"{\"device_id\":\"0000000000000000000000000000000000000000000000000000000000000000\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			expected: `{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","session_id":"11111111-2222-4333-8444-555555555555"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := ConvertGeminiRequestToClaude("claude-test", []byte(tc.rawJSON), false)
			if !ojson.ValidBytes(out) {
				t.Fatalf("output is invalid json: %s", string(out))
			}
			got := ojson.GetBytesNoCopy(out, "metadata.user_id").String()
			if got != tc.expected {
				t.Fatalf("metadata.user_id = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_DifferentSessionsProduceDifferentUserIDs(t *testing.T) {
	a := []byte(`{"model":"claude-test","prompt_cache_key":"gemini-session-a","contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	b := []byte(`{"model":"claude-test","prompt_cache_key":"gemini-session-b","contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	outA, _ := ConvertGeminiRequestToClaude("claude-test", a, false)
	outB, _ := ConvertGeminiRequestToClaude("claude-test", b, false)
	idA := ojson.GetBytesNoCopy(outA, "metadata.user_id").String()
	idB := ojson.GetBytesNoCopy(outB, "metadata.user_id").String()
	if idA == idB {
		t.Fatalf("different prompt_cache_key produced identical metadata.user_id: %q", idA)
	}
}

func TestConvertGeminiRequestToClaude_DefaultRoleDifferentContentProducesDifferentUserIDs(t *testing.T) {
	a := []byte(`{"contents":[{"parts":[{"text":"first prompt"}]}]}`)
	b := []byte(`{"contents":[{"parts":[{"text":"second prompt"}]}]}`)
	outA, _ := ConvertGeminiRequestToClaude("claude-test", a, false)
	outB, _ := ConvertGeminiRequestToClaude("claude-test", b, false)
	idA := ojson.GetBytesNoCopy(outA, "metadata.user_id").String()
	idB := ojson.GetBytesNoCopy(outB, "metadata.user_id").String()
	if idA == "" || idB == "" || idA == "unknown" || idB == "unknown" {
		t.Fatalf("expected valid derived user_id without role, got idA=%q idB=%q", idA, idB)
	}
	if idA == idB {
		t.Fatalf("different prompt texts without role produced identical metadata.user_id: %q", idA)
	}
	if got := ojson.GetBytesNoCopy(outA, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user; out=%s", got, string(outA))
	}
}

func TestConvertGeminiRequestToClaude_SanitizesToolNamesAndProvidesFallbackSchema(t *testing.T) {
	inputJSON := `{
		"contents": [
			{
				"role": "model",
				"parts": [
					{
						"functionCall": {
							"name": "mcp.server:get_data",
							"args": {}
						}
					}
				]
			},
			{
				"role": "user",
				"parts": [
					{
						"functionResponse": {
							"name": "mcp.server:get_data",
							"response": {"result": "ok"}
						}
					}
				]
			}
		],
		"tools": [
			{
				"functionDeclarations": [
					{
						"name": "mcp.server:get_data",
						"description": "parameterless mcp tool"
					}
				]
			}
		],
		"toolConfig": {
			"functionCallingConfig": {
				"mode": "ANY",
				"allowedFunctionNames": ["mcp.server:get_data"]
			}
		}
	}`

	result, _ := ConvertGeminiRequestToClaude("claude-test", []byte(inputJSON), false)

	// 1. Tool declaration name sanitized
	toolName := ojson.GetBytesNoCopy(result, "tools.0.name").String()
	if toolName != "mcp_server_get_data" {
		t.Fatalf("tools.0.name = %q, want mcp_server_get_data. Output: %s", toolName, result)
	}

	// 2. Fallback input_schema
	schema := ojson.GetBytesNoCopy(result, "tools.0.input_schema")
	if !schema.Exists() || schema.Get("type").String() != "object" {
		t.Fatalf("tools.0.input_schema = %s, want object schema. Output: %s", schema, result)
	}

	// 3. Historical tool_use in model/assistant turn sanitized
	toolUseName := ojson.GetBytesNoCopy(result, "messages.0.content.0.name").String()
	if toolUseName != "mcp_server_get_data" {
		t.Fatalf("messages.0.content.0.name = %q, want mcp_server_get_data. Output: %s", toolUseName, result)
	}

	// 4. Tool choice name sanitized
	toolChoiceName := ojson.GetBytesNoCopy(result, "tool_choice.name").String()
	if toolChoiceName != "mcp_server_get_data" {
		t.Fatalf("tool_choice.name = %q, want mcp_server_get_data. Output: %s", toolChoiceName, result)
	}
}

func TestConvertGeminiRequestToClaude_FunctionResponsePartsMedia(t *testing.T) {
	t.Run("inlineData image and PDF alongside response result", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "user", "parts": [{"text": "Read red.png and doc.pdf"}]},
				{"role": "model", "parts": [{"functionCall": {"name": "read_files", "args": {}}}]},
				{"role": "user", "parts": [{
					"functionResponse": {
						"name": "read_files",
						"response": {"result": "Loaded files"},
						"parts": [
							{"inlineData": {"mimeType": "image/png", "data": "iVBORw0KGgo="}},
							{"inlineData": {"mimeType": "application/pdf", "data": "JVBERi0xLjQK"}}
						]
					}
				}]}
			]
		}`)

		out, err := ConvertGeminiRequestToClaude("claude-sonnet-4-6", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToClaude() error = %v", err)
		}
		toolResult := ojson.GetBytesNoCopy(out, "messages.2.content.0")
		if got := toolResult.Get("type").String(); got != "tool_result" {
			t.Fatalf("messages.2.content.0.type = %q, want tool_result; out=%s", got, out)
		}
		content := toolResult.Get("content")
		if !content.IsArray() {
			t.Fatalf("expected tool_result.content to be array, got %s; out=%s", content.Raw, out)
		}
		blocks := content.Array()
		if len(blocks) != 3 {
			t.Fatalf("expected 3 blocks (text + image + document), got %d: %s", len(blocks), content.Raw)
		}
		if blocks[0].Get("type").String() != "text" || blocks[0].Get("text").String() != "Loaded files" {
			t.Fatalf("blocks[0] = %s, want text 'Loaded files'", blocks[0].Raw)
		}
		if blocks[1].Get("type").String() != "image" || blocks[1].Get("source.type").String() != "base64" || blocks[1].Get("source.media_type").String() != "image/png" || blocks[1].Get("source.data").String() != "iVBORw0KGgo=" {
			t.Fatalf("blocks[1] = %s, want base64 image/png block", blocks[1].Raw)
		}
		if blocks[2].Get("type").String() != "document" || blocks[2].Get("source.type").String() != "base64" || blocks[2].Get("source.media_type").String() != "application/pdf" || blocks[2].Get("source.data").String() != "JVBERi0xLjQK" {
			t.Fatalf("blocks[2] = %s, want base64 application/pdf document block", blocks[2].Raw)
		}
	})

	t.Run("fileData image URL with empty response", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [{"functionCall": {"name": "fetch_image", "args": {}}}]},
				{"role": "user", "parts": [{
					"functionResponse": {
						"name": "fetch_image",
						"response": {},
						"parts": [
							{"fileData": {"mimeType": "image/jpeg", "fileUri": "https://example.com/a.jpg"}}
						]
					}
				}]}
			]
		}`)

		out, err := ConvertGeminiRequestToClaude("claude-sonnet-4-6", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToClaude() error = %v", err)
		}
		blocks := ojson.GetBytesNoCopy(out, "messages.1.content.0.content").Array()
		if len(blocks) != 1 {
			t.Fatalf("expected 1 image block (no empty text block), got %d; out=%s", len(blocks), out)
		}
		if blocks[0].Get("type").String() != "image" || blocks[0].Get("source.type").String() != "url" || blocks[0].Get("source.url").String() != "https://example.com/a.jpg" {
			t.Fatalf("blocks[0] = %s, want url image block", blocks[0].Raw)
		}
	})

	t.Run("unsupported media-only functionResponse beside sendable text still refuses request", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [{"functionCall": {"name": "read_audio", "id": "call_1", "args": {}}}]},
				{"role": "user", "parts": [
					{"functionResponse": {
						"name": "read_audio",
						"response": {},
						"parts": [
							{"inline_data": {"mime_type": "audio/wav", "data": "UklGRg=="}}
						]
					}},
					{"text": "continue"}
				]}
			]
		}`)

		out, err := ConvertGeminiRequestToClaude("claude-sonnet-4-6", inputJSON, false)
		if err == nil {
			t.Fatalf("expected error for unsupported audio-only functionResponse, got nil; out=%s", out)
		}
		if err.Error() != "unsupported content part: inlineData" {
			t.Fatalf("err = %q, want %q", err.Error(), "unsupported content part: inlineData")
		}
	})

	t.Run("snake_case function_response with mixed text and extra response fields", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [{"function_call": {"name": "read_files", "id": "call_2", "args": {}}}]},
				{"role": "user", "parts": [{
					"function_response": {
						"name": "read_files",
						"id": "call_2",
						"response": {"result": "partial", "error": "second missing"},
						"parts": [
							{"text": "caption"},
							{"inline_data": {"mime_type": "image/png", "data": "iVBORw0KGgo="}}
						]
					}
				}]}
			]
		}`)

		out, err := ConvertGeminiRequestToClaude("claude-sonnet-4-6", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToClaude() error = %v", err)
		}
		blocks := ojson.GetBytesNoCopy(out, "messages.1.content.0.content").Array()
		if len(blocks) != 3 {
			t.Fatalf("expected 3 blocks, got %d; out=%s", len(blocks), out)
		}
		if blocks[0].Get("type").String() != "text" || !strings.Contains(blocks[0].Get("text").String(), "second missing") {
			t.Fatalf("blocks[0] = %s, want full response JSON preserving error field", blocks[0].Raw)
		}
		if blocks[1].Get("type").String() != "text" || blocks[1].Get("text").String() != "caption" {
			t.Fatalf("blocks[1] = %s, want nested text 'caption'", blocks[1].Raw)
		}
		if blocks[2].Get("type").String() != "image" || blocks[2].Get("source.media_type").String() != "image/png" {
			t.Fatalf("blocks[2] = %s, want image/png block", blocks[2].Raw)
		}
	})

	t.Run("whitespace-formatted empty response and whitespace text still refuse unsupported audio", func(t *testing.T) {
		for _, resp := range []string{`{ }`, `{"result": "   "}`} {
			inputJSON := []byte(`{
				"contents": [
					{"role": "model", "parts": [{"functionCall": {"name": "read_audio", "id": "call_1", "args": {}}}]},
					{"role": "user", "parts": [
						{"functionResponse": {
							"name": "read_audio",
							"response": ` + resp + `,
							"parts": [
								{"text": "   "},
								{"inlineData": {"mimeType": "audio/wav", "data": "UklGRg=="}}
							]
						}},
						{"text": "continue"}
					]}
				]
			}`)

			out, err := ConvertGeminiRequestToClaude("claude-sonnet-4-6", inputJSON, false)
			if err == nil || err.Error() != "unsupported content part: inlineData" {
				t.Fatalf("resp=%s: err = %v, want unsupported content part: inlineData; out=%s", resp, err, out)
			}
		}
	})

	t.Run("fallback preserves sibling response fields when no nested blocks exist", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [{"functionCall": {"name": "read_file", "id": "call_3", "args": {}}}]},
				{"role": "user", "parts": [{
					"functionResponse": {
						"name": "read_file",
						"response": {"result": "", "error": "failed"}
					}
				}]}
			]
		}`)

		out, err := ConvertGeminiRequestToClaude("claude-sonnet-4-6", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToClaude() error = %v", err)
		}
		got := ojson.GetBytesNoCopy(out, "messages.1.content.0.content").String()
		if !strings.Contains(got, `"error"`) || !strings.Contains(got, `"failed"`) {
			t.Fatalf("tool_result.content = %q, want full response JSON preserving error field", got)
		}
	})
}

func TestConvertGeminiRequestToClaude_PreservesPropertyNamedTypeAndSchemaValues(t *testing.T) {
	for _, schemaField := range []string{"parameters", "parametersJsonSchema"} {
		t.Run(schemaField, func(t *testing.T) {
			inputJSON := []byte(`{
				"contents": [{"role": "user", "parts": [{"text": "Reply with exactly hi."}]}],
				"tools": [{
					"functionDeclarations": [{
						"name": "inspect_schedule",
						"description": "Inspect a sample schedule.",
						"` + schemaField + `": {
							"type": "OBJECT",
							"properties": {
								"schedule": {
									"type": "OBJECT",
									"properties": {
										"type": {
											"type": "STRING",
											"enum": ["interval", "once"]
										}
									}
								},
								"kind_only": {
									"type": "OBJECT",
									"properties": {
										"type": {
											"enum": ["alpha", "beta"],
											"description": "Kind without nested type keyword"
										}
									}
								},
								"flag": {
									"properties": {
										"type": true
									}
								},
								"tags": {
									"type": "ARRAY",
									"items": false
								},
								"tuple_items": {
									"type": "ARRAY",
									"items": [true, {"type": "STRING"}]
								},
								"nullable_count": {
									"type": ["integer", "null"]
								},
								"metadata": {
									"type": "OBJECT",
									"default": {"type": "KeepCase"},
									"example": {"type": "AlsoKeepCase"}
								}
							}
						}
					}]
				}]
			}`)

			out, errConvert := ConvertGeminiRequestToClaude("claude-sonnet-4-6", inputJSON, true)
			if errConvert != nil {
				t.Fatalf("ConvertGeminiRequestToClaude() error = %v", errConvert)
			}

			schema := ojson.GetBytesNoCopy(out, "tools.0.input_schema")
			if !schema.IsObject() {
				t.Fatalf("tools.0.input_schema = %s, want object", schema.Raw)
			}
			if got := schema.Get("type").String(); got != "object" {
				t.Fatalf("input_schema.type = %q, want object", got)
			}
			if got := schema.Get("properties.schedule.type").String(); got != "object" {
				t.Fatalf("properties.schedule.type = %q, want object", got)
			}

			nestedTypeProp := schema.Get("properties.schedule.properties.type")
			if !nestedTypeProp.IsObject() {
				t.Fatalf("properties.schedule.properties.type = %s (type=%v), want object; out=%s", nestedTypeProp.Raw, nestedTypeProp.Type, out)
			}
			if got := nestedTypeProp.Get("type").String(); got != "string" {
				t.Fatalf("properties.schedule.properties.type.type = %q, want string", got)
			}
			if enumArr := nestedTypeProp.Get("enum").Array(); len(enumArr) != 2 || enumArr[0].String() != "interval" || enumArr[1].String() != "once" {
				t.Fatalf("properties.schedule.properties.type.enum = %s, want [\"interval\",\"once\"]", nestedTypeProp.Get("enum").Raw)
			}

			boolProp := schema.Get("properties.flag.properties.type")
			if boolProp.Type != ojson.True {
				t.Fatalf("properties.flag.properties.type = %s (type=%v), want boolean true", boolProp.Raw, boolProp.Type)
			}
			kindOnlyType := schema.Get("properties.kind_only.properties.type")
			if !kindOnlyType.IsObject() || kindOnlyType.Get("description").String() != "Kind without nested type keyword" {
				t.Fatalf("properties.kind_only.properties.type = %s, want object with description preserved", kindOnlyType.Raw)
			}
			if itemsBool := schema.Get("properties.tags.items"); itemsBool.Type != ojson.False {
				t.Fatalf("properties.tags.items = %s (type=%v), want boolean false", itemsBool.Raw, itemsBool.Type)
			}
			if tupleBool := schema.Get("properties.tuple_items.items.0"); tupleBool.Type != ojson.True {
				t.Fatalf("properties.tuple_items.items.0 = %s (type=%v), want boolean true", tupleBool.Raw, tupleBool.Type)
			}
			if tupleElemType := schema.Get("properties.tuple_items.items.1.type").String(); tupleElemType != "string" {
				t.Fatalf("properties.tuple_items.items.1.type = %q, want string", tupleElemType)
			}

			arrayType := schema.Get("properties.nullable_count.type")
			if !arrayType.IsArray() || len(arrayType.Array()) != 2 || arrayType.Array()[0].String() != "integer" || arrayType.Array()[1].String() != "null" {
				t.Fatalf("properties.nullable_count.type = %s (type=%v), want [\"integer\",\"null\"]", arrayType.Raw, arrayType.Type)
			}

			if defType := schema.Get("properties.metadata.default.type").String(); defType != "KeepCase" {
				t.Fatalf("properties.metadata.default.type = %q, want KeepCase", defType)
			}
			if exType := schema.Get("properties.metadata.example.type").String(); exType != "AlsoKeepCase" {
				t.Fatalf("properties.metadata.example.type = %q, want AlsoKeepCase", exType)
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_NormalizesNumericProtoSchemaTypes_Issue8032(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
		"tools": [{
			"functionDeclarations": [{
				"name": "search",
				"parameters": {
					"type": 6,
					"properties": {
						"query": {"type": 1},
						"tags": {"type": 5, "items": {"type": 1}},
						"limit": {"type": 3},
						"verbose": {"type": 4}
					}
				}
			}]
		}]
	}`)

	out, err := ConvertGeminiRequestToClaude("claude-sonnet-4-6", inputJSON, false)
	if err != nil {
		t.Fatalf("ConvertGeminiRequestToClaude returned error: %v", err)
	}

	schema := ojson.GetBytesNoCopy(out, "tools.0.input_schema")
	for _, tc := range []struct {
		path string
		want string
	}{
		{"type", "object"},
		{"properties.query.type", "string"},
		{"properties.tags.type", "array"},
		{"properties.tags.items.type", "string"},
		{"properties.limit.type", "integer"},
		{"properties.verbose.type", "boolean"},
	} {
		got := schema.Get(tc.path)
		if got.Type != ojson.String || got.String() != tc.want {
			t.Errorf("tools.0.input_schema.%s = %s (type %v), want string %q", tc.path, got.Raw, got.Type, tc.want)
		}
	}
}

func TestConvertGeminiRequestToClaude_NullAndNonNumericGenerationConfig_Issue8112(t *testing.T) {
	inputJSON := []byte(`{
		"generationConfig": {
			"maxOutputTokens": null,
			"topP": null,
			"topK": null,
			"temperature": 0.4,
			"thinkingConfig": {
				"thinkingBudget": null
			}
		},
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
	}`)

	out, errConvert := ConvertGeminiRequestToClaude("claude-sonnet-4-6", inputJSON, false)
	if errConvert != nil {
		t.Fatalf("ConvertGeminiRequestToClaude returned error: %v", errConvert)
	}

	if got := ojson.GetBytesNoCopy(out, "max_tokens").Int(); got != 32000 {
		t.Fatalf("max_tokens = %d, want default 32000 when maxOutputTokens is null; out=%s", got, out)
	}
	if ojson.GetBytesNoCopy(out, "top_p").Exists() {
		t.Fatalf("top_p should be absent when topP is null; out=%s", out)
	}
	if ojson.GetBytesNoCopy(out, "top_k").Exists() {
		t.Fatalf("top_k should be absent when topK is null; out=%s", out)
	}
	if ojson.GetBytesNoCopy(out, "temperature").Exists() {
		t.Fatalf("temperature should remain dropped per #4071; out=%s", out)
	}
	if ojson.GetBytesNoCopy(out, "thinking").Exists() {
		t.Fatalf("thinking should be absent when thinkingBudget is null; out=%s", out)
	}

	invalidLimits := []string{
		`{"generationConfig":{"maxOutputTokens":0,"topP":"0.8","topK":"40"},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		`{"generationConfig":{"maxOutputTokens":-5,"topK":-1},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		`{"generationConfig":{"maxOutputTokens":0,"topK":-0.5},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		`{"generationConfig":{"maxOutputTokens":"1024","topK":40.5},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
	}
	for _, raw := range invalidLimits {
		outInvalid, errInvalid := ConvertGeminiRequestToClaude("claude-sonnet-4-6", []byte(raw), false)
		if errInvalid != nil {
			t.Fatalf("ConvertGeminiRequestToClaude(%s) error = %v", raw, errInvalid)
		}
		if got := ojson.GetBytesNoCopy(outInvalid, "max_tokens").Int(); got != 32000 {
			t.Fatalf("max_tokens = %d, want default 32000 for %s; out=%s", got, raw, outInvalid)
		}
		if ojson.GetBytesNoCopy(outInvalid, "top_p").Exists() {
			t.Fatalf("top_p should be absent for %s; out=%s", raw, outInvalid)
		}
		if ojson.GetBytesNoCopy(outInvalid, "top_k").Exists() {
			t.Fatalf("top_k should be absent for %s; out=%s", raw, outInvalid)
		}
	}
}

func TestConvertGeminiRequestToClaude_ForwardsNumericTopK_Issue8112(t *testing.T) {
	raw := []byte(`{
		"generationConfig": {
			"maxOutputTokens": 4096,
			"topP": 0.9,
			"topK": 40
		},
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
	}`)

	out, errConvert := ConvertGeminiRequestToClaude("claude-sonnet-4-6", raw, false)
	if errConvert != nil {
		t.Fatalf("ConvertGeminiRequestToClaude returned error: %v", errConvert)
	}
	if got := ojson.GetBytesNoCopy(out, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want 4096; out=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "top_p").Float(); got != 0.9 {
		t.Fatalf("top_p = %v, want 0.9; out=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "top_k").Int(); got != 40 {
		t.Fatalf("top_k = %d, want 40; out=%s", got, out)
	}

	t.Run("zero maxOutputTokens falls back to positive max_output_tokens and negative fractional topK falls back to top_k", func(t *testing.T) {
		fallbackRaw := []byte(`{
			"generationConfig": {
				"maxOutputTokens": 0,
				"max_output_tokens": 2048,
				"topK": -0.5,
				"top_k": 24
			},
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
		}`)
		outFallback, errFallback := ConvertGeminiRequestToClaude("claude-sonnet-4-6", fallbackRaw, false)
		if errFallback != nil {
			t.Fatalf("ConvertGeminiRequestToClaude returned error: %v", errFallback)
		}
		if got := ojson.GetBytesNoCopy(outFallback, "max_tokens").Int(); got != 2048 {
			t.Fatalf("max_tokens = %d, want 2048; out=%s", got, outFallback)
		}
		if got := ojson.GetBytesNoCopy(outFallback, "top_k").Int(); got != 24 {
			t.Fatalf("top_k = %d, want 24; out=%s", got, outFallback)
		}
	})
}

func TestConvertGeminiRequestToClaude_SnakeCaseGeminiSDKFields_Issue8112(t *testing.T) {
	raw := []byte(`{
		"generation_config": {
			"max_output_tokens": 2048,
			"top_p": 0.85,
			"top_k": 32,
			"stop_sequences": ["END", "STOP"],
			"thinking_config": {
				"thinking_level": "high",
				"include_thoughts": true
			}
		},
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
		"tools": [{
			"function_declarations": [{
				"name": "lookup_weather",
				"description": "Look up weather",
				"parameters": null,
				"parameters_json_schema": {
					"type": "OBJECT",
					"properties": {
						"city": {"type": "STRING"}
					},
					"required": ["city"]
				}
			}]
		}]
	}`)

	out, errConvert := ConvertGeminiRequestToClaude("claude-opus-5-5", raw, false)
	if errConvert != nil {
		t.Fatalf("ConvertGeminiRequestToClaude returned error: %v", errConvert)
	}

	if got := ojson.GetBytesNoCopy(out, "max_tokens").Int(); got != 2048 {
		t.Fatalf("max_tokens = %d, want 2048; out=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "top_p").Float(); got != 0.85 {
		t.Fatalf("top_p = %v, want 0.85; out=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "top_k").Int(); got != 32 {
		t.Fatalf("top_k = %d, want 32; out=%s", got, out)
	}
	stopSeqs := ojson.GetBytesNoCopy(out, "stop_sequences").Array()
	if len(stopSeqs) != 2 || stopSeqs[0].String() != "END" || stopSeqs[1].String() != "STOP" {
		t.Fatalf("stop_sequences = %s, want [\"END\",\"STOP\"]; out=%s", ojson.GetBytesNoCopy(out, "stop_sequences").Raw, out)
	}
	if got := ojson.GetBytesNoCopy(out, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive; out=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "output_config.effort").String(); got != "high" {
		t.Fatalf("output_config.effort = %q, want high; out=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "tools.0.name").String(); got != "lookup_weather" {
		t.Fatalf("tools.0.name = %q, want lookup_weather; out=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "tools.0.input_schema.properties.city.type").String(); got != "string" {
		t.Fatalf("tools.0.input_schema.properties.city.type = %q, want string; out=%s", got, out)
	}

	t.Run("null thinkingLevel falls back to numeric thinking_budget and mixed thinking_config summary", func(t *testing.T) {
		mixed := []byte(`{
			"generationConfig": {
				"maxOutputTokens": null,
				"max_output_tokens": 8192,
				"thinking_config": {
					"thinkingLevel": null,
					"thinking_budget": 4096,
					"include_thoughts": true
				}
			},
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
		}`)
		outMixed, errMixed := ConvertGeminiRequestToClaude("claude-sonnet-4-5", mixed, false)
		if errMixed != nil {
			t.Fatalf("ConvertGeminiRequestToClaude returned error: %v", errMixed)
		}
		if got := ojson.GetBytesNoCopy(outMixed, "max_tokens").Int(); got != 8192 {
			t.Fatalf("max_tokens = %d, want 8192; out=%s", got, outMixed)
		}
		if got := ojson.GetBytesNoCopy(outMixed, "thinking.type").String(); got != "enabled" {
			t.Fatalf("thinking.type = %q, want enabled; out=%s", got, outMixed)
		}
		if got := ojson.GetBytesNoCopy(outMixed, "thinking.budget_tokens").Int(); got != 4096 {
			t.Fatalf("thinking.budget_tokens = %d, want 4096; out=%s", got, outMixed)
		}
	})
}

func TestConvertGeminiRequestToClaude_BudgetRespectsModelEffort(t *testing.T) {
	for _, tt := range []struct {
		model  string
		budget int
		want   string
	}{
		{"claude-sonnet-5-5", 32768, "xhigh"},
		{"claude-sonnet-5-5", 32769, "max"},
		{"claude-opus-4-6", 32768, "max"},
		{"claude-opus-4-6", 32769, "max"},
	} {
		t.Run(fmt.Sprintf("%s/%d", tt.model, tt.budget), func(t *testing.T) {
			input := fmt.Appendf(nil, `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":%d}}}`, tt.budget)
			out, err := ConvertGeminiRequestToClaude(tt.model, input, false)
			if err != nil {
				t.Fatal(err)
			}
			if effort := ojson.GetBytesNoCopy(out, "output_config.effort").String(); effort != tt.want || ojson.GetBytesNoCopy(out, "thinking.type").String() != "adaptive" {
				t.Fatalf("expected adaptive thinking with effort %q, got %s", tt.want, out)
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_ForceOnlyDeclaredToolBeforeSanitizing(t *testing.T) {
	for _, tc := range []struct {
		allowed, want string
	}{
		{`["mcp server lookup","mcp_server_lookup"]`, "mcp_server_lookup"},
		{`["mcp server lookup"]`, ""},
	} {
		t.Run(tc.allowed, func(t *testing.T) {
			request := []byte(`{"tools":[{"functionDeclarations":[
				{"name":"mcp.server.lookup","description":"unselected"},
				{"name":"mcp_server_lookup","description":"selected"}
			]}],
			"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":` + tc.allowed + `}}}`)
			out, err := ConvertGeminiRequestToClaude("claude-opus-4-6", request, false)
			if tc.want == "" {
				if err == nil || len(out) != 0 {
					t.Fatalf("expected the undeclared name to fail before sanitizing, got %s, %v", out, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tools := ojson.GetBytesNoCopy(out, "tools").Array()
			if len(tools) != 1 || tools[0].Get("description").String() != "selected" || ojson.GetBytesNoCopy(out, "tool_choice.name").String() != tc.want {
				t.Fatalf("expected only the declared and allowed tool, got %s", out)
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_GenerationConfigAliases(t *testing.T) {
	tests := []struct {
		name, input string
		want        map[string]string
	}{
		{
			name: "snake config and mixed aliases",
			input: `{"generationConfig":null,"generation_config":{
				"maxOutputTokens":0,"max_output_tokens":123,"topP":null,"top_p":0,
				"topK":-0.5,"top_k":32,"stopSequences":null,"stop_sequences":[null,5,"end"],
				"thinkingConfig":null,"thinking_config":{"thinkingLevel":null,"thinking_budget":2048}}}`,
			want: map[string]string{
				"max_tokens": "123", "top_p": "0", "top_k": "32",
				"stop_sequences.#": "1", "stop_sequences.0": `"end"`,
				"thinking.type": `"adaptive"`, "output_config.effort": `"medium"`,
			},
		},
		{
			name: "null fields",
			input: `{"generationConfig":{
				"maxOutputTokens":null,"topP":null,"topK":null,"stopSequences":null,
				"thinkingConfig":{"thinkingBudget":null}}}`,
			want: map[string]string{
				"max_tokens": "32000", "top_p": "", "top_k": "", "stop_sequences": "", "thinking": "",
			},
		},
		{
			name:  "explicit zero budget and omitted limit",
			input: `{"generationConfig":{"maxOutputTokens":0,"topK":0,"thinkingConfig":{"thinkingBudget":0}}}`,
			want: map[string]string{
				"max_tokens": "32000", "top_k": "0", "thinking.type": `"disabled"`, "output_config": "",
			},
		},
		{
			name:  "level wins over budget",
			input: `{"generation_config":{"thinking_config":{"thinkingLevel":null,"thinking_level":"LOW","thinking_budget":0}}}`,
			want:  map[string]string{"thinking.type": `"adaptive"`, "output_config.effort": `"low"`},
		},
		{
			name:  "budget in alternate thinking object",
			input: `{"generationConfig":{"thinkingConfig":{"includeThoughts":false},"thinking_config":{"thinking_budget":0}}}`,
			want:  map[string]string{"thinking.type": `"disabled"`, "output_config.effort": ""},
		},
		{
			name: "level in alternate generation object overrides budget",
			input: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}},
				"generation_config":{"thinkingConfig":{"thinking_level":"LOW"}}}`,
			want: map[string]string{"thinking.type": `"adaptive"`, "output_config.effort": `"low"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ConvertGeminiRequestToClaude("claude-opus-4-6", []byte(tt.input), false)
			if err != nil {
				t.Fatal(err)
			}
			got := ojson.ParseBytes(out)
			for field, want := range tt.want {
				if value := got.Get(field).Raw; value != want {
					t.Errorf("%s = %q, want %q (output: %s)", field, value, want, out)
				}
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_OmittedRolesAndToolAliases(t *testing.T) {
	for _, fields := range [][2]string{{"functionCall", "functionResponse"}, {"function_call", "function_response"}} {
		t.Run(fields[0], func(t *testing.T) {
			input := fmt.Sprintf(`{"contents":[
				{"parts":[{"text":"first"}]},
				{"parts":[{"text":"second"}]},
				{"parts":[{"text":"checking"},{%q:{"id":"call_lookup","name":"lookup","args":{"query":"status"}}}]},
				{"parts":[{%q:{"name":"lookup","response":{"result":"ok"}}}]},
				{"parts":[{"text":"followup"}]}
			]}`, fields[0], fields[1])
			out, err := ConvertGeminiRequestToClaude("claude-test", []byte(input), false)
			if err != nil {
				t.Fatal(err)
			}
			messages := ojson.GetBytesNoCopy(out, "messages").Array()
			if len(messages) != 3 {
				t.Fatalf("want three role groups, got %s", out)
			}
			if messages[0].Get("role").String() != "user" || messages[0].Get("content.0.text").String() != "first" || messages[0].Get("content.1.text").String() != "second" {
				t.Fatalf("plain text without role must stay user: %s", out)
			}
			toolUse := messages[1].Get("content.1")
			if messages[1].Get("role").String() != "assistant" || messages[1].Get("content.0.text").String() != "checking" ||
				toolUse.Get("type").String() != "tool_use" || toolUse.Get("id").String() != "call_lookup" ||
				toolUse.Get("name").String() != "lookup" || toolUse.Get("input.query").String() != "status" {
				t.Fatalf("function call turn not preserved as assistant: %s", out)
			}
			toolResult := messages[2].Get("content.0")
			if messages[2].Get("role").String() != "user" || toolResult.Get("type").String() != "tool_result" ||
				toolResult.Get("tool_use_id").String() != "call_lookup" || toolResult.Get("content").String() != "ok" ||
				messages[2].Get("content.1.text").String() != "followup" {
				t.Fatalf("function response not paired before user followup: %s", out)
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_PreservesToolSchemaValues(t *testing.T) {
	for _, field := range []string{"parameters", "parametersJsonSchema"} {
		t.Run(field, func(t *testing.T) {
			input := fmt.Sprintf(`{"tools":[{"functionDeclarations":[{"name":"inspect",%q:{
				"type":"OBJECT",
				"x-metadata":{"type":"KeepMetadataCase"},
				"properties":{
					"type":{"type":"STRING","enum":["ONE","TWO"]},
					"flags":{"properties":{"type":true}},
					"tuple":{"type":"ARRAY","items":[false,{"type":"INTEGER"}]},
					"nullable":{"type":["integer","null"]},
					"default":{"type":"OBJECT","default":{"type":"KeepCase"},"examples":[{"type":"AlsoKeepCase"}]},
					"a.b*?|#\\z":{"type":"STRING","minLength":"2"}
				}
			}}]}]}`, field)
			out, err := ConvertGeminiRequestToClaude("claude-test", []byte(input), false)
			if err != nil {
				t.Fatal(err)
			}
			schema := ojson.GetBytesNoCopy(out, "tools.0.input_schema")
			want := ojson.Parse(`{
				"type":"object",
				"additionalProperties":false,
				"$schema":"http://json-schema.org/draft-07/schema#",
				"x-metadata":{"type":"KeepMetadataCase"},
				"properties":{
					"type":{"type":"string","enum":["ONE","TWO"]},
					"flags":{"properties":{"type":true}},
					"tuple":{"type":"array","items":[false,{"type":"integer"}]},
					"nullable":{"type":["integer","null"]},
					"default":{"type":"object","default":{"type":"KeepCase"},"examples":[{"type":"AlsoKeepCase"}]},
					"a.b*?|#\\z":{"type":"string","minLength":2}
				}
			}`)
			if !reflect.DeepEqual(schema.Value(), want.Value()) {
				t.Fatalf("tools.0.input_schema = %s, want %s", schema.Raw, want.Raw)
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_RestrictsGeminiSDKToolChoice(t *testing.T) {
	request := []byte(`{
		"contents":[{"parts":[{"text":"hello"}]}],
		"tools":[
			{"functionDeclarations":[{"name":"first"}],"function_declarations":[{"name":"first"}]},
			{"function_declarations":[
				{"name":"mcp.server.lookup","parameters":null,"parameters_json_schema":{"type":"OBJECT","properties":{"query":{"type":"STRING"}}}},
				{"name":"excluded"}
			]}
		],
		"tool_config":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":null,
			"allowed_function_names":["first","mcp.server.lookup","first",null]}}
	}`)
	out, err := ConvertGeminiRequestToClaude("claude-opus-4-6", request, false)
	if err != nil {
		t.Fatal(err)
	}
	root := ojson.ParseBytes(out)
	tools := root.Get("tools").Array()
	if len(tools) != 2 || tools[0].Get("name").String() != "first" || tools[1].Get("name").String() != "mcp_server_lookup" {
		t.Fatalf("expected only the two allowed tools, got %s", out)
	}
	if root.Get("tool_choice.type").String() != "any" || tools[1].Get("input_schema.properties.query.type").String() != "string" {
		t.Fatalf("expected required tool selection and the snake_case parameter schema, got %s", out)
	}
}

func TestConvertGeminiRequestToClaude_ThinkingLevelRespectsModelCapabilities(t *testing.T) {
	const legacy = "claude-sonnet-4-5-20250929"
	const modern = "claude-opus-5-5"
	const aliasHigh = `"max_output_tokens":4096,"thinking_config":{"thinking_level":"high"}`
	for _, tc := range []struct {
		name, model, generation, mode, effort, budget string
	}{
		{name: "legacy high", model: legacy, generation: `"thinking_config":{"thinking_level":"high"}`, mode: "enabled", budget: "24576"},
		{name: "legacy Sonnet short name", model: "claude-sonnet-4-5", generation: `"thinking_config":{"thinking_level":"high"}`, mode: "enabled", budget: "24576"},
		{name: "legacy Haiku short name", model: "claude-haiku-4-5", generation: `"thinking_config":{"thinking_level":"high"}`, mode: "enabled", budget: "24576"},
		{name: "legacy Opus short name", model: "claude-opus-4-5", generation: `"thinking_config":{"thinking_budget":-1}`},
		{name: "Opus 4.1 alias", model: "claude-opus-4-1", generation: aliasHigh, mode: "enabled", budget: "4095"},
		{name: "Opus 4.1 alias auto budget", model: "claude-opus-4-1", generation: `"thinking_config":{"thinking_budget":-1}`},
		{name: "Opus 4.0 alias", model: "claude-opus-4-0", generation: aliasHigh, mode: "enabled", budget: "4095"},
		{name: "Sonnet 4.0 alias", model: "claude-sonnet-4-0", generation: aliasHigh, mode: "enabled", budget: "4095"},
		{name: "Sonnet 3.7 alias", model: "claude-3-7-sonnet-latest", generation: aliasHigh, mode: "enabled", budget: "4095"},
		{name: "legacy minimum", model: legacy, generation: `"thinking_config":{"thinking_level":"minimal"}`, mode: "enabled", budget: "1024"},
		{name: "legacy output ceiling", model: legacy, generation: `"max_output_tokens":4096,"thinking_config":{"thinking_level":"xhigh"}`, mode: "enabled", budget: "4095"},
		{name: "legacy auto", model: legacy, generation: `"thinking_config":{"thinking_level":"auto"}`},
		{name: "legacy auto budget", model: legacy, generation: `"thinking_config":{"thinking_budget":-1}`},
		{name: "legacy explicit positive budget", model: legacy, generation: `"thinking_config":{"thinking_budget":12000}`, mode: "enabled", budget: "12000"},
		{name: "legacy zero budget", model: legacy, generation: `"thinking_config":{"thinking_budget":0}`, mode: "disabled"},
		{name: "modern xhigh", model: modern, generation: `"thinking_config":{"thinking_level":"xhigh"}`, mode: "adaptive", effort: "xhigh"},
		{name: "modern auto", model: modern, generation: `"thinking_config":{"thinking_level":"auto"}`, mode: "adaptive"},
		{name: "4.6 auto fallback", model: "claude-opus-4-6", generation: `"thinking_config":{"thinking_level":"auto"}`, mode: "adaptive", effort: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(`{"generation_config":{` + tc.generation + `}}`)
			out, err := ConvertGeminiRequestToClaude(tc.model, input, false)
			if err != nil {
				t.Fatal(err)
			}
			result := ojson.ParseBytes(out)
			if result.Get("model").Str != tc.model {
				t.Fatalf("resolved capability lookup changed requested model: %s", out)
			}
			if result.Get("thinking.type").Str != tc.mode ||
				result.Get("output_config.effort").Str != tc.effort ||
				result.Get("thinking.budget_tokens").Raw != tc.budget {
				t.Fatalf("thinking=%s, effort=%s; want mode %q, effort %q, budget %q", result.Get("thinking").Raw, result.Get("output_config.effort").Raw, tc.mode, tc.effort, tc.budget)
			}
		})
	}
	t.Run("output cap leaves no room for manual thinking", func(t *testing.T) {
		input := []byte(`{"generationConfig":{"maxOutputTokens":1024,"thinkingConfig":{"thinkingLevel":"high"}}}`)
		if out, err := ConvertGeminiRequestToClaude(legacy, input, false); err == nil || len(out) != 0 {
			t.Fatalf("expected incompatible output cap to fail conversion, got %s, %v", out, err)
		}
	})
}

func TestConvertGeminiRequestToClaude_ToolResultsPrecedeUserText(t *testing.T) {
	tests := map[string]string{
		"text and result in one turn": `[
			{"role":"user","parts":[{"text":"read file"}]},
			{"role":"model","parts":[{"functionCall":{"name":"read","id":"call-a","args":{}}}]},
			{"role":"model","parts":[{"text":"continue"},{"functionResponse":{"name":"read","id":"call-a","response":{"result":"one"}}}]}
		]`,
		"text before a separate result turn": `[
			{"role":"user","parts":[{"text":"read file"}]},
			{"role":"model","parts":[{"functionCall":{"name":"read","id":"call-a","args":{}}}]},
			{"role":"user","parts":[{"text":"continue"}]},
			{"role":"model","parts":[{"functionResponse":{"name":"read","id":"call-a","response":{"result":"one"}}}]}
		]`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			out, errConvert := ConvertGeminiRequestToClaude("claude-sonnet-4", []byte(`{"contents":`+contents+`}`), true)
			if errConvert != nil {
				t.Fatalf("conversion failed: %v", errConvert)
			}
			message := ojson.GetBytesNoCopy(out, "messages.2")
			if message.Get("role").String() != "user" || message.Get("content.0.type").String() != "tool_result" || message.Get("content.0.tool_use_id").String() != "call-a" || message.Get("content.1.text").String() != "continue" {
				t.Fatalf("user content = %s; want tool result before text", message.Raw)
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_ToolResultsUseUserMessages(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		wantIDs  []string
	}{
		{
			name: "model role with parallel responses",
			contents: `[
				{"role":"user","parts":[{"text":"read files"}]},
				{"role":"model","parts":[{"functionCall":{"name":"read","id":"call-a","args":{}}},{"functionCall":{"name":"read","id":"call-b","args":{}}}]},
				{"role":"model","parts":[{"functionResponse":{"name":"read","id":"call-a","response":{"result":"one"}}},{"functionResponse":{"name":"read","id":"call-b","response":{"result":"two"}}}]}
			]`,
			wantIDs: []string{"call-a", "call-b"},
		},
		{
			name: "assistant role with snake case response",
			contents: `[
				{"role":"user","parts":[{"text":"read file"}]},
				{"role":"model","parts":[{"function_call":{"name":"read","id":"call-a","args":{}}}]},
				{"role":"assistant","parts":[{"function_response":{"name":"read","id":"call-a","response":{"result":"one"}}}]}
			]`,
			wantIDs: []string{"call-a"},
		},
		{
			name: "call and response in one model turn",
			contents: `[
				{"role":"user","parts":[{"text":"read file"}]},
				{"role":"model","parts":[{"functionCall":{"name":"read","id":"call-a","args":{}}},{"functionResponse":{"name":"read","id":"call-a","response":{"result":"one"}}}]}
			]`,
			wantIDs: []string{"call-a"},
		},
		{
			name: "call and response in one user turn",
			contents: `[
				{"role":"user","parts":[{"text":"read file"}]},
				{"role":"user","parts":[{"functionCall":{"name":"read","id":"call-a","args":{}}},{"functionResponse":{"name":"read","id":"call-a","response":{"result":"one"}}}]}
			]`,
			wantIDs: []string{"call-a"},
		},
		{
			name: "ordinary user tool result",
			contents: `[
				{"role":"user","parts":[{"text":"read file"}]},
				{"role":"model","parts":[{"functionCall":{"name":"read","id":"call-a","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"read","id":"call-a","response":{"result":"one"}}}]}
			]`,
			wantIDs: []string{"call-a"},
		},
		{
			name: "model result without explicit tool id",
			contents: `[
				{"role":"user","parts":[{"text":"read file"}]},
				{"role":"model","parts":[{"functionCall":{"name":"read","args":{}}}]},
				{"role":"model","parts":[{"functionResponse":{"name":"read","response":{"result":"one"}}}]}
			]`,
			wantIDs: []string{"toolu_gemini_0000000000000001"},
		},
		{
			name: "missing result role",
			contents: `[
				{"role":"user","parts":[{"text":"read file"}]},
				{"role":"model","parts":[{"functionCall":{"name":"read","id":"call-a","args":{}}}]},
				{"parts":[{"functionResponse":{"name":"read","id":"call-a","response":{"result":"one"}}}]}
			]`,
			wantIDs: []string{"call-a"},
		},
	}

	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(tt.name+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				out, errConvert := ConvertGeminiRequestToClaude("claude-sonnet-4", []byte(`{"contents":`+tt.contents+`}`), stream)
				if errConvert != nil {
					t.Fatalf("conversion failed: %v", errConvert)
				}
				messages := ojson.GetBytesNoCopy(out, "messages").Array()
				if len(messages) != 3 {
					t.Fatalf("messages = %s; want user, assistant tool calls, user tool results", ojson.GetBytesNoCopy(out, "messages").Raw)
				}
				if got := messages[1].Get("role").String(); got != "assistant" {
					t.Fatalf("tool call role = %q, want assistant; body=%s", got, out)
				}
				if got := messages[2].Get("role").String(); got != "user" {
					t.Fatalf("tool result role = %q, want user; body=%s", got, out)
				}
				calls := messages[1].Get("content").Array()
				results := messages[2].Get("content").Array()
				if len(calls) != len(tt.wantIDs) || len(results) != len(tt.wantIDs) {
					t.Fatalf("calls=%s results=%s, want %d pairs", messages[1].Get("content").Raw, messages[2].Get("content").Raw, len(tt.wantIDs))
				}
				for i, wantID := range tt.wantIDs {
					if calls[i].Get("type").String() != "tool_use" || calls[i].Get("id").String() != wantID {
						t.Fatalf("call[%d] = %s, want tool_use id %q", i, calls[i].Raw, wantID)
					}
					if results[i].Get("type").String() != "tool_result" || results[i].Get("tool_use_id").String() != wantID {
						t.Fatalf("result[%d] = %s, want tool_result id %q", i, results[i].Raw, wantID)
					}
				}
			})
		}
	}
}
