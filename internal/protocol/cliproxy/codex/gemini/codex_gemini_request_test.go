package gemini

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestConvertGeminiRequestToCodex_SDKToolChoiceUsesDeclarationNames(t *testing.T) {
	prefix := strings.Repeat("a", 64)
	first, second := prefix+"first", prefix+"second"
	shortSecond := prefix[:62] + "_1"
	for _, tc := range []struct {
		name, allowlist, choiceType string
	}{
		{"single name", fmt.Sprintf("[%q]", second), "function"},
		{"multiple names", fmt.Sprintf("[%q,null,%q,%q,%q]", second, first, second, prefix+"unknown"), "allowed_tools"},
		{"valid and colliding undeclared name", fmt.Sprintf("[%q,%q]", second, prefix+"unknown"), "function"},
		{"only colliding undeclared name", fmt.Sprintf("[%q]", prefix+"unknown"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := fmt.Appendf(nil, `{
				"contents":[
					{"parts":[{"text":"hello"}]},
					{"parts":[{"function_call":{"name":%q,"id":"call-x","args":{"query":"term"}}}]},
					{"parts":[{"function_response":{"name":%q,"id":"call-x","response":{"result":"ok"}}}]}
				],
				"tools":[
					{"functionDeclarations":[{"name":%q}],"function_declarations":[{"name":%q}]},
					{"functionDeclarations":null,"function_declarations":[
						{"name":%q,"parametersJsonSchema":null,"parameters_json_schema":{"type":"OBJECT","properties":{"query":{"type":"STRING"}}}},
						{"name":"excluded"}
					]}
				],
				"tool_config":{"function_calling_config":{"mode":"ANY","allowed_function_names":%s}}
			}`, second, second, first, first, second, tc.allowlist)
			out, err := ConvertGeminiRequestToCodex("gpt-6.2-sol", request, false)
			if tc.choiceType == "" {
				if err == nil || len(out) != 0 {
					t.Fatalf("expected the colliding undeclared name to fail, got %s, %v", out, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			root := gjson.ParseBytes(out)
			tools := root.Get("tools").Array()
			if len(tools) != 3 || tools[0].Get("name").String() != prefix || tools[1].Get("name").String() != shortSecond ||
				tools[1].Get("parameters.properties.query.type").String() != "string" {
				t.Fatalf("expected disambiguated declarations and snake_case schema, got %s", out)
			}
			if root.Get("input.1.name").String() != shortSecond {
				t.Fatalf("expected history to use the declared name %q, got %s", shortSecond, out)
			}
			choice := root.Get("tool_choice")
			if choice.Get("type").String() != tc.choiceType {
				t.Fatalf("expected tool choice type %s, got %s", tc.choiceType, choice.Raw)
			}
			if tc.choiceType == "function" {
				if choice.Get("name").String() != shortSecond {
					t.Fatalf("expected to force the second declaration, got %s", choice.Raw)
				}
			} else {
				allowed := choice.Get("tools").Array()
				if choice.Get("mode").String() != "required" ||
					len(allowed) != 2 || allowed[0].Get("name").String() != shortSecond || allowed[1].Get("name").String() != prefix {
					t.Fatalf("expected only the two allowed declarations, got %s", choice.Raw)
				}
			}
			response := fmt.Appendf(nil, `{"type":"response.completed","response":{"output":[{"type":"function_call","name":%q,"arguments":"{}"}]}}`, shortSecond)
			got := ConvertCodexResponseToGeminiNonStream(t.Context(), "gemini", request, out, response, nil)
			if gjson.GetBytes(got, "candidates.0.content.parts.0.functionCall.name").String() != second {
				t.Fatalf("expected the original tool name %q, got %s", second, got)
			}
			var state any
			event := fmt.Appendf(nil, `data: {"type":"response.output_item.done","item":{"type":"function_call","name":%q,"arguments":"{}"}}`, shortSecond)
			frames := ConvertCodexResponseToGemini(t.Context(), "gemini", request, out, event, &state)
			if len(frames) != 1 || gjson.GetBytes(frames[0], "candidates.0.content.parts.0.functionCall.name").String() != second {
				t.Fatalf("expected the original streamed tool name %q, got %s", second, frames)
			}
		})
	}
}

func TestConvertGeminiRequestToCodex_MaxOutputTokens(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int64
	}{
		{"generation config", `{"generationConfig":{"maxOutputTokens":2048}}`, 2048},
		{"snake token field", `{"generationConfig":{"max_output_tokens":2048}}`, 2048},
		{"snake config", `{"generation_config":{"maxOutputTokens":2048}}`, 2048},
		{"snake config and field", `{"generation_config":{"max_output_tokens":2048}}`, 2048},
		{"null config and field aliases", `{"generationConfig":null,"generation_config":{"maxOutputTokens":null,"max_output_tokens":2048}}`, 2048},
		{"top level camel", `{"maxOutputTokens":1024}`, 1024},
		{"top level snake", `{"max_output_tokens":1024}`, 1024},
		{"legacy", `{"max_tokens":512}`, 512},
		{"nested wins", `{"generationConfig":{"maxOutputTokens":2048,"max_output_tokens":1024},"maxOutputTokens":512,"max_output_tokens":256,"max_tokens":128}`, 2048},
		{"top camel wins", `{"maxOutputTokens":512,"max_output_tokens":256,"max_tokens":128}`, 512},
		{"skip nonnumeric", `{"generationConfig":{"maxOutputTokens":"2048"},"max_tokens":512}`, 512},
		{"zero", `{"generationConfig":{"maxOutputTokens":0}}`, 0},
		{"null", `{"generationConfig":{"maxOutputTokens":null}}`, -1},
		{"absent", `{}`, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", []byte(tt.input), false)
			if err != nil {
				t.Fatal(err)
			}
			got := gjson.GetBytes(out, "max_output_tokens")
			if tt.want < 0 {
				if got.Exists() {
					t.Fatalf("unexpected max_output_tokens: %s", out)
				}
			} else if got.Type != gjson.Number || got.Int() != tt.want {
				t.Fatalf("max_output_tokens = %s, want %d; output=%s", got.Raw, tt.want, out)
			}
		})
	}
}

func TestConvertGeminiRequestToCodex_ThinkingConfigAliases(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"snake config and mixed aliases", `{"generation_config":{"thinkingConfig":null,"thinking_config":{"thinkingLevel":null,"thinking_level":"LOW","thinking_budget":0}}}`, "low"},
		{"outer level wins", `{"generation_config":{"thinking_level":"HIGH","thinking_config":{"thinking_level":"LOW"}}}`, "high"},
		{"empty outer level falls through", `{"generationConfig":{"thinkingLevel":"","thinking_config":{"thinkingLevel":null,"thinking_budget":0}}}`, "none"},
		{"explicit zero", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":null,"thinking_budget":0}}}`, "none"},
		{"budget in alternate thinking object", `{"generationConfig":{"thinkingConfig":{"includeThoughts":false},"thinking_config":{"thinking_budget":1024}}}`, "low"},
		{"level in alternate generation object overrides budget", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}},"generation_config":{"thinkingConfig":{"thinking_level":"HIGH"}}}`, "high"},
		{"invalid integer budgets fall through", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0.5,"thinking_budget":-2}},"generation_config":{"thinking_config":{"thinking_budget":1024}}}`, "low"},
		{"snake generation prefers snake thinking object", `{"generation_config":{"thinkingConfig":{"thinkingLevel":"HIGH"},"thinking_config":{"thinkingLevel":"LOW"}}}`, "low"},
		{"automatic level", `{"generationConfig":{"thinking_config":{"thinking_level":" AUTO ","thinking_budget":0}}}`, ""},
		{"automatic budget", `{"generation_config":{"thinking_config":{"thinking_budget":-1}}}`, ""},
		{"null budget", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":null}}}`, "medium"},
		{"missing config", `{}`, "medium"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", []byte(tt.input), false)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "reasoning.effort"); got.String() != tt.want || (tt.want == "" && got.Exists()) {
				t.Fatalf("reasoning.effort = %s, want %q (output: %s)", got.Raw, tt.want, out)
			}
		})
	}
}

