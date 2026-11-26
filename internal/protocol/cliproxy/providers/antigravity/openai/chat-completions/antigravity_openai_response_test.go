package chat_completions

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

func TestAntigravityMultiPartContent(t *testing.T) {
	for _, tc := range []struct {
		name, parts, text, reasoning string
	}{
		{"text", `[{"text":"Hello "},{"text":"world"}]`, "Hello world", ""},
		{"reasoning", `[{"text":"step one ","thought":true},{"text":"step two","thought":true}]`, "", "step one step two"},
		{"mixed", `[{"text":"first ","thought":true},{"text":"Hello "},{"text":"second","thought":true},{"text":"world"}]`, "Hello world", "first second"},
		{"text_signature", `[{"text":"answer"},{"text":"","thoughtSignature":"sig"}]`, "answer", ""},
		{"reasoning_signature", `[{"text":"thought","thought":true},{"text":"","thought":true,"thought_signature":"sig"}]`, "", "thought"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"response":{"candidates":[{"content":{"parts":` + tc.parts + `}}]}}`)
			var state any
			chunks := ConvertAntigravityResponseToOpenAI(context.Background(), "gemini-2.5-pro", nil, nil, raw, &state)
			if len(chunks) != 1 {
				t.Fatalf("expected one chunk, got %d", len(chunks))
			}
			for path, want := range map[string]string{"content": tc.text, "reasoning_content": tc.reasoning} {
				value := ojson.GetBytes(chunks[0], "choices.0.delta."+path)
				if value.String() != want || (want == "" && value.Type != ojson.Null) {
					t.Errorf("%s=%s, want %q: %s", path, value.Raw, want, chunks[0])
				}
			}
		})
	}
}

func TestFinishReasonToolCallsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Contains functionCall - should set SawToolCall = true
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_files","args":{"path":"."}}}]}}]}}`)
	result1 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Verify chunk1 has no finish_reason (null)
	if len(result1) != 1 {
		t.Fatalf("Expected 1 result from chunk1, got %d", len(result1))
	}
	fr1 := ojson.GetBytes(result1[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Errorf("Expected finish_reason to be null in chunk1, got: %v", fr1.String())
	}

	// Chunk 2: Contains finishReason STOP + usage (final chunk, no functionCall)
	// This simulates what the upstream sends AFTER the tool call chunk
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":20,"totalTokenCount":30}}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify chunk2 has finish_reason: "tool_calls" (not "stop")
	if len(result2) != 1 {
		t.Fatalf("Expected 1 result from chunk2, got %d", len(result2))
	}
	fr2 := ojson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr2 != "tool_calls" {
		t.Errorf("Expected finish_reason 'tool_calls', got: %s", fr2)
	}

	// Verify native_finish_reason is lowercase upstream value
	nfr2 := ojson.GetBytes(result2[0], "choices.0.native_finish_reason").String()
	if nfr2 != "stop" {
		t.Errorf("Expected native_finish_reason 'stop', got: %s", nfr2)
	}
}

func TestAntigravityChatStreamNullUsagePreservesPendingZero(t *testing.T) {
	var state any
	convert := func(chunk string) [][]byte {
		return ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, []byte(chunk), &state)
	}
	convert(`{"response":{"candidates":[{"content":{"parts":[{"text":"first"}]}}],"cpaUsageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"totalTokenCount":0}}}`)
	chunks := convert(`{"response":{"candidates":[{"content":{"parts":[{"text":"last"}]},"finishReason":" MAX_TOKENS "}],"usageMetadata":null,"cpaUsageMetadata":null}}`)
	if len(chunks) != 1 || ojson.GetBytes(chunks[0], "choices.0.delta.content").String() != "last" ||
		ojson.GetBytes(chunks[0], "choices.0.finish_reason").Raw != "null" || ojson.GetBytes(chunks[0], "usage").Exists() {
		t.Fatalf("null usage finalized the text delta: %q", chunks)
	}
	chunks = convert(`[DONE]`)
	if len(chunks) != 1 || ojson.GetBytes(chunks[0], "choices.0.finish_reason").String() != "length" ||
		ojson.GetBytes(chunks[0], "usage.total_tokens").Raw != "0" {
		t.Fatalf("expected one terminal with real zero usage: %q", chunks)
	}
	for _, payload := range []string{`{"response":{"usageMetadata":null}}`, `[DONE]`} {
		if got := convert(payload); len(got) != 0 {
			t.Fatalf("post-terminal payload %s emitted %q", payload, got)
		}
	}
}

func TestAntigravityChatNonStreamNullableFinishReasonAndUsage(t *testing.T) {
	input := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":null}],"usageMetadata":null}}`)
	out := ConvertAntigravityResponseToOpenAINonStream(t.Context(), "gemini", nil, nil, input, nil)
	result := ojson.ParseBytes(out)
	if result.Get("choices.0.finish_reason").Raw != "null" || result.Get("choices.0.native_finish_reason").Raw != "null" || result.Get("usage").Exists() {
		t.Fatalf("unexpected finish reason or usage: %s", out)
	}
}

func TestAntigravityStreamSeparatesToolDeltasAndKeepsLateUsage(t *testing.T) {
	var state any
	convert := func(chunk string) [][]byte {
		return ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, []byte(chunk), &state)
	}
	convert(`{"response":{"responseId":"resp-tools","modelVersion":"gemini-wire","createTime":"2026-11-25T00:00:00Z","candidates":[{"content":{"parts":[{"text":"calling tools"}]}}]}}`)
	payload := `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"one","args":{}}},{"functionCall":{"name":"two","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":6}}}`
	chunks := convert(payload)
	if len(chunks) != 2 {
		t.Fatalf("expected a tool delta and a terminal chunk, got %q", chunks)
	}
	delta, terminal := ojson.ParseBytes(chunks[0]), ojson.ParseBytes(chunks[1])
	if delta.Get("choices.0.delta.tool_calls.#").Int() != 2 || delta.Get("choices.0.finish_reason").Type != ojson.Null || delta.Get("usage").Exists() {
		t.Fatalf("expected both tool calls before finish_reason: %s", delta.Raw)
	}
	if terminal.Get("choices.0.finish_reason").String() != "tool_calls" || terminal.Get("choices.0.delta").Raw != `{}` || terminal.Get("usage.total_tokens").Int() != 6 {
		t.Fatalf("expected terminal reason and usage with empty delta: %s", terminal.Raw)
	}
	for _, chunk := range []ojson.Result{delta, terminal} {
		if chunk.Get("id").String() != "resp-tools" || chunk.Get("model").String() != "gemini-wire" || chunk.Get("created").Int() <= 0 {
			t.Fatalf("missing response metadata: %s", chunk.Raw)
		}
	}
	if chunks := convert(payload); len(chunks) != 0 {
		t.Fatalf("replayed terminal frame emitted output: %q", chunks)
	}
	chunks = convert(`{"response":{"usageMetadata":{"totalTokenCount":7}}}`)
	if len(chunks) != 1 || ojson.GetBytes(chunks[0], "choices").Raw != `[]` || ojson.GetBytes(chunks[0], "usage.total_tokens").Int() != 7 {
		t.Fatalf("expected only usage after finish_reason: %q", chunks)
	}
	if chunks := convert(`[DONE]`); len(chunks) != 0 {
		t.Fatalf("terminal reason repeated at [DONE]: %q", chunks)
	}
}

func TestConvertAntigravityResponseToOpenAI_StreamToolArguments(t *testing.T) {
	for _, tt := range []struct{ name, fields, want string }{
		{name: "parameterless", want: `{}`},
		{name: "explicit arguments", fields: `,"args":{"path":"."}`, want: `{"path":"."}`},
		{name: "incremental arguments", fields: `,"willContinue":true`, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_ping","name":"ping"` + tt.fields + `}}]}}]}}`)
			var state any
			chunks := ConvertAntigravityResponseToOpenAI(t.Context(), "gemini-3.8-flash-high", nil, nil, raw, &state)
			if len(chunks) != 1 {
				t.Fatalf("got %d chunks, want one", len(chunks))
			}
			call := ojson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0")
			args := call.Get("function.arguments")
			if call.Get("id").String() != "call_ping" || call.Get("function.name").String() != "ping" || args.Type != ojson.String || args.String() != tt.want {
				t.Fatalf("tool call = %s, want ping with arguments %q", call.Raw, tt.want)
			}
		})
	}
}

