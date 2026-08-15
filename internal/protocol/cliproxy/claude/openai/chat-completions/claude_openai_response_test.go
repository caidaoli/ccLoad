package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func assertCachedCreationTokens(t *testing.T, payload []byte, want int64) {
	t.Helper()

	got := gjson.GetBytes(payload, "usage.prompt_tokens_details.cached_creation_tokens")
	if !got.Exists() {
		t.Fatalf("expected cached_creation_tokens to exist, payload=%s", string(payload))
	}
	if got.Int() != want {
		t.Fatalf("expected cached_creation_tokens %d, got %d", want, got.Int())
	}
}

func TestConvertClaudeResponseToOpenAI_StreamUsageIncludesCachedTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertClaudeResponseToOpenAI(
		ctx,
		"claude-opus-4-6",
		nil,
		nil,
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":13,"output_tokens":4,"cache_read_input_tokens":22000,"cache_creation_input_tokens":31}}`),
		&param,
	)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gotPromptTokens := gjson.GetBytes(out[0], "usage.prompt_tokens").Int(); gotPromptTokens != 22044 {
		t.Fatalf("expected prompt_tokens %d, got %d", 22044, gotPromptTokens)
	}
	if gotCompletionTokens := gjson.GetBytes(out[0], "usage.completion_tokens").Int(); gotCompletionTokens != 4 {
		t.Fatalf("expected completion_tokens %d, got %d", 4, gotCompletionTokens)
	}
	if gotTotalTokens := gjson.GetBytes(out[0], "usage.total_tokens").Int(); gotTotalTokens != 22048 {
		t.Fatalf("expected total_tokens %d, got %d", 22048, gotTotalTokens)
	}
	if gotCachedTokens := gjson.GetBytes(out[0], "usage.prompt_tokens_details.cached_tokens").Int(); gotCachedTokens != 22000 {
		t.Fatalf("expected cached_tokens %d, got %d", 22000, gotCachedTokens)
	}
	assertCachedCreationTokens(t, out[0], 31)
}

func TestConvertClaudeResponseToOpenAI_StreamUsageMergesMessageStartUsage(t *testing.T) {
	ctx := context.Background()
	var param any

	ConvertClaudeResponseToOpenAI(
		ctx,
		"claude-opus-4-6",
		nil,
		nil,
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-opus-4-6","usage":{"input_tokens":13,"output_tokens":1,"cache_read_input_tokens":22000,"cache_creation_input_tokens":31}}}`),
		&param,
	)
	out := ConvertClaudeResponseToOpenAI(
		ctx,
		"claude-opus-4-6",
		nil,
		nil,
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`),
		&param,
	)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gotPromptTokens := gjson.GetBytes(out[0], "usage.prompt_tokens").Int(); gotPromptTokens != 22044 {
		t.Fatalf("expected prompt_tokens %d, got %d", 22044, gotPromptTokens)
	}
	if gotCompletionTokens := gjson.GetBytes(out[0], "usage.completion_tokens").Int(); gotCompletionTokens != 4 {
		t.Fatalf("expected completion_tokens %d, got %d", 4, gotCompletionTokens)
	}
	if gotTotalTokens := gjson.GetBytes(out[0], "usage.total_tokens").Int(); gotTotalTokens != 22048 {
		t.Fatalf("expected total_tokens %d, got %d", 22048, gotTotalTokens)
	}
	if gotCachedTokens := gjson.GetBytes(out[0], "usage.prompt_tokens_details.cached_tokens").Int(); gotCachedTokens != 22000 {
		t.Fatalf("expected cached_tokens %d, got %d", 22000, gotCachedTokens)
	}
	assertCachedCreationTokens(t, out[0], 31)
}

func TestConvertClaudeResponseToOpenAINonStream_UsageIncludesCachedTokens(t *testing.T) {
	rawJSON := []byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-opus-4-6","stop_reason":"end_turn","usage":{"input_tokens":13,"output_tokens":4,"cache_read_input_tokens":22000,"cache_creation_input_tokens":31}}`)

	out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "", nil, nil, rawJSON, nil)

	if gotPromptTokens := gjson.GetBytes(out, "usage.prompt_tokens").Int(); gotPromptTokens != 22044 {
		t.Fatalf("expected prompt_tokens %d, got %d", 22044, gotPromptTokens)
	}
	if gotCompletionTokens := gjson.GetBytes(out, "usage.completion_tokens").Int(); gotCompletionTokens != 4 {
		t.Fatalf("expected completion_tokens %d, got %d", 4, gotCompletionTokens)
	}
	if gotTotalTokens := gjson.GetBytes(out, "usage.total_tokens").Int(); gotTotalTokens != 22048 {
		t.Fatalf("expected total_tokens %d, got %d", 22048, gotTotalTokens)
	}
	if gotCachedTokens := gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int(); gotCachedTokens != 22000 {
		t.Fatalf("expected cached_tokens %d, got %d", 22000, gotCachedTokens)
	}
	assertCachedCreationTokens(t, out, 31)
}

