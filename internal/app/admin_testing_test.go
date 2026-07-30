package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
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
					URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
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

func TestChannelTestCodexStopsAfterResponseCompleted(t *testing.T) {
	streamBody := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"created_at\":1784768634,\"model\":\"gpt-5.6-sol\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"created_at\":1784768634,\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")

	tests := []struct {
		name           string
		clientProtocol string
		transformMode  string
	}{
		{name: "native", clientProtocol: util.ChannelTypeCodex, transformMode: model.ProtocolTransformModeUpstream},
		{name: "translated", clientProtocol: util.ChannelTypeOpenAI, transformMode: model.ProtocolTransformModeLocal},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newInMemoryServer(t)
			srv.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: &errAfterDataReadCloser{
						data: streamBody,
						err:  errors.New("local error: tls: bad record MAC"),
					},
					Request: req,
				}, nil
			})}

			result := srv.testChannelAPI(context.Background(), &model.Config{
				ID:                    int64(i + 1),
				Name:                  tt.name + "-codex-semantic-completion",
				URLs:                  model.ChannelURLs{{URL: "https://upstream.invalid"}},
				ChannelType:           util.ChannelTypeCodex,
				ProtocolTransformMode: tt.transformMode,
				ModelEntries:          []model.ModelEntry{{Model: "gpt-5.6-sol"}},
			}, "sk-test", &testutil.TestChannelRequest{
				Model:          "gpt-5.6-sol",
				ClientProtocol: tt.clientProtocol,
				Stream:         true,
				Content:        "hello",
			})

			if success, _ := result["success"].(bool); !success {
				t.Fatalf("completed Responses stream must succeed despite trailing TLS error: %+v", result)
			}
			if got, _ := result["response_text"].(string); got != "hello" {
				t.Fatalf("response_text=%q, want hello; result=%+v", got, result)
			}
			if _, hasError := result["error"]; hasError {
				t.Fatalf("completed Responses stream must not expose trailing TLS error: %+v", result)
			}
		})
	}
}

func TestChannelTestCodexUsesNativeWebsocketWhenEnabled(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Fatalf("WebSocket 渠道测试错误地走了 HTTP: %s %s", r.Method, r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("升级 WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, requestBody, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("读取 WebSocket 测试请求: %v", err)
			return
		}
		if got := gjson.GetBytes(requestBody, "type").String(); got != responsesWebsocketRequestCreate {
			t.Errorf("请求 type=%q, want %q", got, responsesWebsocketRequestCreate)
		}
		if !gjson.GetBytes(requestBody, "stream").Bool() {
			t.Error("WebSocket 请求必须强制 stream=true")
		}

		for _, event := range []map[string]any{
			{"type": "response.output_text.delta", "delta": "hello"},
			{
				"type": "response.completed",
				"response": map[string]any{
					"id":     "resp_admin_ws",
					"status": "completed",
					"usage": map[string]any{
						"input_tokens":  3,
						"output_tokens": 1,
					},
				},
			},
		} {
			if err := conn.WriteJSON(event); err != nil {
				t.Errorf("写入 WebSocket 测试响应: %v", err)
				return
			}
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	result := srv.testChannelAPI(context.Background(), &model.Config{
		ID:                    97,
		Name:                  "codex-native-websocket-test",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		ChannelType:           util.ChannelTypeCodex,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		Websockets:            true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.6-sol"}},
	}, "sk-test", &testutil.TestChannelRequest{
		Model:       "gpt-5.6-sol",
		Stream:      true,
		Content:     "hello",
		ChannelType: util.ChannelTypeCodex,
	})

	if success, _ := result["success"].(bool); !success {
		t.Fatalf("原生 WebSocket 渠道测试失败: %+v", result)
	}
	if got, _ := result["transport"].(string); got != "websocket" {
		t.Fatalf("transport=%q, want websocket; result=%+v", got, result)
	}
	if got, _ := result["response_text"].(string); got != "hello" {
		t.Fatalf("response_text=%q, want hello; result=%+v", got, result)
	}
	if got, _ := result["upstream_request_url"].(string); !strings.HasPrefix(got, "ws://") {
		t.Fatalf("upstream_request_url=%q, want ws:// URL", got)
	}
	if got, _ := result["upstream_request_body"].(string); gjson.Get(got, "type").String() != responsesWebsocketRequestCreate {
		t.Fatalf("upstream_request_body 未记录实际 WebSocket 帧: %s", got)
	}
}

func TestChannelTestCodexDoesNotHideRejectedWebsocketHandshake(t *testing.T) {
	var httpFallbacks atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUpgradeRequired)
			_, _ = io.WriteString(w, `{"error":{"message":"websocket disabled"}}`)
			return
		}
		httpFallbacks.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_http","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	result := srv.testChannelAPI(context.Background(), &model.Config{
		ID:                    98,
		Name:                  "codex-rejected-websocket-test",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		ChannelType:           util.ChannelTypeCodex,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		Websockets:            true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.6-sol"}},
	}, "sk-test", &testutil.TestChannelRequest{
		Model:       "gpt-5.6-sol",
		Stream:      true,
		Content:     "hello",
		ChannelType: util.ChannelTypeCodex,
	})

	if success, _ := result["success"].(bool); success {
		t.Fatalf("被拒绝的 WebSocket 握手不得被 HTTP 成功掩盖: %+v", result)
	}
	if got, _ := getResultInt(result["status_code"]); got != http.StatusUpgradeRequired {
		t.Fatalf("status_code=%d, want %d; result=%+v", got, http.StatusUpgradeRequired, result)
	}
	if got := httpFallbacks.Load(); got != 0 {
		t.Fatalf("渠道测试在 WebSocket 握手失败后偷偷回退 HTTP: calls=%d", got)
	}
}

