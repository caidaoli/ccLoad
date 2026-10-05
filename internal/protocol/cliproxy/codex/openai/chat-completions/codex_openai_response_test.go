package chat_completions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func assertOpenAITerminalChunks(t *testing.T, chunks [][]byte) []byte {
	t.Helper()
	if len(chunks) != 2 {
		t.Fatalf("expected terminal payload and [DONE], got %d chunks", len(chunks))
	}
	if got := string(chunks[1]); got != "[DONE]" {
		t.Fatalf("terminal chunk = %q, want [DONE]", got)
	}
	return chunks[0]
}

func TestConvertCodexResponseToOpenAI_StreamIncludesCachedTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	created := []byte(`data: {"type":"response.created","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.2-codex"}}`)
	if out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.2-codex", nil, nil, created, &param); len(out) != 0 {
		t.Fatalf("response.created should not emit chunks, got %d", len(out))
	}

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.2-codex","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":64},"output_tokens_details":{"reasoning_tokens":7}}}}`)
	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.2-codex", nil, nil, completed, &param)
	chunk := gjson.ParseBytes(assertOpenAITerminalChunks(t, out))
	if got := chunk.Get("usage.prompt_tokens_details.cached_tokens").Int(); got != 64 {
		t.Fatalf("cached_tokens mismatch: got %d, want %d", got, 64)
	}
	if got := chunk.Get("usage.completion_tokens_details.reasoning_tokens").Int(); got != 7 {
		t.Fatalf("reasoning_tokens mismatch: got %d, want %d", got, 7)
	}
}

func TestConvertCodexResponseToOpenAINonStreamIncludesCachedTokens(t *testing.T) {
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_2","created_at":1700000001,"model":"gpt-5.2-codex","status":"completed","usage":{"input_tokens":88,"output_tokens":12,"total_tokens":100,"input_tokens_details":{"cached_tokens":33}},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)

	out := ConvertCodexResponseToOpenAINonStream(context.Background(), "gpt-5.2-codex", nil, nil, raw, nil)
	if len(out) == 0 {
		t.Fatalf("expected non-empty response")
	}

	resp := gjson.ParseBytes(out)
	if got := resp.Get("usage.prompt_tokens_details.cached_tokens").Int(); got != 33 {
		t.Fatalf("cached_tokens mismatch: got %d, want %d", got, 33)
	}
}

func TestConvertCodexResponseToOpenAI_IncompleteTerminal(t *testing.T) {
	ctx := context.Background()
	terminal := []byte(`{"type":"response.incomplete","response":{"id":"resp_1","model":"gpt-5.5","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)

	var param any
	streamOut := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, append([]byte("data: "), terminal...), &param)
	streamPayload := assertOpenAITerminalChunks(t, streamOut)
	if got := gjson.GetBytes(streamPayload, "choices.0.finish_reason").String(); got != "length" {
		t.Fatalf("stream finish_reason = %q, want length; payload=%s", got, streamPayload)
	}
	if got := gjson.GetBytes(streamPayload, "choices.0.native_finish_reason").String(); got != "max_output_tokens" {
		t.Fatalf("stream native_finish_reason = %q, want max_output_tokens; payload=%s", got, streamPayload)
	}

	var toolParam any
	_ = ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`), &toolParam)
	toolStreamOut := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, append([]byte("data: "), terminal...), &toolParam)
	toolStreamPayload := assertOpenAITerminalChunks(t, toolStreamOut)
	if got := gjson.GetBytes(toolStreamPayload, "choices.0.finish_reason").String(); got != "length" {
		t.Fatalf("tool stream finish_reason = %q, want length; payload=%s", got, toolStreamPayload)
	}

	nonStreamOut := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.5", nil, nil, terminal, nil)
	if got := gjson.GetBytes(nonStreamOut, "choices.0.finish_reason").String(); got != "length" {
		t.Fatalf("non-stream finish_reason = %q, want length; payload=%s", got, nonStreamOut)
	}
}

