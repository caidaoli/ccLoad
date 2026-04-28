package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 代理转发集成测试
// 端到端验证：上游模拟 → Server → gin 路由 → 请求转发 → 响应返回
// ============================================================================

// testChannel 测试用渠道定义
type testChannel struct {
	name        string
	channelType string
	models      string // 逗号分隔的模型列表
	apiKey      string
	priority    int
}

// proxyTestEnv 集成测试环境
type proxyTestEnv struct {
	server *Server
	store  storage.Store
	engine *gin.Engine
}

// setupProxyTestEnv 创建指向 mockUpstream 的完整测试 Server
// 每个渠道的 URL 使用 upstreamURLs map（channelIndex → upstreamURL）
func setupProxyTestEnv(t testing.TB, channels []testChannel, upstreamURLs map[int]string) *proxyTestEnv {
	t.Helper()

	srv := newInMemoryServer(t)
	store := srv.store

	ctx := context.Background()

	// 创建渠道和 API Key
	for i, ch := range channels {
		upURL := upstreamURLs[i]
		if upURL == "" {
			t.Fatalf("missing upstream URL for channel %d", i)
		}

		priority := ch.priority
		if priority == 0 {
			priority = 100 - i*10 // 按顺序递减优先级
		}

		chType := ch.channelType
		if chType == "" {
			chType = util.ChannelTypeOpenAI
		}

		// 构建模型列表
		var modelEntries []model.ModelEntry
		for _, m := range strings.Split(ch.models, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				modelEntries = append(modelEntries, model.ModelEntry{Model: m})
			}
		}

		cfg := &model.Config{
			Name:         ch.name,
			URL:          upURL,
			ChannelType:  chType,
			Priority:     priority,
			Enabled:      true,
			ModelEntries: modelEntries,
		}
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("CreateConfig for %s: %v", ch.name, err)
		}

		// 创建 API Key
		apiKey := ch.apiKey
		if apiKey == "" {
			apiKey = fmt.Sprintf("sk-test-%d", i)
		}
		err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: created.ID, KeyIndex: 0, APIKey: apiKey},
		})
		if err != nil {
			t.Fatalf("CreateAPIKeysBatch for %s: %v", ch.name, err)
		}
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	return &proxyTestEnv{
		server: srv,
		store:  store,
		engine: engine,
	}
}

// doProxyRequest 发送代理请求并返回响应
func doProxyRequest(t testing.TB, engine *gin.Engine, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key") // 默认 token

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// ============================================================================
// P0: 代理转发核心链路测试
// ============================================================================

func TestProxy_Success_NonStreaming(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + JSON
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 验证响应透传
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["id"] != "chatcmpl-1" {
		t.Fatalf("expected id=chatcmpl-1, got %v", resp["id"])
	}
}

func TestProxy_Success_NonStreaming_OpenAIToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":4,"totalTokenCount":11},"modelVersion":"gemini-2.5-pro"}`))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"openai"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gemini-2.5-pro",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
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

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from gemini" {
		t.Fatalf("unexpected translated response: %s", w.Body.String())
	}
}

func TestProxy_Success_NonStreaming_AnthropicToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":4,"totalTokenCount":11},"modelVersion":"gemini-2.5-pro"}`))),
			}, nil
		}),
	}

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

	var resp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Type != "message" || resp.Role != "assistant" || len(resp.Content) != 1 || resp.Content[0].Text != "hello from gemini" {
		t.Fatalf("unexpected translated anthropic response: %s", w.Body.String())
	}
}

func TestProxy_Success_NonStreaming_CodexToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":4,"totalTokenCount":11},"modelVersion":"gemini-2.5-pro"}`))),
			}, nil
		}),
	}

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

	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "response" || resp.Status != "completed" || len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "hello from gemini" {
		t.Fatalf("unexpected translated codex response: %s", w.Body.String())
	}
}

func TestProxy_Success_Streaming(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + SSE 流
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" World"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 验证 SSE 内容被透传
	body := w.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Fatalf("expected SSE to contain 'Hello', body: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected SSE to contain '[DONE]', body: %s", body)
	}
}

