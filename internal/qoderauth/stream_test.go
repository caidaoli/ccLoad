package qoderauth

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

/**
 * @brief readToolStream decodes the public normalized SSE boundary with declared tools.
 * @param t Test context.
 * @param input Native upstream SSE.
 * @return Decoded output chunks and whether DONE was emitted.
 */
func readToolStream(t *testing.T, input string) ([]map[string]any, bool) {
	t.Helper()
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"properties": map[string]any{"text": map[string]any{"type": "string"}, "count": map[string]any{"type": "integer"}, "items": map[string]any{"type": "array"}}}}}}
	stream := NormalizeStream(io.NopCloser(strings.NewReader(input)), tools)
	defer func() {
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	body, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []map[string]any
	done := false
	for _, event := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		data := strings.TrimPrefix(event, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("invalid normalized SSE: %q: %v", event, err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks, done
}

/**
 * @brief toolStreamChoice collects observable text, reasoning, and tool identities.
 */
type toolStreamChoice struct {
	content, reasoning, finish string
	calls                      []map[string]any
}

/**
 * @brief collectToolStream reconstructs client-visible tool calls and rejects unnamed emissions.
 * @param t Test context.
 * @param chunks Normalized Chat SSE chunks.
 * @return Client-visible state by choice index.
 */
func collectToolStream(t *testing.T, chunks []map[string]any) map[int]*toolStreamChoice {
	t.Helper()
	output := map[int]*toolStreamChoice{}
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			choice := raw.(map[string]any)
			index := int(choice["index"].(float64))
			state := output[index]
			if state == nil {
				state = &toolStreamChoice{}
				output[index] = state
			}
			delta := choice["delta"].(map[string]any)
			if value, _ := delta["content"].(string); value != "" {
				state.content += value
			}
			if value, _ := delta["reasoning_content"].(string); value != "" {
				state.reasoning += value
			}
			if value, _ := choice["finish_reason"].(string); value != "" {
				state.finish = value
			}
			calls, _ := delta["tool_calls"].([]any)
			for _, rawCall := range calls {
				call := rawCall.(map[string]any)
				callIndex := int(call["index"].(float64))
				function := call["function"].(map[string]any)
				if callIndex == len(state.calls) {
					if function["name"] == nil || function["name"] == "" || call["id"] == nil || call["id"] == "" {
						t.Fatalf("emitted unnamed or unidentified call: %v", call)
					}
					state.calls = append(state.calls, map[string]any{"id": call["id"], "name": function["name"], "arguments": ""})
				}
				if callIndex >= len(state.calls) {
					t.Fatalf("non-contiguous tool index: %v", call)
				}
				arguments, _ := function["arguments"].(string)
				state.calls[callIndex]["arguments"] = state.calls[callIndex]["arguments"].(string) + arguments
			}
		}
	}
	return output
}

/**
 * @brief nativeToolsEvent creates one upstream choice containing native tool fragments.
 * @param calls Native tool fragments.
 * @param finish Upstream finish reason.
 * @return Encoded native SSE event.
 */
func nativeToolsEvent(calls []any, finish any) string {
	return nativeEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": calls}, "finish_reason": finish}}})
}

/**
 * @brief TestNormalizeStreamNativeToolIdentity covers delayed names and reused native indices.
 * @param t Test context.
 */
func TestNormalizeStreamNativeToolIdentity(t *testing.T) {
	input := nativeToolsEvent([]any{map[string]any{"index": 0, "id": "first", "function": map[string]any{"arguments": `{"count":`}}}, nil)
	input += nativeToolsEvent([]any{map[string]any{"index": 0, "function": map[string]any{"name": " lookup ", "arguments": "1}"}}}, nil)
	input += nativeToolsEvent([]any{map[string]any{"index": 0, "id": "second", "function": map[string]any{"arguments": `{"count":`}}}, nil)
	input += nativeToolsEvent([]any{map[string]any{"index": 0, "function": map[string]any{"name": "lookup", "arguments": "2}"}}}, "tool_calls")
	chunks, done := readToolStream(t, input)
	state := collectToolStream(t, chunks)[0]
	if !done || len(state.calls) != 2 || state.calls[0]["id"] != "first" || state.calls[1]["id"] != "second" || state.calls[0]["arguments"] != `{"count":1}` || state.calls[1]["arguments"] != `{"count":2}` {
		t.Fatalf("lost native identity: %v DONE=%v", state.calls, done)
	}
}

/**
 * @brief TestNormalizeStreamNamelessNativeTools checks empty-answer failure and partial-answer preservation.
 * @param t Test context.
 */
