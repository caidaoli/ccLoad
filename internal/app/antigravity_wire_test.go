package app

import (
	"bytes"
	"strings"
	"testing"

	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

const antigravitySchemaTestModel = "gemini-3-pro"

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
