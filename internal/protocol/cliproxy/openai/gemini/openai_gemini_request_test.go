package gemini

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToOpenAI_OmittedRolesAndToolAliases(t *testing.T) {
	for _, fields := range [][2]string{{"functionCall", "functionResponse"}, {"function_call", "function_response"}} {
		t.Run(fields[0], func(t *testing.T) {
			input := fmt.Sprintf(`{"contents":[
				{"parts":[{"text":"first"}]},
				{"parts":[{"text":"second"}]},
				{"parts":[{"text":"checking"},{%q:{"id":"call_lookup","name":"lookup","args":{"query":"status"}}}]},
				{"parts":[{%q:{"name":"lookup","response":{"result":"ok"}}}]},
				{"parts":[{"text":"followup"}]}
			]}`, fields[0], fields[1])
			out, err := ConvertGeminiRequestToOpenAI("test-model", []byte(input), false)
			if err != nil {
				t.Fatal(err)
			}
			messages := gjson.GetBytes(out, "messages").Array()
			if len(messages) != 5 {
				t.Fatalf("want five messages without an empty user turn after the tool result, got %s", out)
			}
			for _, index := range []int{0, 1, 4} {
				if messages[index].Get("role").String() != "user" {
					t.Fatalf("plain text without role must stay user: %s", out)
				}
			}
			toolCall := messages[2].Get("tool_calls.0")
			if messages[2].Get("role").String() != "assistant" || messages[2].Get("content").String() != "checking" ||
				toolCall.Get("id").String() != "call_lookup" || toolCall.Get("function.name").String() != "lookup" ||
				gjson.Parse(toolCall.Get("function.arguments").String()).Get("query").String() != "status" {
				t.Fatalf("function call turn not preserved as assistant: %s", out)
			}
			if messages[3].Get("role").String() != "tool" || messages[3].Get("tool_call_id").String() != "call_lookup" ||
				gjson.Parse(messages[3].Get("content").String()).Get("result").String() != "ok" {
				t.Fatalf("function response not paired: %s", out)
			}
			if messages[4].Get("content").String() != "followup" {
				t.Fatalf("followup text changed: %s", out)
			}
		})
	}
}

