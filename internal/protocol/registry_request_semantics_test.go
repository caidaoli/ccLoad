package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"

	"github.com/tidwall/gjson"
)

/**
 * @brief 独立 effort 转 Chat 保留等级，显式 thinking 的预算和开关仍优先
 * @param t 测试上下文
 */
func TestRegistryAnthropicStandaloneEffortToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, tc := range []struct {
		name, config, want string
	}{
		{"standalone", `"output_config":{"effort":"high"}`, "high"},
		{"normalized", `"output_config":{"effort":" HIGH "}`, "high"},
		{"empty", `"output_config":{"effort":" "}`, ""},
		{"invalid_type", `"output_config":{"effort":42}`, ""},
		{"budget_priority", `"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"high"}`, "medium"},
		{"enabled_effort", `"thinking":{"type":"enabled"},"output_config":{"effort":"high"}`, "high"},
		{"enabled_default", `"thinking":{"type":"enabled"}`, "auto"},
		{"adaptive", `"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`, "high"},
		{"disabled", `"thinking":{"type":"disabled"},"output_config":{"effort":"high"}`, "none"},
		{"between_tools", `"thinking":{"type":"between_tools"},"output_config":{"effort":"high"}`, ""},
		{"null_thinking", `"thinking":null,"output_config":{"effort":"high"}`, ""},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				raw := []byte(`{` + tc.config + `,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
				out, err := reg.TranslateRequest(protocol.Anthropic, protocol.OpenAI, "gpt-5.4", raw, stream)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(out, "reasoning_effort").String(); got != tc.want {
					t.Fatalf("reasoning_effort=%q, want %q: %s", got, tc.want, out)
				}
			})
		}
	}
}

/**
 * @brief Chat 空白思考不得创建 Claude 内容块，但保留思考块内部的空白
 * @param t 测试上下文
 */
func TestRegistryOpenAIWhitespaceReasoningNonStream(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, tc := range []struct {
		name, message string
		want          []string
	}{
		{"reasoning_content", `{"content":"Hi","reasoning_content":" \n\t"}`, []string{"text:Hi"}},
		{"reasoning", `{"content":"Hi","reasoning":" \n\t"}`, []string{"text:Hi"}},
		{"reasoning_details", `{"content":"Hi","reasoning_details":[{"type":"reasoning.text","text":" "}]}`, []string{"text:Hi"}},
		{"thinking_part", `{"content":[{"type":"thinking","thinking":" "},{"type":"text","text":"Hi"}]}`, []string{"text:Hi"}},
		{"thinking_parts", `{"content":[{"type":"thinking","thinking":"a"},{"type":"reasoning","text":" "},{"type":"thinking","thinking":"b"},{"type":"text","text":"Hi"}]}`, []string{"thinking:a b", "text:Hi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":` + tc.message + `,"finish_reason":"stop"}]}`)
			out, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "m", nil, nil, raw)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, block := range gjson.GetBytes(out, "content").Array() {
				got = append(got, block.Get("type").String()+":"+block.Get("thinking").String()+block.Get("text").String())
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("blocks=%q, want %q: %s", got, tc.want, out)
			}
		})
	}
}

/**
 * @brief Chat 流式及工具缓冲思考只允许非空白片段创建 Claude thinking 块
 * @param t 测试上下文
 */
