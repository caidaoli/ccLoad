package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	antigravityclaude "ccLoad/internal/protocol/cliproxy/providers/antigravity/claude"

	"github.com/tidwall/gjson"
)

const antigravitySchemaTestModel = "gemini-3-pro"

func TestAntigravityGeminiWireRejectsVertexSignature(t *testing.T) {
	t.Parallel()
	sig := base64.StdEncoding.EncodeToString(append([]byte{1, 0x8f, 0x3d, 0x6b, 0x5f}, bytes.Repeat([]byte{0x42}, 400)...))
	body := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"` + sig + `"}]}]}`)
	cfg := &model.Config{AuthType: model.AuthTypeAntigravityOAuth, AntigravityProjectID: "test-project"}
	got, err := prepareAntigravityRequestBody(cfg, "gemini-3-pro", body, body, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	part := gjson.GetBytes(got, "request.contents.0.parts.0")
	if part.Get("thoughtSignature").String() != "skip_thought_signature_validator" || part.Get("functionCall.name").String() != "lookup" {
		t.Fatalf("Vertex signature reached Antigravity: %s", got)
	}
}

func TestPrepareAntigravityRequestBodyClaude55StructuredOutput(t *testing.T) {
	t.Parallel()

	const schema = `{"type":"object","title":"Answer","description":"Final answer","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}},` +
		`"generationConfig":{"temperature":0.2,"responseMimeType":"application/json","responseSchema":` + schema + `}}}`)
	cfg := &model.Config{AuthType: model.AuthTypeAntigravityOAuth, AntigravityProjectID: "gravity-project"}
	// 敏感词命中 schema 字段名：提示词必须在混淆之后追加，否则 schema 被插入零宽字符。
	matcher := regexp.MustCompile(`answer`)

	t.Run("claude 5.5 moves schema into the system prompt", func(t *testing.T) {
		t.Parallel()
		got, err := prepareAntigravityRequestBody(cfg, "claude-opus-5-5-high", body, body, http.Header{}, matcher)
		if err != nil {
			t.Fatal(err)
		}
		request := gjson.GetBytes(got, "request")
		if mode := request.Get("toolConfig.functionCallingConfig.mode").String(); mode != "AUTO" {
			t.Fatalf("mode=%q, want AUTO: %s", mode, got)
		}
		for _, path := range []string{"toolConfig.functionCallingConfig.allowedFunctionNames", "generationConfig.responseSchema", "generationConfig.responseMimeType"} {
			if request.Get(path).Exists() {
				t.Fatalf("%s survived: %s", path, got)
			}
		}
		if request.Get("generationConfig.temperature").Float() != 0.2 {
			t.Fatalf("unrelated generationConfig field lost: %s", got)
		}
		parts := request.Get("systemInstruction.parts").Array()
		if len(parts) == 0 {
			t.Fatalf("missing system prompt: %s", got)
		}
		prompt := parts[len(parts)-1].Get("text").String()
		start := strings.IndexByte(prompt, '{')
		if start < 0 {
			t.Fatalf("prompt has no schema: %q", prompt)
		}
		var gotSchema, wantSchema any
		if err := json.NewDecoder(strings.NewReader(prompt[start:])).Decode(&gotSchema); err != nil {
			t.Fatalf("decode prompt schema: %v: %q", err, prompt)
		}
		if err := json.Unmarshal([]byte(schema), &wantSchema); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotSchema, wantSchema) {
			t.Fatalf("prompt schema=%v, want untouched %v", gotSchema, wantSchema)
		}
	})

	t.Run("other claude keeps native structured output", func(t *testing.T) {
		t.Parallel()
		got, err := prepareAntigravityRequestBody(cfg, "claude-sonnet-4-6", body, body, http.Header{}, matcher)
		if err != nil {
			t.Fatal(err)
		}
		if mode := gjson.GetBytes(got, "request.toolConfig.functionCallingConfig.mode").String(); mode != "VALIDATED" {
			t.Fatalf("mode=%q, want VALIDATED: %s", mode, got)
		}
		if !gjson.GetBytes(got, "request.generationConfig.responseSchema").IsObject() {
			t.Fatalf("responseSchema removed: %s", got)
		}
	})
}

