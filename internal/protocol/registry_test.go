package protocol_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	var request struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(got, &request); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if len(request.Contents) != 1 || request.Contents[0].Role != "user" || len(request.Contents[0].Parts) != 1 || request.Contents[0].Parts[0].Text != "hello" {
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

	var response struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(got, &response); err != nil {
		t.Fatalf("unmarshal translated response: %v", err)
	}
	if response.ID == "" || response.Object != "chat.completion" || response.Model != "gemini-2.5-pro" || len(response.Choices) != 1 || response.Choices[0].Message.Role != "assistant" || response.Choices[0].Message.Content != "world" || response.Choices[0].FinishReason != "stop" || response.Usage.PromptTokens != 3 || response.Usage.CompletionTokens != 5 || response.Usage.TotalTokens != 8 {
		t.Fatalf("unexpected translated response: %s", got)
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
	var request struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(got, &request); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if len(request.Contents) != 1 || request.Contents[0].Role != "user" || len(request.Contents[0].Parts) != 1 || request.Contents[0].Parts[0].Text != "hello" {
		t.Fatalf("unexpected translated request: %s", got)
	}
}

func TestRegistry_TranslateRequest_AnthropicToGemini3_UsesThinkingLevel(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":[{"type":"text","text":"think hard"}]}],
		"thinking":{"type":"adaptive","display":"summarized"},
		"output_config":{"effort":"max"}
	}`)
	got, err := reg.TranslateRequest(protocol.Anthropic, protocol.Gemini, "gemini-3.5-flash", raw, true)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("unmarshal translated request failed: %v", err)
	}
	generationConfig, ok := body["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected generationConfig, got: %s", got)
	}
	thinkingConfig, ok := generationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinkingConfig, got: %s", got)
	}
	if thinkingConfig["thinkingLevel"] != "high" {
		t.Fatalf("thinkingLevel=%v, want high; body=%s", thinkingConfig["thinkingLevel"], got)
	}
	if _, ok := thinkingConfig["thinkingBudget"]; ok {
		t.Fatalf("Gemini 3 thinkingConfig must not include thinkingBudget: %s", got)
	}
	if thinkingConfig["includeThoughts"] != true {
		t.Fatalf("expected includeThoughts=true, body=%s", got)
	}
}

func TestRegistry_TranslateRequest_OpenAIToAnthropic_MapsXHighToClaudeMax(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"think hard"}],"reasoning_effort":"xhigh"}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Anthropic, "claude-sonnet-4-6", raw, true)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("unmarshal translated request failed: %v", err)
	}
	outputConfig, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("expected output_config, got: %s", got)
	}
	if outputConfig["effort"] != "max" {
		t.Fatalf("output_config.effort=%v, want max; body=%s", outputConfig["effort"], got)
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
	if !strings.Contains(string(got), `"type":"message"`) || !strings.Contains(string(got), `"role":"assistant"`) || !strings.Contains(string(got), `"type":"text"`) || !strings.Contains(string(got), `"text":"world"`) || !strings.Contains(string(got), `"model":"gemini-2.5-pro"`) || !strings.Contains(string(got), `"stop_reason":"end_turn"`) {
		t.Fatalf("unexpected translated response: %s", got)
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
	var request struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(got, &request); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if len(request.Contents) != 1 || request.Contents[0].Role != "user" || len(request.Contents[0].Parts) != 1 || request.Contents[0].Parts[0].Text != "hello" {
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
	if !strings.Contains(string(got), `"object":"response"`) || !strings.Contains(string(got), `"status":"completed"`) || !strings.Contains(string(got), `"model":"gemini-2.5-pro"`) || !strings.Contains(string(got), `"type":"message"`) || !strings.Contains(string(got), `"role":"assistant"`) || !strings.Contains(string(got), `"type":"output_text"`) || !strings.Contains(string(got), `"text":"world"`) || !strings.Contains(string(got), `"input_tokens":3`) || !strings.Contains(string(got), `"output_tokens":5`) || !strings.Contains(string(got), `"total_tokens":8`) {
		t.Fatalf("unexpected translated response: %s", got)
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

func TestRegistry_TranslateResponseStream_NilStatePointerSupported(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	testCases := []struct {
		name    string
		from    protocol.Protocol
		to      protocol.Protocol
		model   string
		payload []byte
		want    string
	}{
		{
			name:    "gemini to anthropic",
			from:    protocol.Gemini,
			to:      protocol.Anthropic,
			model:   "gemini-2.5-pro",
			payload: []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n"),
			want:    "event: message_start",
		},
		{
			name:    "gemini to codex",
			from:    protocol.Gemini,
			to:      protocol.Codex,
			model:   "gemini-2.5-pro",
			payload: []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n"),
			want:    "event: response.output_text.delta",
		},
		{
			name:    "openai to codex",
			from:    protocol.OpenAI,
			to:      protocol.Codex,
			model:   "gpt-5-codex",
			payload: []byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5-codex\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"),
			want:    "event: response.output_text.delta",
		},
		{
			name:    "codex to openai",
			from:    protocol.Codex,
			to:      protocol.OpenAI,
			model:   "gpt-4o",
			payload: []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"),
			want:    "\"chat.completion.chunk\"",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, err := reg.TranslateResponseStream(context.Background(), tc.from, tc.to, tc.model, nil, nil, tc.payload, nil)
			if err != nil {
				t.Fatalf("TranslateResponseStream failed: %v", err)
			}
			if len(chunks) == 0 {
				t.Fatalf("expected translated stream chunks, got %#v", chunks)
			}
			if joined := string(bytes.Join(chunks, nil)); !strings.Contains(joined, tc.want) {
				t.Fatalf("unexpected translated stream chunks: %s", joined)
			}
		})
	}
}

func TestRegistry_TranslateResponseStream_GeminiToAnthropic(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"stream":true}`)
	translatedReq := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)

	var state any
	chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Anthropic, "gemini-2.5-pro", rawReq, translatedReq, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"thoughtsTokenCount\":2,\"totalTokenCount\":10}}\n\n"), &state)
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

	done, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Anthropic, "gemini-2.5-pro", rawReq, translatedReq, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	doneJoined := string(bytes.Join(done, nil))
	blockStopAt := strings.Index(doneJoined, "event: content_block_stop")
	messageDeltaAt := strings.Index(doneJoined, "event: message_delta")
	messageStopAt := strings.Index(doneJoined, "event: message_stop")
	if blockStopAt < 0 || messageDeltaAt <= blockStopAt || messageStopAt <= messageDeltaAt {
		t.Fatalf("unexpected Anthropic terminal event order: %s", doneJoined)
	}
	usage := mustMap(t, mustSSEEventData(t, doneJoined, "message_delta")["usage"])
	if mustInt(t, usage["input_tokens"]) != 3 || mustInt(t, usage["output_tokens"]) != 7 {
		t.Fatalf("unexpected Anthropic terminal usage: %#v", usage)
	}
}