func TestHandleChannelWebsocketProbeDetectsSupportedUpstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Errorf("probe used HTTP instead of WebSocket: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-probe" {
			t.Errorf("Authorization=%q, want bearer probe key", got)
		}
		if beta := r.Header.Get("OpenAI-Beta"); !strings.Contains(beta, "responses_websockets=") {
			t.Errorf("OpenAI-Beta=%q, want responses_websockets feature", beta)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade probe websocket: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/websocket-probe", map[string]any{
		"url":     upstream.URL,
		"api_key": "sk-probe",
	}))

	srv.HandleChannelWebsocketProbe(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	result := mustParseAPIResponse[struct {
		Supported bool `json:"supported"`
	}](t, w.Body.Bytes())
	if !result.Data.Supported {
		t.Fatalf("supported=false, want true; body=%s", w.Body.String())
	}
}

func TestHandleChannelWebsocketProbeRejectsUnsupportedUpstreamWithoutHTTPFallback(t *testing.T) {
	var httpFallbacks atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		httpFallbacks.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/websocket-probe", map[string]any{
		"url":     upstream.URL,
		"api_key": "sk-probe",
	}))

	srv.HandleChannelWebsocketProbe(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	result := mustParseAPIResponse[struct {
		Supported bool `json:"supported"`
		Status    int  `json:"status"`
	}](t, w.Body.Bytes())
	if result.Data.Supported {
		t.Fatalf("supported=true, want false; body=%s", w.Body.String())
	}
	if result.Data.Status != http.StatusUpgradeRequired {
		t.Fatalf("status=%d, want %d; body=%s", result.Data.Status, http.StatusUpgradeRequired, w.Body.String())
	}
	if got := httpFallbacks.Load(); got != 0 {
		t.Fatalf("probe fell back to HTTP: calls=%d", got)
	}
}

