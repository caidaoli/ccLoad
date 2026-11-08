package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

func TestHandleProxyRequest_UnknownPathReturns404(t *testing.T) {
	srv := &Server{
		requestBodyMemory: requestBodyMemoryBudget{limit: config.DefaultRequestBodyMemoryBudgetBytes},
		concurrencySem:    make(chan struct{}, 1),
		activeRequests:    newActiveRequestManager(),
	}

	body := bytes.NewBufferString(`{"model":"gpt-4"}`)
	req := newRequest(http.MethodPost, "/v1/unknown", body)
	req.Header.Set("Content-Type", "application/json")

	c, w := newTestContext(t, req)

	srv.HandleProxyRequest(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("预期状态码404，实际%d", w.Code)
	}

	if body := w.Body.String(); !bytes.Contains([]byte(body), []byte("unsupported path")) {
		t.Fatalf("响应内容缺少错误信息，实际: %s", body)
	}
}

func TestWriteFinalProxyResponse_DisablesWriteTimeoutForJSONFallback(t *testing.T) {
	t.Parallel()

	srv := &Server{}
	w := &deadlineRecorderResponseWriter{}
	c, _ := gin.CreateTestContext(w)
	c.Request = newRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := &proxyRequestContext{
		startTime: time.Now(),
		clientIP:  "127.0.0.1",
	}

	srv.writeFinalProxyResponse(c, reqCtx, false, &proxyResult{status: 0}, 1)

	if !w.deadlineCalled {
		t.Fatal("SetWriteDeadline was not called")
	}
	if !w.writeDeadline.IsZero() {
		t.Fatalf("writeDeadline=%v, want zero time", w.writeDeadline)
	}
}

// ============================================================================
// 增加proxy_handler测试覆盖率
// ============================================================================

// TestParseIncomingRequest_ValidJSON 测试有效JSON解析
func TestParseIncomingRequest_ValidJSON(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		path         string
		contentType  string
		expectModel  string
		expectStream bool
		expectError  bool
		// expectBody 非空时断言转发给上游的 body 模型名
		expectBody string
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
			name:         "流式请求-stream字符串true",
			body:         `{"model":"gpt-4","stream":"true","messages":[]}`,
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
		{
			name:         "Codex alpha/search-无模型保持为空",
			body:         `{"query":"codegraph"}`,
			path:         "/v1/alpha/search",
			expectModel:  "",
			expectStream: false,
			expectError:  false,
		},
		{
			name:         "Codex alpha/search-有模型保留",
			body:         `{"model":"gpt-5","query":"codegraph"}`,
			path:         "/v1/alpha/search",
			expectModel:  "gpt-5",
			expectStream: false,
			expectError:  false,
		},
		{
			name:        "尾随JSON被拒绝",
			body:        `{"model":"gpt-4","messages":[]} []`,
			path:        "/v1/chat/completions",
			expectError: true,
		},
		{
			name:         "思考后缀-从模型名剥离等级",
			body:         `{"model":"gpt-5.6-luna(max)","messages":[]}`,
			path:         "/v1/chat/completions",
			expectModel:  "gpt-5.6-luna",
			expectStream: false,
			expectError:  false,
		},
		{
			name:         "思考后缀-未识别括号保留原名",
			body:         `{"model":"gpt-5.6-luna(foo)","messages":[]}`,
			path:         "/v1/chat/completions",
			expectModel:  "gpt-5.6-luna(foo)",
			expectStream: false,
			expectError:  false,
		},
		{
			name:         "思考后缀-从 Gemini 路径剥离",
			body:         `{"contents":[]}`,
			path:         "/v1beta/models/gemini-2.5-pro(high):generateContent",
			expectModel:  "gemini-2.5-pro",
			expectStream: false,
			expectError:  false,
		},
		{
			name:        "Claude Code 上下文后缀-Messages 入口归一",
			body:        `{"model":"claude-opus-4-6[1m][1M]","messages":[]}`,
			path:        "/v1/messages",
			expectModel: "claude-opus-4-6",
			expectBody:  "claude-opus-4-6",
		},
		{
			name:        "Claude Code 上下文后缀-count_tokens 入口归一",
			body:        `{"model":"claude-sonnet-4-6[1m]","messages":[]}`,
			path:        "/v1/messages/count_tokens",
			expectModel: "claude-sonnet-4-6",
			expectBody:  "claude-sonnet-4-6",
		},
		{
			name:        "Claude Code 上下文后缀-非 Anthropic 入口不改",
			body:        `{"model":"deepseek-flash[1m]","messages":[]}`,
			path:        "/v1/chat/completions",
			expectModel: "deepseek-flash[1m]",
			expectBody:  "deepseek-flash[1m]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := bytes.NewBufferString(tt.body)
			req := newRequest(http.MethodPost, tt.path, body)
			if tt.body == "" {
				req.Method = http.MethodGet
			}
			contentType := tt.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			req.Header.Set("Content-Type", contentType)

			c, _ := newTestContext(t, req)

			incoming, err := parseIncomingRequest(c, requestBodyLimits{}, testRequestBodyMemory(t))

			if tt.expectError && err == nil {
				t.Errorf("期望错误但未发生")
			}
			if !tt.expectError && err != nil {
				t.Errorf("不期望错误但发生: %v", err)
			}
			if incoming.originalModel != tt.expectModel {
				t.Errorf("模型名错误: 期望%s, 实际%s", tt.expectModel, incoming.originalModel)
			}
			if incoming.isStreaming != tt.expectStream {
				t.Errorf("流式标志错误: 期望%v, 实际%v", tt.expectStream, incoming.isStreaming)
			}
			if tt.expectBody != "" {
				if got := gjson.GetBytes(incoming.body, "model").String(); got != tt.expectBody {
					t.Errorf("body 模型名错误: 期望%s, 实际%s", tt.expectBody, got)
				}
			}
		})
	}
}

