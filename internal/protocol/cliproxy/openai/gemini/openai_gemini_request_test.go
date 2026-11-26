package gemini

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestConvertGeminiRequestToOpenAI_GenerationConfigAliases(t *testing.T) {
	tests := []struct {
		name, input string
		want        map[string]string
	}{
		{
			name: "snake config and mixed aliases",
			input: `{"generationConfig":null,"generation_config":{
				"temperature":0,"maxOutputTokens":null,"max_output_tokens":123,"topP":null,"top_p":0,"top_k":10,
				"candidateCount":null,"candidate_count":2,"stop_sequences":[null,3,"end"],
				"response_modalities":[null,"TEXT","AUDIO"],
				"thinkingConfig":null,"thinking_config":{"thinkingLevel":null,"thinking_level":"LOW","thinking_budget":0}}}`,
			want: map[string]string{
				"temperature": "0", "max_tokens": "123", "top_p": "0", "top_k": "10", "n": "2",
				"stop.#": "1", "stop.0": `"end"`, "modalities.#": "2", "modalities.0": `"text"`, "modalities.1": `"audio"`,
				"reasoning_effort": `"low"`,
			},
		},
		{
			name: "null fields",
			input: `{"generationConfig":{
				"temperature":null,"maxOutputTokens":null,"topP":null,"topK":null,
				"candidateCount":null,"stopSequences":null,"responseModalities":null,
				"thinkingConfig":{"thinkingBudget":null}}}`,
			want: map[string]string{
				"temperature": "", "max_tokens": "", "top_p": "", "top_k": "", "n": "", "stop": "", "modalities": "", "reasoning_effort": "",
			},
		},
		{
			name:  "explicit zero budget and omitted limits",
			input: `{"generationConfig":{"maxOutputTokens":0,"candidateCount":0,"topK":0,"thinkingConfig":{"thinkingLevel":null,"thinkingBudget":0}}}`,
			want:  map[string]string{"max_tokens": "", "n": "", "top_k": "0", "reasoning_effort": `"none"`},
		},
		{
			name:  "budget in alternate thinking object",
			input: `{"generationConfig":{"thinkingConfig":{"includeThoughts":false},"thinking_config":{"thinking_budget":1024}}}`,
			want:  map[string]string{"reasoning_effort": `"low"`},
		},
		{
			name: "level in alternate generation object overrides budget",
			input: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}},
				"generation_config":{"thinkingConfig":{"thinking_level":" HIGH "}}}`,
			want: map[string]string{"reasoning_effort": `"high"`},
		},
		{
			name: "invalid integer budgets fall through",
			input: `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0.5,"thinking_budget":-2}},
				"generation_config":{"thinking_config":{"thinking_budget":1024}}}`,
			want: map[string]string{"reasoning_effort": `"low"`},
		},
		{
			name:  "automatic level",
			input: `{"generationConfig":{"thinking_config":{"thinking_level":" AUTO ","thinking_budget":0}}}`,
			want:  map[string]string{"reasoning_effort": ""},
		},
		{
			name:  "automatic budget",
			input: `{"generation_config":{"thinking_config":{"thinking_budget":-1}}}`,
			want:  map[string]string{"reasoning_effort": ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ConvertGeminiRequestToOpenAI("gpt-6.2-sol", []byte(tt.input), false)
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

func TestConvertGeminiRequestToOpenAI_SDKToolChoice(t *testing.T) {
	for _, tc := range []struct {
		name, allowed, mode, forcedName string
		wantNames                       []string
		wantError                       bool
	}{
		{"single name", `["lookup"]`, "function", "lookup", []string{"lookup", "count", "excluded"}, false},
		{"multiple names", `["lookup",null,"count","lookup"]`, "required", "", []string{"lookup", "count"}, false},
		{"partly undeclared", `["unknown","lookup"]`, "function", "lookup", []string{"lookup", "count", "excluded"}, false},
		{"empty allowlist", `[]`, "required", "", []string{"lookup", "count", "excluded"}, false},
		{"no allowed declarations", `["unknown","missing"]`, "", "", nil, true},
		{"no valid allowlist entries", `[null,4,""]`, "", "", nil, true},
		{"invalid allowlist type", `"lookup"`, "", "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := []byte(`{
				"contents":[{"parts":[{"text":"hello"}]}],
				"tools":[{
					"function_declarations":[
						{"name":"lookup","parameters":null,"parameters_json_schema":{"type":"OBJECT","properties":{"query":{"type":"STRING"}}}},
						{"name":"count"},
						{"name":"excluded"}
					]
				}],
				"toolConfig":{"function_calling_config":{"mode":"ANY","allowed_function_names":` + tc.allowed + `}}
			}`)
			out, err := ConvertGeminiRequestToOpenAI("gpt-6.2-sol", request, false)
			if tc.wantError {
				if err == nil || len(out) != 0 {
					t.Fatalf("expected an invalid tool allowlist to fail, got %s, %v", out, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			root := ojson.ParseBytes(out)
			tools := root.Get("tools").Array()
			choice := root.Get("tool_choice")
			switch {
			case tc.forcedName != "":
				if choice.Get("type").String() != tc.mode || choice.Get("function.name").String() != tc.forcedName {
					t.Fatalf("expected to force %q, got %s", tc.forcedName, out)
				}
			case tc.mode != "":
				if choice.Type != ojson.String || choice.Str != tc.mode {
					t.Fatalf("expected %q tool choice, got %s", tc.mode, out)
				}
			default:
				if choice.Exists() {
					t.Fatalf("expected tool choice to be omitted, got %s", out)
				}
			}
			if len(tools) != len(tc.wantNames) {
				t.Fatalf("expected tools %v, got %s", tc.wantNames, out)
			}
			for index, name := range tc.wantNames {
				if tools[index].Get("function.name").String() != name {
					t.Fatalf("expected tool %q at index %d, got %s", name, index, out)
				}
			}
			if len(tools) > 0 && tools[0].Get("function.parameters.properties.query.type").String() != "string" {
				t.Fatalf("snake_case parameter schema was lost: %s", out)
			}
		})
	}
}

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
			messages := ojson.GetBytes(out, "messages").Array()
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
				ojson.Parse(toolCall.Get("function.arguments").String()).Get("query").String() != "status" {
				t.Fatalf("function call turn not preserved as assistant: %s", out)
			}
			if messages[3].Get("role").String() != "tool" || messages[3].Get("tool_call_id").String() != "call_lookup" ||
				ojson.Parse(messages[3].Get("content").String()).Get("result").String() != "ok" {
				t.Fatalf("function response not paired: %s", out)
			}
			if messages[4].Get("content").String() != "followup" {
				t.Fatalf("followup text changed: %s", out)
			}
		})
	}
}

func TestConvertGeminiRequestToOpenAI_HistoryToolArguments(t *testing.T) {
	for _, tt := range []struct {
		name      string
		argsField string
		wantArgs  string
		wantID    string
	}{
		{"null", `,"args":null`, "{}", "call_17544649d6896e2d1d8e2c99"},
		{"array", `,"args":[1,2]`, "{}", "call_df6b19dc70741480c09cc976"},
		{"string", `,"args":"{\"q\":\"s\"}"`, "{}", "call_b84bfd5df8e7274caa86a89a"},
		{"missing", "", "{}", "call_d50f0673f9082ac8f7fa7c2a"},
		{"object", `,"args":{"q":"s"}`, `{"q":"s"}`, "call_a72c08faca5ff3b3ae5bdb71"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := fmt.Sprintf(`{"contents":[
				{"role":"model","parts":[{"functionCall":{"name":"lookup"%s}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"ok"}}}]}
			]}`, tt.argsField)
			out, err := ConvertGeminiRequestToOpenAI("test-model", []byte(input), false)
			if err != nil {
				t.Fatal(err)
			}
			callID := ojson.GetBytes(out, "messages.0.tool_calls.0.id").String()
			resultID := ojson.GetBytes(out, "messages.1.tool_call_id").String()
			if callID != tt.wantID || resultID != tt.wantID {
				t.Fatalf("call id = %q, result id = %q, want %q for both", callID, resultID, tt.wantID)
			}
			if got := ojson.GetBytes(out, "messages.0.tool_calls.0.function.arguments"); got.Type != ojson.String || got.String() != tt.wantArgs {
				t.Fatalf("arguments = %s, want JSON string %q", got.Raw, tt.wantArgs)
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
	messages := ojson.GetBytes(out, "messages").Array()
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
	firstID := ojson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	secondID := ojson.GetBytes(out, "messages.0.tool_calls.1.id").String()
	thirdID := ojson.GetBytes(out, "messages.0.tool_calls.2.id").String()

	if firstID == "" || secondID == "" || thirdID == "" {
		t.Fatalf("expected all assistant tool call IDs to be set. Output: %s", string(out))
	}
	if firstID == secondID || secondID == thirdID || firstID == thirdID {
		t.Fatalf("expected distinct assistant tool call IDs, got %q, %q, %q", firstID, secondID, thirdID)
	}
	if got := ojson.GetBytes(out, "messages.1.tool_call_id").String(); got != firstID {
		t.Fatalf("messages.1.tool_call_id = %q, want %q. Output: %s", got, firstID, string(out))
	}
	if got := ojson.GetBytes(out, "messages.2.tool_call_id").String(); got != secondID {
		t.Fatalf("messages.2.tool_call_id = %q, want %q. Output: %s", got, secondID, string(out))
	}
	if got := ojson.GetBytes(out, "messages.3.tool_call_id").String(); got != thirdID {
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
	toolCallID := ojson.GetBytes(out, "messages.0.tool_call_id").String()
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
	callID := ojson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	firstResponseID := ojson.GetBytes(out, "messages.1.tool_call_id").String()
	extraResponseID := ojson.GetBytes(out, "messages.2.tool_call_id").String()

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
			if got := ojson.GetBytes(out, "messages.0.tool_calls.0.id").String(); got != tt.want {
				t.Fatalf("tool call id = %q, want %q. Output: %s", got, tt.want, string(out))
			}
			if got := ojson.GetBytes(out, "messages.1.tool_call_id").String(); got != tt.want {
				t.Fatalf("tool response id = %q, want %q. Output: %s", got, tt.want, string(out))
			}
		})
	}
}

func TestConvertGeminiRequestToOpenAI_AcceptsSnakeInlineData(t *testing.T) {
	out, _ := ConvertGeminiRequestToOpenAI("gpt-test", []byte(`{"contents":[{"role":"user","parts":[{"inline_data":{"mime_type":"image/png","data":"aGVsbG8="}}]}]}`), false)
	if got := ojson.GetBytes(out, "messages.0.content.0.image_url.url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image url = %q, want data:image/png;base64,aGVsbG8=. Output: %s", got, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_SplitsNonImageInlineDataByMIME(t *testing.T) {
	out, _ := ConvertGeminiRequestToOpenAI("gpt-test", []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}},{"inlineData":{"mimeType":"video/mp4","data":"AAAAIGZ0eXA="}},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}}]}]}`), false)

	if got := ojson.GetBytes(out, "messages.0.content.0.type").String(); got != "input_audio" {
		t.Fatalf("audio content type = %q, want input_audio. Output: %s", got, string(out))
	}
	if got := ojson.GetBytes(out, "messages.0.content.1.type").String(); got != "video_url" {
		t.Fatalf("video content type = %q, want video_url. Output: %s", got, string(out))
	}
	if got := ojson.GetBytes(out, "messages.0.content.2.type").String(); got != "file" {
		t.Fatalf("document content type = %q, want file. Output: %s", got, string(out))
	}
	if ojson.GetBytes(out, "messages.0.content.#(type==\"image_url\")").Exists() {
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
		messages := ojson.GetBytes(out, "messages").Array()
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
		messages := ojson.GetBytes(out, "messages").Array()
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
		callID := ojson.GetBytes(first, callPath).String()
		if !strings.HasPrefix(callID, "call_") {
			t.Fatalf("%s = %q, want call_ prefix", callPath, callID)
		}
		if got := ojson.GetBytes(first, responsePath).String(); got != callID {
			t.Fatalf("%s = %q, want %q", responsePath, got, callID)
		}
		if got := ojson.GetBytes(second, callPath).String(); got != callID {
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
	id0 := ojson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	id1 := ojson.GetBytes(out, "messages.0.tool_calls.1.id").String()
	if id0 == id1 {
		t.Fatalf("same-name calls reused ID %q", id0)
	}
	if got := ojson.GetBytes(out, "messages.1.tool_call_id").String(); got != id0 {
		t.Fatalf("first response ID = %q, want %q", got, id0)
	}
	if got := ojson.GetBytes(out, "messages.2.tool_call_id").String(); got != id1 {
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
		ojson.GetBytes(out, "messages.0.tool_calls.1.id").String(),
		ojson.GetBytes(out, "messages.0.tool_calls.0.id").String(),
		ojson.GetBytes(out, "messages.0.tool_calls.3.id").String(),
		ojson.GetBytes(out, "messages.0.tool_calls.2.id").String(),
	}
	for i, want := range wants {
		path := "messages." + string(rune('1'+i)) + ".tool_call_id"
		if got := ojson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestConvertGeminiRequestToOpenAI_DeterministicFallbackOrphanResponse(t *testing.T) {
	inputJSON := []byte(`{"contents":[{"role":"function","parts":[{"functionResponse":{"name":"orphan_tool","response":{"result":"standalone"}}}]}]}`)
	first, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	second, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	firstID := ojson.GetBytes(first, "messages.0.tool_call_id").String()
	if !strings.HasPrefix(firstID, "call_") {
		t.Fatalf("fallback ID = %q, want call_ prefix", firstID)
	}
	if got := ojson.GetBytes(second, "messages.0.tool_call_id").String(); got != firstID {
		t.Fatalf("fallback ID changed: got %q, want %q", got, firstID)
	}
}

func TestConvertGeminiRequestToOpenAI_ExplicitCallInheritedByImplicitResponse(t *testing.T) {
	inputJSON := []byte(`{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"lookup","id":"explicit_call_1","args":{"q":"foo"}}}]},
		{"role":"function","parts":[{"functionResponse":{"name":"lookup","response":{"result":"bar"}}}]}
	]}`)
	out, _ := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	if got := ojson.GetBytes(out, "messages.1.tool_call_id").String(); got != "explicit_call_1" {
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
		if got := ojson.GetBytes(out, path).String(); got != want {
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
	if got := ojson.GetBytes(out, "messages.1.tool_calls.0.extra_content.google.thought_signature").String(); got != "SIG_PARIS" {
		t.Fatalf("messages.1.tool_calls.0.extra_content.google.thought_signature = %q, want SIG_PARIS; out=%s", got, string(out))
	}
	if ojson.GetBytes(out, "messages.1.tool_calls.1.extra_content").Exists() {
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
				if got := ojson.GetBytes(output, "messages.0.tool_calls.0.extra_content.google.thought_signature").String(); got != "SIG_PARIS" {
					t.Fatalf("nested thought signature = %q, want SIG_PARIS; out=%s", got, output)
				}
				if ojson.GetBytes(output, "messages.0.tool_calls.1.extra_content").Exists() {
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
		messages := ojson.GetBytes(out, "messages").Array()
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
		messages := ojson.GetBytes(out, "messages").Array()
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
		messages := ojson.GetBytes(out, "messages").Array()
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
		messages := ojson.GetBytes(out, "messages").Array()
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
		messages := ojson.GetBytes(out, "messages").Array()
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
		messages := ojson.GetBytes(out, "messages").Array()
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
		messages := ojson.GetBytes(out, "messages").Array()
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

func TestConvertGeminiRequestToOpenAI_FunctionResponseDoesNotAppendTrailingEmptyFunctionMessage(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{"parts": [{"text": "What is the weather?"}]},
			{"parts": [{"functionCall": {"name": "get_weather", "args": {"city": "Tokyo"}}}]},
			{"role": "function", "parts": [{"functionResponse": {"name": "get_weather", "response": {"result": "Sunny"}}}]}
		]
	}`)

	out, err := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	if err != nil {
		t.Fatalf("ConvertGeminiRequestToOpenAI() error = %v", err)
	}
	messages := ojson.GetBytesNoCopy(out, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3 (user, assistant, tool); out=%s", len(messages), string(out))
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("messages[0].role = %q, want user; out=%s", got, string(out))
	}
	if got := messages[1].Get("role").String(); got != "assistant" {
		t.Fatalf("messages[1].role = %q, want assistant; out=%s", got, string(out))
	}
	if got := messages[2].Get("role").String(); got != "tool" {
		t.Fatalf("messages[2].role = %q, want tool; out=%s", got, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_NormalizesNumericProtoSchemaTypes_Issue8032(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
		"tools": [{
			"functionDeclarations": [{
				"name": "search",
				"parameters": {
					"type": 6,
					"properties": {
						"query": {"type": 1},
						"tags": {"type": 5, "items": {"type": 1}}
					}
				}
			}]
		}]
	}`)

	out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
	if err != nil {
		t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", err)
	}

	schema := ojson.GetBytesNoCopy(out, "tools.0.function.parameters")
	for _, tc := range []struct {
		path string
		want string
	}{
		{"type", "object"},
		{"properties.query.type", "string"},
		{"properties.tags.type", "array"},
		{"properties.tags.items.type", "string"},
	} {
		got := schema.Get(tc.path)
		if got.Type != ojson.String || got.String() != tc.want {
			t.Errorf("tools.0.function.parameters.%s = %s (type %v), want string %q", tc.path, got.Raw, got.Type, tc.want)
		}
	}
}

func TestConvertGeminiRequestToOpenAI_SkipsNullAndInvalidGenerationConfigNumbers(t *testing.T) {
	t.Run("null numeric fields are omitted", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"generationConfig": {
				"temperature": null,
				"maxOutputTokens": null,
				"topP": null,
				"topK": null,
				"candidateCount": null,
				"thinkingConfig": {
					"thinkingLevel": null,
					"thinkingBudget": null
				}
			}
		}`)

		out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", err)
		}

		for _, field := range []string{"temperature", "max_tokens", "top_p", "top_k", "n", "reasoning_effort"} {
			if got := ojson.GetBytesNoCopy(out, field); got.Exists() {
				t.Errorf("%s = %s, want omitted for null input; out=%s", field, got.Raw, string(out))
			}
		}
	})

	t.Run("non-positive maxOutputTokens and candidateCount are omitted while zero temperature and topP are kept", func(t *testing.T) {
		inputJSON := []byte(`{
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"generationConfig": {
				"temperature": 0,
				"maxOutputTokens": 0,
				"topP": 0,
				"topK": "bad",
				"candidateCount": 0,
				"thinkingConfig": {
					"thinkingLevel": null,
					"thinkingBudget": 4096
				}
			}
		}`)

		out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", err)
		}

		if got := ojson.GetBytesNoCopy(out, "temperature"); !got.Exists() || got.Float() != 0 {
			t.Errorf("temperature = %s, want 0", got.Raw)
		}
		if got := ojson.GetBytesNoCopy(out, "top_p"); !got.Exists() || got.Float() != 0 {
			t.Errorf("top_p = %s, want 0", got.Raw)
		}
		for _, field := range []string{"max_tokens", "top_k", "n"} {
			if got := ojson.GetBytesNoCopy(out, field); got.Exists() {
				t.Errorf("%s = %s, want omitted; out=%s", field, got.Raw, string(out))
			}
		}
		if got := ojson.GetBytesNoCopy(out, "reasoning_effort").String(); got != "medium" {
			t.Errorf("reasoning_effort = %q, want %q when thinkingLevel is null and thinkingBudget is 4096; out=%s", got, "medium", string(out))
		}
	})

	t.Run("null thinkingBudget is omitted while numeric zero maps to none", func(t *testing.T) {
		nullBudgetOut, err := ConvertGeminiRequestToOpenAI("gpt-5", []byte(`{
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"generationConfig": {
				"maxOutputTokens": -1,
				"candidateCount": -2,
				"thinkingConfig": {"thinkingBudget": null}
			}
		}`), false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", err)
		}
		for _, field := range []string{"max_tokens", "n", "reasoning_effort"} {
			if got := ojson.GetBytesNoCopy(nullBudgetOut, field); got.Exists() {
				t.Errorf("%s = %s, want omitted; out=%s", field, got.Raw, string(nullBudgetOut))
			}
		}

		zeroBudgetOut, errZero := ConvertGeminiRequestToOpenAI("gpt-5", []byte(`{
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"generation_config": {
				"thinking_config": {"thinking_budget": 0}
			}
		}`), false)
		if errZero != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", errZero)
		}
		if got := ojson.GetBytesNoCopy(zeroBudgetOut, "reasoning_effort").String(); got != "none" {
			t.Errorf("reasoning_effort = %q, want none for numeric thinking_budget 0; out=%s", got, string(zeroBudgetOut))
		}
	})
}

func TestConvertGeminiRequestToOpenAI_DefaultsNonObjectFunctionCallArgsToEmptyObject(t *testing.T) {
	tests := []struct {
		name     string
		partJSON string
		wantArgs string
	}{
		{name: "null args", partJSON: `{"functionCall":{"name":"lookup","args":null}}`, wantArgs: "{}"},
		{name: "array args", partJSON: `{"functionCall":{"name":"lookup","args":[1,2]}}`, wantArgs: "{}"},
		{name: "string args", partJSON: `{"functionCall":{"name":"lookup","args":"raw"}}`, wantArgs: "{}"},
		{name: "number args", partJSON: `{"functionCall":{"name":"lookup","args":42}}`, wantArgs: "{}"},
		{name: "omitted args", partJSON: `{"functionCall":{"name":"lookup"}}`, wantArgs: "{}"},
		{name: "snake_case function_call null args", partJSON: `{"function_call":{"name":"lookup","args":null}}`, wantArgs: "{}"},
		{name: "object args preserved", partJSON: `{"functionCall":{"name":"lookup","args":{"city":"Paris"}}}`, wantArgs: `{"city":"Paris"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputJSON := []byte(`{"contents":[{"role":"model","parts":[` + tt.partJSON + `]}]}`)
			out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
			if err != nil {
				t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", err)
			}
			if got := ojson.GetBytesNoCopy(out, "messages.0.tool_calls.0.function.arguments").String(); got != tt.wantArgs {
				t.Fatalf("function.arguments = %q, want %q; out=%s", got, tt.wantArgs, string(out))
			}
		})
	}
}

func TestConvertGeminiRequestToOpenAI_SupportsSnakeCaseGeminiSDKFields(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [{"role": "user", "parts": [{"text": "check weather"}]}],
		"generation_config": {
			"temperature": 0.4,
			"max_output_tokens": 256,
			"top_p": 0.85,
			"top_k": 32,
			"stop_sequences": ["END", "STOP"],
			"candidate_count": 2,
			"response_modalities": ["TEXT", "IMAGE"],
			"thinking_config": {
				"thinking_level": "HIGH"
			}
		},
		"tools": [{
			"function_declarations": [{
				"name": "lookup_weather",
				"description": "Look up weather",
				"parameters": null,
				"parameters_json_schema": {
					"type": "object",
					"properties": {
						"city": {"type": "string"}
					}
				}
			}]
		}],
		"tool_config": {
			"function_calling_config": {
				"mode": "ANY",
				"allowed_function_names": ["lookup_weather"]
			}
		}
	}`)

	out, err := ConvertGeminiRequestToOpenAI("gpt-5", inputJSON, false)
	if err != nil {
		t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", err)
	}

	if got := ojson.GetBytesNoCopy(out, "temperature").Float(); got != 0.4 {
		t.Errorf("temperature = %v, want 0.4", got)
	}
	if got := ojson.GetBytesNoCopy(out, "max_tokens").Int(); got != 256 {
		t.Errorf("max_tokens = %d, want 256", got)
	}
	if got := ojson.GetBytesNoCopy(out, "top_p").Float(); got != 0.85 {
		t.Errorf("top_p = %v, want 0.85", got)
	}
	if got := ojson.GetBytesNoCopy(out, "top_k").Int(); got != 32 {
		t.Errorf("top_k = %d, want 32", got)
	}
	if got := ojson.GetBytesNoCopy(out, "stop.0").String(); got != "END" {
		t.Errorf("stop.0 = %q, want END", got)
	}
	if got := ojson.GetBytesNoCopy(out, "stop.1").String(); got != "STOP" {
		t.Errorf("stop.1 = %q, want STOP", got)
	}
	if got := ojson.GetBytesNoCopy(out, "n").Int(); got != 2 {
		t.Errorf("n = %d, want 2", got)
	}
	if got := ojson.GetBytesNoCopy(out, "modalities.0").String(); got != "text" {
		t.Errorf("modalities.0 = %q, want text", got)
	}
	if got := ojson.GetBytesNoCopy(out, "modalities.1").String(); got != "image" {
		t.Errorf("modalities.1 = %q, want image", got)
	}
	if got := ojson.GetBytesNoCopy(out, "reasoning_effort").String(); got != "high" {
		t.Errorf("reasoning_effort = %q, want high", got)
	}
	if got := ojson.GetBytesNoCopy(out, "tools.0.function.name").String(); got != "lookup_weather" {
		t.Errorf("tools.0.function.name = %q, want lookup_weather", got)
	}
	if got := ojson.GetBytesNoCopy(out, "tools.0.function.parameters.properties.city.type").String(); got != "string" {
		t.Errorf("tools.0.function.parameters.properties.city.type = %q, want string", got)
	}
	if got := ojson.GetBytesNoCopy(out, "tool_choice.type").String(); got != "function" {
		t.Errorf("tool_choice.type = %q, want function", got)
	}
	if got := ojson.GetBytesNoCopy(out, "tool_choice.function.name").String(); got != "lookup_weather" {
		t.Errorf("tool_choice.function.name = %q, want lookup_weather", got)
	}

	t.Run("snake_case tool_config modes and mixed spellings", func(t *testing.T) {
		cases := []struct {
			name     string
			input    string
			wantPath string
			wantVal  string
			wantErr  bool // ccLoad fails closed when an ANY allowlist names no declared function.
		}{
			{
				name:     "snake NONE",
				input:    `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tool_config":{"function_calling_config":{"mode":"NONE"}}}`,
				wantPath: "tool_choice",
				wantVal:  "none",
			},
			{
				name:     "snake AUTO",
				input:    `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tool_config":{"function_calling_config":{"mode":"AUTO"}}}`,
				wantPath: "tool_choice",
				wantVal:  "auto",
			},
			{
				name:    "snake ANY multiple allowed_function_names",
				input:   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tool_config":{"function_calling_config":{"mode":"ANY","allowed_function_names":["a","b"]}}}`,
				wantErr: true,
			},
			{
				name:    "mixed camel toolConfig with snake function_calling_config",
				input:   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"toolConfig":{"function_calling_config":{"mode":"ANY","allowed_function_names":["only_tool"]}}}`,
				wantErr: true,
			},
			{
				name:    "snake ANY with non-string allowed_function_names falls back to required",
				input:   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tool_config":{"function_calling_config":{"mode":"ANY","allowed_function_names":[null,""]}}}`,
				wantErr: true,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				gotOut, errConv := ConvertGeminiRequestToOpenAI("gpt-5", []byte(tc.input), false)
				if tc.wantErr {
					if errConv == nil {
						t.Fatalf("expected allowlist error, got %s", gotOut)
					}
					return
				}
				if errConv != nil {
					t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", errConv)
				}
				if got := ojson.GetBytesNoCopy(gotOut, tc.wantPath).String(); got != tc.wantVal {
					t.Fatalf("%s = %q, want %q; out=%s", tc.wantPath, got, tc.wantVal, string(gotOut))
				}
			})
		}
	})

	t.Run("multi-name allowed_function_names filters declared tools", func(t *testing.T) {
		multiInput := []byte(`{
			"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
			"tools": [{
				"function_declarations": [
					{"name": "a", "description": "Tool A"},
					{"name": "b", "description": "Tool B"},
					{"name": "c", "description": "Tool C"}
				]
			}],
			"tool_config": {
				"function_calling_config": {
					"mode": "ANY",
					"allowed_function_names": ["a", "b"]
				}
			}
		}`)

		gotOut, errConv := ConvertGeminiRequestToOpenAI("gpt-5", multiInput, false)
		if errConv != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", errConv)
		}
		if got := ojson.GetBytesNoCopy(gotOut, "tool_choice").String(); got != "required" {
			t.Fatalf("tool_choice = %q, want required; out=%s", got, string(gotOut))
		}
		gotTools := ojson.GetBytesNoCopy(gotOut, "tools").Array()
		if len(gotTools) != 2 {
			t.Fatalf("len(tools) = %d, want 2; out=%s", len(gotTools), string(gotOut))
		}
		if gotTools[0].Get("function.name").String() != "a" || gotTools[1].Get("function.name").String() != "b" {
			t.Fatalf("filtered tool names = [%q, %q], want [a, b]; out=%s",
				gotTools[0].Get("function.name").String(), gotTools[1].Get("function.name").String(), string(gotOut))
		}
	})

	t.Run("multi-name allowed_function_names with no matching declared tools fails closed", func(t *testing.T) {
		noMatchInput := []byte(`{
			"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
			"tools": [{
				"function_declarations": [
					{"name": "c", "description": "Tool C"}
				]
			}],
			"tool_config": {
				"function_calling_config": {
					"mode": "ANY",
					"allowed_function_names": ["a", "b"]
				}
			}
		}`)

		if gotOut, errConv := ConvertGeminiRequestToOpenAI("gpt-5", noMatchInput, false); errConv == nil {
			t.Fatalf("expected allowlist error, got %s", gotOut)
		}
	})

	t.Run("camelCase functionDeclarations takes precedence without duplicating snake_case alias", func(t *testing.T) {
		dualInput := []byte(`{
			"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
			"tools": [{
				"functionDeclarations": [{"name": "lookup", "description": "camel"}],
				"function_declarations": [{"name": "lookup", "description": "snake"}]
			}]
		}`)

		gotOut, errConv := ConvertGeminiRequestToOpenAI("gpt-5", dualInput, false)
		if errConv != nil {
			t.Fatalf("ConvertGeminiRequestToOpenAI returned error: %v", errConv)
		}
		gotTools := ojson.GetBytesNoCopy(gotOut, "tools").Array()
		if len(gotTools) != 1 {
			t.Fatalf("len(tools) = %d, want 1; out=%s", len(gotTools), string(gotOut))
		}
		if got := gotTools[0].Get("function.description").String(); got != "camel" {
			t.Fatalf("tools.0.function.description = %q, want camel; out=%s", got, string(gotOut))
		}
	})
}
