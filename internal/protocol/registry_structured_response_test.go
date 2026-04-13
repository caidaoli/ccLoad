package protocol_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func TestRegistry_TranslateResponseNonStream_GeminiStructuredOutbound(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"candidates":[{"content":{"parts":[{"text":"hello"},{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"modelVersion":"gemini-2.5-pro"}`)

	t.Run("openai", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.OpenAI, "gpt-4o", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		if !strings.Contains(string(got), `"content":"hello"`) || !strings.Contains(string(got), `"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]`) || !strings.Contains(string(got), `"finish_reason":"tool_calls"`) {
			t.Fatalf("unexpected OpenAI response: %s", got)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		if !strings.Contains(string(got), `"type":"text"`) || !strings.Contains(string(got), `"text":"hello"`) || !strings.Contains(string(got), `"type":"tool_use"`) || !strings.Contains(string(got), `"id":"call_1"`) || !strings.Contains(string(got), `"name":"lookup"`) || !strings.Contains(string(got), `"input":{"query":"go"}`) || !strings.Contains(string(got), `"stop_reason":"tool_use"`) {
			t.Fatalf("unexpected Anthropic response: %s", got)
		}
	})

	t.Run("codex", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.Codex, "gpt-5-codex", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		if !strings.Contains(string(got), `"type":"message"`) || !strings.Contains(string(got), `"role":"assistant"`) || !strings.Contains(string(got), `"type":"output_text"`) || !strings.Contains(string(got), `"text":"hello"`) || !strings.Contains(string(got), `"type":"function_call"`) || !strings.Contains(string(got), `"call_id":"call_1"`) || !strings.Contains(string(got), `"name":"lookup"`) || !strings.Contains(string(got), `"arguments":{"query":"go"}`) {
			t.Fatalf("unexpected Codex response: %s", got)
		}
	})
}

func TestRegistry_TranslateResponseStream_GeminiStructuredOutbound(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("openai", func(t *testing.T) {
		var state any
		chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.OpenAI, "gpt-4o", nil, nil, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"go\"}}}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n"), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		joined := string(chunks[0])
		if len(chunks) != 1 || !strings.Contains(joined, `"tool_calls"`) || !strings.Contains(joined, `"id":"call_1"`) || !strings.Contains(joined, `"name":"lookup"`) || !strings.Contains(joined, `"arguments":"{\"query\":\"go\"}"`) || !strings.Contains(joined, `"finish_reason":"tool_calls"`) {
			t.Fatalf("unexpected OpenAI stream chunk: %#v", chunks)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		var state any
		chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"go\"}}}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro\",\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5}}\n\n"), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		joined := string(bytes.Join(chunks, nil))
		if !strings.Contains(joined, `event: message_start`) || !strings.Contains(joined, `"type":"tool_use"`) || !strings.Contains(joined, `"id":"call_1"`) || !strings.Contains(joined, `"name":"lookup"`) || !strings.Contains(joined, `"partial_json":"{\"query\":\"go\"}"`) || !strings.Contains(joined, `"stop_reason":"tool_use"`) || !strings.Contains(joined, `event: message_stop`) {
			t.Fatalf("unexpected Anthropic stream chunks: %s", joined)
		}
	})

	t.Run("codex", func(t *testing.T) {
		var state any
		chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Codex, "gpt-5-codex", nil, nil, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"go\"}}}]}}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n"), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		joined := string(chunks[0])
		if len(chunks) != 1 || !strings.Contains(joined, `event: response.output_item.done`) || !strings.Contains(joined, `"type":"function_call"`) || !strings.Contains(joined, `"call_id":"call_1"`) || !strings.Contains(joined, `"name":"lookup"`) || !strings.Contains(joined, `"arguments":{"query":"go"}`) {
			t.Fatalf("unexpected Codex stream chunk: %#v", chunks)
		}
	})
}

func TestRegistry_TranslateResponseNonStream_AnthropicStructuredOutbound(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"query":"go"}}],"model":"claude-3-5-sonnet","stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`)

	t.Run("openai", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		if !strings.Contains(string(got), `"content":"hello"`) || !strings.Contains(string(got), `"tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]`) || !strings.Contains(string(got), `"finish_reason":"tool_calls"`) {
			t.Fatalf("unexpected OpenAI response: %s", got)
		}
	})

	t.Run("codex", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		if !strings.Contains(string(got), `"type":"message"`) || !strings.Contains(string(got), `"role":"assistant"`) || !strings.Contains(string(got), `"type":"output_text"`) || !strings.Contains(string(got), `"text":"hello"`) || !strings.Contains(string(got), `"type":"function_call"`) || !strings.Contains(string(got), `"call_id":"toolu_1"`) || !strings.Contains(string(got), `"name":"lookup"`) || !strings.Contains(string(got), `"arguments":{"query":"go"}`) {
			t.Fatalf("unexpected Codex response: %s", got)
		}
	})
}