func TestConvertCodexResponseToOpenAI_StreamSetsModelFromResponseCreated(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.3-codex"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected no output for response.created, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_FirstChunkUsesRequestModelName(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_PreservesURLCitations(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		raw := []byte(`{"type":"response.completed","response":{"id":"resp_citation","model":"gpt-5.5","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"前🙂"},{"type":"output_text","text":"引用","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":2},{"type":"file_citation","file_id":"file_1","index":0}]}]}]}}`)
		out := ConvertCodexResponseToOpenAINonStream(t.Context(), "gpt-5.5", nil, nil, raw, nil)

		if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "前🙂引用" {
			t.Fatalf("content = %q, want %q; response=%s", got, "前🙂引用", out)
		}
		annotation := gjson.GetBytes(out, "choices.0.message.annotations.0")
		if !annotation.Exists() {
			t.Fatalf("expected message annotation, response=%s", out)
		}
		if got := gjson.GetBytes(out, "choices.0.message.annotations.#").Int(); got != 1 {
			t.Fatalf("annotation count = %d, want 1 after filtering unsupported types; response=%s", got, out)
		}
		if got := annotation.Get("type").String(); got != "url_citation" {
			t.Fatalf("annotation type = %q, want url_citation; response=%s", got, out)
		}
		if got := annotation.Get("url").String(); got != "https://example.com" {
			t.Fatalf("annotation url = %q, want https://example.com; response=%s", got, out)
		}
		if got := annotation.Get("title").String(); got != "Example" {
			t.Fatalf("annotation title = %q, want Example; response=%s", got, out)
		}
		if got := annotation.Get("start_index").Int(); got != 2 {
			t.Fatalf("annotation start_index = %d, want 2; response=%s", got, out)
		}
		if got := annotation.Get("end_index").Int(); got != 4 {
			t.Fatalf("annotation end_index = %d, want 4; response=%s", got, out)
		}
	})

	t.Run("stream", func(t *testing.T) {
		var param any
		if out := ConvertCodexResponseToOpenAI(t.Context(), "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"前🙂"}`), &param); len(out) != 1 {
			t.Fatalf("expected text delta chunk, got %d", len(out))
		}

		annotationEvent := []byte(`data: {"type":"response.output_text.annotation.added","annotation_index":0,"annotation":{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":1}}`)
		out := ConvertCodexResponseToOpenAI(t.Context(), "gpt-5.5", nil, nil, annotationEvent, &param)
		if len(out) != 1 {
			t.Fatalf("expected citation chunk, got %d", len(out))
		}
		annotation := gjson.GetBytes(out[0], "choices.0.delta.annotations.0")
		if !annotation.Exists() {
			t.Fatalf("expected delta annotation, chunk=%s", out[0])
		}
		if got := annotation.Get("type").String(); got != "url_citation" {
			t.Fatalf("annotation type = %q, want url_citation; chunk=%s", got, out[0])
		}
		if got := annotation.Get("start_index").Int(); got != 2 {
			t.Fatalf("annotation start_index = %d, want 2; chunk=%s", got, out[0])
		}
		if got := annotation.Get("end_index").Int(); got != 3 {
			t.Fatalf("annotation end_index = %d, want 3; chunk=%s", got, out[0])
		}

		if out = ConvertCodexResponseToOpenAI(t.Context(), "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"引用"}`), &param); len(out) != 1 {
			t.Fatalf("expected second text delta chunk, got %d", len(out))
		}
		updatedAnnotationEvent := []byte(`data: {"type":"response.output_text.annotation.added","annotation_index":0,"annotation":{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":2}}`)
		if out = ConvertCodexResponseToOpenAI(t.Context(), "gpt-5.5", nil, nil, updatedAnnotationEvent, &param); len(out) != 0 {
			t.Fatalf("expected duplicate citation to be suppressed after more text, got %d chunks", len(out))
		}
	})

	t.Run("stream completion annotation", func(t *testing.T) {
		var param any
		for _, delta := range []string{"前🙂", "引用"} {
			if out := ConvertCodexResponseToOpenAI(t.Context(), "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":`+string(mustJSONMarshal(t, delta))+`}`), &param); len(out) != 1 {
				t.Fatalf("expected text delta chunk, got %d", len(out))
			}
		}

		doneEvent := []byte(`data: {"type":"response.output_text.done","text":"前🙂引用","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":2}]}`)
		out := ConvertCodexResponseToOpenAI(t.Context(), "gpt-5.5", nil, nil, doneEvent, &param)
		if len(out) != 1 {
			t.Fatalf("expected citation completion chunk, got %d", len(out))
		}
		annotation := gjson.GetBytes(out[0], "choices.0.delta.annotations.0")
		if got := annotation.Get("start_index").Int(); got != 4 {
			t.Fatalf("annotation start_index = %d, want 4; chunk=%s", got, out[0])
		}
		if got := annotation.Get("end_index").Int(); got != 6 {
			t.Fatalf("annotation end_index = %d, want 6; chunk=%s", got, out[0])
		}

		contentPartDoneEvent := []byte(`data: {"type":"response.content_part.done","part":{"type":"output_text","text":"前🙂引用","annotations":[{"type":"url_citation","url":"https://other.example","title":"Other","start_index":0,"end_index":1}]}}`)
		out = ConvertCodexResponseToOpenAI(t.Context(), "gpt-5.5", nil, nil, contentPartDoneEvent, &param)
		if len(out) != 1 || gjson.GetBytes(out[0], "choices.0.delta.annotations.0.url").String() != "https://other.example" {
			t.Fatalf("expected content-part citation chunk, got %d: %s", len(out), out)
		}
		if got := gjson.GetBytes(out[0], "choices.0.delta.annotations.0.start_index").Int(); got != 4 {
			t.Fatalf("content-part annotation start_index = %d, want 4; chunk=%s", got, out[0])
		}

		itemDoneEvent := []byte(`data: {"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"前🙂引用","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":2}]}]}}`)
		if out = ConvertCodexResponseToOpenAI(t.Context(), "gpt-5.5", nil, nil, itemDoneEvent, &param); len(out) != 0 {
			t.Fatalf("expected duplicate completion citation to be suppressed, got %d chunks", len(out))
		}
	})
}