func TestRegistry_TranslateResponseStream_IgnoresSSEComments(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)
	tests := []struct {
		name string
		from protocol.Protocol
		to   protocol.Protocol
	}{
		{name: "generic", from: protocol.Gemini, to: protocol.OpenAI},
		{name: "gemini to anthropic", from: protocol.Gemini, to: protocol.Anthropic},
		{name: "openai to gemini", from: protocol.OpenAI, to: protocol.Gemini},
		{name: "openai to anthropic", from: protocol.OpenAI, to: protocol.Anthropic},
		{name: "openai to codex", from: protocol.OpenAI, to: protocol.Codex},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			chunks, err := reg.TranslateResponseStream(context.Background(), tc.from, tc.to, "client-model", nil, nil, []byte(": ping\n\n"), &state)
			if err != nil {
				t.Fatalf("TranslateResponseStream rejected SSE comment: %v", err)
			}
			if len(chunks) != 0 || state != nil {
				t.Fatalf("SSE comment changed stream state: chunks=%#v state=%T", chunks, state)
			}
		})
	}
}

func TestRegistry_TranslateResponseStream_GeminiToAnthropic_DoneAfterFinishedChunkEmitsNothing(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro\",\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"totalTokenCount\":8}}\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream finished chunk failed: %v", err)
	}
	joined := string(bytes.Join(chunks, nil))
	if !strings.Contains(joined, "event: message_stop") {
		t.Fatalf("expected finished chunk to emit message_stop, got %#v", chunks)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	if done != nil {
		t.Fatalf("expected DONE sentinel to emit nothing after finished chunk, got %#v", done)
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
	events := parseSSEEvents(t, string(bytes.Join(chunks, nil)))
	var textDelta map[string]any
	for _, event := range events {
		if event.Event == "response.output_text.delta" {
			textDelta = event.Data
		}
	}
	if textDelta == nil || textDelta["type"] != "response.output_text.delta" || textDelta["delta"] != "hello" {
		t.Fatalf("unexpected codex stream chunk: %#v", chunks)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Codex, "gemini-2.5-pro", rawReq, translatedReq, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	var completed map[string]any
	for _, event := range parseSSEEvents(t, string(bytes.Join(done, nil))) {
		if event.Event == "response.completed" {
			completed = event.Data
		}
	}
	if completed == nil {
		t.Fatalf("missing codex completion event: %#v", done)
	}
	response := mustMap(t, completed["response"])
	usage := mustMap(t, response["usage"])
	if completed["type"] != "response.completed" || response["status"] != "completed" || response["model"] != "gemini-2.5-pro" || mustInt(t, usage["input_tokens"]) != 3 || mustInt(t, usage["output_tokens"]) != 5 || mustInt(t, usage["total_tokens"]) != 8 {
		t.Fatalf("unexpected codex done payload: %+v", completed)
	}
}

func TestRegistry_TranslateResponseStream_GeminiToCodex_PreservesResponseID(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	if _, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Codex, "gemini-2.5-pro", nil, nil, []byte("data: {\"responseId\":\"resp_1\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"totalTokenCount\":8},\"modelVersion\":\"gemini-2.5-pro\"}\n\n"), &state); err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.Gemini, protocol.Codex, "gemini-2.5-pro", nil, nil, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	var completed map[string]any
	for _, event := range parseSSEEvents(t, string(bytes.Join(done, nil))) {
		if event.Event == "response.completed" {
			completed = event.Data
		}
	}
	if completed == nil {
		t.Fatalf("missing codex completion event: %#v", done)
	}
	if response := mustMap(t, completed["response"]); response["id"] != "resp_1" {
		t.Fatalf("expected response id resp_1, got %+v", completed)
	}
}

func TestRegistry_TranslateResponseStream_CodexToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	translatedReq := []byte(`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`)

	var state any
	chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", rawReq, translatedReq, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}
	if len(chunks) != 1 || !strings.Contains(string(chunks[0]), `"chat.completion.chunk"`) || !strings.Contains(string(chunks[0]), `"content":"hello"`) {
		t.Fatalf("unexpected openai stream chunk: %#v", chunks)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", rawReq, translatedReq, []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-4o\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	if len(done) != 2 || !strings.Contains(string(done[0]), `"finish_reason":"stop"`) || !strings.Contains(string(done[0]), `"prompt_tokens":3`) || string(done[1]) != "data: [DONE]\n\n" {
		t.Fatalf("unexpected done chunks: %#v", done)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIToCodex(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`)
	translatedReq := []byte(`{"model":"gpt-5-codex","messages":[{"role":"user","content":"hello"}],"stream":true}`)

	var state any
	chunks, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5-codex", rawReq, translatedReq, []byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5-codex\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}
	var textDelta map[string]any
	for _, event := range parseSSEEvents(t, string(bytes.Join(chunks, nil))) {
		if event.Event == "response.output_text.delta" {
			textDelta = event.Data
		}
	}
	if textDelta == nil || textDelta["delta"] != "hello" {
		t.Fatalf("unexpected codex stream chunk: %#v", chunks)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5-codex", rawReq, translatedReq, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	var completed map[string]any
	for _, event := range parseSSEEvents(t, string(bytes.Join(done, nil))) {
		if event.Event == "response.completed" {
			completed = event.Data
		}
	}
	if completed == nil || completed["type"] != "response.completed" {
		t.Fatalf("unexpected codex done chunk: %#v", done)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIToCodex_EventHeaderAndResponsesEvents(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		"event: \n" +
			"data: {\"id\":\"chatcmpl-ws-ingress\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\\u200b\"}}]}\n\n",
		"event: response.output_text.delta\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n",
		"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n",
	}

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5.5", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}

	result := allOutput.String()
	if !strings.Contains(result, "event: response.output_text.delta") || !strings.Contains(result, `"delta":"hello"`) {
		t.Fatalf("expected responses text delta passthrough in Codex output, got:\n%s", result)
	}
	if !strings.Contains(result, "event: response.completed") || !strings.Contains(result, `"input_tokens":3`) {
		t.Fatalf("expected responses completion passthrough in Codex output, got:\n%s", result)
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
	if !strings.Contains(string(got), `"system":[{`) || !strings.Contains(string(got), `"text":"be careful"`) {
		t.Fatalf("expected anthropic system field, got %s", got)
	}
	if !strings.Contains(string(got), `"role":"user"`) || !strings.Contains(string(got), `"text":"hello"`) {
		t.Fatalf("unexpected translated request: %s", got)
	}
	if !strings.Contains(string(got), `"stream":false`) {
		t.Fatalf("expected anthropic stream=false to preserve non-stream request, got %s", got)
	}
}

func TestRegistry_TranslateRequest_OpenAIToAnthropic_PreservesTextOnlyAssistantContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"mimo-v2.5","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"previous answer"},{"role":"user","content":"next"}]}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Anthropic, "mimo-v2.5", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages length = %d, want 3; body=%s", len(req.Messages), got)
	}
	if req.Messages[1].Role != "assistant" {
		t.Fatalf("message[1].role = %q, want assistant", req.Messages[1].Role)
	}
	if content := protocolTestContentText(req.Messages[1].Content); content != "previous answer" {
		t.Fatalf("assistant content = %#v, want previous answer; body=%s", req.Messages[1].Content, got)
	}
}

func TestRegistry_TranslateRequest_OpenAIToAnthropic_SystemOnly(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"system","content":"optimize this code"}]}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Anthropic, "claude-3-5-sonnet", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}

	var req struct {
		System   []map[string]any `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if len(req.System) != 0 {
		t.Fatalf("expected no anthropic system field for system-only prompt, got %+v", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("unexpected anthropic messages: %+v", req.Messages)
	}
	content, ok := req.Messages[0].Content.([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("expected one anthropic content block, got %+v", req.Messages[0].Content)
	}
	block, ok := content[0].(map[string]any)
	if !ok || block["type"] != "text" || block["text"] != "optimize this code" {
		t.Fatalf("unexpected anthropic content block: %+v", content[0])
	}
}

func TestRegistry_TranslateRequest_OpenAIToGemini_SystemOnly(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"system","content":"optimize this code"}]}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}

	var req struct {
		SystemInstruction struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if len(req.SystemInstruction.Parts) != 0 {
		t.Fatalf("expected no gemini system instruction for system-only prompt, got %+v", req.SystemInstruction)
	}
	if len(req.Contents) != 1 || req.Contents[0].Role != "user" || len(req.Contents[0].Parts) != 1 || req.Contents[0].Parts[0].Text != "optimize this code" {
		t.Fatalf("expected user prompt content, got %+v", req.Contents)
	}
}

func TestRegistry_TranslateRequest_OpenAIToCodex_SystemOnly(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gpt-5-codex","messages":[{"role":"system","content":"optimize this code"}]}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Codex, "gpt-5-codex", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if req["instructions"] != "optimize this code" {
		t.Fatalf("unexpected codex instructions: %+v", req)
	}
	if _, ok := req["input"]; ok {
		t.Fatalf("expected codex request without input items, got %+v", req)
	}
}

func TestRegistry_TranslateRequest_SystemOnlySemantics_OtherSources(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	const prompt = "optimize this code"

	assertAnthropicUserPrompt := func(t *testing.T, body []byte) {
		t.Helper()
		var req struct {
			System   []map[string]any `json:"system"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal anthropic request: %v", err)
		}
		if len(req.System) != 0 {
			t.Fatalf("expected no anthropic system field, got %+v", req.System)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" || protocolTestContentText(req.Messages[0].Content) != prompt {
			t.Fatalf("unexpected anthropic request: %+v", req)
		}
	}

	assertGeminiUserPrompt := func(t *testing.T, body []byte) {
		t.Helper()
		var req struct {
			SystemInstruction *struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"systemInstruction"`
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal gemini request: %v", err)
		}
		if req.SystemInstruction != nil && len(req.SystemInstruction.Parts) > 0 {
			t.Fatalf("expected no gemini system instruction, got %+v", req.SystemInstruction)
		}
		if len(req.Contents) != 1 || req.Contents[0].Role != "user" || len(req.Contents[0].Parts) != 1 || req.Contents[0].Parts[0].Text != prompt {
			t.Fatalf("unexpected gemini request: %+v", req)
		}
	}

	assertOpenAISystemPrompt := func(t *testing.T, body []byte) {
		t.Helper()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal openai request: %v", err)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "system" || protocolTestContentText(req.Messages[0].Content) != prompt {
			t.Fatalf("unexpected openai request: %+v", req)
		}
	}

	assertCodexInstructionsOnly := func(t *testing.T, body []byte) {
		t.Helper()
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal codex request: %v", err)
		}
		if req["instructions"] != prompt {
			t.Fatalf("unexpected codex instructions: %+v", req)
		}
		if _, ok := req["input"]; ok {
			t.Fatalf("expected codex request without input, got %+v", req)
		}
	}

	tests := []struct {
		name   string
		from   protocol.Protocol
		to     protocol.Protocol
		model  string
		raw    []byte
		assert func(*testing.T, []byte)
	}{
		{
			name:   "anthropic_to_openai",
			from:   protocol.Anthropic,
			to:     protocol.OpenAI,
			model:  "gpt-4o",
			raw:    []byte(`{"model":"gpt-4o","system":[{"type":"text","text":"optimize this code"}],"messages":[]}`),
			assert: assertOpenAISystemPrompt,
		},
		{
			name:   "anthropic_to_gemini",
			from:   protocol.Anthropic,
			to:     protocol.Gemini,
			model:  "gemini-2.5-pro",
			raw:    []byte(`{"model":"gemini-2.5-pro","system":[{"type":"text","text":"optimize this code"}],"messages":[]}`),
			assert: assertGeminiUserPrompt,
		},
		{
			name:   "anthropic_to_codex",
			from:   protocol.Anthropic,
			to:     protocol.Codex,
			model:  "gpt-5-codex",
			raw:    []byte(`{"model":"gpt-5-codex","system":[{"type":"text","text":"optimize this code"}],"messages":[]}`),
			assert: assertCodexInstructionsOnly,
		},
		{
			name:   "codex_to_openai",
			from:   protocol.Codex,
			to:     protocol.OpenAI,
			model:  "gpt-4o",
			raw:    []byte(`{"model":"gpt-4o","instructions":"optimize this code"}`),
			assert: assertOpenAISystemPrompt,
		},
		{
			name:   "codex_to_gemini",
			from:   protocol.Codex,
			to:     protocol.Gemini,
			model:  "gemini-2.5-pro",
			raw:    []byte(`{"model":"gemini-2.5-pro","instructions":"optimize this code"}`),
			assert: assertGeminiUserPrompt,
		},
		{
			name:   "codex_to_anthropic",
			from:   protocol.Codex,
			to:     protocol.Anthropic,
			model:  "claude-3-5-sonnet",
			raw:    []byte(`{"model":"claude-3-5-sonnet","instructions":"optimize this code"}`),
			assert: assertAnthropicUserPrompt,
		},
		{
			name:   "gemini_to_openai",
			from:   protocol.Gemini,
			to:     protocol.OpenAI,
			model:  "gpt-4o",
			raw:    []byte(`{"systemInstruction":{"parts":[{"text":"optimize this code"}]}}`),
			assert: assertOpenAISystemPrompt,
		},
		{
			name:   "gemini_to_anthropic",
			from:   protocol.Gemini,
			to:     protocol.Anthropic,
			model:  "claude-3-5-sonnet",
			raw:    []byte(`{"systemInstruction":{"parts":[{"text":"optimize this code"}]}}`),
			assert: assertAnthropicUserPrompt,
		},
		{
			name:   "gemini_to_codex",
			from:   protocol.Gemini,
			to:     protocol.Codex,
			model:  "gpt-5-codex",
			raw:    []byte(`{"systemInstruction":{"parts":[{"text":"optimize this code"}]}}`),
			assert: assertCodexInstructionsOnly,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reg.TranslateRequest(tt.from, tt.to, tt.model, tt.raw, false)
			if err != nil {
				t.Fatalf("TranslateRequest failed: %v", err)
			}
			tt.assert(t, got)
		})
	}
}

