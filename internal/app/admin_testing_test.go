package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"

	"github.com/gin-gonic/gin"
)

// TestHandleChannelTest 测试渠道测试功能
func TestHandleChannelTest(t *testing.T) {
	tests := []struct {
		name           string
		channelID      string
		requestBody    map[string]any
		setupData      bool
		expectedStatus int
		expectSuccess  bool
	}{
		{
			name:      "无效的渠道ID",
			channelID: "invalid",
			requestBody: map[string]any{
				"model":        "test-model",
				"channel_type": "anthropic",
			},
			setupData:      false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
		{
			name:      "渠道不存在",
			channelID: "999",
			requestBody: map[string]any{
				"model":        "test-model",
				"channel_type": "anthropic",
			},
			setupData:      false,
			expectedStatus: http.StatusNotFound,
			expectSuccess:  false,
		},
		{
			name:      "无效的请求体",
			channelID: "1",
			requestBody: map[string]any{
				"invalid_field": "value",
			},
			setupData:      false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建测试服务器
			srv := newInMemoryServer(t)

			ctx := context.Background()

			// 设置测试数据(如果需要)
			if tt.setupData {
				cfg := &model.Config{
					ID:           1,
					Name:         "test-channel",
					URL:          "http://test.example.com",
					Priority:     1,
					ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
					Enabled:      true,
				}
				_, err := srv.store.CreateConfig(ctx, cfg)
				if err != nil {
					t.Fatalf("创建测试渠道失败: %v", err)
				}
			}

			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+tt.channelID+"/test", tt.requestBody))
			c.Params = gin.Params{{Key: "id", Value: tt.channelID}}

			// 调用处理函数
			srv.HandleChannelTest(c)

			// 验证响应状态码
			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码 %d, 实际 %d, 响应: %s", tt.expectedStatus, w.Code, w.Body.String())
			}

			resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
			if resp.Success != tt.expectSuccess {
				t.Errorf("期望 success=%v, 实际=%v, error=%q", tt.expectSuccess, resp.Success, resp.Error)
			}
		})
	}
}

func TestTestChannelAPI_MultiURLFallbackAndSelectorFeedback(t *testing.T) {
	failCalls := 0
	okCalls := 0

	failUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"upstream fail"}}`))
	}))
	defer failUpstream.Close()

	okUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		time.Sleep(15 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)

	cfg := &model.Config{
		ID:           9527,
		Name:         "multi-url-test",
		URL:          failUpstream.URL + "\n" + okUpstream.URL,
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}

	// 强制第一跳命中失败URL，验证是否会回退到第二个URL。
	srv.urlSelector.CooldownURL(cfg.ID, okUpstream.URL)

	req := &testutil.TestChannelRequest{
		Model:       "gpt-4o-mini",
		ChannelType: "openai",
		Content:     "hello",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	success, _ := result["success"].(bool)
	if !success {
		t.Fatalf("expected fallback success, got result=%+v", result)
	}
	if failCalls < 1 || okCalls < 1 {
		t.Fatalf("expected both URLs attempted, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
	if !srv.urlSelector.IsCooledDown(cfg.ID, failUpstream.URL) {
		t.Fatalf("expected failed URL to be cooled down, url=%s", failUpstream.URL)
	}
	if lat, ok := srv.urlSelector.latencies[urlKey{channelID: cfg.ID, url: okUpstream.URL}]; !ok || lat == nil || lat.value <= 0 {
		t.Fatalf("expected success URL latency recorded, got=%v", lat)
	}
}

func TestTestChannelAPI_MultiURLFallbackOnPlainText502(t *testing.T) {
	failCalls := 0
	okCalls := 0

	failUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("error code: 502"))
	}))
	defer failUpstream.Close()

	okUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)

	cfg := &model.Config{
		ID:           9528,
		Name:         "multi-url-plain-502-test",
		URL:          failUpstream.URL + "\n" + okUpstream.URL,
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}

	// 强制第一跳命中 502 的坏 URL，验证 text/plain 错误体也会继续回退。
	srv.urlSelector.CooldownURL(cfg.ID, okUpstream.URL)

	req := &testutil.TestChannelRequest{
		Model:       "gpt-4o-mini",
		ChannelType: "openai",
		Content:     "hello",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	success, _ := result["success"].(bool)
	if !success {
		t.Fatalf("expected fallback success on plain 502, got result=%+v", result)
	}
	if failCalls < 1 || okCalls < 1 {
		t.Fatalf("expected both URLs attempted, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
	if !srv.urlSelector.IsCooledDown(cfg.ID, failUpstream.URL) {
		t.Fatalf("expected failed URL to be cooled down, url=%s", failUpstream.URL)
	}
	if got, ok := result["response_text"].(string); !ok || got != "ok" {
		t.Fatalf("expected second URL success response_text=ok, got=%+v", result)
	}
}

func TestHandleChannelTest_RejectsBaseURL(t *testing.T) {
	failCalls := 0
	failUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failUpstream.Close()

	okCalls := 0
	okUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	cfg := &model.Config{
		Name:         "channel-test-reject-base-url",
		URL:          failUpstream.URL + "\n" + okUpstream.URL,
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+fmt.Sprintf("%d", created.ID)+"/test", map[string]any{
		"model":        "gpt-4o-mini",
		"channel_type": "openai",
		"base_url":     okUpstream.URL,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
	if resp.Success {
		t.Fatalf("expected success=false, resp=%+v", resp)
	}
	if !strings.Contains(resp.Error, "/test-url") {
		t.Fatalf("expected error to guide /test-url, got %q", resp.Error)
	}
	if failCalls != 0 || okCalls != 0 {
		t.Fatalf("expected no upstream request, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
}

func TestHandleChannelURLTest_UsesForcedURL(t *testing.T) {
	failCalls := 0
	failUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"should not hit this url"}}`))
	}))
	defer failUpstream.Close()

	okCalls := 0
	okUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	cfg := &model.Config{
		Name:         "single-url-test",
		URL:          failUpstream.URL + "\n" + okUpstream.URL,
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}
	// selector 和多 URL 顺序都不该影响显式单 URL 测试。
	srv.urlSelector.CooldownURL(created.ID, okUpstream.URL)

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+fmt.Sprintf("%d", created.ID)+"/test-url", map[string]any{
		"model":        "gpt-4o-mini",
		"channel_type": "openai",
		"base_url":     okUpstream.URL,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelURLTest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("expected success=true, data=%+v", resp.Data)
	}
	if failCalls != 0 {
		t.Fatalf("expected forced base_url to skip fail url, failCalls=%d", failCalls)
	}
	if okCalls != 1 {
		t.Fatalf("expected forced base_url called once, okCalls=%d", okCalls)
	}
}

