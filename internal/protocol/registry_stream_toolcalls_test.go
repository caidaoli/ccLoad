package protocol_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

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
	if !strings.Contains(result, `"text"`) {
		t.Fatalf("expected text field in arguments, got:\n%s", result)
	}
	if !strings.Contains(result, `event: content_block_stop`) {
		t.Fatalf("expected content_block_stop, got:\n%s", result)
	}
	if !strings.Contains(result, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason=tool_use, got:\n%s", result)
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