func TestNormalizeStreamNamelessNativeTools(t *testing.T) {
	for _, prefix := range []string{"", nativeDelta("answer", nil), nativeDelta("<think>reason</think>", nil)} {
		t.Run(prefix, func(t *testing.T) {
			input := prefix + nativeToolsEvent([]any{map[string]any{"index": 0, "id": "unnamed", "function": map[string]any{"arguments": `{"count":1}`}}}, "tool_calls")
			chunks, done := readToolStream(t, input)
			last := chunks[len(chunks)-1]
			if prefix == "" {
				if done || len(chunks) != 1 || last["status"] != float64(502) || last["error"] == nil {
					t.Fatalf("empty nameless answer succeeded: %v DONE=%v", chunks, done)
				}
			} else {
				state := collectToolStream(t, chunks)[0]
				if !done || last["error"] != nil || state.finish != "stop" || len(state.calls) != 0 {
					t.Fatalf("valid answer was discarded: %v DONE=%v", chunks, done)
				}
			}
		})
	}
}

/**
 * @brief TestNormalizeStreamTextTools covers declared JSON/XML calls split across every character.
 * @param t Test context.
 */
func TestNormalizeStreamTextTools(t *testing.T) {
	cases := []struct {
		name, text, content, arguments string
	}{
		{"JSON", `before<tool_call>{"name":"lookup","arguments":{"count":2}}</tool_call> after`, "before after", `{"count":2}`},
		{"complete JSON without close", `<tool_call>{"name":"lookup","arguments":{"count":2}}`, "", `{"count":2}`},
		{"Qwen", `<tool_call><function=lookup><parameter=text>[1, 2]</parameter><parameter=count>2</parameter><parameter=items>[1,2]</parameter></function></tool_call>`, "", `{"count":2,"items":[1,2],"text":"[1, 2]"}`},
		{"bare Qwen", `<function=lookup><parameter=count>2</parameter></function>`, "", `{"count":2}`},
		{"Anthropic", `<antml:function_calls><antml:invoke name="lookup"><antml:parameter name="count">2</antml:parameter></antml:invoke></antml:function_calls>`, "", `{"count":2}`},
		{"bare invoke", `<invoke name="lookup"><parameter name="text">hello</parameter></invoke>`, "", `{"text":"hello"}`},
		{"no arguments", `<function=lookup></function>`, "", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var input strings.Builder
			for _, char := range tc.text {
				input.WriteString(nativeDelta(string(char), nil))
			}
			input.WriteString(nativeDelta("", "stop"))
			chunks, done := readToolStream(t, input.String())
			state := collectToolStream(t, chunks)[0]
			if !done || len(state.calls) != 1 || state.calls[0]["name"] != "lookup" || state.calls[0]["arguments"] != tc.arguments || state.content != tc.content || state.finish != "tool_calls" {
				t.Fatalf("bad text tool result: %+v calls=%v DONE=%v chunks=%v", state, state.calls, done, chunks)
			}
		})
	}
}

/**
 * @brief TestNormalizeStreamToolExamplesStayText prevents converting unknown tools and quoted documentation.
 * @param t Test context.
 */
func TestNormalizeStreamToolExamplesStayText(t *testing.T) {
	for _, text := range []string{
		`<tool_call>{"name":"undeclared","arguments":{"count":2}}</tool_call>`,
		`<function=undeclared><parameter=count>2</parameter></function>`,
		`<tool_call><function=undeclared><parameter=count>2</parameter></function></tool_call>`,
		"```json\n<tool_call>{\"name\":\"lookup\",\"arguments\":{}}</tool_call>\n```",
		"~~~xml\n<function=lookup></function>\n~~~",
		"Use `<tool_call>{\"name\":\"lookup\",\"arguments\":{}}</tool_call>` as an example.",
		"The <tool_call> tag and <function=lookup> tag are documented here.",
	} {
		t.Run(text, func(t *testing.T) {
			var input strings.Builder
			for _, char := range text {
				input.WriteString(nativeDelta(string(char), nil))
			}
			input.WriteString(nativeDelta("", "stop"))
			chunks, done := readToolStream(t, input.String())
			state := collectToolStream(t, chunks)[0]
			if !done || state.content != text || len(state.calls) != 0 {
				t.Fatalf("prose was interpreted as a call: %+v calls=%v DONE=%v", state, state.calls, done)
			}
		})
	}
}

/**
 * @brief TestNormalizeStreamIncompleteTextTools rejects tool syntax cut short by a real terminal marker.
 * @param t Test context.
 */
func TestNormalizeStreamIncompleteTextTools(t *testing.T) {
	for _, text := range []string{
		`<tool_call>{"name":"lookup","arguments":{"count":`,
		`<tool_call>{"name":"lookup","arguments":[]}</tool_call>`,
		`<function=lookup><parameter=count>2`,
		`<invoke name="lookup"><parameter name="count">2</invoke>`,
	} {
		t.Run(text, func(t *testing.T) {
			chunks, done := readToolStream(t, nativeDelta(text, nil)+"data: [DONE]\n\n")
			last := chunks[len(chunks)-1]
			if done || last["status"] != float64(502) || last["error"] == nil {
				t.Fatalf("incomplete tool succeeded: %v DONE=%v", chunks, done)
			}
		})
	}
}