func TestAntigravityStreamKeepsToolArgumentContinuationsOnTheirCall(t *testing.T) {
	var state any
	frames := []string{
		`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","name":"lookup","partialArgs":[{"jsonPath":"$.city","stringValue":"Tok","willContinue":true}],"willContinue":true}},{"functionCall":{"id":"call_b","name":"lookup","args":{"units":"metric"},"willContinue":true}},{"functionCall":{"id":"call_c","name":"lookup","args":{"city":"Osaka"}}}]}}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"call_id":"call_b","args":{"city":"Paris"},"partial_args":[],"will_continue":false}},{"functionCall":{"id":"call_c"}}]}}]}}`,
		`{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"totalTokenCount":8}}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","partial_args":[{"json_path":"$.city","string_value":"yo"}],"willContinue":true}}]}}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","args":{ },"willContinue":false}}]},"finishReason":""}]}}`,
		`{"response":{"candidates":[{"finishReason":123}]}}`,
		`[DONE]`,
	}
	var arguments [3]strings.Builder
	var ids [3]string
	terminalCount := 0
	for frameIndex, frame := range frames {
		chunks := ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, []byte(frame), &state)
		if frameIndex == 2 && len(chunks) != 0 {
			t.Fatalf("usage and a finish reason arrived while a call was still open: %q", chunks)
		}
		for _, chunk := range chunks {
			payload := ojson.ParseBytes(chunk)
			choice := payload.Get("choices.0")
			if finish := choice.Get("finish_reason").String(); finish != "" {
				terminalCount++
				if frameIndex != len(frames)-1 || finish != "tool_calls" || choice.Get("native_finish_reason").String() != "max_tokens" ||
					choice.Get("delta").Raw != `{}` || payload.Get("usage.total_tokens").Int() != 8 {
					t.Fatalf("unexpected terminal chunk: %s", chunk)
				}
			}
			for _, call := range choice.Get("delta.tool_calls").Array() {
				idx := call.Get("index").Int()
				if idx < 0 || idx >= int64(len(arguments)) {
					t.Fatalf("a continuation created another tool: %s", chunk)
				}
				if id := call.Get("id").String(); id != "" {
					if ids[idx] != "" || call.Get("function.name").String() != "lookup" {
						t.Fatalf("expected one named start per call: %s", chunk)
					}
					ids[idx] = id
				}
				arguments[idx].WriteString(call.Get("function.arguments").String())
			}
		}
	}
	if ids != [3]string{"call_a", "call_b", "call_c"} || terminalCount != 1 {
		t.Fatalf("got IDs %q and %d terminal chunks", ids, terminalCount)
	}
	for idx, want := range []string{`{"city":"Tokyo"}`, `{"units":"metric","city":"Paris"}`, `{"city":"Osaka"}`} {
		if got := arguments[idx].String(); got != want {
			t.Errorf("call %q arguments = %q, want %q", ids[idx], got, want)
		}
	}
}

func TestAntigravityChatCompleteToolCallArguments(t *testing.T) {
	const object = `{"n":9007199254740993,"query":"<tag>"}`
	for _, tc := range []struct{ name, argsField, want string }{
		{"missing", "", "{}"},
		{"null", `,"args":null`, "{}"},
		{"array", `,"args":[1,2]`, "{}"},
		{"object", `,"args":` + object, object},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":null}},{"functionCall":{"name":"ping"` + tc.argsField + `}}]},"finishReason":"STOP"}]}}`)
			for _, stream := range []bool{true, false} {
				var calls []ojson.Result
				if stream {
					var state any
					for _, chunk := range ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, body, &state) {
						calls = append(calls, ojson.GetBytes(chunk, "choices.0.delta.tool_calls").Array()...)
					}
					if err := state.(interface{ ToolInputError() error }).ToolInputError(); err != nil {
						t.Fatal(err)
					}
				} else {
					out := ConvertAntigravityResponseToOpenAINonStream(t.Context(), "gemini", nil, nil, body, nil)
					calls = ojson.GetBytes(out, "choices.0.message.tool_calls").Array()
				}
				if len(calls) != 1 || calls[0].Get("function.name").Str != "ping" || calls[0].Get("function.arguments").Str != tc.want {
					t.Fatalf("stream=%v calls = %v, want one ping call with arguments %q", stream, calls, tc.want)
				}
			}
		})
	}
}

func TestAntigravityStreamMatchesExplicitToolTailWithoutID(t *testing.T) {
	var state any
	convert := func(frame string) [][]byte {
		return ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, []byte(frame), &state)
	}
	first := convert(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"units":"metric"},"willContinue":true}}]}}]}}`)
	if len(first) != 1 || ojson.GetBytes(first[0], "choices.0.delta.tool_calls.0.id").String() == "" {
		t.Fatalf("expected named tool start: %q", first)
	}
	if placeholders := convert(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{}},{"functionCall":{"args":{ }} }]}}]}}`); len(placeholders) != 0 {
		t.Fatalf("empty placeholders changed tool arguments: %q", placeholders)
	}
	independent := convert(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"other":true}}}]}}]}}`)
	if len(independent) != 1 || ojson.GetBytes(independent[0], "choices.0.delta.tool_calls.0.index").Int() != 1 ||
		ojson.GetBytes(independent[0], "choices.0.delta.tool_calls.0.function.arguments").String() != `{"other":true}` {
		t.Fatalf("unmarked same-name call did not receive its own index: %q", independent)
	}
	last := convert(`{"response":{"candidates":[{"content":{"parts":[{"function_call":{"name":"lookup","args":{"city":"Paris"},"will_continue":false}}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":7}}}`)
	if len(last) != 2 || ojson.GetBytes(last[1], "choices.0.finish_reason").String() != "tool_calls" {
		t.Fatalf("expected the last tool delta followed by completion: %q", last)
	}
	delta := ojson.GetBytes(last[0], "choices.0.delta.tool_calls.0")
	initial := ojson.GetBytes(first[0], "choices.0.delta.tool_calls.0.function.arguments").String()
	if delta.Get("id").Exists() || delta.Get("index").Int() != 0 || initial+delta.Get("function.arguments").String() != `{"units":"metric","city":"Paris"}` {
		t.Fatalf("explicit tail did not complete the first index: %q", last[0])
	}
}

func TestAntigravityStreamRejectsInvalidOrTruncatedToolArguments(t *testing.T) {
	for _, tc := range []struct{ name, frame, errorText string }{
		{"invalid partial args", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","name":"lookup","partialArgs":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":7}}}`, "vertex partialArgs must be an array"},
		{"invalid fragment args", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","name":"lookup","args":null,"willContinue":true}}]}}]}}`, "upstream Antigravity tool argument fragment must be an object"},
		{"nonempty args without a matching call", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"city":"Paris"}}}]}}]}}`, "upstream Antigravity function arguments have no matching tool call"},
		{"truncated tool", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","name":"lookup","willContinue":true}}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":7}}}`, "upstream Antigravity tool call ended before complete arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			chunks := ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, []byte(tc.frame), &state)
			chunks = append(chunks, ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, []byte("[DONE]"), &state)...)
			for _, chunk := range chunks {
				if ojson.GetBytes(chunk, "choices.0.finish_reason").String() != "" {
					t.Fatalf("invalid tool arguments reported a successful finish: %s", chunk)
				}
			}
			if err := state.(interface{ ToolInputError() error }).ToolInputError(); err == nil || err.Error() != tc.errorText {
				t.Fatalf("got error %v, want %q", err, tc.errorText)
			}
		})
	}
	t.Run("nonempty args cannot replace partial arguments", func(t *testing.T) {
		var state any
		first := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","name":"lookup","partialArgs":[{"jsonPath":"$.city","stringValue":"Tok","willContinue":true}],"willContinue":true}}]}}]}}`)
		ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, first, &state)
		final := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","args":{"units":"metric"},"willContinue":false}}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":7}}}`)
		if chunks := ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, final, &state); len(chunks) != 0 {
			t.Fatalf("different argument formats emitted a terminal chunk: %q", chunks)
		}
		if err := state.(interface{ ToolInputError() error }).ToolInputError(); err == nil || err.Error() != "upstream Antigravity switched tool argument formats mid-call" {
			t.Fatalf("different argument formats did not report an error: %v", err)
		}
	})
	for _, text := range []string{`{"text":"interruption"}`, `{"thought":true,"text":"interruption"}`} {
		var state any
		first := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","willContinue":true}}]}}]}}`)
		ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, first, &state)
		next := []byte(`{"response":{"candidates":[{"content":{"parts":[` + text + `]}}]}}`)
		if chunks := ConvertAntigravityResponseToOpenAI(t.Context(), "gemini", nil, nil, next, &state); len(chunks) != 0 {
			t.Fatalf("interleaved text leaked into an incomplete tool response: %q", chunks)
		}
		if err := state.(interface{ ToolInputError() error }).ToolInputError(); err == nil || err.Error() != "upstream Antigravity content interrupted incomplete tool arguments" {
			t.Fatalf("interleaved text did not report interrupted tool arguments: %v", err)
		}
	}
}

