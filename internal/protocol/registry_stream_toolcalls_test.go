package protocol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"

	"github.com/tidwall/gjson"
)

func registryToolInputEvents(t *testing.T, chunks [][]byte) []gjson.Result {
	t.Helper()
	var events []gjson.Result
	for _, chunk := range chunks {
		for _, line := range bytes.Split(chunk, []byte{'\n'}) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if !json.Valid(payload) {
				t.Fatalf("invalid translated SSE data %q", payload)
			}
			events = append(events, gjson.ParseBytes(payload))
		}
	}
	return events
}

func TestRegistryApplyPatchResponsesValidateArgumentsAndCompleteConsistently(t *testing.T) {
	request := []byte(`{"model":"test","input":"edit","tools":[{"type":"custom","name":"apply_patch"}]}`)
	for _, source := range []protocol.Protocol{protocol.OpenAI, protocol.Anthropic, protocol.Gemini} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/invalid=%v", source, invalid), func(t *testing.T) {
				reg := protocol.NewRegistry()
				builtin.Register(reg)
				translated, err := reg.TranslateRequest(protocol.Codex, source, "test", request, true)
				if err != nil {
					t.Fatal(err)
				}
				input := any("patch\nnext")
				if invalid {
					input = 42
				}
				args, err := json.Marshal(map[string]any{"input": input})
				if err != nil {
					t.Fatal(err)
				}
				call := map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "apply_patch", "arguments": string(args)}}
				var response any
				var rawEvents []any
				switch source {
				case protocol.OpenAI:
					response = map[string]any{"id": "chat_1", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": []any{call}}, "finish_reason": "tool_calls"}}}
					call["index"] = 0
					rawEvents = []any{map[string]any{"id": "chat_1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}, "finish_reason": "tool_calls"}}}, "[DONE]"}
				case protocol.Anthropic:
					response = map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "test", "content": []any{map[string]any{"type": "tool_use", "id": "c1", "name": "apply_patch", "input": json.RawMessage(args)}}, "stop_reason": "tool_use", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}
					rawEvents = []any{
						map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "model": "test", "usage": map[string]int{"input_tokens": 1}}},
						map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "c1", "name": "apply_patch", "input": map[string]any{}}},
						map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)}},
						map[string]any{"type": "content_block_stop", "index": 0},
						map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}, "usage": map[string]int{"output_tokens": 1}},
						map[string]any{"type": "message_stop"},
					}
				case protocol.Gemini:
					response = map[string]any{"responseId": "gemini_1", "modelVersion": "test", "candidates": []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"id": "c1", "name": "apply_patch", "args": json.RawMessage(args)}}}}, "finishReason": "STOP"}}}
					rawEvents = []any{response}
				}
				raw, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				body, nonStreamErr := reg.TranslateResponseNonStream(context.Background(), source, protocol.Codex, "test", request, translated, raw)
				if (nonStreamErr != nil) != invalid {
					t.Fatalf("non-stream error=%v, invalid=%v, response=%s", nonStreamErr, invalid, body)
				}
				var state any
				var events []gjson.Result
				var streamErr error
				for _, event := range rawEvents {
					var payload []byte
					if event == "[DONE]" {
						payload = []byte("[DONE]")
					} else {
						payload, err = json.Marshal(event)
						if err != nil {
							t.Fatal(err)
						}
					}
					chunks, err := reg.TranslateResponseStream(context.Background(), source, protocol.Codex, "test", request, translated, append(append([]byte("data: "), payload...), '\n', '\n'), &state)
					events = append(events, registryToolInputEvents(t, chunks)...)
					if err != nil {
						streamErr = err
						break
					}
				}
				chunks, finalizeErr := protocol.FinalizeResponseToolInput(state)
				events = append(events, registryToolInputEvents(t, chunks)...)
				if (streamErr != nil) != invalid || (finalizeErr != nil) != invalid {
					t.Fatalf("stream error=%v finalize error=%v, invalid=%v", streamErr, finalizeErr, invalid)
				}
				failures, completions := 0, 0
				var deltaInput, doneInput, itemInput, terminalInput string
				for _, event := range events {
					switch event.Get("type").String() {
					case "response.failed":
						failures++
						if event.Get("response.error.code").String() != "invalid_tool_arguments" {
							t.Fatalf("unexpected failure: %s", event.Raw)
						}
					case "response.completed":
						completions++
						terminalInput = event.Get("response.output.0.input").String()
					case "response.custom_tool_call_input.delta":
						deltaInput += event.Get("delta").String()
					case "response.custom_tool_call_input.done":
						doneInput = event.Get("input").String()
					case "response.output_item.done":
						itemInput = event.Get("item.input").String()
					}
				}
				if invalid {
					if failures != 1 || completions != 0 || doneInput != "" || itemInput != "" {
						t.Fatalf("invalid input completed: failures=%d completions=%d done=%q item=%q", failures, completions, doneInput, itemInput)
					}
				} else if failures != 0 || completions != 1 || deltaInput != input || doneInput != input || itemInput != input || terminalInput != input || gjson.GetBytes(body, "output.0.input").String() != input {
					t.Fatalf("inconsistent successful patch: delta=%q done=%q item=%q terminal=%q nonstream=%s", deltaInput, doneInput, itemInput, terminalInput, body)
				}
			})
		}
	}
}

