package app

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAnthropicClaudeCodeMimicBetasIncludesThinkingDisplayUpdates(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-5-5","thinking":{"type":"adaptive","display":"updates"},"messages":[{"role":"user","content":"hi"}]}`)
	betas := anthropicClaudeCodeMimicBetas(body, true)
	if !strings.Contains(betas, "thinking-display-updates-2026-08-18") {
		t.Fatalf("OAuth betas missing thinking-display-updates: %s", betas)
	}
	plain := []byte(`{"model":"claude-opus-5-5","thinking":{"type":"adaptive","display":"omitted"},"messages":[{"role":"user","content":"hi"}]}`)
	if strings.Contains(anthropicClaudeCodeMimicBetas(plain, true), "thinking-display-updates-2026-08-18") {
		t.Fatal("thinking-display-updates declared without display=updates")
	}
}

func TestSanitizeAnthropicBodyForBetaTokensStripsUpdatesDisplayWithoutBeta(t *testing.T) {
	t.Parallel()
	body := []byte(`{"thinking":{"type":"adaptive","display":"updates"},"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitizeAnthropicBodyForBetaTokens(body, "claude-code-20250219")
	if gjson.GetBytes(got, "thinking.display").Exists() {
		t.Fatalf("updates display survived without beta: %s", got)
	}
	if gjson.GetBytes(got, "thinking.type").String() != "adaptive" {
		t.Fatalf("adaptive thinking lost: %s", got)
	}
	kept := sanitizeAnthropicBodyForBetaTokens(body, "claude-code-20250219,thinking-display-updates-2026-08-18")
	if gjson.GetBytes(kept, "thinking.display").String() != "updates" {
		t.Fatalf("updates display dropped despite beta: %s", kept)
	}
}

func TestIsAnthropicThinkingBlockErrorSkipsDisplayExtraInputs(t *testing.T) {
	t.Parallel()
	if isAnthropicThinkingBlockError("thinking.display: Extra inputs are not permitted") {
		t.Fatal("thinking.display Extra inputs must not trigger thinking downgrade")
	}
	if !isAnthropicThinkingBlockError("thinking blocks are not supported") {
		t.Fatal("real thinking-unsupported errors must still match")
	}
}