func TestConvertCodexResponseToOpenAI_ToolCallChunkOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls").Exists() {
		t.Fatalf("expected tool_calls to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallArgumentsDeltaOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected tool call announcement chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","delta":"{\"query\":\"OpenAI\"}"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").Exists() {
		t.Fatalf("expected tool call arguments delta to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_CustomToolCallStreamDeltas(t *testing.T) {
	ctx := context.Background()
	var param any
	send := func(event string) [][]byte {
		return ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte("data: "+event), &param)
	}

	out := send(`{"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"unexpected input"}}`)
	if len(out) != 1 {
		t.Fatalf("expected 1 announcement chunk, got %d", len(out))
	}
	toolCall := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0")
	if got := toolCall.Get("index").Int(); got != 0 {
		t.Fatalf("expected tool index 0, got %d; chunk=%s", got, out[0])
	}
	if got := toolCall.Get("id").String(); got != "call_apply" {
		t.Fatalf("expected call id call_apply, got %q; chunk=%s", got, out[0])
	}
	if got := toolCall.Get("function.name").String(); got != "ApplyPatch" {
		t.Fatalf("expected tool name ApplyPatch, got %q; chunk=%s", got, out[0])
	}
	if args := toolCall.Get("function.arguments"); !args.Exists() || args.String() != "" {
		t.Fatalf("expected empty announced arguments, got %s; chunk=%s", args.Raw, out[0])
	}

	for _, delta := range []string{"*** Begin Patch\n", "*** End Patch"} {
		out = send(`{"type":"response.custom_tool_call_input.delta","delta":` + string(mustJSONMarshal(t, delta)) + `}`)
		if len(out) != 1 {
			t.Fatalf("expected 1 arguments delta chunk, got %d", len(out))
		}
		if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != delta {
			t.Fatalf("expected arguments delta %q, got %q; chunk=%s", delta, got, out[0])
		}
	}

	fullInput := "*** Begin Patch\n*** End Patch"
	out = send(`{"type":"response.custom_tool_call_input.done","input":` + string(mustJSONMarshal(t, fullInput)) + `}`)
	if len(out) != 0 {
		t.Fatalf("expected custom input done to be suppressed after deltas, got %d chunks", len(out))
	}
	out = send(`{"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":` + string(mustJSONMarshal(t, fullInput)) + `}}`)
	if len(out) != 0 {
		t.Fatalf("expected output item done to be suppressed after deltas, got %d chunks", len(out))
	}

	out = send(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	completion := assertOpenAITerminalChunks(t, out)
	if got := gjson.GetBytes(completion, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("expected finish reason tool_calls, got %q; chunk=%s", got, completion)
	}
}

func TestConvertCodexResponseToOpenAI_EmptyCustomToolDeltaUsesDoneFallback(t *testing.T) {
	ctx := context.Background()
	var param any

	_ = ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":""}}`), &param)
	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","output_index":0,"delta":""}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected empty delta to be suppressed, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.custom_tool_call_input.done","item_id":"ctc_1","output_index":0,"input":"full patch"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 done fallback chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "full patch" {
		t.Fatalf("expected full patch arguments, got %q; chunk=%s", got, out[0])
	}
}

func TestConvertCodexResponseToOpenAI_InterleavedToolCallsKeepStateByItem(t *testing.T) {
	ctx := context.Background()
	var param any
	send := func(event string) [][]byte {
		return ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte("data: "+event), &param)
	}

	out := send(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":""}}`)
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("expected function call index 0, got %d; chunk=%s", got, out[0])
	}
	out = send(`{"type":"response.output_item.added","output_index":1,"item":{"id":"ctc_2","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":""}}`)
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 1 {
		t.Fatalf("expected custom call index 1, got %d; chunk=%s", got, out[0])
	}

	out = send(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"query\":"}`)
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("expected interleaved function delta index 0, got %d; chunk=%s", got, out[0])
	}
	out = send(`{"type":"response.custom_tool_call_input.delta","output_index":1,"delta":""}`)
	if len(out) != 0 {
		t.Fatalf("expected empty custom delta to be suppressed, got %d chunks", len(out))
	}
	out = send(`{"type":"response.custom_tool_call_input.done","output_index":1,"input":"patch"}`)
	if len(out) != 1 {
		t.Fatalf("expected custom done fallback, got %d chunks", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 1 {
		t.Fatalf("expected output-index-routed custom fallback index 1, got %d; chunk=%s", got, out[0])
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "patch" {
		t.Fatalf("expected custom fallback arguments patch, got %q; chunk=%s", got, out[0])
	}

	for _, event := range []string{
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"query\":\"test\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{\"query\":\"test\"}"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"ctc_2","type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"patch"}}`,
	} {
		if out = send(event); len(out) != 0 {
			t.Fatalf("expected terminal tool event to avoid duplicate output, got %d chunks for %s", len(out), event)
		}
	}
}