func TestRegistry_TranslateRequest_OpenAIToAnthropic_StringStreamAccepted(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}],"stream":"true","tools":[{"type":"function","function":{"name":"get_current_weather","description":"Get the current weather in a given location","parameters":{"type":"object","properties":{"location":{"type":"string"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location"]}}}],"tool_choice":"auto"}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Anthropic, "claude-3-5-sonnet", raw, true)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"stream":true`) {
		t.Fatalf("expected anthropic stream=true, got %s", got)
	}
	if !strings.Contains(string(got), `"name":"get_current_weather"`) || !strings.Contains(string(got), `"type":"auto"`) {
		t.Fatalf("unexpected translated tool payload: %s", got)
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

func TestRegistry_TranslateResponseNonStream_AnthropicSSEToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"mimo-v2.5","messages":[{"role":"user","content":"hello"}]}`)
	translatedReq := []byte(`{"model":"mimo-v2.5","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"stream":false}`)
	rawResp := []byte(
		"event: message_start\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"mimo-v2.5\",\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":2,\"cache_creation_input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"think\"}}\n\n" +
			"event: content_block_stop\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: content_block_start\n" +
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
			"event: message_delta\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
			"event: message_stop\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "mimo-v2.5", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	body := string(got)
	if strings.Contains(body, "event:") {
		t.Fatalf("expected OpenAI JSON, got raw SSE: %s", body)
	}
	if !strings.Contains(body, `"object":"chat.completion"`) || !strings.Contains(body, `"content":"hello"`) {
		t.Fatalf("unexpected translated response: %s", got)
	}
	if !strings.Contains(body, `"reasoning_content":"think"`) {
		t.Fatalf("expected reasoning_content from thinking SSE, got %s", got)
	}
	if !strings.Contains(body, `"prompt_tokens":6`) || !strings.Contains(body, `"completion_tokens":5`) {
		t.Fatalf("unexpected usage payload: %s", got)
	}
}

func TestRegistry_TranslateRequest_AnthropicToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gpt-4o","system":[{"type":"text","text":"be careful"}],"tools":[{"name":"search","description":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"search"},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":{"query":"go"}}]},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png","media_type":"image/png"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"},"title":"doc.pdf"},{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}],"stream":true}`)
	got, err := reg.TranslateRequest(protocol.Anthropic, protocol.OpenAI, "gpt-4o", raw, true)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"role":"system"`) || !strings.Contains(string(got), `"be careful"`) {
		t.Fatalf("expected openai system message, got %s", got)
	}
	payload := mustJSONMap(t, got)
	var assistant map[string]any
	for _, rawMessage := range mustSlice(t, payload["messages"]) {
		message := mustMap(t, rawMessage)
		if message["role"] == "assistant" {
			assistant = message
			break
		}
	}
	if assistant == nil {
		t.Fatalf("expected assistant message, got %s", got)
	}
	toolCalls := mustSlice(t, assistant["tool_calls"])
	if len(toolCalls) != 1 {
		t.Fatalf("expected one assistant tool call, got %s", got)
	}
	toolCall := mustMap(t, toolCalls[0])
	function := mustMap(t, toolCall["function"])
	if toolCall["id"] != "toolu_1" || toolCall["type"] != "function" || function["name"] != "search" || function["arguments"] != `{"query":"go"}` {
		t.Fatalf("unexpected assistant tool call: %s", got)
	}
	if !strings.Contains(string(got), `"type":"image_url"`) || !strings.Contains(string(got), `"type":"file"`) || !strings.Contains(string(got), `"role":"tool"`) {
		t.Fatalf("unexpected translated openai request: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_OpenAIToAnthropic(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	translatedReq := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	rawResp := []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":0,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"text","text":"world"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"query\":\"go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "gpt-4o", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	if !strings.Contains(string(got), `"type":"message"`) || !strings.Contains(string(got), `"type":"tool_use"`) || !strings.Contains(string(got), `"stop_reason":"tool_use"`) {
		t.Fatalf("unexpected translated response: %s", got)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIToAnthropic(t *testing.T) {
	rawReq := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"stream":true}`)
	translatedReq := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":true}`)

	assertAnthropicStreamTextTranslation(
		t,
		protocol.OpenAI,
		"gpt-4o",
		rawReq,
		translatedReq,
		"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n",
		"data: [DONE]\n\n",
		"hello",
	)
}

