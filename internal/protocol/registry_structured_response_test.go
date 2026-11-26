package protocol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"

	"github.com/tidwall/gjson"
)

/**
 * @brief 验证 Gemini 结构化输出在各协议保留 schema 约束，并拒绝无法表示的根类型。
 * @param t 测试上下文。
 */
func TestRegistryGeminiStructuredOutput(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	const native = `{"type":"OBJECT","properties":{"properties":{"type":"STRING","minLength":"1"}},"required":["properties"]}`
	const normalized = `{"type":"object","properties":{"properties":{"type":"string","minLength":1}},"required":["properties"]}`
	const nullable = `{"type":"OBJECT","properties":{"nullable":{"type":"STRING","enum":["default"],"nullable":true},"samples":{"type":"ARRAY","items":{"type":"INTEGER","nullable":true}}}}`
	const normalizedNullable = `{"type":"object","properties":{"nullable":{"anyOf":[{"type":"string","enum":["default"]},{"type":"null"}]},"samples":{"type":"array","items":{"anyOf":[{"type":"integer"},{"type":"null"}]}}}}`
	const jsonSchema = `{"$ref":"#/$defs/Value","$defs":{"Value":{"type":"object","properties":{"type":{"type":"string","const":"OBJECT"},"$ref":{"type":"string"}}}}}`
	const closedObject = `{"type":"object","additionalProperties":false,"properties":{"settings":{"type":"object","additionalProperties":false,"properties":{"minimum":{"type":"number"},"additionalProperties":{"type":"boolean"},"$ref":{"type":"string"}}},"nullable":{"anyOf":[{"type":"string"},{"type":"null"}]}}}`
	const nestedOpenObject = `{"type":"object","additionalProperties":false,"properties":{"settings":{"type":"object","properties":{"answer":{"type":"string"}}}}}`
	const numericConstraint = `{"type":"object","additionalProperties":false,"properties":{"score":{"type":"number","minimum":1}}}`
	const openDictionary = `{"type":"object","additionalProperties":{"type":"string"}}`
	for _, target := range []struct {
		protocol protocol.Protocol
		model    string
		format   string
		schema   string
	}{
		{protocol.OpenAI, "gpt-5.4", "response_format", "response_format.json_schema.schema"},
		{protocol.Codex, "gpt-5.4", "text.format", "text.format.schema"},
		{protocol.Anthropic, "claude-sonnet-4-6", "output_config.format", "output_config.format.schema"},
	} {
		for _, tc := range []struct {
			name, generation, want string
			claudeNative           bool
		}{
			{"camel protobuf", `"generationConfig":{"responseMimeType":"application/json","responseSchema":` + native + `}`, normalized, false},
			{"snake protobuf", `"generation_config":{"response_mime_type":"application/json","response_schema":` + native + `}`, normalized, false},
			{"protobuf nested nullable", `"generationConfig":{"responseSchema":` + nullable + `}`, normalizedNullable, false},
			{"camel JSON Schema", `"generationConfig":{"responseJsonSchema":` + jsonSchema + `}`, jsonSchema, false},
			{"snake JSON Schema", `"generation_config":{"response_json_schema":` + jsonSchema + `}`, jsonSchema, false},
			{"invalid aliases before JSON Schema", `"generationConfig":{"responseJsonSchema":null,"responseSchema":"invalid"},"generation_config":{"response_json_schema":` + jsonSchema + `}`, jsonSchema, false},
			{"JSON Schema wins over protobuf", `"generationConfig":{"responseJsonSchema":` + jsonSchema + `,"responseSchema":` + native + `}`, jsonSchema, false},
			{"empty object is a schema", `"generationConfig":{"responseJsonSchema":{},"responseSchema":` + native + `}`, `{}`, false},
			{"closed objects and nullable field", `"generationConfig":{"responseJsonSchema":` + closedObject + `}`, closedObject, true},
			{"nested open object", `"generationConfig":{"responseJsonSchema":` + nestedOpenObject + `}`, nestedOpenObject, false},
			{"closed object with numeric constraint", `"generationConfig":{"responseJsonSchema":` + numericConstraint + `}`, numericConstraint, false},
			{"open dictionary", `"generationConfig":{"responseJsonSchema":` + openDictionary + `}`, openDictionary, false},
		} {
			t.Run(string(target.protocol)+"/"+tc.name, func(t *testing.T) {
				in := []byte(`{"contents":[{"parts":[{"text":"question"}]}],` + tc.generation + `}`)
				out, err := reg.TranslateRequest(protocol.Gemini, target.protocol, target.model, in, false)
				if err != nil {
					t.Fatal(err)
				}
				body := gjson.ParseBytes(out)
				if target.protocol == protocol.Anthropic && !tc.claudeNative {
					assertClaudeSchemaInstruction(t, body, tc.want)
					return
				}
				if body.Get(target.format+".type").Str != "json_schema" ||
					!reflect.DeepEqual(body.Get(target.schema).Value(), gjson.Parse(tc.want).Value()) {
					t.Fatalf("structured output missing or modified: %s", out)
				}
			})
		}
		invalid := []string{`"responseJsonSchema":"not a schema"`, `"responseJsonSchema":false`}
		if target.protocol != protocol.Anthropic {
			invalid = append(invalid,
				`"responseJsonSchema":true`,
				`"responseJsonSchema":{"type":"array"}`,
				`"responseSchema":{"type":"OBJECT","nullable":true}`,
			)
		}
		for _, bad := range invalid {
			t.Run(string(target.protocol)+"/unrepresentable/"+bad, func(t *testing.T) {
				in := []byte(`{"generationConfig":{"responseMimeType":"application/json",` + bad + `}}`)
				out, err := reg.TranslateRequest(protocol.Gemini, target.protocol, target.model, in, false)
				var translationErr *protocol.RequestTranslationError
				if !errors.As(err, &translationErr) || out != nil {
					t.Fatalf("expected request translation error, got %s, %v", out, err)
				}
			})
		}
	}
}