func TestRegistryApplyPatchEmptyEOFRejectsMissingSourceTerminator(t *testing.T) {
	request := []byte(`{"model":"test","input":"edit","tools":[{"type":"custom","name":"apply_patch"}]}`)
	for _, source := range []protocol.Protocol{protocol.OpenAI, protocol.Anthropic, protocol.Gemini} {
		t.Run(string(source), func(t *testing.T) {
			reg := protocol.NewRegistry()
			builtin.Register(reg)
			var state any
			if _, err := reg.TranslateResponseStream(context.Background(), source, protocol.Codex, "test", request, nil, nil, &state); err != nil {
				t.Fatal(err)
			}
			chunks, err := protocol.FinalizeResponseToolInput(state)
			events := registryToolInputEvents(t, chunks)
			if err == nil || len(events) != 1 || events[0].Get("type").String() != "response.failed" {
				t.Fatalf("empty EOF succeeded: error=%v events=%v", err, events)
			}
			if chunks, err := protocol.FinalizeResponseToolInput(state); err == nil || len(chunks) != 0 {
				t.Fatalf("repeated EOF duplicated failure: error=%v chunks=%q", err, chunks)
			}
		})
	}
}

// TestRegistry_Stream_OpenAIToCodex_ToolCalls 验证 OpenAI stream tool_calls 增量
// 经过多个 chunk 拼接 arguments 后，[DONE] 时输出 response.output_item.done（type=function_call）。
func TestRegistry_Stream_OpenAIToCodex_ToolCalls(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		// chunk 1: tool_call 开头，携带 id/name，arguments 为空字符串
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}` + "\n\n",
		// chunk 2: arguments 第一段
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}` + "\n\n",
		// chunk 3: arguments 第二段（完整 JSON 闭合）
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]}}]}` + "\n\n",
		// chunk 4: finish_reason
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		// chunk 5: [DONE]
		"data: [DONE]\n\n",
	}

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-4o", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}

	result := allOutput.String()
	if !strings.Contains(result, `event: response.output_item.done`) {
		t.Fatalf("expected response.output_item.done event, got:\n%s", result)
	}
	if !strings.Contains(result, `"type":"function_call"`) {
		t.Fatalf("expected type=function_call, got:\n%s", result)
	}
	if !strings.Contains(result, `"name":"get_weather"`) {
		t.Fatalf("expected name=get_weather, got:\n%s", result)
	}
	// 拼接后的完整 arguments 字符串应完整出现（JSON 字符串内部以转义形式存在）
	if !strings.Contains(result, `city`) {
		t.Fatalf("expected city in arguments, got:\n%s", result)
	}
	if !strings.Contains(result, `Beijing`) {
		t.Fatalf("expected Beijing in arguments, got:\n%s", result)
	}
	// call_id 应保留原始 id
	if !strings.Contains(result, `"call_id":"call_abc"`) {
		t.Fatalf("expected call_id=call_abc, got:\n%s", result)
	}
	// 必须有 response.completed
	if !strings.Contains(result, `event: response.completed`) {
		t.Fatalf("expected response.completed event, got:\n%s", result)
	}
}

// TestRegistry_Stream_CodexToOpenAI_FunctionCall 验证 Codex stream function_call 转成 OpenAI tool_calls chunk。
func TestRegistry_Stream_CodexToOpenAI_FunctionCall(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	// Codex SSE 格式: event: <type>\ndata: <json>\n\n
	codexChunk := `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_xyz","name":"search","arguments":"{\"q\":\"hello\"}"}}