func TestFinishReasonStopForNormalText(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Text content only
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello world"}]}}]}}`)
	ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Chunk 2: Final chunk with STOP
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify finish_reason is "stop" (no tool calls were made)
	fr := ojson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr != "stop" {
		t.Errorf("Expected finish_reason 'stop', got: %s", fr)
	}
}

func TestFinishReasonMaxTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Text content
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}}`)
	ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Chunk 2: Final chunk with MAX_TOKENS
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":100,"totalTokenCount":110}}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	nonStream := ConvertAntigravityResponseToOpenAINonStream(ctx, "model", nil, nil, chunk2, nil)
	for name, payload := range map[string][]byte{
		"terminal": result2[0], "non-stream": nonStream,
	} {
		if got := ojson.GetBytes(payload, "choices.0.finish_reason").String(); got != "length" {
			t.Errorf("%s finish_reason = %q, want length", name, got)
		}
		if got := ojson.GetBytes(payload, "choices.0.native_finish_reason").String(); got != "max_tokens" {
			t.Errorf("%s native_finish_reason = %q, want max_tokens", name, got)
		}
	}
}

func TestToolCallTakesPriorityOverMaxTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Contains functionCall
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"test","args":{}}}]}}]}}`)
	ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Chunk 2: Final chunk with MAX_TOKENS (but we had a tool call, so tool_calls should win)
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":100,"totalTokenCount":110}}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify finish_reason is "tool_calls" (takes priority over max_tokens)
	fr := ojson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr != "tool_calls" {
		t.Errorf("Expected finish_reason 'tool_calls', got: %s", fr)
	}
}

func TestNoFinishReasonOnIntermediateChunks(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Text content (no finish reason, no usage)
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}}`)
	result1 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Verify no finish_reason on intermediate chunk
	fr1 := ojson.GetBytes(result1[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Errorf("Expected no finish_reason on intermediate chunk, got: %v", fr1)
	}

	// Chunk 2: More text (no finish reason, no usage)
	chunk2 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":" world"}]}}]}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify no finish_reason on intermediate chunk
	fr2 := ojson.GetBytes(result2[0], "choices.0.finish_reason")
	if fr2.Exists() && fr2.String() != "" && fr2.Type.String() != "Null" {
		t.Errorf("Expected no finish_reason on intermediate chunk, got: %v", fr2)
	}
}

func TestConvertAntigravityResponseToOpenAICompletionTokensIncludeThoughts(t *testing.T) {
	tests := []struct {
		name       string
		chunk      string
		wantOutput int64
	}{
		{
			name:       "candidates and thoughts",
			chunk:      `{"response":{"usageMetadata":{"promptTokenCount":16,"candidatesTokenCount":5,"thoughtsTokenCount":42,"totalTokenCount":63}}}`,
			wantOutput: 47,
		},
		{
			name:       "thoughts without candidates",
			chunk:      `{"response":{"usageMetadata":{"promptTokenCount":16,"thoughtsTokenCount":42,"totalTokenCount":58}}}`,
			wantOutput: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var param any
			result := ConvertAntigravityResponseToOpenAI(context.Background(), "model", nil, nil, []byte(tt.chunk), &param)
			if len(result) != 1 {
				t.Fatalf("expected 1 result, got %d", len(result))
			}
			completionTokens := ojson.GetBytes(result[0], "usage.completion_tokens")
			if !completionTokens.Exists() || completionTokens.Int() != tt.wantOutput {
				t.Fatalf("completion_tokens = %s, want present with value %d. Output: %s", completionTokens.Raw, tt.wantOutput, result[0])
			}
			if got := ojson.GetBytes(result[0], "usage.completion_tokens_details.reasoning_tokens").Int(); got != 42 {
				t.Fatalf("reasoning_tokens = %d, want 42. Output: %s", got, result[0])
			}
			promptTokens := ojson.GetBytes(result[0], "usage.prompt_tokens").Int()
			totalTokens := ojson.GetBytes(result[0], "usage.total_tokens").Int()
			if promptTokens+completionTokens.Int() != totalTokens {
				t.Fatalf("prompt_tokens + completion_tokens = %d, total_tokens = %d", promptTokens+completionTokens.Int(), totalTokens)
			}
		})
	}
}

func TestConvertAntigravityResponseToOpenAI_SynthesizesStopOnUsageChunkWhenFinishReasonOmitted(t *testing.T) {
	ctx := context.Background()
	var param any

	// Pure thinking chunk with usageMetadata but NO finishReason (reproducing real Antigravity upstream logs).
	// Usage alone does not make a chunk terminal: upstream can keep streaming after it.
	chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"Analyzing..."}]}}],"usageMetadata":{"promptTokenCount":100,"totalTokenCount":150,"thoughtsTokenCount":50},"modelVersion":"gemini-3.7-flash","responseId":"resp-999"}}`)
	result := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, chunk, &param)
	if len(result) != 1 {
		t.Fatalf("expected 1 result from chunk, got %d", len(result))
	}
	if fr := ojson.GetBytes(result[0], "choices.0.finish_reason").String(); fr != "" {
		t.Fatalf("usage chunk without finishReason must not be terminal, got %q", fr)
	}

	// The terminal chunk is synthesized on [DONE] instead.
	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 1 {
		t.Fatalf("expected 1 synthesized terminal chunk on [DONE], got %d", len(done))
	}
	if fr := ojson.GetBytes(done[0], "choices.0.finish_reason").String(); fr != "stop" {
		t.Fatalf("terminal finish_reason = %q, want stop", fr)
	}
}