/**
 * @brief 验证 Gemini 枚举输出以纯文本提示保留可选值，并拒绝无效枚举。
 * @param t 测试上下文。
 */
func TestRegistryGeminiEnumOutput(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	const values = `["Woodwind","String, bowed","a\"b",""]`
	const contents = `"contents":[{"role":"user","parts":[{"text":"Classify this instrument"}]}]`
	for _, target := range []struct {
		protocol                   protocol.Protocol
		model, instruction, format string
	}{
		{protocol.OpenAI, "gpt-5.4", `messages.#(role=="system").content`, "response_format"},
		{protocol.Codex, "gpt-5.4", "instructions", "text.format"},
		{protocol.Anthropic, "claude-sonnet-4-6", "system.0.text", "output_config.format"},
	} {
		t.Run(string(target.protocol), func(t *testing.T) {
			for _, generation := range []string{
				`"generationConfig":{"responseMimeType":"text/x.enum","responseSchema":{"type":"STRING","enum":` + values + `}}`,
				`"generation_config":{"response_mime_type":"text/x.enum","response_schema":{"type":1,"enum":` + values + `}}`,
				`"generationConfig":{"responseMimeType":"text/x.enum","responseJsonSchema":{"type":"string","enum":` + values + `}}`,
			} {
				in := []byte(`{` + contents + `,` + generation + `}`)
				out, err := reg.TranslateRequest(protocol.Gemini, target.protocol, target.model, in, false)
				if err != nil {
					t.Fatal(err)
				}
				result := gjson.ParseBytes(out)
				instruction := result.Get(target.instruction).Str
				directive, choices, found := strings.Cut(instruction, ": ")
				var labels []string
				if result.Get(target.format).Exists() || !found ||
					!strings.Contains(directive, "plain text, without adding quotation marks") ||
					json.Unmarshal([]byte(choices), &labels) != nil ||
					!reflect.DeepEqual(labels, []string{"Woodwind", "String, bowed", `a"b`, ""}) {
					t.Fatalf("expected plain-text enum instruction and no JSON format: %s", out)
				}
			}
			in := []byte(`{` + contents + `,"generationConfig":{"responseMimeType":"text/x.enum"}}`)
			out, err := reg.TranslateRequest(protocol.Gemini, target.protocol, target.model, in, false)
			result := gjson.ParseBytes(out)
			if err != nil || result.Get(target.format).Exists() || result.Get(target.instruction).Str != "" {
				t.Fatalf("expected enum MIME without schema to preserve plain text: %s, %v", out, err)
			}
			for _, schema := range []string{
				`"not a schema"`,
				`{"type":"INTEGER","enum":["1"]}`,
				`{"type":"STRING","enum":[]}`,
				`{"type":"STRING","enum":["Woodwind",1]}`,
			} {
				in := []byte(`{"generationConfig":{"responseMimeType":"text/x.enum","responseSchema":` + schema + `}}`)
				out, err := reg.TranslateRequest(protocol.Gemini, target.protocol, target.model, in, false)
				var translationErr *protocol.RequestTranslationError
				if !errors.As(err, &translationErr) || out != nil {
					t.Fatalf("expected enum translation error for %s, got %s, %v", schema, out, err)
				}
			}
		})
	}
}

func TestRegistryGeminiStructuredOutputClaudeRootShapes(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, modelName := range []string{"claude-sonnet-4-6", "claude-3-7-sonnet-20250219"} {
		for _, tc := range []struct {
			name, field, want string
			native            bool
		}{
			{"array", `"responseSchema":{"type":"ARRAY","items":{"type":"STRING"}}`, `{"type":"array","items":{"type":"string"}}`, true},
			{"scalar", `"responseJsonSchema":{"type":"string","const":"answer"}`, `{"type":"string","const":"answer"}`, true},
			{"nullable object", `"responseSchema":{"type":"OBJECT","nullable":true,"additionalProperties":false}`, `{"anyOf":[{"type":"object","additionalProperties":false},{"type":"null"}]}`, true},
			{"array with unsupported constraint", `"responseSchema":{"type":"ARRAY","items":{"type":"STRING"},"maxItems":2}`, `{"type":"array","items":{"type":"string"},"maxItems":2}`, false},
			{"allow-all Boolean schema", `"responseJsonSchema":true`, `true`, false},
		} {
			t.Run(modelName+"/"+tc.name, func(t *testing.T) {
				in := []byte(`{"generationConfig":{"responseMimeType":"application/json",` + tc.field + `},"contents":[{"parts":[{"text":"question"}]}]}`)
				out, err := reg.TranslateRequest(protocol.Gemini, protocol.Anthropic, modelName, in, false)
				if err != nil {
					t.Fatal(err)
				}
				body := gjson.ParseBytes(out)
				if tc.native && modelName == "claude-sonnet-4-6" {
					format := body.Get("output_config.format")
					if format.Get("type").Str != "json_schema" || !reflect.DeepEqual(format.Get("schema").Value(), gjson.Parse(tc.want).Value()) {
						t.Fatalf("Claude native schema = %s, want %s", format.Raw, tc.want)
					}
					return
				}
				assertClaudeSchemaInstruction(t, body, tc.want)
				if !strings.Contains(body.Get("system.0.text").Str, "outside of the JSON value") {
					t.Fatalf("the prompt should permit any root described by the schema: %s", out)
				}
			})
		}
	}
}

func assertClaudeSchemaInstruction(t *testing.T, body gjson.Result, want string) {
	t.Helper()
	prompt := body.Get("system.0.text").Str
	_, schema, found := strings.Cut(prompt, "JSON Schema:\n")
	if body.Get("output_config.format").Exists() || !found {
		t.Fatalf("expected Claude schema instruction, got %s", body.Raw)
	}
	var got any
	if err := json.NewDecoder(strings.NewReader(schema)).Decode(&got); err != nil || !reflect.DeepEqual(got, gjson.Parse(want).Value()) {
		t.Fatalf("prompt schema = %v, error = %v, want %s", got, err, want)
	}
}

