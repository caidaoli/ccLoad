package chat_completions

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiResponseToOpenAI_FunctionCallNullOrNonObjectArgsUsesEmptyObject(t *testing.T) {
	const object = `{"n":9007199254740993,"query":"<tag>"}`
	for _, tc := range []struct {
		name, argsField, want string
	}{
		{"missing", "", "{}"},
		{"null", `,"args":null`, "{}"},
		{"array", `,"args":[1,2]`, "{}"},
		{"string", `,"args":"{}"`, "{}"},
		{"object", `,"args":` + object, object},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"ping"` + tc.argsField + `}}]},"finishReason":"STOP"}]}`)
			var state any
			chunks := ConvertGeminiResponseToOpenAI(t.Context(), "gemini", nil, nil, body, &state)
			if len(chunks) != 1 {
				t.Fatalf("expected one stream delta, got %q", chunks)
			}
			call := gjson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0.function")
			if call.Get("name").Str != "ping" || call.Get("arguments").Str != tc.want {
				t.Errorf("stream function = %s, want ping with arguments %q", call.Raw, tc.want)
			}
			out := ConvertGeminiResponseToOpenAINonStream(t.Context(), "gemini", nil, nil, body, nil)
			call = gjson.GetBytes(out, "choices.0.message.tool_calls.0.function")
			if call.Get("name").Str != "ping" || call.Get("arguments").Str != tc.want {
				t.Errorf("nonstream function = %s, want ping with arguments %q", call.Raw, tc.want)
			}
		})
	}
}

func TestGeminiChatNamelessNonObjectArgsPreservesDetachedSignature(t *testing.T) {
	for _, args := range []string{"null", "[1,2]"} {
		t.Run(args, func(t *testing.T) {
			placeholder := `{"functionCall":{"args":` + args + `},"thoughtSignature":"` + capturedGeminiToolCallThoughtSignature + `"}`
			var state any
			initial := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"ping","args":{}}}]}}]}`)
			ConvertGeminiResponseToOpenAI(t.Context(), "gemini", nil, nil, initial, &state)
			followup := []byte(`{"candidates":[{"content":{"parts":[` + placeholder + `]}}]}`)
			chunks := ConvertGeminiResponseToOpenAI(t.Context(), "gemini", nil, nil, followup, &state)
			if len(chunks) != 1 {
				t.Fatalf("expected signature-only tool delta, got %q", chunks)
			}
			call := gjson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0")
			if call.Get("index").Raw != "0" || call.Get("function").Exists() || call.Get("id").Exists() ||
				call.Get("extra_content.google.thought_signature").Str != capturedGeminiToolCallThoughtSignature {
				t.Fatalf("expected signature on preceding call, got %s", call.Raw)
			}
			full := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"ping","args":{}}},` + placeholder + `]},"finishReason":"STOP"}]}`)
			out := ConvertGeminiResponseToOpenAINonStream(t.Context(), "gemini", nil, nil, full, nil)
			calls := gjson.GetBytes(out, "choices.0.message.tool_calls").Array()
			if len(calls) != 1 || calls[0].Get("function.name").Str != "ping" ||
				calls[0].Get("extra_content.google.thought_signature").Str != capturedGeminiToolCallThoughtSignature {
				t.Fatalf("expected one signed call, got %q", out)
			}
			var standalone any
			if chunks := ConvertGeminiResponseToOpenAI(t.Context(), "gemini", nil, nil, followup, &standalone); len(chunks) != 0 {
				t.Fatalf("standalone placeholder emitted chunks: %q", chunks)
			}
		})
	}
}

func TestConvertGeminiResponseToOpenAICompletionTokensIncludeThoughts(t *testing.T) {
	var param any
	chunk := []byte(`{"usageMetadata":{"promptTokenCount":16,"candidatesTokenCount":5,"thoughtsTokenCount":42,"totalTokenCount":63}}`)

	result := ConvertGeminiResponseToOpenAI(context.Background(), "model", nil, nil, chunk, &param)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	completionTokens := gjson.GetBytes(result[0], "usage.completion_tokens")
	if !completionTokens.Exists() || completionTokens.Int() != 47 {
		t.Fatalf("completion_tokens = %s, want present with value 47. Output: %s", completionTokens.Raw, result[0])
	}
}

func TestConvertGeminiResponseToOpenAINonStreamCompletionTokensIncludeThoughts(t *testing.T) {
	response := []byte(`{"usageMetadata":{"promptTokenCount":16,"thoughtsTokenCount":42,"totalTokenCount":58}}`)

	result := ConvertGeminiResponseToOpenAINonStream(context.Background(), "model", nil, nil, response, nil)
	completionTokens := gjson.GetBytes(result, "usage.completion_tokens")
	if !completionTokens.Exists() || completionTokens.Int() != 42 {
		t.Fatalf("completion_tokens = %s, want present with value 42. Output: %s", completionTokens.Raw, result)
	}
}

func TestGeminiParameterlessToolCallArguments(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_ping","name":"ping"}}]},"finishReason":"STOP"}]}`)
	var state any
	chunks := ConvertGeminiResponseToOpenAI(ctx, "model", nil, nil, raw, &state)
	if len(chunks) != 1 {
		t.Fatalf("expected one tool delta before usage, got %q", chunks)
	}
	nonStream := ConvertGeminiResponseToOpenAINonStream(ctx, "model", nil, nil, raw, nil)
	for _, call := range []gjson.Result{
		gjson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0"),
		gjson.GetBytes(nonStream, "choices.0.message.tool_calls.0"),
	} {
		args := call.Get("function.arguments")
		if call.Get("id").String() != "call_ping" || args.Type != gjson.String || args.String() != "{}" {
			t.Fatalf("expected a function call with an empty argument object string, got %s", call.Raw)
		}
	}
}

