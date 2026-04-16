package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestProxy_Success_Streaming_GeminiToAnthropicTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", channelType: "anthropic", models: "claude-3-5-sonnet", apiKey: "sk-ant"},
	}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString(
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3-5-sonnet\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\"}}\n\n" +
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n" +
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"gemini"}
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1beta/models/claude-3-5-sonnet:streamGenerateContent", map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"messages"`)) || !bytes.Contains(gotBody, []byte(`"role":"user"`)) || !bytes.Contains(gotBody, []byte(`"text":"hi"`)) {
		t.Fatalf("expected anthropic request body, got %s", gotBody)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"functionCall":{"name":"lookup","args":{"query":"go"}}`) {
		t.Fatalf("expected translated Gemini functionCall, got %s", body)
	}
	if !strings.Contains(body, `"finishReason":"STOP"`) || !strings.Contains(body, `"promptTokenCount":3`) || !strings.Contains(body, `"candidatesTokenCount":5`) {
		t.Fatalf("expected Gemini usage metadata, got %s", body)
	}
}

func TestProxy_Success_Streaming_GeminiToCodexTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ch", channelType: "codex", models: "gpt-5-codex", apiKey: "sk-cdx"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString(
				"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":{\"query\":\"go\"}}}\n\n" +
					"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n",
			)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"gemini"}
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1beta/models/gpt-5-codex:streamGenerateContent", map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("expected codex responses path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"input"`)) || !bytes.Contains(gotBody, []byte(`"input_text"`)) || !bytes.Contains(gotBody, []byte(`"text":"hi"`)) {
		t.Fatalf("expected codex request body, got %s", gotBody)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"functionCall":{"name":"lookup","args":{"query":"go"}}`) {
		t.Fatalf("expected translated Gemini functionCall, got %s", body)
	}
	if !strings.Contains(body, `"finishReason":"STOP"`) || !strings.Contains(body, `"promptTokenCount":3`) || !strings.Contains(body, `"candidatesTokenCount":5`) {
		t.Fatalf("expected Gemini completion payload, got %s", body)
	}
}
