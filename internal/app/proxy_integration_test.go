package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestProxy_Success_Streaming(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + SSE 流
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestProxy_ChannelRetry_On503(t *testing.T) {
	t.Parallel()

	// 渠道1：返回 503
	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer upstream1.Close()

	// 渠道2：返回 200
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestProxy_KeyRetry_On401(t *testing.T) {
	t.Parallel()

	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream.URL, 1: upstream.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// 所有渠道失败时应返回最后一个错误状态码
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_ClientCancel_Returns499(t *testing.T) {
	t.Parallel()

	// 上游延迟响应
	upstreamStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestProxy_CostLimitExceeded_Returns429(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 先正常发几个 chunk，然后发 error
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"Hi"}}]}`,
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
}