func TestNormalizeAntigravitySchemasConsolidatesParametersAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantSource string
	}{
		{
			name: "canonical and two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"parameters":{"type":"object","properties":{"canonical":{"type":"string"}}},
				"parametersJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"parameters_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "canonical",
		},
		{
			name: "two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"parametersJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"parameters_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "camel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeAntigravitySchemas([]byte(tt.input), antigravitySchemaTestModel)
			basePath := "tools.0.functionDeclarations.0"
			if !gjson.GetBytes(got, basePath+".parameters").IsObject() {
				t.Fatalf("parameters missing: %s", got)
			}
			if gotProp := gjson.GetBytes(got, basePath+".parameters.properties."+tt.wantSource+".type").String(); gotProp != "string" {
				t.Fatalf("parameters.properties.%s.type = %q, want string; output=%s", tt.wantSource, gotProp, got)
			}
			for _, alias := range []string{"parametersJsonSchema", "parameters_json_schema"} {
				if gjson.GetBytes(got, basePath+"."+alias).Exists() {
					t.Fatalf("%s should be removed; output=%s", alias, got)
				}
			}
		})
	}
}

func TestNormalizeAntigravitySchemasConsolidatesResponseAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantSource string
	}{
		{
			name: "canonical and two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"response":{"type":"object","properties":{"canonical":{"type":"string"}}},
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "canonical",
		},
		{
			name: "two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "camel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeAntigravitySchemas([]byte(tt.input), antigravitySchemaTestModel)
			basePath := "tools.0.functionDeclarations.0"
			if !gjson.GetBytes(got, basePath+".response").IsObject() {
				t.Fatalf("response missing: %s", got)
			}
			if gotProp := gjson.GetBytes(got, basePath+".response.properties."+tt.wantSource+".type").String(); gotProp != "string" {
				t.Fatalf("response.properties.%s.type = %q, want string; output=%s", tt.wantSource, gotProp, got)
			}
			for _, alias := range []string{"responseJsonSchema", "response_json_schema"} {
				if gjson.GetBytes(got, basePath+"."+alias).Exists() {
					t.Fatalf("%s should be removed; output=%s", alias, got)
				}
			}
		})
	}
}

func TestNormalizeAntigravitySchemasConsolidatesGenerationConfigSchemaAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantSource string
	}{
		{
			name: "canonical and two aliases coexist",
			input: `{"generationConfig":{
				"responseSchema":{"type":"object","properties":{"canonical":{"type":"string"}}},
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}}`,
			wantSource: "canonical",
		},
		{
			name: "two aliases coexist",
			input: `{"generationConfig":{
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}}`,
			wantSource: "camel",
		},
		{
			name: "generation_config alias carries schema aliases",
			input: `{"generation_config":{
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}}`,
			wantSource: "camel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeAntigravitySchemas([]byte(tt.input), antigravitySchemaTestModel)
			if !gjson.GetBytes(got, "generationConfig.responseSchema").IsObject() {
				t.Fatalf("generationConfig.responseSchema missing: %s", got)
			}
			if gotProp := gjson.GetBytes(got, "generationConfig.responseSchema.properties."+tt.wantSource+".type").String(); gotProp != "string" {
				t.Fatalf("generationConfig.responseSchema.properties.%s.type = %q, want string; output=%s", tt.wantSource, gotProp, got)
			}
			for _, alias := range []string{"responseJsonSchema", "response_schema", "response_json_schema"} {
				if gjson.GetBytes(got, "generationConfig."+alias).Exists() {
					t.Fatalf("generationConfig.%s should be removed; output=%s", alias, got)
				}
			}
			if gjson.GetBytes(got, "generation_config").Exists() {
				t.Fatalf("generation_config parent should be removed; output=%s", got)
			}
		})
	}
}

