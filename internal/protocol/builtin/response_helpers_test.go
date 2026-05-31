package builtin

import (
	"context"
	"strings"
	"testing"
)

func TestMapAnthropicStopReasonToOpenAIUsesActualToolCalls(t *testing.T) {
	if got := mapAnthropicStopReasonToOpenAI("tool_use", false); got != "stop" {
		t.Fatalf("tool_use without tool blocks mapped to %q, want stop", got)
	}
	if got := mapAnthropicStopReasonToOpenAI("tool_use", true); got != "tool_calls" {
		t.Fatalf("tool_use with tool blocks mapped to %q, want tool_calls", got)
	}
}

func TestAnthropicOpenAIStreamToolUseStopReasonNeedsToolBlock(t *testing.T) {
	var state any
	chunks, err := convertAnthropicResponseToOpenAIStream(
		context.Background(),
		"gpt-4o",
		nil,
		nil,
		[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n"),
		&state,
	)
	if err != nil {
		t.Fatalf("convertAnthropicResponseToOpenAIStream failed: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected one finish chunk, got %#v", chunks)
	}
	got := string(chunks[0])
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason should be stop without a tool block, got %s", got)
	}
}