func TestProxy_Success_Streaming_OpenAIToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" World\"}]}}]}\n\ndata: [DONE]\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(body),
			}, nil
		}),
	}

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
		"model":    "gemini-2.5-pro",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
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
	if !strings.Contains(body, `"chat.completion.chunk"`) {
		t.Fatalf("expected OpenAI stream chunk, got %s", body)
	}
	if !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, `"content":" World"`) {
		t.Fatalf("expected translated content chunks, got %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected done marker, got %s", body)
	}
}

func TestProxy_Success_Streaming_AnthropicToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" World\"}]}}]}\n\ndata: [DONE]\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(body),
			}, nil
		}),
	}

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
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: content_block_delta") || !strings.Contains(body, `"text":"Hello"`) {
		t.Fatalf("expected anthropic stream events, got %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("expected anthropic message_stop event, got %s", body)
	}
}

func TestProxy_Success_Streaming_CodexToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" World\"}]}}]}\n\ndata: [DONE]\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(body),
			}, nil
		}),
	}

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
	if !strings.Contains(body, "event: response.output_text.delta") || !strings.Contains(body, `"delta":"Hello"`) {
		t.Fatalf("expected codex delta event, got %s", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("expected codex completed event, got %s", body)
	}
}

func TestProxy_Success_NonStreaming_OpenAIToAnthropicTransform(t *testing.T) {
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
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":4}}`))),
			}, nil
		}),
	}

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

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from anthropic" {
		t.Fatalf("unexpected translated response: %s", w.Body.String())
	}
}

func TestProxy_UpstreamMode_PassesThroughClientProtocolNatively(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotAuth string
	var gotAPIKey string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", channelType: "anthropic", models: "gpt-4o", apiKey: "sk-openai-upstream"},
	}, map[int]string{0: "https://openai-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("x-api-key")
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"chatcmpl-upstream","object":"chat.completion","created":123,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"native upstream"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"openai"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeUpstream
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	reloaded, err := env.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if reloaded.GetProtocolTransformMode() != model.ProtocolTransformModeUpstream {
		t.Fatalf("expected persisted protocol transform mode upstream, got %q", reloaded.GetProtocolTransformMode())
	}
	if reloaded.ResolveUpstreamProtocol("openai") != "openai" {
		t.Fatalf("expected runtime upstream protocol openai, got %q", reloaded.ResolveUpstreamProtocol("openai"))
	}
	env.server.InvalidateChannelListCache()
	candidates, err := env.server.selectCandidatesByModelAndType(context.Background(), "gpt-4o", "openai")
	if err != nil {
		t.Fatalf("selectCandidatesByModelAndType failed: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].GetProtocolTransformMode() != model.ProtocolTransformModeUpstream {
		t.Fatalf("expected candidate protocol transform mode upstream, got %q", candidates[0].GetProtocolTransformMode())
	}
	if candidates[0].ResolveUpstreamProtocol("openai") != "openai" {
		t.Fatalf("expected candidate runtime upstream protocol openai, got %q", candidates[0].ResolveUpstreamProtocol("openai"))
	}

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected native openai upstream path, got %s", gotPath)
	}
	if gotAuth != "Bearer sk-openai-upstream" {
		t.Fatalf("expected openai auth header, got %q", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("expected no anthropic x-api-key header, got %q", gotAPIKey)
	}
	if !bytes.Contains(gotBody, []byte(`"messages"`)) {
		t.Fatalf("expected native openai request body, got %s", gotBody)
	}
	if bytes.Contains(gotBody, []byte(`"anthropic_version"`)) {
		t.Fatalf("expected request body to skip anthropic transform, got %s", gotBody)
	}

	var resp struct {
		Object string `json:"object"`
		Model  string `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "chat.completion" || resp.Model != "gpt-4o" {
		t.Fatalf("expected native openai response passthrough, got %+v", resp)
	}
}

func TestProxy_Success_Streaming_OpenAIToAnthropicTransform(t *testing.T) {
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
			body := bytes.NewBufferString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet\",\"stop_reason\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hello\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\" World\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(body),
			}, nil
		}),
	}

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
		"stream":   true,
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
	body := w.Body.String()
	if !strings.Contains(body, `"chat.completion.chunk"`) || !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected translated stream: %s", body)
	}
}

func TestProxy_Success_NonStreaming_CodexToAnthropicTransform(t *testing.T) {
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
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":4}}`))),
			}, nil
		}),
	}

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
	var resp struct {
		Object string `json:"object"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "response" || len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "hello from anthropic" {
		t.Fatalf("unexpected translated codex response: %s", w.Body.String())
	}
}

func TestProxy_Success_NonStreaming_CodexBareMessageToAnthropicTransform(t *testing.T) {
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
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":4}}`))),
			}, nil
		}),
	}

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
}