func TestConvertAntigravityResponseToOpenAI_PreservesFilteredUsageUntilDone(t *testing.T) {
	ctx := context.Background()
	var param any

	// Mirrors a payload already processed by FilterSSEUsageMetadata, which renames
	// non-terminal usage to cpaUsageMetadata. It is inlined so this package keeps no
	// dependency on the executor helpers; the executor tests cover the real filter.
	filtered := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"Thinking..."}]}}],"modelVersion":"gemini-3.7-flash","responseId":"resp-filtered","cpaUsageMetadata":{"promptTokenCount":100,"totalTokenCount":150,"thoughtsTokenCount":50}}}`)
	result := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, filtered, &param)
	if len(result) != 1 {
		t.Fatalf("expected 1 intermediate result, got %d", len(result))
	}
	if usage := ojson.GetBytes(result[0], "usage"); usage.Exists() {
		t.Fatalf("intermediate filtered usage should not be emitted: %s", usage.Raw)
	}
	if finishReason := ojson.GetBytes(result[0], "choices.0.finish_reason"); finishReason.String() != "" {
		t.Fatalf("intermediate finish_reason = %q, want null", finishReason.String())
	}

	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 1 {
		t.Fatalf("expected 1 synthetic terminal result, got %d", len(done))
	}
	if finishReason := ojson.GetBytes(done[0], "choices.0.finish_reason").String(); finishReason != "stop" {
		t.Fatalf("terminal finish_reason = %q, want stop", finishReason)
	}
	if got := ojson.GetBytes(done[0], "usage.prompt_tokens").Int(); got != 100 {
		t.Fatalf("terminal prompt_tokens = %d, want 100", got)
	}
	if got := ojson.GetBytes(done[0], "usage.total_tokens").Int(); got != 150 {
		t.Fatalf("terminal total_tokens = %d, want 150", got)
	}
}

func TestConvertAntigravityResponseToOpenAI_SynthesizesStopOnDoneWhenFinishReasonAndUsageOmitted(t *testing.T) {
	ctx := context.Background()
	var param any

	// Intermediate text chunk without finishReason or usage
	chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Partial output"}]}}],"modelVersion":"gemini-3.7-flash","responseId":"resp-888"}}`)
	result := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, chunk, &param)
	if len(result) != 1 {
		t.Fatalf("expected 1 result from chunk, got %d", len(result))
	}
	fr1 := ojson.GetBytes(result[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Fatalf("expected finish_reason to be null on intermediate chunk, got %v", fr1)
	}

	// [DONE] without prior finishReason or usage -> synthesizes final chunk with finish_reason: "stop"
	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 1 {
		t.Fatalf("expected 1 result on [DONE], got %d", len(done))
	}
	frDone := ojson.GetBytes(done[0], "choices.0.finish_reason").String()
	if frDone != "stop" {
		t.Fatalf("expected finish_reason 'stop' on [DONE], got %q", frDone)
	}
	if mv := ojson.GetBytes(done[0], "model").String(); mv != "gemini-3.7-flash" {
		t.Fatalf("expected model 'gemini-3.7-flash', got %q", mv)
	}
	if rid := ojson.GetBytes(done[0], "id").String(); rid != "resp-888" {
		t.Fatalf("expected id 'resp-888', got %q", rid)
	}
}

func TestConvertAntigravityResponseToOpenAI_DoesNotFinalizeEmptyUpstreamStream(t *testing.T) {
	ctx := context.Background()
	var param any

	// A 200 response whose body carried no response chunk must not be reported as
	// a successful empty completion.
	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 0 {
		t.Fatalf("empty stream must not synthesize a terminal chunk, got %d results: %s", len(done), done[0])
	}
}

func TestConvertAntigravityResponseToOpenAI_ResponseFreeEventDoesNotStartStream(t *testing.T) {
	ctx := context.Background()
	var param any

	for _, chunk := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"response":{}}`),
		[]byte(`{"response":{"candidates":[]}}`),
		[]byte(`{"response":{"candidates":[],"usageMetadata":{}}}`),
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":null}}]}}],"usageMetadata":null}}`),
	} {
		ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, chunk, &param)
	}
	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 0 {
		t.Fatalf("response-free events must not synthesize a terminal chunk, got %d results: %s", len(done), done[0])
	}
}

func TestConvertAntigravityResponseToOpenAINonStreamRestoresDisambiguatedName(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"tools":[
		{"type":"function","function":{"name":"` + first + `"}},
		{"type":"function","function":{"name":"` + second + `"}}
	]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + mapped + `","args":{}}}]}}]}}`)

	output := ConvertAntigravityResponseToOpenAINonStream(context.Background(), "gemini-3-flash", original, nil, responseJSON, nil)
	if got := ojson.GetBytes(output, "choices.0.message.tool_calls.0.function.name").String(); got != second {
		t.Fatalf("function.name = %q, want %q. Output: %s", got, second, output)
	}
}

func TestConvertAntigravityResponseToOpenAINonStreamRestoresToolNameOnce(t *testing.T) {
	original := []byte(`{"messages":[{"role":"user","content":"search"}],"tools":[{"type":"function","function":{"name":"mcp/search"}},{"type":"function","function":{"name":"mcp_search"}}],"tool_choice":{"type":"allowed_tools","allowed_tools":{"tools":[{"type":"function","function":{"name":"mcp_search"}}]}}}`)
	request, err := ConvertOpenAIRequestToAntigravity("gemini", original, false)
	if err != nil {
		t.Fatal(err)
	}
	wireName := ojson.GetBytes(request, "request.tools.0.functionDeclarations.0.name").String()
	if wireName == "" || wireName == "mcp_search" {
		t.Fatalf("expected an Antigravity alias for mcp_search, got %q", wireName)
	}
	upstream := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + wireName + `","args":{}}}]}}]}}`)
	output := ConvertAntigravityResponseToOpenAINonStream(t.Context(), "gemini", original, request, upstream, nil)
	if got := ojson.GetBytes(output, "choices.0.message.tool_calls.0.function.name").String(); got != "mcp_search" {
		t.Fatalf("function name = %q, want mcp_search: %s", got, output)
	}
}

func TestConvertAntigravityResponseToOpenAINonStreamIncludesReasoningContent(t *testing.T) {
	ctx := context.Background()
	responseJSON := []byte(`{
		"response": {
			"candidates": [{
				"index": 0,
				"content": {
					"parts": [
						{"text": "I need to multiply 17 by 24.", "thought": true},
						{"text": "408", "thoughtSignature": "sig-final-answer"}
					]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 16,
				"candidatesTokenCount": 3,
				"thoughtsTokenCount": 42,
				"totalTokenCount": 61
			},
			"modelVersion": "gemini-3.1-pro-low",
			"responseId": "resp-reasoning"
		}
	}`)

	output := ConvertAntigravityResponseToOpenAINonStream(ctx, "gemini-3.1-pro-low", nil, nil, responseJSON, nil)
	if got := ojson.GetBytes(output, "choices.0.message.reasoning_content").String(); got != "I need to multiply 17 by 24." {
		t.Fatalf("reasoning_content = %q, want thought text. Output: %s", got, output)
	}
	if got := ojson.GetBytes(output, "choices.0.message.content").String(); got != "408" {
		t.Fatalf("content = %q, want final answer. Output: %s", got, output)
	}
	if got := ojson.GetBytes(output, "usage.completion_tokens_details.reasoning_tokens").Int(); got != 42 {
		t.Fatalf("reasoning_tokens = %d, want 42. Output: %s", got, output)
	}
}

func TestConvertAntigravityResponseToOpenAI_NonStopFinishReasonsMapToContentFilter(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		upstreamReason string
		wantNative     string
	}{
		{upstreamReason: "MALFORMED_RESPONSE", wantNative: "malformed_response"},
		{upstreamReason: "MALFORMED_FUNCTION_CALL", wantNative: "malformed_function_call"},
		{upstreamReason: "SAFETY", wantNative: "safety"},
		{upstreamReason: "RECITATION", wantNative: "recitation"},
	} {
		t.Run(tc.upstreamReason, func(t *testing.T) {
			var param any
			chunk := []byte(`{"response":{"candidates":[{"content":{"role":"model"},"finishReason":"` + tc.upstreamReason + `"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1,"totalTokenCount":11}}}`)
			out := ConvertAntigravityResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, chunk, &param)
			if len(out) != 1 {
				t.Fatalf("expected 1 chunk, got %d", len(out))
			}
			if got := ojson.GetBytes(out[0], "choices.0.finish_reason").String(); got != "content_filter" {
				t.Fatalf("finish_reason = %q, want content_filter; payload=%s", got, out[0])
			}
			if got := ojson.GetBytes(out[0], "choices.0.native_finish_reason").String(); got != tc.wantNative {
				t.Fatalf("native_finish_reason = %q, want %q; payload=%s", got, tc.wantNative, out[0])
			}
		})
	}
}