func TestConvertGeminiRequestToCodex_OmittedRolesAndToolAliases(t *testing.T) {
	for _, fields := range [][2]string{{"functionCall", "functionResponse"}, {"function_call", "function_response"}} {
		t.Run(fields[0], func(t *testing.T) {
			input := fmt.Sprintf(`{"contents":[
				{"parts":[{"text":"first"}]},
				{"parts":[{"text":"second"}]},
				{"parts":[{"text":"checking"},{%q:{"id":"call_lookup","name":"lookup","args":{"query":"status"}}}]},
				{"parts":[{%q:{"name":"lookup","response":{"result":"ok"}}}]},
				{"parts":[{"text":"followup"}]}
			]}`, fields[0], fields[1])
			out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", []byte(input), false)
			if err != nil {
				t.Fatal(err)
			}
			items := gjson.GetBytes(out, "input").Array()
			if len(items) != 6 {
				t.Fatalf("want six input items, got %s", out)
			}
			for _, index := range []int{0, 1, 5} {
				if items[index].Get("role").String() != "user" {
					t.Fatalf("plain text without role must stay user: %s", out)
				}
			}
			if items[2].Get("role").String() != "assistant" || items[2].Get("content.0.type").String() != "output_text" {
				t.Fatalf("function call turn must use assistant text: %s", out)
			}
			if items[3].Get("type").String() != "function_call" || items[3].Get("call_id").String() != "call_lookup" ||
				gjson.Parse(items[3].Get("arguments").String()).Get("query").String() != "status" {
				t.Fatalf("function call not preserved: %s", out)
			}
			if items[4].Get("type").String() != "function_call_output" || items[4].Get("call_id").String() != "call_lookup" || items[4].Get("output").String() != "ok" {
				t.Fatalf("function response not paired: %s", out)
			}
		})
	}
}