func TestTestChannelAPI_MultiURL5xxDoesNotFallbackOrCooldownURL(t *testing.T) {
	failCalls := 0
	okCalls := 0

	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"upstream fail"}}`))
	}))
	defer failUpstream.Close()

	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		URLs:         channelURLsForTest(failUpstream.URL, okUpstream.URL),
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}

	// 强制第一跳命中失败URL，模型级 5xx 不应改打同渠道的第二个 URL。
	srv.urlSelector.CooldownURL(cfg.ID, okUpstream.URL)

	req := &testutil.TestChannelRequest{
		Model:       "gpt-4o-mini",
		ChannelType: "openai",
		Content:     "hello",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	success, _ := result["success"].(bool)
	if success {
		t.Fatalf("expected first 5xx result to be returned, got result=%+v", result)
	}
	if failCalls < 1 || okCalls != 0 {
		t.Fatalf("expected only failing URL attempted, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
	if srv.urlSelector.IsCooledDown(cfg.ID, failUpstream.URL) {
		t.Fatalf("model-scoped 5xx must not cool URL, url=%s", failUpstream.URL)
	}
}

func TestExecuteChannelTestWithCooldown_RespectsRPMLimitWithoutCooldown(t *testing.T) {
	hits := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)

	cfg := &model.Config{
		ID:                    9528,
		Name:                  "rpm-limited-test",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		RPMLimit:              1,
		ChannelType:           "openai",
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:               true,
	}
	req := &testutil.TestChannelRequest{
		Model:       "gpt-4o-mini",
		ChannelType: "openai",
		Content:     "hello",
	}

	first := srv.executeChannelTestWithCooldown(context.Background(), cfg, 0, "sk-test", req, true)
	if success, _ := first["success"].(bool); !success {
		t.Fatalf("first test should succeed, got result=%+v", first)
	}

	second := srv.executeChannelTestWithCooldown(context.Background(), cfg, 0, "sk-test", req, true)
	if success, _ := second["success"].(bool); success {
		t.Fatalf("second test should be RPM limited, got result=%+v", second)
	}
	if limited, _ := second["rpm_limited"].(bool); !limited {
		t.Fatalf("expected rpm_limited marker, got result=%+v", second)
	}
	if action, _ := second["cooldown_action"].(string); action != "rpm_limited_no_cooldown" {
		t.Fatalf("cooldown_action=%q, want rpm_limited_no_cooldown, result=%+v", action, second)
	}
	if retryAfterMs, _ := getResultInt(second["retry_after_ms"]); retryAfterMs <= 0 {
		t.Fatalf("retry_after_ms=%d, want positive value, result=%+v", retryAfterMs, second)
	}
	if hits != 1 {
		t.Fatalf("upstream hits=%d, want 1", hits)
	}
}

func TestExecuteChannelTestWithCooldown_ModelCooldownUsesSentModelKey(t *testing.T) {
	const sentModel = "model-c"

	var upstreamModel string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		} else {
			upstreamModel, _ = body["model"].(string)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"model_cooldown","message":"model temporarily unavailable","model":"model-c","reset_seconds":300}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()
	cfg, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:        "model-cooldown-sent-key-test",
		URLs:        model.ChannelURLs{{URL: upstream.URL}},
		Priority:    1,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "model-a", RedirectModel: "model-b"},
			{Model: "model-b", RedirectModel: sentModel},
			{Model: sentModel, RedirectModel: "model-d"},
		},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	req := &testutil.TestChannelRequest{
		Model:       "model-a",
		ChannelType: "openai",
		Content:     "hello",
	}
	result := srv.executeChannelTestWithCooldown(ctx, cfg, 0, "sk-test", req, true)
	if action, _ := result["cooldown_action"].(string); action != "model_cooldown_applied" {
		t.Fatalf("cooldown_action=%q, want model_cooldown_applied; result=%+v", action, result)
	}
	if upstreamModel != sentModel {
		t.Fatalf("upstream model=%q, want %q", upstreamModel, sentModel)
	}

	cooldowns, err := srv.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("get model cooldowns: %v", err)
	}
	if until := cooldowns[cfg.ID][sentModel]; !until.After(time.Now()) {
		t.Fatalf("sent model cooldown=%s, want active cooldown", until.Format(time.RFC3339))
	}
	if _, exists := cooldowns[cfg.ID]["model-d"]; exists {
		t.Fatal("model cooldown must not be re-resolved after the request")
	}
}

func TestTestChannelAPI_MultiURLPlainText502DoesNotFallback(t *testing.T) {
	failCalls := 0
	okCalls := 0

	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("error code: 502"))
	}))
	defer failUpstream.Close()

	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		URLs:         channelURLsForTest(failUpstream.URL, okUpstream.URL),
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}

	// 强制第一跳命中 502 的坏 URL，text/plain 错误体也必须保持模型级语义。
	srv.urlSelector.CooldownURL(cfg.ID, okUpstream.URL)

	req := &testutil.TestChannelRequest{
		Model:       "gpt-4o-mini",
		ChannelType: "openai",
		Content:     "hello",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	success, _ := result["success"].(bool)
	if success {
		t.Fatalf("expected plain 502 failure, got result=%+v", result)
	}
	if failCalls < 1 || okCalls != 0 {
		t.Fatalf("expected only failing URL attempted, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
	if srv.urlSelector.IsCooledDown(cfg.ID, failUpstream.URL) {
		t.Fatalf("model-scoped 502 must not cool URL, url=%s", failUpstream.URL)
	}
}

func TestTestChannelAPI_NonStreamUsesConfiguredTimeout(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(160 * time.Millisecond):
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"late"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.nonStreamTimeout = 25 * time.Millisecond

	cfg := &model.Config{
		ID:           9530,
		Name:         "non-stream-timeout-test",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	req := &testutil.TestChannelRequest{
		Model:       "gpt-4o-mini",
		ChannelType: "openai",
		Content:     "hello",
		Stream:      false,
	}

	start := time.Now()
	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	elapsed := time.Since(start)

	if success, _ := result["success"].(bool); success {
		t.Fatalf("expected timeout failure, got result=%+v", result)
	}
	if elapsed >= 120*time.Millisecond {
		t.Fatalf("expected configured timeout before delayed upstream response, elapsed=%v result=%+v", elapsed, result)
	}
	if statusCode, _ := getResultInt(result["status_code"]); statusCode != http.StatusGatewayTimeout {
		t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, http.StatusGatewayTimeout, result)
	}
}

func TestTestChannelAPI_StreamFirstValidContentTimeoutIgnoresHeartbeats(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		lateContent := time.NewTimer(500 * time.Millisecond)
		defer lateContent.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte(": keep-alive\n\n"))
				flusher.Flush()
			case <-lateContent.C:
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				flusher.Flush()
				return
			}
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.firstByteTimeout = 30 * time.Millisecond

	cfg := &model.Config{
		ID:           9531,
		Name:         "stream-first-content-timeout-test",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	req := &testutil.TestChannelRequest{
		Model:       "gpt-4o-mini",
		ChannelType: "openai",
		Content:     "hello",
		Stream:      true,
	}

	start := time.Now()
	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	elapsed := time.Since(start)

	if success, _ := result["success"].(bool); success {
		t.Fatalf("expected first valid stream content timeout, got result=%+v", result)
	}
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("expected timeout before late content, elapsed=%v result=%+v", elapsed, result)
	}
	if statusCode, _ := getResultInt(result["status_code"]); statusCode != util.StatusFirstByteTimeout {
		t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, util.StatusFirstByteTimeout, result)
	}
	if _, ok := result["first_byte_duration_ms"]; ok {
		t.Fatalf("heartbeat must not set first_byte_duration_ms, result=%+v", result)
	}
}

func TestTestChannelAPI_StreamFirstValidContentTimeoutEOFReturns598(t *testing.T) {
	srv := newInMemoryServer(t)
	srv.firstByteTimeout = 10 * time.Millisecond
	srv.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &heartbeatThenContextEOFBody{ctx: req.Context()},
			Request:    req,
		}, nil
	})}

	result := srv.testChannelAPI(context.Background(), &model.Config{
		ID:           9532,
		Name:         "stream-first-content-timeout-eof-test",
		URLs:         model.ChannelURLs{{URL: "http://test-upstream.invalid"}},
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}, "sk-test", &testutil.TestChannelRequest{
		Model:       "gpt-4o-mini",
		ChannelType: "openai",
		Content:     "hello",
		Stream:      true,
	})

	if statusCode, _ := getResultInt(result["status_code"]); statusCode != util.StatusFirstByteTimeout {
		t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, util.StatusFirstByteTimeout, result)
	}
}

type heartbeatThenContextEOFBody struct {
	ctx       context.Context
	heartbeat bool
}

func (b *heartbeatThenContextEOFBody) Read(p []byte) (int, error) {
	if !b.heartbeat {
		b.heartbeat = true
		return copy(p, ": keep-alive\n\n"), nil
	}
	<-b.ctx.Done()
	return 0, io.EOF
}

func (b *heartbeatThenContextEOFBody) Close() error {
	return nil
}

func TestHandleChannelTest_InvalidRequestDoesNotLeakDecoderError(t *testing.T) {
	srv := newInMemoryServer(t)

	c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/1/test", []byte(`{"model":123,"channel_type":"anthropic"}`)))
	c.Params = gin.Params{{Key: "id", Value: "1"}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
	if resp.Error != "invalid request" {
		t.Fatalf("error=%q, want generic invalid request", resp.Error)
	}
	if strings.Contains(resp.Error, "unmarshal") || strings.Contains(resp.Error, "TestChannelRequest") {
		t.Fatalf("decoder detail leaked in response: %q", resp.Error)
	}
}

func TestHandleChannelTest_RejectsBaseURL(t *testing.T) {
	failCalls := 0
	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failUpstream.Close()

	okCalls := 0
	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	cfg := &model.Config{
		Name:         "channel-test-reject-base-url",
		URLs:         channelURLsForTest(failUpstream.URL, okUpstream.URL),
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
	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"should not hit this url"}}`))
	}))
	defer failUpstream.Close()

	okCalls := 0
	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong protocol path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	cfg := &model.Config{
		Name:         "single-url-test",
		URLs:         model.ChannelURLs{{URL: failUpstream.URL, Protocols: []string{"anthropic"}}, {URL: okUpstream.URL, Protocols: []string{"openai"}}},
		Priority:     1,
		ChannelType:  "anthropic",
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
		URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
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
		URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
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

func TestHandleChannelTest_DefaultsClientProtocolToChannelType(t *testing.T) {
	var gotPath string

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "default-protocol-transform-openai",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ChannelType:           "openai",
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4.1"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model": "gpt-4.1",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path=%q, want %q", gotPath, "/v1/chat/completions")
	}
}