func TestGeminiFinishReasonOnlyOnFinalChunk(t *testing.T) {
	ctx := context.Background()
	var param any

	chunk1 := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_dir","args":{"path":"C:/"}}}]}}],"usageMetadata":{"trafficType":"ON_DEMAND"}}`)
	result1 := ConvertGeminiResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)
	if len(result1) != 1 {
		t.Fatalf("expected 1 result from chunk1, got %d", len(result1))
	}
	fr1 := gjson.GetBytes(result1[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Fatalf("expected null finish_reason on tool chunk, got %v", fr1.String())
	}

	chunk2 := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_dir","args":{"path":"D:/"}}}]}}],"usageMetadata":{"trafficType":"ON_DEMAND"}}`)
	ConvertGeminiResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	chunk3 := []byte(`{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`)
	result3 := ConvertGeminiResponseToOpenAI(ctx, "model", nil, nil, chunk3, &param)
	if len(result3) != 1 {
		t.Fatalf("expected 1 result from chunk3, got %d", len(result3))
	}
	fr3 := gjson.GetBytes(result3[0], "choices.0.finish_reason").String()
	if fr3 != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %s", fr3)
	}
	nfr3 := gjson.GetBytes(result3[0], "choices.0.native_finish_reason").String()
	if nfr3 != "stop" {
		t.Fatalf("expected native_finish_reason stop, got %s", nfr3)
	}
}

func TestConvertGeminiResponseToOpenAINonStream_EmptyTextProducesEmptyString(t *testing.T) {
	response := []byte(`{"candidates":[{"content":{"parts":[{"text":""},{"text":"","thought":true}]},"finishReason":"STOP"}]}`)
	result := ConvertGeminiResponseToOpenAINonStream(context.Background(), "model", nil, nil, response, nil)

	content := gjson.GetBytes(result, "choices.0.message.content")
	if !content.Exists() || content.String() != "" || content.Type == gjson.Null {
		t.Fatalf("expected content to be empty string \"\", got %v (type %v)", content.Value(), content.Type)
	}

	reasoning := gjson.GetBytes(result, "choices.0.message.reasoning_content")
	if !reasoning.Exists() || reasoning.String() != "" || reasoning.Type == gjson.Null {
		t.Fatalf("expected reasoning_content to be empty string \"\", got %v (type %v)", reasoning.Value(), reasoning.Type)
	}
}

// Gemini 3.5 Transcribe returns its transcript in an audioTranscription part instead
// of the regular text part. Without handling it, the transcript is silently dropped
// and the OpenAI client sees an empty assistant message.
func TestConvertGeminiResponseToOpenAINonStream_AudioTranscriptionPartProducesContent(t *testing.T) {
	response := []byte(`{"candidates":[{"content":{"parts":[{"text":""},{"audioTranscription":{"text":"Hello world"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":185,"totalTokenCount":185}}`)
	result := ConvertGeminiResponseToOpenAINonStream(context.Background(), "model", nil, nil, response, nil)

	content := gjson.GetBytes(result, "choices.0.message.content")
	if !content.Exists() || content.String() != "Hello world" {
		t.Fatalf("expected content to carry the transcript, got %v. Output: %s", content.Raw, result)
	}
}

func TestConvertGeminiResponseToOpenAINonStream_SingleAudioTranscriptionPart(t *testing.T) {
	response := []byte(`{"candidates":[{"content":{"parts":[{"audioTranscription":{"text":"Single transcription part"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":185,"totalTokenCount":185}}`)
	result := ConvertGeminiResponseToOpenAINonStream(context.Background(), "model", nil, nil, response, nil)

	content := gjson.GetBytes(result, "choices.0.message.content")
	if !content.Exists() || content.String() != "Single transcription part" {
		t.Fatalf("expected content to carry the transcript, got %v. Output: %s", content.Raw, result)
	}
}

func TestConvertGeminiResponseToOpenAI_AudioTranscriptionPartStreamsContent(t *testing.T) {
	ctx := context.Background()
	var param any

	chunk := []byte(`{"candidates":[{"content":{"parts":[{"text":""},{"audioTranscription":{"text":"Testing one two three."}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":185,"candidatesTokenCount":8,"totalTokenCount":193}}`)
	result := ConvertGeminiResponseToOpenAI(ctx, "model", nil, nil, chunk, &param)
	if len(result) != 2 {
		t.Fatalf("expected a transcript chunk followed by completion, got %q", result)
	}

	content := gjson.GetBytes(result[0], "choices.0.delta.content")
	if !content.Exists() || content.String() != "Testing one two three." {
		t.Fatalf("expected delta.content to carry the transcript, got %v. Output: %s", content.Raw, result[0])
	}
}

func TestConvertGeminiResponseToOpenAINonStream_TextPrecedenceOverAudioTranscription(t *testing.T) {
	response := []byte(`{"candidates":[{"content":{"parts":[{"text":"explicit text","audioTranscription":{"text":"ignored transcription"}}]},"finishReason":"STOP"}]}`)
	result := ConvertGeminiResponseToOpenAINonStream(context.Background(), "model", nil, nil, response, nil)

	content := gjson.GetBytes(result, "choices.0.message.content")
	if !content.Exists() || content.String() != "explicit text" {
		t.Fatalf("expected content to prioritize explicit text, got %v. Output: %s", content.Raw, result)
	}
}