func TestRegistry_TranslateResponseNonStream_OpenAIStructuredOutboundToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":0,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	body := string(got)
	if !strings.Contains(body, `"functionCall"`) || !strings.Contains(body, `"name":"lookup"`) || !strings.Contains(body, `"query":"go"`) || !strings.Contains(body, `"finishReason":"STOP"`) {
		t.Fatalf("unexpected Gemini response: %s", got)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIStructuredOutboundToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}` + "\n\n",
	}

	var state any
	var outputs [][]byte
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		outputs = append(outputs, out...)
	}

	joined := string(bytes.Join(outputs, nil))
	if !strings.Contains(joined, `"functionCall"`) || !strings.Contains(joined, `"name":"lookup"`) || !strings.Contains(joined, `"query":"go"`) || !strings.Contains(joined, `"finishReason":"STOP"`) || !strings.Contains(joined, `"promptTokenCount":3`) {
		t.Fatalf("unexpected Gemini stream output: %s", joined)
	}
}

func TestRegistry_TranslateResponseStream_AnthropicStructuredOutbound(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("openai", func(t *testing.T) {
		var state any
		start := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\"}}\n\n")
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, start, &state); err != nil || out != nil {
			t.Fatalf("content_block_start = %#v, %v", out, err)
		}
		delta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n")
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, delta, &state); err != nil || out != nil {
			t.Fatalf("content_block_delta = %#v, %v", out, err)
		}
		toolChunk, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_stop failed: %v", err)
		}
		joined := string(toolChunk[0])
		if len(toolChunk) != 1 || !strings.Contains(joined, `"tool_calls"`) || !strings.Contains(joined, `"id":"toolu_1"`) || !strings.Contains(joined, `"name":"lookup"`) || !strings.Contains(joined, `"arguments":"{\"query\":\"go\"}"`) {
			t.Fatalf("unexpected OpenAI tool chunk: %#v", toolChunk)
		}
		finish, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_delta failed: %v", err)
		}
		if len(finish) != 1 || !strings.Contains(string(finish[0]), `"finish_reason":"tool_calls"`) || !strings.Contains(string(finish[0]), `"prompt_tokens":3`) {
			t.Fatalf("unexpected OpenAI finish chunk: %#v", finish)
		}
		done, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_stop failed: %v", err)
		}
		if len(done) != 1 || string(done[0]) != "data: [DONE]\n\n" {
			t.Fatalf("unexpected OpenAI done chunk: %#v", done)
		}
	})

	t.Run("codex", func(t *testing.T) {
		var state any
		start := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3-5-sonnet\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n")
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, start, &state); err != nil || out != nil {
			t.Fatalf("message_start = %#v, %v", out, err)
		}
		toolStart := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\"}}\n\n")
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, toolStart, &state); err != nil || out != nil {
			t.Fatalf("content_block_start = %#v, %v", out, err)
		}
		toolDelta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n")
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, toolDelta, &state); err != nil || out != nil {
			t.Fatalf("content_block_delta = %#v, %v", out, err)
		}
		toolChunk, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_stop failed: %v", err)
		}
		joined := string(toolChunk[0])
		if len(toolChunk) != 1 || !strings.Contains(joined, `event: response.output_item.done`) || !strings.Contains(joined, `"type":"function_call"`) || !strings.Contains(joined, `"call_id":"toolu_1"`) || !strings.Contains(joined, `"name":"lookup"`) || !strings.Contains(joined, `"arguments":{"query":"go"}`) {
			t.Fatalf("unexpected Codex tool chunk: %#v", toolChunk)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("message_delta = %#v, %v", out, err)
		}
		done, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_stop failed: %v", err)
		}
		if len(done) != 1 || !strings.Contains(string(done[0]), `event: response.completed`) || !strings.Contains(string(done[0]), `"input_tokens":3`) || !strings.Contains(string(done[0]), `"output_tokens":5`) {
			t.Fatalf("unexpected Codex done chunk: %#v", done)
		}
	})
}

func TestRegistry_TranslateResponseNonStream_AnthropicReasoningAndUsageDetails(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"step by step","signature":"sig_1"},{"type":"text","text":"hello"},{"type":"redacted_thinking","data":"redacted_blob"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5,"cache_read_input_tokens":7,"cache_creation_input_tokens":11,"reasoning_tokens":13}}`)

	t.Run("openai", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		body := string(got)
		if !strings.Contains(body, `"reasoning_content":"step by step"`) || !strings.Contains(body, `"type":"thinking"`) || !strings.Contains(body, `"signature":"sig_1"`) || !strings.Contains(body, `"type":"redacted_thinking"`) || !strings.Contains(body, `"data":"redacted_blob"`) {
			t.Fatalf("unexpected OpenAI reasoning payload: %s", got)
		}
		if !strings.Contains(body, `"prompt_tokens":21`) || !strings.Contains(body, `"cached_tokens":7`) || !strings.Contains(body, `"cache_creation_input_tokens":11`) || !strings.Contains(body, `"reasoning_tokens":13`) {
			t.Fatalf("unexpected OpenAI usage payload: %s", got)
		}
	})

	t.Run("codex", func(t *testing.T) {
		got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, rawResp)
		if err != nil {
			t.Fatalf("TranslateResponseNonStream failed: %v", err)
		}
		body := string(got)
		if !strings.Contains(body, `"type":"reasoning"`) || !strings.Contains(body, `"type":"reasoning_text"`) || !strings.Contains(body, `"text":"step by step"`) || !strings.Contains(body, `"encrypted_content":"sig_1"`) || !strings.Contains(body, `"text":"hello"`) {
			t.Fatalf("unexpected Codex reasoning payload: %s", got)
		}
		if !strings.Contains(body, `"input_tokens":21`) || !strings.Contains(body, `"cached_tokens":7`) || !strings.Contains(body, `"cache_creation_input_tokens":11`) || !strings.Contains(body, `"reasoning_tokens":13`) {
			t.Fatalf("unexpected Codex usage payload: %s", got)
		}
	})
}