func TestConvertGeminiRequestToCodex_HistoryToolArguments(t *testing.T) {
	for _, tt := range []struct {
		name      string
		argsField string
		wantArgs  string
	}{
		{"null", `,"args":null`, "{}"},
		{"array", `,"args":[1,2]`, "{}"},
		{"string", `,"args":"{\"q\":\"s\"}"`, "{}"},
		{"missing", "", "{}"},
		{"object", `,"args":{"q":"s"}`, `{"q":"s"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := fmt.Sprintf(`{"contents":[
				{"role":"model","parts":[{"functionCall":{"name":"lookup"%s}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"ok"}}}]}
			]}`, tt.argsField)
			out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", []byte(input), false)
			if err != nil {
				t.Fatal(err)
			}
			const wantID = "call_gemini_0000000000000001"
			callID := gjson.GetBytes(out, "input.0.call_id").String()
			resultID := gjson.GetBytes(out, "input.1.call_id").String()
			if callID != wantID || resultID != wantID {
				t.Fatalf("call id = %q, result id = %q, want %q for both", callID, resultID, wantID)
			}
			if got := gjson.GetBytes(out, "input.0.arguments"); got.Type != gjson.String || got.String() != tt.wantArgs {
				t.Fatalf("arguments = %s, want JSON string %q", got.Raw, tt.wantArgs)
			}
		})
	}
}

func TestConvertGeminiRequestToCodex_PreservesCustomCallIDs(t *testing.T) {
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

			out, _ := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)

			gotCallID := gjson.GetBytes(out, "input.0.call_id").String()
			if gotCallID != tt.want {
				t.Fatalf("expected function_call call_id %q, got %q; output=%s", tt.want, gotCallID, string(out))
			}

			gotOutputID := gjson.GetBytes(out, "input.1.call_id").String()
			if gotOutputID != tt.want {
				t.Fatalf("expected function_call_output call_id %q, got %q; output=%s", tt.want, gotOutputID, string(out))
			}
		})
	}
}

func TestConvertGeminiRequestToCodex_AcceptsInlineData(t *testing.T) {
	out, _ := ConvertGeminiRequestToCodex("gpt-5.1-codex", []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}]}`), false)
	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "input_image" {
		t.Fatalf("content type = %q, want input_image. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.0.image_url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image_url = %q, want data:image/png;base64,aGVsbG8=. Output: %s", got, string(out))
	}
}