func TestConvertGeminiResponseToOpenAI_PinsSanitizedNameMapPerStream(t *testing.T) {
	ctx := context.Background()

	// 1. Only the first chunk carries the request with a sanitized tool name.
	var param any
	reqWithTool := []byte(`{"model":"gemini-2.5-flash","tools":[{"name":"mcp/search"}]}`)
	chunk1 := []byte(`{"candidates":[{"content":{"parts":[{"text":"thinking"}]}}]}`)
	chunk2 := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"mcp_search","args":{"q":"test"}}}]}}]}`)

	ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-flash", reqWithTool, nil, chunk1, &param)
	out := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-flash", nil, nil, chunk2, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk output, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "mcp/search" {
		t.Fatalf("restored tool name = %q, want mcp/search", got)
	}

	// 2. When the first chunk's request produces a nil SanitizedNameMap, later chunks must not rebuild it.
	var nilMapParam any
	reqNoSanitize := []byte(`{"model":"gemini-2.5-flash","tools":[{"name":"plain_tool"}]}`)
	ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-flash", reqNoSanitize, nil, chunk1, &nilMapParam)
	outNil := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-flash", reqWithTool, nil, chunk2, &nilMapParam)
	if len(outNil) != 1 {
		t.Fatalf("expected 1 chunk output, got %d", len(outNil))
	}
	if got := gjson.GetBytes(outNil[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "mcp_search" {
		t.Fatalf("tool name = %q, want mcp_search (must not rebuild nil SanitizedNameMap on later chunks)", got)
	}
}

func TestOpenAIToolNamesRoundTripThroughGemini(t *testing.T) {
	const declarations = `"tools":[{"type":"function","function":{"name":"mcp/search"}},{"type":"function","function":{"name":"mcp?search"}}]`
	for _, tc := range []struct {
		name, choice, wantName, wantMode string
	}{
		{"allowed tool", `,"tool_choice":{"type":"allowed_tools","allowed_tools":{"tools":[{"type":"function","function":{"name":"mcp/search"}}]}}`, "mcp/search", "AUTO"},
		{"ambiguous tools", "", "mcp_search", "NONE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(`{"messages":[{"role":"user","content":"search"}],` + declarations + tc.choice + `}`)
			request, err := ConvertOpenAIRequestToGemini("gemini", original, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(request, "toolConfig.functionCallingConfig.mode").String(); got != tc.wantMode {
				t.Fatalf("function mode = %q, want %q", got, tc.wantMode)
			}
			wireName := gjson.GetBytes(request, "tools.0.functionDeclarations.0.name").String()
			if wireName != "mcp_search" {
				t.Fatalf("declared Gemini tool = %q, want mcp_search", wireName)
			}
			upstream := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + wireName + `","args":{"q":"test"}}}]}}]}`)
			var state any
			stream := ConvertGeminiResponseToOpenAI(t.Context(), "gemini", original, request, upstream, &state)
			if len(stream) != 1 {
				t.Fatalf("expected one tool delta, got %q", stream)
			}
			if got := gjson.GetBytes(stream[0], "choices.0.delta.tool_calls.0.function.name").String(); got != tc.wantName {
				t.Errorf("stream tool name = %q, want %q", got, tc.wantName)
			}
			response := ConvertGeminiResponseToOpenAINonStream(t.Context(), "gemini", original, request, upstream, nil)
			if got := gjson.GetBytes(response, "choices.0.message.tool_calls.0.function.name").String(); got != tc.wantName {
				t.Errorf("non-stream tool name = %q, want %q", got, tc.wantName)
			}
		})
	}
}

func TestGeminiChatNonStreamNullableFinishReasonAndUsage(t *testing.T) {
	for _, tc := range []struct{ reason, want, native string }{
		{"", "null", "null"},
		{`null`, "null", "null"},
		{`""`, "null", "null"},
		{`"  "`, "null", "null"},
		{`false`, "null", "null"},
		{`" max_tokens "`, `"length"`, `"max_tokens"`},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			field := ""
			if tc.reason != "" {
				field = `,"finishReason":` + tc.reason
			}
			input := []byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]}` + field + `}],"usageMetadata":null}`)
			out := ConvertGeminiResponseToOpenAINonStream(t.Context(), "gemini", nil, nil, input, nil)
			result := gjson.ParseBytes(out)
			if result.Get("choices.0.finish_reason").Raw != tc.want || result.Get("choices.0.native_finish_reason").Raw != tc.native || result.Get("usage").Exists() {
				t.Fatalf("unexpected finish reason or usage: %s", out)
			}
		})
	}
}

func TestGeminiChatStreamNullUsagePreservesDetachedSignatureAndPendingZero(t *testing.T) {
	const signature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"
	var state any
	convert := func(chunk string) [][]byte {
		return ConvertGeminiResponseToOpenAI(t.Context(), "gemini", nil, nil, []byte(chunk), &state)
	}
	chunks := convert(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":null,"cpaUsageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"totalTokenCount":0}}`)
	if len(chunks) != 1 || gjson.GetBytes(chunks[0], "choices.0.finish_reason").Raw != "null" || gjson.GetBytes(chunks[0], "usage").Exists() {
		t.Fatalf("null usage finalized the tool delta: %q", chunks)
	}
	chunks = convert(`{"candidates":[{"finishReason":null,"content":{"parts":[{"thoughtSignature":"` + signature + `"}]}}],"cpaUsageMetadata":null}`)
	if len(chunks) != 1 || gjson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0.extra_content.google.thought_signature").String() != signature {
		t.Fatalf("missing detached signature: %q", chunks)
	}
	chunks = convert(`[DONE]`)
	if len(chunks) != 2 || string(chunks[1]) != "[DONE]" ||
		gjson.GetBytes(chunks[0], "choices.0.finish_reason").String() != "tool_calls" || gjson.GetBytes(chunks[0], "usage.total_tokens").Raw != "0" {
		t.Fatalf("expected terminal tool_calls with real zero usage and one SSE marker: %q", chunks)
	}
	if chunks := convert(`[DONE]`); len(chunks) != 0 {
		t.Fatalf("repeated [DONE] emitted a second terminal: %q", chunks)
	}
}

func TestGeminiStreamSeparatesToolDeltasAndKeepsLateUsage(t *testing.T) {
	var state any
	convert := func(chunk string) [][]byte {
		return ConvertGeminiResponseToOpenAI(t.Context(), "gemini", nil, nil, []byte(chunk), &state)
	}
	payload := `{"candidates":[{"index":1,"content":{"parts":[{"functionCall":{"name":"one","args":{}}},{"functionCall":{"name":"two","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":6}}`
	chunks := convert(payload)
	if len(chunks) != 2 {
		t.Fatalf("expected a tool delta and a terminal chunk, got %q", chunks)
	}
	delta, terminal := gjson.ParseBytes(chunks[0]), gjson.ParseBytes(chunks[1])
	if delta.Get("choices.0.index").Int() != 1 || delta.Get("choices.0.delta.tool_calls.#").Int() != 2 || delta.Get("choices.0.finish_reason").Type != gjson.Null {
		t.Fatalf("expected both tool calls before finish_reason: %s", delta.Raw)
	}
	if terminal.Get("choices.0.index").Int() != 1 || terminal.Get("choices.0.finish_reason").String() != "tool_calls" || terminal.Get("choices.0.delta").Raw != `{}` || terminal.Get("usage.total_tokens").Int() != 6 {
		t.Fatalf("expected terminal reason with empty delta: %s", terminal.Raw)
	}
	if chunks := convert(payload); len(chunks) != 0 {
		t.Fatalf("replayed terminal payload emitted another choice: %q", chunks)
	}
	chunks = convert(`{"usageMetadata":{"totalTokenCount":7}}`)
	if len(chunks) != 1 || gjson.GetBytes(chunks[0], "choices").Raw != `[]` || gjson.GetBytes(chunks[0], "usage.total_tokens").Int() != 7 {
		t.Fatalf("expected late usage without repeating finish_reason: %q", chunks)
	}
	chunks = convert(`{"candidates":[{"index":1,"finishReason":"STOP"},{"index":0,"content":{"parts":[{"text":"second choice"}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":8}}`)
	if len(chunks) != 2 || gjson.GetBytes(chunks[0], "choices.0.delta.content").String() != "second choice" ||
		gjson.GetBytes(chunks[1], "choices.0.index").Int() != 0 || gjson.GetBytes(chunks[1], "choices.0.finish_reason").String() != "stop" {
		t.Fatalf("expected only the unfinished choice to emit content and a terminal chunk: %q", chunks)
	}
	if chunks := convert(`[DONE]`); len(chunks) != 1 || string(chunks[0]) != `[DONE]` {
		t.Fatalf("expected only the final SSE marker: %q", chunks)
	}
}

