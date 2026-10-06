package cursorauth

import (
	"strings"
	"testing"
)

func TestParseRequestPromptIncludesToolTurns(t *testing.T) {
	t.Parallel()
	prompt := ParseRequest([]byte(`{
		"system":"sys",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"bash","input":{"cmd":"ls"}}]},
			{"role":"user","content":"follow up"}
		]
	}`)).Prompt
	if !strings.Contains(prompt, "sys") || !strings.Contains(prompt, "hello") ||
		!strings.Contains(prompt, "[tool_use id=1 name=bash]") || !strings.Contains(prompt, "follow up") {
		t.Fatalf("prompt = %q", prompt)
	}
}