func TestRegistryOpenAIWhitespaceReasoningStream(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	toolCall := `{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{}"}}]}`
	for _, tc := range []struct {
		name   string
		deltas []string
		want   []string
	}{
		{"whitespace_only", []string{`{"reasoning_content":" \n\t"}`, `{"content":"Hi"}`}, []string{"text:Hi"}},
		{"inside_thought", []string{`{"reasoning_content":"a"}`, `{"reasoning_content":"\n\n"}`, `{"reasoning_content":"b"}`}, []string{"thinking:a\n\nb"}},
		{"after_text", []string{`{"reasoning_content":"a"}`, `{"content":"x"}`, `{"reasoning_content":" "}`, `{"content":"y"}`}, []string{"thinking:a", "text:xy"}},
		{"tool_whitespace", []string{toolCall, `{"reasoning_content":" "}`}, []string{"tool_use:"}},
		{"tool_thought", []string{toolCall, `{"reasoning_content":"a"}`, `{"reasoning_content":" "}`, `{"reasoning_content":"b"}`}, []string{"tool_use:", "thinking:a b"}},
		{"typed_parts", []string{`{"content":[{"type":"thinking","thinking":" "},{"type":"text","text":"Hi"}]}`}, []string{"text:Hi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var chunks []string
			for _, delta := range tc.deltas {
				chunks = append(chunks, "data: "+`{"id":"c1","model":"m","choices":[{"index":0,"delta":`+delta+`,"finish_reason":null}]}`+"\n\n")
			}
			chunks = append(chunks, "data: "+`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n", "data: [DONE]\n\n")
			stream := translateResponseStreamChunks(t, reg, protocol.OpenAI, protocol.Anthropic, "m", chunks...)
			var got []string
			var stopCount int
			for _, event := range parseSSEEvents(t, stream) {
				switch event.Event {
				case "content_block_start":
					got = append(got, gjson.Get(event.RawData, "content_block.type").String()+":")
				case "content_block_delta":
					got[len(got)-1] += gjson.Get(event.RawData, "delta.thinking").String() + gjson.Get(event.RawData, "delta.text").String()
				case "message_stop":
					stopCount++
				}
			}
			if !slices.Equal(got, tc.want) || stopCount != 1 {
				t.Fatalf("blocks=%q, want %q; message_stop=%d: %s", got, tc.want, stopCount, stream)
			}
		})
	}
}

func TestRegistryAnthropicMidConversationEffort(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, thinking := range []string{`{"type":"adaptive"}`, `{"type":"enabled","budget_tokens":1024}`, `null`} {
		for _, target := range []struct {
			protocol    protocol.Protocol
			model, path string
		}{
			{protocol.OpenAI, "gpt-5", "reasoning_effort"},
			{protocol.Codex, "gpt-5", "reasoning.effort"},
			{protocol.Gemini, "gemini-3.8-flash-high", "generationConfig.thinkingConfig.thinkingLevel"},
		} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", target.protocol, thinking, stream), func(t *testing.T) {
					raw := []byte(`{"thinking":` + thinking + `,"output_config":{"effort":"medium"},"messages":[{"role":"system","content":"earlier","output_config":{"effort":"low"}},{"role":"user","content":"work"},{"role":"system","content":"latest","output_config":{"effort":" HIGH "}},{"role":"system","content":"empty","output_config":{"effort":" "}},{"role":"user","content":"continue","output_config":{"effort":"low"}}]}`)
					out, err := reg.TranslateRequest(protocol.Anthropic, target.protocol, target.model, raw, stream)
					if err != nil {
						t.Fatal(err)
					}
					if got := gjson.GetBytes(out, target.path).String(); got != "high" {
						t.Fatalf("effort=%q, want high: %s", got, out)
					}
					native, err := reg.TranslateRequest(protocol.Anthropic, protocol.Anthropic, "claude-opus-5-5", raw, stream)
					if err != nil || string(native) != string(raw) {
						t.Fatalf("native request changed: %s, %v", native, err)
					}
				})
			}
		}
	}
}

func TestRegistryAnthropicEffortDirectiveControls(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, tc := range []struct {
		name, thinking, role, directive, wantChat, wantCodex, wantGemini string
	}{
		{"disabled", "disabled", "system", `"high"`, "none", "none", ""},
		{"between_tools", "between_tools", "system", `"high"`, "", "medium", ""},
		{"none", "adaptive", "system", `"none"`, "none", "none", ""},
		{"auto", "adaptive", "system", `"auto"`, "xhigh", "xhigh", ""},
		{"user", "adaptive", "user", `"high"`, "medium", "medium", "medium"},
		{"invalid_type", "adaptive", "system", `42`, "medium", "medium", "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"thinking":{"type":%q},"output_config":{"effort":"medium"},"messages":[{"role":%q,"content":"work","output_config":{"effort":%s}}]}`, tc.thinking, tc.role, tc.directive))
			for _, target := range []struct {
				protocol          protocol.Protocol
				model, path, want string
			}{
				{protocol.OpenAI, "gpt-5", "reasoning_effort", tc.wantChat},
				{protocol.Codex, "gpt-5", "reasoning.effort", tc.wantCodex},
				{protocol.Gemini, "gemini-3.8-flash-high", "generationConfig.thinkingConfig.thinkingLevel", tc.wantGemini},
			} {
				out, err := reg.TranslateRequest(protocol.Anthropic, target.protocol, target.model, raw, false)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(out, target.path).String(); got != target.want {
					t.Fatalf("%s effort=%q, want %q: %s", target.protocol, got, target.want, out)
				}
				if tc.name == "none" && target.protocol == protocol.Gemini {
					if budget := gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingBudget"); budget.Type != gjson.Number || budget.Int() != 0 {
						t.Fatalf("none directive did not disable Gemini thinking: %s", out)
					}
				}
			}
		})
	}
}

