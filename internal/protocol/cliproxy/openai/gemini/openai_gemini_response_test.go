package gemini

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponseToGeminiNonStreamPreservesToolCallID(t *testing.T) {
	raw := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_chat_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}}]}`)
	out := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "gpt-test", nil, nil, raw, nil)
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.functionCall.id").String(); got != "call_chat_1" {
		t.Fatalf("functionCall.id = %q, want call_chat_1", got)
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.functionCall.args.q").String(); got != "x" {
		t.Fatalf("functionCall.args.q = %q, want x", got)
	}
}

func TestConvertOpenAIResponseToGeminiNonStream_SeparatesChoices(t *testing.T) {
	raw := []byte(`{"choices":[
		{"index":3,"message":{"reasoning_content":"first thought","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"one\"}"},"thoughtSignature":"signature"}]},"finish_reason":"tool_calls"},
		{"index":1,"message":{"content":"second text"},"finish_reason":"length"},
		{"message":{"content":"third text"}}
	],"usage":{"prompt_tokens":10}}`)
	out := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "gpt-test", nil, nil, raw, nil)
	candidates := gjson.GetBytes(out, "candidates").Array()
	if len(candidates) != 3 {
		t.Fatalf("expected three independent candidates, got %s", out)
	}
	for pos, wantIndex := range []int64{3, 1, 2} {
		candidate := candidates[pos]
		if candidate.Get("index").Int() != wantIndex || candidate.Get("content.role").String() != "model" {
			t.Fatalf("candidate %d has wrong index or role: %s", pos, candidate.Raw)
		}
	}
	first := candidates[0]
	if first.Get("finishReason").String() != "STOP" || first.Get("content.parts.#").Int() != 2 ||
		!first.Get("content.parts.0.thought").Bool() || first.Get("content.parts.0.text").String() != "first thought" ||
		first.Get("content.parts.1.functionCall.id").String() != "call_1" ||
		first.Get("content.parts.1.functionCall.name").String() != "lookup" ||
		first.Get("content.parts.1.functionCall.args.query").String() != "one" ||
		first.Get("content.parts.1.thoughtSignature").String() != "signature" {
		t.Fatalf("first candidate lost thinking, tool, or finish reason: %s", first.Raw)
	}
	second := candidates[1]
	if second.Get("finishReason").String() != "MAX_TOKENS" || second.Get("content.parts.#").Int() != 1 ||
		second.Get("content.parts.0.text").String() != "second text" || second.Get("content.parts.0.thought").Exists() ||
		second.Get("content.parts.0.functionCall").Exists() {
		t.Fatalf("second candidate was truncated or mixed with the first: %s", second.Raw)
	}
	third := candidates[2]
	if third.Get("finishReason").Exists() || third.Get("content.parts.0.text").String() != "third text" ||
		gjson.GetBytes(out, "usageMetadata.promptTokenCount").Int() != 10 {
		t.Fatalf("expected optional finish reason and shared usage: %s", out)
	}
}

func TestConvertOpenAIResponseToGeminiStream_SeparatesChoices(t *testing.T) {
	var state any
	send := func(payload string) [][]byte {
		return ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, []byte(payload), &state)
	}

	out := send(`{"choices":[
		{"index":1,"delta":{"content":"one","tool_calls":[{"index":0,"id":"call_1","function":{"name":"first","arguments":"{\"query\":"}}]}},
		{"index":2,"delta":{"content":"two","tool_calls":[{"index":0,"id":"call_2","function":{"name":"second","arguments":"{\"query\":"}}]}}
	],"usage":{"prompt_tokens":10}}`)
	if len(out) != 2 || gjson.GetBytes(out[0], "candidates.0.content.parts.0.text").String() != "one" ||
		gjson.GetBytes(out[1], "candidates.0.content.parts.0.text").String() != "two" ||
		gjson.GetBytes(out[0], "usageMetadata.promptTokenCount").Int() != 10 ||
		gjson.GetBytes(out[1], "usageMetadata").Exists() {
		t.Fatalf("want two independent content chunks and one usage record, got %q", out)
	}

	out = send(`{"choices":[
		{"index":1,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"one\"}"}}]},"finish_reason":"tool_calls"}
	]}`)
	if len(out) != 1 || gjson.GetBytes(out[0], "candidates.0.index").Int() != 1 ||
		gjson.GetBytes(out[0], "candidates.0.finishReason").String() != "STOP" ||
		gjson.GetBytes(out[0], "candidates.0.content.parts.0.functionCall.id").String() != "call_1" ||
		gjson.GetBytes(out[0], "candidates.0.content.parts.0.functionCall.args.query").String() != "one" {
		t.Fatalf("candidate 1 received the wrong tool or terminal reason: %q", out)
	}

	out = send(`{"choices":[
		{"index":1,"delta":{},"finish_reason":"tool_calls"},
		{"index":2,"delta":{"content":"still working","tool_calls":[{"index":0,"function":{"arguments":"\"two\"}"},"thoughtSignature":"sig_2"}]}},
		{"index":3,"delta":{"content":"third"}}
	]}`)
	if len(out) != 2 || gjson.GetBytes(out[0], "candidates.0.index").Int() != 2 ||
		gjson.GetBytes(out[0], "candidates.0.content.parts.0.text").String() != "still working" {
		t.Fatalf("duplicate terminal for candidate 1 interrupted later candidates: %q", out)
	}

	out = send("[DONE]")
	if len(out) != 2 || gjson.GetBytes(out[0], "candidates.0.index").Int() != 2 ||
		gjson.GetBytes(out[0], "candidates.0.finishReason").String() != "STOP" ||
		gjson.GetBytes(out[0], "candidates.0.content.parts.0.functionCall.id").String() != "call_2" ||
		gjson.GetBytes(out[0], "candidates.0.content.parts.0.functionCall.args.query").String() != "two" ||
		gjson.GetBytes(out[0], "candidates.0.content.parts.0.thoughtSignature").String() != "sig_2" ||
		gjson.GetBytes(out[1], "candidates.0.index").Int() != 3 ||
		gjson.GetBytes(out[1], "candidates.0.finishReason").String() != "STOP" {
		t.Fatalf("stream completion did not flush only candidates 2 and 3: %q", out)
	}
	if out := send("[DONE]"); len(out) != 0 {
		t.Fatalf("duplicate stream completion produced %q", out)
	}
}

func TestConvertOpenAIResponseToGeminiStream_PreservesUpdatedUsage(t *testing.T) {
	for _, tt := range []struct{ name, choices string }{
		{"empty delta", `[{"index":0,"delta":{},"finish_reason":null}]`},
		{"empty choices", `[]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var state any
			send := func(payload string) [][]byte {
				return ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, []byte(payload), &state)
			}
			assertUsage := func(chunks [][]byte, output, total int64) {
				t.Helper()
				if len(chunks) != 1 || gjson.GetBytes(chunks[0], "usageMetadata.candidatesTokenCount").Int() != output ||
					gjson.GetBytes(chunks[0], "usageMetadata.totalTokenCount").Int() != total {
					t.Fatalf("want output=%d and total=%d, got %q", output, total, chunks)
				}
			}
			assertUsage(send(`{"choices":[{"index":0,"delta":{"content":"answer"}}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`), 2, 13)
			assertUsage(send(`{"choices":`+tt.choices+`,"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`), 5, 16)
			assertUsage(send(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`), 5, 16)
			if out := send(`{"choices":[],"usage":{"output_tokens":5,"total_tokens":16,"input_tokens":11}}`); len(out) != 0 {
				t.Fatalf("same terminal usage repeated: %q", out)
			}
			assertUsage(send(`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`), 7, 18)
		})
	}
}

func TestConvertOpenAIResponseToGemini_PreservesZeroAndSkipsNullUsageFields(t *testing.T) {
	const usage = `"usage":{"prompt_tokens":null,"input_tokens":0,"completion_tokens":"2","total_tokens":null,"completion_tokens_details":{"reasoning_tokens":null},"output_tokens_details":{"reasoning_tokens":1}}`
	check := func(out []byte) {
		t.Helper()
		metadata := gjson.GetBytes(out, "usageMetadata")
		for field, want := range map[string]string{"promptTokenCount": "0", "candidatesTokenCount": "1", "totalTokenCount": "2", "thoughtsTokenCount": "1"} {
			if got := metadata.Get(field).Raw; got != want {
				t.Errorf("%s = %s, want %s: %s", field, got, want, out)
			}
		}
	}
	var state any
	if out := ConvertOpenAIResponseToGemini(t.Context(), "gpt-test", nil, nil, []byte(`{"choices":[],"usage":null}`), &state); len(out) != 0 {
		t.Fatalf("null usage emitted a frame: %q", out)
	}
	out := ConvertOpenAIResponseToGemini(t.Context(), "gpt-test", nil, nil, []byte(`{"choices":[],`+usage+`}`), &state)
	if len(out) != 1 {
		t.Fatalf("expected one usage frame, got %q", out)
	}
	check(out[0])
	check(ConvertOpenAIResponseToGeminiNonStream(t.Context(), "gpt-test", nil, nil, []byte(`{"choices":[],`+usage+`}`), nil))
}

func TestConvertOpenAIResponseToGemini_UsageBreakdown(t *testing.T) {
	for _, tt := range []struct {
		name, usage string
		want        map[string]string
	}{
		{"authoritative total", `{"prompt_tokens":12,"completion_tokens":8,"total_tokens":25,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":5}}`, map[string]string{
			"promptTokenCount": "12", "candidatesTokenCount": "3", "totalTokenCount": "25", "thoughtsTokenCount": "5", "cachedContentTokenCount": "3",
		}},
		{"explicit zero", `{"prompt_tokens":0,"input_tokens":60,"completion_tokens":0,"output_tokens":40,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"input_tokens_details":{"cached_tokens":9},"completion_tokens_details":{"reasoning_tokens":0},"output_tokens_details":{"reasoning_tokens":8}}`, map[string]string{
			"promptTokenCount": "0", "candidatesTokenCount": "0", "totalTokenCount": "0", "thoughtsTokenCount": "0", "cachedContentTokenCount": "0",
		}},
		{"no breakdown", `{"prompt_tokens":10,"completion_tokens":3,"total_tokens":null,"prompt_tokens_details":{"cached_tokens":null},"completion_tokens_details":{"reasoning_tokens":null}}`, map[string]string{
			"promptTokenCount": "10", "candidatesTokenCount": "3", "totalTokenCount": "13",
		}},
		{"invalid fields skip to valid aliases", `{"prompt_tokens":"","input_tokens":10,"completion_tokens":0.5,"output_tokens":"4","total_tokens":"garbage","prompt_tokens_details":{"cached_tokens":false},"input_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":-1},"output_tokens_details":{"reasoning_tokens":3}}`, map[string]string{
			"promptTokenCount": "10", "candidatesTokenCount": "1", "totalTokenCount": "14", "thoughtsTokenCount": "3", "cachedContentTokenCount": "2",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"choices":[],"usage":` + tt.usage + `}`)
			var state any
			stream := ConvertOpenAIResponseToGemini(t.Context(), "gpt-test", nil, nil, raw, &state)
			if len(stream) != 1 {
				t.Fatalf("want one usage event, got %q", stream)
			}
			for _, out := range [][]byte{stream[0], ConvertOpenAIResponseToGeminiNonStream(t.Context(), "gpt-test", nil, nil, raw, nil)} {
				metadata := gjson.GetBytes(out, "usageMetadata")
				if len(metadata.Map()) != len(tt.want) {
					t.Errorf("unexpected fields in %s", metadata.Raw)
				}
				for field, want := range tt.want {
					if got := metadata.Get(field).Raw; got != want {
						t.Errorf("%s = %s, want %s: %s", field, got, want, out)
					}
				}
			}
		})
	}
}

func TestConvertOpenAIResponseToGemini_ReasoningAndTypedContent(t *testing.T) {
	type part struct {
		text    string
		thought bool
	}
	for _, tt := range []struct {
		name   string
		fields string
		want   []part
	}{
		{
			name:   "top-level precedes inline thought",
			fields: `"reasoning_content":"preferred","reasoning":"duplicate","reasoning_details":[{"text":"duplicate"}],"content":[{"type":"thinking","thinking":"duplicate"},{"type":"text","text":"answer"}]`,
			want:   []part{{"preferred", true}, {"answer", false}},
		},
		{
			name:   "reasoning alias",
			fields: `"reasoning_content":"","reasoning":"alias","content":"answer"`,
			want:   []part{{"alias", true}, {"answer", false}},
		},
		{
			name:   "reasoning summary and details",
			fields: `"reasoning_details":[{"type":"reasoning.summary","summary":"plan"},{"type":"reasoning.text","text":"check"}],"content":[{"type":"refusal","refusal":"refused"}]`,
			want:   []part{{"plan", true}, {"check", true}, {"refused", false}},
		},
		{
			name:   "typed parts retain order",
			fields: `"content":[{"type":"text","text":"before"},{"type":"reasoning","reasoning":"inline"},{"type":"unknown","text":"hidden"},{"type":"output_text","text":"after"}]`,
			want:   []part{{"before", false}, {"inline", true}, {"after", false}},
		},
	} {
		for _, wire := range []string{"message", "delta"} {
			t.Run(tt.name+"/"+wire, func(t *testing.T) {
				raw := []byte(`{"choices":[{"index":0,"` + wire + `":{` + tt.fields + `}}]}`)
				ctx := context.Background()
				var out []byte
				if wire == "delta" {
					var state any
					chunks := ConvertOpenAIResponseToGemini(ctx, "gpt-test", nil, nil, raw, &state)
					if len(chunks) != 1 {
						t.Fatalf("expected one output chunk, got %q", chunks)
					}
					out = chunks[0]
				} else {
					out = ConvertOpenAIResponseToGeminiNonStream(ctx, "gpt-test", nil, nil, raw, nil)
				}

				parts := gjson.GetBytes(out, "candidates.0.content.parts").Array()
				if len(parts) != len(tt.want) {
					t.Fatalf("want %d parts, got %s", len(tt.want), out)
				}
				for i, want := range tt.want {
					if parts[i].Get("text").String() != want.text || parts[i].Get("thought").Bool() != want.thought {
						t.Fatalf("part %d = %s, want %+v", i, parts[i].Raw, want)
					}
				}
			})
		}
	}
}

func TestConvertOpenAIResponseToGeminiStreamPreservesToolCallID(t *testing.T) {
	var param any
	ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_stream_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}}]}`), &param)
	out := ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`), &param)
	if len(out) == 0 {
		t.Fatalf("stream output is empty")
	}
	if got := gjson.GetBytes(out[len(out)-1], "candidates.0.content.parts.0.functionCall.id").String(); got != "call_stream_1" {
		t.Fatalf("functionCall.id = %q, want call_stream_1", got)
	}
	if got := gjson.GetBytes(out[len(out)-1], "candidates.0.content.parts.0.functionCall.args.q").String(); got != "x" {
		t.Fatalf("functionCall.args.q = %q, want x", got)
	}
}

func TestConvertOpenAIResponseToGeminiStream_NullFinishReasonIgnored(t *testing.T) {
	var param any

	// Contentless role chunk with a blank finish reason must leave the choice open.
	chunk1 := []byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"   "}]}`)
	out1 := ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, chunk1, &param)
	for i, chunk := range out1 {
		if fr := gjson.GetBytes(chunk, "candidates.0.finishReason"); fr.Exists() && fr.String() != "" {
			t.Fatalf("chunk1[%d] unexpectedly set finishReason = %q on non-final chunk; payload=%s", i, fr.String(), chunk)
		}
	}

	// Chunk 2: Reasoning delta with finish_reason: null
	chunk2 := []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"thinking..."},"finish_reason":null}]}`)
	out2 := ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, chunk2, &param)
	if len(out2) == 0 {
		t.Fatalf("expected output for reasoning chunk, got 0 chunks")
	}
	if gotThought := gjson.GetBytes(out2[0], "candidates.0.content.parts.0.thought").Bool(); !gotThought {
		t.Fatalf("expected thought: true on reasoning chunk, got false")
	}
	if gotText := gjson.GetBytes(out2[0], "candidates.0.content.parts.0.text").String(); gotText != "thinking..." {
		t.Fatalf("expected reasoning text %q, got %q", "thinking...", gotText)
	}
	for i, chunk := range out2 {
		if fr := gjson.GetBytes(chunk, "candidates.0.finishReason"); fr.Exists() && fr.String() != "" {
			t.Fatalf("chunk2[%d] unexpectedly set finishReason = %q on reasoning chunk; payload=%s", i, fr.String(), chunk)
		}
	}

	// Chunk 3: Contentless delta with finish_reason: "" (empty string should not be treated as stop)
	chunk3 := []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":""}]}`)
	out3 := ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, chunk3, &param)
	for i, chunk := range out3 {
		if fr := gjson.GetBytes(chunk, "candidates.0.finishReason"); fr.Exists() && fr.String() != "" {
			t.Fatalf("chunk3[%d] unexpectedly set finishReason = %q on contentless chunk with empty finish_reason; payload=%s", i, fr.String(), chunk)
		}
	}

	// Chunk 4: Content delta with finish_reason: null
	chunk4 := []byte(`{"choices":[{"index":0,"delta":{"content":"hello world"},"finish_reason":null}]}`)
	out4 := ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, chunk4, &param)
	if len(out4) == 0 {
		t.Fatalf("expected output for content chunk, got 0 chunks")
	}
	if gotText := gjson.GetBytes(out4[0], "candidates.0.content.parts.0.text").String(); gotText != "hello world" {
		t.Fatalf("expected content text %q, got %q", "hello world", gotText)
	}
	for i, chunk := range out4 {
		if fr := gjson.GetBytes(chunk, "candidates.0.finishReason"); fr.Exists() && fr.String() != "" {
			t.Fatalf("chunk4[%d] unexpectedly set finishReason = %q on non-final content chunk; payload=%s", i, fr.String(), chunk)
		}
	}

	// Chunk 5: Final chunk with finish_reason: "stop"
	chunk5 := []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	out5 := ConvertOpenAIResponseToGemini(context.Background(), "gpt-test", nil, nil, chunk5, &param)
	if len(out5) == 0 {
		t.Fatalf("expected output for final chunk, got 0 chunks")
	}
	if got := gjson.GetBytes(out5[len(out5)-1], "candidates.0.finishReason").String(); got != "STOP" {
		t.Fatalf("expected finishReason STOP on final chunk, got %q", got)
	}
}

