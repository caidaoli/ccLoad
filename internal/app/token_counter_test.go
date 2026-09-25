package app

import (
	"net/http"
	"strings"
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

	t.Run("any model name is estimated", func(t *testing.T) {
		for _, model := range []string{"glm-4.6", "deepseek-v3.2", "kimi-k2", "qwen3-coder-plus"} {
			c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/v1/messages/count_tokens",
				[]byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
			srv.handleCountTokens(c)
			if w.Code != http.StatusOK {
				t.Fatalf("model %s status=%d, want %d, body=%s", model, w.Code, http.StatusOK, w.Body.String())
			}
		}
	})

	// 数组 tool_result 与文本文档按实际内容估算：Claude Code 读大文件后要能看到上下文涨了。
	t.Run("array tool_result and text document scale with content", func(t *testing.T) {
		longText := strings.Repeat("lorem ipsum dolor sit amet ", 800) // ~21.6K 字符，约 5.4K token
		for name, block := range map[string]any{
			"tool_result array": map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
				map[string]any{"type": "text", "text": longText},
			}},
			"text document": map[string]any{"type": "document", "source": map[string]any{
				"type": "text", "media_type": "text/plain", "data": longText,
			}},
			"content document": map[string]any{"type": "document", "source": map[string]any{
				"type": "content", "content": []any{map[string]any{"type": "text", "text": longText}},
			}},
		} {
			payload := map[string]any{
				"model":    "claude-sonnet-4-6",
				"messages": []any{map[string]any{"role": "user", "content": []any{block}}},
			}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/v1/messages/count_tokens", payload))
			srv.handleCountTokens(c)
			var resp CountTokensResponse
			mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
			if resp.InputTokens < 5_000 {
				t.Fatalf("%s: InputTokens=%d, want >=5000", name, resp.InputTokens)
			}
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