func TestProxy_Success_Streaming_CodexToAnthropicTransform(t *testing.T) {
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
			body := bytes.NewBufferString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet\",\"stop_reason\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hello\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\" World\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(body),
			}, nil
		}),
	}

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
		"model":  "claude-3-5-sonnet",
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
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"messages"`)) || !bytes.Contains(gotBody, []byte(`"text":"hi"`)) {
		t.Fatalf("expected anthropic request body, got %s", gotBody)
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: response.output_text.delta") || !strings.Contains(body, `"delta":"Hello"`) {
		t.Fatalf("expected codex stream delta event, got %s", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("expected codex completed event, got %s", body)
	}
}

func TestProxy_Success_NonStreaming_OpenAIToCodexTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ch", channelType: "codex", models: "gpt-5-codex", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from codex"}]}],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}`,
				))),
			}, nil
		}),
	}

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
		"model":    "gpt-5-codex",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("expected codex responses path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"type":"input_text"`)) {
		t.Fatalf("expected codex request body, got %s", gotBody)
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from codex" {
		t.Fatalf("unexpected translated response: %s", w.Body.String())
	}
}

func TestProxy_Success_Streaming_OpenAIToCodexTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ch", channelType: "codex", models: "gpt-5-codex", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			body := bytes.NewBufferString("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":7,\"output_tokens\":4,\"total_tokens\":11}}}\n\n")
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
	cfg.ProtocolTransforms = []string{"openai"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-5-codex",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("expected codex responses path, got %s", gotPath)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"chat.completion.chunk"`) || !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected openai stream chunk, got %s", body)
	}
}

func TestProxy_Success_NonStreaming_CodexToOpenAITransform(t *testing.T) {
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
	cfg.ProtocolTransforms = []string{"codex"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/responses", map[string]any{
		"model": "gpt-4o",
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
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

	var resp struct {
		Object string `json:"object"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "response" || len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "hello from openai" {
		t.Fatalf("unexpected translated codex response: %s", w.Body.String())
	}
}

func TestProxy_Success_Streaming_CodexToOpenAITransform(t *testing.T) {
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
	cfg.ProtocolTransforms = []string{"codex"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/responses", map[string]any{
		"model":  "gpt-4o",
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
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected openai chat completions path, got %s", gotPath)
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: response.output_text.delta") || !strings.Contains(body, `"delta":"Hello"`) || !strings.Contains(body, "event: response.completed") {
		t.Fatalf("expected codex stream output, got %s", body)
	}
}

func TestProxy_GeminiTransform_UsesResolvedActualModelInUpstreamPath(t *testing.T) {
	t.Parallel()

	var gotPath string

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "alias-model", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ProtocolTransforms = []string{"openai"}
	cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
	cfg.ModelEntries = []model.ModelEntry{{
		Model:         "alias-model",
		RedirectModel: "gemini-2.5-pro",
	}}
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"gemini-2.5-pro"}`))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "alias-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected resolved actual model path, got %s", gotPath)
	}

	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Model != "alias-model" {
		t.Fatalf("expected client-visible response model alias-model, got %s", resp.Model)
	}
}