func TestConvertOpenAIResponseToGeminiNonStream_NullFinishReasonIgnored(t *testing.T) {
	testCases := []struct {
		name    string
		payload []byte
	}{
		{
			name:    "null finish_reason",
			payload: []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":null}]}`),
		},
		{
			name:    "empty finish_reason",
			payload: []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":""}]}`),
		},
		{
			name:    "whitespace finish_reason",
			payload: []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"   "}]}`),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			out := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "gpt-test", nil, nil, tc.payload, nil)
			if fr := gjson.GetBytes(out, "candidates.0.finishReason"); fr.Exists() && fr.String() != "" {
				t.Fatalf("unexpected finishReason = %q on non-stream response; output=%s", fr.String(), out)
			}
		})
	}
}

func TestConvertOpenAIResponseToGeminiNonStream_PreservesToolCallThoughtSignature(t *testing.T) {
	up := []byte(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"},"extra_content":{"google":{"thought_signature":"SIG"}}},
		{"id":"c2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"London\"}"}}
	]}}]}`)
	res := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "", nil, nil, up, nil)
	if got := gjson.GetBytes(res, "candidates.0.content.parts.0.thoughtSignature").String(); got != "SIG" {
		t.Fatalf("candidates.0.content.parts.0.thoughtSignature = %q, want SIG; res=%s", got, string(res))
	}
	if gjson.GetBytes(res, "candidates.0.content.parts.1.thoughtSignature").Exists() {
		t.Fatalf("expected unsigned second tool call not to have thoughtSignature; res=%s", string(res))
	}
}