func TestConvertCodexResponseToOpenAI_CustomToolCallInputDoneFallback(t *testing.T) {
	ctx := context.Background()
	var param any

	_ = ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":""}}`), &param)
	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.custom_tool_call_input.done","input":"full patch"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 fallback arguments chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "full patch" {
		t.Fatalf("expected full patch arguments, got %q; chunk=%s", got, out[0])
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"full patch"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected output item done to be suppressed after input done fallback, got %d chunks", len(out))
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallOutputItemDoneFallbacks(t *testing.T) {
	t.Run("announced custom call emits arguments only", func(t *testing.T) {
		ctx := context.Background()
		var param any

		_ = ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_first","name":"ApplyPatch","input":""}}`), &param)
		out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call_first","name":"ApplyPatch","input":"first patch"}}`), &param)
		if len(out) != 1 {
			t.Fatalf("expected 1 fallback arguments chunk, got %d", len(out))
		}
		toolCall := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0")
		if got := toolCall.Get("index").Int(); got != 0 {
			t.Fatalf("expected tool index 0, got %d; chunk=%s", got, out[0])
		}
		if toolCall.Get("id").Exists() || toolCall.Get("function.name").Exists() {
			t.Fatalf("expected arguments-only fallback, got %s", toolCall.Raw)
		}
		if got := toolCall.Get("function.arguments").String(); got != "first patch" {
			t.Fatalf("expected first patch arguments, got %q; chunk=%s", got, out[0])
		}

		_ = ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_second","name":"ApplyPatch","input":""}}`), &param)
		out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call_second","name":"ApplyPatch","input":"second patch"}}`), &param)
		if len(out) != 1 {
			t.Fatalf("expected 1 second fallback arguments chunk, got %d", len(out))
		}
		if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 1 {
			t.Fatalf("expected second tool index 1, got %d; chunk=%s", got, out[0])
		}
	})

	t.Run("unannounced custom call emits complete call", func(t *testing.T) {
		ctx := context.Background()
		var param any
		out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"full patch"}}`), &param)
		if len(out) != 1 {
			t.Fatalf("expected 1 complete fallback chunk, got %d", len(out))
		}
		toolCall := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0")
		if got := toolCall.Get("id").String(); got != "call_apply" {
			t.Fatalf("expected call id call_apply, got %q; chunk=%s", got, out[0])
		}
		if got := toolCall.Get("function.name").String(); got != "ApplyPatch" {
			t.Fatalf("expected tool name ApplyPatch, got %q; chunk=%s", got, out[0])
		}
		if got := toolCall.Get("function.arguments").String(); got != "full patch" {
			t.Fatalf("expected full patch arguments, got %q; chunk=%s", got, out[0])
		}
	})

	t.Run("announced function call still falls back", func(t *testing.T) {
		ctx := context.Background()
		var param any

		_ = ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_lookup","name":"lookup","arguments":""}}`), &param)
		out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{\"query\":\"test\"}"}}`), &param)
		if len(out) != 1 {
			t.Fatalf("expected 1 function arguments fallback chunk, got %d", len(out))
		}
		if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != `{"query":"test"}` {
			t.Fatalf("expected function arguments fallback, got %q; chunk=%s", got, out[0])
		}
	})
}

func TestConvertCodexResponseToOpenAI_ToolCallStateFallsBackFromUnknownItemID(t *testing.T) {
	ctx := context.Background()
	var param any

	added := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.6-terra",
		nil,
		nil,
		[]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"TaskCreate","arguments":""}}`),
		&param,
	)
	if len(added) != 1 {
		t.Fatalf("added chunks = %d, want 1", len(added))
	}

	done := ConvertCodexResponseToOpenAI(
		ctx,
		"gpt-5.6-terra",
		nil,
		nil,
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"TaskCreate","arguments":"{\"subject\":\"test\"}"}}`),
		&param,
	)
	if len(done) != 1 {
		t.Fatalf("done chunks = %d, want 1", len(done))
	}

	addedName := gjson.GetBytes(added[0], "choices.0.delta.tool_calls.0.function.name").String()
	doneName := gjson.GetBytes(done[0], "choices.0.delta.tool_calls.0.function.name").String()
	if got := addedName + doneName; got != "TaskCreate" {
		t.Fatalf("assembled tool name = %q, want %q", got, "TaskCreate")
	}

	toolCall := gjson.GetBytes(done[0], "choices.0.delta.tool_calls.0")
	if toolCall.Get("id").Exists() || toolCall.Get("function.name").Exists() {
		t.Fatalf("done chunk repeated tool identity: %s", toolCall.Raw)
	}
	if got := toolCall.Get("index").Int(); got != 0 {
		t.Fatalf("done tool index = %d, want 0", got)
	}
	if got := toolCall.Get("function.arguments").String(); got != `{"subject":"test"}` {
		t.Fatalf("done arguments = %q", got)
	}
}

func TestConvertCodexResponseToOpenAINonStream_CustomToolCall(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.5","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"type":"custom_tool_call","call_id":"call_apply","name":"ApplyPatch","input":"full patch"}]}}`)

	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.5", nil, nil, raw, nil)
	toolCall := gjson.GetBytes(out, "choices.0.message.tool_calls.0")
	if got := toolCall.Get("id").String(); got != "call_apply" {
		t.Fatalf("expected call id call_apply, got %q; response=%s", got, out)
	}
	if got := toolCall.Get("function.name").String(); got != "ApplyPatch" {
		t.Fatalf("expected tool name ApplyPatch, got %q; response=%s", got, out)
	}
	if got := toolCall.Get("function.arguments").String(); got != "full patch" {
		t.Fatalf("expected full patch arguments, got %q; response=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("expected finish reason tool_calls, got %q; response=%s", got, out)
	}
}

func TestConvertCodexResponseToOpenAI_StreamPartialImageEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	chunk := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out[0]))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 0 {
		t.Fatalf("expected duplicate image chunk to be suppressed, got %d", len(out))
	}
}

func TestConvertCodexResponseToOpenAI_StreamImageGenerationCallDoneEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"png","result":"aGVsbG8="}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected output_item.done to be suppressed when identical to last partial image, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"jpeg","result":"Ymll"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/jpeg;base64,Ymll" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/jpeg;base64,Ymll", gotURL, string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamImageGenerationCallAddsMessageImages(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"image_generation_call","output_format":"png","result":"aGVsbG8="}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	gotURL := gjson.GetBytes(out, "choices.0.message.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out))
	}
}

func TestConvertCodexResponseToOpenAI_StreamForwardsCacheWriteTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	// Seed response.created so response.completed can reuse response metadata.
	_ = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4"}}`), &param)

	chunk := []byte(`data: {"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":40},"output_tokens_details":{"reasoning_tokens":5}}}}`)
	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	assertUsageMapping(t, assertOpenAITerminalChunks(t, out), 40, true)
}