func TestProxy_Success_Streaming_OpenAIToGeminiTransform_TextPlainSSE(t *testing.T) {
	t.Parallel()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body := bytes.NewBufferString("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\ndata: [DONE]\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/plain; charset=utf-8"},
				},
				Body: io.NopCloser(body),
			}, nil
		}),
	}

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
		"model":    "gemini-2.5-pro",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"chat.completion.chunk"`) || !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected translated text/plain SSE stream: %s", body)
	}
}

func TestProxy_StructuredOpenAIImageTransformHitsUpstream(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-2.5-pro"}`,
				))),
			}, nil
		}),
	}

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
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "hi"},
				{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
			},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(gotBody, []byte(`"fileUri":"https://example.com/a.png"`)) {
		t.Fatalf("expected structured image request to reach upstream, got %s", gotBody)
	}
}

func TestProxy_StructuredAnthropicBlocksTransformHitsUpstream(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-2.5-pro"}`,
				))),
			}, nil
		}),
	}

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
		"messages": []map[string]any{
			{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"query": "go"}}}},
			{"role": "user", "content": []map[string]any{
				{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "cGRm"}},
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"},
			}},
		},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(gotBody, []byte(`"functionCall"`)) || !bytes.Contains(gotBody, []byte(`"functionResponse"`)) || !bytes.Contains(gotBody, []byte(`"inlineData"`)) {
		t.Fatalf("expected structured anthropic blocks to reach upstream, got %s", gotBody)
	}
}

func TestProxy_StructuredCodexFunctionFamilyTransformHitsUpstream(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-2.5-pro"}`,
				))),
			}, nil
		}),
	}

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
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": []map[string]any{
				{"type": "input_image", "image_url": "https://example.com/a.png"},
				{"type": "input_file", "file_id": "file_123", "filename": "doc.pdf"},
			}},
			{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": map[string]any{"query": "go"}},
			{"type": "function_call_output", "call_id": "call_1", "output": "done"},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(gotBody, []byte(`"functionCall"`)) || !bytes.Contains(gotBody, []byte(`"functionResponse"`)) || !bytes.Contains(gotBody, []byte(`"fileUri":"https://example.com/a.png"`)) {
		t.Fatalf("expected structured codex request to reach upstream, got %s", gotBody)
	}
}

func TestProxy_UnsupportedStructuredTransformRequestReturns400(t *testing.T) {
	t.Parallel()

	var called bool
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("should not hit upstream")
		}),
	}

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
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type":  "mystery",
				"value": true,
			}},
		}},
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for unsupported structured transform request")
	}
}

func TestProxy_UnsupportedStructuredAnthropicTransformRequestReturns400(t *testing.T) {
	t.Parallel()

	var called bool
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("should not hit upstream")
		}),
	}

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
			"role": "user",
			"content": []map[string]any{{
				"type":  "mystery",
				"value": true,
			}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for unsupported anthropic structured transform request")
	}
}

func TestProxy_UnsupportedStructuredCodexTransformRequestReturns400(t *testing.T) {
	t.Parallel()

	var called bool
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", channelType: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("should not hit upstream")
		}),
	}

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
			"content": []map[string]any{{"type": "mystery", "value": true}},
		}},
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for unsupported codex structured transform request")
	}
}

