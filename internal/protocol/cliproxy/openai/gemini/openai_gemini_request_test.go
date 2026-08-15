package gemini

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToOpenAI_FunctionResponsesConsumeToolCallIDsFIFO(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "read_file", "args": {"path": "a.txt"}}},
					{"functionCall": {"name": "grep", "args": {"pattern": "needle"}}},
					{"functionCall": {"name": "list_dir", "args": {"path": "."}}}
				]
			},
			{
				"role": "function",
				"parts": [
					{"functionResponse": {"name": "read_file", "response": {"result": "a"}}},
					{"functionResponse": {"name": "grep", "response": {"result": "b"}}},
					{"functionResponse": {"name": "list_dir", "response": {"result": "c"}}}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	firstID := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	secondID := gjson.GetBytes(out, "messages.0.tool_calls.1.id").String()
	thirdID := gjson.GetBytes(out, "messages.0.tool_calls.2.id").String()

	if firstID == "" || secondID == "" || thirdID == "" {
		t.Fatalf("expected all assistant tool call IDs to be set. Output: %s", string(out))
	}
	if firstID == secondID || secondID == thirdID || firstID == thirdID {
		t.Fatalf("expected distinct assistant tool call IDs, got %q, %q, %q", firstID, secondID, thirdID)
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != firstID {
		t.Fatalf("messages.1.tool_call_id = %q, want %q. Output: %s", got, firstID, string(out))
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != secondID {
		t.Fatalf("messages.2.tool_call_id = %q, want %q. Output: %s", got, secondID, string(out))
	}
	if got := gjson.GetBytes(out, "messages.3.tool_call_id").String(); got != thirdID {
		t.Fatalf("messages.3.tool_call_id = %q, want %q. Output: %s", got, thirdID, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_FunctionResponseWithoutPriorCallGetsFallbackID(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{
				"role": "function",
				"parts": [
					{"functionResponse": {"name": "read_file", "response": {"result": "ok"}}}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	toolCallID := gjson.GetBytes(out, "messages.0.tool_call_id").String()
	if !strings.HasPrefix(toolCallID, "call_") {
		t.Fatalf("fallback tool_call_id = %q, want call_ prefix. Output: %s", toolCallID, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_ExtraFunctionResponsesUseFallbackID(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "read_file", "args": {"path": "a.txt"}}}
				]
			},
			{
				"role": "function",
				"parts": [
					{"functionResponse": {"name": "read_file", "response": {"result": "a"}}},
					{"functionResponse": {"name": "read_file", "response": {"result": "extra"}}}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	callID := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	firstResponseID := gjson.GetBytes(out, "messages.1.tool_call_id").String()
	extraResponseID := gjson.GetBytes(out, "messages.2.tool_call_id").String()

	if firstResponseID != callID {
		t.Fatalf("messages.1.tool_call_id = %q, want %q. Output: %s", firstResponseID, callID, string(out))
	}
	if !strings.HasPrefix(extraResponseID, "call_") {
		t.Fatalf("extra response fallback tool_call_id = %q, want call_ prefix. Output: %s", extraResponseID, string(out))
	}
	if extraResponseID == callID {
		t.Fatalf("extra response reused consumed tool_call_id %q. Output: %s", extraResponseID, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_PreservesExplicitFunctionCallIDs(t *testing.T) {
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
		{
			name:          "callId",
			callField:     `"callId":"call_gateway_camel_id"`,
			responseField: `"callId":"call_gateway_camel_id"`,
			want:          "call_gateway_camel_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := []byte(`{
				"contents": [
					{"role": "model", "parts": [{"functionCall": {"name": "lookup", ` + tt.callField + `, "args": {"q": "x"}}}]},
					{"role": "function", "parts": [{"functionResponse": {"name": "lookup", ` + tt.responseField + `, "response": {"result": "ok"}}}]}
				]
			}`)

			out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
			if got := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String(); got != tt.want {
				t.Fatalf("tool call id = %q, want %q. Output: %s", got, tt.want, string(out))
			}
			if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != tt.want {
				t.Fatalf("tool response id = %q, want %q. Output: %s", got, tt.want, string(out))
			}
		})
	}
}

func TestConvertGeminiRequestToOpenAI_AcceptsSnakeInlineData(t *testing.T) {
	out := ConvertGeminiRequestToOpenAI("gpt-test", []byte(`{"contents":[{"role":"user","parts":[{"inline_data":{"mime_type":"image/png","data":"aGVsbG8="}}]}]}`), false)
	if got := gjson.GetBytes(out, "messages.0.content.0.image_url.url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image url = %q, want data:image/png;base64,aGVsbG8=. Output: %s", got, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_SplitsNonImageInlineDataByMIME(t *testing.T) {
	out := ConvertGeminiRequestToOpenAI("gpt-test", []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}},{"inlineData":{"mimeType":"video/mp4","data":"AAAAIGZ0eXA="}},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}}]}]}`), false)

	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "input_audio" {
		t.Fatalf("audio content type = %q, want input_audio. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.type").String(); got != "video_url" {
		t.Fatalf("video content type = %q, want video_url. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.2.type").String(); got != "file" {
		t.Fatalf("document content type = %q, want file. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "messages.0.content.#(type==\"image_url\")").Exists() {
		t.Fatalf("non-image inlineData must not be converted to image_url. Output: %s", string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_DropsHiddenThoughtParts(t *testing.T) {
	t.Run("thought-only turn", func(t *testing.T) {
		out := ConvertGeminiRequestToOpenAI("openai-test", []byte(`{
			"contents":[
				{"role":"model","parts":[{"thought":true,"text":"internal reasoning","thoughtSignature":"opaque-provider-state"}]},
				{"role":"user","parts":[{"text":"continue"}]}
			]
		}`), false)
		messages := gjson.GetBytes(out, "messages").Array()
		if len(messages) != 1 || messages[0].Get("role").String() != "user" || messages[0].Get("content").String() != "continue" {
			t.Fatalf("hidden thought turn was not dropped. Output: %s", string(out))
		}
	})

	t.Run("mixed turn", func(t *testing.T) {
		out := ConvertGeminiRequestToOpenAI("openai-test", []byte(`{
			"contents":[{"role":"model","parts":[
				{"thought":true,"text":"internal reasoning"},
				{"text":"visible answer"}
			]}]
		}`), false)
		messages := gjson.GetBytes(out, "messages").Array()
		if len(messages) != 1 || messages[0].Get("role").String() != "assistant" || messages[0].Get("content").String() != "visible answer" {
			t.Fatalf("hidden thought was not dropped independently. Output: %s", string(out))
		}
	})
}

func TestConvertGeminiRequestToOpenAI_DeterministicToolCallIDs(t *testing.T) {
	inputJSON := []byte(`{
		"contents":[
			{"role":"model","parts":[
				{"functionCall":{"name":"read_file","args":{"path":"main.go"}}},
				{"functionCall":{"name":"grep","args":{"pattern":"TODO"}}}
			]},
			{"role":"function","parts":[
				{"functionResponse":{"name":"read_file","response":{"result":"code"}}},
				{"functionResponse":{"name":"grep","response":{"result":"matches"}}}
			]}
		]
	}`)

	first := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	second := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	for i := 0; i < 2; i++ {
		callPath := "messages.0.tool_calls." + string(rune('0'+i)) + ".id"
		responsePath := "messages." + string(rune('1'+i)) + ".tool_call_id"
		callID := gjson.GetBytes(first, callPath).String()
		if !strings.HasPrefix(callID, "call_") {
			t.Fatalf("%s = %q, want call_ prefix", callPath, callID)
		}
		if got := gjson.GetBytes(first, responsePath).String(); got != callID {
			t.Fatalf("%s = %q, want %q", responsePath, got, callID)
		}
		if got := gjson.GetBytes(second, callPath).String(); got != callID {
			t.Fatalf("second conversion %s = %q, want %q", callPath, got, callID)
		}
	}
}

func TestConvertGeminiRequestToOpenAI_SameNameCallsInSameMessageDistinct(t *testing.T) {
	inputJSON := []byte(`{
		"contents":[
			{"role":"model","parts":[
				{"functionCall":{"name":"read_file","args":{"path":"a.txt"}}},
				{"functionCall":{"name":"read_file","args":{"path":"a.txt"}}}
			]},
			{"role":"function","parts":[
				{"functionResponse":{"name":"read_file","response":{"result":"first"}}},
				{"functionResponse":{"name":"read_file","response":{"result":"second"}}}
			]}
		]
	}`)

	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	id0 := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	id1 := gjson.GetBytes(out, "messages.0.tool_calls.1.id").String()
	if id0 == id1 {
		t.Fatalf("same-name calls reused ID %q", id0)
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != id0 {
		t.Fatalf("first response ID = %q, want %q", got, id0)
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != id1 {
		t.Fatalf("second response ID = %q, want %q", got, id1)
	}
}

func TestConvertGeminiRequestToOpenAI_InterleavedPerNameFIFOMatching(t *testing.T) {
	inputJSON := []byte(`{
		"contents":[
			{"role":"model","parts":[
				{"functionCall":{"name":"tool_a","args":{"step":1}}},
				{"functionCall":{"name":"tool_b","args":{"step":1}}},
				{"functionCall":{"name":"tool_a","args":{"step":2}}},
				{"functionCall":{"name":"tool_b","args":{"step":2}}}
			]},
			{"role":"function","parts":[
				{"functionResponse":{"name":"tool_b","response":{"step":1}}},
				{"functionResponse":{"name":"tool_a","response":{"step":1}}},
				{"functionResponse":{"name":"tool_b","response":{"step":2}}},
				{"functionResponse":{"name":"tool_a","response":{"step":2}}}
			]}
		]
	}`)

	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	wants := []string{
		gjson.GetBytes(out, "messages.0.tool_calls.1.id").String(),
		gjson.GetBytes(out, "messages.0.tool_calls.0.id").String(),
		gjson.GetBytes(out, "messages.0.tool_calls.3.id").String(),
		gjson.GetBytes(out, "messages.0.tool_calls.2.id").String(),
	}
	for i, want := range wants {
		path := "messages." + string(rune('1'+i)) + ".tool_call_id"
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestConvertGeminiRequestToOpenAI_DeterministicFallbackOrphanResponse(t *testing.T) {
	inputJSON := []byte(`{"contents":[{"role":"function","parts":[{"functionResponse":{"name":"orphan_tool","response":{"result":"standalone"}}}]}]}`)
	first := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	second := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	firstID := gjson.GetBytes(first, "messages.0.tool_call_id").String()
	if !strings.HasPrefix(firstID, "call_") {
		t.Fatalf("fallback ID = %q, want call_ prefix", firstID)
	}
	if got := gjson.GetBytes(second, "messages.0.tool_call_id").String(); got != firstID {
		t.Fatalf("fallback ID changed: got %q, want %q", got, firstID)
	}
}

func TestConvertGeminiRequestToOpenAI_ExplicitCallInheritedByImplicitResponse(t *testing.T) {
	inputJSON := []byte(`{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"lookup","id":"explicit_call_1","args":{"q":"foo"}}}]},
		{"role":"function","parts":[{"functionResponse":{"name":"lookup","response":{"result":"bar"}}}]}
	]}`)
	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "explicit_call_1" {
		t.Fatalf("response ID = %q, want explicit_call_1", got)
	}
}

func TestConvertGeminiRequestToOpenAI_OutOrderExplicitResponseDoesNotDuplicateID(t *testing.T) {
	inputJSON := []byte(`{"contents":[
		{"role":"model","parts":[
			{"functionCall":{"name":"foo","id":"call_1","args":{"n":1}}},
			{"functionCall":{"name":"foo","id":"call_2","args":{"n":2}}},
			{"functionCall":{"name":"foo","id":"call_3","args":{"n":3}}}
		]},
		{"role":"function","parts":[
			{"functionResponse":{"name":"foo","id":"call_2","response":{"r":2}}},
			{"functionResponse":{"name":"foo","response":{"r":1}}},
			{"functionResponse":{"name":"foo","response":{"r":3}}}
		]}
	]}`)
	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	for i, want := range []string{"call_2", "call_1", "call_3"} {
		path := "messages." + string(rune('1'+i)) + ".tool_call_id"
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}
