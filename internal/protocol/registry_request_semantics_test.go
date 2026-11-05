package protocol_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"

	"github.com/tidwall/gjson"
)

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