func TestConvertAntigravityResponseToOpenAI_OmitToolCallsWhenAbsentInStreamingDelta(t *testing.T) {
	ctx := context.Background()
	var param any

	// 1. Text chunk: delta must omit tool_calls entirely instead of emitting "tool_calls": null.
	textChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello world"}]}}]}}`)
	textResults := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.5-flash-lite", nil, nil, textChunk, &param)
	if len(textResults) != 1 {
		t.Fatalf("expected 1 result, got %d", len(textResults))
	}
	if tc := ojson.GetBytes(textResults[0], "choices.0.delta.tool_calls"); tc.Exists() {
		t.Fatalf("choices.0.delta.tool_calls must not exist in text-only chunk, got: %s (type: %s)", tc.Raw, tc.Type)
	}

	// 2. Reasoning chunk: delta must omit tool_calls entirely.
	thoughtChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"thinking...","thought":true}]}}]}}`)
	thoughtResults := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.5-flash-lite", nil, nil, thoughtChunk, &param)
	if len(thoughtResults) != 1 {
		t.Fatalf("expected 1 result, got %d", len(thoughtResults))
	}
	if tc := ojson.GetBytes(thoughtResults[0], "choices.0.delta.tool_calls"); tc.Exists() {
		t.Fatalf("choices.0.delta.tool_calls must not exist in reasoning chunk, got: %s (type: %s)", tc.Raw, tc.Type)
	}

	// 3. Tool call chunk: delta must emit tool_calls as an array.
	toolChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Beijing"}}}]}}]}}`)
	toolResults := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.5-flash-lite", nil, nil, toolChunk, &param)
	if len(toolResults) != 1 {
		t.Fatalf("expected 1 result, got %d", len(toolResults))
	}
	tc := ojson.GetBytes(toolResults[0], "choices.0.delta.tool_calls")
	if !tc.Exists() || !tc.IsArray() || len(tc.Array()) != 1 {
		t.Fatalf("choices.0.delta.tool_calls must be an array with 1 call, got: %s", tc.Raw)
	}
	if gotName := tc.Array()[0].Get("function.name").String(); gotName != "get_weather" {
		t.Fatalf("function.name = %q, want get_weather", gotName)
	}
	if gotArgs := tc.Array()[0].Get("function.arguments").String(); gotArgs != `{"city":"Beijing"}` {
		t.Fatalf("function.arguments = %q, want {\"city\":\"Beijing\"}", gotArgs)
	}

	// 4. Final chunk with finishReason and usage: delta must omit tool_calls.
	finalChunk := []byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`)
	finalResults := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.5-flash-lite", nil, nil, finalChunk, &param)
	if len(finalResults) != 1 {
		t.Fatalf("expected 1 result, got %d", len(finalResults))
	}
	if tcFinal := ojson.GetBytes(finalResults[0], "choices.0.delta.tool_calls"); tcFinal.Exists() {
		t.Fatalf("choices.0.delta.tool_calls must not exist in final chunk, got: %s (type: %s)", tcFinal.Raw, tcFinal.Type)
	}

	// 5. Synthesized DONE terminal chunk: delta must omit tool_calls.
	var doneParam any
	ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.5-flash-lite", nil, nil, textChunk, &doneParam)
	doneResults := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.5-flash-lite", nil, nil, []byte("[DONE]"), &doneParam)
	if len(doneResults) == 1 {
		if tcDone := ojson.GetBytes(doneResults[0], "choices.0.delta.tool_calls"); tcDone.Exists() {
			t.Fatalf("choices.0.delta.tool_calls must not exist in synthesized DONE chunk, got: %s (type: %s)", tcDone.Raw, tcDone.Type)
		}
	}
}

func TestConvertAntigravityResponseToOpenAI_SignatureOnlyEmptyTextDoesNotStartStreamOrEmitNullDelta(t *testing.T) {
	for _, part := range []string{`{"thoughtSignature":"opaque-sig","text":""}`, `{"thought":true,"thought_signature":"opaque-sig","text":""}`, `{}`} {
		var state any
		chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[` + part + `]}}],"cpaUsageMetadata":{"promptTokenCount":378515,"cachedContentTokenCount":376466,"totalTokenCount":378515},"modelVersion":"gemini-4-hydrogen","responseId":"resp-7536"}}`)
		for _, raw := range [][]byte{chunk, []byte("[DONE]")} {
			if output := ConvertAntigravityResponseToOpenAI(context.Background(), "gemini-4-hydrogen", nil, nil, raw, &state); len(output) != 0 {
				t.Fatalf("signature-only stream emitted %s", bytes.Join(output, nil))
			}
		}
	}
}

func TestConvertAntigravityResponseToOpenAI_SkipsThoughtInlineDataImages(t *testing.T) {
	ctx := context.Background()
	const finalURL = "data:image/png;base64,RklOQUxfSU1BR0U="

	t.Run("stream keeps only deliverable image", func(t *testing.T) {
		var param any
		thought := []byte(`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"thought":true,"inlineData":{"mimeType":"image/png","data":"RFJBRlRfSU1BR0U="}}]}}],"cpaUsageMetadata":{"promptTokenCount":12},"modelVersion":"gemini-3-pro-image","responseId":"resp_ag_7566"}}`)
		if out := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3-pro-image", nil, nil, thought, &param); len(out) != 0 {
			t.Fatalf("thought image chunk must not emit a delta, got %s", out[0])
		}
		final := []byte(`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"thought":true,"inline_data":{"mime_type":"image/png","data":"RFJBRlRfMg=="}},{"inlineData":{"mimeType":"image/png","data":"RklOQUxfSU1BR0U="},"thoughtSignature":"EjQKDg=="}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":1120,"totalTokenCount":1132},"modelVersion":"gemini-3-pro-image","responseId":"resp_ag_7566"}}`)
		out := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3-pro-image", nil, nil, final, &param)
		if len(out) != 2 {
			t.Fatalf("expected an image chunk followed by completion, got %q", out)
		}
		images := ojson.GetBytes(out[0], "choices.0.delta.images").Array()
		if len(images) != 1 || images[0].Get("index").Int() != 0 || images[0].Get("image_url.url").String() != finalURL {
			t.Fatalf("delta.images must contain only the final image at index 0: %s", out[0])
		}
	})

	t.Run("non-stream skips thought image", func(t *testing.T) {
		raw := []byte(`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"thought":true,"text":"Drafting."},{"thought":true,"inlineData":{"mimeType":"image/png","data":"RFJBRlRfSU1BR0U="}},{"inlineData":{"mimeType":"image/png","data":"RklOQUxfSU1BR0U="}}]},"finishReason":"STOP"}]}}`)
		out := ConvertAntigravityResponseToOpenAINonStream(ctx, "gemini-3-pro-image", nil, nil, raw, nil)
		images := ojson.GetBytes(out, "choices.0.message.images").Array()
		if len(images) != 1 || images[0].Get("image_url.url").String() != finalURL {
			t.Fatalf("message.images must contain only the final image: %s", out)
		}
	})

	t.Run("thought-only image stream does not synthesize stop", func(t *testing.T) {
		var param any
		chunk := []byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"inlineData":{"mimeType":"image/png","data":"RFJBRlRfSU1BR0U="}}]}}],"cpaUsageMetadata":{"promptTokenCount":12,"totalTokenCount":12},"modelVersion":"gemini-3-pro-image","responseId":"resp_ag_empty"}}`)
		if out := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3-pro-image", nil, nil, chunk, &param); len(out) != 0 {
			t.Fatalf("expected no chunks for thought-only image, got %s", out[0])
		}
		if done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3-pro-image", nil, nil, []byte("[DONE]"), &param); len(done) != 0 {
			t.Fatalf("thought-only image stream must not synthesize stop on [DONE], got %s", done[0])
		}
	})
}

func TestConvertAntigravityResponseToOpenAI_UsesEmptyObjectWhenFunctionArgsAreOmitted(t *testing.T) {
	ctx := context.Background()
	var param any
	chunk := []byte(`{"response":{"responseId":"resp_1","candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"get_current_time"}}]}}]}}`)

	outputs := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.8-flash-high", nil, nil, chunk, &param)
	if len(outputs) != 1 {
		t.Fatalf("output count = %d, want 1; output=%s", len(outputs), outputs)
	}
	if got := ojson.GetBytesNoCopy(outputs[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "{}" {
		t.Fatalf("function.arguments = %q, want valid empty JSON object %q; output=%s", got, "{}", outputs[0])
	}
}

func TestConvertAntigravityResponseToOpenAI_PreservesArgumentsAfterStreamingToolStart(t *testing.T) {
	ctx := context.Background()
	var param any
	chunks := [][]byte{
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"get_value","willContinue":true}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","args":{"value":1}}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`),
	}

	var arguments strings.Builder
	for _, chunk := range chunks {
		for _, output := range ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.8-flash-high", nil, nil, chunk, &param) {
			arguments.WriteString(ojson.GetBytesNoCopy(output, "choices.0.delta.tool_calls.0.function.arguments").String())
		}
	}
	if got := arguments.String(); got != `{"value":1}` {
		t.Fatalf("concatenated function.arguments = %q, want valid JSON %q", got, `{"value":1}`)
	}
}

func TestConvertAntigravityResponseToOpenAI_SingleChunkPartialArgs(t *testing.T) {
	ctx := context.Background()
	var param any
	chunks := [][]byte{
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"Paris"}]}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.8-flash-high", nil, nil, chunk, &param)...)
	}
	if len(outputs) != 2 {
		t.Fatalf("output count = %d, want 2", len(outputs))
	}
	call := ojson.GetBytesNoCopy(outputs[0], "choices.0.delta.tool_calls.0")
	if got := call.Get("index").Int(); got != 0 {
		t.Fatalf("index = %d, want 0; output=%s", got, outputs[0])
	}
	if got := call.Get("function.name").String(); got != "get_weather" {
		t.Fatalf("function.name = %q, want get_weather; output=%s", got, outputs[0])
	}
	if got := call.Get("function.arguments").String(); got != `{"city":"Paris"}` {
		t.Fatalf("function.arguments = %q, want %q; output=%s", got, `{"city":"Paris"}`, outputs[0])
	}
	if tcFinal := ojson.GetBytesNoCopy(outputs[1], "choices.0.delta.tool_calls"); tcFinal.Exists() {
		t.Fatalf("terminal chunk must not emit extra tool_calls after completed single-chunk partialArgs, got: %s", tcFinal.Raw)
	}
}