func TestRegistry_TranslateResponseStream_OpenAIToAnthropic_EventHeaderAndResponsesEvents(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		"event: \n" +
			"data: {\"id\":\"chatcmpl-ws-ingress\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\\u200b\"}}]}\n\n",
		"event: response.output_text.delta\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n",
		"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n",
	}

	result := translateResponseStreamChunks(t, reg, protocol.OpenAI, protocol.Anthropic, "gpt-5.5", chunks...)
	if !strings.Contains(result, `"text":"hello"`) {
		t.Fatalf("expected responses text delta in Anthropic output, got:\n%s", result)
	}
	if !strings.Contains(result, "event: message_stop") || !strings.Contains(result, `"input_tokens":3`) {
		t.Fatalf("expected responses completion and usage in Anthropic output, got:\n%s", result)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIToAnthropic_DoneAfterFinishedChunkEmitsNothing(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	chunks, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream finished chunk failed: %v", err)
	}
	joined := string(bytes.Join(chunks, nil))
	if !strings.Contains(joined, "event: message_stop") {
		t.Fatalf("expected finished chunk to emit message_stop, got %#v", chunks)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	if done != nil {
		t.Fatalf("expected DONE sentinel to emit nothing after finished chunk, got %#v", done)
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
	var req struct {
		System   []map[string]any `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if len(req.System) != 1 || req.System[0]["text"] != "be careful" {
		t.Fatalf("expected anthropic system field, got %+v", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || protocolTestContentText(req.Messages[0].Content) != "hello" {
		t.Fatalf("unexpected translated request: %+v", req)
	}
}

func TestRegistry_TranslateRequest_CodexBareMessageToAnthropic(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"claude-3-5-sonnet","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.Anthropic, "claude-3-5-sonnet", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || protocolTestContentText(req.Messages[0].Content) != "hello" {
		t.Fatalf("unexpected translated request: %+v", req)
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

func TestRegistry_TranslateRequest_AnthropicToCodex(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gpt-5-codex","system":[{"type":"text","text":"be careful"}],"tools":[{"name":"search","description":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"search"},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":{"query":"go"}}]},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png","media_type":"image/png"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"},"title":"doc.pdf"},{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}],"stream":true}`)
	got, err := reg.TranslateRequest(protocol.Anthropic, protocol.Codex, "gpt-5-codex", raw, true)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"instructions":"be careful"`) {
		t.Fatalf("expected codex instructions, got %s", got)
	}
	if !strings.Contains(string(got), `"type":"function_call"`) || !strings.Contains(string(got), `"type":"function_call_output"`) {
		t.Fatalf("expected codex tool items, got %s", got)
	}
	if !strings.Contains(string(got), `"type":"input_image"`) || !strings.Contains(string(got), `"type":"input_file"`) {
		t.Fatalf("unexpected translated codex request: %s", got)
	}
}

func TestRegistry_TranslateRequest_AnthropicToCodex_StringToolArguments(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gpt-5-codex","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_skill_1","name":"Skill","input":{"skill":"superpowers:using-superpowers","args":""}}]}]}`)
	got, err := reg.TranslateRequest(protocol.Anthropic, protocol.Codex, "gpt-5-codex", raw, true)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}

	var req struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal codex request: %v", err)
	}
	if len(req.Input) != 1 {
		t.Fatalf("unexpected codex input: %+v", req.Input)
	}
	if req.Input[0]["type"] != "function_call" || req.Input[0]["call_id"] != "call_skill_1" || req.Input[0]["name"] != "Skill" {
		t.Fatalf("unexpected codex function_call: %+v", req.Input[0])
	}
	if req.Input[0]["arguments"] != `{"skill":"superpowers:using-superpowers","args":""}` && req.Input[0]["arguments"] != `{"args":"","skill":"superpowers:using-superpowers"}` {
		t.Fatalf("expected codex string arguments, got %+v", req.Input[0]["arguments"])
	}
}

