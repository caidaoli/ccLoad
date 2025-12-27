package app

import (
	"bytes"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/util"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestHandleProxyRequest_UnknownPathReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := &Server{
		concurrencySem: make(chan struct{}, 1),
	}

	body := bytes.NewBufferString(`{"model":"gpt-4"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/unknown", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	srv.HandleProxyRequest(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("预期状态码404，实际%d", w.Code)
	}

	if body := w.Body.String(); !bytes.Contains([]byte(body), []byte("unsupported path")) {
		t.Fatalf("响应内容缺少错误信息，实际: %s", body)
	}
}

// ============================================================================
// 增加proxy_handler测试覆盖率
// ============================================================================

// TestParseIncomingRequest_ValidJSON 测试有效JSON解析
func TestParseIncomingRequest_ValidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		body         string
		path         string
		expectModel  string
		expectStream bool
		expectError  bool
	}{
		{
			name:         "有效JSON-claude模型",
			body:         `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hello"}]}`,
			path:         "/v1/messages",
			expectModel:  "claude-3-5-sonnet-20241022",
			expectStream: false,
			expectError:  false,
		},
		{
			name:         "流式请求-stream=true",
			body:         `{"model":"gpt-4","stream":true,"messages":[]}`,
			path:         "/v1/chat/completions",
			expectModel:  "gpt-4",
			expectStream: true,
			expectError:  false,
		},
		{
			name:         "空模型名-从路径提取",
			body:         `{"messages":[{"role":"user","content":"test"}]}`,
			path:         "/v1/models/gpt-4/completions",
			expectModel:  "gpt-4",
			expectStream: false,
			expectError:  false,
		},
		{
			name:         "GET请求-无模型使用通配符",
			body:         "",
			path:         "/v1/models",
			expectModel:  "*",
			expectStream: false,
			expectError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := bytes.NewBufferString(tt.body)
			req := httptest.NewRequest(http.MethodPost, tt.path, body)
			if tt.body == "" {
				req.Method = http.MethodGet
			}
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			model, _, isStreaming, err := parseIncomingRequest(c)

			if tt.expectError && err == nil {
				t.Errorf("期望错误但未发生")
			}
			if !tt.expectError && err != nil {
				t.Errorf("不期望错误但发生: %v", err)
			}
			if model != tt.expectModel {
				t.Errorf("模型名错误: 期望%s, 实际%s", tt.expectModel, model)
			}
			if isStreaming != tt.expectStream {
				t.Errorf("流式标志错误: 期望%v, 实际%v", tt.expectStream, isStreaming)
			}
		})
	}
}

// TestParseIncomingRequest_BodyTooLarge 测试请求体过大
func TestParseIncomingRequest_BodyTooLarge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 创建超大请求体（>2MB）
	largeBody := make([]byte, 3*1024*1024) // 3MB
	for i := range largeBody {
		largeBody[i] = 'a'
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(largeBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	_, _, _, err := parseIncomingRequest(c)

	if err != errBodyTooLarge {
		t.Errorf("期望errBodyTooLarge错误, 实际: %v", err)
	}
}

// TestAcquireConcurrencySlot 测试并发槽位获取
func TestAcquireConcurrencySlot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := &Server{
		concurrencySem: make(chan struct{}, 2), // 最大并发数=2
		maxConcurrency: 2,
	}

	// 创建有效的gin.Context
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	// 第一次获取应该成功
	release1, acquired1 := srv.acquireConcurrencySlot(c)
	if !acquired1 {
		t.Fatal("第一次获取应该成功")
	}

	// 第二次获取应该成功
	release2, acquired2 := srv.acquireConcurrencySlot(c)
	if !acquired2 {
		t.Fatal("第二次获取应该成功")
	}

	// 释放一个槽位
	release1()

	// 现在应该可以再次获取
	release3, acquired3 := srv.acquireConcurrencySlot(c)
	if !acquired3 {
		t.Fatal("释放后再次获取应该成功")
	}

	// 清理
	release2()
	release3()

	t.Log("[INFO] 并发控制测试通过：2个槽位正确管理")
}

func TestAcquireConcurrencySlot_ContextCanceled_Returns499(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := &Server{
		concurrencySem: make(chan struct{}, 1),
	}
	srv.concurrencySem <- struct{}{} // 填满槽位，迫使走等待分支

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	release, acquired := srv.acquireConcurrencySlot(c)
	if acquired || release != nil {
		t.Fatal("预期获取失败且release=nil")
	}
	if w.Code != StatusClientClosedRequest {
		t.Fatalf("预期状态码%d，实际%d", StatusClientClosedRequest, w.Code)
	}
}