func TestHandleChannelTest_RejectsUnknownClientProtocol(t *testing.T) {
	failCalls := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "unsupported-protocol-transform-openai",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4.1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "gpt-4.1",
		"client_protocol": "unknown",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatalf("expected data.success=false, data=%+v", resp.Data)
	}
	dataError, _ := resp.Data["error"].(string)
	if !strings.Contains(dataError, "协议") {
		t.Fatalf("expected protocol error, got %q", dataError)
	}
	if failCalls != 0 {
		t.Fatalf("expected no upstream request, failCalls=%d", failCalls)
	}
}

func TestHandleChannelTest_UsesSelectedOpenAIProtocol(t *testing.T) {
	var gotPath string
	var gotBody string

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPath = r.URL.Path
		gotBody = string(body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl_test",
			"object": "chat.completion",
			"choices": [{"message": {"role": "assistant", "content": "native openai ok"}}],
			"model": "claude-3-5-sonnet",
			"usage": {"prompt_tokens": 10, "completion_tokens": 5}
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "anthropic-with-runtime-openai-transform",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ChannelType:           "anthropic",
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "openai",
		"content":         "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path=%q, want %q", gotPath, "/v1/chat/completions")
	}
	if !strings.Contains(gotBody, `"messages"`) {
		t.Fatalf("expected openai request body, body=%s", gotBody)
	}

	apiResp, ok := resp.Data["api_response"].(map[string]any)
	if !ok {
		t.Fatalf("expected translated api_response map, data=%+v", resp.Data)
	}
	if _, ok := apiResp["choices"]; !ok {
		t.Fatalf("expected openai-compatible api_response, got=%+v", apiResp)
	}
}

func TestHandleChannelTest_UsesSelectedCodexProtocolWithBasePathPrefix(t *testing.T) {
	var gotPath string
	var gotBody string

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPath = r.URL.Path
		gotBody = string(body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "resp_test",
			"object": "response",
			"status": "completed",
			"output": [{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "native codex ok"}]}],
			"model": "claude-3-5-sonnet",
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "anthropic-with-prefixed-base-path",
		URLs:                  model.ChannelURLs{{URL: upstream.URL + "/anthropic"}},
		Priority:              1,
		ChannelType:           "anthropic",
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "codex",
		"content":         "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if gotPath != "/anthropic/v1/responses" {
		t.Fatalf("path=%q, want %q", gotPath, "/anthropic/v1/responses")
	}
	if !strings.Contains(gotBody, `"input"`) {
		t.Fatalf("expected codex request body, body=%s", gotBody)
	}

	apiResp, ok := resp.Data["api_response"].(map[string]any)
	if !ok {
		t.Fatalf("expected translated api_response map, data=%+v", resp.Data)
	}
	if _, ok := apiResp["object"]; !ok {
		t.Fatalf("expected codex-compatible api_response, got=%+v", apiResp)
	}
}

