package protocol_test

import (
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
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
		body := string(out)
		if !strings.Contains(body, `"reasoning_content":"step by step"`) || !strings.Contains(body, `"content":"hello"`) {
			t.Fatalf("unexpected openai request body: %s", body)
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

func TestRegistryRequestJSONTopLevelOrderStable(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	cases := []struct {
		name  string
		from  protocol.Protocol
		to    protocol.Protocol
		model string
		raw   []byte
		order []string
		exact bool
	}{
		{
			name:  "anthropic to codex keeps cache-sensitive prefix stable",
			from:  protocol.Anthropic,
			to:    protocol.Codex,
			model: "gpt-5-codex",
			raw:   []byte(`{"model":"claude-3-5-sonnet","system":[{"type":"text","text":"be careful"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"stream":true}`),
			order: []string{`"model"`, `"instructions"`, `"input"`, `"stream"`},
			exact: true,
		},
		{
			name:  "anthropic to openai top-level order stable",
			from:  protocol.Anthropic,
			to:    protocol.OpenAI,
			model: "gpt-4o",
			raw:   []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"stream":true}`),
			order: []string{`"model"`, `"messages"`, `"stream"`},
			exact: true,
		},
		{
			name:  "openai to anthropic top-level order stable",
			from:  protocol.OpenAI,
			to:    protocol.Anthropic,
			model: "claude-3-5-sonnet",
			raw:   []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":true}`),
			order: []string{`"model"`, `"messages"`, `"stream"`, `"tools"`, `"max_tokens"`, `"metadata"`},
		},
		{
			name:  "openai to gemini top-level order stable",
			from:  protocol.OpenAI,
			to:    protocol.Gemini,
			model: "gemini-2.5-pro",
			raw:   []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"be careful"},{"role":"user","content":"hello"}],"stream":true}`),
			order: []string{`"contents"`, `"systemInstruction"`},
			exact: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var first string
			for i := 0; i < 20; i++ {
				out, err := reg.TranslateRequest(tc.from, tc.to, tc.model, tc.raw, true)
				if err != nil {
					t.Fatalf("TranslateRequest failed: %v", err)
				}
				body := string(out)
				if i == 0 {
					first = body
					assertJSONFieldOrder(t, body, tc.order...)
					continue
				}
				if tc.exact && body != first {
					t.Fatalf("request JSON changed between runs:\nfirst=%s\nrun%d=%s", first, i, body)
				}
				assertJSONFieldOrder(t, body, tc.order...)
			}
		})
	}
}

func assertJSONFieldOrder(t *testing.T, body string, fields ...string) {
	t.Helper()
	prev := -1
	for _, field := range fields {
		idx := strings.Index(body, field)
		if idx < 0 {
			t.Fatalf("field %s missing in %s", field, body)
		}
		if idx <= prev {
			t.Fatalf("field order broken at %s in %s", field, body)
		}
		prev = idx
	}
}