func TestNormalizeAntigravitySchemasFoldsGenerationConfigMixedFields(t *testing.T) {
	t.Parallel()

	input := `{"generation_config":{
		"temperature":0.2,
		"response_json_schema":{"type":"object","properties":{"field":{"type":"string"}}}
	}}`
	got := normalizeAntigravitySchemas([]byte(input), antigravitySchemaTestModel)

	if gjson.GetBytes(got, "generation_config").Exists() {
		t.Fatalf("generation_config parent should be removed; output=%s", got)
	}
	if !gjson.GetBytes(got, "generationConfig").IsObject() {
		t.Fatalf("generationConfig missing: %s", got)
	}
	if gotTemp := gjson.GetBytes(got, "generationConfig.temperature").Float(); gotTemp != 0.2 {
		t.Fatalf("generationConfig.temperature = %v, want 0.2; output=%s", gotTemp, got)
	}
	if !gjson.GetBytes(got, "generationConfig.responseSchema").IsObject() {
		t.Fatalf("generationConfig.responseSchema missing: %s", got)
	}
	if gotProp := gjson.GetBytes(got, "generationConfig.responseSchema.properties.field.type").String(); gotProp != "string" {
		t.Fatalf("generationConfig.responseSchema.properties.field.type = %q, want string; output=%s", gotProp, got)
	}
	for _, alias := range []string{"responseJsonSchema", "response_schema", "response_json_schema"} {
		if gjson.GetBytes(got, "generationConfig."+alias).Exists() {
			t.Fatalf("generationConfig.%s should be removed; output=%s", alias, got)
		}
	}
}

// Gemini 的 promptTokenCount 与 totalTokenCount 都**含**缓存命中部分，而 Anthropic 的
// input_tokens 语义是**未命中缓存**的输入，cache_read 与它相加才是总输入。两边口径不同，
// 转换时必须减掉 cached，否则缓存部分被按全价重复计费、缓存命中率也被腰斩。
//
// 取值来自一次真实的 Antigravity 响应：prompt 62277 / cached 62181 / candidates 248，
// 正确的 input_tokens 是 96。上游 CLIProxyAPI 只在流式路径做了这个减法，
// 偏差登记见 protocol/cliproxy/UPSTREAM.md。
const (
	antigravityUsagePromptTokens = 62277
	antigravityUsageCachedTokens = 62181
	antigravityUsageInputTokens  = antigravityUsagePromptTokens - antigravityUsageCachedTokens

	// candidatesTokenCount 缺失，强制走 totalTokenCount 回退分支。
	antigravityUsageNoCandidates = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":62277,"totalTokenCount":62525,"cachedContentTokenCount":62181},"modelVersion":"claude-sonnet-4-6","responseId":"msg_vrtx_1"}}`
)

func TestTranslateAntigravityResponseNonStreamSubtractsCachedTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantOutput int64
	}{
		{
			name:       "candidatesTokenCount present",
			raw:        `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":62277,"candidatesTokenCount":248,"totalTokenCount":62525,"cachedContentTokenCount":62181},"modelVersion":"claude-sonnet-4-6","responseId":"msg_vrtx_1"}}`,
			wantOutput: 248,
		},
		{
			// 回退时 prompt 必须按含 cached 的原值参与减法，否则缓存命中的
			// 输入会被整段算成输出、按输出价计费。
			name:       "candidatesTokenCount missing falls back to totalTokenCount",
			raw:        antigravityUsageNoCandidates,
			wantOutput: 248,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := translateAntigravityResponseNonStream(
				t.Context(), protocol.Anthropic, "claude-sonnet-4-6",
				nil, []byte(`{"model":"claude-sonnet-4-6"}`), []byte(testCase.raw),
			)
			if err != nil {
				t.Fatal(err)
			}
			assertAntigravityClaudeUsage(t, gjson.GetBytes(got, "usage"), testCase.wantOutput)
		})
	}
}

