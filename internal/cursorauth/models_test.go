package cursorauth

import "testing"

func TestPublicModelIDStripsThinkingInfix(t *testing.T) {
	t.Parallel()
	if got := PublicModelID("claude-sonnet-5-thinking-high"); got != "claude-sonnet-5-high" {
		t.Fatalf("got %q", got)
	}
	if got := PublicModelID("claude-sonnet-5"); got != "claude-sonnet-5" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveModelMapsClientThinking(t *testing.T) {
	t.Parallel()
	got := ResolveModel("claude-sonnet-5", ClientThinking{Enabled: true, Effort: "high"})
	if got != "claude-sonnet-5-thinking-high" {
		t.Fatalf("thinking on = %q", got)
	}
	got = ResolveModel("claude-sonnet-5", ClientThinking{Enabled: false, Effort: "high"})
	if got != "claude-sonnet-5-high" {
		t.Fatalf("thinking off = %q", got)
	}
	got = ResolveModel("claude-sonnet-4-5", ClientThinking{Enabled: true, Effort: "high"})
	if got != "claude-sonnet-5-thinking-high" {
		t.Fatalf("legacy sonnet = %q", got)
	}
	got = ResolveModel("gpt-5.3-codex", ClientThinking{Enabled: true, Effort: "high"})
	if got != "gpt-5.3-codex" {
		t.Fatalf("non-claude = %q", got)
	}
}

func TestParseClientThinkingDefaultsHigh(t *testing.T) {
	t.Parallel()
	thinking := ParseClientThinking([]byte(`{"model":"claude-sonnet-5"}`))
	if !thinking.Enabled || thinking.Effort != "high" {
		t.Fatalf("default = %+v", thinking)
	}
	thinking = ParseClientThinking([]byte(`{"thinking":{"type":"disabled"}}`))
	if thinking.Enabled {
		t.Fatalf("disabled = %+v", thinking)
	}
	thinking = ParseClientThinking([]byte(`{"thinking":{"type":"enabled","budget_tokens":16000}}`))
	if !thinking.Enabled || thinking.Effort != "xhigh" {
		t.Fatalf("budget = %+v", thinking)
	}
	thinking = ParseClientThinking([]byte(`{"reasoning_effort":"low"}`))
	if !thinking.Enabled || thinking.Effort != "low" {
		t.Fatalf("reasoning = %+v", thinking)
	}
}

func TestExtractPromptIgnoresToolBlocks(t *testing.T) {
	t.Parallel()
	prompt := ExtractPrompt([]byte(`{
		"system":"sys",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"1","name":"bash"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"bash"}]},
			{"role":"user","content":"follow up"}
		]
	}`))
	if prompt != "sys\n\nuser: hello\n\nuser: follow up" {
		t.Fatalf("prompt = %q", prompt)
	}
}