func TestRegistryResponsesPreservesDeveloperRoles(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	raw := []byte(`{"instructions":"system rule","input":[{"role":"developer","content":"planning rule"},{"role":"user","content":"hello"},{"role":"developer","content":[{"type":"input_text","text":"remain in planning mode"}]}]}`)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			out, err := reg.TranslateRequest(protocol.Codex, protocol.OpenAI, "gpt-5", raw, stream)
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Messages []struct {
					Role    string `json:"role"`
					Content any    `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(out, &request); err != nil {
				t.Fatal(err)
			}
			wantRoles := []string{"system", "developer", "user", "developer"}
			wantText := []string{"system rule", "planning rule", "hello", "remain in planning mode"}
			if len(request.Messages) != len(wantRoles) {
				t.Fatalf("message count changed: %s", out)
			}
			for i, message := range request.Messages {
				if message.Role != wantRoles[i] || protocolTestContentText(message.Content) != wantText[i] {
					t.Errorf("message %d lost role or content: %s", i, out)
				}
			}
		})
	}
}

func TestRegistryGeminiNumericSchemaConstraints(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"check","parameters":{"type":"OBJECT","minProperties":"1","maxProperties":"10","properties":{"text":{"type":"STRING","minLength":"1","maxLength":"200","enum":["1","2"],"default":"1"},"items":{"type":"ARRAY","minItems":"1","maxItems":"5","items":{"type":"STRING","minLength":"2"}}}}}]}]}`)
	for _, target := range []struct {
		protocol protocol.Protocol
		model    string
		path     string
	}{
		{protocol.OpenAI, "gpt-5", "tools.0.function.parameters"},
		{protocol.Anthropic, "claude-sonnet-4-6", "tools.0.input_schema"},
		{protocol.Codex, "gpt-5", "tools.0.parameters"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", target.protocol, stream), func(t *testing.T) {
				out, err := reg.TranslateRequest(protocol.Gemini, target.protocol, target.model, raw, stream)
				if err != nil {
					t.Fatal(err)
				}
				schema := gjson.GetBytes(out, target.path)
				for path, want := range map[string]int64{
					"minProperties": 1, "maxProperties": 10,
					"properties.text.minLength": 1, "properties.text.maxLength": 200,
					"properties.items.minItems": 1, "properties.items.maxItems": 5,
					"properties.items.items.minLength": 2,
				} {
					if got := schema.Get(path); got.Type != gjson.Number || got.Int() != want {
						t.Errorf("%s = %s, want JSON number %d: %s", path, got.Raw, want, out)
					}
				}
				for _, path := range []string{"properties.text.enum.0", "properties.text.default"} {
					if got := schema.Get(path); got.Type != gjson.String || got.String() != "1" {
						t.Errorf("literal data changed at %s: %s", path, out)
					}
				}
			})
		}
	}
}