/**
 * @brief 验证 Gemini JSON MIME-only 模式和 Claude 旧模型的提示词兜底。
 * @param t 测试上下文。
 */
func TestRegistryGeminiJSONMIMEAndClaudeFallback(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, tc := range []struct {
		protocol protocol.Protocol
		model    string
		format   string
		prompt   string
	}{
		{protocol.OpenAI, "gpt-5.4", "response_format.type", "messages.1.content"},
		{protocol.Codex, "gpt-5.4", "text.format.type", "instructions"},
		{protocol.Anthropic, "claude-sonnet-4-6", "", "system.0.text"},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			in := []byte(`{"generation_config":{"response_mime_type":"application/json"},"system_instruction":{"parts":[{"text":"original instruction"}]},"contents":[{"parts":[{"text":"question"}]}]}`)
			out, err := reg.TranslateRequest(protocol.Gemini, tc.protocol, tc.model, in, false)
			if err != nil {
				t.Fatal(err)
			}
			root := gjson.ParseBytes(out)
			if tc.format != "" && root.Get(tc.format).Str != "json_object" {
				t.Fatalf("JSON object mode missing: %s", out)
			}
			if !strings.Contains(root.Get(tc.prompt).Str, "JSON object") || !bytes.Contains(out, []byte("original instruction")) {
				t.Fatalf("JSON instruction or caller's system instruction missing: %s", out)
			}
		})
	}
	for _, model := range []string{"claude-3-7-sonnet-20250219", "glm-5"} {
		t.Run(model, func(t *testing.T) {
			in := []byte(`{"generationConfig":{"responseJsonSchema":{"type":"object","properties":{"answer":{"type":"string"}}}},"contents":[{"parts":[{"text":"question"}]}]}`)
			out, err := reg.TranslateRequest(protocol.Gemini, protocol.Anthropic, model, in, false)
			if err != nil {
				t.Fatal(err)
			}
			root := gjson.ParseBytes(out)
			if root.Get("output_config.format").Exists() || !strings.Contains(root.Get("system.0.text").Str, `"answer":{"type":"string"}`) {
				t.Fatalf("schema instruction fallback missing: %s", out)
			}
		})
	}
}

/**
 * @brief 验证 Gemini 请求的空占位字段不会遮蔽有效的系统指令、工具历史或工具开关。
 * @param t 测试上下文。
 */
func TestRegistryGeminiRequestAliasFallback(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, target := range []struct {
		protocol protocol.Protocol
		model    string
		system   string
		callID   string
		callName string
		resultID string
		result   string
		choice   string
	}{
		{protocol.OpenAI, "gpt-5.4", "messages.0.content.0.text", "messages.1.tool_calls.0.id", "messages.1.tool_calls.0.function.name", "messages.2.tool_call_id", "messages.2.content", "tool_choice"},
		{protocol.Codex, "gpt-5.4", "instructions", "input.0.call_id", "input.0.name", "input.1.call_id", "input.1.output", "tool_choice"},
		{protocol.Anthropic, "claude-sonnet-4-6", "messages.0.content.0.text", "messages.1.content.0.id", "messages.1.content.0.name", "messages.2.content.0.tool_use_id", "messages.2.content.0.content", "tool_choice.type"},
	} {
		for _, placeholder := range []string{`null`, `{}`, `{"parts":null}`, `{"parts":[]}`, `"wrong type"`} {
			t.Run(string(target.protocol)+"/"+placeholder, func(t *testing.T) {
				fallback, preferred := "system_instruction", "systemInstruction"
				if target.protocol == protocol.Codex {
					fallback, preferred = preferred, fallback
				}
				toolPlaceholder := placeholder
				if placeholder == `{"parts":null}` || placeholder == `{"parts":[]}` {
					toolPlaceholder = "{}"
				}
				in := fmt.Appendf(nil, `{
					%q:%s,%q:{"parts":[{"text":"alternate instruction"}]},
					"contents":[{"role":"model","parts":[{"functionCall":%s,"function_call":{"name":"lookup","id":"call_lookup","args":{"q":"s"}}}]},
						{"role":"user","parts":[{"functionResponse":%s,"function_response":{"name":"lookup","id":"call_lookup","response":{"result":"tool result"}}}]}],
					"tools":[{"function_declarations":[{"name":"lookup"}]}],
					"toolConfig":{"functionCallingConfig":{"mode":null}},
					"tool_config":{"function_calling_config":{"mode":"NONE"}}
				}`, preferred, placeholder, fallback, toolPlaceholder, toolPlaceholder)
				out, err := reg.TranslateRequest(protocol.Gemini, target.protocol, target.model, in, false)
				if err != nil {
					t.Fatal(err)
				}
				root := gjson.ParseBytes(out)
				for path, want := range map[string]string{
					target.system: "alternate instruction", target.callID: "call_lookup", target.callName: "lookup",
					target.resultID: "call_lookup", target.choice: "none",
				} {
					if got := root.Get(path).Str; got != want {
						t.Fatalf("%s = %q, want %q: %s", path, got, want, out)
					}
				}
				if !strings.Contains(root.Get(target.result).Str, "tool result") {
					t.Fatalf("tool result missing: %s", out)
				}
			})
		}
	}
}

/**
 * @brief Chat 内容数组通过 Responses 边界保留思考、正文与截断状态
 * @param t 测试上下文
 */
func TestRegistryOpenAIToResponsesTypedContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	parts := `[{"type":"thinking","thinking":[{"type":"text","text":"consider"}]},{"type":"text","text":"answer"}]`
	raw := []byte(`{"id":"chatcmpl_parts","choices":[{"index":0,"message":{"role":"assistant","content":` + parts + `},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`)
	out, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Codex, "model", nil, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	response := mustJSONMap(t, out)
	output := mustSlice(t, response["output"])
	if len(output) != 2 || response["status"] != "incomplete" {
		t.Fatalf("unexpected output or status: %s", out)
	}
	reasoning, message := mustMap(t, output[0]), mustMap(t, output[1])
	if reasoning["type"] != "reasoning" || mustMap(t, mustSlice(t, reasoning["summary"])[0])["text"] != "consider" ||
		message["type"] != "message" || mustMap(t, mustSlice(t, message["content"])[0])["text"] != "answer" {
		t.Fatalf("typed content lost its meaning: %s", out)
	}
	stream := translateResponseStreamChunks(t, reg, protocol.OpenAI, protocol.Codex, "model",
		`data: {"id":"chatcmpl_parts","choices":[{"index":0,"delta":{"content":`+parts+`},"finish_reason":"length"}]}`+"\n\n",
		"data: [DONE]\n\n")
	var text, thought strings.Builder
	var incomplete bool
	for _, event := range parseSSEEvents(t, stream) {
		switch event.Event {
		case "response.output_text.delta":
			text.WriteString(mustString(t, event.Data["delta"]))
		case "response.reasoning_summary_text.delta":
			thought.WriteString(mustString(t, event.Data["delta"]))
		case "response.incomplete":
			incomplete = mustMap(t, event.Data["response"])["status"] == "incomplete"
		case "response.completed":
			t.Fatalf("truncated response reported completion: %s", stream)
		}
	}
	if text.String() != "answer" || thought.String() != "consider" || !incomplete {
		t.Fatalf("typed stream lost content or termination: %s", stream)
	}
}

/**
 * @brief 同帧思考的顶层表示优先于内容数组，JSON、SSE 增量及终态保留相同思考和正文。
 * @param t 测试上下文。
 */
func TestRegistryOpenAIToResponsesReasoningRepresentationPriority(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, tc := range []struct {
		name   string
		fields string
	}{
		{"reasoning content", `"reasoning_content":"thought"`},
		{"structured reasoning", `"reasoning":{"text":"thought"}`},
		{"reasoning details", `"reasoning_details":[{"text":"thought"}]`},
		{"alias priority", `"reasoning_content":"thought","reasoning":"ignored","reasoning_details":[{"text":"ignored"}]`},
		{"empty aliases use typed thinking", `"reasoning_content":"","reasoning":{"text":""},"reasoning_details":[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := `{"role":"assistant",` + tc.fields + `,"content":[{"type":"thinking","thinking":"thought"},{"type":"text","text":"answer"}]}`
			raw := []byte(`{"id":"priority","choices":[{"index":0,"message":` + message + `,"finish_reason":"stop"}]}`)
			out, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Codex, "model", nil, nil, raw)
			if err != nil {
				t.Fatal(err)
			}
			stream := translateResponseStreamChunks(t, reg, protocol.OpenAI, protocol.Codex, "model",
				`data: {"id":"priority","choices":[{"index":0,"delta":`+message+`,"finish_reason":"stop"}]}`+"\n\n",
				"data: [DONE]\n\n")
			var thought, text strings.Builder
			var completed map[string]any
			for _, event := range parseSSEEvents(t, stream) {
				switch event.Event {
				case "response.reasoning_summary_text.delta":
					thought.WriteString(mustString(t, event.Data["delta"]))
				case "response.output_text.delta":
					text.WriteString(mustString(t, event.Data["delta"]))
				case "response.completed":
					completed = mustMap(t, event.Data["response"])
				}
			}
			if thought.String() != "thought" || text.String() != "answer" {
				t.Fatalf("stream thought=%q text=%q, want thought/answer", thought.String(), text.String())
			}
			if completed == nil {
				t.Fatalf("stream has no completed response: %s", stream)
			}
			for name, response := range map[string]map[string]any{"JSON": mustJSONMap(t, out), "SSE": completed} {
				output := mustSlice(t, response["output"])
				if len(output) != 2 {
					t.Fatalf("%s output count=%d, want 2: %+v", name, len(output), response)
				}
				reasoning, message := mustMap(t, output[0]), mustMap(t, output[1])
				if reasoning["type"] != "reasoning" || mustMap(t, mustSlice(t, reasoning["summary"])[0])["text"] != "thought" ||
					message["type"] != "message" || mustMap(t, mustSlice(t, message["content"])[0])["text"] != "answer" {
					t.Fatalf("%s changed reasoning or text: %+v", name, response)
				}
			}
		})
	}
}

func TestRegistry_TranslateResponseNonStream_AnthropicTruncation(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, block := range []struct {
		name string
		json string
	}{
		{"text", `{"type":"text","text":"partial answer"}`},
		{"tool", `{"type":"tool_use","id":"call_1","name":"lookup","input":{"query":"go"}}`},
		{"thinking", `{"type":"thinking","thinking":"partial thought","signature":"sig_1"}`},
	} {
		t.Run(block.name, func(t *testing.T) {
			for _, stopReason := range []string{"end_turn", "max_tokens"} {
				raw := fmt.Appendf(nil, `{"id":"msg_partial","type":"message","role":"assistant","content":[{"type":"text","text":"first"},%s],"stop_reason":%q,"usage":{"input_tokens":3,"cache_read_input_tokens":7,"cache_creation_input_tokens":11,"output_tokens":5}}`, block.json, stopReason)
				request := []byte(`{"instructions":"preserve me"}`)
				got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", request, nil, raw)
				if err != nil {
					t.Fatal(err)
				}
				response := mustJSONMap(t, got)
				output := mustSlice(t, response["output"])
				if len(output) != 2 || mustMap(t, output[0])["status"] != "completed" || response["instructions"] != "preserve me" {
					t.Fatalf("lost content order or request echo: %s", got)
				}
				wantStatus := "completed"
				if stopReason == "max_tokens" {
					wantStatus = "incomplete"
					if mustMap(t, response["incomplete_details"])["reason"] != "max_output_tokens" || mustMap(t, output[1])["status"] != wantStatus {
						t.Fatalf("lost partial item state: %s", got)
					}
				}
				if response["status"] != wantStatus || mustMap(t, response["usage"])["input_tokens"] != float64(21) {
					t.Fatalf("unexpected status or cache accounting: %s", got)
				}
			}
		})
	}
}

func TestRegistry_TranslateResponseNonStream_GeminiStructuredOutbound(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"candidates":[{"content":{"parts":[{"text":"hello"},{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"modelVersion":"gemini-2.5-pro"}`)

	t.Run("openai", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.OpenAI, "gpt-4o", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		payload := mustJSONMap(t, got)
		choice := mustMap(t, mustSlice(t, payload["choices"])[0])
		message := mustMap(t, choice["message"])
		toolCall := mustMap(t, mustSlice(t, message["tool_calls"])[0])
		function := mustMap(t, toolCall["function"])
		arguments := mustJSONMap(t, []byte(mustString(t, function["arguments"])))
		if message["content"] != "hello" || mustString(t, toolCall["id"]) == "" || toolCall["type"] != "function" || function["name"] != "lookup" || arguments["query"] != "go" || choice["finish_reason"] != "tool_calls" {
			t.Fatalf("unexpected OpenAI response semantics: %s", got)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		payload := mustJSONMap(t, got)
		content := mustSlice(t, payload["content"])
		if len(content) != 2 {
			t.Fatalf("unexpected Anthropic content: %s", got)
		}
		textBlock := mustMap(t, content[0])
		toolBlock := mustMap(t, content[1])
		input := mustMap(t, toolBlock["input"])
		if textBlock["type"] != "text" || textBlock["text"] != "hello" || toolBlock["type"] != "tool_use" || mustString(t, toolBlock["id"]) == "" || toolBlock["name"] != "lookup" || input["query"] != "go" || payload["stop_reason"] != "tool_use" {
			t.Fatalf("unexpected Anthropic response semantics: %s", got)
		}
	})

	t.Run("codex", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.Codex, "gpt-5-codex", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		payload := mustJSONMap(t, got)
		var messageItem, functionItem map[string]any
		for _, rawItem := range mustSlice(t, payload["output"]) {
			item := mustMap(t, rawItem)
			switch item["type"] {
			case "message":
				messageItem = item
			case "function_call":
				functionItem = item
			}
		}
		if messageItem == nil || functionItem == nil {
			t.Fatalf("missing Codex output items: %s", got)
		}
		content := mustMap(t, mustSlice(t, messageItem["content"])[0])
		arguments := mustJSONMap(t, []byte(mustString(t, functionItem["arguments"])))
		callID := mustString(t, functionItem["call_id"])
		if messageItem["role"] != "assistant" || content["type"] != "output_text" || content["text"] != "hello" || callID == "" || functionItem["id"] != "fc_"+callID || functionItem["name"] != "lookup" || arguments["query"] != "go" {
			t.Fatalf("unexpected Codex response semantics: %s", got)
		}
	})
}