// TestParseIncomingRequest_BodyTooLarge 测试请求体过大
func TestParseIncomingRequest_BodyTooLarge(t *testing.T) {
	// 设置较小的限制以便测试
	bodyLimits := newRequestBodyLimits(1048576, 1048576) // 1MB

	// 创建超大请求体（>1MB）
	largeBody := make([]byte, 2*1024*1024) // 2MB
	for i := range largeBody {
		largeBody[i] = 'a'
	}

	req := newRequest(http.MethodPost, "/v1/messages", bytes.NewReader(largeBody))
	req.Header.Set("Content-Type", "application/json")

	c, _ := newTestContext(t, req)

	_, err := parseIncomingRequest(c, bodyLimits, testRequestBodyMemory(t))

	if err != errBodyTooLarge {
		t.Errorf("期望errBodyTooLarge错误, 实际: %v", err)
	}
}

func TestParseIncomingRequest_DecodesContentEncoding(t *testing.T) {
	plain := []byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[]}`)
	gzipBody := func(data []byte) []byte {
		var buf bytes.Buffer
		writer := gzip.NewWriter(&buf)
		_, _ = writer.Write(data)
		_ = writer.Close()
		return buf.Bytes()
	}
	zstdBody := func(data []byte) []byte {
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		defer func() { _ = encoder.Close() }()
		return encoder.EncodeAll(data, nil)
	}
	limits := newRequestBodyLimits(1024, 1024)
	parse := func(body io.Reader, contentEncoding string) (*gin.Context, incomingRequest, error) {
		req := newRequest(http.MethodPost, "/v1/messages", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", contentEncoding)
		c, _ := newTestContext(t, req)
		incoming, err := parseIncomingRequest(c, limits, testRequestBodyMemory(t))
		return c, incoming, err
	}

	for _, tc := range []struct {
		name, encoding string
		body           []byte
	}{
		{name: "gzip", encoding: "gzip", body: gzipBody(plain)},
		{name: "zstd", encoding: "zstd", body: zstdBody(plain)},
		{name: "stacked", encoding: "gzip, zstd", body: zstdBody(gzipBody(plain))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, incoming, err := parse(bytes.NewReader(tc.body), tc.encoding)
			if err != nil {
				t.Fatalf("parseIncomingRequest: %v", err)
			}
			if !bytes.Equal(incoming.body, plain) || incoming.originalModel != "claude-sonnet-4-6" || !incoming.isStreaming {
				t.Fatalf("incoming = body %q model %q stream %v", incoming.body, incoming.originalModel, incoming.isStreaming)
			}
			// 请求体已是明文，编码头不能再随通用请求头转发到上游。
			if got := c.Request.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q after decoding", got)
			}
		})
	}

	t.Run("unsupported encoding", func(t *testing.T) {
		_, _, err := parse(bytes.NewReader(plain), "compress")
		if !errors.Is(err, errUnsupportedContentEncoding) {
			t.Fatalf("err = %v, want errUnsupportedContentEncoding", err)
		}
	})
	t.Run("corrupt body", func(t *testing.T) {
		_, _, err := parse(bytes.NewReader(plain), "gzip")
		if err == nil || errors.Is(err, errUnsupportedContentEncoding) || errors.Is(err, errBodyTooLarge) || errors.Is(err, errBodyReadTimeout) {
			t.Fatalf("err = %v, want a plain bad-request error", err)
		}
	})
	t.Run("decoded size counts against limit", func(t *testing.T) {
		bomb := gzipBody(bytes.Repeat([]byte("a"), 64*1024))
		if len(bomb) > 1024 {
			t.Fatalf("compressed fixture %d bytes must fit the limit", len(bomb))
		}
		_, _, err := parse(bytes.NewReader(bomb), "gzip")
		if !errors.Is(err, errBodyTooLarge) {
			t.Fatalf("err = %v, want errBodyTooLarge", err)
		}
	})
	t.Run("read timeout inside decoder", func(t *testing.T) {
		compressed := gzipBody(plain)
		body := io.MultiReader(bytes.NewReader(compressed[:len(compressed)/2]), &errorReader{err: os.ErrDeadlineExceeded})
		_, _, err := parse(body, "gzip")
		if !errors.Is(err, errBodyReadTimeout) {
			t.Fatalf("err = %v, want errBodyReadTimeout", err)
		}
	})
}

// TestAcquireConcurrencySlot 测试并发槽位获取
func TestAcquireConcurrencySlot(t *testing.T) {
	srv := &Server{
		concurrencySem: make(chan struct{}, 2), // 最大并发数=2
		maxConcurrency: 2,
	}

	// 创建有效的gin.Context
	c, _ := newTestContext(t, newRequest(http.MethodPost, "/test", nil))

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
	srv := &Server{
		concurrencySem: make(chan struct{}, 1),
	}
	srv.concurrencySem <- struct{}{} // 填满槽位，迫使走等待分支

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c, w := newTestContext(t, newRequest(http.MethodPost, "/test", nil).WithContext(ctx))

	release, acquired := srv.acquireConcurrencySlot(c)
	if acquired || release != nil {
		t.Fatal("预期获取失败且release=nil")
	}
	if w.Code != StatusClientClosedRequest {
		t.Fatalf("预期状态码%d，实际%d", StatusClientClosedRequest, w.Code)
	}
}

func TestAcquireConcurrencySlot_DeadlineExceeded_Returns504(t *testing.T) {
	srv := &Server{
		concurrencySem: make(chan struct{}, 1),
	}
	srv.concurrencySem <- struct{}{} // 填满槽位，迫使走等待分支

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	c, w := newTestContext(t, newRequest(http.MethodPost, "/test", nil).WithContext(ctx))

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
		{"400 Channel-level (参数错误) => 400", &proxyResult{status: 400, nextAction: cooldown.ActionRetryChannel}, 400},
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

// ============================================================================
// handleSpecialRoutes 测试
// ============================================================================

// TestHandleSpecialRoutes_OpenAIModels 测试 GET /v1/models 路由匹配
func TestHandleSpecialRoutes_OpenAIModels(t *testing.T) {
	srv := newInMemoryServer(t)

	req := newRequest(http.MethodGet, "/v1/models", nil)
	c, w := newTestContext(t, req)

	handled := srv.handleSpecialRoutes(c)
	if !handled {
		t.Fatal("GET /v1/models 应被 handleSpecialRoutes 处理")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if resp["object"] != "list" {
		t.Fatalf("期望 object=list, 实际=%v", resp["object"])
	}
}

// TestHandleSpecialRoutes_GeminiModels 测试 GET /v1beta/models 路由匹配
func TestHandleSpecialRoutes_GeminiModels(t *testing.T) {
	srv := newInMemoryServer(t)

	req := newRequest(http.MethodGet, "/v1beta/models", nil)
	c, w := newTestContext(t, req)

	handled := srv.handleSpecialRoutes(c)
	if !handled {
		t.Fatal("GET /v1beta/models 应被 handleSpecialRoutes 处理")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if _, ok := resp["models"]; !ok {
		t.Fatal("Gemini models 响应应包含 models 字段")
	}
}

// TestHandleSpecialRoutes_CountTokensFallsThrough verifies that token counting
// now enters the regular upstream routing path.
func TestHandleSpecialRoutes_CountTokensFallsThrough(t *testing.T) {
	srv := newInMemoryServer(t)

	body := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}]}`
	req := newRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	c, _ := newTestContext(t, req)

	handled := srv.handleSpecialRoutes(c)
	if handled {
		t.Fatal("POST /v1/messages/count_tokens 应进入代理选路")
	}
}