func TestConvertAntigravityResponseToOpenAI_MultiChunkPartialArgs(t *testing.T) {
	ctx := context.Background()
	var param any
	chunks := [][]byte{
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"New ","willContinue":true}],"willContinue":true}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.city","stringValue":"York"}],"willContinue":false}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.8-flash-high", nil, nil, chunk, &param)...)
	}
	if len(outputs) != 3 {
		t.Fatalf("output count = %d, want 3", len(outputs))
	}
	cont := ojson.GetBytesNoCopy(outputs[1], "choices.0.delta.tool_calls.0")
	if got := cont.Get("index").Int(); got != 0 {
		t.Fatalf("continuation index = %d, want 0; output=%s", got, outputs[1])
	}
	if id := cont.Get("id"); id.Exists() {
		t.Fatalf("continuation must omit id, got %s", id.Raw)
	}
	if name := cont.Get("function.name"); name.Exists() {
		t.Fatalf("continuation must omit function.name, got %s", name.Raw)
	}
	if tcFinal := ojson.GetBytesNoCopy(outputs[2], "choices.0.delta.tool_calls"); tcFinal.Exists() {
		t.Fatalf("terminal chunk must not emit extra tool_calls after completed partialArgs, got: %s", tcFinal.Raw)
	}

	var arguments strings.Builder
	for _, output := range outputs {
		for _, call := range ojson.GetBytesNoCopy(output, "choices.0.delta.tool_calls").Array() {
			arguments.WriteString(call.Get("function.arguments").String())
		}
	}
	if got := arguments.String(); got != `{"city":"New York"}` {
		t.Fatalf("concatenated partialArgs arguments = %q, want %q", got, `{"city":"New York"}`)
	}
}

func TestConvertAntigravityResponseToOpenAI_RepeatedNamePartialArgsContinuation(t *testing.T) {
	ctx := context.Background()
	var param any
	chunks := [][]byte{
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"New ","willContinue":true}],"willContinue":true}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"York"}],"willContinue":false}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`),
	}

	var arguments strings.Builder
	for _, chunk := range chunks {
		for _, output := range ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.8-flash-high", nil, nil, chunk, &param) {
			for _, call := range ojson.GetBytesNoCopy(output, "choices.0.delta.tool_calls").Array() {
				if gotIdx := call.Get("index").Int(); gotIdx != 0 {
					t.Fatalf("tool call index = %d, want 0; output=%s", gotIdx, output)
				}
				arguments.WriteString(call.Get("function.arguments").String())
			}
		}
	}
	if got := arguments.String(); got != `{"city":"New York"}` {
		t.Fatalf("concatenated arguments = %q, want %q", got, `{"city":"New York"}`)
	}
}

func TestConvertAntigravityResponseToOpenAI_ParallelCompleteToolCallsEmitOnceWithoutDuplicatedIndices(t *testing.T) {
	ctx := context.Background()
	var param any
	chunks := [][]byte{
		[]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_time","args":{"tz":"UTC"}}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_time","args":{"tz":"America/Santiago"}}},{"functionCall":{"name":"get_time","args":{"tz":"Asia/Tokyo"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`),
	}

	var indices []int64
	var arguments []string
	for _, chunk := range chunks {
		for _, output := range ConvertAntigravityResponseToOpenAI(ctx, "gemini-3-flash", nil, nil, chunk, &param) {
			for _, call := range ojson.GetBytesNoCopy(output, "choices.0.delta.tool_calls").Array() {
				indices = append(indices, call.Get("index").Int())
				arguments = append(arguments, call.Get("function.arguments").String())
			}
		}
	}

	wantIndices := []int64{0, 1, 2}
	if len(indices) != len(wantIndices) {
		t.Fatalf("stream tool indices = %v, want %v", indices, wantIndices)
	}
	for i, want := range wantIndices {
		if indices[i] != want {
			t.Fatalf("stream tool indices = %v, want %v", indices, wantIndices)
		}
	}
	wantArgs := []string{`{"tz":"UTC"}`, `{"tz":"America/Santiago"}`, `{"tz":"Asia/Tokyo"}`}
	if len(arguments) != len(wantArgs) {
		t.Fatalf("tool arguments = %v, want %v", arguments, wantArgs)
	}
	for i, want := range wantArgs {
		if arguments[i] != want {
			t.Fatalf("tool arguments = %v, want %v", arguments, wantArgs)
		}
	}
}

func TestConvertAntigravityResponseToOpenAI_SingleCompleteToolCallInTerminalChunkEmitsSingleDelta(t *testing.T) {
	ctx := context.Background()
	var param any
	chunk := []byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_time","args":{"tz":"UTC"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`)

	outputs := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3-flash", nil, nil, chunk, &param)
	if len(outputs) != 2 {
		t.Fatalf("output count = %d, want 2 (payload + terminal); outputs=%s", len(outputs), outputs)
	}
	calls := ojson.GetBytesNoCopy(outputs[0], "choices.0.delta.tool_calls").Array()
	if len(calls) != 1 {
		t.Fatalf("delta.tool_calls count = %d, want 1; output=%s", len(calls), outputs[0])
	}
	if gotIdx := calls[0].Get("index").Int(); gotIdx != 0 {
		t.Fatalf("tool call index = %d, want 0; output=%s", gotIdx, outputs[0])
	}
	if gotArgs := calls[0].Get("function.arguments").String(); gotArgs != `{"tz":"UTC"}` {
		t.Fatalf("function.arguments = %q, want %q; output=%s", gotArgs, `{"tz":"UTC"}`, outputs[0])
	}
	if tcFinal := ojson.GetBytesNoCopy(outputs[1], "choices.0.delta.tool_calls"); tcFinal.Exists() {
		t.Fatalf("terminal chunk must not emit delta.tool_calls, got: %s", outputs[1])
	}
	if got := ojson.GetBytesNoCopy(outputs[1], "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("terminal finish_reason = %q, want tool_calls; output=%s", got, outputs[1])
	}
}

