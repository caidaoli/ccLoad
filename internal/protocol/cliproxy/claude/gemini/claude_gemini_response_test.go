package gemini

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestConvertClaudeResponseToGemini_StreamPreservesMaxTokensFinishReason(t *testing.T) {
	ctx := context.Background()
	var param any
	chunk := []byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":10,"output_tokens":100}}`)

	out := ConvertClaudeResponseToGemini(ctx, "claude-sonnet-4-5", nil, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected one output chunk, got %d", len(out))
	}
	if got := ojson.GetBytesNoCopy(out[0], "candidates.0.finishReason").String(); got != "MAX_TOKENS" {
		t.Fatalf("finishReason = %q, want MAX_TOKENS; output=%s", got, out[0])
	}
}

func TestConvertClaudeResponseToGemini_StreamWithoutStopReasonDefaultsToStop(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
	}{
		{
			name:  "empty delta",
			chunk: `data: {"type":"message_delta","delta":{},"usage":{"input_tokens":10,"output_tokens":100}}`,
		},
		{
			name:  "missing delta",
			chunk: `data: {"type":"message_delta","usage":{"input_tokens":10,"output_tokens":100}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			var param any
			out := ConvertClaudeResponseToGemini(ctx, "claude-sonnet-4-5", nil, nil, []byte(tt.chunk), &param)
			if len(out) != 1 {
				t.Fatalf("expected one output chunk, got %d", len(out))
			}
			if got := ojson.GetBytesNoCopy(out[0], "candidates.0.finishReason").String(); got != "STOP" {
				t.Fatalf("finishReason = %q, want STOP; output=%s", got, out[0])
			}
		})
	}
}

func TestConvertClaudeResponseToGemini_StreamPreservesToolUseID(t *testing.T) {
	ctx := context.Background()
	var param any

	start := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_gateway","name":"lookup"}}`)
	out := ConvertClaudeResponseToGemini(ctx, "gemini-2.5-pro", nil, nil, start, &param)
	if len(out) != 0 {
		t.Fatalf("expected content_block_start to be buffered, got %d chunks", len(out))
	}

	delta := []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"status\"}"}}`)
	out = ConvertClaudeResponseToGemini(ctx, "gemini-2.5-pro", nil, nil, delta, &param)
	if len(out) != 0 {
		t.Fatalf("expected input_json_delta to be buffered, got %d chunks", len(out))
	}

	stop := []byte(`data: {"type":"content_block_stop","index":0}`)
	out = ConvertClaudeResponseToGemini(ctx, "gemini-2.5-pro", nil, nil, stop, &param)
	if len(out) != 1 {
		t.Fatalf("expected content_block_stop to emit 1 chunk, got %d", len(out))
	}

	got := ojson.GetBytesNoCopy(out[0], "candidates.0.content.parts.0.functionCall.id").String()
	if got != "toolu_gateway" {
		t.Fatalf("expected functionCall.id %q, got %q; chunk=%s", "toolu_gateway", got, string(out[0]))
	}
}

func TestConvertClaudeResponseToGeminiNonStreamPreservesToolUseID(t *testing.T) {
	ctx := context.Background()
	raw := []byte(strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_gateway","name":"lookup"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"status\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	}, "\n"))

	out := ConvertClaudeResponseToGeminiNonStream(ctx, "gemini-2.5-pro", nil, nil, raw, nil)

	got := ojson.GetBytesNoCopy(out, "candidates.0.content.parts.0.functionCall.id").String()
	if got != "toolu_gateway" {
		t.Fatalf("expected functionCall.id %q, got %q; chunk=%s", "toolu_gateway", got, string(out))
	}
}

func TestConvertClaudeResponseToGemini_StreamThinkingSignature(t *testing.T) {
	const validGeminiSignature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"

	tests := []struct {
		name          string
		signature     string
		wantSignature string
	}{
		{
			name:          "foreign claude signature is omitted",
			signature:     "foreign_claude_sig_123",
			wantSignature: "",
		},
		{
			name:          "preserves valid gemini signature",
			signature:     "gemini#" + validGeminiSignature,
			wantSignature: validGeminiSignature,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			var param any

			chunks := [][]byte{
				[]byte(`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-7-sonnet-20250219"}}`),
				[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
				[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thinking text"}}`),
				[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + tt.signature + `"}}`),
				[]byte(`data: {"type":"content_block_stop","index":0}`),
				[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
				[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"final answer"}}`),
				[]byte(`data: {"type":"content_block_stop","index":1}`),
				[]byte(`data: {"type":"message_stop"}`),
			}

			var emittedParts []ojson.Result
			for _, chunk := range chunks {
				out := ConvertClaudeResponseToGemini(ctx, "gemini-2.5-pro", nil, nil, chunk, &param)
				for _, c := range out {
					parts := ojson.GetBytesNoCopy(c, "candidates.0.content.parts").Array()
					emittedParts = append(emittedParts, parts...)
				}
			}

			var foundSignature string
			for _, p := range emittedParts {
				if p.Get("thought").Bool() && p.Get("thoughtSignature").Exists() {
					foundSignature = p.Get("thoughtSignature").String()
				}
			}

			if foundSignature != tt.wantSignature {
				t.Fatalf("expected thoughtSignature %q, got %q", tt.wantSignature, foundSignature)
			}
		})
	}
}

func TestConvertClaudeResponseToGeminiNonStream_ThinkingSignature(t *testing.T) {
	const validGeminiSignature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"

	tests := []struct {
		name          string
		signature     string
		wantSignature string
	}{
		{
			name:          "foreign claude signature is omitted",
			signature:     "foreign_claude_sig_123",
			wantSignature: "",
		},
		{
			name:          "preserves valid gemini signature",
			signature:     "gemini#" + validGeminiSignature,
			wantSignature: validGeminiSignature,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			raw := []byte(strings.Join([]string{
				`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-7-sonnet-20250219"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thinking text"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + tt.signature + `"}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"final answer"}}`,
				`data: {"type":"content_block_stop","index":1}`,
				`data: {"type":"message_stop"}`,
			}, "\n"))

			out := ConvertClaudeResponseToGeminiNonStream(ctx, "gemini-2.5-pro", nil, nil, raw, nil)

			thoughtPart := ojson.GetBytesNoCopy(out, "candidates.0.content.parts.0")
			if !thoughtPart.Get("thought").Bool() || thoughtPart.Get("text").String() != "thinking text" {
				t.Fatalf("expected thought part with text 'thinking text', got %s", thoughtPart.Raw)
			}
			if got := thoughtPart.Get("thoughtSignature").String(); got != tt.wantSignature {
				t.Fatalf("expected thoughtSignature %q, got %q", tt.wantSignature, got)
			}

			textPart := ojson.GetBytesNoCopy(out, "candidates.0.content.parts.1")
			if textPart.Get("text").String() != "final answer" {
				t.Fatalf("expected text part 'final answer', got %s", textPart.Raw)
			}
		})
	}
}