// 流式终止事件必须给出与非流式一致的 usage 口径。
func TestTranslateAntigravityResponseStreamUsageMatchesNonStream(t *testing.T) {
	t.Parallel()

	state := any(nil)
	chunks, err := translateAntigravityResponseStream(
		t.Context(), protocol.Anthropic, "claude-sonnet-4-6",
		nil, []byte(`{"model":"claude-sonnet-4-6"}`), []byte(antigravityUsageNoCandidates), &state,
	)
	if err != nil {
		t.Fatal(err)
	}

	stream := string(bytes.Join(chunks, nil))
	delta := ""
	for _, line := range strings.Split(stream, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if ok && gjson.Get(payload, "type").String() == "message_delta" {
			delta = payload
		}
	}
	if delta == "" {
		t.Fatalf("未找到 message_delta 事件:\n%s", stream)
	}
	assertAntigravityClaudeUsage(t, gjson.Get(delta, "usage"), 248)
}

func assertAntigravityClaudeUsage(t *testing.T, usage gjson.Result, wantOutput int64) {
	t.Helper()
	if got := usage.Get("input_tokens").Int(); got != antigravityUsageInputTokens {
		t.Errorf("input_tokens = %d, want %d (promptTokenCount 未减去 cachedContentTokenCount)",
			got, antigravityUsageInputTokens)
	}
	if got := usage.Get("cache_read_input_tokens").Int(); got != antigravityUsageCachedTokens {
		t.Errorf("cache_read_input_tokens = %d, want %d", got, antigravityUsageCachedTokens)
	}
	if got := usage.Get("output_tokens").Int(); got != wantOutput {
		t.Errorf("output_tokens = %d, want %d (回退计算误把缓存输入算成输出)", got, wantOutput)
	}
}

func TestConvertAntigravityResponseToClaude_ImplicitOpenToolContinuationOrdering(t *testing.T) {
	origRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"echo","input_schema":{"type":"object","properties":{}}}]}`)
	translatedRequest := []byte(`{"model":"gemini-3.8-flash-high"}`)

	for _, tc := range []struct {
		name      string
		firstArgs string
		contArgs  string
		want      string
	}{
		{"empty_then_real_args", `{}`, `{"user":"v"}`, `{"user":"v"}`},
		{"placeholder_then_real_args", `{"reason":"thinking"}`, `{"user":"v"}`, `{"user":"v"}`},
		{"real_args_then_empty", `{"user":"v"}`, `{}`, `{"user":"v"}`},
		{"real_args_then_placeholder", `{"user":"v"}`, `{"reason":"done"}`, `{"user":"v"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":` + tc.firstArgs + `}}]}}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-1"}}`)
			continuationChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":` + tc.contArgs + `}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-1"}}`)

			var param any
			streamBytes := bytes.Join(antigravityclaude.ConvertAntigravityResponseToClaude(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, firstChunk, &param), nil)
			streamBytes = append(streamBytes, bytes.Join(antigravityclaude.ConvertAntigravityResponseToClaude(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, continuationChunk, &param), nil)...)
			streamText := string(streamBytes)
			var deltaPartial strings.Builder
			for _, line := range strings.Split(streamText, "\n") {
				if strings.HasPrefix(line, "data: ") {
					payload := strings.TrimPrefix(line, "data: ")
					if gjson.Get(payload, "delta.type").String() == "input_json_delta" {
						deltaPartial.WriteString(gjson.Get(payload, "delta.partial_json").String())
					}
				}
			}
			if got := deltaPartial.String(); got != tc.want {
				t.Fatalf("expected concatenated partial_json %s, got %q (full output: %s)", tc.want, got, streamText)
			}
		})
	}
}