func TestAcquireConcurrencySlot_DeadlineExceeded_Returns504(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := &Server{
		concurrencySem: make(chan struct{}, 1),
	}
	srv.concurrencySem <- struct{}{} // 填满槽位，迫使走等待分支

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	release, acquired := srv.acquireConcurrencySlot(c)
	if acquired || release != nil {
		t.Fatal("预期获取失败且release=nil")
	}
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("预期状态码%d，实际%d", http.StatusGatewayTimeout, w.Code)
	}
}

func TestDetermineFinalClientStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		last     *proxyResult
		expected int
	}{
		{"nil last => 503", nil, http.StatusServiceUnavailable},
		{"status 0 => 503", &proxyResult{status: 0}, http.StatusServiceUnavailable},
		{"negative => 502", &proxyResult{status: -1, nextAction: cooldown.ActionRetryChannel}, http.StatusBadGateway},
		{"596 => 429", &proxyResult{status: util.StatusQuotaExceeded, nextAction: cooldown.ActionRetryKey}, http.StatusTooManyRequests},
		{"597 => 502", &proxyResult{status: util.StatusSSEError, nextAction: cooldown.ActionRetryKey}, http.StatusBadGateway},
		{"598 => 504", &proxyResult{status: util.StatusFirstByteTimeout, nextAction: cooldown.ActionRetryChannel}, http.StatusGatewayTimeout},
		{"599 => 502", &proxyResult{status: util.StatusStreamIncomplete, nextAction: cooldown.ActionRetryChannel}, http.StatusBadGateway},
		{"499 client-canceled passthrough", &proxyResult{status: 499, isClientCanceled: true, nextAction: cooldown.ActionReturnClient}, 499},
		{"499 upstream mapped to 502", &proxyResult{status: 499, isClientCanceled: false, nextAction: cooldown.ActionRetryChannel}, http.StatusBadGateway},
		{"401 Key-level mapped to 401 (透明代理)", &proxyResult{status: http.StatusUnauthorized, nextAction: cooldown.ActionRetryKey}, http.StatusUnauthorized},
		{"5xx Channel-level passthrough", &proxyResult{status: http.StatusBadGateway, nextAction: cooldown.ActionRetryChannel}, http.StatusBadGateway},
		// [FIX] 透明代理：所有上游状态码都透传，不映射
		{"400 Key-level (invalid_api_key) => 400", &proxyResult{status: 400, nextAction: cooldown.ActionRetryKey}, 400},
		{"400 Client-level (参数错误) => 400", &proxyResult{status: 400, nextAction: cooldown.ActionReturnClient}, 400},
		{"404 Channel-level (BaseURL错误) => 404", &proxyResult{status: 404, nextAction: cooldown.ActionRetryChannel}, 404},
		{"404 Client-level (模型不存在) => 404", &proxyResult{status: 404, nextAction: cooldown.ActionReturnClient}, 404},
		{"429 Key-level => 429", &proxyResult{status: 429, nextAction: cooldown.ActionRetryKey}, 429},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := determineFinalClientStatus(tt.last); got != tt.expected {
				t.Fatalf("determineFinalClientStatus()=%d, expected %d", got, tt.expected)
			}
		})
	}
}

func TestShouldStopTryingChannels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		in       *proxyResult
		expected bool
	}{
		{"nil => stop", nil, true},
		{"client canceled => stop", &proxyResult{status: 499, isClientCanceled: true, nextAction: cooldown.ActionReturnClient}, true},
		{"broken pipe => stop", &proxyResult{status: 499, nextAction: cooldown.ActionReturnClient}, true},
		{"client-level => stop", &proxyResult{status: 404, nextAction: cooldown.ActionReturnClient}, true},
		{"channel-level => continue", &proxyResult{status: 404, nextAction: cooldown.ActionRetryChannel}, false},
		{"key-level (400 invalid_api_key) => continue", &proxyResult{status: 400, nextAction: cooldown.ActionRetryKey}, false},
		{"key-level 429 => continue", &proxyResult{status: 429, nextAction: cooldown.ActionRetryKey}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldStopTryingChannels(tt.in); got != tt.expected {
				t.Fatalf("shouldStopTryingChannels()=%v, expected %v", got, tt.expected)
			}
		})
	}
}