func TestRegistry_TranslateResponseStream_GeminiStructuredOutbound(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("openai", func(t *testing.T) {
		var state any
		chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.OpenAI, "gpt-4o", nil, nil, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"go\"}}}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro\",\"usageMetadata\":{\"totalTokenCount\":8}}\n\n"), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		if len(chunks) != 2 {
			t.Fatalf("expected separate delta and terminal chunks, got %q", chunks)
		}
		payload := mustSSEEventData(t, string(chunks[0]), "")
		choice := mustMap(t, mustSlice(t, payload["choices"])[0])
		delta := mustMap(t, choice["delta"])
		toolCall := mustMap(t, mustSlice(t, delta["tool_calls"])[0])
		function := mustMap(t, toolCall["function"])
		arguments := mustJSONMap(t, []byte(mustString(t, function["arguments"])))
		if mustString(t, toolCall["id"]) == "" || mustInt(t, toolCall["index"]) != 0 || function["name"] != "lookup" || arguments["query"] != "go" || choice["finish_reason"] != nil {
			t.Fatalf("unexpected OpenAI tool delta: %s", chunks[0])
		}
		terminal := mustMap(t, mustSlice(t, mustSSEEventData(t, string(chunks[1]), "")["choices"])[0])
		if terminal["finish_reason"] != "tool_calls" || len(mustMap(t, terminal["delta"])) != 0 {
			t.Fatalf("unexpected OpenAI terminal chunk: %s", chunks[1])
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		var state any
		chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"go\"}}}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro\",\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5}}\n\n"), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		joined := string(bytes.Join(chunks, nil))
		toolStart := mustSSEEventData(t, joined, "content_block_start")
		toolBlock := mustMap(t, toolStart["content_block"])
		var toolInput strings.Builder
		for _, event := range parseSSEEvents(t, joined) {
			if event.Event != "content_block_delta" || event.Data["index"] != toolStart["index"] {
				continue
			}
			delta := mustMap(t, event.Data["delta"])
			if delta["type"] == "input_json_delta" {
				toolInput.WriteString(mustString(t, delta["partial_json"]))
			}
		}
		messageDelta := mustMap(t, mustSSEEventData(t, joined, "message_delta")["delta"])
		arguments := mustJSONMap(t, []byte(toolInput.String()))
		if mustString(t, toolBlock["id"]) == "" || toolBlock["type"] != "tool_use" || toolBlock["name"] != "lookup" || arguments["query"] != "go" || messageDelta["stop_reason"] != "tool_use" || !strings.Contains(joined, `event: message_stop`) {
			t.Fatalf("unexpected Anthropic stream chunks: %s", joined)
		}
	})

	t.Run("codex", func(t *testing.T) {
		var state any
		chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Codex, "gpt-5-codex", nil, nil, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"go\"}}}]}}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n"), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		joined := string(bytes.Join(chunks, nil))
		item := mustMap(t, mustSSEEventData(t, joined, "response.output_item.done")["item"])
		arguments := mustJSONMap(t, []byte(mustString(t, item["arguments"])))
		callID := mustString(t, item["call_id"])
		if item["type"] != "function_call" || callID == "" || item["id"] != "fc_"+callID || item["name"] != "lookup" || arguments["query"] != "go" {
			t.Fatalf("unexpected Codex stream chunk: %#v", chunks)
		}
	})
}