func TestConvertClaudeResponseToGeminiNonStream_NativeMessagesJSON(t *testing.T) {
	t.Run("thinking text max_tokens", func(t *testing.T) {
		raw := []byte(`{"id":"msg_01abc","type":"message","role":"assistant","model":"claude-sonnet-4.5","content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"Hello world"}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":100}}`)

		out := ConvertClaudeResponseToGeminiNonStream(context.Background(), "claude-sonnet-4.5", nil, nil, raw, nil)
		if !ojson.ValidBytes(out) {
			t.Fatalf("invalid JSON output: %s", out)
		}

		if got := ojson.GetBytesNoCopy(out, "responseId").String(); got != "msg_01abc" {
			t.Errorf("responseId = %q, want %q; output=%s", got, "msg_01abc", out)
		}
		if got := ojson.GetBytesNoCopy(out, "createTime").String(); got == "" {
			t.Errorf("createTime is empty; output=%s", out)
		}
		if got := ojson.GetBytesNoCopy(out, "modelVersion").String(); got != "claude-sonnet-4.5" {
			t.Errorf("modelVersion = %q, want %q", got, "claude-sonnet-4.5")
		}
		if got := ojson.GetBytesNoCopy(out, "candidates.0.finishReason").String(); got != "MAX_TOKENS" {
			t.Errorf("finishReason = %q, want MAX_TOKENS; output=%s", got, out)
		}

		parts := ojson.GetBytesNoCopy(out, "candidates.0.content.parts").Array()
		if len(parts) != 2 {
			t.Fatalf("len(parts) = %d, want 2; output=%s", len(parts), out)
		}
		if !parts[0].Get("thought").Bool() || parts[0].Get("text").String() != "plan" {
			t.Errorf("parts[0] = %s, want thought part with text 'plan'", parts[0].Raw)
		}
		if parts[1].Get("thought").Exists() || parts[1].Get("text").String() != "Hello world" {
			t.Errorf("parts[1] = %s, want text part 'Hello world'", parts[1].Raw)
		}

		if got := ojson.GetBytesNoCopy(out, "usageMetadata.promptTokenCount").Int(); got != 25 {
			t.Errorf("promptTokenCount = %d, want 25", got)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.candidatesTokenCount").Int(); got != 100 {
			t.Errorf("candidatesTokenCount = %d, want 100", got)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.totalTokenCount").Int(); got != 125 {
			t.Errorf("totalTokenCount = %d, want 125", got)
		}
	})

	t.Run("tool_use and cache usage", func(t *testing.T) {
		raw := []byte(`{"id":"msg_tool","type":"message","role":"assistant","model":"claude-opus-4.5","content":[{"type":"text","text":"Checking weather"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Tokyo"}},{"type":"tool_use","id":"toolu_2","name":"get_time","input":{}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":30,"cache_creation_input_tokens":4,"cache_read_input_tokens":6,"output_tokens":15}}`)

		out := ConvertClaudeResponseToGeminiNonStream(context.Background(), "claude-opus-4.5", nil, nil, raw, nil)
		if !ojson.ValidBytes(out) {
			t.Fatalf("invalid JSON output: %s", out)
		}

		if got := ojson.GetBytesNoCopy(out, "responseId").String(); got != "msg_tool" {
			t.Errorf("responseId = %q, want %q", got, "msg_tool")
		}
		if got := ojson.GetBytesNoCopy(out, "candidates.0.finishReason").String(); got != "STOP" {
			t.Errorf("finishReason = %q, want STOP", got)
		}

		parts := ojson.GetBytesNoCopy(out, "candidates.0.content.parts").Array()
		if len(parts) != 3 {
			t.Fatalf("len(parts) = %d, want 3; output=%s", len(parts), out)
		}
		if got := parts[0].Get("text").String(); got != "Checking weather" {
			t.Errorf("parts[0].text = %q, want %q", got, "Checking weather")
		}
		if got := parts[1].Get("functionCall.id").String(); got != "toolu_1" {
			t.Errorf("parts[1].functionCall.id = %q, want toolu_1", got)
		}
		if got := parts[1].Get("functionCall.name").String(); got != "get_weather" {
			t.Errorf("parts[1].functionCall.name = %q, want get_weather", got)
		}
		if got := parts[1].Get("functionCall.args.city").String(); got != "Tokyo" {
			t.Errorf("parts[1].functionCall.args.city = %q, want Tokyo", got)
		}
		if got := parts[2].Get("functionCall.id").String(); got != "toolu_2" {
			t.Errorf("parts[2].functionCall.id = %q, want toolu_2", got)
		}
		if got := parts[2].Get("functionCall.name").String(); got != "get_time" {
			t.Errorf("parts[2].functionCall.name = %q, want get_time", got)
		}
		if !parts[2].Get("functionCall.args").IsObject() {
			t.Errorf("parts[2].functionCall.args = %s, want JSON object", parts[2].Get("functionCall.args").Raw)
		}

		if got := ojson.GetBytesNoCopy(out, "usageMetadata.promptTokenCount").Int(); got != 40 {
			t.Errorf("promptTokenCount = %d, want 40", got)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.candidatesTokenCount").Int(); got != 15 {
			t.Errorf("candidatesTokenCount = %d, want 15", got)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.totalTokenCount").Int(); got != 55 {
			t.Errorf("totalTokenCount = %d, want 55", got)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.cachedContentTokenCount").Int(); got != 6 {
			t.Errorf("cachedContentTokenCount = %d, want 6", got)
		}
	})

	t.Run("nested thinking tokens do not double count output", func(t *testing.T) {
		raw := []byte(`{"id":"msg_think","type":"message","role":"assistant","model":"claude-sonnet-4.5","content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":100,"output_tokens_details":{"thinking_tokens":40}}}`)

		out := ConvertClaudeResponseToGeminiNonStream(context.Background(), "claude-sonnet-4.5", nil, nil, raw, nil)
		if !ojson.ValidBytes(out) {
			t.Fatalf("invalid JSON output: %s", out)
		}

		if got := ojson.GetBytesNoCopy(out, "usageMetadata.promptTokenCount").Int(); got != 20 {
			t.Errorf("promptTokenCount = %d, want 20; output=%s", got, out)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.candidatesTokenCount").Int(); got != 60 {
			t.Errorf("candidatesTokenCount = %d, want 60; output=%s", got, out)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.thoughtsTokenCount").Int(); got != 40 {
			t.Errorf("thoughtsTokenCount = %d, want 40; output=%s", got, out)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.totalTokenCount").Int(); got != 120 {
			t.Errorf("totalTokenCount = %d, want 120; output=%s", got, out)
		}
	})

	t.Run("thinking tokens exceeding output clamp candidates without inflating total", func(t *testing.T) {
		raw := []byte(`{"id":"msg_think_clamp","type":"message","role":"assistant","model":"claude-sonnet-4.5","content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":30,"output_tokens_details":{"thinking_tokens":40}}}`)

		out := ConvertClaudeResponseToGeminiNonStream(context.Background(), "claude-sonnet-4.5", nil, nil, raw, nil)
		if !ojson.ValidBytes(out) {
			t.Fatalf("invalid JSON output: %s", out)
		}

		if got := ojson.GetBytesNoCopy(out, "usageMetadata.promptTokenCount").Int(); got != 20 {
			t.Errorf("promptTokenCount = %d, want 20; output=%s", got, out)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.candidatesTokenCount").Int(); got != 0 {
			t.Errorf("candidatesTokenCount = %d, want 0; output=%s", got, out)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.thoughtsTokenCount").Int(); got != 40 {
			t.Errorf("thoughtsTokenCount = %d, want 40; output=%s", got, out)
		}
		if got := ojson.GetBytesNoCopy(out, "usageMetadata.totalTokenCount").Int(); got != 50 {
			t.Errorf("totalTokenCount = %d, want 50; output=%s", got, out)
		}
	})
}

