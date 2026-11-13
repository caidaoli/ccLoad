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

/**
 * @brief Gemini 工具签名经 Claude 响应和工具结果回放后仍属于同一调用
 * @param t 测试上下文
 */
func TestRegistryGeminiClaudeToolSignatureRoundTrip(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	const signature = "EggKBgEMOdbHNA=="
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"checking"},{"functionCall":{"id":"call_1","name":"lookup","args":{"query":"go"}},"thoughtSignature":"` + signature + `"}]},"finishReason":"STOP"}]}`)
	out, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.Anthropic, "gemini-2.5-pro", nil, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	toolID := gjson.GetBytes(out, `content.#(type=="tool_use").id`).String()
	request := fmt.Appendf(nil, `{"messages":[{"role":"assistant","content":%s},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}]}`, gjson.GetBytes(out, "content").Raw, toolID)
	replayed, err := reg.TranslateRequest(protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", request, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, part := range gjson.GetBytes(replayed, "contents.0.parts").Array() {
		if part.Get("functionCall").Exists() {
			found = true
			if part.Get("functionCall.id").String() != toolID || part.Get("thoughtSignature").String() != signature {
				t.Fatalf("tool signature or identity lost: %s", replayed)
			}
		} else if part.Get("thoughtSignature").Exists() {
			t.Fatalf("tool signature attached to another part: %s", replayed)
		}
	}
	if !found {
		t.Fatalf("replayed tool call missing: %s", replayed)
	}
}

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
	if len(req.System) != 0 {
		t.Fatalf("unexpected anthropic system: %+v", req.System)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("unexpected anthropic messages: %+v", req.Messages)
	}
	if req.Messages[0].Role != "user" || req.Messages[0].Content[0]["text"] != "be careful" {
		t.Fatalf("unexpected user message: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content[0]["text"] != "hello" {
		t.Fatalf("unexpected user message: %+v", req.Messages[1])
	}
	toolUseID, _ := req.Messages[2].Content[0]["id"].(string)
	if req.Messages[2].Role != "assistant" || req.Messages[2].Content[0]["type"] != "tool_use" || toolUseID == "" || req.Messages[2].Content[0]["name"] != "lookup" {
		t.Fatalf("unexpected tool use message: %+v", req.Messages[2])
	}
	if req.Messages[3].Role != "user" || req.Messages[3].Content[0]["type"] != "tool_result" || req.Messages[3].Content[0]["tool_use_id"] != toolUseID || req.Messages[3].Content[0]["content"] != "done" {
		t.Fatalf("unexpected tool result message: %+v", req.Messages[3])
	}
	if len(req.Tools) != 1 || req.Tools[0]["name"] != "lookup" || req.Tools[0]["description"] != "lookup docs" {
		t.Fatalf("unexpected anthropic tools: %+v", req.Tools)
	}
	if req.ToolChoice["type"] != "tool" || req.ToolChoice["name"] != "lookup" {
		t.Fatalf("unexpected anthropic tool choice: %+v", req.ToolChoice)
	}
}

/**
 * @brief 验证 Gemini 混合工具调用和结果在 Claude 请求中分离角色并保留配对
 * @param t 测试上下文
 */