func TestConvertAntigravityResponseToOpenAI_StreamingToolCallExplicitWillContinueFalseClosesImmediately(t *testing.T) {
	ctx := context.Background()
	var param any
	chunks := [][]byte{
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"write_file","args":{"path":"a.txt"},"willContinue":true}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","args":{"content":"hello"},"willContinue":false}}]}}]}}`),
		[]byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`),
	}

	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.8-flash-high", nil, nil, chunk, &param)...)
	}
	if len(outputs) != 3 {
		t.Fatalf("output count = %d, want 3", len(outputs))
	}
	if tcFinal := ojson.GetBytesNoCopy(outputs[2], "choices.0.delta.tool_calls"); tcFinal.Exists() {
		t.Fatalf("terminal chunk must not emit extra tool_calls after willContinue=false, got: %s", tcFinal.Raw)
	}
	var arguments strings.Builder
	for _, output := range outputs {
		for _, call := range ojson.GetBytesNoCopy(output, "choices.0.delta.tool_calls").Array() {
			if gotIdx := call.Get("index").Int(); gotIdx != 0 {
				t.Fatalf("tool call index = %d, want 0; output=%s", gotIdx, output)
			}
			arguments.WriteString(call.Get("function.arguments").String())
		}
	}
	if got := arguments.String(); got != `{"path":"a.txt","content":"hello"}` {
		t.Fatalf("concatenated arguments = %q, want %q", got, `{"path":"a.txt","content":"hello"}`)
	}
}

func TestConvertAntigravityResponseToOpenAI_ClosedCompleteToolCallIgnoresEmptySeparatorPart(t *testing.T) {
	ctx := context.Background()
	var param any
	firstChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"get_time","args":{"tz":"UTC"}}}]}}]}}`)
	firstOutputs := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3-flash", nil, nil, firstChunk, &param)
	if len(firstOutputs) != 1 {
		t.Fatalf("first chunk output count = %d, want 1", len(firstOutputs))
	}

	separatorChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{}}]}}]}}`)
	separatorOutputs := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3-flash", nil, nil, separatorChunk, &param)
	if len(separatorOutputs) != 0 {
		t.Fatalf("empty separator chunk must emit no delta, got %d outputs: %s", len(separatorOutputs), separatorOutputs[0])
	}
}

func TestConvertAntigravityResponseToOpenAINonStream_DoesNotRemapRestoredCollidingName(t *testing.T) {
	original := []byte(`{"tools":[
		{"type":"function","function":{"name":"mcp/search"}},
		{"type":"function","function":{"name":"mcp_search"}}
	]}`)
	mapped := util.SanitizedFunctionNameMap(original)["mcp_search"]
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + mapped + `","args":{"q":"test"}}}]}}]}}`)

	output := ConvertAntigravityResponseToOpenAINonStream(context.Background(), "gemini-3-flash", original, nil, responseJSON, nil)
	if got := ojson.GetBytesNoCopy(output, "choices.0.message.tool_calls.0.function.name").String(); got != "mcp_search" {
		t.Fatalf("function.name = %q, want %q. Output: %s", got, "mcp_search", output)
	}
}

func TestConvertAntigravityResponseToOpenAINonStream_DoesNotDoubleRestoreChainedAlias(t *testing.T) {
	// Build a 3-tool chain where the third tool's original name equals the
	// disambiguated upstream alias assigned to the first tool.
	twoTools := []byte(`{"tools":[
		{"type":"function","function":{"name":"mcp/search"}},
		{"type":"function","function":{"name":"mcp_search"}}
	]}`)
	firstAlias := util.SanitizedFunctionNameMap(twoTools)["mcp/search"]
	if firstAlias == "" || firstAlias == "mcp/search" {
		t.Fatalf("expected disambiguated alias for mcp/search, got %q", firstAlias)
	}

	original := []byte(`{"tools":[
		{"type":"function","function":{"name":"mcp/search"}},
		{"type":"function","function":{"name":"mcp_search"}},
		{"type":"function","function":{"name":"` + firstAlias + `"}}
	]}`)
	forward := util.SanitizedFunctionNameMap(original)
	if forward["mcp/search"] != firstAlias {
		t.Fatalf("forward[mcp/search] = %q, want %q", forward["mcp/search"], firstAlias)
	}
	thirdMapped := forward[firstAlias]
	if thirdMapped == "" || thirdMapped == firstAlias {
		t.Fatalf("expected disambiguated upstream name for third tool %q, got %q", firstAlias, thirdMapped)
	}

	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + thirdMapped + `","args":{"q":"test"}}}]}}]}}`)
	output := ConvertAntigravityResponseToOpenAINonStream(context.Background(), "gemini-3-flash", original, nil, responseJSON, nil)
	if got := ojson.GetBytesNoCopy(output, "choices.0.message.tool_calls.0.function.name").String(); got != firstAlias {
		t.Fatalf("function.name = %q, want %q. Output: %s", got, firstAlias, output)
	}
}