func TestProxy_ChannelRetry_On503(t *testing.T) {
	t.Parallel()

	// 渠道1：返回 503
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer upstream1.Close()

	// 渠道2：返回 200
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-fail", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (fallback to ch2), got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_MultiURL5xx_SwitchesToNextChannel(t *testing.T) {
	t.Parallel()

	var ch1FailCalls atomic.Int64
	var ch1SecondURLCalls atomic.Int64
	var ch2Calls atomic.Int64

	// 渠道1 URL1: 固定 503
	upstreamFail := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch1FailCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer upstreamFail.Close()

	// 渠道1 URL2: 即使可用也不应被尝试（新策略：5xx 直接切渠道）
	upstreamShouldSkip := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch1SecondURLCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch1-url2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamShouldSkip.Close()

	// 渠道2: 正常返回，用于验证“切换到下一个渠道”
	upstreamCh2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch2Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamCh2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-multi-url", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch-fallback", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{
		0: upstreamFail.URL + "\n" + upstreamShouldSkip.URL,
		1: upstreamCh2.URL,
	})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 config, got %d", len(configs))
	}

	var channelID int64
	for _, cfg := range configs {
		if cfg.Name == "ch-multi-url" {
			channelID = cfg.ID
			break
		}
	}
	if channelID == 0 {
		t.Fatalf("ch-multi-url not found in configs")
	}

	// 强制渠道1首跳命中失败URL，避免随机首跳影响稳定性
	env.server.urlSelector.CooldownURL(channelID, upstreamShouldSkip.URL)

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected switch to next channel, got body: %s", w.Body.String())
	}
	ch1Fail := ch1FailCalls.Load()
	ch1Second := ch1SecondURLCalls.Load()
	ch2 := ch2Calls.Load()
	if ch1Fail < 1 {
		t.Fatalf("expected channel1 first URL attempted, got %d", ch1Fail)
	}
	if ch1Second != 0 {
		t.Fatalf("expected channel1 second URL not attempted on 5xx, got %d", ch1Second)
	}
	if ch2 < 1 {
		t.Fatalf("expected next channel attempted, got %d", ch2)
	}
}

func TestProxy_MultiURLFallbackOn598_DoesNotChannelCooldownEarly(t *testing.T) {
	t.Parallel()

	var failCalls atomic.Int64
	var okCalls atomic.Int64

	// URL1: 首字节超时（598）
	upstreamTimeout := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls.Add(1)
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstreamTimeout.Close()

	// URL2: 正常返回
	upstreamOK := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from-url2\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-multi-url", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamTimeout.URL + "\n" + upstreamOK.URL,
	})

	// 缩短首字节超时，稳定触发 598
	env.server.firstByteTimeout = 50 * time.Millisecond

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	// 强制 URL2 进入冷却，确保首跳先打到 timeout URL
	env.server.urlSelector.CooldownURL(channelID, upstreamOK.URL)

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-url2") {
		t.Fatalf("expected fallback to url2 on 598, got body: %s", w.Body.String())
	}
	fail := failCalls.Load()
	ok := okCalls.Load()
	if fail < 1 || ok < 1 {
		t.Fatalf("expected both URLs attempted, failCalls=%d okCalls=%d", fail, ok)
	}

	// 关键断言：598 触发多URL内部回退成功后，不应残留渠道级冷却
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if _, exists := cooldowns[channelID]; exists {
		t.Fatalf("unexpected channel cooldown for multi-url fallback success, channel_id=%d", channelID)
	}
}

func TestProxy_MultiURLFirstAttempt_UsesWeightedRandom(t *testing.T) {
	t.Parallel()

	var fastCalls atomic.Int64
	var slowCalls atomic.Int64

	upstreamFast := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastCalls.Add(1)
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-fast","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamFast.Close()

	upstreamSlow := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowCalls.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-slow","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamSlow.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-weighted-first", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamSlow.URL + "\n" + upstreamFast.URL,
	})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	// 预热EWMA，确保不是“未探索优先”分支
	env.server.urlSelector.RecordLatency(channelID, upstreamFast.URL, 5*time.Millisecond)
	env.server.urlSelector.RecordLatency(channelID, upstreamSlow.URL, 30*time.Millisecond)

	const rounds = 120
	for range rounds {
		w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model":    "gpt-4",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	}

	fast := fastCalls.Load()
	slow := slowCalls.Load()
	if fast <= slow {
		t.Fatalf("expected weighted random to prefer fast URL, fast=%d slow=%d", fast, slow)
	}
	if slow < 5 {
		t.Fatalf("expected slow URL to be selected sometimes (not deterministic first pick), fast=%d slow=%d", fast, slow)
	}
}