func TestRegistryRequestOutputTokenLimits(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, tc := range []struct {
		name   string
		source protocol.Protocol
		body   string
	}{
		{"chat", protocol.OpenAI, `{"messages":[{"role":"user","content":"hello"}],"max_completion_tokens":32,"max_tokens":64}`},
		{"claude", protocol.Anthropic, `{"messages":[{"role":"user","content":"hello"}],"max_tokens":32}`},
		{"gemini", protocol.Gemini, `{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":32}}`},
		{"responses passthrough", protocol.Codex, `{"input":"hello","max_output_tokens":32}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				out, err := reg.TranslateRequest(tc.source, protocol.Codex, "gpt-5.6-sol", []byte(tc.body), stream)
				if err != nil {
					t.Fatal(err)
				}
				if limit := gjson.GetBytes(out, "max_output_tokens"); limit.Type != gjson.Number || limit.Int() != 32 {
					t.Fatalf("output token limit was lost: %s", out)
				}
				for _, field := range []string{"max_tokens", "max_completion_tokens"} {
					if gjson.GetBytes(out, field).Exists() {
						t.Fatalf("legacy limit %s reached Responses request: %s", field, out)
					}
				}
			})
		}
	}
}

func TestRegistryRequestSemantics(t *testing.T) {
	t.Run("anthropic to openai keeps assistant thinking as reasoning_content", func(t *testing.T) {
		reg := protocol.NewRegistry()
		builtin.Register(reg)

		raw := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"step by step"},{"type":"text","text":"hello"}]}]}`)
		out, err := reg.TranslateRequest(protocol.Anthropic, protocol.OpenAI, "gpt-4o", raw, false)
		if err != nil {
			t.Fatalf("TranslateRequest failed: %v", err)
		}
		var request struct {
			Messages []struct {
				ReasoningContent string `json:"reasoning_content"`
				Content          any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(out, &request); err != nil {
			t.Fatalf("unmarshal OpenAI request: %v", err)
		}
		if len(request.Messages) != 1 || request.Messages[0].ReasoningContent != "step by step" || protocolTestContentText(request.Messages[0].Content) != "hello" {
			t.Fatalf("unexpected openai request body: %s", out)
		}
	})

	t.Run("anthropic to codex preserves redacted_thinking as encrypted_content", func(t *testing.T) {
		reg := protocol.NewRegistry()
		builtin.Register(reg)

		raw := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"enc_1"},{"type":"text","text":"hello"}]}]}`)
		out, err := reg.TranslateRequest(protocol.Anthropic, protocol.Codex, "gpt-5-codex", raw, false)
		if err != nil {
			t.Fatalf("TranslateRequest failed: %v", err)
		}
		body := string(out)
		if !strings.Contains(body, `"type":"reasoning"`) || !strings.Contains(body, `"encrypted_content":"enc_1"`) || !strings.Contains(body, `"type":"output_text"`) || !strings.Contains(body, `"text":"hello"`) {
			t.Fatalf("unexpected codex request body: %s", body)
		}
	})

	t.Run("anthropic rich tool_result becomes structured openai tool content", func(t *testing.T) {
		reg := protocol.NewRegistry()
		builtin.Register(reg)

		raw := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"tool ok"},{"type":"image","source":{"type":"url","url":"https://example.com/tool.png","media_type":"image/png"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"},"title":"doc.pdf"}]}]}]}`)
		out, err := reg.TranslateRequest(protocol.Anthropic, protocol.OpenAI, "gpt-4o", raw, false)
		if err != nil {
			t.Fatalf("TranslateRequest failed: %v", err)
		}
		body := string(out)
		if !strings.Contains(body, `"role":"tool"`) || !strings.Contains(body, `"type":"image_url"`) || !strings.Contains(body, `"type":"file"`) {
			t.Fatalf("unexpected openai request body: %s", body)
		}
	})
}