// TestHandleSpecialRoutes_Fallthrough 测试不匹配的路由返回 false
func TestHandleSpecialRoutes_Fallthrough(t *testing.T) {
	srv := newInMemoryServer(t)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"POST /v1/models 不匹配", http.MethodPost, "/v1/models"},
		{"GET /v1/chat/completions 不匹配", http.MethodGet, "/v1/chat/completions"},
		{"POST /v1/messages 不匹配", http.MethodPost, "/v1/messages"},
		{"GET /v1/messages/count_tokens 不匹配", http.MethodGet, "/v1/messages/count_tokens"},
		{"GET /v1beta/models/xxx 不匹配", http.MethodGet, "/v1beta/models/xxx"},
		{"POST /v1beta/models 不匹配", http.MethodPost, "/v1beta/models"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newRequest(tt.method, tt.path, nil)
			c, _ := newTestContext(t, req)

			handled := srv.handleSpecialRoutes(c)
			if handled {
				t.Fatalf("%s %s 不应被 handleSpecialRoutes 处理", tt.method, tt.path)
			}
		})
	}
}

// TestParseIncomingRequest_MultipartModel 测试 multipart/form-data 中提取 model
func TestParseIncomingRequest_MultipartModel(t *testing.T) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "dall-e-2")
	_ = writer.WriteField("prompt", "a cute cat")
	_ = writer.WriteField("n", "1")
	_ = writer.Close()

	req := newRequest(http.MethodPost, "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	c, _ := newTestContext(t, req)

	incoming, err := parseIncomingRequest(c, requestBodyLimits{}, testRequestBodyMemory(t))
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	if incoming.originalModel != "dall-e-2" {
		t.Fatalf("模型名应为 dall-e-2, 实际: %s", incoming.originalModel)
	}
	if incoming.isStreaming {
		t.Fatal("images 请求不应为流式")
	}
}

