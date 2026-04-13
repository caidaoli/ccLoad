package protocol_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func TestRegistry_TranslateRequest_GeminiToAnthropic(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"systemInstruction":{"parts":[{"text":"be careful"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"done"}}}]}],"tools":[{"functionDeclarations":[{"name":"lookup","description":"lookup docs","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}}`)
	got, err := reg.TranslateRequest(protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var req struct {
		System   []map[string]any `json:"system"`
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		Tools      []map[string]any `json:"tools"`
		ToolChoice map[string]any   `json:"tool_choice"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal anthropic request: %v", err)
	}
	if len(req.System) != 1 || req.System[0]["text"] != "be careful" {
		t.Fatalf("unexpected anthropic system: %+v", req.System)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("unexpected anthropic messages: %+v", req.Messages)
	}
	if req.Messages[0].Role != "user" || req.Messages[0].Content[0]["text"] != "hello" {
		t.Fatalf("unexpected user message: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "assistant" || req.Messages[1].Content[0]["type"] != "tool_use" || req.Messages[1].Content[0]["id"] != "call_1" || req.Messages[1].Content[0]["name"] != "lookup" {
		t.Fatalf("unexpected tool use message: %+v", req.Messages[1])
	}
	if req.Messages[2].Role != "user" || req.Messages[2].Content[0]["type"] != "tool_result" || req.Messages[2].Content[0]["tool_use_id"] != "call_1" || req.Messages[2].Content[0]["content"] != "done" {
		t.Fatalf("unexpected tool result message: %+v", req.Messages[2])
	}
	if len(req.Tools) != 1 || req.Tools[0]["name"] != "lookup" || req.Tools[0]["description"] != "lookup docs" {
		t.Fatalf("unexpected anthropic tools: %+v", req.Tools)
	}
	if req.ToolChoice["type"] != "tool" || req.ToolChoice["name"] != "lookup" {
		t.Fatalf("unexpected anthropic tool choice: %+v", req.ToolChoice)
	}
}

func TestRegistry_TranslateResponseNonStream_AnthropicToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"query":"go"}}],"model":"claude-3-5-sonnet","stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`)
	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", nil, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	var resp struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Role  string `json:"role"`
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		ModelVersion  string `json:"modelVersion"`
		ResponseID    string `json:"responseId"`
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
	if resp.Candidates[0].Content.Role != "model" || resp.Candidates[0].Content.Parts[0].Text != "hello" {
		t.Fatalf("unexpected text part: %+v", resp.Candidates[0].Content)
	}
	if resp.Candidates[0].Content.Parts[1].FunctionCall.Name != "lookup" || resp.Candidates[0].Content.Parts[1].FunctionCall.Args["query"] != "go" {
		t.Fatalf("unexpected function call part: %+v", resp.Candidates[0].Content.Parts[1])
	}
	if resp.Candidates[0].FinishReason != "STOP" || resp.ModelVersion != "gemini-2.5-pro" || resp.ResponseID != "msg_1" {
		t.Fatalf("unexpected gemini metadata: %+v", resp)
	}
	if resp.UsageMetadata.PromptTokenCount != 3 || resp.UsageMetadata.CandidatesTokenCount != 5 || resp.UsageMetadata.TotalTokenCount != 8 {
		t.Fatalf("unexpected gemini usage: %+v", resp.UsageMetadata)
	}
}

func TestRegistry_TranslateResponseStream_AnthropicToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	start := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"lookup\"}}\n\n")
	if _, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", nil, nil, start, &state); err != nil {
		t.Fatalf("content_block_start failed: %v", err)
	}
	deltaJSON := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n")
	if _, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", nil, nil, deltaJSON, &state); err != nil {
		t.Fatalf("content_block_delta failed: %v", err)
	}
	toolCall, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
	if err != nil {
		t.Fatalf("content_block_stop failed: %v", err)
	}
	if len(toolCall) != 1 || !strings.Contains(string(toolCall[0]), `"functionCall":{"name":"lookup","args":{"query":"go"}}`) {
		t.Fatalf("unexpected gemini tool call chunk: %#v", toolCall)
	}

	usage, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}\n\n"), &state)
	if err != nil {
		t.Fatalf("message_delta failed: %v", err)
	}
	if len(usage) != 1 || !strings.Contains(string(usage[0]), `"promptTokenCount":3`) || !strings.Contains(string(usage[0]), `"candidatesTokenCount":5`) || !strings.Contains(string(usage[0]), `"finishReason":"STOP"`) {
		t.Fatalf("unexpected gemini usage chunk: %#v", usage)
	}
}
