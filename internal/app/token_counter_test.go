package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

func countTokensBody(t *testing.T, payload any) []byte {
	t.Helper()
	body, err := sonic.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

func TestLocalCountTokens(t *testing.T) {
	t.Run("invalid json", func(t *testing.T) {
		if status, _, inputTokens := localCountTokens([]byte(`{`)); status != http.StatusBadRequest || inputTokens != 0 {
			t.Fatalf("status=%d inputTokens=%d, want 400 and 0", status, inputTokens)
		}
	})

	t.Run("any model name is estimated", func(t *testing.T) {
		for _, model := range []string{"glm-4.6", "deepseek-v3.2", "kimi-k2", "qwen3-coder-plus"} {
			status, payload, _ := localCountTokens([]byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`))
			if status != http.StatusOK {
				t.Fatalf("model %s status=%d, want %d, payload=%v", model, status, http.StatusOK, payload)
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
			if _, _, inputTokens := localCountTokens(countTokensBody(t, payload)); inputTokens < 5_000 {
				t.Fatalf("%s: InputTokens=%d, want >=5000", name, inputTokens)
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
		status, resp, inputTokens := localCountTokens(countTokensBody(t, payload))
		if status != http.StatusOK || inputTokens <= 0 || resp != (CountTokensResponse{InputTokens: inputTokens}) {
			t.Fatalf("status=%d inputTokens=%d resp=%v", status, inputTokens, resp)
		}
	})
}