func TestRegistryAnthropicMidConversationTools(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	raw := []byte(`{"tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{}}]},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"Finish","input_schema":{"type":"object","properties":{"summary":{"type":"string"}}}}}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}]}`)
	for _, target := range []protocol.Protocol{protocol.OpenAI, protocol.Codex} {
		for _, stream := range []bool{false, true} {
			out, err := reg.TranslateRequest(protocol.Anthropic, target, "gpt-5", raw, stream)
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Tools []struct {
					Name     string `json:"name"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
				Input []struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Tools  []struct {
						Name       string `json:"name"`
						Parameters struct {
							Properties map[string]any `json:"properties"`
						} `json:"parameters"`
					} `json:"tools"`
				} `json:"input"`
			}
			if err := json.Unmarshal(out, &request); err != nil {
				t.Fatal(err)
			}
			if target == protocol.OpenAI {
				if len(request.Tools) != 2 || request.Tools[0].Function.Name != "Read" || request.Tools[1].Function.Name != "Finish" {
					t.Fatalf("dynamic Chat tools lost: %s", out)
				}
			} else {
				if len(request.Tools) != 1 || request.Tools[0].Name != "Read" || len(request.Input) != 3 {
					t.Fatalf("dynamic Responses tools changed cache prefix or history: %s", out)
				}
				if request.Input[0].Type != "function_call" || request.Input[1].Type != "function_call_output" || request.Input[0].CallID != request.Input[1].CallID {
					t.Fatalf("tool pair separated: %s", out)
				}
				addition := request.Input[2]
				if addition.Type != "additional_tools" || len(addition.Tools) != 1 || addition.Tools[0].Name != "Finish" || addition.Tools[0].Parameters.Properties["summary"] == nil {
					t.Fatalf("dynamic Responses tool definition lost: %s", out)
				}
			}
		}
	}
}

/**
 * @brief 验证 Anthropic 独立工具引用仅在 Codex 桥接开放并保留发现工具定义
 * @param t 测试上下文
 */
func TestRegistryAnthropicDeferredToolReferences(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, block := range []string{
		`{"type":"tool_reference","tool_name":"Lookup"}`,
		`{"type":"tool_search_tool_result","content":{"tool_references":[{"type":"tool_reference","name":"Lookup"}]}}`,
	} {
		for _, target := range []protocol.Protocol{protocol.Codex, protocol.OpenAI, protocol.Gemini} {
			for _, stream := range []bool{false, true} {
				raw := []byte(`{"tools":[{"name":"Lookup","defer_loading":true,"input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[` + block + `]}]}`)
				out, err := reg.TranslateRequest(protocol.Anthropic, target, "target", raw, stream)
				if target != protocol.Codex {
					if _, ok := errors.AsType[*protocol.RequestTranslationError](err); !ok {
						t.Fatalf("unsupported tool reference target=%s: %v", target, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(out, "input.0.type").String() != "additional_tools" || gjson.GetBytes(out, "input.0.tools.0.name").String() != "Lookup" || gjson.GetBytes(out, "tools.#").Int() != 0 {
					t.Fatalf("deferred tool definition was lost or leaked to top-level tools: %s", out)
				}
			}
		}
	}
}

/**
 * @brief 验证未知引用和不可表示的搜索结果返回协议能力错误
 * @param t 测试上下文
 */
func TestRegistryAnthropicRejectsInvalidDeferredToolReferences(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, block := range []string{
		`{"type":"tool_reference","tool_name":"missing"}`,
		`{"type":"tool_reference"}`,
		`{"type":"tool_search_tool_result","content":{"tool_references":[{"type":"tool_reference","name":"missing"}]}}`,
		`{"type":"tool_search_tool_result","content":{"tool_references":{}}}`,
		`{"type":"tool_search_tool_result","content":{"tool_references":[{"type":"text","text":"unsupported reference"}]}}`,
	} {
		for _, stream := range []bool{false, true} {
			raw := []byte(`{"messages":[{"role":"user","content":[` + block + `]}]}`)
			_, err := reg.TranslateRequest(protocol.Anthropic, protocol.Codex, "target", raw, stream)
			if _, ok := errors.AsType[*protocol.RequestTranslationError](err); !ok {
				t.Fatalf("invalid reference %s did not return a translation error: %v", block, err)
			}
		}
	}
	raw := []byte(`{"tools":[{"name":"Lookup","defer_loading":true,"input_schema":{"type":"object"}}],"messages":[{"role":"system","content":[{"type":"tool_reference","name":"Lookup"}]}]}`)
	if _, err := reg.TranslateRequest(protocol.Anthropic, protocol.Codex, "target", raw, false); err == nil {
		t.Fatal("system tool reference must not be silently ignored")
	}
}

/**
 * @brief 验证历史工具结果内缺少当前声明的引用仍以结果文本透传
 * @param t 测试上下文
 */
func TestRegistryAnthropicPreservesUnresolvedToolResultReference(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	raw := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"search_1","name":"ToolSearch","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"search_1","content":[{"type":"text","text":"found"},{"type":"tool_reference","tool_name":"Bash"}]}]}]}`)
	for _, stream := range []bool{false, true} {
		out, err := reg.TranslateRequest(protocol.Anthropic, protocol.Codex, "target", raw, stream)
		if err != nil {
			t.Fatal(err)
		}
		items := gjson.GetBytes(out, "input").Array()
		if len(items) != 2 || items[1].Get("type").String() != "function_call_output" || items[1].Get("output.0.text").String() != "found" || gjson.Parse(items[1].Get("output.1.text").String()).Get("tool_name").String() != "Bash" {
			t.Fatalf("historical tool reference was lost or generated a declaration: %s", out)
		}
	}
}

func TestRegistryAnthropicToolResultErrors(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, stream := range []bool{false, true} {
		for _, isError := range []bool{false, true} {
			raw, err := json.Marshal(map[string]any{"messages": []any{
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_1", "name": "Read", "input": map[string]any{}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_1", "is_error": isError, "content": map[string]any{"code": "EACCES"}}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			out, err := reg.TranslateRequest(protocol.Anthropic, protocol.Gemini, "gemini-3-flash-preview", raw, stream)
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Contents []struct {
					Parts []struct {
						FunctionResponse struct {
							ID       string `json:"id"`
							Name     string `json:"name"`
							Response map[string]struct {
								Code string `json:"code"`
							} `json:"response"`
						} `json:"functionResponse"`
					} `json:"parts"`
				} `json:"contents"`
			}
			if err := json.Unmarshal(out, &request); err != nil {
				t.Fatal(err)
			}
			if len(request.Contents) != 2 || len(request.Contents[1].Parts) != 1 {
				t.Fatalf("tool history lost: %s", out)
			}
			result := request.Contents[1].Parts[0].FunctionResponse
			key := "result"
			if isError {
				key = "error"
			}
			if result.ID != "call_1" || result.Name != "Read" || len(result.Response) != 1 || result.Response[key].Code != "EACCES" {
				t.Fatalf("tool result semantics lost: %s", out)
			}
		}
	}
}

func TestRegistryAnthropicRejectsUnsupportedToolAdditions(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, target := range []protocol.Protocol{protocol.OpenAI, protocol.Codex, protocol.Gemini} {
		for _, role := range []string{"system", "user", "assistant"} {
			for _, stream := range []bool{false, true} {
				raw := []byte(`{"messages":[{"role":"` + role + `","content":[{"type":"text","text":"ordinary text must not hide unsupported tools"},{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"Finish","input_schema":{"type":"object"}}}}]}]}`)
				out, err := reg.TranslateRequest(protocol.Anthropic, target, "target", raw, stream)
				if role == "system" && target != protocol.Gemini {
					if err != nil {
						t.Fatal(err)
					}
					path := "messages.0.content.0.text"
					if target == protocol.Codex {
						path = "input.0.content.0.text"
					}
					if !strings.Contains(gjson.GetBytes(out, path).String(), "ordinary text must not hide unsupported tools") {
						t.Fatalf("tool addition dropped sibling text: %s", out)
					}
					continue
				}
				if _, ok := errors.AsType[*protocol.RequestTranslationError](err); !ok {
					t.Fatalf("unsupported dynamic tool target=%s role=%s: %v", target, role, err)
				}
			}
		}
	}
	for _, target := range []protocol.Protocol{protocol.OpenAI, protocol.Codex} {
		for _, spec := range []string{`{}`, `{"type":"tool_reference","name":"missing"}`, `{"type":"tool_definition","definition":{}}`, `{"type":"tool_reference","definition":{"name":"Finish"}}`, `{"type":"tool_definition","definition":{"name":"search","type":"web_search_20250305"}}`} {
			raw := []byte(`{"messages":[{"role":"system","content":[{"type":"tool_addition","tool":` + spec + `}]}]}`)
			_, err := reg.TranslateRequest(protocol.Anthropic, target, "target", raw, false)
			if _, ok := errors.AsType[*protocol.RequestTranslationError](err); !ok {
				t.Fatalf("unsupported tool specification target=%s spec=%s: %v", target, spec, err)
			}
		}
	}
}
