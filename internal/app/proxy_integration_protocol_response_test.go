package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
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