func TestConvertOpenAIResponseToGeminiStream_PreservesToolCallThoughtSignature(t *testing.T) {
	var param any
	ConvertOpenAIResponseToGemini(context.Background(), "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[
		{"index":1,"id":"c2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}},
		{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"},"extra_content":{"google":{"thought_signature":"SIG"}}}
	]}}]}`), &param)
	ConvertOpenAIResponseToGemini(context.Background(), "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[
		{"index":1,"function":{"arguments":"\"London\"}"}},
		{"index":0,"function":{"arguments":"\"Paris\"}"}}
	]}}]}`), &param)
	out := ConvertOpenAIResponseToGemini(context.Background(), "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`), &param)
	if len(out) == 0 {
		t.Fatalf("stream output is empty")
	}
	last := out[len(out)-1]
	if got := gjson.GetBytes(last, "candidates.0.content.parts.0.functionCall.id").String(); got != "c1" {
		t.Fatalf("candidates.0.content.parts.0.functionCall.id = %q, want c1; payload=%s", got, string(last))
	}
	if got := gjson.GetBytes(last, "candidates.0.content.parts.0.thoughtSignature").String(); got != "SIG" {
		t.Fatalf("candidates.0.content.parts.0.thoughtSignature = %q, want SIG; payload=%s", got, string(last))
	}
	if got := gjson.GetBytes(last, "candidates.0.content.parts.1.functionCall.id").String(); got != "c2" {
		t.Fatalf("candidates.0.content.parts.1.functionCall.id = %q, want c2; payload=%s", got, string(last))
	}
	if gjson.GetBytes(last, "candidates.0.content.parts.1.thoughtSignature").Exists() {
		t.Fatalf("expected unsigned second tool call not to have thoughtSignature; payload=%s", string(last))
	}

	// Single-chunk tool call with finish_reason in the same SSE delta
	var singleChunkParam any
	singleOut := ConvertOpenAIResponseToGemini(context.Background(), "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c_single","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Rome\"}"},"extra_content":{"google":{"thought_signature":"SIG_SINGLE"}}}]},"finish_reason":"tool_calls"}]}`), &singleChunkParam)
	if len(singleOut) == 0 {
		t.Fatalf("expected output for single-chunk tool_calls + finish_reason")
	}
	if got := gjson.GetBytes(singleOut[len(singleOut)-1], "candidates.0.content.parts.0.thoughtSignature").String(); got != "SIG_SINGLE" {
		t.Fatalf("single-chunk thoughtSignature = %q, want SIG_SINGLE; payload=%s", got, string(singleOut[len(singleOut)-1]))
	}

	// Reasoning/content in the same delta as tool_calls, plus signature arriving in a later delta without function
	var mixedParam any
	mixedChunk1 := ConvertOpenAIResponseToGemini(context.Background(), "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"checking weather","tool_calls":[{"index":0,"id":"c_mixed","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Berlin\"}"}}]}}]}`), &mixedParam)
	if len(mixedChunk1) != 1 || !gjson.GetBytes(mixedChunk1[0], "candidates.0.content.parts.0.thought").Bool() {
		t.Fatalf("expected reasoning chunk output on mixed delta, got %v", mixedChunk1)
	}
	ConvertOpenAIResponseToGemini(context.Background(), "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"extra_content":{"google":{"thought_signature":"SIG_LATE"}}}]}}]}`), &mixedParam)
	mixedOut := ConvertOpenAIResponseToGemini(context.Background(), "google/gemini-3-pro", nil, nil, []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`), &mixedParam)
	if len(mixedOut) == 0 {
		t.Fatalf("expected output when flushing mixed tool call")
	}
	if got := gjson.GetBytes(mixedOut[len(mixedOut)-1], "candidates.0.content.parts.0.thoughtSignature").String(); got != "SIG_LATE" {
		t.Fatalf("mixed/late-signature thoughtSignature = %q, want SIG_LATE; payload=%s", got, string(mixedOut[len(mixedOut)-1]))
	}
}

func TestConvertOpenAIResponseToGeminiStream_ContentWithFinishReason(t *testing.T) {
	var param any
	ConvertOpenAIResponseToGemini(context.Background(), "glm-test", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`), &param)
	out := ConvertOpenAIResponseToGemini(context.Background(), "glm-test", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`), &param)
	var text, reason string
	for _, c := range out {
		if s := gjson.GetBytes(c, "candidates.0.content.parts.0.text").String(); s != "" {
			text = s
		}
		if s := gjson.GetBytes(c, "candidates.0.finishReason").String(); s != "" {
			reason = s
		}
	}
	if text != "ok" || reason != "STOP" {
		t.Fatalf("want text ok and finishReason STOP, got text=%q reason=%q chunks=%q", text, reason, out)
	}

	var reasoningParam any
	reasoningOut := ConvertOpenAIResponseToGemini(context.Background(), "glm-test", nil, nil, []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"thinking"},"finish_reason":"length"}]}`), &reasoningParam)
	var thoughtText, reasoningFinish string
	for _, c := range reasoningOut {
		if gjson.GetBytes(c, "candidates.0.content.parts.0.thought").Bool() {
			thoughtText = gjson.GetBytes(c, "candidates.0.content.parts.0.text").String()
		}
		if s := gjson.GetBytes(c, "candidates.0.finishReason").String(); s != "" {
			reasoningFinish = s
		}
	}
	if thoughtText != "thinking" || reasoningFinish != "MAX_TOKENS" {
		t.Fatalf("want thoughtText thinking and finishReason MAX_TOKENS, got thought=%q reason=%q chunks=%q", thoughtText, reasoningFinish, reasoningOut)
	}
}
