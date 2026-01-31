package app

import (
	"net/http"
	"testing"
)

func TestHandleCountTokens(t *testing.T) {
	srv := &Server{}

	t.Run("invalid json", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/v1/messages/count_tokens", []byte(`{`)))

		srv.handleCountTokens(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid model", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/v1/messages/count_tokens", []byte(`{"model":"bad","messages":[{"role":"user","content":"hi"}]}`)))

		srv.handleCountTokens(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("success mixed content and tools", func(t *testing.T) {
		payload := map[string]any{
			"model": "claude-3-5-sonnet-latest",
			"system": []any{
				map[string]any{"type": "text", "text": "你是一个助手"},
			},
			"messages": []any{
				map[string]any{"role": "user", "content": "hello world"},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "text", "text": "你好"},
					map[string]any{"type": "image"},
					map[string]any{"type": "tool_use", "input": map[string]any{"a": 1}},
				}},
			},
			"tools": []any{
				map[string]any{
					"name":        "mcp__Playwright__browser_navigate_back",
					"description": "navigate back",
					"input_schema": map[string]any{
						"$schema": "http://json-schema.org/draft-07/schema#",
						"type":    "object",
					},
				},
			},
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/v1/messages/count_tokens", payload))

		srv.handleCountTokens(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp CountTokensResponse
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if resp.InputTokens <= 0 {
			t.Fatalf("InputTokens=%d, want >0", resp.InputTokens)
		}
	})
}

func TestIsValidClaudeModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"claude-3-5-sonnet-latest", true},
		{"gpt-4o", true},
		{"chatgpt-4o-latest", true},
		{"o1", true},
		{"o3-mini", true},
		{"o4", true},
		{"gemini-1.5-pro", true},
		{"text-davinci-003", true},
		{"anthropic.claude-v2", true},
		{"", false},
		{"bad", false},
	}

	for _, tt := range tests {
		if got := isValidClaudeModel(tt.model); got != tt.want {
			t.Fatalf("isValidClaudeModel(%q)=%v, want %v", tt.model, got, tt.want)
		}
	}
}
