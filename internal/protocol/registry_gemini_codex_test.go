package protocol_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func TestRegistry_TranslateRequest_GeminiToCodex(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"systemInstruction":{"parts":[{"text":"be careful"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"done"}}}]}],"tools":[{"functionDeclarations":[{"name":"lookup","description":"lookup docs","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}}`)
	got, err := reg.TranslateRequest(protocol.Gemini, protocol.Codex, "gpt-5-codex", raw, true)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var req struct {
		Instructions string           `json:"instructions"`
		Stream       bool             `json:"stream"`
		Input        []map[string]any `json:"input"`
		Tools        []map[string]any `json:"tools"`
		ToolChoice   map[string]any   `json:"tool_choice"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal codex request: %v", err)
	}
	if req.Instructions != "be careful" || !req.Stream {
		t.Fatalf("unexpected codex request metadata: %+v", req)
	}
	if len(req.Input) != 3 {
		t.Fatalf("unexpected codex input: %+v", req.Input)
	}
	if req.Input[0]["type"] != "message" || req.Input[0]["role"] != "user" {
		t.Fatalf("unexpected codex message input: %+v", req.Input[0])
	}
	content, ok := req.Input[0]["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("unexpected codex message content: %+v", req.Input[0]["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["type"] != "input_text" || part["text"] != "hello" {
		t.Fatalf("unexpected codex message part: %+v", content[0])
	}
	if req.Input[1]["type"] != "function_call" || req.Input[1]["call_id"] != "call_1" || req.Input[1]["name"] != "lookup" {
		t.Fatalf("unexpected codex function_call: %+v", req.Input[1])
	}
	args, ok := req.Input[1]["arguments"].(map[string]any)
	if !ok || args["query"] != "go" {
		t.Fatalf("unexpected codex function_call args: %+v", req.Input[1]["arguments"])
	}
	if req.Input[2]["type"] != "function_call_output" || req.Input[2]["call_id"] != "call_1" || req.Input[2]["name"] != "lookup" || req.Input[2]["output"] != "done" {
		t.Fatalf("unexpected codex function_call_output: %+v", req.Input[2])
	}
	if len(req.Tools) != 1 || req.Tools[0]["type"] != "function" || req.Tools[0]["name"] != "lookup" || req.Tools[0]["description"] != "lookup docs" {
		t.Fatalf("unexpected codex tools: %+v", req.Tools)
	}
	if req.ToolChoice["type"] != "function" || req.ToolChoice["name"] != "lookup" {
		t.Fatalf("unexpected codex tool choice: %+v", req.ToolChoice)
	}
}

func TestRegistry_TranslateResponseNonStream_CodexToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":{"query":"go"}}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`)
	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		ResponseID    string `json:"responseId"`
		ModelVersion  string `json:"modelVersion"`
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			TotalTokenCount      int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatalf("unmarshal gemini response: %v", err)
	}
	if len(resp.Candidates) != 1 || len(resp.Candidates[0].Content.Parts) != 2 {
		t.Fatalf("unexpected candidates: %+v", resp)
	}
	if resp.Candidates[0].Content.Parts[0].Text != "hello" {
		t.Fatalf("unexpected text part: %+v", resp.Candidates[0].Content.Parts[0])
	}
	if resp.Candidates[0].Content.Parts[1].FunctionCall.Name != "lookup" || resp.Candidates[0].Content.Parts[1].FunctionCall.Args["query"] != "go" {
		t.Fatalf("unexpected function call part: %+v", resp.Candidates[0].Content.Parts[1])
	}
	if resp.Candidates[0].FinishReason != "STOP" || resp.ResponseID != "resp_1" || resp.ModelVersion != "gemini-2.5-pro" {
		t.Fatalf("unexpected gemini metadata: %+v", resp)
	}
	if resp.UsageMetadata.PromptTokenCount != 3 || resp.UsageMetadata.CandidatesTokenCount != 5 || resp.UsageMetadata.TotalTokenCount != 8 {
		t.Fatalf("unexpected gemini usage: %+v", resp.UsageMetadata)
	}
}

func TestRegistry_TranslateResponseStream_CodexToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	textChunk, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"), &state)
	if err != nil {
		t.Fatalf("response.output_text.delta failed: %v", err)
	}
	if len(textChunk) != 1 || !strings.Contains(string(textChunk[0]), `"text":"hello"`) {
		t.Fatalf("unexpected gemini text chunk: %#v", textChunk)
	}

	toolChunk, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":{\"query\":\"go\"}}}\n\n"), &state)
	if err != nil {
		t.Fatalf("response.output_item.done failed: %v", err)
	}
	if len(toolChunk) != 1 || !strings.Contains(string(toolChunk[0]), `"functionCall":{"name":"lookup","args":{"query":"go"}}`) {
		t.Fatalf("unexpected gemini tool chunk: %#v", toolChunk)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"), &state)
	if err != nil {
		t.Fatalf("response.completed failed: %v", err)
	}
	if len(done) != 1 || !strings.Contains(string(done[0]), `"promptTokenCount":3`) || !strings.Contains(string(done[0]), `"candidatesTokenCount":5`) || !strings.Contains(string(done[0]), `"finishReason":"STOP"`) {
		t.Fatalf("unexpected gemini done chunk: %#v", done)
	}
}