func TestConvertGeminiRequestToOpenAI_FunctionResponseWithTextPreservesUserContent(t *testing.T) {
	out, err := ConvertGeminiRequestToOpenAI("test-model", []byte(`{"contents":[{"parts":[
		{"function_response":{"id":"call_lookup","name":"lookup","response":{"result":"ok"}}},
		{"text":"followup"}
	]}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 || messages[0].Get("role").String() != "tool" ||
		messages[1].Get("role").String() != "user" || messages[1].Get("content").String() != "followup" {
		t.Fatalf("function response must retain accompanying user text: %s", out)
	}
}

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

	out, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
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

	out, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
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

	out, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
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

			out, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
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
	out, _ := ConvertGeminiRequestToOpenAI("gpt-test", []byte(`{"contents":[{"role":"user","parts":[{"inline_data":{"mime_type":"image/png","data":"aGVsbG8="}}]}]}`), false)
	if got := gjson.GetBytes(out, "messages.0.content.0.image_url.url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image url = %q, want data:image/png;base64,aGVsbG8=. Output: %s", got, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_SplitsNonImageInlineDataByMIME(t *testing.T) {
	out, _ := ConvertGeminiRequestToOpenAI("gpt-test", []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}},{"inlineData":{"mimeType":"video/mp4","data":"AAAAIGZ0eXA="}},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}}]}]}`), false)

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
		out, _ := ConvertGeminiRequestToOpenAI("openai-test", []byte(`{
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
		out, _ := ConvertGeminiRequestToOpenAI("openai-test", []byte(`{
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

	first, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	second, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
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

	out, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
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

	out, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
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
	first, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	second, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
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
	out, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
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
	out, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	for i, want := range []string{"call_2", "call_1", "call_3"} {
		path := "messages." + string(rune('1'+i)) + ".tool_call_id"
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestConvertGeminiRequestToOpenAI_PreservesFunctionCallThoughtSignature(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{"role": "user", "parts": [{"text": "weather in Paris and London?"}]},
			{
				"role": "model",
				"parts": [
					{
						"functionCall": {"name": "get_weather", "args": {"city": "Paris"}},
						"thoughtSignature": "SIG_PARIS"
					},
					{
						"functionCall": {"name": "get_weather", "args": {"city": "London"}}
					}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "get_weather", "response": {"t": 20}}},
					{"functionResponse": {"name": "get_weather", "response": {"t": 15}}}
				]
			}
		]
	}`)

	out, err := ConvertGeminiRequestToOpenAI("google/gemini-3-pro", inputJSON, false)
	if err != nil {
		t.Fatalf("ConvertGeminiRequestToOpenAI() error = %v", err)
	}
	if got := gjson.GetBytes(out, "messages.1.tool_calls.0.extra_content.google.thought_signature").String(); got != "SIG_PARIS" {
		t.Fatalf("messages.1.tool_calls.0.extra_content.google.thought_signature = %q, want SIG_PARIS; out=%s", got, string(out))
	}
	if gjson.GetBytes(out, "messages.1.tool_calls.1.extra_content").Exists() {
		t.Fatalf("expected unsigned sibling tool call not to have extra_content; out=%s", string(out))
	}
	for _, functionField := range []string{"functionCall", "function_call"} {
		for _, signatureField := range []string{"thoughtSignature", "thought_signature"} {
			t.Run(functionField+"/"+signatureField, func(t *testing.T) {
				input := fmt.Sprintf(`{"contents":[{"parts":[
					{%q:{"name":"get_weather","args":{"city":"Paris"},%q:"SIG_PARIS"}},
					{%q:{"name":"get_weather","args":{"city":"London"}}}
				]}]}`, functionField, signatureField, functionField)
				output, err := ConvertGeminiRequestToOpenAI("google/gemini-3-pro", []byte(input), false)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(output, "messages.0.tool_calls.0.extra_content.google.thought_signature").String(); got != "SIG_PARIS" {
					t.Fatalf("nested thought signature = %q, want SIG_PARIS; out=%s", got, output)
				}
				if gjson.GetBytes(output, "messages.0.tool_calls.1.extra_content").Exists() {
					t.Fatalf("unsigned sibling acquired a thought signature: %s", output)
				}
			})
		}
	}
}

func TestConvertGeminiRequestToOpenAI_FunctionResponsePartsMedia(t *testing.T) {
	t.Run("inlineData image and PDF alongside response relayed in user message", func(t *testing.T) {
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

		out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI() error = %v", err)
		}
		messages := gjson.GetBytes(out, "messages").Array()
		if len(messages) != 4 {
			t.Fatalf("len(messages) = %d, want 4 (user, assistant, tool, user relay); out=%s", len(messages), string(out))
		}
		if messages[2].Get("role").String() != "tool" {
			t.Fatalf("messages[2].role = %q, want tool; out=%s", messages[2].Get("role").String(), string(out))
		}
		relay := messages[3]
		if relay.Get("role").String() != "user" {
			t.Fatalf("messages[3].role = %q, want user; out=%s", relay.Get("role").String(), string(out))
		}
		parts := relay.Get("content").Array()
		if len(parts) != 3 {
			t.Fatalf("expected 3 relay parts (notice + image_url + file), got %d: %s", len(parts), relay.Get("content").Raw)
		}
		if parts[0].Get("type").String() != "text" || parts[0].Get("text").String() == "" {
			t.Fatalf("parts[0] = %s, want non-empty relay notice text", parts[0].Raw)
		}
		if parts[1].Get("type").String() != "image_url" || parts[1].Get("image_url.url").String() != "data:image/png;base64,iVBORw0KGgo=" {
			t.Fatalf("parts[1] = %s, want image_url data URL", parts[1].Raw)
		}
		if parts[2].Get("type").String() != "file" || parts[2].Get("file.file_data").String() != "data:application/pdf;base64,JVBERi0xLjQK" || parts[2].Get("file.filename").String() != "document.pdf" {
			t.Fatalf("parts[2] = %s, want file part with PDF data URL", parts[2].Raw)
		}
	})

	t.Run("media-only functionResponse sets placeholder on tool message and merges with trailing user text", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [{"functionCall": {"name": "read_image", "args": {}}}]},
				{"role": "user", "parts": [
					{"functionResponse": {
						"name": "read_image",
						"response": {},
						"parts": [
							{"fileData": {"mimeType": "image/jpeg", "fileUri": "https://example.com/a.jpg"}}
						]
					}},
					{"text": "What color is it?"}
				]}
			]
		}`)

		out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI() error = %v", err)
		}
		messages := gjson.GetBytes(out, "messages").Array()
		if len(messages) != 3 {
			t.Fatalf("len(messages) = %d, want 3 (assistant, tool, user); out=%s", len(messages), string(out))
		}
		if messages[1].Get("role").String() != "tool" || messages[1].Get("content").String() == "" || messages[1].Get("content").String() == "{}" {
			t.Fatalf("messages[1] = %s, want non-empty placeholder tool content", messages[1].Raw)
		}
		parts := messages[2].Get("content").Array()
		if len(parts) != 3 {
			t.Fatalf("expected 3 parts in merged user message, got %d: %s", len(parts), messages[2].Get("content").Raw)
		}
		if parts[1].Get("type").String() != "image_url" || parts[1].Get("image_url.url").String() != "https://example.com/a.jpg" {
			t.Fatalf("parts[1] = %s, want image_url", parts[1].Raw)
		}
		if parts[2].Get("type").String() != "text" || parts[2].Get("text").String() != "What color is it?" {
			t.Fatalf("parts[2] = %s, want user text", parts[2].Raw)
		}
	})

	t.Run("parallel tool calls across adjacent contents turns do not split tool messages with relay", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "read_a", "id": "call_a", "args": {}}},
					{"functionCall": {"name": "read_b", "id": "call_b", "args": {}}}
				]},
				{"role": "user", "parts": [{
					"functionResponse": {
						"name": "read_a",
						"id": "call_a",
						"response": {"result": "a"},
						"parts": [
							{"inlineData": {"mimeType": "image/png", "data": "QUFB"}}
						]
					}
				}]},
				{"role": "user", "parts": [{
					"functionResponse": {
						"name": "read_b",
						"id": "call_b",
						"response": {"result": "b"}
					}
				}]}
			]
		}`)

		out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI() error = %v", err)
		}
		messages := gjson.GetBytes(out, "messages").Array()
		if len(messages) != 4 {
			t.Fatalf("len(messages) = %d, want 4 (assistant, tool, tool, user relay); out=%s", len(messages), string(out))
		}
		if messages[0].Get("role").String() != "assistant" {
			t.Fatalf("messages[0].role = %q, want assistant", messages[0].Get("role").String())
		}
		if messages[1].Get("role").String() != "tool" || messages[1].Get("tool_call_id").String() != "call_a" {
			t.Fatalf("messages[1] = %s, want tool call_a", messages[1].Raw)
		}
		if messages[2].Get("role").String() != "tool" || messages[2].Get("tool_call_id").String() != "call_b" {
			t.Fatalf("messages[2] = %s, want tool call_b before user relay; out=%s", messages[2].Raw, string(out))
		}
		if messages[3].Get("role").String() != "user" || messages[3].Get("content.1.image_url.url").String() != "data:image/png;base64,QUFB" {
			t.Fatalf("messages[3] = %s, want user relay after all tool messages", messages[3].Raw)
		}
	})

	t.Run("parallel tool calls with sibling text in first response turn keep all tool messages contiguous", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "read_a", "id": "call_a", "args": {}}},
					{"functionCall": {"name": "read_b", "id": "call_b", "args": {}}}
				]},
				{"role": "user", "parts": [
					{"functionResponse": {
						"name": "read_a",
						"id": "call_a",
						"response": {"result": "a"},
						"parts": [
							{"inlineData": {"mimeType": "image/png", "data": "QUFB"}}
						]
					}},
					{"text": "check"}
				]},
				{"role": "user", "parts": [{
					"functionResponse": {
						"name": "read_b",
						"id": "call_b",
						"response": {"result": "b"}
					}
				}]}
			]
		}`)

		out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI() error = %v", err)
		}
		messages := gjson.GetBytes(out, "messages").Array()
		if len(messages) != 4 {
			t.Fatalf("len(messages) = %d, want 4 (assistant, tool, tool, user); out=%s", len(messages), string(out))
		}
		if messages[1].Get("role").String() != "tool" || messages[1].Get("tool_call_id").String() != "call_a" {
			t.Fatalf("messages[1] = %s, want tool call_a", messages[1].Raw)
		}
		if messages[2].Get("role").String() != "tool" || messages[2].Get("tool_call_id").String() != "call_b" {
			t.Fatalf("messages[2] = %s, want tool call_b before user message; out=%s", messages[2].Raw, string(out))
		}
		if messages[3].Get("role").String() != "user" {
			t.Fatalf("messages[3] = %s, want user message after all tool messages", messages[3].Raw)
		}
		userParts := messages[3].Get("content").Array()
		if len(userParts) != 3 || userParts[1].Get("type").String() != "image_url" || userParts[2].Get("text").String() != "check" {
			t.Fatalf("messages[3].content = %s, want [notice, image_url, text 'check']", messages[3].Get("content").Raw)
		}
	})

	t.Run("parallel tool responses tagged with model role keep tool messages contiguous before user relay", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "read_a", "id": "call_a", "args": {}}},
					{"functionCall": {"name": "read_b", "id": "call_b", "args": {}}}
				]},
				{"role": "model", "parts": [{
					"functionResponse": {
						"name": "read_a",
						"id": "call_a",
						"response": {"result": "a"},
						"parts": [
							{"inlineData": {"mimeType": "image/png", "data": "QUFB"}}
						]
					}
				}]},
				{"role": "model", "parts": [{
					"functionResponse": {
						"name": "read_b",
						"id": "call_b",
						"response": {"result": "b"}
					}
				}]}
			]
		}`)

		out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI() error = %v", err)
		}
		messages := gjson.GetBytes(out, "messages").Array()
		if len(messages) != 4 {
			t.Fatalf("len(messages) = %d, want 4 (assistant, tool, tool, user relay); out=%s", len(messages), string(out))
		}
		if messages[1].Get("role").String() != "tool" || messages[1].Get("tool_call_id").String() != "call_a" {
			t.Fatalf("messages[1] = %s, want tool call_a", messages[1].Raw)
		}
		if messages[2].Get("role").String() != "tool" || messages[2].Get("tool_call_id").String() != "call_b" {
			t.Fatalf("messages[2] = %s, want tool call_b before user relay; out=%s", messages[2].Raw, string(out))
		}
		if messages[3].Get("role").String() != "user" || messages[3].Get("content.1.image_url.url").String() != "data:image/png;base64,QUFB" {
			t.Fatalf("messages[3] = %s, want user relay after all tool messages", messages[3].Raw)
		}
		noticeText := messages[3].Get("content.0.text").String()
		if !strings.Contains(noticeText, "read_a") || !strings.Contains(noticeText, "call_a") {
			t.Fatalf("messages[3].content[0].text = %q, want attribution to read_a and call_a", noticeText)
		}
	})

	t.Run("parallel tool calls with identical text and only one media attachment still preserve tool attribution", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "inspect_a", "id": "call_1", "args": {}}},
					{"functionCall": {"name": "inspect_b", "id": "call_2", "args": {}}}
				]},
				{"role": "user", "parts": [
					{"functionResponse": {
						"name": "inspect_a",
						"id": "call_1",
						"response": {"result": "ok"},
						"parts": [
							{"inlineData": {"mimeType": "image/png", "data": "QUFB"}}
						]
					}},
					{"functionResponse": {
						"name": "inspect_b",
						"id": "call_2",
						"response": {"result": "ok"}
					}}
				]}
			]
		}`)

		out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI() error = %v", err)
		}
		messages := gjson.GetBytes(out, "messages").Array()
		if len(messages) != 4 {
			t.Fatalf("len(messages) = %d, want 4; out=%s", len(messages), string(out))
		}
		noticeText := messages[3].Get("content.0.text").String()
		if !strings.Contains(noticeText, "inspect_a") || !strings.Contains(noticeText, "call_1") {
			t.Fatalf("relay notice = %q, want attribution to inspect_a and call_1", noticeText)
		}
		if strings.Contains(noticeText, "inspect_b") || strings.Contains(noticeText, "call_2") {
			t.Fatalf("relay notice = %q, must not misattribute inspect_b/call_2", noticeText)
		}
	})

	t.Run("parallel tool media relay preserves per-tool attribution labels when multiple tools return media", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "read_a", "id": "call_a", "args": {}}},
					{"functionCall": {"name": "read_b", "id": "call_b", "args": {}}}
				]},
				{"role": "user", "parts": [
					{"functionResponse": {
						"name": "read_a",
						"id": "call_a",
						"response": {},
						"parts": [
							{"inlineData": {"mimeType": "image/png", "data": "QUFB"}},
							{"inlineData": {"mimeType": "image/png", "data": "QkJC"}}
						]
					}},
					{"functionResponse": {
						"name": "read_b",
						"id": "call_b",
						"response": {},
						"parts": [
							{"inlineData": {"mimeType": "image/png", "data": "Q0ND"}}
						]
					}}
				]}
			]
		}`)

		out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI() error = %v", err)
		}
		messages := gjson.GetBytes(out, "messages").Array()
		if len(messages) != 4 {
			t.Fatalf("len(messages) = %d, want 4; out=%s", len(messages), string(out))
		}
		relayParts := messages[3].Get("content").Array()
		// Expected: [notice, "Tool read_a (call_a):", img1, img2, "Tool read_b (call_b):", img3]
		if len(relayParts) != 6 {
			t.Fatalf("len(relayParts) = %d, want 6 with per-tool attribution labels; content=%s", len(relayParts), messages[3].Get("content").Raw)
		}
		if relayParts[1].Get("type").String() != "text" || !strings.Contains(relayParts[1].Get("text").String(), "call_a") || !strings.Contains(relayParts[1].Get("text").String(), "read_a") {
			t.Fatalf("relayParts[1] = %s, want label identifying read_a / call_a", relayParts[1].Raw)
		}
		if relayParts[2].Get("image_url.url").String() != "data:image/png;base64,QUFB" || relayParts[3].Get("image_url.url").String() != "data:image/png;base64,QkJC" {
			t.Fatalf("relayParts[2..3] = %s, %s, want QUFB and QkJC under call_a", relayParts[2].Raw, relayParts[3].Raw)
		}
		if relayParts[4].Get("type").String() != "text" || !strings.Contains(relayParts[4].Get("text").String(), "call_b") || !strings.Contains(relayParts[4].Get("text").String(), "read_b") {
			t.Fatalf("relayParts[4] = %s, want label identifying read_b / call_b", relayParts[4].Raw)
		}
		if relayParts[5].Get("image_url.url").String() != "data:image/png;base64,Q0ND" {
			t.Fatalf("relayParts[5] = %s, want Q0ND under call_b", relayParts[5].Raw)
		}
	})
}