func TestConvertGeminiRequestToCodex_SplitsNonImageInlineDataByMIME(t *testing.T) {
	out, _ := ConvertGeminiRequestToCodex("gpt-5.1-codex", []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}},{"inlineData":{"mimeType":"video/mp4","data":"AAAAIGZ0eXA="}},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}}]}]}`), false)

	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "input_audio" {
		t.Fatalf("audio content type = %q, want input_audio. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.1.content.0.type").String(); got != "input_file" {
		t.Fatalf("video content type = %q, want input_file. Output: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.2.content.0.type").String(); got != "input_file" {
		t.Fatalf("document content type = %q, want input_file. Output: %s", got, string(out))
	}
}

func TestConvertGeminiRequestToCodex_DropsHiddenThoughtParts(t *testing.T) {
	t.Run("thought-only turn", func(t *testing.T) {
		out, _ := ConvertGeminiRequestToCodex("codex-test", []byte(`{
			"contents":[
				{"role":"model","parts":[{"thought":true,"text":"internal reasoning","thoughtSignature":"opaque-provider-state"}]},
				{"role":"user","parts":[{"text":"continue"}]}
			]
		}`), false)

		input := gjson.GetBytes(out, "input").Array()
		if len(input) != 1 || input[0].Get("role").String() != "user" || input[0].Get("content.0.text").String() != "continue" {
			t.Fatalf("hidden thought turn was not dropped. Output: %s", string(out))
		}
	})

	t.Run("mixed turn", func(t *testing.T) {
		out, _ := ConvertGeminiRequestToCodex("codex-test", []byte(`{
			"contents":[{"role":"model","parts":[
				{"thought":true,"text":"internal reasoning","thoughtSignature":"opaque-provider-state"},
				{"text":"visible answer"}
			]}]
		}`), false)

		input := gjson.GetBytes(out, "input").Array()
		if len(input) != 1 || input[0].Get("content.0.type").String() != "output_text" || input[0].Get("content.0.text").String() != "visible answer" {
			t.Fatalf("hidden thought was not dropped independently of visible text. Output: %s", string(out))
		}
	})
}

func testGeminiMetaThoughtSignature() string {
	// Shape-valid synthetic Meta envelope; no captured value is embedded.
	signature := "Q-PaDgE"
	for len(signature) < 1426 {
		signature += "A"
	}
	return signature
}

func TestConvertGeminiRequestToCodex_PreservesMetaThoughtSignatureForMuseTarget(t *testing.T) {
	signature := testGeminiMetaThoughtSignature()
	payload := []byte(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"internal reasoning","thoughtSignature":""},{"text":"visible answer"}]},{"role":"user","parts":[{"text":"continue"}]}]}`)
	payload, _ = sjson.SetBytes(payload, "contents.0.parts.0.thoughtSignature", signature)

	out, _ := ConvertGeminiRequestToCodex("muse-spark-1.3", payload, false)
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 3 {
		t.Fatalf("input len = %d, want reasoning+message+user. Output: %s", len(input), string(out))
	}
	if got := input[0].Get("type").String(); got != "reasoning" {
		t.Fatalf("input.0 type = %q, want reasoning. Output: %s", got, string(out))
	}
	if got := input[0].Get("encrypted_content").String(); got != signature {
		t.Fatalf("encrypted_content = %q, want Meta signature", got)
	}
	if got := input[0].Get("summary.0.text").String(); got != "internal reasoning" {
		t.Fatalf("summary text = %q, want thought text. Output: %s", got, string(out))
	}
	if got := input[1].Get("content.0.text").String(); got != "visible answer" {
		t.Fatalf("visible text = %q. Output: %s", got, string(out))
	}
}