func TestProxy_MultiURLProbeCanceledByShutdown_DoesNotPolluteCooldown(t *testing.T) {
	upstreamA := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-a","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamA.Close()

	upstreamB := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-b","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamB.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-probe-shutdown", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamA.URL + "\n" + upstreamB.URL,
	})

	env.server.urlSelector.probeTimeout = 5 * time.Second
	started := make(chan struct{}, 2)
	env.server.urlSelector.probeDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("probe dial did not start in time")
		}
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		env.server.urlSelector.mu.RLock()
		probingLeft := len(env.server.urlSelector.probing)
		env.server.urlSelector.mu.RUnlock()
		if probingLeft == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected probing markers to be cleared after shutdown, got %d", probingLeft)
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, u := range []string{upstreamA.URL, upstreamB.URL} {
		if env.server.urlSelector.IsCooledDown(channelID, u) {
			t.Fatalf("expected canceled probe not to cooldown url: %s", u)
		}
	}
}

func TestProxy_KeyRetry_On401(t *testing.T) {
	t.Parallel()

	callCount := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		if strings.Contains(auth, "sk-bad") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"authentication_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	// 创建服务器并使用其 store
	srv := newInMemoryServer(t)
	store := srv.store

	ctx := context.Background()
	cfg := &model.Config{
		Name:         "ch1-multikey",
		URL:          upstream.URL,
		ChannelType:  util.ChannelTypeOpenAI,
		Priority:     100,
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4"}},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-bad"},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-good"},
	})
	if err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	w := doProxyRequest(t, engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (key retry to sk-good), got %d: %s", w.Code, w.Body.String())
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 upstream calls (key retry), got %d", callCount)
	}
}

func TestProxy_AllChannelsExhausted(t *testing.T) {
	t.Parallel()

	callCount1 := 0
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount1++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream1.Close()

	callCount2 := 0
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount2++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// 所有渠道失败时应返回最后一个错误状态码
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	// 关键行为：必须耗尽所有可用渠道，而不是只尝试第一个就返回（避免“假绿”）。
	if callCount1 < 1 || callCount2 < 1 {
		t.Fatalf("expected to try all channels at least once, got upstream1=%d upstream2=%d", callCount1, callCount2)
	}
}

