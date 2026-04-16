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

func TestProxy_Success_NonStreaming_GeminiToOpenAITransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "openai-ch", channelType: "openai", models: "gpt-4o", apiKey: "sk-oai"},
	}, map[int]string{0: "https://openai-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"chatcmpl_1","object":"chat.completion","created":0,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello from openai"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11}}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"gemini"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1beta/models/gpt-4o:generateContent", map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected openai chat completions path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"role":"user"`)) || !bytes.Contains(gotBody, []byte(`"content":"hi"`)) {
		t.Fatalf("expected openai request body, got %s", gotBody)
	}
	if !strings.Contains(w.Body.String(), `"role":"model"`) || !strings.Contains(w.Body.String(), `"text":"hello from openai"`) {
		t.Fatalf("unexpected gemini response: %s", w.Body.String())
	}
}

func TestProxy_Success_Streaming_GeminiToOpenAITransform(t *testing.T) {
	t.Parallel()

	var gotPath string

	env := setupProxyTestEnv(t, []testChannel{
		{name: "openai-ch", channelType: "openai", models: "gpt-4o", apiKey: "sk-oai"},
	}, map[int]string{0: "https://openai-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			body := bytes.NewBufferString("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
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
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1beta/models/gpt-4o:streamGenerateContent", map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected openai chat completions path, got %s", gotPath)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"text":"Hello"`) || !strings.Contains(body, `"finishReason":"STOP"`) {
		t.Fatalf("unexpected gemini stream body: %s", body)
	}
}