func TestConvertCodexResponseToOpenAI_StreamOmitsMissingCacheWriteTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	_ = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4"}}`), &param)

	chunk := []byte(`data: {"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":5}}}}`)
	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	assertUsageMapping(t, assertOpenAITerminalChunks(t, out), 0, false)
}

func TestConvertCodexResponseToOpenAI_StreamPreservesExplicitZeroCacheWriteTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	_ = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4"}}`), &param)

	chunk := []byte(`data: {"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":5}}}}`)
	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	assertUsageMapping(t, assertOpenAITerminalChunks(t, out), 0, true)
}

func TestConvertCodexResponseToOpenAI_NonStreamForwardsCacheWriteTokens(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":40},"output_tokens_details":{"reasoning_tokens":5}},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)
	assertUsageMapping(t, out, 40, true)
}

func TestConvertCodexResponseToOpenAI_NonStreamOmitsMissingCacheWriteTokens(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":5}},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)
	assertUsageMapping(t, out, 0, false)
}

func TestConvertCodexResponseToOpenAI_NonStreamPreservesExplicitZeroCacheWriteTokens(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":5}},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)
	assertUsageMapping(t, out, 0, true)
}

func mustJSONMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("failed to marshal test JSON: %v", errMarshal)
	}
	return data
}

func assertUsageMapping(t *testing.T, payload []byte, wantCachedCreation int64, expectCachedCreation bool) {
	t.Helper()

	if got := gjson.GetBytes(payload, "usage.prompt_tokens").Int(); got != 100 {
		t.Fatalf("expected prompt_tokens=100, got %d; payload=%s", got, string(payload))
	}
	if got := gjson.GetBytes(payload, "usage.completion_tokens").Int(); got != 20 {
		t.Fatalf("expected completion_tokens=20, got %d; payload=%s", got, string(payload))
	}
	if got := gjson.GetBytes(payload, "usage.total_tokens").Int(); got != 120 {
		t.Fatalf("expected total_tokens=120, got %d; payload=%s", got, string(payload))
	}
	if got := gjson.GetBytes(payload, "usage.prompt_tokens_details.cached_tokens").Int(); got != 30 {
		t.Fatalf("expected cached_tokens=30, got %d; payload=%s", got, string(payload))
	}
	if got := gjson.GetBytes(payload, "usage.completion_tokens_details.reasoning_tokens").Int(); got != 5 {
		t.Fatalf("expected reasoning_tokens=5, got %d; payload=%s", got, string(payload))
	}

	gotCachedCreation := gjson.GetBytes(payload, "usage.prompt_tokens_details.cached_creation_tokens")
	gotCacheWrite := gjson.GetBytes(payload, "usage.prompt_tokens_details.cache_write_tokens")
	if expectCachedCreation {
		if !gotCachedCreation.Exists() {
			t.Fatalf("expected cached_creation_tokens to exist, payload=%s", string(payload))
		}
		if gotCachedCreation.Int() != wantCachedCreation {
			t.Fatalf("expected cached_creation_tokens=%d, got %d; payload=%s", wantCachedCreation, gotCachedCreation.Int(), string(payload))
		}
		if !gotCacheWrite.Exists() {
			t.Fatalf("expected cache_write_tokens to exist, payload=%s", string(payload))
		}
		if gotCacheWrite.Int() != wantCachedCreation {
			t.Fatalf("expected cache_write_tokens=%d, got %d; payload=%s", wantCachedCreation, gotCacheWrite.Int(), string(payload))
		}
	} else if gotCachedCreation.Exists() || gotCacheWrite.Exists() {
		t.Fatalf("expected cache creation/write tokens to be omitted, payload=%s", string(payload))
	}
	if legacy := gjson.GetBytes(payload, "usage.cache_creation_input_tokens"); legacy.Exists() {
		t.Fatalf("expected legacy cache_creation_input_tokens to be omitted, payload=%s", string(payload))
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamMultiMessageEmptyTrailingKeepsContent(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.5","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"output":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},` +
		`{"type":"message","content":[{"type":"output_text","text":"the real answer"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking again"}]},` +
		`{"type":"message","content":[{"type":"output_text","text":""}]}` +
		`]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.5", nil, nil, raw, nil)

	got := gjson.GetBytes(out, "choices.0.message.content")
	if !got.Exists() || got.Type == gjson.Null {
		t.Fatalf("content was dropped to null by trailing empty message; resp=%s", string(out))
	}
	if got.String() != "the real answer" {
		t.Fatalf("expected content %q, got %q; resp=%s", "the real answer", got.String(), string(out))
	}
}

func TestConvertCodexResponseToOpenAI_StreamReasoningTextDeltaAndDone(t *testing.T) {
	ctx := context.Background()
	var param any

	deltaRaw := []byte(`data: {"type":"response.reasoning_text.delta","delta":"Thinking step 1"}`)
	streamOut := ConvertCodexResponseToOpenAI(ctx, "MiniMax-M3", nil, nil, deltaRaw, &param)
	if len(streamOut) != 1 {
		t.Fatalf("expected 1 streaming chunk for reasoning_text.delta, got %d", len(streamOut))
	}
	if got := gjson.GetBytes(streamOut[0], "choices.0.delta.reasoning_content").String(); got != "Thinking step 1" {
		t.Fatalf("expected reasoning_content %q, got %q; payload=%s", "Thinking step 1", got, streamOut[0])
	}
	if got := gjson.GetBytes(streamOut[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("expected role assistant, got %q; payload=%s", got, streamOut[0])
	}

	doneRaw := []byte(`data: {"type":"response.reasoning_text.done","text":"Thinking step 1"}`)
	doneOut := ConvertCodexResponseToOpenAI(ctx, "MiniMax-M3", nil, nil, doneRaw, &param)
	if len(doneOut) != 1 {
		t.Fatalf("expected 1 streaming chunk for reasoning_text.done, got %d", len(doneOut))
	}
	if got := gjson.GetBytes(doneOut[0], "choices.0.delta.reasoning_content").String(); got != "\n\n" {
		t.Fatalf("expected reasoning_content %q, got %q; payload=%s", "\n\n", got, doneOut[0])
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamReasoningTextContent(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"MiniMax-M3","status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"output_tokens_details":{"reasoning_tokens":15}},"output":[` +
		`{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"Full reasoning from MiniMax"}]},` +
		`{"type":"message","content":[{"type":"output_text","text":"Answer"}]}` +
		`]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "MiniMax-M3", nil, nil, raw, nil)

	got := gjson.GetBytes(out, "choices.0.message.reasoning_content")
	if !got.Exists() || got.Type == gjson.Null {
		t.Fatalf("expected reasoning_content to exist, got null/missing; payload=%s", string(out))
	}
	if got.String() != "Full reasoning from MiniMax" {
		t.Fatalf("expected reasoning_content %q, got %q; payload=%s", "Full reasoning from MiniMax", got.String(), string(out))
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamReasoningSummaryAndContent(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"MiniMax-M3","status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"output_tokens_details":{"reasoning_tokens":15}},"output":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"Summary part"}],"content":[{"type":"reasoning_text","text":" and Content part"}]},` +
		`{"type":"message","content":[{"type":"output_text","text":"Answer"}]}` +
		`]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "MiniMax-M3", nil, nil, raw, nil)

	got := gjson.GetBytes(out, "choices.0.message.reasoning_content")
	if !got.Exists() || got.Type == gjson.Null {
		t.Fatalf("expected reasoning_content to exist, got null/missing; payload=%s", string(out))
	}
	// ccLoad uses full reasoning when supplied and falls back to summaries otherwise.
	if got.String() != " and Content part" {
		t.Fatalf("expected reasoning_content %q, got %q; payload=%s", " and Content part", got.String(), string(out))
	}
}

func TestConvertCodexResponseToOpenAI_Issue5543_CacheWriteTokensAndServiceTier(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_example","model":"example-model","service_tier":"default","output":[],"usage":{"input_tokens":7378,"output_tokens":6,"total_tokens":7384,"input_tokens_details":{"cached_tokens":7168,"cache_write_tokens":128}}}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "example-model", nil, nil, raw, nil)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "default" {
		t.Fatalf("service_tier=%q, want default; payload=%s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.prompt_tokens_details.cache_write_tokens").Int(); got != 128 {
		t.Fatalf("cache_write_tokens=%d, want 128; payload=%s", got, out)
	}
	var state any
	created := ConvertCodexResponseToOpenAI(ctx, "example-model", nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_stream","model":"example-model","service_tier":"priority"}}`), &state)
	if len(created) != 0 {
		t.Fatalf("response.created emitted %d chunks", len(created))
	}
	delta := ConvertCodexResponseToOpenAI(ctx, "example-model", nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &state)
	if len(delta) != 1 || gjson.GetBytes(delta[0], "service_tier").String() != "priority" {
		t.Fatalf("service tier was not carried to delta: %s", delta)
	}
}

func TestConvertCodexResponseToOpenAI_RestoresNormalizedToolNames(t *testing.T) {
	ctx := context.Background()
	originalName := "mcp.server:search tool"
	normalizedName := "mcp_server_search_tool"
	originalRequest := []byte(`{
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "` + originalName + `"
				}
			}
		]
	}`)

	// Test non-stream response
	rawNonStream := []byte(`{"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"type":"function_call","call_id":"call_1","name":"` + normalizedName + `","arguments":"{}"}]}}`)
	outNonStream := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.6-sol", originalRequest, nil, rawNonStream, nil)
	gotNameNonStream := gjson.GetBytes(outNonStream, "choices.0.message.tool_calls.0.function.name").String()
	if gotNameNonStream != originalName {
		t.Fatalf("non-stream expected restored name %q, got %q", originalName, gotNameNonStream)
	}

	// Test stream response
	var param any
	rawStreamAdded := []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"` + normalizedName + `"}}`)
	streamChunks := ConvertCodexResponseToOpenAI(ctx, "gpt-5.6-sol", originalRequest, nil, rawStreamAdded, &param)
	if len(streamChunks) != 1 {
		t.Fatalf("expected 1 stream chunk, got %d", len(streamChunks))
	}
	gotNameStream := gjson.GetBytes(streamChunks[0], "choices.0.delta.tool_calls.0.function.name").String()
	if gotNameStream != originalName {
		t.Fatalf("stream expected restored name %q, got %q", originalName, gotNameStream)
	}
}

// Only the original winning custom patch declaration allows the JSON wrapper.
func TestApplyPatchCustomChatCompletionsWrapper(t *testing.T) {
	for _, toolType := range []string{"custom", "function"} {
		t.Run(toolType, func(t *testing.T) {
			request := []byte(`{"tools":[{"type":"` + toolType + `","name":"apply_patch"}]}`)
			var param any
			var arguments strings.Builder
			send := func(event string) {
				for _, out := range ConvertCodexResponseToOpenAI(t.Context(), "model", request, request, []byte("data: "+event), &param) {
					arguments.WriteString(gjson.GetBytes(out, "choices.0.delta.tool_calls.0.function.arguments").String())
				}
			}
			send(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"a","call_id":"c","name":"apply_patch","input":""}}`)
			for _, delta := range []string{"*** Begin Patch\n", "+中文😀 \"\n", "*** End Patch\n"} {
				send(`{"type":"response.custom_tool_call_input.delta","item_id":"a","output_index":0,"delta":` + string(mustJSONMarshal(t, delta)) + `}`)
			}
			patch := "*** Begin Patch\n+中文😀 \"\n*** End Patch\n"
			send(`{"type":"response.custom_tool_call_input.done","item_id":"a","output_index":0,"input":` + string(mustJSONMarshal(t, patch)) + `}`)
			send(`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"a","call_id":"c","name":"apply_patch","input":` + string(mustJSONMarshal(t, patch)) + `}}`)
			want := patch
			if toolType == "custom" {
				want = `{"input":` + string(mustJSONMarshal(t, patch)) + `}`
			}
			if arguments.String() != want {
				t.Fatalf("arguments=%q want=%q", arguments.String(), want)
			}
			var nonStream any
			out := ConvertCodexResponseToOpenAINonStream(t.Context(), "model", request, request, []byte(`{"type":"response.completed","response":{"output":[{"type":"custom_tool_call","name":"apply_patch","call_id":"c","input":`+string(mustJSONMarshal(t, patch))+`}]}}`), &nonStream)
			if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(); got != want {
				t.Fatalf("nonstream arguments=%q want=%q", got, want)
			}
		})
	}
}

