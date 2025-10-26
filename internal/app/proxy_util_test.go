package app

import "testing"

func TestDetectChannelTypeFromPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{name: "Gemini", path: "/v1beta/models/gemini-pro:streamGenerateContent", expected: "gemini"},
		{name: "OpenAIChat", path: "/v1/chat/completions", expected: "openai"},
		{name: "OpenAIEmbeddings", path: "/v1/embeddings", expected: "openai"},
		{name: "ClaudeMessages", path: "/v1/messages", expected: "anthropic"},
		{name: "ClaudeCountTokens", path: "/v1/messages/count_tokens", expected: "anthropic"},
		{name: "CodexResponses", path: "/v1/responses", expected: "codex"},
		{name: "Unknown", path: "/v2/internal", expected: ""},
	}

	for _, tt := range tests {
		if got := detectChannelTypeFromPath(tt.path); got != tt.expected {
			t.Errorf("%s: detectChannelTypeFromPath(%q) = %q, want %q", tt.name, tt.path, got, tt.expected)
		}
	}
}