func TestConvertClaudeResponseToGeminiNonStream_SSEPreservesMessageStartUsageAndMaxTokens(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_sse","model":"claude-sonnet-4-5","usage":{"input_tokens":25,"cache_creation_input_tokens":4,"cache_read_input_tokens":6}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Partial output"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":100}}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	out := ConvertClaudeResponseToGeminiNonStream(context.Background(), "claude-sonnet-4-5", nil, nil, raw, nil)
	if !ojson.ValidBytes(out) {
		t.Fatalf("invalid JSON output: %s", out)
	}

	if got := ojson.GetBytesNoCopy(out, "responseId").String(); got != "msg_sse" {
		t.Errorf("responseId = %q, want msg_sse", got)
	}
	if got := ojson.GetBytesNoCopy(out, "candidates.0.content.parts.0.text").String(); got != "Partial output" {
		t.Errorf("parts.0.text = %q, want %q", got, "Partial output")
	}
	if got := ojson.GetBytesNoCopy(out, "candidates.0.finishReason").String(); got != "MAX_TOKENS" {
		t.Errorf("finishReason = %q, want MAX_TOKENS; output=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "usageMetadata.promptTokenCount").Int(); got != 35 {
		t.Errorf("promptTokenCount = %d, want 35; output=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "usageMetadata.candidatesTokenCount").Int(); got != 100 {
		t.Errorf("candidatesTokenCount = %d, want 100; output=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "usageMetadata.totalTokenCount").Int(); got != 135 {
		t.Errorf("totalTokenCount = %d, want 135; output=%s", got, out)
	}
	if got := ojson.GetBytesNoCopy(out, "usageMetadata.cachedContentTokenCount").Int(); got != 6 {
		t.Errorf("cachedContentTokenCount = %d, want 6; output=%s", got, out)
	}
}

func TestConvertClaudeResponseToGemini_StreamToolUseOmitsPrematureFinishReason(t *testing.T) {
	t.Run("parallel tool calls emit finishReason only on message_delta", func(t *testing.T) {
		ctx := context.Background()
		var param any

		events := []string{
			`data: {"type":"message_start","message":{"id":"msg_parallel","model":"claude-sonnet-4-5"}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"a\"}"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"fetch"}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"id\":2}"}}`,
			`data: {"type":"content_block_stop","index":1}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":12,"output_tokens":34}}`,
			`data: {"type":"message_stop"}`,
		}

		var chunks [][]byte
		for _, event := range events {
			chunks = append(chunks, ConvertClaudeResponseToGemini(ctx, "claude-sonnet-4-5", nil, nil, []byte(event), &param)...)
		}

		if len(chunks) != 3 {
			t.Fatalf("expected 3 output chunks (2 tool calls + 1 message_delta), got %d", len(chunks))
		}

		for i, want := range []struct {
			id   string
			name string
		}{
			{id: "toolu_1", name: "lookup"},
			{id: "toolu_2", name: "fetch"},
		} {
			if fr := ojson.GetBytesNoCopy(chunks[i], "candidates.0.finishReason"); fr.Exists() {
				t.Errorf("chunk[%d] unexpectedly set finishReason=%q; chunk=%s", i, fr.String(), chunks[i])
			}
			if got := ojson.GetBytesNoCopy(chunks[i], "candidates.0.content.parts.0.functionCall.id").String(); got != want.id {
				t.Errorf("chunk[%d] functionCall.id = %q, want %q", i, got, want.id)
			}
			if got := ojson.GetBytesNoCopy(chunks[i], "candidates.0.content.parts.0.functionCall.name").String(); got != want.name {
				t.Errorf("chunk[%d] functionCall.name = %q, want %q", i, got, want.name)
			}
		}

		if got := ojson.GetBytesNoCopy(chunks[2], "candidates.0.finishReason").String(); got != "STOP" {
			t.Errorf("final chunk finishReason = %q, want STOP; chunk=%s", got, chunks[2])
		}
		if got := ojson.GetBytesNoCopy(chunks[2], "usageMetadata.promptTokenCount").Int(); got != 12 {
			t.Errorf("final chunk promptTokenCount = %d, want 12; chunk=%s", got, chunks[2])
		}
		if got := ojson.GetBytesNoCopy(chunks[2], "usageMetadata.candidatesTokenCount").Int(); got != 34 {
			t.Errorf("final chunk candidatesTokenCount = %d, want 34; chunk=%s", got, chunks[2])
		}
	})

	t.Run("tool_use followed by max_tokens emits only MAX_TOKENS finishReason", func(t *testing.T) {
		ctx := context.Background()
		var param any

		events := []string{
			`data: {"type":"message_start","message":{"id":"msg_max","model":"claude-sonnet-4-5"}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"a\"}"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":10,"output_tokens":50}}`,
			`data: {"type":"message_stop"}`,
		}

		var chunks [][]byte
		for _, event := range events {
			chunks = append(chunks, ConvertClaudeResponseToGemini(ctx, "claude-sonnet-4-5", nil, nil, []byte(event), &param)...)
		}

		if len(chunks) != 2 {
			t.Fatalf("expected 2 output chunks (1 tool call + 1 message_delta), got %d", len(chunks))
		}
		if fr := ojson.GetBytesNoCopy(chunks[0], "candidates.0.finishReason"); fr.Exists() {
			t.Errorf("tool_use chunk unexpectedly set finishReason=%q; chunk=%s", fr.String(), chunks[0])
		}
		if got := ojson.GetBytesNoCopy(chunks[1], "candidates.0.finishReason").String(); got != "MAX_TOKENS" {
			t.Errorf("final chunk finishReason = %q, want MAX_TOKENS; chunk=%s", got, chunks[1])
		}
	})
}

func TestConvertClaudeResponseToGemini_ClaudeSSEFinishReason(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name  string
		delta string
		want  string
	}{
		{"truncated", `{"stop_reason":"max_tokens"}`, "MAX_TOKENS"},
		{"success", `{"stop_reason":"end_turn"}`, "STOP"},
		{"omitted", `{}`, "STOP"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			event := `data: {"type":"message_delta","delta":` + tt.delta + `,"usage":{"output_tokens":2}}` + "\n\n"
			var state any
			out := ConvertClaudeResponseToGemini(ctx, "gemini-3-flash-preview", nil, nil, []byte(event), &state)
			if len(out) != 1 || ojson.GetBytesNoCopy(out[0], "candidates.0.finishReason").String() != tt.want {
				t.Fatalf("stream finishReason should be %q, got %q", tt.want, out)
			}
			event += "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
			aggregate := ConvertClaudeResponseToGeminiNonStream(ctx, "gemini-3-flash-preview", nil, nil, []byte(event), nil)
			if got := ojson.GetBytesNoCopy(aggregate, "candidates.0.finishReason").String(); got != tt.want {
				t.Fatalf("aggregated finishReason=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestConvertClaudeResponseToGemini_ThinkingSignature(t *testing.T) {
	const validGeminiSignature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"
	tests := []struct {
		name      string
		signature string
		want      string
	}{
		{name: "foreign Claude", signature: "foreign_claude_sig_123"},
		{name: "foreign base64", signature: "Y2xhdWRlLW9wYXF1ZS1zaWduYXR1cmU="},
		{name: "bypass sentinel", signature: "skip_thought_signature_validator"},
		{name: "invalid native base64", signature: "gemini#invalid@bytes"},
		{name: "native Gemini", signature: "gemini#" + validGeminiSignature, want: validGeminiSignature},
		{name: "unprefixed Gemini", signature: validGeminiSignature, want: validGeminiSignature},
		{name: "unpadded Gemini", signature: "gemini#EggKBgEMOdbHNA", want: "EggKBgEMOdbHNA=="},
		{name: "padded Gemini", signature: "gemini#EggKBgEMOdbHNA==", want: "EggKBgEMOdbHNA=="},
	}
	for _, tt := range tests {
		for _, format := range []string{"stream start", "stream delta", "SSE start", "SSE delta", "message"} {
			t.Run(tt.name+"/"+format, func(t *testing.T) {
				startSignature, deltaSignature := "", ""
				if strings.HasSuffix(format, "start") {
					startSignature = tt.signature
				} else {
					deltaSignature = tt.signature
				}
				events := []string{
					`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-test"}}`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"` + startSignature + `"}}`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thinking text"}}`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + deltaSignature + `"}}`,
					`data: {"type":"content_block_stop","index":0}`,
					`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
					`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"final answer"}}`,
					`data: {"type":"content_block_stop","index":1}`,
					`data: {"type":"message_stop"}`,
				}
				var outputs [][]byte
				switch {
				case strings.HasPrefix(format, "stream"):
					var param any
					for _, event := range events {
						outputs = append(outputs, ConvertClaudeResponseToGemini(context.Background(), "gemini-2.5-pro", nil, nil, []byte(event), &param)...)
					}
				case strings.HasPrefix(format, "SSE"):
					outputs = append(outputs, ConvertClaudeResponseToGeminiNonStream(context.Background(), "gemini-2.5-pro", nil, nil, []byte(strings.Join(events, "\n")), nil))
				case format == "message":
					raw := `{"type":"message","id":"msg_123","content":[{"type":"thinking","thinking":"thinking text","signature":"` + tt.signature + `"},{"type":"text","text":"final answer"}],"stop_reason":"end_turn"}`
					outputs = append(outputs, ConvertClaudeResponseToGeminiNonStream(context.Background(), "gemini-2.5-pro", nil, nil, []byte(raw), nil))
				}
				var thoughtText, answer strings.Builder
				var foundSignature string
				for _, out := range outputs {
					for _, part := range ojson.GetBytesNoCopy(out, "candidates.0.content.parts").Array() {
						if part.Get("thought").Bool() {
							thoughtText.WriteString(part.Get("text").String())
						} else {
							answer.WriteString(part.Get("text").String())
						}
						if signature := part.Get("thoughtSignature"); signature.Exists() {
							if tt.want == "" {
								t.Fatalf("incompatible signature was emitted: %s", part.Raw)
							}
							foundSignature = signature.String()
							if _, err := base64.StdEncoding.DecodeString(foundSignature); err != nil {
								t.Fatalf("thoughtSignature %q is not valid base64: %v; response=%s", foundSignature, err, out)
							}
						}
					}
				}
				if thoughtText.String() != "thinking text" || answer.String() != "final answer" {
					t.Fatalf("thought=%q answer=%q; expected both texts to survive signature filtering", thoughtText.String(), answer.String())
				}
				if foundSignature != tt.want {
					t.Fatalf("thoughtSignature = %q, want %q", foundSignature, tt.want)
				}
			})
		}
	}
}

func TestConvertClaudeResponseToGemini_RedactedThinkingSignature(t *testing.T) {
	tests := []struct {
		name      string
		signature string
		want      string
	}{
		{name: "foreign Claude", signature: "opaque"},
		{name: "invalid native", signature: "gemini#invalid@bytes"},
		{name: "native Gemini", signature: "gemini#EggKBgEMOdbHNA", want: "EggKBgEMOdbHNA=="},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(tt.name+map[bool]string{false: "/message", true: "/stream"}[stream], func(t *testing.T) {
				var outputs [][]byte
				if stream {
					var param any
					for _, event := range []string{
						`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"` + tt.signature + `"}}`,
						`data: {"type":"content_block_stop","index":0}`,
						`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"final answer"}}`,
					} {
						outputs = append(outputs, ConvertClaudeResponseToGemini(context.Background(), "gemini-2.5-pro", nil, nil, []byte(event), &param)...)
					}
				} else {
					raw := `{"type":"message","content":[{"type":"redacted_thinking","data":"` + tt.signature + `"},{"type":"text","text":"final answer"}]}`
					outputs = append(outputs, ConvertClaudeResponseToGeminiNonStream(context.Background(), "gemini-2.5-pro", nil, nil, []byte(raw), nil))
				}
				var foundSignature, answer string
				for _, out := range outputs {
					for _, part := range ojson.GetBytesNoCopy(out, "candidates.0.content.parts").Array() {
						if part.Get("thought").Bool() {
							foundSignature = part.Get("thoughtSignature").String()
							if foundSignature == "" {
								t.Fatalf("redacted thinking emitted an empty part: %s", part.Raw)
							}
						} else {
							answer += part.Get("text").String()
						}
					}
				}
				if foundSignature != tt.want || answer != "final answer" {
					t.Fatalf("thoughtSignature=%q answer=%q, want %q and final answer", foundSignature, answer, tt.want)
				}
			})
		}
	}
}

func TestConvertClaudeResponseToGemini_UsageAcrossResponseFormats(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name           string
		startUsage     string
		deltaUsage     string
		wantPrompt     int64
		wantCandidates int64
		wantTotal      int64
		wantCached     int64
		wantThoughts   int64
	}{
		{
			name:           "cache read and write with nested thinking",
			startUsage:     `"input_tokens":11,"cache_creation_input_tokens":13,"cache_read_input_tokens":17`,
			deltaUsage:     `"output_tokens":19,"output_tokens_details":{"thinking_tokens":7},"reasoning_tokens":5`,
			wantPrompt:     41,
			wantCandidates: 12,
			wantTotal:      60,
			wantCached:     17,
			wantThoughts:   7,
		},
		{
			name:           "cache write alone with legacy thinking",
			startUsage:     `"input_tokens":11,"cache_creation_input_tokens":13`,
			deltaUsage:     `"output_tokens":19,"thinking_tokens":5`,
			wantPrompt:     24,
			wantCandidates: 14,
			wantTotal:      43,
			wantCached:     -1,
			wantThoughts:   5,
		},
		{
			name:           "thinking beyond reported output",
			startUsage:     `"input_tokens":11,"cache_read_input_tokens":0`,
			deltaUsage:     `"output_tokens":2,"reasoning_tokens":5`,
			wantPrompt:     11,
			wantCandidates: 0,
			wantTotal:      13,
			wantCached:     0,
			wantThoughts:   5,
		},
	} {
		for _, wire := range []string{"message JSON", "aggregated SSE", "streaming SSE"} {
			t.Run(tt.name+"/"+wire, func(t *testing.T) {
				events := []string{
					`data: {"type":"message_start","message":{"id":"msg_a","model":"claude","usage":{` + tt.startUsage + `}}}`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
					`data: {"type":"content_block_stop","index":0}`,
					`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{` + tt.deltaUsage + `}}`,
					`data: {"type":"message_stop"}`,
				}
				var out []byte
				switch wire {
				case "message JSON":
					message := `{"id":"msg_a","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"max_tokens","usage":{` + tt.startUsage + "," + tt.deltaUsage + "}}"
					out = ConvertClaudeResponseToGeminiNonStream(ctx, "claude", nil, nil, []byte(message), nil)
				case "aggregated SSE":
					eventStream := strings.Join(events, "\n\n") + "\n\n"
					out = ConvertClaudeResponseToGeminiNonStream(ctx, "claude", nil, nil, []byte(eventStream), nil)
				case "streaming SSE":
					var state any
					for _, event := range events {
						for _, chunk := range ConvertClaudeResponseToGemini(ctx, "claude", nil, nil, []byte(event), &state) {
							if ojson.GetBytesNoCopy(chunk, "candidates.0.finishReason").Exists() {
								out = chunk
							}
						}
					}
				}

				if ojson.GetBytesNoCopy(out, "responseId").String() != "msg_a" ||
					ojson.GetBytesNoCopy(out, "candidates.0.finishReason").String() != "MAX_TOKENS" {
					t.Fatalf("expected response ID and truncation, got %s", out)
				}
				if wire != "streaming SSE" && ojson.GetBytesNoCopy(out, "candidates.0.content.parts.0.text").String() != "hello" {
					t.Fatalf("expected assistant text, got %s", out)
				}
				usage := ojson.GetBytesNoCopy(out, "usageMetadata")
				for field, want := range map[string]int64{
					"promptTokenCount":        tt.wantPrompt,
					"candidatesTokenCount":    tt.wantCandidates,
					"totalTokenCount":         tt.wantTotal,
					"cachedContentTokenCount": tt.wantCached,
					"thoughtsTokenCount":      tt.wantThoughts,
				} {
					got := usage.Get(field)
					if want == -1 && !got.Exists() {
						continue
					}
					if got.Type != ojson.Number || got.Int() != want {
						t.Errorf("%s = %s, want %d; response=%s", field, got.Raw, want, out)
					}
				}
			})
		}
	}
}