func TestGeminiStreamFlushesAllFinishedChoicesWhenUsageArrives(t *testing.T) {
	for _, tc := range []struct {
		name, finalFrame string
		wantDelta        bool
	}{
		{"usage with second choice", `{"candidates":[{"index":2,"content":{"parts":[{"text":"second"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"totalTokenCount":7}}`, true},
		{"usage alone", `{"usageMetadata":{"totalTokenCount":7}}`, false},
		{"usage with empty finish reason", `{"candidates":[{"index":2,"finishReason":""}],"usageMetadata":{"totalTokenCount":7}}`, false},
		{"usage with null finish reason", `{"candidates":[{"index":2,"finishReason":null}],"usageMetadata":{"totalTokenCount":7}}`, false},
		{"usage with whitespace finish reason", `{"candidates":[{"index":2,"finishReason":"  "}],"usageMetadata":{"totalTokenCount":7}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			convert := func(chunk string) [][]byte {
				return ConvertGeminiResponseToOpenAI(t.Context(), "gemini", nil, nil, []byte(chunk), &state)
			}
			chunks := convert(`{"candidates":[{"index":1,"content":{"parts":[{"text":"first"}]},"finishReason":"STOP"}]}`)
			if len(chunks) != 1 || gjson.GetBytes(chunks[0], "choices.0.finish_reason").Type != gjson.Null {
				t.Fatalf("expected first delta while usage is pending: %q", chunks)
			}
			if !tc.wantDelta {
				convert(`{"candidates":[{"index":2,"content":{"parts":[{"text":"second"}]},"finishReason":"MAX_TOKENS"}]}`)
			}
			chunks = convert(tc.finalFrame)
			if tc.wantDelta {
				if len(chunks) != 3 || gjson.GetBytes(chunks[0], "choices.0.delta.content").String() != "second" ||
					gjson.GetBytes(chunks[0], "choices.0.finish_reason").Type != gjson.Null || gjson.GetBytes(chunks[0], "usage").Exists() {
					t.Fatalf("expected second delta before both terminal chunks: %q", chunks)
				}
				chunks = chunks[1:]
			}
			if len(chunks) != 2 {
				t.Fatalf("expected two terminal chunks: %q", chunks)
			}
			for i, chunk := range chunks {
				terminal := gjson.ParseBytes(chunk)
				choice := terminal.Get("choices.0")
				finish, native := "stop", "stop"
				if i == 1 {
					finish, native = "length", "max_tokens"
				}
				if choice.Get("index").Int() != int64(i+1) || choice.Get("finish_reason").String() != finish ||
					choice.Get("native_finish_reason").String() != native || choice.Get("delta").Raw != `{}` {
					t.Fatalf("bad terminal choice: %s", terminal.Raw)
				}
				if usage := terminal.Get("usage.total_tokens"); i == 0 && usage.Exists() || i == 1 && usage.Int() != 7 {
					t.Fatalf("expected global usage only on the last terminal chunk: %s", terminal.Raw)
				}
			}
		})
	}
}

func TestConvertGeminiResponseToOpenAI_NonStopFinishReasonsMapToContentFilter(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name             string
		upstreamReason   string
		wantFinishReason string
		wantNativeReason string
		includeEmptyText bool
	}{
		{
			name:             "MALFORMED_RESPONSE without parts",
			upstreamReason:   "MALFORMED_RESPONSE",
			wantFinishReason: "content_filter",
			wantNativeReason: "malformed_response",
		},
		{
			name:             "MALFORMED_FUNCTION_CALL without parts",
			upstreamReason:   "MALFORMED_FUNCTION_CALL",
			wantFinishReason: "content_filter",
			wantNativeReason: "malformed_function_call",
		},
		{
			name:             "SAFETY",
			upstreamReason:   "SAFETY",
			wantFinishReason: "content_filter",
			wantNativeReason: "safety",
		},
		{
			name:             "RECITATION",
			upstreamReason:   "RECITATION",
			wantFinishReason: "content_filter",
			wantNativeReason: "recitation",
		},
		{
			name:             "PROHIBITED_CONTENT",
			upstreamReason:   "PROHIBITED_CONTENT",
			wantFinishReason: "content_filter",
			wantNativeReason: "prohibited_content",
		},
	}

	for _, tc := range cases {
		t.Run("stream/"+tc.name, func(t *testing.T) {
			var param any
			chunk := []byte(`{"candidates":[{"content":{"role":"model"},"finishReason":"` + tc.upstreamReason + `"}],"usageMetadata":{"promptTokenCount":481701,"candidatesTokenCount":16,"totalTokenCount":482881,"cachedContentTokenCount":479652,"thoughtsTokenCount":1164},"modelVersion":"gemini-4-argon","responseId":"EAjaaoCDC_Ooq8YP3f-2wAQ"}`)
			out := ConvertGeminiResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, chunk, &param)
			if len(out) != 1 {
				t.Fatalf("expected 1 chunk, got %d", len(out))
			}
			if got := gjson.GetBytes(out[0], "choices.0.finish_reason").String(); got != tc.wantFinishReason {
				t.Fatalf("stream finish_reason = %q, want %q; payload=%s", got, tc.wantFinishReason, out[0])
			}
			if got := gjson.GetBytes(out[0], "choices.0.native_finish_reason").String(); got != tc.wantNativeReason {
				t.Fatalf("stream native_finish_reason = %q, want %q; payload=%s", got, tc.wantNativeReason, out[0])
			}
		})

		t.Run("nonstream/"+tc.name, func(t *testing.T) {
			raw := []byte(`{"candidates":[{"content":{"role":"model"},"finishReason":"` + tc.upstreamReason + `"}],"usageMetadata":{"promptTokenCount":481701,"candidatesTokenCount":16,"totalTokenCount":482881},"modelVersion":"gemini-4-argon","responseId":"EAjaaoCDC_Ooq8YP3f-2wAQ"}`)
			out := ConvertGeminiResponseToOpenAINonStream(ctx, "gemini-4-argon", nil, nil, raw, nil)
			if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != tc.wantFinishReason {
				t.Fatalf("non-stream finish_reason = %q, want %q; payload=%s", got, tc.wantFinishReason, out)
			}
			if got := gjson.GetBytes(out, "choices.0.native_finish_reason").String(); got != tc.wantNativeReason {
				t.Fatalf("non-stream native_finish_reason = %q, want %q; payload=%s", got, tc.wantNativeReason, out)
			}
		})
	}
}

func TestConvertGeminiResponseToOpenAI_FinishReasonWithoutInlineUsage(t *testing.T) {
	ctx := context.Background()

	t.Run("finishReason chunk followed by standalone usage chunk", func(t *testing.T) {
		var param any
		chunk1 := []byte(`{"candidates":[{"content":{"role":"model"},"finishReason":"MALFORMED_RESPONSE"}],"modelVersion":"gemini-4-argon","responseId":"resp-split-1"}`)
		chunk2 := []byte(`{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5,"totalTokenCount":105},"modelVersion":"gemini-4-argon","responseId":"resp-split-1"}`)

		out1 := ConvertGeminiResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, chunk1, &param)
		out2 := ConvertGeminiResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, chunk2, &param)
		done := ConvertGeminiResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, []byte("[DONE]"), &param)

		var all [][]byte
		all = append(all, out1...)
		all = append(all, out2...)
		all = append(all, done...)

		var terminals [][]byte
		for _, c := range all {
			if gjson.GetBytes(c, "choices.0.finish_reason").Value() != nil {
				terminals = append(terminals, c)
			}
		}
		if len(terminals) != 1 {
			t.Fatalf("expected exactly 1 chunk with non-null finish_reason, got %d; chunks=%q", len(terminals), all)
		}
		terminal := terminals[0]
		if got := gjson.GetBytes(terminal, "choices.0.finish_reason").String(); got != "content_filter" {
			t.Fatalf("finish_reason = %q, want content_filter; payload=%s", got, terminal)
		}
		if got := gjson.GetBytes(terminal, "choices.0.native_finish_reason").String(); got != "malformed_response" {
			t.Fatalf("native_finish_reason = %q, want malformed_response; payload=%s", got, terminal)
		}
		if got := gjson.GetBytes(terminal, "usage.prompt_tokens").Int(); got != 100 {
			t.Fatalf("usage.prompt_tokens = %d, want 100; payload=%s", got, terminal)
		}
	})

	t.Run("finishReason chunk with cpaUsageMetadata finalized on DONE", func(t *testing.T) {
		var param any
		chunk1 := []byte(`{"candidates":[{"content":{"role":"model"},"finishReason":"MALFORMED_RESPONSE"}],"cpaUsageMetadata":{"promptTokenCount":200,"candidatesTokenCount":8,"totalTokenCount":208},"modelVersion":"gemini-4-argon","responseId":"resp-done-1"}`)

		out1 := ConvertGeminiResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, chunk1, &param)
		done := ConvertGeminiResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, []byte("[DONE]"), &param)

		var all [][]byte
		all = append(all, out1...)
		all = append(all, done...)

		var terminals [][]byte
		for _, c := range all {
			if gjson.GetBytes(c, "choices.0.finish_reason").Value() != nil {
				terminals = append(terminals, c)
			}
		}
		if len(terminals) != 1 {
			t.Fatalf("expected exactly 1 chunk with non-null finish_reason on [DONE], got %d; chunks=%q", len(terminals), all)
		}
		terminal := terminals[0]
		if got := gjson.GetBytes(terminal, "choices.0.finish_reason").String(); got != "content_filter" {
			t.Fatalf("finish_reason = %q, want content_filter; payload=%s", got, terminal)
		}
		if got := gjson.GetBytes(terminal, "choices.0.native_finish_reason").String(); got != "malformed_response" {
			t.Fatalf("native_finish_reason = %q, want malformed_response; payload=%s", got, terminal)
		}
		if got := gjson.GetBytes(terminal, "usage.prompt_tokens").Int(); got != 200 {
			t.Fatalf("usage.prompt_tokens = %d, want 200; payload=%s", got, terminal)
		}
		if extraDone := ConvertGeminiResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, []byte("[DONE]"), &param); len(extraDone) != 0 {
			t.Fatalf("expected repeated [DONE] to emit no chunks, got %q", extraDone)
		}
	})

	t.Run("finishReason chunk without any usage finalized on DONE", func(t *testing.T) {
		var param any
		chunk1 := []byte(`{"candidates":[{"content":{"role":"model"},"finishReason":"SAFETY"}],"modelVersion":"gemini-4-argon","responseId":"resp-nousage-1"}`)

		out1 := ConvertGeminiResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, chunk1, &param)
		done := ConvertGeminiResponseToOpenAI(ctx, "gemini-4-argon", nil, nil, []byte("[DONE]"), &param)
		if len(done) != 2 || !bytes.Equal(done[1], []byte("[DONE]")) {
			t.Fatalf("expected terminal chunk followed by [DONE], got %q (out1=%q)", done, out1)
		}
		if got := gjson.GetBytes(done[0], "choices.0.finish_reason").String(); got != "content_filter" {
			t.Fatalf("finish_reason = %q, want content_filter; payload=%s", got, done[0])
		}
		if got := gjson.GetBytes(done[0], "choices.0.native_finish_reason").String(); got != "safety" {
			t.Fatalf("native_finish_reason = %q, want safety; payload=%s", got, done[0])
		}
	})
}

func TestConvertGeminiResponseToOpenAI_Done(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name             string
		chunk            string
		wantFinishReason string
	}{
		{
			name:  "text without finish reason",
			chunk: `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`,
		},
		{
			name:             "complete tool without usage",
			chunk:            `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},"finishReason":"STOP"}]}`,
			wantFinishReason: "tool_calls",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var param any
			out := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, []byte(tc.chunk), &param)
			if len(out) != 1 || gjson.GetBytes(out[0], "choices.0.finish_reason").Type != gjson.Null {
				t.Fatalf("unexpected content chunk: %q", out)
			}
			done := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, []byte("data: [DONE]"), &param)
			if tc.wantFinishReason != "" {
				if len(done) != 2 || gjson.GetBytes(done[0], "choices.0.delta").Raw != `{}` || gjson.GetBytes(done[0], "choices.0.finish_reason").String() != tc.wantFinishReason {
					t.Fatalf("expected empty-delta terminal chunk before [DONE], got %q", done)
				}
				done = done[1:]
			}
			if len(done) != 1 || !bytes.Equal(done[0], []byte("[DONE]")) {
				t.Fatalf("expected [DONE], got %q", done)
			}
			if again := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, []byte("[DONE]"), &param); len(again) != 0 {
				t.Fatalf("repeated [DONE] emitted chunks: %q", again)
			}
		})
	}

	t.Run("invalid partialArgs cannot complete", func(t *testing.T) {
		var param any
		chunk := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","partialArgs":[{"jsonPath":"$.items[999999999]","stringValue":"x"}],"willContinue":false}}]},"finishReason":"STOP"}]}`)
		ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, chunk, &param)
		if done := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, []byte("[DONE]"), &param); len(done) != 0 {
			t.Fatalf("invalid arguments emitted successful completion: %q", done)
		}
		if param.(interface{ ToolInputError() error }).ToolInputError() == nil {
			t.Fatal("invalid arguments must retain the conversion error")
		}
	})
}