func TestConvertGeminiRequestToCodex_DropsMetaThoughtSignatureForNonMetaTargets(t *testing.T) {
	signature := testGeminiMetaThoughtSignature()
	payload := []byte(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"internal reasoning","thoughtSignature":""},{"text":"visible answer"}]},{"role":"user","parts":[{"text":"continue"}]}]}`)
	payload, _ = sjson.SetBytes(payload, "contents.0.parts.0.thoughtSignature", signature)

	for _, modelName := range []string{"gpt-5.1-codex", "claude-sonnet-4-6", "grok-4.5"} {
		t.Run(modelName, func(t *testing.T) {
			out, _ := ConvertGeminiRequestToCodex(modelName, payload, false)
			for _, item := range gjson.GetBytes(out, "input").Array() {
				if item.Get("type").String() == "reasoning" {
					t.Fatalf("Meta signature leaked to %s target. Output: %s", modelName, string(out))
				}
			}
		})
	}
}

func testGeminiGPTThoughtSignature() string {
	decoded := make([]byte, 89)
	decoded[0] = 0x80
	for i := 9; i < len(decoded); i++ {
		decoded[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(decoded)
}

func TestConvertGeminiRequestToCodex_NormalizesPrefixedAndCarrierSignatures(t *testing.T) {
	gptSignature := testGeminiGPTThoughtSignature()
	metaSignature := testGeminiMetaThoughtSignature()
	// OpenRouter-appended suffix shape: one '.' plus 67 alphanumeric chars.
	routedMetaSignature := metaSignature + "." + strings.Repeat("Ab3", 23)[:67]
	cases := []struct {
		name      string
		model     string
		echoed    string
		want      string
		wantFound bool
	}{
		{"gpt cache prefixed", "gpt-5.1-codex", "gpt#" + gptSignature, gptSignature, true},
		{"carrier wrapped gpt prefixed", "gpt-5.1-codex", encodeGeminiCodexThoughtSignature("gpt#" + gptSignature), gptSignature, true},
		{"carrier wrapped meta", "muse-spark-1.3", encodeGeminiCodexThoughtSignature(metaSignature), metaSignature, true},
		{"carrier wrapped meta to gpt target", "gpt-5.1-codex", encodeGeminiCodexThoughtSignature(metaSignature), "", false},
		{"carrier wrapped routed meta", "meta/muse-wonder", encodeGeminiCodexThoughtSignature(routedMetaSignature), routedMetaSignature, true},
		{"carrier wrapped routed meta to gpt target", "gpt-5.1-codex", encodeGeminiCodexThoughtSignature(routedMetaSignature), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"internal reasoning","thoughtSignature":""},{"text":"visible answer"}]},{"role":"user","parts":[{"text":"continue"}]}]}`)
			payload, _ = sjson.SetBytes(payload, "contents.0.parts.0.thoughtSignature", tc.echoed)
			out, _ := ConvertGeminiRequestToCodex(tc.model, payload, false)
			found := ""
			for _, item := range gjson.GetBytes(out, "input").Array() {
				if item.Get("type").String() == "reasoning" {
					found = item.Get("encrypted_content").String()
				}
			}
			if tc.wantFound && found != tc.want {
				t.Fatalf("encrypted_content = %q, want normalized signature; output=%s", found, string(out))
			}
			if !tc.wantFound && found != "" {
				t.Fatalf("foreign signature leaked as reasoning item; output=%s", string(out))
			}
		})
	}
}

func TestConvertGeminiRequestToCodex_DeterministicCallIDs(t *testing.T) {
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

	out1, _ := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)
	out2, _ := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)

	if string(out1) != string(out2) {
		t.Fatalf("expected deterministic output across multiple conversions, got different outputs:\nout1=%s\nout2=%s", string(out1), string(out2))
	}

	wantID1 := "call_gemini_0000000000000001"
	wantID2 := "call_gemini_0000000000000002"

	gotCall1 := gjson.GetBytes(out1, "input.0.call_id").String()
	gotResp1 := gjson.GetBytes(out1, "input.1.call_id").String()
	gotCall2 := gjson.GetBytes(out1, "input.2.call_id").String()
	gotResp2 := gjson.GetBytes(out1, "input.3.call_id").String()

	if gotCall1 != wantID1 || gotResp1 != wantID1 {
		t.Fatalf("expected first tool pair to have id %q, got call=%q, resp=%q", wantID1, gotCall1, gotResp1)
	}
	if gotCall2 != wantID2 || gotResp2 != wantID2 {
		t.Fatalf("expected second tool pair to have id %q, got call=%q, resp=%q", wantID2, gotCall2, gotResp2)
	}
}

