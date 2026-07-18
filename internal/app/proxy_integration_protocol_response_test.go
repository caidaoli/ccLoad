package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/model"
)

func TestProxy_StructuredGeminiResponseToAnthropicTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:        "gemini-ch",
		channelType: "gemini",
		models:      "gemini-2.5-pro",
		apiKey:      "sk-gem",
	}}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader([]byte(
				`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"},{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"modelVersion":"gemini-2.5-pro"}`,
			))),
		}, nil
	})}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"anthropic"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "hi"}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected transformed Gemini path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"type":"tool_use"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"name":"lookup"`)) {
		t.Fatalf("expected anthropic tool_use response, got %s", w.Body.String())
	}
}

func TestProxy_StructuredGeminiResponseToCodexTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:        "gemini-ch",
		channelType: "gemini",
		models:      "gemini-2.5-pro",
		apiKey:      "sk-gem",
	}}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader([]byte(
				`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"},{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"modelVersion":"gemini-2.5-pro"}`,
			))),
		}, nil
	})}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"codex"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gemini-2.5-pro",
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected transformed Gemini path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"type":"function_call"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"name":"lookup"`)) {
		t.Fatalf("expected codex function_call response, got %s", w.Body.String())
	}
}

func TestProxy_StructuredAnthropicResponseToOpenAITransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:        "anthropic-ch",
		channelType: "anthropic",
		models:      "claude-3-5-sonnet",
		apiKey:      "sk-ant",
	}}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader([]byte(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"query":"go"}}],"model":"claude-3-5-sonnet","stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`,
			))),
		}, nil
	})}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"openai"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	assertChatRequestUserText(t, gotBody, "hi")
	if !bytes.Contains(w.Body.Bytes(), []byte(`"tool_calls"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"name":"lookup"`)) {
		t.Fatalf("expected OpenAI tool_calls response, got %s", w.Body.String())
	}
}

func TestProxy_StructuredAnthropicResponseToCodexTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:        "anthropic-ch",
		channelType: "anthropic",
		models:      "claude-3-5-sonnet",
		apiKey:      "sk-ant",
	}}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader([]byte(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"query":"go"}}],"model":"claude-3-5-sonnet","stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`,
			))),
		}, nil
	})}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"codex"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "claude-3-5-sonnet",
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	assertChatRequestUserText(t, gotBody, "hi")
	if !bytes.Contains(w.Body.Bytes(), []byte(`"type":"function_call"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"name":"lookup"`)) {
		t.Fatalf("expected Codex function_call response, got %s", w.Body.String())
	}
}

func TestProxy_StreamingGeminiResponseToAnthropicTransform_MultipleToolCallsAcrossChunks(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:        "gemini-ch",
		channelType: "gemini",
		models:      "gemini-2.5-pro",
		apiKey:      "sk-gem",
	}}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		body := bytes.NewBufferString(
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"one\"}}}]}}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"search\",\"args\":{\"query\":\"two\"}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"totalTokenCount\":8},\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
				"data: [DONE]\n\n",
		)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(body),
		}, nil
	})}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"anthropic"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":  "gemini-2.5-pro",
		"stream": true,
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "hi"}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("expected transformed Gemini stream path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	body := w.Body.String()
	var toolBlocks []map[string]any
	var partialJSON []string
	var stopReason string
	messageStopped := false
	for _, block := range strings.Split(body, "\n\n") {
		eventType, data := parseSSEEventChunk([]byte(block))
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		switch eventType {
		case "content_block_start":
			contentBlock, _ := payload["content_block"].(map[string]any)
			if contentBlock != nil && contentBlock["type"] == "tool_use" {
				toolBlocks = append(toolBlocks, contentBlock)
			}
		case "content_block_delta":
			delta, _ := payload["delta"].(map[string]any)
			if delta != nil && delta["type"] == "input_json_delta" {
				partial, _ := delta["partial_json"].(string)
				partialJSON = append(partialJSON, partial)
			}
		case "message_delta":
			delta, _ := payload["delta"].(map[string]any)
			stopReason, _ = delta["stop_reason"].(string)
		case "message_stop":
			messageStopped = true
		}
	}
	if len(toolBlocks) != 2 {
		t.Fatalf("expected two anthropic tool_use blocks, got %s", body)
	}
	firstID, _ := toolBlocks[0]["id"].(string)
	secondID, _ := toolBlocks[1]["id"].(string)
	if firstID == "" || secondID == "" || firstID == secondID || toolBlocks[0]["name"] != "lookup" || toolBlocks[1]["name"] != "search" {
		t.Fatalf("unexpected anthropic tool identities, got %s", body)
	}
	if len(partialJSON) != 2 || partialJSON[0] != `{"query":"one"}` || partialJSON[1] != `{"query":"two"}` {
		t.Fatalf("unexpected anthropic tool arguments, got %s", body)
	}
	if stopReason != "tool_use" || !messageStopped {
		t.Fatalf("expected anthropic terminal events, got %s", body)
	}
}

func TestProxy_StreamingGeminiResponseToCodexTransform_MultipleToolCallsAcrossChunks(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:        "gemini-ch",
		channelType: "gemini",
		models:      "gemini-2.5-pro",
		apiKey:      "sk-gem",
	}}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		body := bytes.NewBufferString(
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"one\"}}}]}}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"search\",\"args\":{\"query\":\"two\"}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"totalTokenCount\":8},\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
				"data: [DONE]\n\n",
		)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(body),
		}, nil
	})}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"codex"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gemini-2.5-pro",
		"stream": true,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("expected transformed Gemini stream path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	body := w.Body.String()
	type functionCall struct {
		outputIndex int
		item        map[string]any
	}
	var calls []functionCall
	var completed map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		eventType, data := parseSSEEventChunk([]byte(block))
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		switch eventType {
		case "response.output_item.done":
			item, _ := payload["item"].(map[string]any)
			outputIndex, _ := payload["output_index"].(float64)
			if item != nil && item["type"] == "function_call" {
				calls = append(calls, functionCall{outputIndex: int(outputIndex), item: item})
			}
		case "response.completed":
			completed = payload
		}
	}
	if len(calls) != 2 {
		t.Fatalf("expected two completed Codex function calls, got %s", body)
	}
	firstID, _ := calls[0].item["id"].(string)
	firstCallID, _ := calls[0].item["call_id"].(string)
	secondID, _ := calls[1].item["id"].(string)
	secondCallID, _ := calls[1].item["call_id"].(string)
	if calls[0].outputIndex != 0 || calls[1].outputIndex != 1 ||
		firstID == "" || firstCallID == "" || secondID == "" || secondCallID == "" ||
		firstID == secondID || firstCallID == secondCallID ||
		calls[0].item["name"] != "lookup" || calls[1].item["name"] != "search" {
		t.Fatalf("unexpected Codex function call identities, got %s", body)
	}
	for index, wantQuery := range []string{"one", "two"} {
		arguments, _ := calls[index].item["arguments"].(string)
		var got map[string]any
		if err := json.Unmarshal([]byte(arguments), &got); err != nil || got["query"] != wantQuery {
			t.Fatalf("unexpected Codex function call arguments at index %d: %q", index, arguments)
		}
	}
	response, _ := completed["response"].(map[string]any)
	usage, _ := response["usage"].(map[string]any)
	if completed == nil || usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(5) {
		t.Fatalf("expected Codex completion event with usage, got %s", body)
	}
}