func TestApplyPatchCustomChatCompletionsDoneFallback(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)
	for _, added := range []bool{false, true} {
		var param any
		if added {
			_ = ConvertCodexResponseToOpenAI(t.Context(), "m", request, request, []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"a","call_id":"c","name":"apply_patch","input":""}}`), &param)
		}
		out := ConvertCodexResponseToOpenAI(t.Context(), "m", request, request, []byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"a","call_id":"c","name":"apply_patch","input":"p"}}`), &param)
		if len(out) != 1 || gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").String() != `{"input":"p"}` {
			t.Fatalf("fallback: %s", out)
		}
	}
}

// This round trip requires the request converter to unwrap only a winning custom
// patch's normalized function envelope, while explicit custom inputs remain raw.
func TestApplyPatchChatCompletionNativeHistoryRoundTrip(t *testing.T) {
	original := []byte(`{"messages":[{"role":"user","content":"patch"}],"tools":[{"type":"custom","name":"apply_patch"}]}`)
	response := []byte(`{"type":"response.completed","response":{"output":[{"type":"custom_tool_call","call_id":"c","name":"apply_patch","input":"p"}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(t.Context(), "m", original, original, response, nil)
	message := gjson.GetBytes(out, "choices.0.message")
	followup := []byte(`{"messages":[` + message.Raw + `,{"role":"tool","tool_call_id":"c","content":"ok"}],"tools":[{"type":"custom","name":"apply_patch"}]}`)
	request := ConvertOpenAIRequestToCodex("m", followup, true)
	if got := gjson.GetBytes(request, "input.0.input").String(); got != "p" {
		t.Fatalf("normalized function history must restore raw patch before native Codex: got %q, request=%s", got, request)
	}
}

func TestApplyPatchChatResponseOrdinaryFunctionPreference(t *testing.T) {
	original := []byte(`{"tools":[{"type":"custom","name":"apply_patch"},{"type":"function","function":{"name":"apply_patch","parameters":{}}}]}`)
	out := ConvertCodexResponseToOpenAINonStream(t.Context(), "m", original, nil, []byte(`{"type":"response.completed","response":{"output":[{"type":"custom_tool_call","call_id":"c","name":"apply_patch","input":"raw"}]}}`), nil)
	if gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String() != "raw" {
		t.Fatalf("ordinary preference stolen: %s", out)
	}
}

func TestApplyPatchCustomChatSnapshotsCompleteStreamedPrefix(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)
	prefix, input := "prefix\n\"", "prefix\n\"suffix中文😀"
	for _, terminal := range []string{"input.done", "item.done", "response.completed"} {
		t.Run(terminal, func(t *testing.T) {
			var state any
			var arguments strings.Builder
			completions := 0
			send := func(raw string) {
				for _, chunk := range ConvertCodexResponseToOpenAI(t.Context(), "model", request, request, []byte("data: "+raw), &state) {
					if string(chunk) == "[DONE]" {
						continue
					}
					if !gjson.ValidBytes(chunk) {
						t.Fatalf("invalid Chat chunk: %s", chunk)
					}
					arguments.WriteString(gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments").String())
					if gjson.GetBytes(chunk, "choices.0.finish_reason").String() == "tool_calls" {
						completions++
					}
				}
			}
			send(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"a","call_id":"c","name":"apply_patch","input":""}}`)
			send(`{"type":"response.custom_tool_call_input.delta","item_id":"a","output_index":0,"delta":` + string(mustJSONMarshal(t, prefix)) + `}`)
			item := `{"type":"custom_tool_call","id":"a","call_id":"c","name":"apply_patch","input":` + string(mustJSONMarshal(t, input)) + `}`
			if terminal == "input.done" {
				send(`{"type":"response.custom_tool_call_input.done","item_id":"a","output_index":0,"input":` + string(mustJSONMarshal(t, input)) + `}`)
			}
			if terminal != "response.completed" {
				send(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`)
			}
			send(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[` + item + `]}}`)
			if !gjson.Valid(arguments.String()) || gjson.Get(arguments.String(), "input").String() != input || completions != 1 {
				t.Fatalf("snapshot lost input suffix or completion: arguments=%q completed=%d", arguments.String(), completions)
			}
			if errInput := state.(interface{ ToolInputError() error }).ToolInputError(); errInput != nil {
				t.Fatal(errInput)
			}
			if late := ConvertCodexResponseToOpenAI(t.Context(), "model", request, nil, []byte(`data: {"type":"response.custom_tool_call_input.done","item_id":"a","input":"different"}`), &state); len(late) != 0 {
				t.Fatalf("sealed stream emitted late data: %q", late)
			}
		})
	}
}