// TestHandleChannelTest_NoAPIKey 渠道存在但无 API key
func TestHandleChannelTest_NoAPIKey(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	// 创建渠道但不添加 API key
	cfg := &model.Config{
		Name:         "no-key-channel",
		URL:          "http://test.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "test-model"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":        "test-model",
		"channel_type": "anthropic",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	// 状态码 200，但 data 中 success=false
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	// RespondJSON 包装 success=true (外层), data 内部有 success: false
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("外层 APIResponse.Success 应为 true, error=%q", resp.Error)
	}

	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatal("data.success 应为 false（渠道无 API key）")
	}

	dataError, _ := resp.Data["error"].(string)
	if dataError == "" {
		t.Fatal("data.error 不应为空")
	}
}

// TestHandleChannelTest_UnsupportedModel 渠道存在、有 Key，但模型不支持
func TestHandleChannelTest_UnsupportedModel(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	cfg := &model.Config{
		Name:         "limited-model-channel",
		URL:          "http://test.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 添加 API key
	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "test-key-001"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":        "gpt-4-not-supported",
		"channel_type": "anthropic",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatal("data.success 应为 false（模型不支持）")
	}
}

// TestHandleChannelTest_SuccessfulAPI 使用 mock server 模拟成功的 API 调用
func TestHandleChannelTest_SuccessfulAPI(t *testing.T) {
	// 创建 mock 上游服务器，返回成功的 Anthropic 响应
	mockResp := `{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello"}],
		"model": "claude-3-5-sonnet",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResp))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	// 替换 HTTP client 以使用 mock server
	srv.client = upstream.Client()

	ctx := context.Background()

	cfg := &model.Config{
		Name:         "test-success-channel",
		URL:          upstream.URL,
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":        "claude-3-5-sonnet",
		"channel_type": "anthropic",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("外层 APIResponse.Success 应为 true, error=%q", resp.Error)
	}

	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("data.success 应为 true（API 调用成功）, data=%+v", resp.Data)
	}
}

// TestHandleChannelTest_FailedAPI 使用 mock server 模拟失败的 API 调用
func TestHandleChannelTest_FailedAPI(t *testing.T) {
	// 创建 mock 上游服务器，返回 401 错误
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()

	cfg := &model.Config{
		Name:         "test-fail-channel",
		URL:          upstream.URL,
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-invalid-key"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":        "claude-3-5-sonnet",
		"channel_type": "anthropic",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatal("data.success 应为 false（API 调用失败 401）")
	}

	// 验证冷却决策被记录
	if action, ok := resp.Data["cooldown_action"].(string); ok {
		if action == "" {
			t.Fatal("失败时应有冷却决策记录")
		}
		t.Logf("冷却决策: %s", action)
	}
}

func TestHandleChannelTest_EventStreamHeaderWithJSONBodyFallback(t *testing.T) {
	// 模拟“Content-Type=event-stream，但实际返回完整JSON”场景
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id":"resp_test",
			"status":"completed",
			"output":[
				{
					"type":"message",
					"content":[{"type":"output_text","text":"fallback text"}]
				}
			],
			"usage":{"input_tokens":12,"output_tokens":8}
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:         "test-codex-json-fallback",
		URL:          upstream.URL,
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.2"}},
		Enabled:      true,
		ChannelType:  "codex",
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":        "gpt-5.2",
		"channel_type": "codex",
		"stream":       false,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("data.success 应为 true, data=%+v", resp.Data)
	}

	responseText, _ := resp.Data["response_text"].(string)
	if responseText == "" {
		t.Fatalf("应解析出 response_text, data=%+v", resp.Data)
	}
	if responseText != "fallback text" {
		t.Fatalf("response_text 解析错误: %q", responseText)
	}

	message, _ := resp.Data["message"].(string)
	if message != "API测试成功" {
		t.Fatalf("应按非流式成功文案返回，实际: %q", message)
	}
}
