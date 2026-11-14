package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	antigravityclaude "ccLoad/internal/protocol/cliproxy/providers/antigravity/claude"

	"github.com/tidwall/gjson"
)

const antigravitySchemaTestModel = "gemini-3-pro"

/**
 * @brief 验证只有完整 TOS 违规签名会持久停用渠道，HTTP 与流内错误均生效且不暂停其他账号。
 * @param t 测试上下文。
 */
func TestProxyAntigravityTOSViolation(t *testing.T) {
	t.Parallel()
	tosBody := []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Account suspended for TOS_VIOLATION","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION","metadata":{"appeal_url":"https://example.com/appeal?id=private","appeal_url_link_text":"Appeal"}}]}}`)
	textFrame := `data: {"response":{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}],"modelVersion":"gemini-3-flash"}}` + "\n\n"
	completeFrame := `data: {"response":{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"complete"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash"}}` + "\n\n"
	for _, tc := range []struct {
		name         string
		body         []byte
		streamPrefix string
		stream       bool
		disabled     bool
	}{
		{name: "http", body: tosBody, disabled: true},
		{name: "first SSE error", body: tosBody, stream: true, disabled: true},
		{name: "midstream SSE error", body: tosBody, stream: true, streamPrefix: textFrame, disabled: true},
		{name: "error after completed stream", body: tosBody, stream: true, streamPrefix: completeFrame},
		{name: "ordinary permission error", body: setJSONValue(tosBody, "error.details.0.reason", "ACCESS_DENIED")},
		{name: "wrong RPC detail type", body: setJSONValue(tosBody, "error.details.0.@type", "type.googleapis.com/google.rpc.BadRequest")},
		{name: "wrong RPC status", body: setJSONValue(tosBody, "error.status", "UNAVAILABLE")},
		{name: "wrong RPC code", body: setJSONValue(tosBody, "error.code", 429)},
		{name: "string RPC code", body: setJSONValue(tosBody, "error.code", "403")},
		{name: "fractional RPC code", body: setJSONValue(tosBody, "error.code", 403.5)},
		{name: "invalid JSON", body: append(append([]byte(nil), tosBody...), []byte("trailing")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var primaryCalls atomic.Int32
			primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				primaryCalls.Add(1)
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tc.streamPrefix+"data: "+string(tc.body)+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write(tc.body)
			}))
			var fallbackCalls atomic.Int32
			fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackCalls.Add(1)
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, strings.ReplaceAll(completeFrame, "complete", "fallback"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"fallback"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash"}}`)
			}))
			env := setupProxyTestEnv(t, []testChannel{
				{name: "tos-primary", upstreamProtocol: "gemini", models: "gemini-3-flash", priority: 100, authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "tos-at")},
				{name: "tos-fallback", upstreamProtocol: "gemini", models: "gemini-3-flash", priority: 10, authType: model.AuthTypeAntigravityOAuth, oauthCredential: string(setJSONValue([]byte(antigravityProxyTestCredential(t, "fallback-at")), "refresh_token", "fallback-rt"))},
			}, map[int]string{0: primary.URL, 1: fallback.URL})
			ctx := context.Background()
			cached, err := env.server.GetEnabledChannelsByModel(ctx, "gemini-3-flash")
			if err != nil || len(cached) != 2 {
				t.Fatalf("initial channels=%d err=%v", len(cached), err)
			}
			request := map[string]any{"model": "gemini-3-flash", "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": tc.stream}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
			}
			configs, err := env.store.ListConfigs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, cfg := range configs {
				if cfg.Name == "tos-primary" {
					if cfg.Enabled == tc.disabled {
						t.Fatalf("primary enabled=%v, want %v", cfg.Enabled, !tc.disabled)
					}
					if tc.disabled && (cfg.CooldownUntil != 0 || cfg.CooldownDurationMs != 0) {
						t.Fatalf("disabled channel retains cooldown: %+v", cfg)
					}
				} else if !cfg.Enabled {
					t.Fatal("TOS violation disabled another account")
				}
			}
			if !tc.disabled {
				return
			}
			cached, err = env.server.GetEnabledChannelsByModel(ctx, "gemini-3-flash")
			if err != nil || len(cached) != 1 || cached[0].Name != "tos-fallback" {
				t.Fatalf("enabled channels after TOS=%v err=%v", cached, err)
			}
			response = doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "fallback") || fallbackCalls.Load() == 0 {
				t.Fatalf("other account unavailable: status=%d body=%s", response.Code, response.Body.String())
			}
			if primaryCalls.Load() != 1 {
				t.Fatalf("disabled account requested %d times", primaryCalls.Load())
			}
		})
	}
}