func TestApplyPatchCustomChatRejectsConflictingSnapshotsAndEOF(t *testing.T) {
	request := []byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)
	for _, conflict := range []string{"input.done", "item.done", "late input.done", "late item.done", "response.completed", "EOF"} {
		t.Run(conflict, func(t *testing.T) {
			var state any
			send := func(raw string) [][]byte {
				return ConvertCodexResponseToOpenAI(t.Context(), "model", request, request, []byte("data: "+raw), &state)
			}
			send(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"a","call_id":"c","name":"apply_patch","input":""}}`)
			send(`{"type":"response.custom_tool_call_input.delta","item_id":"a","output_index":0,"delta":"prefix"}`)
			if strings.HasPrefix(conflict, "late ") || conflict == "response.completed" || conflict == "EOF" {
				send(`{"type":"response.custom_tool_call_input.done","item_id":"a","output_index":0,"input":"prefix suffix"}`)
			}
			var rejected [][]byte
			switch conflict {
			case "input.done", "late input.done":
				rejected = send(`{"type":"response.custom_tool_call_input.done","item_id":"a","output_index":0,"input":"different"}`)
			case "item.done", "late item.done":
				rejected = send(`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"a","call_id":"c","name":"apply_patch","input":"different"}}`)
			case "response.completed":
				rejected = send(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"custom_tool_call","id":"a","call_id":"c","name":"apply_patch","input":"different"}]}}`)
			case "EOF":
				rejected = state.(interface{ FinalizeToolInput() [][]byte }).FinalizeToolInput()
			}
			if errInput := state.(interface{ ToolInputError() error }).ToolInputError(); errInput == nil || len(rejected) != 0 {
				t.Fatalf("conflicting or truncated input succeeded: error=%v chunks=%q", errInput, rejected)
			}
			if late := send(`{"type":"response.completed","response":{"status":"completed"}}`); len(late) != 0 {
				t.Fatalf("failed patch emitted successful terminal: %q", late)
			}
		})
	}
}