/**
 * @brief TestNormalizeStreamToolsAcrossChoices preserves independent tool indices and reasoning.
 * @param t Test context.
 */
func TestNormalizeStreamToolsAcrossChoices(t *testing.T) {
	input := nativeEvent(map[string]any{"choices": []any{
		map[string]any{"index": 0, "delta": map[string]any{"content": `<think>reason</think><tool_call>{"name":"lookup","arguments":{"count":1}}</tool_call>`}, "finish_reason": "stop"},
		map[string]any{"index": 2, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "native", "function": map[string]any{"name": "lookup", "arguments": `{"count":2}`}}}}, "finish_reason": "tool_calls"},
	}})
	chunks, done := readToolStream(t, input)
	states := collectToolStream(t, chunks)
	if !done || len(states) != 2 || states[0].reasoning != "reason" || len(states[0].calls) != 1 || len(states[2].calls) != 1 || states[2].calls[0]["id"] != "native" {
		t.Fatalf("choices were merged: %+v DONE=%v", states, done)
	}
}

/**
 * @brief TestNormalizeStreamMalformedNativeTools rejects invalid upstream tool shapes.
 * @param t Test context.
 */
func TestNormalizeStreamMalformedNativeTools(t *testing.T) {
	for _, call := range []any{
		"invalid",
		map[string]any{"index": -1},
		map[string]any{"index": 0.5},
		map[string]any{"index": "zero"},
		map[string]any{"id": 1},
		map[string]any{"function": "lookup"},
		map[string]any{"function": map[string]any{"name": 1}},
		map[string]any{"function": map[string]any{"name": "lookup", "arguments": map[string]any{}}},
	} {
		input := nativeToolsEvent([]any{call}, "tool_calls")
		chunks, done := readToolStream(t, input)
		last := chunks[len(chunks)-1]
		if done || last["status"] != float64(502) || last["error"] == nil {
			t.Fatalf("invalid native call accepted: %v, output=%v DONE=%v", call, chunks, done)
		}
	}
}

/**
 * @brief TestNormalizeStreamMultipleTextTools keeps multiple wrapped calls separate from native calls.
 * @param t Test context.
 */
func TestNormalizeStreamMultipleTextTools(t *testing.T) {
	input := nativeDelta(`<function_calls><invoke name="lookup"><parameter name="count">1</parameter></invoke><invoke name="lookup"><parameter name="count">2</parameter></invoke></function_calls>`, nil)
	input += nativeToolsEvent([]any{map[string]any{"index": 0, "id": "native", "function": map[string]any{"name": "lookup", "arguments": `{"count":3}`}}}, "tool_calls")
	chunks, done := readToolStream(t, input)
	state := collectToolStream(t, chunks)[0]
	if !done || len(state.calls) != 3 || state.calls[0]["arguments"] != `{"count":1}` || state.calls[1]["arguments"] != `{"count":2}` || state.calls[2]["arguments"] != `{"count":3}` || state.calls[2]["id"] != "native" {
		t.Fatalf("text/native calls collided: %v DONE=%v", state.calls, done)
	}
}

/**
 * @brief TestNormalizeStreamToolFinishReasons preserves truncation and filtering outcomes.
 * @param t Test context.
 */
func TestNormalizeStreamToolFinishReasons(t *testing.T) {
	for _, finish := range []string{"stop", "tool_calls", "length", "content_filter"} {
		for _, native := range []bool{false, true} {
			var input string
			if native {
				input = nativeToolsEvent([]any{map[string]any{"index": 0, "id": "native", "function": map[string]any{"name": "lookup", "arguments": `{"count":1}`}}}, finish)
			} else {
				input = nativeDelta(`<tool_call>{"name":"lookup","arguments":{"count":1}}</tool_call>`, finish)
			}
			chunks, done := readToolStream(t, input)
			state := collectToolStream(t, chunks)[0]
			want := finish
			if finish == "stop" {
				want = "tool_calls"
			}
			if !done || len(state.calls) != 1 || state.finish != want {
				t.Fatalf("finish=%s native=%v became %s; chunks=%v DONE=%v", finish, native, state.finish, chunks, done)
			}
		}
	}
}

/**
 * @brief TestNormalizeStreamNamelessChoiceBeforeAnswer retains a later choice's valid answer.
 * @param t Test context.
 */