func TestRegistry_TranslateRequest_GeminiToAnthropic_MixedToolResults(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"read files"}]},{"role":"model","parts":[{"functionCall":{"id":"call-a","name":"read","args":{}}},{"functionCall":{"id":"call-b","name":"read","args":{}}},{"functionResponse":{"id":"call-b","name":"read","response":{"result":"two"}}},{"text":"continue"},{"functionResponse":{"id":"call-a","name":"read","response":{"result":"one"}}}]}]}`)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			out, err := reg.TranslateRequest(protocol.Gemini, protocol.Anthropic, "claude-sonnet-4", raw, stream)
			if err != nil {
				t.Fatal(err)
			}
			messages := gjson.GetBytes(out, "messages").Array()
			if len(messages) != 3 || messages[1].Get("role").String() != "assistant" || messages[2].Get("role").String() != "user" {
				t.Fatalf("expected user/calls/results turns: %s", out)
			}
			calls := messages[1].Get("content").Array()
			results := messages[2].Get("content").Array()
			if len(calls) != 2 || len(results) != 3 {
				t.Fatalf("expected two calls, two results and text: %s", out)
			}
			for i, id := range []string{"call-a", "call-b"} {
				if calls[i].Get("type").String() != "tool_use" || calls[i].Get("id").String() != id {
					t.Fatalf("tool call %d changed: %s", i, out)
				}
			}
			for i, id := range []string{"call-b", "call-a"} {
				if results[i].Get("type").String() != "tool_result" || results[i].Get("tool_use_id").String() != id {
					t.Fatalf("tool result %d lost its identity or order: %s", i, out)
				}
			}
			if results[2].Get("text").String() != "continue" {
				t.Fatalf("user text must follow the tool results: %s", out)
			}
		})
	}
}

/**
 * @brief 验证 Claude 原生 JSON 与 SSE 经 Registry 输出的 Gemini 签名可按 bytes 字段解码
 * @param t 测试上下文
 */
func TestRegistry_TranslateResponse_AnthropicToGemini_ThoughtSignatureBytes(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	for _, tc := range []struct {
		name      string
		signature string
		want      string
	}{
		{name: "foreign", signature: "foreign_claude_sig_123"},
		{name: "native unpadded", signature: "gemini#EggKBgEMOdbHNA", want: "EggKBgEMOdbHNA=="},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var payloads [][]byte
				if stream {
					var state any
					for _, event := range []string{
						`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
						`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning"}}`,
						fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":%q}}`, tc.signature),
						`{"type":"content_block_stop","index":0}`,
						`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
					} {
						chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Gemini, "claude-sonnet-4", nil, nil, []byte("data: "+event+"\n\n"), &state)
						if err != nil {
							t.Fatal(err)
						}
						for _, chunk := range chunks {
							for line := range strings.SplitSeq(string(chunk), "\n") {
								if data, ok := strings.CutPrefix(line, "data: "); ok {
									payloads = append(payloads, []byte(data))
								}
							}
						}
					}
				} else {
					raw := []byte(fmt.Sprintf(`{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"reasoning","signature":%q}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`, tc.signature))
					out, err := reg.TranslateResponseNonStream(context.Background(), protocol.Anthropic, protocol.Gemini, "claude-sonnet-4", nil, nil, raw)
					if err != nil {
						t.Fatal(err)
					}
					payloads = append(payloads, out)
				}
				var text, signature, finish string
				for _, payload := range payloads {
					var response struct {
						Candidates []struct {
							Content struct {
								Parts []struct {
									ThoughtSignature []byte `json:"thoughtSignature"`
								} `json:"parts"`
							} `json:"content"`
						} `json:"candidates"`
					}
					if err := json.Unmarshal(payload, &response); err != nil {
						t.Fatalf("Gemini bytes field cannot be decoded: %v; %s", err, payload)
					}
					for _, part := range gjson.GetBytes(payload, "candidates.0.content.parts").Array() {
						text += part.Get("text").String()
						if value := part.Get("thoughtSignature"); value.Exists() {
							signature = value.String()
						}
					}
					if value := gjson.GetBytes(payload, "candidates.0.finishReason"); value.Exists() {
						finish = value.String()
					}
				}
				if text != "reasoning" || signature != tc.want || finish != "STOP" {
					t.Fatalf("text=%q signature=%q finish=%q; want reasoning/%q/STOP", text, signature, finish, tc.want)
				}
			})
		}
	}
}

func TestRegistry_TranslateRequest_Gemini_DefaultsMissingRoleToUser(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"contents":[{"parts":[{"text":"hello"}]},{"parts":[{"text":"another user turn"}]}]}`)
	tests := []struct {
		name     string
		target   protocol.Protocol
		rolePath string
	}{
		{name: "Anthropic", target: protocol.Anthropic, rolePath: "messages.0.role"},
		{name: "OpenAI", target: protocol.OpenAI, rolePath: "messages.0.role"},
		{name: "Codex", target: protocol.Codex, rolePath: "input.0.role"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reg.TranslateRequest(protocol.Gemini, tt.target, "test-model", raw, false)
			if err != nil {
				t.Fatalf("TranslateRequest failed: %v", err)
			}
			if role := gjson.GetBytes(got, tt.rolePath).String(); role != "user" {
				t.Fatalf("missing Gemini role translated to %s role %q, want user: %s", tt.target, role, got)
			}
			for _, message := range gjson.GetBytes(got, strings.Split(tt.rolePath, ".")[0]).Array() {
				if role := message.Get("role").String(); role != "user" {
					t.Fatalf("consecutive missing Gemini roles must remain user turns: %s", got)
				}
			}
		})
	}
}