func TestHandleChannelTest_UsesSelectedProtocolEndpoint(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"endpoint not found"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_test","object":"response","status":"completed","model":"claude-3-5-sonnet","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "codex-upstream",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ChannelType:           "codex",
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "anthropic",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if !reflect.DeepEqual(gotPaths, []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"}) {
		t.Fatalf("paths=%v, want Anthropic, OpenAI, then Codex", gotPaths)
	}
	var nativeRequest map[string]any
	if err := json.Unmarshal(gotBodies[0], &nativeRequest); err != nil {
		t.Fatalf("decode native request: %v", err)
	}
	if _, ok := nativeRequest["messages"].([]any); !ok {
		t.Fatalf("expected anthropic messages array, body=%s", gotBodies[0])
	}
	if stream, _ := nativeRequest["stream"].(bool); !stream {
		t.Fatalf("expected stream=true, body=%s", gotBodies[0])
	}
	if _, ok := nativeRequest["max_tokens"]; !ok {
		t.Fatalf("expected anthropic max_tokens, body=%s", gotBodies[0])
	}
	var openAIRequest map[string]any
	if err := json.Unmarshal(gotBodies[1], &openAIRequest); err != nil {
		t.Fatalf("decode OpenAI request: %v", err)
	}
	if _, ok := openAIRequest["messages"].([]any); !ok {
		t.Fatalf("expected OpenAI messages array, body=%s", gotBodies[1])
	}
	var codexRequest map[string]any
	if err := json.Unmarshal(gotBodies[2], &codexRequest); err != nil {
		t.Fatalf("decode Codex request: %v", err)
	}
	if _, ok := codexRequest["input"].([]any); !ok {
		t.Fatalf("expected Codex input array, body=%s", gotBodies[2])
	}

	apiResp, ok := resp.Data["api_response"].(map[string]any)
	if !ok {
		t.Fatalf("expected translated api_response map, data=%+v", resp.Data)
	}
	if apiResp["type"] != "message" {
		t.Fatalf("expected anthropic api_response, got=%+v", apiResp)
	}
}