`

	var state any
	out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", nil, nil, []byte(codexChunk), &state)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected output chunks, got none")
	}

	result := string(bytes.Join(out, nil))
	if !strings.Contains(result, `"tool_calls"`) {
		t.Fatalf("expected tool_calls in output, got:\n%s", result)
	}
	if !strings.Contains(result, `"id":"call_xyz"`) {
		t.Fatalf("expected id=call_xyz, got:\n%s", result)
	}
	if !strings.Contains(result, `"name":"search"`) {
		t.Fatalf("expected name=search, got:\n%s", result)
	}
	if !strings.Contains(result, `hello`) {
		t.Fatalf("expected 'hello' in arguments, got:\n%s", result)
	}
}

func TestRegistry_Stream_CodexToOpenAI_FunctionCallStringArgumentsStayRawJSON(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	codexChunk := `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_skill_1","name":"Skill","arguments":"{\"args\":\"skill: \\\"superpowers:using-superpowers\\\"\",\"skill\":\"superpowers:using-superpowers\"}"}}

`

	var state any
	out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", nil, nil, []byte(codexChunk), &state)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected one output chunk, got %#v", out)
	}
	result := string(out[0])
	if !strings.Contains(result, `"arguments":"{\"args\":\"skill: \\\"superpowers:using-superpowers\\\"\",\"skill\":\"superpowers:using-superpowers\"}"`) {
		t.Fatalf("expected raw JSON arguments string, got:\n%s", result)
	}
}

func TestRegistry_Stream_CodexToOpenAI_FunctionCallIndices(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"one\"}"}}

`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_2","name":"search","arguments":"{\"q\":\"two\"}"}}

`,
	}

	var state any
	var outputs [][]byte
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		outputs = append(outputs, out...)
	}

	if len(outputs) != 2 {
		t.Fatalf("expected 2 output chunks, got %d", len(outputs))
	}
	if !strings.Contains(string(outputs[0]), `"index":0`) {
		t.Fatalf("expected first tool call index 0, got:\n%s", outputs[0])
	}
	if !strings.Contains(string(outputs[1]), `"index":1`) {
		t.Fatalf("expected second tool call index 1, got:\n%s", outputs[1])
	}
}

func TestRegistry_Stream_CodexToOpenAI_FunctionCallCompletion(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"one\"}"}}

`,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-4o","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}

`,
	}

	var state any
	var outputs [][]byte
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		outputs = append(outputs, out...)
	}

	if len(outputs) != 3 {
		t.Fatalf("expected tool call chunk + finish chunk + [DONE], got %d", len(outputs))
	}
	if !strings.Contains(string(outputs[1]), `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected completion finish_reason=tool_calls, got:\n%s", outputs[1])
	}
	if string(outputs[2]) != "data: [DONE]\n\n" {
		t.Fatalf("expected OpenAI DONE sentinel, got:\n%s", outputs[2])
	}
}

// TestRegistry_Stream_CodexToAnthropic_FunctionCall 验证 Codex stream function_call
// 转成 Anthropic content_block_start(type=tool_use) + input_json_delta + content_block_stop。
func TestRegistry_Stream_CodexToAnthropic_FunctionCall(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	codexChunk := `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","id":"toolu_01","call_id":"call_fc1","name":"calculator","arguments":"{\"expr\":\"1+2\"}"}}

`

	var state any
	out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte(codexChunk), &state)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected output chunks, got none")
	}

	result := string(bytes.Join(out, nil))
	if !strings.Contains(result, `event: content_block_start`) {
		t.Fatalf("expected content_block_start, got:\n%s", result)
	}
	if !strings.Contains(result, `"type":"tool_use"`) {
		t.Fatalf("expected type=tool_use, got:\n%s", result)
	}
	if !strings.Contains(result, `"name":"calculator"`) {
		t.Fatalf("expected name=calculator, got:\n%s", result)
	}
	if !strings.Contains(result, `event: content_block_delta`) {
		t.Fatalf("expected content_block_delta, got:\n%s", result)
	}
	if !strings.Contains(result, `"input_json_delta"`) {
		t.Fatalf("expected input_json_delta type, got:\n%s", result)
	}
	if !strings.Contains(result, `expr`) {
		t.Fatalf("expected expr in partial_json, got:\n%s", result)
	}
	if !strings.Contains(result, `event: content_block_stop`) {
		t.Fatalf("expected content_block_stop, got:\n%s", result)
	}
}

