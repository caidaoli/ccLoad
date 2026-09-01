package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestSetResponsesToolCallIdentity(t *testing.T) {
	tests := []struct {
		name                string
		input               string
		toolName            string
		namespace           string
		itemPath            string
		namePath            string
		namespacePath       string
		wantName            string
		wantNamespace       string
		wantNamespaceExists bool
	}{
		{
			name:                "top level",
			input:               `{"name":"functions__exec"}`,
			toolName:            "exec",
			namespace:           "functions",
			namePath:            "name",
			namespacePath:       "namespace",
			wantName:            "exec",
			wantNamespace:       "functions",
			wantNamespaceExists: true,
		},
		{
			name:                "nested item",
			input:               `{"item":{"name":"functions__exec"}}`,
			toolName:            "exec",
			namespace:           "functions",
			itemPath:            "item",
			namePath:            "item.name",
			namespacePath:       "item.namespace",
			wantName:            "exec",
			wantNamespace:       "functions",
			wantNamespaceExists: true,
		},
		{
			name:          "remove stale namespace",
			input:         `{"name":"old","namespace":"stale"}`,
			toolName:      "plain",
			namePath:      "name",
			namespacePath: "namespace",
			wantName:      "plain",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SetResponsesToolCallIdentity([]byte(test.input), test.toolName, test.namespace, test.itemPath)
			if actual := gjson.GetBytes(got, test.namePath).String(); actual != test.wantName {
				t.Fatalf("name = %q, want %q; output=%s", actual, test.wantName, got)
			}
			namespace := gjson.GetBytes(got, test.namespacePath)
			if namespace.Exists() != test.wantNamespaceExists {
				t.Fatalf("namespace exists = %t, want %t; output=%s", namespace.Exists(), test.wantNamespaceExists, got)
			}
			if test.wantNamespaceExists && namespace.String() != test.wantNamespace {
				t.Fatalf("namespace = %q, want %q; output=%s", namespace.String(), test.wantNamespace, got)
			}
		})
	}
}

func TestNormalizeAnthropicResponsePreservesValidatedJSONObject(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"role":"assistant","type":"message","content":[{"text":"ok","type":"text"}],"usage":{"output_tokens":1,"input_tokens":2}}`)
	got, err := NormalizeAnthropicResponse(raw)
	if err != nil {
		t.Fatalf("NormalizeAnthropicResponse() error = %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("response was re-encoded: got %s, want %s", got, raw)
	}
}

func TestNormalizeAnthropicResponseRejectsNonMessageType(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"error","role":"assistant","content":[]}`)
	if _, err := NormalizeAnthropicResponse(raw); err == nil {
		t.Fatal("NormalizeAnthropicResponse accepted a non-message envelope")
	}
}