func TestProxy_ClientCancel_Returns499(t *testing.T) {
	t.Parallel()

	// 上游延迟响应
	upstreamStarted := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-upstreamStarted:
			// already closed
		default:
			close(upstreamStarted)
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 创建可取消的请求
	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")

	// 等上游请求真的发出后再取消，避免“还没发出去就 cancel”导致语义漂移
	go func() {
		select {
		case <-upstreamStarted:
		case <-time.After(1 * time.Second):
		}
		cancel()
	}()

	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	// 客户端取消应返回 499 或超时相关状态
	if w.Code != StatusClientClosedRequest && w.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 499 or 504, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_ModelNotAllowed_Returns403(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4,gpt-3.5-turbo", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 限制 token 只能使用 gpt-3.5-turbo
	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenModels[tokenHash] = []string{"gpt-3.5-turbo"}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_ChannelRestriction_UsesOnlyAllowedChannel(t *testing.T) {
	t.Parallel()

	var disallowedHits atomic.Int32
	disallowedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		disallowedHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"disallowed"}`))
	}))
	defer disallowedUpstream.Close()

	var allowedHits atomic.Int32
	allowedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"allowed","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer allowedUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "disallowed-high-priority", models: "gpt-4", apiKey: "sk-disallowed", priority: 100},
		{name: "allowed-low-priority", models: "gpt-4", apiKey: "sk-allowed", priority: 10},
	}, map[int]string{0: disallowedUpstream.URL, 1: allowedUpstream.URL})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	var allowedID int64
	for _, cfg := range configs {
		if cfg.Name == "allowed-low-priority" {
			allowedID = cfg.ID
			break
		}
	}
	if allowedID == 0 {
		t.Fatal("allowed channel id not found")
	}

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenChannels[tokenHash] = []int64{allowedID}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := allowedHits.Load(); got != 1 {
		t.Fatalf("allowed upstream hits=%d, want 1", got)
	}
	if got := disallowedHits.Load(); got != 0 {
		t.Fatalf("disallowed upstream hits=%d, want 0", got)
	}
}

func TestProxy_ChannelRestriction_Returns403WhenNoAllowedCandidate(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only-channel", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenChannels[tokenHash] = []int64{999999}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream hits=%d, want 0", got)
	}
}

func TestProxy_ChannelRestriction_PreservesNoCandidateResponse(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only-channel", models: "gpt-3.5-turbo", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenChannels[tokenHash] = []int64{configs[0].ID}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream hits=%d, want 0", got)
	}
}

func TestProxy_CostLimitExceeded_Returns429(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 设置 token 费用已超限
	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenCostLimits[tokenHash] = tokenCostLimit{
		usedMicroUSD:  200_000, // $0.20
		limitMicroUSD: 100_000, // $0.10 限额
	}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}

	// 验证错误包含 cost_limit_exceeded
	body := w.Body.String()
	if !strings.Contains(body, "cost_limit_exceeded") {
		t.Fatalf("expected 'cost_limit_exceeded' in body: %s", body)
	}
}

func TestProxy_NoChannels_Returns503(t *testing.T) {
	t.Parallel()

	// 创建没有渠道的环境
	srv := newInMemoryServer(t)
	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	w := doProxyRequest(t, engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_SSEErrorEvent_TriggersCooldown(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + SSE 但包含 error 事件
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 先正常发几个 chunk，然后发 error
		// 这里首个 chunk 故意做大于 SSEBufferSize，确保代理已经向客户端提交过响应，
		// 后续 error event 才会落到“只能冷却，不能同请求重试”的路径。
		largeContent := strings.Repeat("Hi", SSEBufferSize)
		chunks := []string{
			fmt.Sprintf(`data: {"choices":[{"delta":{"content":"%s"}}]}`, largeContent),
			`event: error` + "\n" + `data: {"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	ctx := context.Background()
	// 先拿到渠道ID（避免硬编码）
	var channelID int64
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	for _, cfg := range configs {
		if cfg.Name == "ch1" {
			channelID = cfg.ID
			break
		}
	}
	if channelID == 0 {
		t.Fatalf("channel ch1 not found")
	}

	// 预期：请求前没有渠道冷却（否则测试语义不成立）
	beforeCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns(before): %v", err)
	}
	if _, exists := beforeCooldowns[channelID]; exists {
		t.Fatalf("expected no channel cooldown before request, but found one for channel_id=%d", channelID)
	}

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// SSE error 事件的处理：HTTP 状态码已经是 200（头部已发送），
	// 但内部应触发冷却逻辑。测试验证响应不崩溃。
	// 响应仍是 200（因为 header 已发送），但内部会记录冷却
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (header already sent), got %d: %s", w.Code, w.Body.String())
	}

	// 关键断言：SSE error 事件必须触发冷却副作用（单Key渠道会升级为渠道级冷却）。
	afterCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns(after): %v", err)
	}
	until, exists := afterCooldowns[channelID]
	if !exists {
		t.Fatalf("expected channel cooldown to be set after SSE error event, channel_id=%d", channelID)
	}
	if time.Until(until) <= 0 {
		t.Fatalf("expected channel cooldown until in the future, got %v", until)
	}
}

func TestProxy_SSEErrorEventBeforeClientOutput_RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int32
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","param":null},"sequence_number":2}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream1.Close()

	var secondCalls atomic.Int32
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"from-ch2"}}]}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-overloaded", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next channel, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "from-ch2") {
		t.Fatalf("expected response body from second channel, got: %s", body)
	}
	if strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("expected first channel SSE error not to leak to client, body: %s", body)
	}
	if firstCalls.Load() != 1 {
		t.Fatalf("expected first channel to be tried once, got %d", firstCalls.Load())
	}
	if secondCalls.Load() != 1 {
		t.Fatalf("expected second channel to be tried once, got %d", secondCalls.Load())
	}
}
