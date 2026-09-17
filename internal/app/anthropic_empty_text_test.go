package app

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeAnthropicEmptyTextBlocksDropsToolUsePlaceholders(t *testing.T) {
	t.Parallel()
	in := []byte(`{"model":"claude-opus-5","max_tokens":64000,"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":[{"type":"text","text":"run it"}]},{"role":"assistant","content":[{"type":"text","text":""},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":""},{"type":"text","text":"ok"}]}]}],"thinking":{"type":"adaptive"}}`)
	out := sanitizeAnthropicEmptyTextBlocks(in)
	if gjson.GetBytes(out, "messages.1.content.#").Int() != 1 {
		t.Fatalf("assistant content = %s", gjson.GetBytes(out, "messages.1.content").Raw)
	}
	if gjson.GetBytes(out, "messages.1.content.0.type").String() != "tool_use" {
		t.Fatalf("expected tool_use to remain, got %s", gjson.GetBytes(out, "messages.1.content").Raw)
	}
	if gjson.GetBytes(out, "messages.2.content.0.content.#").Int() != 1 || gjson.GetBytes(out, "messages.2.content.0.content.0.text").String() != "ok" {
		t.Fatalf("tool_result = %s", gjson.GetBytes(out, "messages.2.content").Raw)
	}
	if gjson.GetBytes(out, "output_config.effort").String() != "xhigh" || gjson.GetBytes(out, "thinking.type").String() != "adaptive" {
		t.Fatal("non-message fields must stay")
	}
}

func TestSanitizeAnthropicEmptyTextBlocksNoopsWhenClean(t *testing.T) {
	t.Parallel()
	in := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]}`)
	out := sanitizeAnthropicEmptyTextBlocks(in)
	if string(out) != string(in) {
		t.Fatalf("clean body rewritten:\n%s\n%s", in, out)
	}
}

func TestSanitizeAnthropicEmptyTextBlocksKeepsThinkingAndNonEmptyText(t *testing.T) {
	t.Parallel()
	in := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"  "},{"type":"text","text":"done"}]}]}`)
	out := sanitizeAnthropicEmptyTextBlocks(in)
	if gjson.GetBytes(out, "messages.0.content.#").Int() != 2 {
		t.Fatalf("content = %s", gjson.GetBytes(out, "messages.0.content").Raw)
	}
	if gjson.GetBytes(out, "messages.0.content.0.type").String() != "thinking" {
		t.Fatal("thinking block dropped")
	}
	if gjson.GetBytes(out, "messages.0.content.1.text").String() != "done" {
		t.Fatal("non-empty text dropped")
	}
}

func TestSanitizeAnthropicEmptyTextBlocksDoesNotEmptyAMessage(t *testing.T) {
	t.Parallel()
	in := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":""}]},{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`)
	out := sanitizeAnthropicEmptyTextBlocks(in)
	if gjson.GetBytes(out, "messages.#").Int() != 2 {
		t.Fatalf("message count changed: %s", out)
	}
	if gjson.GetBytes(out, "messages.0.content.0.text").String() != "" {
		t.Fatal("sole empty user text should stay rather than become []")
	}
}

func TestNativeClaudeCodeFinalizeStripsEmptyTextOnly(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-5","max_tokens":64000,"temperature":0.7,"messages":[{"role":"assistant","content":[{"type":"text","text":""},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"pwd"}}]}],"system":[{"type":"text","text":"keep me"}]}`)
	out, err := finalizeAnthropicClaudeCodeMessagesBody(body, anthropicGoldenConfig(t, anthropicGoldenCase{oauth: true}), "", anthropicGoldenNativeHeaders(), anthropicOfficialTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "messages.0.content.#").Int() != 1 || gjson.GetBytes(out, "messages.0.content.0.name").String() != "Bash" {
		t.Fatalf("native empty text not stripped: %s", out)
	}
	if gjson.GetBytes(out, "temperature").Float() != 0.7 {
		t.Fatal("native sampling must not be rewritten")
	}
	if gjson.GetBytes(out, "system.0.text").String() != "keep me" {
		t.Fatal("native system rewritten")
	}
	if !strings.Contains(string(out), `"name":"Bash"`) {
		t.Fatal("tool_use lost")
	}
}

func TestApplyAnthropicMessagesAPIInvariantsIsTheEmptyTextHook(t *testing.T) {
	t.Parallel()
	in := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":""},{"type":"tool_use","id":"t","name":"Bash","input":{}}]}]}`)
	out := applyAnthropicMessagesAPIInvariants(in)
	if gjson.GetBytes(out, "messages.0.content.0.type").String() != "tool_use" {
		t.Fatalf("API invariant hook did not strip empty text: %s", out)
	}
}

func TestNativeClaudeCodeFinalizeLeavesCleanBodyBytes(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	out, err := finalizeAnthropicClaudeCodeMessagesBody(body, anthropicGoldenConfig(t, anthropicGoldenCase{oauth: true}), "", anthropicGoldenNativeHeaders(), anthropicOfficialTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("clean native body changed:\n%s\n%s", body, out)
	}
}