func TestRegistry_TranslateResponseNonStream_CodexToAnthropic(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-5-codex","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	translatedReq := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	rawResp := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]},{"type":"function_call","call_id":"call_1","name":"search","arguments":{"query":"go"}}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.Anthropic, "gpt-5-codex", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	if !strings.Contains(string(got), `"type":"message"`) || !strings.Contains(string(got), `"type":"tool_use"`) || !strings.Contains(string(got), `"stop_reason":"tool_use"`) {
		t.Fatalf("unexpected translated response: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_CodexToAnthropic_MapsReasoningTokensToThinkingTokens(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"type":"response.completed","response":{"id":"resp_xai","model":"grok-4.5","usage":{"input_tokens":17273,"input_tokens_details":{"cached_tokens":3456},"output_tokens":2391,"output_tokens_details":{"reasoning_tokens":1119},"total_tokens":19664},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.Anthropic, "grok-4.5", []byte(`{"messages":[]}`), nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	var payload struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
			ThinkingTokens       int `json:"thinking_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal translated response: %v", err)
	}
	if payload.Usage.InputTokens != 17273-3456 || payload.Usage.OutputTokens != 2391 || payload.Usage.CacheReadInputTokens != 3456 || payload.Usage.ThinkingTokens != 1119 {
		t.Fatalf("unexpected translated usage: %+v; body=%s", payload.Usage, got)
	}
}

func TestRegistry_TranslateResponseNonStream_CodexToAnthropic_StringArguments(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-5-codex","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	translatedReq := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	rawResp := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"function_call","call_id":"call_skill_1","name":"Skill","arguments":"{\"args\":\"skill: \\\"superpowers:using-superpowers\\\"\",\"skill\":\"superpowers:using-superpowers\"}"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.Anthropic, "gpt-5-codex", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal anthropic payload: %v", err)
	}
	content := payload["content"].([]any)
	toolUse := content[0].(map[string]any)
	input := toolUse["input"].(map[string]any)
	if toolUse["type"] != "tool_use" || input["args"] != `skill: "superpowers:using-superpowers"` || input["skill"] != "superpowers:using-superpowers" {
		t.Fatalf("expected anthropic tool_use input object, got %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_CodexToOpenAI_StringArguments(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"function_call","call_id":"call_skill_1","name":"Skill","arguments":"{\"args\":\"skill: \\\"superpowers:using-superpowers\\\"\",\"skill\":\"superpowers:using-superpowers\"}"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", nil, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	payload := mustJSONMap(t, got)
	choices := mustSlice(t, payload["choices"])
	choice := mustMap(t, choices[0])
	message := mustMap(t, choice["message"])
	toolCalls := mustSlice(t, message["tool_calls"])
	toolCall := mustMap(t, toolCalls[0])
	function := mustMap(t, toolCall["function"])

	if mustString(t, toolCall["id"]) != "call_skill_1" ||
		mustString(t, toolCall["type"]) != "function" ||
		mustString(t, function["name"]) != "Skill" ||
		mustString(t, function["arguments"]) != `{"args":"skill: \"superpowers:using-superpowers\"","skill":"superpowers:using-superpowers"}` ||
		mustString(t, choice["finish_reason"]) != "tool_calls" {
		t.Fatalf("expected openai tool arguments raw json string, got %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_CodexToGemini_StringArguments(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"function_call","call_id":"call_skill_1","name":"Skill","arguments":"{\"args\":\"skill: \\\"superpowers:using-superpowers\\\"\",\"skill\":\"superpowers:using-superpowers\"}"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal gemini payload: %v", err)
	}
	candidates := payload["candidates"].([]any)
	content := candidates[0].(map[string]any)["content"].(map[string]any)
	parts := content["parts"].([]any)
	functionCall := parts[0].(map[string]any)["functionCall"].(map[string]any)
	args := functionCall["args"].(map[string]any)
	if functionCall["name"] != "Skill" || args["args"] != `skill: "superpowers:using-superpowers"` || args["skill"] != "superpowers:using-superpowers" {
		t.Fatalf("expected gemini functionCall args object, got %s", got)
	}
}

func TestRegistry_TranslateResponseStream_CodexToAnthropic(t *testing.T) {
	rawReq := []byte(`{"model":"gpt-5-codex","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"stream":true}`)
	translatedReq := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`)

	assertAnthropicStreamTextTranslation(
		t,
		protocol.Codex,
		"gpt-5-codex",
		rawReq,
		translatedReq,
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5-codex\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n",
		"hello",
	)
}

func TestRegistry_TranslateRequest_OpenAIToCodex(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gpt-5-codex","messages":[{"role":"system","content":"be careful"},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"high"}}]}]}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Codex, "gpt-5-codex", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"instructions":"be careful"`) {
		t.Fatalf("expected codex instructions, got %s", got)
	}
	if !strings.Contains(string(got), `"type":"input_image"`) || !strings.Contains(string(got), `"image_url":"https://example.com/a.png"`) {
		t.Fatalf("unexpected translated codex request: %s", got)
	}
}

func TestRegistry_TranslateRequest_OpenAIToCodex_BuiltinWebSearch(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gpt-5-codex","tools":[{"type":"web_search","search_context_size":"high"}],"tool_choice":{"type":"web_search"},"messages":[{"role":"user","content":"hello"}]}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Codex, "gpt-5-codex", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var req struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice map[string]any   `json:"tool_choice"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0]["type"] != "web_search" || req.Tools[0]["search_context_size"] != "high" {
		t.Fatalf("unexpected builtin tools: %+v", req.Tools)
	}
	if req.ToolChoice["type"] != "web_search" {
		t.Fatalf("unexpected builtin tool choice: %+v", req.ToolChoice)
	}
}

func TestRegistry_TranslateResponseNonStream_CodexToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	translatedReq := []byte(`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	rawResp := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-4o","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	if !strings.Contains(string(got), `"object":"chat.completion"`) || !strings.Contains(string(got), `"content":"world"`) {
		t.Fatalf("unexpected translated response: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_CodexToOpenAI_ReasoningAndUsageDetails(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-4o","output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"step by step"}],"encrypted_content":"enc_1"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]}],"usage":{"input_tokens":21,"input_tokens_details":{"cached_tokens":7},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":13},"cache_creation_input_tokens":11,"total_tokens":26}}`)
	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", nil, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	body := string(got)
	if !strings.Contains(body, `"reasoning_content":"step by step"`) || !strings.Contains(body, `"encrypted_content":"enc_1"`) {
		t.Fatalf("unexpected reasoning translation: %s", got)
	}
	if !strings.Contains(body, `"cached_tokens":7`) || !strings.Contains(body, `"cache_creation_input_tokens":11`) || !strings.Contains(body, `"reasoning_tokens":13`) {
		t.Fatalf("unexpected usage translation: %s", got)
	}
}

