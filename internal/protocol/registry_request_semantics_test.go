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
		if !strings.Contains(body, `"type":"reasoning"`) || !strings.Contains(body, `"encrypted_content":"enc_1"`) || !strings.Contains(body, `"text":"hello"`) {
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