func TestRegistry_Stream_CodexToAnthropic_FunctionCallUsesCallID(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	codexChunk := `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_fc1","name":"calculator","arguments":"{\"expr\":\"1+2\"}"}}

`

	var state any
	out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte(codexChunk), &state)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected output chunks, got none")
	}

	result := string(bytes.Join(out, nil))
	if !strings.Contains(result, `"id":"call_fc1"`) {
		t.Fatalf("expected tool_use id to preserve call_id, got:\n%s", result)
	}
}

func TestRegistry_Stream_CodexToAnthropic_FunctionCallCompletion(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_fc1","name":"calculator","arguments":"{\"expr\":\"1+2\"}"}}

`,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"claude-3-5-sonnet","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}

`,
	}

	var state any
	var outputs [][]byte
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		outputs = append(outputs, out...)
	}

	result := string(bytes.Join(outputs, nil))
	if strings.Count(result, `event: content_block_stop`) != 1 {
		t.Fatalf("expected exactly one content_block_stop for tool-only response, got:\n%s", result)
	}
	if !strings.Contains(result, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason=tool_use for tool-only response, got:\n%s", result)
	}
}

func TestRegistry_Stream_CodexToAnthropic_TextThenFunctionCall(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"thinking..."}

`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_fc1","name":"calculator","arguments":"{\"expr\":\"1+2\"}"}}

`,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"claude-3-5-sonnet","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}

`,
	}

	var state any
	var outputs [][]byte
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		outputs = append(outputs, out...)
	}

	result := string(bytes.Join(outputs, nil))
	if strings.Count(result, `event: content_block_start`) != 2 {
		t.Fatalf("expected separate text/tool content blocks, got:\n%s", result)
	}
	if !strings.Contains(result, `event: content_block_start`) || !strings.Contains(result, `"index":0`) || !strings.Contains(result, `"type":"text"`) {
		t.Fatalf("expected text block at index 0, got:\n%s", result)
	}
	if !strings.Contains(result, `"index":1`) || !strings.Contains(result, `"type":"tool_use"`) || !strings.Contains(result, `"id":"call_fc1"`) || !strings.Contains(result, `"name":"calculator"`) {
		t.Fatalf("expected tool block at index 1, got:\n%s", result)
	}
	if strings.Count(result, `event: content_block_stop`) != 2 {
		t.Fatalf("expected exactly two content_block_stop events, got:\n%s", result)
	}
	if !strings.Contains(result, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason=tool_use when final block is tool_use, got:\n%s", result)
	}
}

// TestRegistry_Stream_CodexToAnthropic_Reasoning 验证 Codex stream reasoning（有 summary text）
// 转成 Anthropic thinking 块（content_block_start + thinking_delta + content_block_stop）。
func TestRegistry_Stream_CodexToAnthropic_Reasoning(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	// reasoning item 包含 summary 数组
	codexChunk := `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_01","summary":[{"type":"summary_text","text":"step by step reasoning"}]}}

