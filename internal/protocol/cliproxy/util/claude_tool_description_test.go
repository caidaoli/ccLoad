package util

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnsureClaudeToolDescriptionFillsMissingField(t *testing.T) {
	t.Parallel()
	got := EnsureClaudeToolDescription([]byte(`{"name":"shell","input_schema":{"type":"object"}}`))
	if gjson.GetBytes(got, "description").String() != "shell" {
		t.Fatalf("description = %s", got)
	}
	if !gjson.GetBytes(got, "description").Exists() {
		t.Fatal("description key missing")
	}
}

func TestEnsureClaudeToolDescriptionKeepsExisting(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"shell","description":"Run a command"}`)
	got := EnsureClaudeToolDescription(in)
	if gjson.GetBytes(got, "description").String() != "Run a command" {
		t.Fatalf("rewrote existing description: %s", got)
	}
}

func TestEnsureClaudeToolsDescriptionsWebSearchWithoutDescription(t *testing.T) {
	t.Parallel()
	in := []byte(`{"tools":[{"name":"a","description":"A","input_schema":{}},{"type":"web_search_20250305","name":"web_search"}]}`)
	out, changed := EnsureClaudeToolsDescriptions(in)
	if !changed {
		t.Fatal("expected web_search description to be filled")
	}
	if gjson.GetBytes(out, "tools.0.description").String() != "A" {
		t.Fatal("existing description changed")
	}
	if gjson.GetBytes(out, "tools.1.description").String() != "web_search" {
		t.Fatalf("web_search description = %s", out)
	}
}
