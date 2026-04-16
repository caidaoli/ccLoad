package app

import (
	"bytes"
	"context"
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

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/messages", map[string]any{
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

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/responses", map[string]any{
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

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"messages"`)) || !bytes.Contains(gotBody, []byte(`"text":"hi"`)) {
		t.Fatalf("expected anthropic request body, got %s", gotBody)
	}
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

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/responses", map[string]any{
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
	if !bytes.Contains(gotBody, []byte(`"messages"`)) || !bytes.Contains(gotBody, []byte(`"text":"hi"`)) {
		t.Fatalf("expected anthropic request body, got %s", gotBody)
	}
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

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/messages", map[string]any{
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
	if strings.Count(body, `event: content_block_start`) < 2 {
		t.Fatalf("expected at least two anthropic tool content blocks, got %s", body)
	}
	if !strings.Contains(body, `"id":"call_1"`) || !strings.Contains(body, `"name":"lookup"`) {
		t.Fatalf("expected first anthropic tool_use block, got %s", body)
	}
	if !strings.Contains(body, `"id":"call_2"`) || !strings.Contains(body, `"name":"search"`) {
		t.Fatalf("expected second anthropic tool_use block, got %s", body)
	}
	if !strings.Contains(body, `"partial_json":"{\"query\":\"one\"}"`) || !strings.Contains(body, `"partial_json":"{\"query\":\"two\"}"`) {
		t.Fatalf("expected both anthropic input_json_delta payloads, got %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) || !strings.Contains(body, "event: message_stop") {
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

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/responses", map[string]any{
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
	if strings.Count(body, `event: response.output_item.done`) < 2 {
		t.Fatalf("expected at least two Codex output item events, got %s", body)
	}
	if !strings.Contains(body, `"call_id":"call_1"`) || !strings.Contains(body, `"name":"lookup"`) {
		t.Fatalf("expected first Codex function_call item, got %s", body)
	}
	if !strings.Contains(body, `"call_id":"call_2"`) || !strings.Contains(body, `"name":"search"`) {
		t.Fatalf("expected second Codex function_call item, got %s", body)
	}
	if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, `"input_tokens":3`) || !strings.Contains(body, `"output_tokens":5`) {
		t.Fatalf("expected Codex completion event with usage, got %s", body)
	}
}