func TestConvertGeminiResponseToOpenAI_ParallelToolCallsAcrossChunks(t *testing.T) {
	ctx := context.Background()
	var param any
	chunk1 := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_utc","name":"get_time","args":{"tz":"UTC"}}}]}}],"modelVersion":"gemini-3-flash","responseId":"resp-parallel-1"}`)
	chunk2 := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_scl","name":"get_time","args":{"tz":"America/Santiago"}}},{"functionCall":{"id":"call_tyo","name":"get_time","args":{"tz":"Asia/Tokyo"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-3-flash","responseId":"resp-parallel-1"}`)

	var indices []int64
	var ids []string
	for _, chunk := range [][]byte{chunk1, chunk2} {
		for _, out := range ConvertGeminiResponseToOpenAI(ctx, "gemini-3-flash", nil, nil, chunk, &param) {
			for _, tc := range gjson.GetBytes(out, "choices.0.delta.tool_calls").Array() {
				indices = append(indices, tc.Get("index").Int())
				ids = append(ids, tc.Get("id").String())
			}
		}
	}
	if len(indices) != 3 || indices[0] != 0 || indices[1] != 1 || indices[2] != 2 {
		t.Fatalf("parallel tool call indices across chunks = %v, want [0 1 2]", indices)
	}
	if len(ids) != 3 || ids[0] != "call_utc" || ids[1] != "call_scl" || ids[2] != "call_tyo" {
		t.Fatalf("parallel tool call ids across chunks = %v, want [call_utc call_scl call_tyo]", ids)
	}
}

func TestConvertGeminiResponseToOpenAI_VertexStreamingPartialArgs(t *testing.T) {
	ctx := context.Background()
	var param any
	chunks := [][]byte{
		[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"New ","willContinue":true}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-partial-1"}`),
		[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.city","stringValue":"York"}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-partial-1"}`),
		[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-2.5-pro","responseId":"resp-partial-1"}`),
	}

	var (
		callID       string
		callName     string
		argsBuilder  strings.Builder
		firstChunkOK bool
	)
	for chunkIdx, chunk := range chunks {
		outs := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, chunk, &param)
		for _, out := range outs {
			tcs := gjson.GetBytes(out, "choices.0.delta.tool_calls")
			if !tcs.Exists() || !tcs.IsArray() {
				continue
			}
			for _, tc := range tcs.Array() {
				if gotIdx := tc.Get("index").Int(); gotIdx != 0 {
					t.Fatalf("chunk %d tool_call index = %d, want 0; payload=%s", chunkIdx, gotIdx, out)
				}
				if id := tc.Get("id").String(); id != "" {
					if callID != "" && callID != id {
						t.Fatalf("chunk %d changed tool_call id from %q to %q", chunkIdx, callID, id)
					}
					callID = id
				}
				if name := tc.Get("function.name").String(); name != "" {
					callName = name
				}
				argDelta := tc.Get("function.arguments").String()
				argsBuilder.WriteString(argDelta)
				if chunkIdx == 0 && strings.Contains(argDelta, "New ") {
					firstChunkOK = true
				}
			}
		}
	}
	if !firstChunkOK {
		t.Fatalf("expected first chunk to stream 'New ' immediately, got total args %q", argsBuilder.String())
	}
	if callName != "get_weather" {
		t.Fatalf("tool call name = %q, want get_weather", callName)
	}
	if got := argsBuilder.String(); got != `{"city":"New York"}` {
		t.Fatalf("concatenated arguments = %q, want %q", got, `{"city":"New York"}`)
	}

	t.Run("multiple same-name calls and complex types", func(t *testing.T) {
		var p any
		streamChunks := [][]byte{
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"Tok","willContinue":true}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-multi"}`),
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.city","stringValue":"yo","willContinue":true}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-multi"}`),
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.city"},{"jsonPath":"$.days","numberValue":0},{"jsonPath":"$.metric","boolValue":false},{"jsonPath":"$.extra","nullValue":"NULL_VALUE"}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-multi"}`),
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-multi"}`),
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"Os","willContinue":true}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-multi"}`),
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.city","stringValue":"aka"}]}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-2.5-pro","responseId":"resp-multi"}`),
		}

		idsByIndex := make(map[int64]string)
		argsByIndex := make(map[int64]*strings.Builder)
		var finishReason string
		for _, ch := range streamChunks {
			for _, out := range ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, ch, &p) {
				if fr := gjson.GetBytes(out, "choices.0.finish_reason"); fr.Exists() && fr.Type != gjson.Null {
					finishReason = fr.String()
				}
				for _, tc := range gjson.GetBytes(out, "choices.0.delta.tool_calls").Array() {
					idx := tc.Get("index").Int()
					if id := tc.Get("id").String(); id != "" {
						idsByIndex[idx] = id
					}
					if argsByIndex[idx] == nil {
						argsByIndex[idx] = &strings.Builder{}
					}
					argsByIndex[idx].WriteString(tc.Get("function.arguments").String())
				}
			}
		}
		if len(idsByIndex) != 2 || idsByIndex[0] == "" || idsByIndex[1] == "" || idsByIndex[0] == idsByIndex[1] {
			t.Fatalf("expected 2 distinct tool call IDs at indices 0 and 1, got %v", idsByIndex)
		}
		if got0 := argsByIndex[0].String(); got0 != `{"city":"Tokyo","days":0,"metric":false,"extra":null}` {
			t.Fatalf("call 0 args = %q", got0)
		}
		if got1 := argsByIndex[1].String(); got1 != `{"city":"Osaka"}` {
			t.Fatalf("call 1 args = %q", got1)
		}
		if finishReason != "tool_calls" {
			t.Fatalf("finish_reason = %q, want tool_calls", finishReason)
		}
	})

	t.Run("single-chunk willContinue false parameterless call and empty partialArgs with complete args", func(t *testing.T) {
		var p1 any
		cPing := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"ping","willContinue":false}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7},"modelVersion":"gemini-2.5-pro","responseId":"resp-ping"}`)
		outs1 := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, cPing, &p1)
		if got := gjson.GetBytes(outs1[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "{}" {
			t.Fatalf("ping arguments = %q, want {}", got)
		}

		var p2 any
		cArgs := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"},"partialArgs":[]}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7},"modelVersion":"gemini-2.5-pro","responseId":"resp-args"}`)
		outs2 := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, cArgs, &p2)
		if got := gjson.GetBytes(outs2[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != `{"city":"Paris"}` {
			t.Fatalf("args fallback = %q", got)
		}
	})

	t.Run("continuation chunk repeating tool name without id continues active call", func(t *testing.T) {
		var p any
		chunks := [][]byte{
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"New ","willContinue":true}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-repeat-name"}`),
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"York"}],"willContinue":false}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-2.5-pro","responseId":"resp-repeat-name"}`),
			[]byte("[DONE]"),
		}
		var argBuf strings.Builder
		var finishReason string
		var idCount int
		for _, ch := range chunks {
			for _, out := range ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, ch, &p) {
				for _, tc := range gjson.GetBytes(out, "choices.0.delta.tool_calls").Array() {
					if tc.Get("id").String() != "" {
						idCount++
					}
					argBuf.WriteString(tc.Get("function.arguments").String())
				}
				if fr := gjson.GetBytes(out, "choices.0.finish_reason"); fr.Exists() && fr.Type != gjson.Null {
					finishReason = fr.String()
				}
			}
		}
		if idCount != 1 {
			t.Fatalf("expected 1 tool call start, got %d", idCount)
		}
		if got := argBuf.String(); got != `{"city":"New York"}` {
			t.Fatalf("arguments = %q, want %q", got, `{"city":"New York"}`)
		}
		if finishReason != "tool_calls" {
			t.Fatalf("finish_reason = %q, want tool_calls", finishReason)
		}
	})

	t.Run("mid-stream empty text and thoughtSignature do not interrupt active call", func(t *testing.T) {
		var p any
		chunks := [][]byte{
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"Ber","willContinue":true}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-mid-sig"}`),
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"","thoughtSignature":"sig-mid"}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-mid-sig"}`),
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.city","stringValue":"lin"}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-mid-sig"}`),
			[]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-2.5-pro","responseId":"resp-mid-sig"}`),
			[]byte("[DONE]"),
		}
		var argBuf strings.Builder
		var finishReason string
		for _, ch := range chunks {
			for _, out := range ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, ch, &p) {
				for _, tc := range gjson.GetBytes(out, "choices.0.delta.tool_calls").Array() {
					argBuf.WriteString(tc.Get("function.arguments").String())
				}
				if fr := gjson.GetBytes(out, "choices.0.finish_reason"); fr.Exists() && fr.Type != gjson.Null {
					finishReason = fr.String()
				}
			}
		}
		if got := argBuf.String(); got != `{"city":"Berlin"}` {
			t.Fatalf("arguments = %q, want %q", got, `{"city":"Berlin"}`)
		}
		if finishReason != "tool_calls" {
			t.Fatalf("finish_reason = %q, want tool_calls", finishReason)
		}
	})

	t.Run("replaced unfinished call does not fabricate completion", func(t *testing.T) {
		var p any
		c1 := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_a","name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"New ","willContinue":true}],"willContinue":true}}]}}],"modelVersion":"gemini-2.5-pro","responseId":"resp-replace"}`)
		c2 := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_b","name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"LA"}],"willContinue":false}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-2.5-pro","responseId":"resp-replace"}`)
		var all [][]byte
		all = append(all, ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, c1, &p)...)
		all = append(all, ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, c2, &p)...)
		all = append(all, ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, []byte("[DONE]"), &p)...)
		for _, out := range all {
			if bytes.Equal(out, []byte("[DONE]")) {
				t.Fatalf("unfinished replacement emitted [DONE]")
			}
			if fr := gjson.GetBytes(out, "choices.0.finish_reason"); fr.Exists() && fr.Type != gjson.Null {
				t.Fatalf("unexpected finish_reason when call_a never completed: %s", out)
			}
		}
	})

	t.Run("interrupted stream does not fabricate completion", func(t *testing.T) {
		var p any
		incomplete := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","partialArgs":[{"jsonPath":"$.city","stringValue":"New ","willContinue":true}],"willContinue":true}}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-pro","responseId":"resp-trunc"}`)
		outs := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, incomplete, &p)
		doneOuts := ConvertGeminiResponseToOpenAI(ctx, "gemini-2.5-pro", nil, nil, []byte("[DONE]"), &p)
		all := append(outs, doneOuts...)
		for _, out := range all {
			if bytes.Equal(out, []byte("[DONE]")) {
				t.Fatalf("interrupted tool call emitted [DONE]")
			}
			if fr := gjson.GetBytes(out, "choices.0.finish_reason"); fr.Exists() && fr.Type != gjson.Null {
				t.Fatalf("unexpected finish_reason on interrupted tool call stream: %s", out)
			}
		}
	})
}

func BenchmarkConvertGeminiResponseToOpenAI_TextDelta(b *testing.B) {
	request := []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"` + strings.Repeat("x", 1024*1024) + `"}]}`)
	delta := []byte(`{"candidates":[{"content":{"parts":[{"text":"token "}]}}],"modelVersion":"gemini-2.5-flash","responseId":"resp_bench"}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var param any
		for j := 0; j < 64; j++ {
			ConvertGeminiResponseToOpenAI(context.Background(), "gemini-2.5-flash", request, nil, delta, &param)
		}
	}
}

func TestConvertGeminiResponseToOpenAI_RejectsContentDuringPartialArgs(t *testing.T) {
	for _, part := range []string{`{"text":"interrupted"}`, `{"thought":true,"text":"interrupted"}`} {
		t.Run(part, func(t *testing.T) {
			var param any
			initial := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"fn","partialArgs":[{"jsonPath":"$.x","stringValue":"A","willContinue":true}],"willContinue":true}}]}}]}`)
			ConvertGeminiResponseToOpenAI(context.Background(), "gemini", nil, nil, initial, &param)
			chunk := []byte(`{"candidates":[{"content":{"parts":[` + part + `,{"functionCall":{"partialArgs":[{"jsonPath":"$.x","stringValue":"B"}],"willContinue":false}}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":1}}`)
			out := ConvertGeminiResponseToOpenAI(context.Background(), "gemini", nil, nil, chunk, &param)
			if param.(interface{ ToolInputError() error }).ToolInputError() == nil {
				t.Fatal("substantive content must immediately reject incomplete tool input")
			}
			out = append(out, ConvertGeminiResponseToOpenAI(context.Background(), "gemini", nil, nil, initial, &param)...)
			out = append(out, ConvertGeminiResponseToOpenAI(context.Background(), "gemini", nil, nil, []byte("[DONE]"), &param)...)
			for _, chunk := range out {
				if bytes.Equal(chunk, []byte("[DONE]")) {
					t.Fatal("failed tool input emitted DONE")
				}
				choice := gjson.GetBytes(chunk, "choices.0")
				if choice.Get("finish_reason").String() != "" || choice.Get("delta.content").String() != "" || choice.Get("delta.reasoning_content").String() != "" || len(choice.Get("delta.tool_calls").Array()) != 0 {
					t.Fatalf("failed tool input emitted further content or success: %s", chunk)
				}
			}
		})
	}
}