func TestConvertAntigravityResponseToOpenAI_MultiPartThoughtAndTrailingSignature(t *testing.T) {
	ctx := context.Background()

	t.Run("thought followed by empty-text thoughtSignature part", func(t *testing.T) {
		var param any
		chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"Analyzing the request..."},{"thought":true,"text":"","thoughtSignature":"EjQK..."}]}}]}}`)
		out := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.1-pro", nil, nil, chunk, &param)
		if len(out) != 1 {
			t.Fatalf("expected 1 chunk, got %d", len(out))
		}
		if got := ojson.GetBytesNoCopy(out[0], "choices.0.delta.reasoning_content").String(); got != "Analyzing the request..." {
			t.Fatalf("delta.reasoning_content = %q, want %q; payload=%s", got, "Analyzing the request...", out[0])
		}
		if content := ojson.GetBytesNoCopy(out[0], "choices.0.delta.content"); content.Type != ojson.Null {
			t.Fatalf("delta.content = %s (type %v), want null; payload=%s", content.Raw, content.Type, out[0])
		}
		if role := ojson.GetBytesNoCopy(out[0], "choices.0.delta.role").String(); role != "assistant" {
			t.Fatalf("delta.role = %q, want assistant; payload=%s", role, out[0])
		}
	})

	t.Run("thought followed by non-thought empty-text thoughtSignature part", func(t *testing.T) {
		var param any
		chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"Analyzing the request..."},{"text":"","thoughtSignature":"EjQK..."}]}}]}}`)
		out := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.1-pro", nil, nil, chunk, &param)
		if len(out) != 1 {
			t.Fatalf("expected 1 chunk, got %d", len(out))
		}
		if got := ojson.GetBytesNoCopy(out[0], "choices.0.delta.reasoning_content").String(); got != "Analyzing the request..." {
			t.Fatalf("delta.reasoning_content = %q, want %q; payload=%s", got, "Analyzing the request...", out[0])
		}
		if content := ojson.GetBytesNoCopy(out[0], "choices.0.delta.content"); content.Type != ojson.Null {
			t.Fatalf("delta.content = %s (type %v), want null; payload=%s", content.Raw, content.Type, out[0])
		}
	})

	t.Run("visible text followed by empty-text thoughtSignature part", func(t *testing.T) {
		var param any
		chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Final answer"},{"text":"","thoughtSignature":"EjQK..."}]}}]}}`)
		out := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.1-pro", nil, nil, chunk, &param)
		if len(out) != 1 {
			t.Fatalf("expected 1 chunk, got %d", len(out))
		}
		if got := ojson.GetBytesNoCopy(out[0], "choices.0.delta.content").String(); got != "Final answer" {
			t.Fatalf("delta.content = %q, want %q; payload=%s", got, "Final answer", out[0])
		}
		if reasoning := ojson.GetBytesNoCopy(out[0], "choices.0.delta.reasoning_content"); reasoning.Type != ojson.Null {
			t.Fatalf("delta.reasoning_content = %s (type %v), want null; payload=%s", reasoning.Raw, reasoning.Type, out[0])
		}
	})

	t.Run("multiple thought parts and multiple text parts in same chunk", func(t *testing.T) {
		var param any
		chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"Step 1. "},{"thought":true,"text":"Step 2."},{"text":"Hello "},{"text":"world"}]}}]}}`)
		out := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.1-pro", nil, nil, chunk, &param)
		if len(out) != 1 {
			t.Fatalf("expected 1 chunk, got %d", len(out))
		}
		if got := ojson.GetBytesNoCopy(out[0], "choices.0.delta.reasoning_content").String(); got != "Step 1. Step 2." {
			t.Fatalf("delta.reasoning_content = %q, want %q; payload=%s", got, "Step 1. Step 2.", out[0])
		}
		if got := ojson.GetBytesNoCopy(out[0], "choices.0.delta.content").String(); got != "Hello world" {
			t.Fatalf("delta.content = %q, want %q; payload=%s", got, "Hello world", out[0])
		}
	})
}

func TestConvertAntigravityResponseToOpenAI_NullOrEmptyFinishReasonDoesNotClobberCachedReason(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name           string
		upstreamReason string
		trailingRaw    string
		wantFinish     string
		wantNative     string
	}{
		{name: "safety_trailing_empty", upstreamReason: "SAFETY", trailingRaw: `""`, wantFinish: "content_filter", wantNative: "safety"},
		{name: "safety_trailing_null", upstreamReason: "SAFETY", trailingRaw: "null", wantFinish: "content_filter", wantNative: "safety"},
		{name: "safety_trailing_whitespace", upstreamReason: "SAFETY", trailingRaw: `"   "`, wantFinish: "content_filter", wantNative: "safety"},
		{name: "recitation_trailing_empty", upstreamReason: "RECITATION", trailingRaw: `""`, wantFinish: "content_filter", wantNative: "recitation"},
		{name: "max_tokens_trailing_empty", upstreamReason: "MAX_TOKENS", trailingRaw: `""`, wantFinish: "length", wantNative: "max_tokens"},
		{name: "max_tokens_trailing_null", upstreamReason: "MAX_TOKENS", trailingRaw: "null", wantFinish: "length", wantNative: "max_tokens"},
		{name: "lowercase_max_tokens_trailing_null", upstreamReason: "max_tokens", trailingRaw: "null", wantFinish: "length", wantNative: "max_tokens"},
	}

	for _, tc := range cases {
		t.Run("trailing_usage_"+tc.name, func(t *testing.T) {
			var param any
			chunk1 := []byte(`{"response":{"responseId":"resp_8186","modelVersion":"claude-opus-5-5","candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"finishReason":"` + tc.upstreamReason + `"}]}}`)
			chunk2 := []byte(`{"response":{"responseId":"resp_8186","modelVersion":"claude-opus-5-5","candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":` + tc.trailingRaw + `}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`)

			_ = ConvertAntigravityResponseToOpenAI(ctx, "claude-opus-5-5", nil, nil, chunk1, &param)
			out2 := ConvertAntigravityResponseToOpenAI(ctx, "claude-opus-5-5", nil, nil, chunk2, &param)
			if len(out2) != 1 {
				t.Fatalf("output count = %d, want 1; outputs=%q", len(out2), out2)
			}
			if got := ojson.GetBytesNoCopy(out2[0], "choices.0.finish_reason").String(); got != tc.wantFinish {
				t.Fatalf("finish_reason = %q, want %q; payload=%s", got, tc.wantFinish, out2[0])
			}
			if got := ojson.GetBytesNoCopy(out2[0], "choices.0.native_finish_reason").String(); got != tc.wantNative {
				t.Fatalf("native_finish_reason = %q, want %q; payload=%s", got, tc.wantNative, out2[0])
			}
			if got := ojson.GetBytesNoCopy(out2[0], "usage.total_tokens").Int(); got != 15 {
				t.Fatalf("usage.total_tokens = %d, want 15; payload=%s", got, out2[0])
			}

			done := ConvertAntigravityResponseToOpenAI(ctx, "claude-opus-5-5", nil, nil, []byte("[DONE]"), &param)
			if len(done) != 0 {
				t.Fatalf("DONE output count = %d, want 0 after terminal chunk; outputs=%q", len(done), done)
			}
		})
	}

	t.Run("empty_null_and_whitespace_finishReason_preserve_cached_reason_until_DONE", func(t *testing.T) {
		var param any
		chunk1 := []byte(`{"response":{"responseId":"resp_8186_done","modelVersion":"claude-opus-5-5","candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"finishReason":"SAFETY"}]}}`)
		chunk2 := []byte(`{"response":{"candidates":[{"finishReason":""}],"cpaUsageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`)
		chunk3 := []byte(`{"response":{"candidates":[{"finishReason":null}]}}`)
		chunk4 := []byte(`{"response":{"candidates":[{"finishReason":"   "}]}}`)

		_ = ConvertAntigravityResponseToOpenAI(ctx, "claude-opus-5-5", nil, nil, chunk1, &param)
		_ = ConvertAntigravityResponseToOpenAI(ctx, "claude-opus-5-5", nil, nil, chunk2, &param)
		_ = ConvertAntigravityResponseToOpenAI(ctx, "claude-opus-5-5", nil, nil, chunk3, &param)
		_ = ConvertAntigravityResponseToOpenAI(ctx, "claude-opus-5-5", nil, nil, chunk4, &param)

		done := ConvertAntigravityResponseToOpenAI(ctx, "claude-opus-5-5", nil, nil, []byte("[DONE]"), &param)
		if len(done) != 1 {
			t.Fatalf("DONE output count = %d, want 1; outputs=%q", len(done), done)
		}
		if got := ojson.GetBytesNoCopy(done[0], "choices.0.finish_reason").String(); got != "content_filter" {
			t.Fatalf("DONE finish_reason = %q, want %q; payload=%s", got, "content_filter", done[0])
		}
		if got := ojson.GetBytesNoCopy(done[0], "choices.0.native_finish_reason").String(); got != "safety" {
			t.Fatalf("DONE native_finish_reason = %q, want %q; payload=%s", got, "safety", done[0])
		}
		if got := ojson.GetBytesNoCopy(done[0], "usage.total_tokens").Int(); got != 15 {
			t.Fatalf("DONE usage.total_tokens = %d, want 15; payload=%s", got, done[0])
		}
	})

	t.Run("blank_or_null_finishReason_with_usage_does_not_finalize_prematurely", func(t *testing.T) {
		for _, rawFinish := range []string{`""`, "null", `"   "`} {
			var param any
			chunk := []byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":` + rawFinish + `}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}}`)
			out := ConvertAntigravityResponseToOpenAI(ctx, "claude-opus-5-5", nil, nil, chunk, &param)
			if len(out) != 1 {
				t.Fatalf("finishReason=%s: output count = %d, want 1; outputs=%q", rawFinish, len(out), out)
			}
			if fr := ojson.GetBytesNoCopy(out[0], "choices.0.finish_reason"); fr.Type != ojson.Null {
				t.Fatalf("finishReason=%s: finish_reason = %s, want null; payload=%s", rawFinish, fr.Raw, out[0])
			}
		}
	})
}