func TestRegistry_TranslateRequest_CodexToOpenAI(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gpt-4o","instructions":"be careful","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_file","file_id":"file_123","filename":"doc.pdf"}]},{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.OpenAI, "gpt-4o", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"role":"system"`) || !strings.Contains(string(got), `"be careful"`) {
		t.Fatalf("expected system message, got %s", got)
	}
	if !strings.Contains(string(got), `"type":"file"`) || !strings.Contains(string(got), `"role":"tool"`) {
		t.Fatalf("unexpected translated openai request: %s", got)
	}
}

func TestRegistry_TranslateRequest_CodexToOpenAI_MapsBuiltinWebSearchToOptions(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gpt-4o","tools":[{"type":"web_search","search_context_size":"high","user_location":{"type":"approximate","country":"US"}}],"tool_choice":{"type":"web_search"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.OpenAI, "gpt-4o", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	searchOptions, ok := req["web_search_options"].(map[string]any)
	if !ok {
		t.Fatalf("web_search_options missing or invalid: %s", got)
	}
	if gotSize := searchOptions["search_context_size"]; gotSize != "high" {
		t.Fatalf("search_context_size = %#v, want high; body=%s", gotSize, got)
	}
	location, ok := searchOptions["user_location"].(map[string]any)
	if !ok || location["type"] != "approximate" {
		t.Fatalf("user_location missing or invalid: %#v; body=%s", searchOptions["user_location"], got)
	}
	approximate, ok := location["approximate"].(map[string]any)
	if !ok || approximate["country"] != "US" {
		t.Fatalf("user_location.approximate missing or invalid: %#v; body=%s", location["approximate"], got)
	}
	if _, ok := req["tools"]; ok {
		t.Fatalf("OpenAI chat search must use web_search_options, not tools: %s", got)
	}
	if _, ok := req["tool_choice"]; ok {
		t.Fatalf("OpenAI chat search must not reuse web_search tool_choice: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_OpenAIToCodex(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	translatedReq := []byte(`{"model":"gpt-5-codex","messages":[{"role":"user","content":"hello"}]}`)
	rawResp := []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":0,"model":"gpt-5-codex","choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5-codex", rawReq, translatedReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	if !strings.Contains(string(got), `"object":"response"`) || !strings.Contains(string(got), `"type":"output_text"`) || !strings.Contains(string(got), `"text":"world"`) {
		t.Fatalf("unexpected translated response: %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_OpenAIToCodex_ReasoningAndUsageDetails(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":0,"model":"gpt-5-codex","choices":[{"index":0,"message":{"role":"assistant","content":"world","reasoning_content":"step by step","reasoning":[{"type":"reasoning","text":"step by step","encrypted_content":"enc_1"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"prompt_tokens_details":{"cached_tokens":7},"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":13},"cache_creation_input_tokens":11,"total_tokens":26}}`)
	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5-codex", nil, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	body := string(got)
	if !strings.Contains(body, `"type":"reasoning"`) || !strings.Contains(body, `"type":"reasoning_text"`) || !strings.Contains(body, `"text":"step by step"`) || !strings.Contains(body, `"encrypted_content":"enc_1"`) {
		t.Fatalf("unexpected reasoning translation: %s", got)
	}
	if !strings.Contains(body, `"cached_tokens":7`) || !strings.Contains(body, `"cache_creation_input_tokens":11`) || !strings.Contains(body, `"reasoning_tokens":13`) {
		t.Fatalf("unexpected usage translation: %s", got)
	}
}

func TestRegistry_TranslateRequest_OpenAIToGemini_SupportsStructuredContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],"tool_choice":"required","messages":[{"role":"assistant","content":[{"type":"text","text":"calling"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"query\":\"go\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"done"},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	got, err := reg.TranslateRequest(protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"functionDeclarations"`) || !strings.Contains(string(got), `"functionCall"`) || !strings.Contains(string(got), `"functionResponse"`) || !strings.Contains(string(got), `"fileUri":"https://example.com/a.png"`) {
		t.Fatalf("unexpected translated gemini request: %s", got)
	}
}

func TestRegistry_TranslateRequest_AnthropicToGemini_SupportsStructuredContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"query":"go"}}]},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png","media_type":"image/png"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"},"title":"doc.pdf"},{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"functionCall"`) || !strings.Contains(string(got), `"functionResponse"`) || !strings.Contains(string(got), `"inlineData"`) || !strings.Contains(string(got), `"fileUri":"https://example.com/a.png"`) {
		t.Fatalf("unexpected translated gemini request: %s", got)
	}
}

func TestRegistry_TranslateRequest_CodexToGemini_SupportsStructuredContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"https://example.com/a.png"},{"type":"input_file","file_id":"file_123","filename":"doc.pdf"}]},{"type":"function_call","call_id":"call_1","name":"tool","arguments":{"q":"go"}},{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if !strings.Contains(string(got), `"functionCall"`) || !strings.Contains(string(got), `"functionResponse"`) || !strings.Contains(string(got), `"fileUri":"https://example.com/a.png"`) || !strings.Contains(string(got), `"fileUri":"file_123"`) {
		t.Fatalf("unexpected translated gemini request: %s", got)
	}
}

func TestRegistry_TranslateRequest_OpenAIToGemini_RejectsUnknownStructuredContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":[{"type":"mystery","value":true}]}]}`)
	_, err := reg.TranslateRequest(protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err == nil {
		t.Fatal("expected unsupported structured content error")
	}
}

func TestRegistry_TranslateRequest_AnthropicToGemini_RejectsUnknownStructuredContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":[{"type":"mystery","value":true}]}]}`)
	_, err := reg.TranslateRequest(protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err == nil {
		t.Fatal("expected unsupported structured content error")
	}
}

func TestRegistry_TranslateRequest_CodexToGemini_RejectsUnknownStructuredContent(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","input":[{"type":"message","role":"user","content":[{"type":"mystery","value":true}]}]}`)
	_, err := reg.TranslateRequest(protocol.Codex, protocol.Gemini, "gemini-2.5-pro", raw, false)
	if err == nil {
		t.Fatal("expected unsupported codex input error")
	}
}