func TestRegistry_TranslateResponseNonStream_AnthropicStructuredOutbound(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"query":"go"}}],"model":"claude-3-5-sonnet","stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`)

	t.Run("openai", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		if !strings.Contains(string(got), `"content":"hello"`) || !strings.Contains(string(got), `"tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]`) || !strings.Contains(string(got), `"finish_reason":"tool_calls"`) {
			t.Fatalf("unexpected OpenAI response: %s", got)
		}
	})

	t.Run("codex", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		if !strings.Contains(string(got), `"type":"message"`) || !strings.Contains(string(got), `"role":"assistant"`) || !strings.Contains(string(got), `"type":"output_text"`) || !strings.Contains(string(got), `"text":"hello"`) || !strings.Contains(string(got), `"type":"function_call"`) || !strings.Contains(string(got), `"call_id":"toolu_1"`) || !strings.Contains(string(got), `"name":"lookup"`) || !strings.Contains(string(got), `"arguments":"{\"query\":\"go\"}"`) {
			t.Fatalf("unexpected Codex response: %s", got)
		}
	})
}

func TestRegistry_TranslateResponseNonStream_OpenAIStructuredOutboundToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":0,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	body := string(got)
	if !strings.Contains(body, `"functionCall"`) || !strings.Contains(body, `"name":"lookup"`) || !strings.Contains(body, `"query":"go"`) || !strings.Contains(body, `"finishReason":"STOP"`) {
		t.Fatalf("unexpected Gemini response: %s", got)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIStructuredOutboundToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}` + "\n\n",
	}

	var state any
	var outputs [][]byte
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		outputs = append(outputs, out...)
	}

	joined := string(bytes.Join(outputs, nil))
	if !strings.Contains(joined, `"functionCall"`) || !strings.Contains(joined, `"name":"lookup"`) || !strings.Contains(joined, `"query":"go"`) || !strings.Contains(joined, `"finishReason":"STOP"`) || !strings.Contains(joined, `"promptTokenCount":3`) {
		t.Fatalf("unexpected Gemini stream output: %s", joined)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIStructuredOutboundToGeminiDoneSentinel(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}` + "\n\n",
	}

	var state any
	for _, chunk := range chunks {
		if _, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(chunk), &state); err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	if done != nil {
		t.Fatalf("expected [DONE] to emit no extra Gemini payload after finish_reason, got: %#v", done)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIStructuredOutboundToGeminiUsageOnlyTail(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}` + "\n\n",
	}

	var state any
	for _, chunk := range chunks {
		if _, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(chunk), &state); err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
	}

	usageOnly, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`+"\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream usage-only tail failed: %v", err)
	}
	if usageOnly != nil {
		t.Fatalf("expected usage-only tail to emit no extra Gemini payload after terminal chunk, got: %#v", usageOnly)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIStructuredOutboundToGeminiUsageOnlyTailCarriesUsage(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
	}

	var state any
	for _, chunk := range chunks {
		if _, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(chunk), &state); err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
	}

	usageOnly, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`+"\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream usage-only tail failed: %v", err)
	}
	if len(usageOnly) != 1 || !strings.Contains(string(usageOnly[0]), `"promptTokenCount":3`) || !strings.Contains(string(usageOnly[0]), `"candidatesTokenCount":5`) || strings.Contains(string(usageOnly[0]), `"finishReason"`) {
		t.Fatalf("expected usage-only tail to emit usageMetadata without duplicate finishReason, got: %#v", usageOnly)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	if done != nil {
		t.Fatalf("expected DONE sentinel to emit nothing after usage-only tail, got: %#v", done)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIStructuredOutboundToGemini_FragmentedToolCalls_AllSplitPoints(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	arguments := `{"query":"go"}`
	for split := 0; split <= len(arguments); split++ {
		t.Run(fmt.Sprintf("split-%d", split), func(t *testing.T) {
			var state any
			var outputs [][]byte
			chunks := []string{
				`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]}}]}` + "\n\n",
				fmt.Sprintf(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`+"\n\n", arguments[:split]),
				fmt.Sprintf(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`+"\n\n", arguments[split:]),
			}
			for _, chunk := range chunks {
				out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(chunk), &state)
				if err != nil {
					t.Fatalf("split %d TranslateResponseStream failed: %v", split, err)
				}
				outputs = append(outputs, out...)
			}
			joined := string(bytes.Join(outputs, nil))
			if !strings.Contains(joined, `"functionCall"`) || !strings.Contains(joined, `"query":"go"`) || !strings.Contains(joined, `"promptTokenCount":3`) {
				t.Fatalf("split %d unexpected Gemini stream output: %s", split, joined)
			}
		})
	}
}

func TestRegistry_TranslateResponseStream_GeminiStructuredOutbound_MultipleToolCallsAcrossChunks(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	firstChunk := []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"one\"}}}]}}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n")
	secondChunk := []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"search\",\"args\":{\"query\":\"two\"}}}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n")

	t.Run("openai", func(t *testing.T) {
		var state any
		first, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.OpenAI, "gpt-4o", nil, nil, firstChunk, &state)
		if err != nil {
			t.Fatalf("first chunk failed: %v", err)
		}
		second, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.OpenAI, "gpt-4o", nil, nil, secondChunk, &state)
		if err != nil {
			t.Fatalf("second chunk failed: %v", err)
		}
		extractToolCall := func(raw [][]byte) map[string]any {
			payload := mustSSEEventData(t, string(bytes.Join(raw, nil)), "")
			choice := mustMap(t, mustSlice(t, payload["choices"])[0])
			delta := mustMap(t, choice["delta"])
			return mustMap(t, mustSlice(t, delta["tool_calls"])[0])
		}
		firstCall := extractToolCall(first)
		secondCall := extractToolCall(second)
		firstID := mustString(t, firstCall["id"])
		secondID := mustString(t, secondCall["id"])
		if mustInt(t, firstCall["index"]) != 0 || mustInt(t, secondCall["index"]) != 1 || firstID == "" || secondID == "" || firstID == secondID {
			t.Fatalf("unexpected OpenAI tool call identities: first=%s second=%s", string(bytes.Join(first, nil)), string(bytes.Join(second, nil)))
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		var state any
		first, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, firstChunk, &state)
		if err != nil {
			t.Fatalf("first chunk failed: %v", err)
		}
		second, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, secondChunk, &state)
		if err != nil {
			t.Fatalf("second chunk failed: %v", err)
		}
		firstStart := mustSSEEventData(t, string(bytes.Join(first, nil)), "content_block_start")
		secondStart := mustSSEEventData(t, string(bytes.Join(second, nil)), "content_block_start")
		firstBlock := mustMap(t, firstStart["content_block"])
		secondBlock := mustMap(t, secondStart["content_block"])
		firstID := mustString(t, firstBlock["id"])
		secondID := mustString(t, secondBlock["id"])
		if mustInt(t, firstStart["index"]) != 0 || mustInt(t, secondStart["index"]) != 1 || firstID == "" || secondID == "" || firstID == secondID || firstBlock["name"] != "lookup" || secondBlock["name"] != "search" {
			t.Fatalf("unexpected Anthropic tool identities: first=%s second=%s", string(bytes.Join(first, nil)), string(bytes.Join(second, nil)))
		}
	})

	t.Run("codex", func(t *testing.T) {
		var state any
		first, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Codex, "gpt-5-codex", nil, nil, firstChunk, &state)
		if err != nil {
			t.Fatalf("first chunk failed: %v", err)
		}
		second, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Codex, "gpt-5-codex", nil, nil, secondChunk, &state)
		if err != nil {
			t.Fatalf("second chunk failed: %v", err)
		}
		firstDone := mustSSEEventData(t, string(bytes.Join(first, nil)), "response.output_item.done")
		secondDone := mustSSEEventData(t, string(bytes.Join(second, nil)), "response.output_item.done")
		firstItem := mustMap(t, firstDone["item"])
		secondItem := mustMap(t, secondDone["item"])
		firstID := mustString(t, firstItem["call_id"])
		secondID := mustString(t, secondItem["call_id"])
		if mustInt(t, firstDone["output_index"]) != 0 || mustInt(t, secondDone["output_index"]) != 1 || firstID == "" || secondID == "" || firstID == secondID || firstItem["id"] != "fc_"+firstID || secondItem["id"] != "fc_"+secondID {
			t.Fatalf("unexpected Codex tool identities: first=%s second=%s", string(bytes.Join(first, nil)), string(bytes.Join(second, nil)))
		}
	})
}

func TestRegistry_TranslateResponseStream_AnthropicStructuredOutbound(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("openai", func(t *testing.T) {
		var state any
		start := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\"}}\n\n")
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, start, &state); err != nil || out != nil {
			t.Fatalf("content_block_start = %#v, %v", out, err)
		}
		delta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n")
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, delta, &state); err != nil || out != nil {
			t.Fatalf("content_block_delta = %#v, %v", out, err)
		}
		toolChunk, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_stop failed: %v", err)
		}
		joined := string(toolChunk[0])
		if len(toolChunk) != 1 || !strings.Contains(joined, `"tool_calls"`) || !strings.Contains(joined, `"id":"toolu_1"`) || !strings.Contains(joined, `"name":"lookup"`) || !strings.Contains(joined, `"arguments":"{\"query\":\"go\"}"`) {
			t.Fatalf("unexpected OpenAI tool chunk: %#v", toolChunk)
		}
		finish, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_delta failed: %v", err)
		}
		if len(finish) != 1 || !strings.Contains(string(finish[0]), `"finish_reason":"tool_calls"`) || !strings.Contains(string(finish[0]), `"prompt_tokens":3`) {
			t.Fatalf("unexpected OpenAI finish chunk: %#v", finish)
		}
		done, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_stop failed: %v", err)
		}
		if len(done) != 1 || string(done[0]) != "data: [DONE]\n\n" {
			t.Fatalf("unexpected OpenAI done chunk: %#v", done)
		}
	})

	t.Run("codex", func(t *testing.T) {
		var state any
		start := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3-5-sonnet\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n")
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, start, &state)
		if err != nil || !strings.Contains(string(bytes.Join(out, nil)), `event: response.created`) {
			t.Fatalf("message_start = %#v, %v", out, err)
		}
		toolStart := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\"}}\n\n")
		out, err = reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, toolStart, &state)
		if err != nil || !strings.Contains(string(bytes.Join(out, nil)), `event: response.output_item.added`) || !strings.Contains(string(bytes.Join(out, nil)), `"call_id":"toolu_1"`) {
			t.Fatalf("content_block_start = %#v, %v", out, err)
		}
		toolDelta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n")
		out, err = reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, toolDelta, &state)
		if err != nil || !strings.Contains(string(bytes.Join(out, nil)), `event: response.function_call_arguments.delta`) || !strings.Contains(string(bytes.Join(out, nil)), `"delta":"{\"query\":\"go\"}"`) {
			t.Fatalf("content_block_delta = %#v, %v", out, err)
		}
		toolChunk, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_stop failed: %v", err)
		}
		if len(toolChunk) != 0 {
			t.Fatalf("tool completion must wait for the terminal stop reason: %q", toolChunk)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("message_delta = %#v, %v", out, err)
		}
		done, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_stop failed: %v", err)
		}
		stream := string(bytes.Join(done, nil))
		item := mustMap(t, mustSSEEventData(t, stream, "response.output_item.done")["item"])
		arguments := mustJSONMap(t, []byte(mustString(t, item["arguments"])))
		if item["type"] != "function_call" || item["status"] != "completed" || item["call_id"] != "toolu_1" || item["name"] != "lookup" || arguments["query"] != "go" {
			t.Fatalf("unexpected Codex tool completion: %s", stream)
		}
		completed := mustMap(t, mustSSEEventData(t, stream, "response.completed")["response"])
		usage := mustMap(t, completed["usage"])
		if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(5) {
			t.Fatalf("unexpected Codex usage: %s", stream)
		}
	})
}

func TestRegistry_TranslateResponseNonStream_AnthropicReasoningAndUsageDetails(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"step by step","signature":"sig_1"},{"type":"text","text":"hello"},{"type":"redacted_thinking","data":"redacted_blob"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5,"cache_read_input_tokens":7,"cache_creation_input_tokens":11,"reasoning_tokens":13}}`)

	t.Run("openai", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		body := string(got)
		if !strings.Contains(body, `"reasoning_content":"step by step"`) || !strings.Contains(body, `"type":"thinking"`) || !strings.Contains(body, `"signature":"sig_1"`) || !strings.Contains(body, `"type":"redacted_thinking"`) || !strings.Contains(body, `"data":"redacted_blob"`) {
			t.Fatalf("unexpected OpenAI reasoning payload: %s", got)
		}
		if !strings.Contains(body, `"prompt_tokens":21`) || !strings.Contains(body, `"cached_tokens":7`) || !strings.Contains(body, `"cache_creation_input_tokens":11`) || !strings.Contains(body, `"reasoning_tokens":13`) {
			t.Fatalf("unexpected OpenAI usage payload: %s", got)
		}
	})

	t.Run("codex", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		body := string(got)
		if !strings.Contains(body, `"type":"reasoning"`) || !strings.Contains(body, `"type":"reasoning_text"`) || !strings.Contains(body, `"text":"step by step"`) || !strings.Contains(body, `"encrypted_content":"sig_1"`) || !strings.Contains(body, `"text":"hello"`) {
			t.Fatalf("unexpected Codex reasoning payload: %s", got)
		}
		if !strings.Contains(body, `"input_tokens":21`) || !strings.Contains(body, `"cached_tokens":7`) || !strings.Contains(body, `"cache_creation_input_tokens":11`) || !strings.Contains(body, `"reasoning_tokens":13`) {
			t.Fatalf("unexpected Codex usage payload: %s", got)
		}
	})
}