func TestConvertGeminiRequestToCodex_FunctionResponsePartsMedia(t *testing.T) {
	t.Run("inlineData image and PDF alongside response result", func(t *testing.T) {
		raw := []byte(`{
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

		out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToCodex() error = %v", err)
		}
		fnOut := gjson.GetBytes(out, "input.2")
		if got := fnOut.Get("type").String(); got != "function_call_output" {
			t.Fatalf("input.2.type = %q, want function_call_output; out=%s", got, string(out))
		}
		output := fnOut.Get("output")
		if !output.IsArray() {
			t.Fatalf("expected function_call_output.output to be array, got %s; out=%s", output.Raw, string(out))
		}
		items := output.Array()
		if len(items) != 3 {
			t.Fatalf("expected 3 output items (input_text + input_image + input_file), got %d: %s", len(items), output.Raw)
		}
		if items[0].Get("type").String() != "input_text" || items[0].Get("text").String() != "Loaded files" {
			t.Fatalf("items[0] = %s, want input_text 'Loaded files'", items[0].Raw)
		}
		if items[1].Get("type").String() != "input_image" || items[1].Get("image_url").String() != "data:image/png;base64,iVBORw0KGgo=" {
			t.Fatalf("items[1] = %s, want input_image data URL", items[1].Raw)
		}
		if items[2].Get("type").String() != "input_file" || items[2].Get("file_data").String() != "data:application/pdf;base64,JVBERi0xLjQK" || items[2].Get("filename").String() != "document.pdf" {
			t.Fatalf("items[2] = %s, want input_file document.pdf with data URL", items[2].Raw)
		}
	})

	t.Run("fileData image URL with empty response", func(t *testing.T) {
		raw := []byte(`{
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

		out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToCodex() error = %v", err)
		}
		items := gjson.GetBytes(out, "input.1.output").Array()
		if len(items) != 1 {
			t.Fatalf("expected 1 item (no empty input_text), got %d; out=%s", len(items), string(out))
		}
		if items[0].Get("type").String() != "input_image" || items[0].Get("image_url").String() != "https://example.com/a.jpg" {
			t.Fatalf("items[0] = %s, want input_image URL", items[0].Raw)
		}
	})

	t.Run("whitespace-formatted empty response emits no empty input_text and fallback preserves sibling fields", func(t *testing.T) {
		raw := []byte(`{
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "fetch_image", "id": "call_1", "args": {}}},
					{"functionCall": {"name": "read_file", "id": "call_2", "args": {}}}
				]},
				{"role": "user", "parts": [
					{"functionResponse": {
						"name": "fetch_image",
						"id": "call_1",
						"response": { },
						"parts": [
							{"inlineData": {"mimeType": "image/png", "data": "QUFB"}}
						]
					}},
					{"functionResponse": {
						"name": "read_file",
						"id": "call_2",
						"response": {"result": "", "error": "failed"}
					}}
				]}
			]
		}`)

		out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToCodex() error = %v", err)
		}
		items := gjson.GetBytes(out, "input.2.output").Array()
		if len(items) != 1 || items[0].Get("type").String() != "input_image" {
			t.Fatalf("input.2.output = %s, want single input_image item", gjson.GetBytes(out, "input.2.output").Raw)
		}
		fallback := gjson.GetBytes(out, "input.3.output").String()
		if !strings.Contains(fallback, `"error"`) || !strings.Contains(fallback, `"failed"`) {
			t.Fatalf("input.3.output = %q, want full response JSON preserving error field", fallback)
		}
	})

	t.Run("inlineData audio in functionResponse.parts is not placed inside function_call_output and is relayed after contiguous tool outputs", func(t *testing.T) {
		raw := []byte(`{
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "read_audio", "id": "call_a", "args": {}}},
					{"functionCall": {"name": "read_text", "id": "call_b", "args": {}}}
				]},
				{"role": "user", "parts": [{
					"functionResponse": {
						"name": "read_audio",
						"id": "call_a",
						"response": {},
						"parts": [
							{"inlineData": {"mimeType": "audio/wav", "data": "UklGRg=="}}
						]
					}
				}]},
				{"role": "user", "parts": [{
					"functionResponse": {
						"name": "read_text",
						"id": "call_b",
						"response": {"result": "done"}
					}
				}]}
			]
		}`)

		out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToCodex() error = %v", err)
		}
		items := gjson.GetBytes(out, "input").Array()
		if len(items) != 5 {
			t.Fatalf("len(input) = %d, want 5 (2 function_call, 2 function_call_output, 1 user audio relay); out=%s", len(items), string(out))
		}
		if items[2].Get("type").String() != "function_call_output" || items[2].Get("call_id").String() != "call_a" {
			t.Fatalf("input[2] = %s, want function_call_output call_a", items[2].Raw)
		}
		if strings.Contains(items[2].Raw, "input_audio") {
			t.Fatalf("function_call_output must not contain input_audio: %s", items[2].Raw)
		}
		if items[2].Get("output").String() == "" {
			t.Fatalf("input[2].output must not be empty: %s", items[2].Raw)
		}
		if items[3].Get("type").String() != "function_call_output" || items[3].Get("call_id").String() != "call_b" {
			t.Fatalf("input[3] = %s, want function_call_output call_b before user audio relay; out=%s", items[3].Raw, string(out))
		}
		if items[4].Get("type").String() != "message" || items[4].Get("role").String() != "user" {
			t.Fatalf("input[4] = %s, want user message relay", items[4].Raw)
		}
		audioPart := items[4].Get("content.1")
		if audioPart.Get("type").String() != "input_audio" || audioPart.Get("input_audio.data").String() != "UklGRg==" || audioPart.Get("input_audio.format").String() != "wav" {
			t.Fatalf("input[4].content[1] = %s, want input_audio wav", audioPart.Raw)
		}
		noticeText := items[4].Get("content.0.text").String()
		if !strings.Contains(noticeText, "read_audio") || !strings.Contains(noticeText, "call_a") {
			t.Fatalf("input[4].content[0].text = %q, want attribution to read_audio and call_a", noticeText)
		}
	})

	t.Run("parallel tool calls with identical text and only one audio attachment still preserve tool attribution", func(t *testing.T) {
		raw := []byte(`{
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "clip_a", "id": "call_1", "args": {}}},
					{"functionCall": {"name": "clip_b", "id": "call_2", "args": {}}}
				]},
				{"role": "user", "parts": [
					{"functionResponse": {
						"name": "clip_a",
						"id": "call_1",
						"response": {"result": "ok"},
						"parts": [
							{"inlineData": {"mimeType": "audio/wav", "data": "UklGRg=="}}
						]
					}},
					{"functionResponse": {
						"name": "clip_b",
						"id": "call_2",
						"response": {"result": "ok"}
					}}
				]}
			]
		}`)

		out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToCodex() error = %v", err)
		}
		items := gjson.GetBytes(out, "input").Array()
		if len(items) != 5 {
			t.Fatalf("len(input) = %d, want 5; out=%s", len(items), string(out))
		}
		noticeText := items[4].Get("content.0.text").String()
		if !strings.Contains(noticeText, "clip_a") || !strings.Contains(noticeText, "call_1") {
			t.Fatalf("relay notice = %q, want attribution to clip_a and call_1", noticeText)
		}
		if strings.Contains(noticeText, "clip_b") || strings.Contains(noticeText, "call_2") {
			t.Fatalf("relay notice = %q, must not misattribute clip_b/call_2", noticeText)
		}
	})

	t.Run("parallel tool audio relay preserves per-tool attribution labels when multiple tools return audio", func(t *testing.T) {
		raw := []byte(`{
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "rec_a", "id": "call_a", "args": {}}},
					{"functionCall": {"name": "rec_b", "id": "call_b", "args": {}}}
				]},
				{"role": "user", "parts": [
					{"functionResponse": {
						"name": "rec_a",
						"id": "call_a",
						"response": {},
						"parts": [
							{"inlineData": {"mimeType": "audio/wav", "data": "QUFB"}},
							{"inlineData": {"mimeType": "audio/wav", "data": "QkJC"}}
						]
					}},
					{"functionResponse": {
						"name": "rec_b",
						"id": "call_b",
						"response": {},
						"parts": [
							{"inlineData": {"mimeType": "audio/mp3", "data": "Q0ND"}}
						]
					}}
				]}
			]
		}`)

		out, err := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)
		if err != nil {
			t.Fatalf("ConvertGeminiRequestToCodex() error = %v", err)
		}
		items := gjson.GetBytes(out, "input").Array()
		if len(items) != 5 {
			t.Fatalf("len(input) = %d, want 5; out=%s", len(items), string(out))
		}
		relayParts := items[4].Get("content").Array()
		if len(relayParts) != 6 {
			t.Fatalf("len(relayParts) = %d, want 6 with per-tool attribution labels; content=%s", len(relayParts), items[4].Get("content").Raw)
		}
		if relayParts[1].Get("type").String() != "input_text" || !strings.Contains(relayParts[1].Get("text").String(), "call_a") || !strings.Contains(relayParts[1].Get("text").String(), "rec_a") {
			t.Fatalf("relayParts[1] = %s, want label identifying rec_a / call_a", relayParts[1].Raw)
		}
		if relayParts[4].Get("type").String() != "input_text" || !strings.Contains(relayParts[4].Get("text").String(), "call_b") || !strings.Contains(relayParts[4].Get("text").String(), "rec_b") {
			t.Fatalf("relayParts[4] = %s, want label identifying rec_b / call_b", relayParts[4].Raw)
		}
	})
}