func TestParseIncomingRequest_RejectsMalformedMultipartFraming(t *testing.T) {
	req := newRequest(http.MethodPost, "/v1/images/edits", bytes.NewBufferString("not a multipart payload"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=missing")
	c, _ := newTestContext(t, req)
	if _, err := parseIncomingRequest(c, requestBodyLimits{}, testRequestBodyMemory(t)); err == nil {
		t.Fatal("malformed multipart framing was accepted")
	}
}

// TestParseIncomingRequest_ImagesJSON 测试 images/generations 的标准 JSON 请求
func TestParseIncomingRequest_ImagesJSON(t *testing.T) {
	body := `{"model":"gpt-image-1","prompt":"a white cat","n":1,"size":"1024x1024"}`
	req := newRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	c, _ := newTestContext(t, req)

	incoming, err := parseIncomingRequest(c, requestBodyLimits{}, testRequestBodyMemory(t))
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	if incoming.originalModel != "gpt-image-1" {
		t.Fatalf("模型名应为 gpt-image-1, 实际: %s", incoming.originalModel)
	}
	if incoming.isStreaming {
		t.Fatal("images 请求不应为流式")
	}
}

// TestParseIncomingRequest_ImagesLargerBodyAllowed 测试 images 路径允许更大的请求体
func TestParseIncomingRequest_ImagesLargerBodyAllowed(t *testing.T) {
	// 创建 15MB 的 multipart 请求体（超过默认 10MB，但在 images 20MB 限制内）
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "gpt-image-1")
	_ = writer.WriteField("prompt", "test")
	part, _ := writer.CreateFormFile("image", "test.png")
	largeData := make([]byte, 15*1024*1024)
	_, _ = part.Write(largeData)
	_ = writer.Close()

	req := newRequest(http.MethodPost, "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	c, _ := newTestContext(t, req)

	incoming, err := parseIncomingRequest(c, requestBodyLimits{}, testRequestBodyMemory(t))
	if err != nil {
		t.Fatalf("images 路径 15MB 请求体不应报错, 实际: %v", err)
	}
	if incoming.originalModel != "gpt-image-1" {
		t.Fatalf("模型名应为 gpt-image-1, 实际: %s", incoming.originalModel)
	}
}

/**
 * @brief 为请求解析测试创建独立预算，并在测试结束时释放正文预留。
 * @param t 当前测试上下文。
 * @return 使用默认容量的请求预留。
 */
func testRequestBodyMemory(t *testing.T) *requestBodyMemoryReservation {
	t.Helper()
	memory := &requestBodyMemoryReservation{budget: &requestBodyMemoryBudget{limit: config.DefaultRequestBodyMemoryBudgetBytes}}
	t.Cleanup(memory.close)
	return memory
}

/**
 * @brief HTTP 正文预算由并发请求共享，直到上游响应处理完成后才允许新请求复用。
 * @param t 当前测试上下文。
 */
func TestHandleProxyRequest_BodyMemoryBudgetSharedUntilResponseEnds(t *testing.T) {
	for _, knownLength := range []bool{true, false} {
		t.Run(fmt.Sprintf("known_length_%t", knownLength), func(t *testing.T) {
			entered := make(chan struct{})
			resume := make(chan struct{})
			var resumeOnce sync.Once
			release := func() { resumeOnce.Do(func() { close(resume) }) }
			defer release()
			var calls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					close(entered)
					<-resume
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			env := setupProxyTestEnvWithSettings(t,
				[]testChannel{{name: "budget", models: "budget-model"}},
				map[int]string{0: upstream.URL},
				map[string]string{config.RequestBodyMemoryBudgetSettingKey: "512"})
			request := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"budget-model","messages":[{"role":"user","content":"hi"}]}`))
				if !knownLength {
					req.ContentLength = -1
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer test-api-key")
				w := httptest.NewRecorder()
				env.engine.ServeHTTP(w, req)
				return w
			}
			first := make(chan *httptest.ResponseRecorder, 1)
			go func() { first <- request() }()
			select {
			case <-entered:
			case result := <-first:
				t.Fatalf("first request failed before upstream: %d: %s", result.Code, result.Body.String())
			case <-time.After(5 * time.Second):
				t.Fatal("first request did not reach upstream")
			}
			rejected := request()
			if rejected.Code != http.StatusServiceUnavailable || rejected.Header().Get("Retry-After") != "1" {
				t.Fatalf("budget rejection = %d, headers %v, body %s", rejected.Code, rejected.Header(), rejected.Body.String())
			}
			if got := gjson.GetBytes(rejected.Body.Bytes(), "error").String(); !strings.Contains(got, config.RequestBodyMemoryBudgetSettingKey) {
				t.Fatalf("budget error message = %q", got)
			}
			if calls.Load() != 1 {
				t.Fatal("rejected request reached upstream")
			}
			release()
			select {
			case result := <-first:
				if result.Code != http.StatusOK {
					t.Fatalf("first response = %d: %s", result.Code, result.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("first handler did not finish")
			}
			if result := request(); result.Code != http.StatusOK {
				t.Fatalf("budget was not released: %d: %s", result.Code, result.Body.String())
			}
		})
	}
}

/**
 * @brief 解析、解压、读取及取消失败都归还预算，不使后续有效请求被错误拒绝。
 * @param t 当前测试上下文。
 */
func TestHandleProxyRequest_BodyMemoryBudgetReleasedAfterFailures(t *testing.T) {
	for _, failure := range []string{"invalid-json", "oversize", "chunked-oversize", "growth-budget", "read-timeout", "cancelled", "compressed-workspace", "corrupt-gzip", "unsupported-encoding"} {
		t.Run(failure, func(t *testing.T) {
			budget := int64(512)
			if failure == "corrupt-gzip" {
				budget += 256 << 10
			}
			maxBody := 512
			if failure == "growth-budget" {
				budget = 1024
				maxBody = 2048
			}
			srv := &Server{
				requestBodyMemory: requestBodyMemoryBudget{limit: budget},
				bodyLimits:        newRequestBodyLimits(maxBody, maxBody),
				concurrencySem:    make(chan struct{}, 1),
				activeRequests:    newActiveRequestManager(),
			}
			var body io.Reader = strings.NewReader(`{"model":`)
			status := http.StatusBadRequest
			switch failure {
			case "oversize", "chunked-oversize":
				body = strings.NewReader(strings.Repeat("x", 513))
				status = http.StatusRequestEntityTooLarge
			case "read-timeout":
				body = io.MultiReader(strings.NewReader(`{"model":`), &errorReader{err: os.ErrDeadlineExceeded})
				status = http.StatusRequestTimeout
			case "compressed-workspace":
				status = http.StatusServiceUnavailable
			case "growth-budget":
				body = strings.NewReader(strings.Repeat("x", 513))
				status = http.StatusServiceUnavailable
			case "unsupported-encoding":
				status = http.StatusUnsupportedMediaType
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/unknown", body)
			req.Header.Set("Content-Type", "application/json")
			switch failure {
			case "chunked-oversize", "growth-budget":
				req.ContentLength = -1
			case "cancelled":
				ctx, cancel := context.WithCancel(req.Context())
				defer cancel()
				req = req.WithContext(ctx)
				// 先进入 handler 再取消，确保已经取得并发槽。
				req.Body = io.NopCloser(&cancelBodyReader{cancel: cancel})
			case "compressed-workspace", "corrupt-gzip":
				req.Header.Set("Content-Encoding", "gzip")
			case "unsupported-encoding":
				req.Header.Set("Content-Encoding", "compress")
			}
			c, w := newTestContext(t, req)
			srv.HandleProxyRequest(c)
			if w.Code != status {
				t.Fatalf("failure status = %d, want %d: %s", w.Code, status, w.Body.String())
			}
			var nextBody io.Reader = strings.NewReader(`{"model":"valid"}`)
			if failure == "growth-budget" {
				nextBody = strings.NewReader(`{"model":"valid"}` + strings.Repeat(" ", 700))
			}
			if failure == "corrupt-gzip" {
				var compressed bytes.Buffer
				encoder := gzip.NewWriter(&compressed)
				_, _ = io.WriteString(encoder, `{"model":"valid"}`)
				_ = encoder.Close()
				nextBody = &compressed
			}
			next := httptest.NewRequest(http.MethodPost, "/v1/unknown", nextBody)
			next.Header.Set("Content-Type", "application/json")
			if failure == "corrupt-gzip" {
				next.Header.Set("Content-Encoding", "gzip")
			}
			c, w = newTestContext(t, next)
			srv.HandleProxyRequest(c)
			if w.Code != http.StatusNotFound {
				t.Fatalf("next request should parse and reach routing: %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

/**
 * @brief 首次正文读取时模拟客户端取消，覆盖已进入 handler 的资源释放。
 */
type cancelBodyReader struct {
	cancel  context.CancelFunc ///< 模拟客户端断开的取消函数。
	started bool               ///< 已读取首字节并分配正文缓冲。
}

/**
 * @brief 首字节正常读取，后续读取模拟客户端取消。
 * @param p 调用方提供的读取缓冲。
 * @return 首次返回一字节，其余为零。
 * @return 首次返回 nil，后续返回 context.Canceled。
 */
func (r *cancelBodyReader) Read(p []byte) (int, error) {
	if !r.started {
		r.started = true
		p[0] = '{'
		return 1, nil
	}
	r.cancel()
	return 0, context.Canceled
}

/**
 * @brief HTTP/1 慢上传在预算不足时立即收到 503，不等待剩余正文或读取超时。
 * @param t 当前测试上下文。
 */
func TestHandleProxyRequest_BodyMemoryBudgetRejectsSlowUpload(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			srv := &Server{
				requestBodyMemory: requestBodyMemoryBudget{limit: 512},
				bodyLimits:        newRequestBodyLimits(4096, 4096),
				concurrencySem:    make(chan struct{}, 1),
				activeRequests:    newActiveRequestManager(),
			}
			engine := gin.New()
			engine.POST("/v1/unknown", srv.HandleProxyRequest)
			server := httptest.NewUnstartedServer(engine)
			server.Config.ReadTimeout = 10 * time.Second
			server.Start()
			defer server.Close()
			conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(conn, "POST /v1/unknown HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 1024\r\nContent-Encoding: %s\r\n\r\n", encoding); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("budget rejection waited for the unfinished upload: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" || !response.Close {
				t.Fatalf("rejection = %d, headers %v, close=%t", response.StatusCode, response.Header, response.Close)
			}
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Fatal(err)
			}
			// 下一连接应重新取得并发槽和正文预算，并正常走到未支持路由。
			next, err := server.Client().Post(server.URL+"/v1/unknown", "application/json", strings.NewReader(`{"model":"valid"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = next.Body.Close() }()
			if next.StatusCode != http.StatusNotFound {
				t.Fatalf("next request status = %d", next.StatusCode)
			}
		})
	}
}

/**
 * @brief 无长度正文恰好占满预算时，EOF 探测不额外申请缓冲。
 * @param t 当前测试上下文。
 */
func TestHandleProxyRequest_BodyMemoryBudgetExactCapacity(t *testing.T) {
	srv := &Server{
		requestBodyMemory: requestBodyMemoryBudget{limit: 512},
		bodyLimits:        newRequestBodyLimits(4096, 4096),
		concurrencySem:    make(chan struct{}, 1),
		activeRequests:    newActiveRequestManager(),
	}
	body := `{"model":"valid"}`
	body += strings.Repeat(" ", 512-len(body))
	req := httptest.NewRequest(http.MethodPost, "/v1/unknown", strings.NewReader(body))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	c, w := newTestContext(t, req)
	srv.HandleProxyRequest(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("full buffer should parse and reach routing: %d: %s", w.Code, w.Body.String())
	}
}