func TestHandleChannelTest_AutoFallsBackOnNonModelDeployment404(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404: Not Found (DEPLOYMENT_NOT_FOUND)\n\nThe requested deployment does not exist."))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "openai-auto",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ChannelType:           "openai",
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "claude-4.5-haiku"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-4.5-haiku",
		"client_protocol": "codex",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !reflect.DeepEqual(gotPaths, []string{
		"/v1/messages",
		"/v1/chat/completions",
		"/v1/responses",
		"/v1beta/models/claude-4.5-haiku:streamGenerateContent",
	}) {
		t.Fatalf("paths=%v, want all protocols in fixed order", gotPaths)
	}
	if len(gotBodies) != 4 {
		t.Fatalf("request count=%d, want 4", len(gotBodies))
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if got, _ := resp.Data["upstream_protocol"].(string); got != "gemini" {
		t.Fatalf("upstream_protocol=%q, want gemini, data=%+v", got, resp.Data)
	}
	if got, _ := resp.Data["cooldown_action"].(string); got != "channel_cooldown_applied" {
		t.Fatalf("cooldown_action=%q, want channel_cooldown_applied, data=%+v", got, resp.Data)
	}
}

func TestHandleChannelTest_AutoFallsBackOnCloudflareBlockPage(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)

		if r.URL.Path == "/v1/messages" {
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.Header().Set("Server", "cloudflare")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Attention Required! | Cloudflare</title></head><body><h1>Sorry, you have been blocked</h1><p>Cloudflare Ray ID: test</p></body></html>`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "openai-auto-cloudflare-block",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ChannelType:           "openai",
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "test-model"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "test-model",
		"client_protocol": "gemini",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !reflect.DeepEqual(gotPaths, []string{"/v1/messages", "/v1/chat/completions"}) {
		t.Fatalf("paths=%v, want Anthropic then OpenAI", gotPaths)
	}
	if !gjson.GetBytes(gotBodies[0], "messages").IsArray() {
		t.Fatalf("Anthropic request must use messages: %s", gotBodies[0])
	}
	if !gjson.GetBytes(gotBodies[1], "messages").IsArray() {
		t.Fatalf("fallback request must use OpenAI messages: %s", gotBodies[1])
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if got, _ := resp.Data["upstream_protocol"].(string); got != "openai" {
		t.Fatalf("upstream_protocol=%q, want openai, data=%+v", got, resp.Data)
	}
}

func TestHandleChannelTest_AutoTriesProtocolsInFixedOrder(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, ":generateContent") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"test-model"}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "auto-fixed-protocol-order",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ChannelType:           "gemini",
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "test-model"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "test-model",
		"client_protocol": "openai",
		"content":         "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !reflect.DeepEqual(gotPaths, []string{
		"/v1/messages",
		"/v1/chat/completions",
		"/v1/responses",
		"/v1beta/models/test-model:generateContent",
	}) {
		t.Fatalf("paths=%v, want Anthropic, OpenAI, Codex, Gemini", gotPaths)
	}
	if !gjson.GetBytes(gotBodies[0], "messages").IsArray() {
		t.Fatalf("Anthropic request must contain messages: %s", gotBodies[0])
	}
	if !gjson.GetBytes(gotBodies[1], "messages").IsArray() {
		t.Fatalf("OpenAI request must contain messages: %s", gotBodies[1])
	}
	if !gjson.GetBytes(gotBodies[2], "input").IsArray() {
		t.Fatalf("Codex request must contain input: %s", gotBodies[2])
	}
	if !gjson.GetBytes(gotBodies[3], "contents").IsArray() {
		t.Fatalf("Gemini request must contain contents: %s", gotBodies[3])
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if got, _ := resp.Data["upstream_protocol"].(string); got != "gemini" {
		t.Fatalf("upstream_protocol=%q, want gemini, data=%+v", got, resp.Data)
	}
}

func TestHandleChannelTest_AutoFallsBackOnConvertRequestNotImplemented(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/messages" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"not implemented (request id: req_test)","type":"new_api_error","param":"","code":"convert_request_failed"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"claude-4.5-haiku","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "openai-auto-convert-not-implemented",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ChannelType:           "openai",
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "claude-4.5-haiku"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-4.5-haiku",
		"client_protocol": "codex",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if !reflect.DeepEqual(gotPaths, []string{"/v1/messages", "/v1/chat/completions"}) {
		t.Fatalf("paths=%v, want Anthropic then OpenAI", gotPaths)
	}
	if len(gotBodies) != 2 {
		t.Fatalf("request count=%d, want 2", len(gotBodies))
	}
	if !gjson.GetBytes(gotBodies[0], "messages").IsArray() {
		t.Fatalf("Anthropic request must use messages: %s", gotBodies[0])
	}
	if !gjson.GetBytes(gotBodies[1], "messages").IsArray() {
		t.Fatalf("fallback request must use OpenAI messages: %s", gotBodies[1])
	}
	if got, _ := resp.Data["upstream_protocol"].(string); got != "openai" {
		t.Fatalf("upstream_protocol=%q, want openai, data=%+v", got, resp.Data)
	}
}

func TestChannelTest_StrictProtocolTransformModes(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		wantPath    string
		wantSuccess bool
	}{
		{name: "upstream", mode: model.ProtocolTransformModeUpstream, wantPath: "/v1/messages"},
		{name: "local", mode: model.ProtocolTransformModeLocal, wantPath: "/v1/responses", wantSuccess: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			var bodies [][]byte
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("ReadAll: %v", err)
				}
				paths = append(paths, r.URL.Path)
				bodies = append(bodies, body)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/messages" {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":{"message":"endpoint not found"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"resp_test","object":"response","status":"completed","model":"test-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer upstream.Close()

			srv := newInMemoryServer(t)
			srv.client = upstream.Client()
			result := srv.testChannelAPI(context.Background(), &model.Config{
				ID: 1, Name: "strict", URLs: model.ChannelURLs{{URL: upstream.URL}}, ChannelType: "codex",
				ProtocolTransformMode: tt.mode,
				ModelEntries:          []model.ModelEntry{{Model: "test-model"}},
			}, "sk-test", &testutil.TestChannelRequest{
				Model: "test-model", ClientProtocol: "anthropic", Content: "hello",
			})

			if success, _ := result["success"].(bool); success != tt.wantSuccess {
				t.Fatalf("success=%v, want %v, result=%+v", success, tt.wantSuccess, result)
			}
			if !reflect.DeepEqual(paths, []string{tt.wantPath}) {
				t.Fatalf("paths=%v, want [%s]", paths, tt.wantPath)
			}
			var requestBody map[string]any
			if err := json.Unmarshal(bodies[0], &requestBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if tt.mode == model.ProtocolTransformModeLocal {
				if _, ok := requestBody["input"].([]any); !ok {
					t.Fatalf("local request must use Codex input: %+v", requestBody)
				}
			} else if _, ok := requestBody["messages"].([]any); !ok {
				t.Fatalf("upstream request must use Anthropic messages: %+v", requestBody)
			}
		})
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
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
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

func TestHandleChannelTest_OpenAIRequestIncludesSessionID(t *testing.T) {
	var gotSessionID string
	var gotBody []byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSessionID = r.Header.Get("Session_id")
		if got := r.Header.Get("Session-Id"); got != "" {
			t.Fatalf("Session-Id header should be omitted, got %q", got)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl_test",
			"object": "chat.completion",
			"model": "gpt-test",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "openai-test-session-id",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ChannelType:           "openai",
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-test"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":        "gpt-test",
		"channel_type": "openai",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}
	if !uuidPattern.MatchString(gotSessionID) {
		t.Fatalf("Session_id header missing or invalid: %q", gotSessionID)
	}
	var upstreamBody map[string]any
	if err := json.Unmarshal(gotBody, &upstreamBody); err != nil {
		t.Fatalf("unmarshal upstream body failed: %v; body=%s", err, gotBody)
	}
	if got, _ := upstreamBody["user"].(string); got != gotSessionID {
		t.Fatalf("body user = %q, want session id %q; body=%s", got, gotSessionID, gotBody)
	}
	if got, _ := upstreamBody["prompt_cache_key"].(string); got != gotSessionID {
		t.Fatalf("body prompt_cache_key = %q, want session id %q; body=%s", got, gotSessionID, gotBody)
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("data.success 应为 true, data=%+v", resp.Data)
	}
}

// TestHandleChannelTest_FailedAPI 使用 mock server 模拟失败的 API 调用
func TestHandleChannelTest_FailedAPI(t *testing.T) {
	// 创建 mock 上游服务器，返回 401 错误
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
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

func TestHandleChannelTest_HonorsRequestedKeyIndexEvenIfCooled(t *testing.T) {
	mockResp := `{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello"}],
		"model": "claude-3-5-sonnet",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`
	var gotAuth string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResp))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "test-honor-cooled-key-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ChannelType:  "anthropic",
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-cooled"},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-fresh"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	if err := srv.store.SetKeyCooldown(ctx, created.ID, 0, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":        "claude-3-5-sonnet",
		"channel_type": "anthropic",
		"key_index":    0,
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if dataSuccess, _ := resp.Data["success"].(bool); !dataSuccess {
		t.Fatalf("data.success=false, data=%+v", resp.Data)
	}
	if gotAuth != "Bearer sk-cooled" {
		t.Fatalf("Authorization=%q, want Bearer sk-cooled (requested key must be honored even if cooled)", gotAuth)
	}
	if gotIndex, _ := resp.Data["tested_key_index"].(float64); gotIndex != 0 {
		t.Fatalf("tested_key_index=%v, want 0", resp.Data["tested_key_index"])
	}
}

// TestHandleChannelTest_RejectsUnknownKeyIndex 验证：请求一个不存在的 key_index 时直接报错，
// 不再静默回退到其他可用 Key（既往会调用 SelectAvailableKey）。配合 HonorsRequestedKeyIndexEvenIfCooled
// 共同保证"显式 key_index 即真"语义。
func TestHandleChannelTest_RejectsUnknownKeyIndex(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "test-reject-unknown-key-channel",
		URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority:     1,
		ChannelType:  "anthropic",
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-only"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":        "claude-3-5-sonnet",
		"channel_type": "anthropic",
		"key_index":    99, // 不存在
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatalf("data.success=true, want false; data=%+v", resp.Data)
	}
	dataError, _ := resp.Data["error"].(string)
	if !strings.Contains(dataError, "Key #99") {
		t.Fatalf("data.error=%q, want mention of Key #99", dataError)
	}
}

func TestHandleChannelTest_UsesRequestAPIKeyWithoutTouchingSavedCooldown(t *testing.T) {
	mockResp := `{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello"}],
		"model": "claude-3-5-sonnet",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`
	var gotAuth string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResp))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "test-request-key-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ChannelType:  "anthropic",
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-saved-key"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	coolUntil := time.Now().Add(10 * time.Minute)
	if err := srv.store.SetKeyCooldown(ctx, created.ID, 0, coolUntil); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":        "claude-3-5-sonnet",
		"channel_type": "anthropic",
		"key_index":    1,
		"api_key":      "sk-unsaved-key",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if dataSuccess, _ := resp.Data["success"].(bool); !dataSuccess {
		t.Fatalf("data.success=false, data=%+v", resp.Data)
	}
	if gotAuth != "Bearer sk-unsaved-key" {
		t.Fatalf("Authorization=%q, want request api key", gotAuth)
	}

	keys, err := srv.store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys failed: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys len=%d, want 1", len(keys))
	}
	if keys[0].CooldownUntil == 0 {
		t.Fatalf("saved key cooldown was reset for an unsaved request key")
	}
}

func TestHandleChannelTest_WritesManualTestLog(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	now := time.Now().Add(-time.Minute)

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "manual-test-log-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ChannelType:  "anthropic",
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-invalid-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":        "claude-3-5-sonnet",
		"channel_type": "anthropic",
	}))
	c.Request.RemoteAddr = "198.51.100.10:12345"
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	logs, err := srv.store.ListLogs(ctx, now, 10, 0, &model.LogFilter{LogSource: model.LogSourceManualTest})
	if err != nil {
		t.Fatalf("ListLogs failed: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 manual test log, got %d", len(logs))
	}
	entry := logs[0]
	if entry.LogSource != model.LogSourceManualTest {
		t.Fatalf("LogSource=%q, want %q", entry.LogSource, model.LogSourceManualTest)
	}
	if entry.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode=%d, want %d", entry.StatusCode, http.StatusUnauthorized)
	}
	if entry.ClientIP != "198.51.100.10" {
		t.Fatalf("ClientIP=%q, want %q", entry.ClientIP, "198.51.100.10")
	}
	if entry.AuthTokenID != 0 {
		t.Fatalf("AuthTokenID=%d, want 0", entry.AuthTokenID)
	}
	if entry.BaseURL != upstream.URL {
		t.Fatalf("BaseURL=%q, want %q", entry.BaseURL, upstream.URL)
	}
}

func TestHandleChannelTest_SSESoftErrorTriggersCooldown(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: \n")
		_, _ = fmt.Fprint(w, "data: {\"error\":{\"code\":\"1113\",\"message\":\"Insufficient balance or no resource package. Please recharge.\"},\"request_id\":\"req_1113\"}\n\n")
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:         "test-sse-soft-error",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
		CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
			Enabled: true, Name: "HTTP 200 soft error", Priority: 0, StatusCodes: []int{http.StatusOK},
			MessagePattern: "Insufficient balance", Scope: model.CooldownScopeChannel,
			Mode: model.CooldownModeFixed, CooldownSeconds: 90,
		}}},
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-soft-error"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":        "claude-3-5-sonnet",
		"channel_type": "anthropic",
		"stream":       true,
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
	if dataSuccess {
		t.Fatalf("data.success 应为 false, data=%+v", resp.Data)
	}

	if got, _ := resp.Data["error"].(string); got != "Insufficient balance or no resource package. Please recharge." {
		t.Fatalf("错误信息不对，got=%q data=%+v", got, resp.Data)
	}

	if got, _ := resp.Data["cooldown_action"].(string); got != "channel_cooldown_applied" {
		t.Fatalf("1113 软错误在单 Key 渠道应升级为渠道冷却，got=%q data=%+v", got, resp.Data)
	}

	cooldowns, err := srv.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	until, exists := cooldowns[created.ID]
	if !exists {
		t.Fatalf("HTTP 200 自定义规则未写入渠道冷却")
	}
	if remaining := time.Until(until); remaining < 85*time.Second || remaining > 95*time.Second {
		t.Fatalf("渠道冷却剩余时间=%v，期望约 90 秒", remaining)
	}
}

func TestHandleChannelTest_EventStreamHeaderWithJSONBodyFallback(t *testing.T) {
	// 模拟“Content-Type=event-stream，但实际返回完整JSON”场景
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		Name:                  "test-codex-json-fallback",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.2"}},
		Enabled:               true,
		ChannelType:           "codex",
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
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

func TestHandleChannelTest_CodexJSONFailedResponseShouldBeFailure(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id":"resp_failed",
			"object":"response",
			"status":"failed",
			"error":{
				"code":"server_error",
				"message":"upstream failed"
			},
			"output":[]
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:                  "test-codex-json-failed",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.4"}},
		Enabled:               true,
		ChannelType:           "codex",
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
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
		"model":        "gpt-5.4",
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
	if dataSuccess {
		t.Fatalf("data.success 应为 false, data=%+v", resp.Data)
	}

	errorMsg, _ := resp.Data["error"].(string)
	if errorMsg != "upstream failed" {
		t.Fatalf("应返回上游错误信息，实际: %q, data=%+v", errorMsg, resp.Data)
	}

	if message, _ := resp.Data["message"].(string); message != "" {
		t.Fatalf("失败响应不应返回成功文案，实际: %q", message)
	}
}

func TestHandleChannelTest_StringAPIErrorShouldExposeUpstreamMessage(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{
			"error":"由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试",
			"type":"error"
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:         "test-string-api-error",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.4"}},
		Enabled:      true,
		ChannelType:  "openai",
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
		"model":        "gpt-5.4",
		"channel_type": "openai",
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
	if dataSuccess {
		t.Fatalf("data.success 应为 false, data=%+v", resp.Data)
	}

	errorMsg, _ := resp.Data["error"].(string)
	expected := "由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试"
	if errorMsg != expected {
		t.Fatalf("应返回上游字符串错误信息，实际: %q, data=%+v", errorMsg, resp.Data)
	}
}

func TestHandleChannelTest_HTMLBlockPageShouldBeFailure(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
	<meta charset="UTF-8">
	<title>您的IP已被封锁</title>
</head>
<body>
	<div class="container">
		<h1>当前 IP 已被封锁</h1>
		<p>暂时无法访问本站内容。</p>
	</div>
</body>
</html>`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:                  "test-html-block-page",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.4"}},
		Enabled:               true,
		ChannelType:           "openai",
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
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
		"model":        "gpt-5.4",
		"channel_type": "openai",
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
	if dataSuccess {
		t.Fatalf("HTML 封禁页必须判定为失败, data=%+v", resp.Data)
	}

	errorMsg, _ := resp.Data["error"].(string)
	if !strings.Contains(errorMsg, "IP") || !strings.Contains(errorMsg, "封锁") {
		t.Fatalf("应提炼出上游封禁信息，实际: %q, data=%+v", errorMsg, resp.Data)
	}

	rawResp, _ := resp.Data["raw_response"].(string)
	if !strings.Contains(rawResp, "<title>您的IP已被封锁</title>") {
		t.Fatalf("应保留原始 HTML 响应，实际: %q", rawResp)
	}

	if message, _ := resp.Data["message"].(string); message != "" {
		t.Fatalf("失败响应不应返回成功文案，实际: %q", message)
	}
}

func TestShouldFallbackToNextURL_StructuredSoftErrors(t *testing.T) {
	t.Run("key_level_soft_error_should_not_fallback_or_cooldown_url", func(t *testing.T) {
		result := map[string]any{
			"success":     false,
			"status_code": http.StatusOK,
			"api_error": map[string]any{
				"error": map[string]any{
					"code":    "1113",
					"message": "Insufficient balance or no resource package. Please recharge.",
				},
			},
			"response_headers": map[string]string{
				"Content-Type": "text/event-stream",
			},
		}

		continueFallback, shouldCooldown := shouldFallbackToNextURL(result)
		if continueFallback || shouldCooldown {
			t.Fatalf("Key级软错误不应继续切URL或冷却URL，got fallback=%v cooldown=%v", continueFallback, shouldCooldown)
		}
	})

	t.Run("channel_level_soft_error_should_fallback_and_cooldown_url", func(t *testing.T) {
		result := map[string]any{
			"success":     false,
			"status_code": http.StatusOK,
			"api_error": map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": "upstream overloaded",
				},
			},
		}

		continueFallback, shouldCooldown := shouldFallbackToNextURL(result)
		if !continueFallback || !shouldCooldown {
			t.Fatalf("渠道级软错误应继续切URL并冷却当前URL，got fallback=%v cooldown=%v", continueFallback, shouldCooldown)
		}
	})
}

func TestExtractSSEErrorMessage_ResponseFailedNestedError(t *testing.T) {
	obj := map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id":     "resp_5ca0fb7943504d6a93576c7fb7e3a760",
			"object": "response",
			"model":  "gpt-5.6-sol",
			"status": "failed",
			"output": []any{},
			"error": map[string]any{
				"code":    "rate_limit_exceeded",
				"message": "Upstream rate limit exceeded, please retry later",
			},
		},
	}
	msg, raw, matched := extractSSEErrorMessage(obj)
	if !matched {
		t.Fatal("response.failed nested error must match")
	}
	if msg != "Upstream rate limit exceeded, please retry later" {
		t.Fatalf("msg=%q, want nested error message", msg)
	}
	if raw == nil {
		t.Fatal("raw payload must be returned")
	}
}
