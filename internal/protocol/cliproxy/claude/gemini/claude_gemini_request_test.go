package gemini

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

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
			messages := gjson.GetBytes(out, "messages").Array()
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

			gotCallID := gjson.GetBytes(out, "messages.0.content.0.id").String()
			if gotCallID != tt.want {
				t.Fatalf("expected tool_use id %q, got %q; output=%s", tt.want, gotCallID, string(out))
			}

			gotResultID := gjson.GetBytes(out, "messages.1.content.0.tool_use_id").String()
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
	messages := gjson.GetBytes(out, "messages").Array()
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
	messages := gjson.GetBytes(out, "messages").Array()
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
	messages := gjson.GetBytes(out, "messages").Array()
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

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != 0.8 {
		t.Fatalf("top_p = %v, want 0.8", got)
	}
}

func TestConvertGeminiRequestToClaude_AcceptsCamelInlineData(t *testing.T) {
	out, _ := ConvertGeminiRequestToClaude("claude-sonnet-4", []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}]}`), false)
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "image" {
		t.Fatalf("content type = %q, want image. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.source.media_type").String(); got != "image/png" {
		t.Fatalf("media_type = %q, want image/png. Output: %s", got, string(out))
	}
}

func TestConvertGeminiRequestToClaude_SplitsNonImageInlineDataByMIME(t *testing.T) {
	out, _ := ConvertGeminiRequestToClaude("claude-sonnet-4", []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}},{"inlineData":{"mimeType":"video/mp4","data":"AAAAIGZ0eXA="}},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}}]}]}`), false)

	// A user attachment Claude cannot read is dropped rather than replaced by placeholder text.
	if got := gjson.GetBytes(out, "messages.0.content.#").Int(); got != 1 {
		t.Fatalf("user content has %d blocks, want only the document. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "document" {
		t.Fatalf("document content type = %q, want document. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "messages.0.content.#(type==\"image\")").Exists() {
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

		messages := gjson.GetBytes(out, "messages").Array()
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

		content := gjson.GetBytes(out, "messages.0.content").Array()
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

	gotCall1 := gjson.GetBytes(out1, "messages.0.content.0.id").String()
	gotResp1 := gjson.GetBytes(out1, "messages.1.content.0.tool_use_id").String()
	gotCall2 := gjson.GetBytes(out1, "messages.2.content.0.id").String()
	gotResp2 := gjson.GetBytes(out1, "messages.3.content.0.tool_use_id").String()

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
			if !gjson.ValidBytes(out) {
				t.Fatalf("output is invalid json: %s", string(out))
			}
			got := gjson.GetBytes(out, "metadata.user_id").String()
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
	idA := gjson.GetBytes(outA, "metadata.user_id").String()
	idB := gjson.GetBytes(outB, "metadata.user_id").String()
	if idA == idB {
		t.Fatalf("different prompt_cache_key produced identical metadata.user_id: %q", idA)
	}
}

func TestConvertGeminiRequestToClaude_DefaultRoleDifferentContentProducesDifferentUserIDs(t *testing.T) {
	a := []byte(`{"contents":[{"parts":[{"text":"first prompt"}]}]}`)
	b := []byte(`{"contents":[{"parts":[{"text":"second prompt"}]}]}`)
	outA, _ := ConvertGeminiRequestToClaude("claude-test", a, false)
	outB, _ := ConvertGeminiRequestToClaude("claude-test", b, false)
	idA := gjson.GetBytes(outA, "metadata.user_id").String()
	idB := gjson.GetBytes(outB, "metadata.user_id").String()
	if idA == "" || idB == "" || idA == "unknown" || idB == "unknown" {
		t.Fatalf("expected valid derived user_id without role, got idA=%q idB=%q", idA, idB)
	}
	if idA == idB {
		t.Fatalf("different prompt texts without role produced identical metadata.user_id: %q", idA)
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
	toolName := gjson.GetBytes(result, "tools.0.name").String()
	if toolName != "mcp_server_get_data" {
		t.Fatalf("tools.0.name = %q, want mcp_server_get_data. Output: %s", toolName, result)
	}

	// 2. Fallback input_schema
	schema := gjson.GetBytes(result, "tools.0.input_schema")
	if !schema.Exists() || schema.Get("type").String() != "object" {
		t.Fatalf("tools.0.input_schema = %s, want object schema. Output: %s", schema, result)
	}

	// 3. Historical tool_use in model/assistant turn sanitized
	toolUseName := gjson.GetBytes(result, "messages.0.content.0.name").String()
	if toolUseName != "mcp_server_get_data" {
		t.Fatalf("messages.0.content.0.name = %q, want mcp_server_get_data. Output: %s", toolUseName, result)
	}

	// 4. Tool choice name sanitized
	toolChoiceName := gjson.GetBytes(result, "tool_choice.name").String()
	if toolChoiceName != "mcp_server_get_data" {
		t.Fatalf("tool_choice.name = %q, want mcp_server_get_data. Output: %s", toolChoiceName, result)
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
				messages := gjson.GetBytes(out, "messages").Array()
				if len(messages) != 3 {
					t.Fatalf("messages = %s; want user, assistant tool calls, user tool results", gjson.GetBytes(out, "messages").Raw)
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
			message := gjson.GetBytes(out, "messages.2")
			if message.Get("role").String() != "user" || message.Get("content.0.type").String() != "tool_result" || message.Get("content.0.tool_use_id").String() != "call-a" || message.Get("content.1.text").String() != "continue" {
				t.Fatalf("user content = %s; want tool result before text", message.Raw)
			}
		})
	}
}