`

	var state any
	out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte(codexChunk), &state)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected output chunks, got none")
	}

	result := string(bytes.Join(out, nil))
	if !strings.Contains(result, `event: content_block_start`) {
		t.Fatalf("expected content_block_start, got:\n%s", result)
	}
	if !strings.Contains(result, `"type":"thinking"`) {
		t.Fatalf("expected type=thinking, got:\n%s", result)
	}
	if !strings.Contains(result, `event: content_block_delta`) {
		t.Fatalf("expected content_block_delta, got:\n%s", result)
	}
	if !strings.Contains(result, `"thinking_delta"`) {
		t.Fatalf("expected thinking_delta type, got:\n%s", result)
	}
	if !strings.Contains(result, `"step by step reasoning"`) {
		t.Fatalf("expected reasoning text, got:\n%s", result)
	}
	if !strings.Contains(result, `event: content_block_stop`) {
		t.Fatalf("expected content_block_stop, got:\n%s", result)
	}
}

// TestRegistry_Stream_OpenAIToAnthropic_ToolCalls 验证 OpenAI stream tool_calls 增量
// 在 finish_reason=tool_calls 时批量输出 Anthropic tool_use 块（start+delta+stop）。
func TestRegistry_Stream_OpenAIToAnthropic_ToolCalls(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		// chunk 1: tool_call 首 chunk，携带 id/name
		`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_t1","type":"function","function":{"name":"translate","arguments":""}}]}}]}` + "\n\n",
		// chunk 2: arguments 增量
		`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"text\":\"hello\"}"}}]}}]}` + "\n\n",
		// chunk 3: finish_reason，触发 flush
		`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "gpt-4o", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}

	result := allOutput.String()
	if !strings.Contains(result, `event: content_block_start`) {
		t.Fatalf("expected content_block_start, got:\n%s", result)
	}
	if !strings.Contains(result, `"type":"tool_use"`) {
		t.Fatalf("expected type=tool_use, got:\n%s", result)
	}
	if !strings.Contains(result, `"name":"translate"`) {
		t.Fatalf("expected name=translate, got:\n%s", result)
	}
	if !strings.Contains(result, `event: content_block_delta`) {
		t.Fatalf("expected content_block_delta, got:\n%s", result)
	}
	if !strings.Contains(result, `"input_json_delta"`) {
		t.Fatalf("expected input_json_delta type, got:\n%s", result)
	}
	if !strings.Contains(result, `"partial_json":"{\"text\":\"hello\"}"`) {
		t.Fatalf("expected tool arguments in partial_json, got:\n%s", result)
	}
	if !strings.Contains(result, `event: content_block_stop`) {
		t.Fatalf("expected content_block_stop, got:\n%s", result)
	}
	if !strings.Contains(result, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason=tool_use, got:\n%s", result)
	}
}

func TestRegistry_Stream_OpenAIToAnthropic_TextThenFragmentedToolCalls(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`data: {"id":"chatcmpl-4","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"让我检查一下"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-4","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"Bash","arguments":""}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-4","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"ls\""}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-4","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "gpt-4o", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}

	result := allOutput.String()
	events := parseSSEEvents(t, result)
	expectedEvents := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(events) != len(expectedEvents) {
		t.Fatalf("expected %d SSE events, got %d:\n%s", len(expectedEvents), len(events), result)
	}
	for i, expected := range expectedEvents {
		if events[i].Event != expected {
			t.Fatalf("expected event[%d]=%s, got %s:\n%s", i, expected, events[i].Event, result)
		}
	}

	textStart := mustMap(t, events[1].Data["content_block"])
	if mustInt(t, events[1].Data["index"]) != 0 || mustString(t, textStart["type"]) != "text" {
		t.Fatalf("expected text block to start at index 0, got:\n%s", result)
	}
	textDelta := mustMap(t, events[2].Data["delta"])
	if mustInt(t, events[2].Data["index"]) != 0 || mustString(t, textDelta["type"]) != "text_delta" || mustString(t, textDelta["text"]) != "让我检查一下" {
		t.Fatalf("expected text delta to stay on index 0, got:\n%s", result)
	}
	if mustInt(t, events[3].Data["index"]) != 0 {
		t.Fatalf("expected text block to close on index 0 before tool call, got:\n%s", result)
	}

	toolStart := mustMap(t, events[4].Data["content_block"])
	if mustInt(t, events[4].Data["index"]) != 1 ||
		mustString(t, toolStart["type"]) != "tool_use" ||
		mustString(t, toolStart["id"]) != "call_lookup" ||
		mustString(t, toolStart["name"]) != "Bash" {
		t.Fatalf("expected tool block to open on index 1, got:\n%s", result)
	}
	toolDelta := mustMap(t, events[5].Data["delta"])
	if mustInt(t, events[5].Data["index"]) != 1 ||
		mustString(t, toolDelta["type"]) != "input_json_delta" ||
		mustString(t, toolDelta["partial_json"]) != `{"command":"ls"}` {
		t.Fatalf("expected fragmented tool arguments to be reassembled, got:\n%s", result)
	}
	if mustInt(t, events[6].Data["index"]) != 1 {
		t.Fatalf("expected tool block to close on index 1, got:\n%s", result)
	}

	messageDelta := mustMap(t, events[7].Data["delta"])
	if mustString(t, messageDelta["stop_reason"]) != "tool_use" {
		t.Fatalf("expected stop_reason=tool_use, got:\n%s", result)
	}
}