/**
 * @brief 验证在途 TOS 结果跨 access token 刷新仍停用原账号，替换为另一账号后不受旧结果影响。
 * @param t 测试上下文。
 */
func TestProxyAntigravityTOSViolationCredentialReplacement(t *testing.T) {
	t.Parallel()
	for _, replaceAccount := range []bool{false, true} {
		name := "same account refresh"
		if replaceAccount {
			name = "different account"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var env *proxyTestEnv
			var channelID int64
			original := antigravityProxyTestCredential(t, "old-at")
			renewed := string(setJSONValue([]byte(original), "access_token", "new-at"))
			if replaceAccount {
				renewed = string(setJSONValue([]byte(renewed), "refresh_token", "new-account-rt"))
			}
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				updated, err := env.store.CompareAndSwapOAuthCredential(context.Background(), channelID, model.AuthTypeAntigravityOAuth, original, renewed)
				if err != nil || !updated {
					t.Errorf("concurrent credential update=%v err=%v", updated, err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`)
			}))
			env = setupProxyTestEnv(t, []testChannel{{name: "tos-race", upstreamProtocol: "gemini", models: "gemini-3-flash", authType: model.AuthTypeAntigravityOAuth, oauthCredential: original}}, map[int]string{0: upstream.URL})
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil || len(configs) != 1 {
				t.Fatalf("configs=%d err=%v", len(configs), err)
			}
			channelID = configs[0].ID
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "gemini-3-flash", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
			if response.Code != http.StatusForbidden {
				t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
			}
			current, err := env.store.GetConfig(context.Background(), channelID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Enabled != replaceAccount || current.OAuthCredential != renewed || current.CooldownUntil != 0 {
				t.Fatalf("concurrent credential result: enabled=%v credential=%s cooldown=%d", current.Enabled, current.OAuthCredential, current.CooldownUntil)
			}
		})
	}
}

/**
 * @brief 验证最终发送的 Antigravity 请求按模型能力保留 xhigh，并兼容旧等级映射。
 * @param t 测试上下文。
 */
func TestAntigravityGeminiWireThinkingLevels(t *testing.T) {
	cfg := &model.Config{AuthType: model.AuthTypeAntigravityOAuth, AntigravityProjectID: "test-project"}
	for _, tc := range []struct {
		model string
		level string
		want  string
	}{
		{"gemini-4-hydrogen-xhigh", " XHIGH ", "xhigh"},
		{"gemini-3.8-flash-tiered", "xhigh", "high"},
		{"gemini-3-pro", "xhigh", "high"},
		{"gemini-unlisted", "xhigh", "high"},
		{"gemini-4-hydrogen-xhigh", "max", "high"},
		{"gemini-4-hydrogen-xhigh", "minimal", "low"},
	} {
		t.Run(tc.model+"/"+tc.level, func(t *testing.T) {
			for _, path := range []string{"generationConfig.thinkingConfig.thinkingLevel", "generation_config.thinking_config.thinking_level"} {
				body := setJSONValue([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`), path, tc.level)
				got, err := prepareAntigravityRequestBody(cfg, tc.model, body, body, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				outputPath := "request." + strings.Replace(path, "generation_config", "generationConfig", 1)
				if level := gjson.GetBytes(got, outputPath).String(); level != tc.want {
					t.Fatalf("%s=%q, want %q; body=%s", path, level, tc.want, got)
				}
			}
		})
	}
}

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
	for line := range strings.SplitSeq(stream, "\n") {
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
			for line := range strings.SplitSeq(streamText, "\n") {
				if after, ok := strings.CutPrefix(line, "data: "); ok {
					payload := after
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