func TestOpenAIChatNestedCustomToolResponse(t *testing.T) {
	ctx := context.Background()
	original := []byte(`{"tools":[{"type":"custom","custom":{"name":"apply_patch","format":{"type":"text"}}}]}`)
	patch := "*** Begin Patch\n*** End Patch"

	t.Run("stream", func(t *testing.T) {
		var param any
		send := func(event string) [][]byte {
			return ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", original, nil, []byte("data: "+event), &param)
		}
		out := send(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":""}}`)
		call := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0")
		if call.Get("type").String() != "custom" || call.Get("id").String() != "call_patch" || call.Get("custom.name").String() != "apply_patch" || call.Get("function").Exists() {
			t.Fatalf("expected native custom tool call announcement, got %s", call.Raw)
		}
		var input strings.Builder
		for _, delta := range []string{"*** Begin Patch\n", "*** End Patch"} {
			out = send(`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","output_index":0,"delta":` + string(mustJSONMarshal(t, delta)) + `}`)
			input.WriteString(gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.custom.input").String())
		}
		if input.String() != patch {
			t.Fatalf("expected raw custom input %q, got %q", patch, input.String())
		}
		send(`{"type":"response.custom_tool_call_input.done","item_id":"ctc_1","output_index":0,"input":` + string(mustJSONMarshal(t, patch)) + `}`)
		send(`{"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":` + string(mustJSONMarshal(t, patch)) + `}}`)
		out = send(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
		if got := gjson.GetBytes(assertOpenAITerminalChunks(t, out), "choices.0.finish_reason").String(); got != "tool_calls" {
			t.Fatalf("expected finish reason tool_calls, got %q", got)
		}
	})

	t.Run("stream without added", func(t *testing.T) {
		var param any
		out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.5", original, nil, []byte(`data: {"type":"response.output_item.done","item":{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":`+string(mustJSONMarshal(t, patch))+`}}`), &param)
		call := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0")
		if call.Get("type").String() != "custom" || call.Get("custom.input").String() != patch {
			t.Fatalf("expected complete native custom tool call, got %s", call.Raw)
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		raw := []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":` + string(mustJSONMarshal(t, patch)) + `}]}}`)
		out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.5", original, nil, raw, nil)
		call := gjson.GetBytes(out, "choices.0.message.tool_calls.0")
		if call.Get("type").String() != "custom" || call.Get("id").String() != "call_patch" || call.Get("custom.name").String() != "apply_patch" || call.Get("custom.input").String() != patch || call.Get("index").Exists() {
			t.Fatalf("expected native custom tool call, got %s", call.Raw)
		}
	})
}
