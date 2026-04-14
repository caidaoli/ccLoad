package protocol_test

import (
	"context"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func TestRegistryCodexGeminiStream(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("message done fallback emits text when no delta arrived", func(t *testing.T) {
		var state any
		out, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Gemini,
			"gemini-2.5-pro",
			nil,
			nil,
			[]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\n"),
			&state,
		)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		if len(out) != 1 || !strings.Contains(string(out[0]), `"text":"ok"`) {
			t.Fatalf("unexpected gemini chunks: %#v", out)
		}
	})

	t.Run("response created seeds completion metadata", func(t *testing.T) {
		var state any
		if _, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Gemini,
			"gemini-2.5-pro",
			nil,
			nil,
			[]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\"}}\n\n"),
			&state,
		); err != nil {
			t.Fatalf("response.created failed: %v", err)
		}
		done, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Gemini,
			"gemini-2.5-pro",
			nil,
			nil,
			[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"),
			&state,
		)
		if err != nil {
			t.Fatalf("response.completed failed: %v", err)
		}
		if len(done) != 1 || !strings.Contains(string(done[0]), `"responseId":"resp_1"`) || !strings.Contains(string(done[0]), `"finishReason":"STOP"`) {
			t.Fatalf("unexpected completion chunk: %#v", done)
		}
	})
}
