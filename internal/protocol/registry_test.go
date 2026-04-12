package protocol_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func TestRegistry_TranslateRequest_OpenAIToGemini(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if string(got) != `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}` {
		t.Fatalf("unexpected translated request: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_GeminiToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	translatedReq := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	rawResp := []byte(`{"candidates":[{"content":{"parts":[{"text":"world"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"modelVersion":"gemini-2.5-pro"}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.OpenAI, "gemini-2.5-pro", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	want := `{"id":"chatcmpl-proxy","object":"chat.completion","created":0,"model":"gemini-2.5-pro","choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`
	if string(got) != want {
		t.Fatalf("unexpected translated response:\nwant: %s\ngot:  %s", want, got)
	}
}

func TestRegistry_TranslateRequest_AnthropicToGemini(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if string(got) != `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}` {
		t.Fatalf("unexpected translated request: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_GeminiToAnthropic(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	translatedReq := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	rawResp := []byte(`{"candidates":[{"content":{"parts":[{"text":"world"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"modelVersion":"gemini-2.5-pro"}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.Anthropic, "gemini-2.5-pro", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	want := `{"id":"msg-proxy","type":"message","role":"assistant","content":[{"type":"text","text":"world"}],"model":"gemini-2.5-pro","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`
	if string(got) != want {
		t.Fatalf("unexpected translated response:\nwant: %s\ngot:  %s", want, got)
	}
}

func TestRegistry_TranslateRequest_CodexToGemini(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if string(got) != `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}` {
		t.Fatalf("unexpected translated request: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_GeminiToCodex(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gemini-2.5-pro","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	translatedReq := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	rawResp := []byte(`{"candidates":[{"content":{"parts":[{"text":"world"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"modelVersion":"gemini-2.5-pro"}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.Codex, "gemini-2.5-pro", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	want := `{"id":"resp-proxy","object":"response","status":"completed","model":"gemini-2.5-pro","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`
	if string(got) != want {
		t.Fatalf("unexpected translated response:\nwant: %s\ngot:  %s", want, got)
	}
}

func TestRegistry_TranslateResponseStream_GeminiToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	translatedReq := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)

	chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.OpenAI, "gemini-2.5-pro", rawReq, translatedReq, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n"), nil)
	if err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("unexpected translated stream chunk: %#v", chunks)
	}
	gotChunk := string(chunks[0])
	if !strings.HasPrefix(gotChunk, "data: {") || !strings.Contains(gotChunk, `"object":"chat.completion.chunk"`) || !strings.Contains(gotChunk, `"content":"hello"`) {
		t.Fatalf("unexpected translated stream chunk: %#v", chunks)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.OpenAI, "gemini-2.5-pro", rawReq, translatedReq, []byte("data: [DONE]\n\n"), nil)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	if len(done) != 1 || string(done[0]) != "data: [DONE]\n\n" {
		t.Fatalf("unexpected done chunk: %#v", done)
	}
}

func TestRegistry_TranslateResponseStream_GeminiToAnthropic(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"stream":true}`)
	translatedReq := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)

	var state any
	chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Anthropic, "gemini-2.5-pro", rawReq, translatedReq, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected anthropic stream bootstrap chunks, got %#v", chunks)
	}
	joined := string(bytes.Join(chunks, nil))
	if !strings.Contains(joined, "event: message_start") || !strings.Contains(joined, "event: content_block_delta") || !strings.Contains(joined, "\"text\":\"hello\"") {
		t.Fatalf("unexpected anthropic stream chunks: %#v", chunks)
	}
}

func TestRegistry_TranslateResponseStream_GeminiToCodex(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gemini-2.5-pro","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`)
	translatedReq := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)

	var state any
	chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Codex, "gemini-2.5-pro", rawReq, translatedReq, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"totalTokenCount\":8},\"modelVersion\":\"gemini-2.5-pro\"}\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("unexpected codex stream chunks: %#v", chunks)
	}
	if !strings.Contains(string(chunks[0]), "event: response.output_text.delta") || !strings.Contains(string(chunks[0]), `"delta":"hello"`) {
		t.Fatalf("unexpected codex stream chunk: %#v", chunks)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Codex, "gemini-2.5-pro", rawReq, translatedReq, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("unexpected codex done chunks: %#v", done)
	}
	gotDone := string(done[0])
	if !strings.Contains(gotDone, "event: response.completed") || !strings.Contains(gotDone, `"status":"completed"`) || !strings.Contains(gotDone, `"model":"gemini-2.5-pro"`) || !strings.Contains(gotDone, `"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}`) {
		t.Fatalf("unexpected codex done chunk: %#v", done)
	}
}

func TestRegistry_TranslateRequest_OpenAIToAnthropic(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"system","content":"be careful"},{"role":"user","content":"hello"}]}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Anthropic, "claude-3-5-sonnet", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"system":[{"type":"text","text":"be careful"}]`) {
		t.Fatalf("expected anthropic system field, got %s", got)
	}
	if !strings.Contains(string(got), `"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]`) {
		t.Fatalf("unexpected translated request: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_AnthropicToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`)
	translatedReq := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	rawResp := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"world"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "claude-3-5-sonnet", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	if !strings.Contains(string(got), `"object":"chat.completion"`) || !strings.Contains(string(got), `"content":"world"`) {
		t.Fatalf("unexpected translated response: %s", got)
	}
}

func TestRegistry_TranslateRequest_CodexToAnthropic(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"claude-3-5-sonnet","instructions":"be careful","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.Anthropic, "claude-3-5-sonnet", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"system":[{"type":"text","text":"be careful"}]`) {
		t.Fatalf("expected anthropic system field, got %s", got)
	}
	if !strings.Contains(string(got), `"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]`) {
		t.Fatalf("unexpected translated request: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_AnthropicToCodex(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"claude-3-5-sonnet","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	translatedReq := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	rawResp := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"world"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.Codex, "claude-3-5-sonnet", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	if !strings.Contains(string(got), `"object":"response"`) || !strings.Contains(string(got), `"text":"world"`) {
		t.Fatalf("unexpected translated response: %s", got)
	}
}

func TestRegistry_TranslateRequest_OpenAIToGemini_RejectsUnsupportedStructuredContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	_, err := reg.TranslateRequest(protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err == nil {
		t.Fatal("expected unsupported structured content error")
	}
}

func TestRegistry_TranslateRequest_AnthropicToGemini_RejectsUnsupportedStructuredContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}]}`)
	_, err := reg.TranslateRequest(protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err == nil {
		t.Fatal("expected unsupported structured content error")
	}
}

func TestRegistry_TranslateRequest_CodexToGemini_RejectsUnsupportedStructuredContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","input":[{"type":"function_call","name":"tool","arguments":"{}"}]}`)
	_, err := reg.TranslateRequest(protocol.Codex, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err == nil {
		t.Fatal("expected unsupported codex input error")
	}
}