func TestConvertGeminiResponseToOpenAI_PartialArgsIDAliases(t *testing.T) {
	for _, identity := range []string{`"call_id":"a"`, `"callId":"a"`, `"id":"a","call_id":"ignored","callId":"ignored2"`} {
		t.Run(identity, func(t *testing.T) {
			for _, continuationID := range []string{"a", "b"} {
				var param any
				initial := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{` + identity + `,"name":"fn","partialArgs":[{"jsonPath":"$.x","stringValue":"A","willContinue":true}],"willContinue":true}}]}}]}`)
				out := ConvertGeminiResponseToOpenAI(context.Background(), "gemini", nil, nil, initial, &param)
				if len(out) != 1 || gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.id").String() != "a" {
					t.Fatalf("alias identity must be preserved: %q", out)
				}
				continuation := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"callId":"` + continuationID + `","name":"fn","partialArgs":[{"jsonPath":"$.x","stringValue":"B"}],"willContinue":false}}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":1}}`)
				out = append(out, ConvertGeminiResponseToOpenAI(context.Background(), "gemini", nil, nil, continuation, &param)...)
				param.(interface{ FinalizeToolInput() [][]byte }).FinalizeToolInput()
				err := param.(interface{ ToolInputError() error }).ToolInputError()
				if (err == nil) != (continuationID == "a") {
					t.Fatalf("continuation ID %s: error=%v", continuationID, err)
				}
				if continuationID == "a" {
					var arguments strings.Builder
					for _, chunk := range out {
						for _, call := range gjson.GetBytes(chunk, "choices.0.delta.tool_calls").Array() {
							if call.Get("index").Int() != 0 {
								t.Fatalf("continuation created a second call: %s", chunk)
							}
							arguments.WriteString(call.Get("function.arguments").String())
						}
					}
					if arguments.String() != `{"x":"AB"}` {
						t.Fatalf("continued arguments = %q", arguments.String())
					}
				}
			}
		})
	}
}