func TestConvertClaudeResponseToOpenAINonStream_UsageIncludesCacheCreationAndReasoningTokens(t *testing.T) {
	rawJSON := []byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"sig"},{"type":"text","text":"ok"}],"model":"claude-opus-4-6","stop_reason":"end_turn","usage":{"input_tokens":13,"output_tokens":4,"cache_read_input_tokens":22000,"cache_creation_input_tokens":31,"reasoning_tokens":9}}`)

	out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "", nil, nil, rawJSON, nil)

	if gotPromptTokens := gjson.GetBytes(out, "usage.prompt_tokens").Int(); gotPromptTokens != 22044 {
		t.Fatalf("expected prompt_tokens %d, got %d", 22044, gotPromptTokens)
	}
	if gotCompletionTokens := gjson.GetBytes(out, "usage.completion_tokens").Int(); gotCompletionTokens != 4 {
		t.Fatalf("expected completion_tokens %d, got %d", 4, gotCompletionTokens)
	}
	if gotTotalTokens := gjson.GetBytes(out, "usage.total_tokens").Int(); gotTotalTokens != 22048 {
		t.Fatalf("expected total_tokens %d, got %d", 22048, gotTotalTokens)
	}
	if gotCachedTokens := gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int(); gotCachedTokens != 22000 {
		t.Fatalf("expected cached_tokens %d, got %d", 22000, gotCachedTokens)
	}
	if gotCacheCreationTokens := gjson.GetBytes(out, "usage.cache_creation_input_tokens").Int(); gotCacheCreationTokens != 31 {
		t.Fatalf("expected cache_creation_input_tokens %d, got %d", 31, gotCacheCreationTokens)
	}
	if gotReasoningTokens := gjson.GetBytes(out, "usage.completion_tokens_details.reasoning_tokens").Int(); gotReasoningTokens != 9 {
		t.Fatalf("expected reasoning_tokens %d, got %d", 9, gotReasoningTokens)
	}
}

func TestConvertClaudeResponseToOpenAI_RefusalStopReason(t *testing.T) {
	for _, stopReason := range []string{"refusal", "sensitive"} {
		t.Run(stopReason, func(t *testing.T) {
			var param any
			out := ConvertClaudeResponseToOpenAI(
				context.Background(),
				"claude-opus-4-6",
				nil,
				nil,
				[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"`+stopReason+`"},"usage":{"output_tokens":10}}`),
				&param,
			)
			if len(out) != 1 {
				t.Fatalf("expected 1 chunk, got %d", len(out))
			}
			if got := gjson.GetBytes(out[0], "choices.0.finish_reason").String(); got != "content_filter" {
				t.Fatalf("finish_reason = %q, want content_filter; payload=%s", got, string(out[0]))
			}
		})
	}
}

func TestConvertClaudeResponseToOpenAINonStream_RefusalStopReason(t *testing.T) {
	for _, stopReason := range []string{"refusal", "sensitive"} {
		t.Run(stopReason, func(t *testing.T) {
			rawJSON := []byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"blocked"}],"model":"claude-opus-4-6","stop_reason":"` + stopReason + `","usage":{"input_tokens":10,"output_tokens":20}}`)

			out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "", nil, nil, rawJSON, nil)
			if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "content_filter" {
				t.Fatalf("finish_reason = %q, want content_filter; payload=%s", got, string(out))
			}
		})
	}
}