func TestBuildTransformPlan_SupportedTransformDefaults(t *testing.T) {
	plan, err := protocol.BuildTransformPlan(
		protocol.OpenAI,
		protocol.Gemini,
		"/v1/chat/completions",
		"",
		[]byte(`{"model":"alias-model"}`),
		nil,
		"alias-model",
		"",
		true,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if !plan.NeedsTransform {
		t.Fatal("expected transform plan to require protocol translation")
	}
	if got := string(plan.TranslatedBody); got != `{"model":"alias-model"}` {
		t.Fatalf("expected prepared body to default to original body, got %s", got)
	}
	if got := plan.UpstreamPath; got != "/v1/chat/completions" {
		t.Fatalf("expected upstream path to default to original path, got %s", got)
	}
	if got := plan.RequestModel(); got != "alias-model" {
		t.Fatalf("expected request model alias-model, got %s", got)
	}
	if plan.RequestFamily != protocol.RequestFamilyChatCompletions {
		t.Fatalf("expected chat_completions family, got %s", plan.RequestFamily)
	}
}

func TestBuildTransformPlan_SupportsAnthropicToOpenAI(t *testing.T) {
	plan, err := protocol.BuildTransformPlan(
		protocol.Anthropic,
		protocol.OpenAI,
		"/v1/messages",
		"",
		[]byte(`{"model":"gpt-4o"}`),
		nil,
		"gpt-4o",
		"",
		false,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if !plan.NeedsTransform || plan.RequestFamily != protocol.RequestFamilyMessages {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestBuildTransformPlan_SupportsAnthropicToCodex(t *testing.T) {
	plan, err := protocol.BuildTransformPlan(
		protocol.Anthropic,
		protocol.Codex,
		"/v1/messages",
		"",
		[]byte(`{"model":"gpt-5-codex"}`),
		nil,
		"gpt-5-codex",
		"",
		true,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if !plan.NeedsTransform || plan.RequestFamily != protocol.RequestFamilyMessages {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestBuildTransformPlan_RejectsUnsupportedTransform(t *testing.T) {
	_, err := protocol.BuildTransformPlan(
		protocol.Gemini,
		protocol.OpenAI,
		"/v1/messages",
		"/v1/messages",
		[]byte(`{"model":"gpt-4o"}`),
		[]byte(`{"model":"gpt-4o"}`),
		"gpt-4o",
		"gpt-4o",
		false,
	)
	if err == nil {
		t.Fatal("expected unsupported transform error")
	}
}

func TestDetectRequestFamily_AlphaSearch(t *testing.T) {
	t.Parallel()

	if got := protocol.DetectRequestFamily("/v1/alpha/search"); got != protocol.RequestFamilyAlphaSearch {
		t.Fatalf("DetectRequestFamily(/v1/alpha/search) = %q, want %q", got, protocol.RequestFamilyAlphaSearch)
	}
	if got := protocol.DetectRequestFamily("/prefix/v1/alpha/search"); got != protocol.RequestFamilyAlphaSearch {
		t.Fatalf("DetectRequestFamily with prefix = %q, want %q", got, protocol.RequestFamilyAlphaSearch)
	}
	if got := protocol.DetectRequestFamily("/v1/alpha/search/extra"); got != protocol.RequestFamilyAlphaSearch {
		t.Fatalf("DetectRequestFamily with subpath = %q, want %q", got, protocol.RequestFamilyAlphaSearch)
	}
	// 子串误匹配：alpha 前缀下的无关路径不得命中
	if got := protocol.DetectRequestFamily("/v1/alpha/searching"); got != protocol.RequestFamilyUnknown {
		t.Fatalf("DetectRequestFamily(/v1/alpha/searching) = %q, want unknown", got)
	}
}

func TestBuildTransformPlan_CodexAlphaSearchPassthroughOnly(t *testing.T) {
	t.Parallel()

	plan, err := protocol.BuildTransformPlan(
		protocol.Codex,
		protocol.Codex,
		"/v1/alpha/search",
		"/v1/alpha/search",
		[]byte(`{"query":"codegraph"}`),
		[]byte(`{"query":"codegraph"}`),
		"",
		"",
		false,
	)
	if err != nil {
		t.Fatalf("same-protocol alpha/search plan failed: %v", err)
	}
	if plan.NeedsTransform {
		t.Fatalf("expected alpha/search same-protocol passthrough, got %+v", plan)
	}
	if plan.RequestFamily != protocol.RequestFamilyAlphaSearch {
		t.Fatalf("RequestFamily = %q, want %q", plan.RequestFamily, protocol.RequestFamilyAlphaSearch)
	}

	// alpha/search 不是协议互转 family：跨协议应拒绝（只能原生 Codex 透传）
	_, err = protocol.BuildTransformPlan(
		protocol.Codex,
		protocol.OpenAI,
		"/v1/alpha/search",
		"/v1/chat/completions",
		[]byte(`{"query":"codegraph"}`),
		[]byte(`{"query":"codegraph"}`),
		"",
		"",
		false,
	)
	if err == nil {
		t.Fatal("expected unsupported protocol transform for alpha/search cross-protocol")
	}
}

func TestBuildTransformPlan_SameProtocolNoOp(t *testing.T) {
	t.Parallel()

	plan, err := protocol.BuildTransformPlan(
		protocol.Gemini,
		protocol.Gemini,
		"/v1beta/models/gemini-2.5-pro:generateContent",
		"",
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
		nil,
		"gemini-2.5-pro",
		"",
		true,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if plan.NeedsTransform {
		t.Fatalf("expected same-protocol plan to skip translation, got %+v", plan)
	}
	if plan.RequestFamily != protocol.RequestFamilyGenerateContent {
		t.Fatalf("expected generate_content family, got %s", plan.RequestFamily)
	}
	if got := string(plan.TranslatedBody); got != `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}` {
		t.Fatalf("expected translated body to reuse original body, got %s", got)
	}
}

func TestBuildTransformPlan_SupportsOpenAIToCodex(t *testing.T) {
	plan, err := protocol.BuildTransformPlan(
		protocol.OpenAI,
		protocol.Codex,
		"/v1/chat/completions",
		"",
		[]byte(`{"model":"gpt-5-codex"}`),
		nil,
		"gpt-5-codex",
		"",
		false,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if !plan.NeedsTransform || plan.RequestFamily != protocol.RequestFamilyChatCompletions {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestBuildTransformPlan_SupportsCodexToOpenAI(t *testing.T) {
	plan, err := protocol.BuildTransformPlan(
		protocol.Codex,
		protocol.OpenAI,
		"/v1/responses",
		"",
		[]byte(`{"model":"gpt-4o"}`),
		nil,
		"gpt-4o",
		"",
		false,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if !plan.NeedsTransform || plan.RequestFamily != protocol.RequestFamilyResponses {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestBuildTransformPlan_SupportsCodexToAnthropicWithBasePathPrefix(t *testing.T) {
	plan, err := protocol.BuildTransformPlan(
		protocol.Codex,
		protocol.Anthropic,
		"/anthropic/v1/responses",
		"/anthropic/v1/messages",
		[]byte(`{"model":"claude-3-5-sonnet"}`),
		nil,
		"claude-3-5-sonnet",
		"",
		false,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if !plan.NeedsTransform || plan.RequestFamily != protocol.RequestFamilyResponses {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestBuildTransformPlan_RejectsUnsupportedFamilyForSupportedPair(t *testing.T) {
	_, err := protocol.BuildTransformPlan(
		protocol.OpenAI,
		protocol.Codex,
		"/v1/embeddings",
		"",
		[]byte(`{"model":"gpt-5-codex"}`),
		nil,
		"gpt-5-codex",
		"",
		false,
	)
	if err == nil {
		t.Fatal("expected unsupported transform error")
	}
}

func TestTransformPlan_ResponseModelPreservesClientAlias(t *testing.T) {
	plan, err := protocol.BuildTransformPlan(
		protocol.OpenAI,
		protocol.Gemini,
		"/v1/chat/completions",
		"/v1beta/models/gemini-2.5-pro:generateContent",
		[]byte(`{"model":"alias-model"}`),
		[]byte(`{"model":"gemini-2.5-pro"}`),
		"alias-model",
		"gemini-2.5-pro",
		false,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if got := plan.RequestModel(); got != "gemini-2.5-pro" {
		t.Fatalf("expected request model gemini-2.5-pro, got %s", got)
	}
	if got := plan.ResponseModel(); got != "alias-model" {
		t.Fatalf("expected response model alias-model, got %s", got)
	}
}

func TestRegistry_SameProtocolNoOp(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	gotReq, err := reg.TranslateRequest(protocol.OpenAI, protocol.OpenAI, "gpt-4o", rawReq, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	if string(gotReq) != string(rawReq) {
		t.Fatalf("expected same request body, got %s", gotReq)
	}

	rawResp := []byte(`{"ok":true}`)
	gotResp, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.OpenAI, "gpt-4o", rawReq, rawReq, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	if string(gotResp) != string(rawResp) {
		t.Fatalf("expected same response body, got %s", gotResp)
	}

	gotStream, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.OpenAI, "gpt-4o", rawReq, rawReq, []byte("data: [DONE]\n\n"), nil)
	if err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}
	if len(gotStream) != 1 || string(gotStream[0]) != "data: [DONE]\n\n" {
		t.Fatalf("unexpected no-op stream chunks: %#v", gotStream)
	}
}

func TestRegistry_TranslateRequest_OpenAIToAnthropic_ToolCalls(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	// OpenAI assistant 消息含 tool_calls，后跟 tool role 消息
	req := `{
		"model": "claude-3-5-sonnet",
		"messages": [
			{"role": "user", "content": "what is weather in Beijing?"},
			{"role": "assistant", "content": null, "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Beijing\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "sunny, 25C"}
		]
	}`

	out, err := reg.TranslateRequest(protocol.OpenAI, protocol.Anthropic, "claude-3-5-sonnet", []byte(req), false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	result := string(out)
	// assistant tool_calls → Anthropic tool_use block
	if !strings.Contains(result, `"type":"tool_use"`) {
		t.Fatalf("expected type=tool_use in anthropic request, got:\n%s", result)
	}
	if !strings.Contains(result, `"name":"get_weather"`) {
		t.Fatalf("expected tool name get_weather, got:\n%s", result)
	}
	// tool role → Anthropic tool_result block
	if !strings.Contains(result, `"type":"tool_result"`) {
		t.Fatalf("expected type=tool_result in anthropic request, got:\n%s", result)
	}
	if !strings.Contains(result, `"tool_use_id":"call_1"`) {
		t.Fatalf("expected tool_use_id=call_1, got:\n%s", result)
	}
}

func TestRegistry_TranslateRequest_AnthropicToOpenAI_ToolCalls(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	// Anthropic 请求含 tool_use block + tool_result block
	req := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "search for cats"}]},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "tu_1", "name": "search", "input": {"query": "cats"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "tu_1", "content": "many cats found"}]}
		]
	}`

	out, err := reg.TranslateRequest(protocol.Anthropic, protocol.OpenAI, "gpt-4o", []byte(req), false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	result := string(out)
	// tool_use → OpenAI tool_calls
	if !strings.Contains(result, `"tool_calls"`) {
		t.Fatalf("expected tool_calls in openai request, got:\n%s", result)
	}
	if !strings.Contains(result, `"name":"search"`) {
		t.Fatalf("expected tool name search, got:\n%s", result)
	}
	// tool_result → OpenAI role=tool
	if !strings.Contains(result, `"role":"tool"`) {
		t.Fatalf("expected role=tool, got:\n%s", result)
	}
	if !strings.Contains(result, `"tool_call_id":"tu_1"`) {
		t.Fatalf("expected tool_call_id=tu_1, got:\n%s", result)
	}
}

func TestRegistry_TranslateResponseNonStream_OpenAIToAnthropic_ToolCalls(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	resp := `{
		"id": "chatcmpl-tc1",
		"object": "chat.completion",
		"model": "gpt-4o",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{"id": "call_2", "type": "function", "function": {"name": "lookup", "arguments": "{\"key\":\"val\"}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`

	out, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte(resp))
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	result := string(out)
	if !strings.Contains(result, `"type":"tool_use"`) {
		t.Fatalf("expected type=tool_use, got:\n%s", result)
	}
	if !strings.Contains(result, `"name":"lookup"`) {
		t.Fatalf("expected name=lookup, got:\n%s", result)
	}
	if !strings.Contains(result, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason=tool_use, got:\n%s", result)
	}
	if !strings.Contains(result, `"key"`) || !strings.Contains(result, `"val"`) {
		t.Fatalf("expected input args key=val in tool_use block, got:\n%s", result)
	}
}

func TestRegistry_TranslateResponseNonStream_AnthropicToOpenAI_ToolCalls(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	resp := `{
		"id": "msg_tc1",
		"type": "message",
		"role": "assistant",
		"model": "claude-3-5-sonnet",
		"content": [
			{"type": "tool_use", "id": "tu_2", "name": "calculate", "input": {"expr": "1+1"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 8, "output_tokens": 4}
	}`

	out, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.OpenAI, "gpt-4o", nil, nil, []byte(resp))
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	result := string(out)
	if !strings.Contains(result, `"tool_calls"`) {
		t.Fatalf("expected tool_calls, got:\n%s", result)
	}
	if !strings.Contains(result, `"name":"calculate"`) {
		t.Fatalf("expected name=calculate, got:\n%s", result)
	}
	if !strings.Contains(result, `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected finish_reason=tool_calls, got:\n%s", result)
	}
	if !strings.Contains(result, `1+1`) {
		t.Fatalf("expected expr=1+1 in function arguments, got:\n%s", result)
	}
}

func TestSupportedClientProtocolsForUpstream_BidirectionalMatrix(t *testing.T) {
	tests := []struct {
		upstream protocol.Protocol
		want     []protocol.Protocol
	}{
		{upstream: protocol.OpenAI, want: []protocol.Protocol{protocol.Anthropic, protocol.Codex, protocol.Gemini}},
		{upstream: protocol.Anthropic, want: []protocol.Protocol{protocol.Codex, protocol.Gemini, protocol.OpenAI}},
		{upstream: protocol.Codex, want: []protocol.Protocol{protocol.Anthropic, protocol.Gemini, protocol.OpenAI}},
		{upstream: protocol.Gemini, want: []protocol.Protocol{protocol.Anthropic, protocol.Codex, protocol.OpenAI}},
	}

	for _, tt := range tests {
		got := protocol.SupportedClientProtocolsForUpstream(tt.upstream)
		if len(got) != len(tt.want) {
			t.Fatalf("upstream %s: expected %v, got %v", tt.upstream, tt.want, got)
		}
		for i, want := range tt.want {
			if got[i] != want {
				t.Fatalf("upstream %s: expected %v, got %v", tt.upstream, tt.want, got)
			}
		}
	}
}