func TestConvertGeminiResponseToOpenAI_SkipsThoughtInlineDataImages(t *testing.T) {
	ctx := context.Background()
	const finalURL = "data:image/png;base64,RklOQUxfSU1BR0U="

	t.Run("non-stream", func(t *testing.T) {
		raw := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"thought":true,"text":"Drafting."},{"thought":true,"inlineData":{"mimeType":"image/png","data":"RFJBRlRfSU1BR0U="}},{"thought":true,"text":"Refining."},{"inlineData":{"mimeType":"image/png","data":"RklOQUxfSU1BR0U="},"thoughtSignature":"EjQKDg=="}]},"finishReason":"STOP"}]}`)
		out := ConvertGeminiResponseToOpenAINonStream(ctx, "gemini-3-pro-image", nil, nil, raw, nil)
		if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "Drafting.Refining." {
			t.Fatalf("reasoning_content = %q; payload=%s", got, out)
		}
		images := gjson.GetBytes(out, "choices.0.message.images").Array()
		if len(images) != 1 || images[0].Get("index").Int() != 0 || images[0].Get("image_url.url").String() != finalURL {
			t.Fatalf("message.images must contain only the final image at index 0: %s", out)
		}
	})

	t.Run("stream drops standalone thought image frame", func(t *testing.T) {
		var param any
		thought := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"thought":true,"inlineData":{"mimeType":"image/png","data":"RFJBRlRfSU1BR0U="}}]}}],"modelVersion":"gemini-3-pro-image","responseId":"resp_7566"}`)
		if out := ConvertGeminiResponseToOpenAI(ctx, "gemini-3-pro-image", nil, nil, thought, &param); len(out) != 0 {
			t.Fatalf("thought image chunk must not emit a delta, got %s", out[0])
		}
		final := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"thought":true,"inline_data":{"mime_type":"image/png","data":"RFJBRlRfMg=="}},{"inlineData":{"mimeType":"image/png","data":"RklOQUxfSU1BR0U="}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":1120,"totalTokenCount":1132},"modelVersion":"gemini-3-pro-image","responseId":"resp_7566"}`)
		out := ConvertGeminiResponseToOpenAI(ctx, "gemini-3-pro-image", nil, nil, final, &param)
		if len(out) != 2 {
			t.Fatalf("expected an image chunk followed by completion, got %q", out)
		}
		images := gjson.GetBytes(out[0], "choices.0.delta.images").Array()
		if len(images) != 1 || images[0].Get("index").Int() != 0 || images[0].Get("image_url.url").String() != finalURL {
			t.Fatalf("delta.images must contain only the final image at index 0: %s", out[0])
		}
	})
}
