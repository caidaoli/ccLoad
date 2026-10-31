package protocol_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"

	"github.com/tidwall/gjson"
)

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
				var translationError *protocol.RequestTranslationError
				if !errors.As(err, &translationError) {
					t.Fatalf("unsupported dynamic tool target=%s role=%s: %v", target, role, err)
				}
			}
		}
	}
	for _, target := range []protocol.Protocol{protocol.OpenAI, protocol.Codex} {
		for _, spec := range []string{`{}`, `{"type":"tool_reference","name":"missing"}`, `{"type":"tool_definition","definition":{}}`, `{"type":"tool_reference","definition":{"name":"Finish"}}`, `{"type":"tool_definition","definition":{"name":"search","type":"web_search_20250305"}}`} {
			raw := []byte(`{"messages":[{"role":"system","content":[{"type":"tool_addition","tool":` + spec + `}]}]}`)
			_, err := reg.TranslateRequest(protocol.Anthropic, target, "target", raw, false)
			var translationError *protocol.RequestTranslationError
			if !errors.As(err, &translationError) {
				t.Fatalf("unsupported tool specification target=%s spec=%s: %v", target, spec, err)
			}
		}
	}
}