func TestRegistry_TranslateResponseStream_AnthropicReasoningAndUsageDetails(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("openai", func(t *testing.T) {
		var state any
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-3-5-sonnet\",\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":11}}}\n\n"), &state); err != nil || !strings.Contains(string(bytes.Join(out, nil)), `"role":"assistant"`) {
			t.Fatalf("message_start = %#v, %v", out, err)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("content_block_start = %#v, %v", out, err)
		}
		reasoning, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"step by step\"}}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_delta failed: %v", err)
		}
		if len(reasoning) != 1 || !strings.Contains(string(reasoning[0]), `"reasoning_content":"step by step"`) {
			t.Fatalf("unexpected OpenAI reasoning chunk: %#v", reasoning)
		}
		meta, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig_1\"}}\n\n"), &state)
		if err != nil || meta != nil {
			t.Fatalf("signature_delta = %#v, %v", meta, err)
		}
		meta, err = reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_stop failed: %v", err)
		}
		if len(meta) != 1 || !strings.Contains(string(meta[0]), `"reasoning"`) || !strings.Contains(string(meta[0]), `"type":"thinking"`) || !strings.Contains(string(meta[0]), `"signature":"sig_1"`) {
			t.Fatalf("unexpected OpenAI reasoning meta chunk: %#v", meta)
		}
		finish, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":11,\"reasoning_tokens\":13}}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_delta failed: %v", err)
		}
		if len(finish) != 1 || !strings.Contains(string(finish[0]), `"prompt_tokens":21`) || !strings.Contains(string(finish[0]), `"cached_tokens":7`) || !strings.Contains(string(finish[0]), `"cache_creation_input_tokens":11`) || !strings.Contains(string(finish[0]), `"reasoning_tokens":13`) {
			t.Fatalf("unexpected OpenAI finish chunk: %#v", finish)
		}
	})

	t.Run("codex", func(t *testing.T) {
		var state any
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3-5-sonnet\",\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":11}}}\n\n"), &state); err != nil || !strings.Contains(string(bytes.Join(out, nil)), `event: response.created`) {
			t.Fatalf("message_start = %#v, %v", out, err)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n"), &state); err != nil || !strings.Contains(string(bytes.Join(out, nil)), `event: response.output_item.added`) {
			t.Fatalf("content_block_start = %#v, %v", out, err)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"step by step\"}}\n\n"), &state); err != nil || !strings.Contains(string(bytes.Join(out, nil)), `event: response.reasoning_summary_text.delta`) {
			t.Fatalf("thinking_delta = %#v, %v", out, err)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig_1\"}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("signature_delta = %#v, %v", out, err)
		}
		reasoning, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_stop failed: %v", err)
		}
		summary := mustSSEEventData(t, string(bytes.Join(reasoning, nil)), "response.reasoning_summary_text.done")
		if summary["text"] != "step by step" {
			t.Fatalf("unexpected Codex reasoning summary: %q", reasoning)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":11,\"reasoning_tokens\":13}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("message_delta = %#v, %v", out, err)
		}
		done, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_stop failed: %v", err)
		}
		stream := string(bytes.Join(done, nil))
		item := mustMap(t, mustSSEEventData(t, stream, "response.output_item.done")["item"])
		if item["type"] != "reasoning" || item["encrypted_content"] != "sig_1" {
			t.Fatalf("unexpected Codex reasoning completion: %s", stream)
		}
		completed := mustMap(t, mustSSEEventData(t, stream, "response.completed")["response"])
		usage := mustMap(t, completed["usage"])
		inputDetails := mustMap(t, usage["input_tokens_details"])
		outputDetails := mustMap(t, usage["output_tokens_details"])
		if usage["input_tokens"] != float64(21) || inputDetails["cached_tokens"] != float64(7) || usage["cache_creation_input_tokens"] != float64(11) || outputDetails["reasoning_tokens"] != float64(13) {
			t.Fatalf("unexpected Codex usage: %s", stream)
		}
	})
}