func TestRegistry_TranslateRequest_GeminiToAnthropic_PreservesThinkingLevel(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"think hard"}]}],
		"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}
	}`)
	got, err := reg.TranslateRequest(protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var req struct {
		Thinking struct {
			Type string `json:"type"`
		} `json:"thinking"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal anthropic request: %v", err)
	}
	if req.Thinking.Type != "adaptive" || req.OutputConfig.Effort != "high" {
		t.Fatalf("expected high thinking config, got %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_GeminiMalformedFunctionCallToAnthropicFails(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":3,"totalTokenCount":3},"modelVersion":"gemini-2.5-pro"}`)
	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Gemini, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, rawResp)
	if err == nil || !strings.Contains(err.Error(), "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("malformed Gemini function call must fail translation, got err=%v body=%s", err, got)
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

func TestRegistry_TranslateResponseStream_AnthropicToGemini_ThinkingBlock(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	// Anthropic SSE: thinking block followed by text block
	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-opus-4-5\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"step 1\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello gemini\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n",
	}

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Gemini, "claude-opus-4-5", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error on chunk %q: %v", chunk, err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}

	result := allOutput.String()
	if !strings.Contains(result, `"thought":true`) || !strings.Contains(result, `"text":"step 1"`) {
		t.Fatalf("expected Anthropic thinking as Gemini thought content, got:\n%s", result)
	}
	// 文本块应正常输出
	if !strings.Contains(result, `"hello gemini"`) {
		t.Fatalf("expected text content 'hello gemini', got:\n%s", result)
	}
	// 必须有 finishReason=STOP
	if !strings.Contains(result, `"finishReason":"STOP"`) {
		t.Fatalf("expected finishReason=STOP, got:\n%s", result)
	}
}

/**
 * @brief 验证不兼容的隐藏思考签名不泄漏到 Gemini bytes 字段且正文和终态保留
 * @param t 测试上下文
 */
func TestRegistry_TranslateResponseStream_AnthropicToGemini_RedactedThinking(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"model\":\"claude-opus-4-5\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"opaque\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
	}

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Gemini, "claude-opus-4-5", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error on chunk %q: %v", chunk, err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}

	var text, finish string
	for line := range strings.SplitSeq(allOutput.String(), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			for _, part := range gjson.Get(data, "candidates.0.content.parts").Array() {
				if part.Get("thoughtSignature").Exists() {
					t.Fatalf("foreign redacted signature leaked: %s", data)
				}
				text += part.Get("text").String()
			}
			if value := gjson.Get(data, "candidates.0.finishReason"); value.Exists() {
				finish = value.String()
			}
		}
	}
	if text != "answer" || finish != "STOP" {
		t.Fatalf("text=%q finish=%q, want answer/STOP", text, finish)
	}
}

/**
 * @brief 验证不兼容签名增量被省略且思考文本和流终态保留
 * @param t 测试上下文
 */
func TestRegistry_TranslateResponseStream_AnthropicToGemini_SignatureDelta(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	// signature_delta 紧跟 thinking_delta，流不应挂起
	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_3\",\"model\":\"claude-opus-4-5\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"reasoning\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"abc123\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
	}

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Gemini, "claude-opus-4-5", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error on chunk %q: %v", chunk, err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}

	var text, finish string
	for line := range strings.SplitSeq(allOutput.String(), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			for _, part := range gjson.Get(data, "candidates.0.content.parts").Array() {
				if part.Get("thoughtSignature").Exists() {
					t.Fatalf("incompatible signature leaked: %s", data)
				}
				text += part.Get("text").String()
			}
			if value := gjson.Get(data, "candidates.0.finishReason"); value.Exists() {
				finish = value.String()
			}
		}
	}
	if text != "reasoning" || finish != "STOP" {
		t.Fatalf("text=%q finish=%q, want reasoning/STOP", text, finish)
	}
}

func TestRegistry_TranslateResponseStream_AnthropicToGemini_UsesMessageStartInputTokens(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_4\",\"model\":\"claude-opus-4-5\",\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
	}

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("stream error on chunk %q: %v", chunk, err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}

	result := allOutput.String()
	if !strings.Contains(result, `"promptTokenCount":11`) {
		t.Fatalf("expected promptTokenCount from message_start usage, got:\n%s", result)
	}
	if !strings.Contains(result, `"candidatesTokenCount":5`) {
		t.Fatalf("expected candidatesTokenCount from message_delta usage, got:\n%s", result)
	}
}
