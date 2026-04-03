package testutil

import (
	"strings"
	"testing"
)

func TestBuildRequestFromTemplate_PreservesAnthropicTopLevelFieldOrder(t *testing.T) {
	replacements := map[string]any{
		"MODEL":      "claude-haiku-4-5-20251001",
		"CONTENT":    "list file",
		"USER_ID":    "user_123",
		"MAX_TOKENS": 32000,
		"STREAM":     true,
	}
	wantOrder := []string{
		`"model":`,
		`"messages":`,
		`"system":`,
		`"tools":`,
		`"metadata":`,
		`"max_tokens":`,
		`"stream":`,
	}

	for i := 0; i < 16; i++ {
		body, err := buildRequestFromTemplate("anthropic", replacements)
		if err != nil {
			t.Fatalf("buildRequestFromTemplate failed: %v", err)
		}

		lastIndex := -1
		for _, field := range wantOrder {
			index := strings.Index(string(body), field)
			if index == -1 {
				t.Fatalf("field %s not found in output: %s", field, string(body))
			}
			if index <= lastIndex {
				t.Fatalf("field order mismatch for %s in output: %s", field, string(body))
			}
			lastIndex = index
		}
	}
}