func TestNormalizeStreamNamelessChoiceBeforeAnswer(t *testing.T) {
	input := nativeToolsEvent([]any{map[string]any{"index": 0, "id": "unnamed", "function": map[string]any{"arguments": `{"count":1}`}}}, "tool_calls")
	input += nativeEvent(map[string]any{"choices": []any{map[string]any{"index": 1, "delta": map[string]any{"content": "answer"}, "finish_reason": "stop"}}})
	chunks, done := readToolStream(t, input)
	states := collectToolStream(t, chunks)
	if !done || states[0].finish != "stop" || len(states[0].calls) != 0 || states[1].content != "answer" || states[1].finish != "stop" {
		t.Fatalf("later valid choice was discarded: %v DONE=%v", chunks, done)
	}
	firstChoices := chunks[0]["choices"].([]any)
	if firstChoices[0].(map[string]any)["index"] != float64(1) {
		t.Fatalf("empty nameless choice emitted an early frame: %v", chunks[0])
	}
}

/**
 * @brief TestNormalizeStreamQueueRetryHeader exports queue retry guidance in the shared error envelope.
 * @param t Test context.
 */
func TestNormalizeStreamQueueRetryHeader(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		header  string
	}{{0, "30"}, {7, "7"}} {
		input := nativeEvent(map[string]any{"code": 10605, "message": "queued", "retryAfterSeconds": tc.seconds})
		chunks, done := readToolStream(t, input)
		last := chunks[len(chunks)-1]
		headers, ok := last["headers"].(map[string]any)
		if done || last["status"] != float64(503) || !ok || headers["Retry-After"] != tc.header {
			t.Fatalf("queue retry guidance missing: %v DONE=%v", chunks, done)
		}
	}
}

/**
 * @brief TestNormalizeStreamDoneOnlyFinish supplies terminal reasons only after a real DONE marker.
 * @param t Test context.
 */
func TestNormalizeStreamDoneOnlyFinish(t *testing.T) {
	for _, tc := range []struct {
		name, input, finish string
	}{
		{"text tool", nativeDelta(`<tool_call>{"name":"lookup","arguments":{"count":1}}</tool_call>`, nil), "tool_calls"},
		{"buffered text tool", nativeDelta(`<tool_call>{"name":"lookup","arguments":{"count":1}}`, nil), "tool_calls"},
		{"native tool", nativeToolsEvent([]any{map[string]any{"index": 0, "id": "native", "function": map[string]any{"name": "lookup", "arguments": `{"count":1}`}}}, nil), "tool_calls"},
		{"text", nativeDelta("answer", nil), "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, marker := range []string{"", "data: [DONE]\n\n"} {
				chunks, done := readToolStream(t, tc.input+marker)
				state := collectToolStream(t, chunks)[0]
				last := chunks[len(chunks)-1]
				if marker == "" {
					if done || (state != nil && state.finish != "") || last["status"] != float64(502) {
						t.Fatalf("unfinished EOF became success: %v DONE=%v", chunks, done)
					}
				} else if !done || state.finish != tc.finish || last["error"] != nil {
					t.Fatalf("real DONE lost finish=%s: %v DONE=%v", tc.finish, chunks, done)
				}
			}
		})
	}
}

/**
 * @brief TestNormalizeStreamHoldsEmptyPrelude retains metadata without committing empty output.
 * @param t Test context.
 */
func TestNormalizeStreamHoldsEmptyPrelude(t *testing.T) {
	prelude := nativeEvent(map[string]any{"id": "chat-held", "model": "held-model", "created": 123, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}})
	prelude += nativeEvent(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 9, "completion_tokens": 1, "total_tokens": 10}})
	prelude += nativeToolsEvent([]any{map[string]any{"index": 0, "id": "late", "function": map[string]any{"arguments": `{"count":1}`}}}, nil)
	for _, named := range []bool{false, true} {
		input := prelude
		if named {
			input += nativeToolsEvent([]any{map[string]any{"index": 0, "function": map[string]any{"name": "lookup"}}}, "tool_calls")
		} else {
			input += nativeDelta("", "tool_calls")
		}
		chunks, done := readToolStream(t, input)
		first := chunks[0]
		if !named {
			if done || len(chunks) != 1 || first["status"] != float64(502) || first["error"] == nil {
				t.Fatalf("empty prelude committed before nameless failure: %v DONE=%v", chunks, done)
			}
			continue
		}
		states := collectToolStream(t, chunks)
		if !done || len(states[0].calls) != 1 || first["id"] != "chat-held" || first["model"] != "held-model" || first["created"] != float64(123) {
			t.Fatalf("late name lost identity: %v DONE=%v", chunks, done)
		}
		choices := first["choices"].([]any)
		delta := choices[0].(map[string]any)["delta"].(map[string]any)
		calls, ok := delta["tool_calls"].([]any)
		if !ok || len(calls) != 1 || delta["role"] != "assistant" || first["usage"].(map[string]any)["total_tokens"] != float64(10) {
			t.Fatalf("first frame was empty or lost metadata: %v", first)
		}
	}
}