func TestRegistry_TranslateResponseStream_AnthropicReasoningAndUsageDetails(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("openai", func(t *testing.T) {
		var state any
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-3-5-sonnet\",\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":11}}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("message_start = %#v, %v", out, err)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("content_block_start = %#v, %v", out, err)
		}
		reasoning, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"step by step\"}}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_delta failed: %v", err)
		}
		if len(reasoning) != 1 || !strings.Contains(string(reasoning[0]), `"reasoning_content":"step by step"`) {
			t.Fatalf("unexpected OpenAI reasoning chunk: %#v", reasoning)
		}
		meta, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig_1\"}}\n\n"), &state)
		if err != nil || meta != nil {
			t.Fatalf("signature_delta = %#v, %v", meta, err)
		}
		meta, err = reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_stop failed: %v", err)
		}
		if len(meta) != 1 || !strings.Contains(string(meta[0]), `"reasoning"`) || !strings.Contains(string(meta[0]), `"type":"thinking"`) || !strings.Contains(string(meta[0]), `"signature":"sig_1"`) {
			t.Fatalf("unexpected OpenAI reasoning meta chunk: %#v", meta)
		}
		finish, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":11,\"reasoning_tokens\":13}}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_delta failed: %v", err)
		}
		if len(finish) != 1 || !strings.Contains(string(finish[0]), `"prompt_tokens":21`) || !strings.Contains(string(finish[0]), `"cached_tokens":7`) || !strings.Contains(string(finish[0]), `"cache_creation_input_tokens":11`) || !strings.Contains(string(finish[0]), `"reasoning_tokens":13`) {
			t.Fatalf("unexpected OpenAI finish chunk: %#v", finish)
		}
	})

	t.Run("codex", func(t *testing.T) {
		var state any
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3-5-sonnet\",\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":11}}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("message_start = %#v, %v", out, err)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("content_block_start = %#v, %v", out, err)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"step by step\"}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("thinking_delta = %#v, %v", out, err)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig_1\"}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("signature_delta = %#v, %v", out, err)
		}
		reasoning, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
		if err != nil {
			t.Fatalf("content_block_stop failed: %v", err)
		}
		if len(reasoning) != 1 || !strings.Contains(string(reasoning[0]), `event: response.output_item.done`) || !strings.Contains(string(reasoning[0]), `"type":"reasoning"`) || !strings.Contains(string(reasoning[0]), `"text":"step by step"`) || !strings.Contains(string(reasoning[0]), `"encrypted_content":"sig_1"`) {
			t.Fatalf("unexpected Codex reasoning chunk: %#v", reasoning)
		}
		if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":11,\"reasoning_tokens\":13}}\n\n"), &state); err != nil || out != nil {
			t.Fatalf("message_delta = %#v, %v", out, err)
		}
		done, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5-codex", nil, nil, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), &state)
		if err != nil {
			t.Fatalf("message_stop failed: %v", err)
		}
		if len(done) != 1 || !strings.Contains(string(done[0]), `"input_tokens":21`) || !strings.Contains(string(done[0]), `"cached_tokens":7`) || !strings.Contains(string(done[0]), `"cache_creation_input_tokens":11`) || !strings.Contains(string(done[0]), `"reasoning_tokens":13`) {
			t.Fatalf("unexpected Codex done chunk: %#v", done)
		}
	})
}
