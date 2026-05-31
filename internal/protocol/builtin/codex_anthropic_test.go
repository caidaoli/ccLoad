package builtin

import (
	"context"
	"strings"
	"testing"
)

func TestCodexAnthropicStreamPrefersSSEEventHeader(t *testing.T) {
	var state any
	chunks, err := convertCodexResponseToAnthropicStream(
		context.Background(),
		"gpt-5-codex",
		nil,
		nil,
		[]byte("event: response.completed\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"bad\"}\n\n"),
		&state,
	)
	if err != nil {
		t.Fatalf("convertCodexResponseToAnthropicStream failed: %v", err)
	}
	got := stringJoinChunks(chunks)
	if !strings.Contains(got, "event: message_delta") || !strings.Contains(got, "event: message_stop") {
		t.Fatalf("SSE event header should drive completion handling, got:\n%s", got)
	}
	if strings.Contains(got, "bad") || strings.Contains(got, "content_block_delta") {
		t.Fatalf("payload type incorrectly overrode SSE event header, got:\n%s", got)
	}
}

func stringJoinChunks(chunks [][]byte) string {
	var b strings.Builder
	for _, chunk := range chunks {
		b.Write(chunk)
	}
	return b.String()
}