func TestRegistry_Stream_OpenAIToAnthropic_ToolCalls_AllSplitPoints(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	arguments := `{"command":"ls"}`
	for split := 0; split <= len(arguments); split++ {
		t.Run(fmt.Sprintf("split-%d", split), func(t *testing.T) {
			chunks := []string{
				`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"Bash","arguments":""}}]}}]}` + "\n\n",
				fmt.Sprintf(`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`+"\n\n", arguments[:split]),
				fmt.Sprintf(`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":"tool_calls"}]}`+"\n\n", arguments[split:]),
				"data: [DONE]\n\n",
			}

			var state any
			var allOutput bytes.Buffer
			for _, chunk := range chunks {
				out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "gpt-4o", nil, nil, []byte(chunk), &state)
				if err != nil {
					t.Fatalf("split %d stream error: %v", split, err)
				}
				for _, b := range out {
					allOutput.Write(b)
				}
			}

			result := allOutput.String()
			if !strings.Contains(result, `"id":"call_lookup"`) || !strings.Contains(result, `"name":"Bash"`) {
				t.Fatalf("split %d expected tool_use metadata, got:\n%s", split, result)
			}
			if !strings.Contains(result, `"partial_json":"{\"command\":\"ls\"}"`) || !strings.Contains(result, `"stop_reason":"tool_use"`) {
				t.Fatalf("split %d expected reassembled anthropic tool payload, got:\n%s", split, result)
			}
		})
	}
}

// TestRegistry_Stream_OpenAIToAnthropic_Reasoning 验证 OpenAI stream reasoning_content
// 转成 Anthropic thinking 块（start + thinking_delta + stop），finish_reason=stop 时关闭块。
func TestRegistry_Stream_OpenAIToAnthropic_Reasoning(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		// chunk 1: reasoning_content 第一段
		`data: {"id":"chatcmpl-3","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"reasoning_content":"Let me think"}}]}` + "\n\n",
		// chunk 2: reasoning_content 第二段
		`data: {"id":"chatcmpl-3","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"reasoning_content":" carefully"}}]}` + "\n\n",
		// chunk 3: finish_reason=stop，关闭 thinking 块
		`data: {"id":"chatcmpl-3","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "gpt-4o", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}

	result := allOutput.String()
	if !strings.Contains(result, `event: content_block_start`) {
		t.Fatalf("expected content_block_start, got:\n%s", result)
	}
	if !strings.Contains(result, `"type":"thinking"`) {
		t.Fatalf("expected type=thinking, got:\n%s", result)
	}
	if !strings.Contains(result, `"thinking_delta"`) {
		t.Fatalf("expected thinking_delta type, got:\n%s", result)
	}
	if !strings.Contains(result, `"Let me think"`) {
		t.Fatalf("expected first reasoning chunk, got:\n%s", result)
	}
	if !strings.Contains(result, `" carefully"`) {
		t.Fatalf("expected second reasoning chunk, got:\n%s", result)
	}
	if !strings.Contains(result, `event: content_block_stop`) {
		t.Fatalf("expected content_block_stop, got:\n%s", result)
	}
	// finish_reason=stop → stop_reason=end_turn
	if !strings.Contains(result, `"stop_reason":"end_turn"`) {
		t.Fatalf("expected stop_reason=end_turn, got:\n%s", result)
	}
}
