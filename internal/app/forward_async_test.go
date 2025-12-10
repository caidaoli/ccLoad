package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// TestMain 在所有测试运行前设置环境变量
func TestMain(m *testing.M) {
	// 为测试设置必需的环境变量
	os.Setenv("CCLOAD_PASS", "test_password_123")

	// 运行测试
	code := m.Run()

	// 清理
	os.Unsetenv("CCLOAD_PASS")

	os.Exit(code)
}

// TestRequestContextCreation 测试请求上下文创建
func TestRequestContextCreation(t *testing.T) {
	store, _ := storage.CreateSQLiteStore(":memory:", nil)
	srv := NewServer(store)

	tests := []struct {
		name          string
		requestPath   string
		body          []byte
		wantStreaming bool
	}{
		{
			name:          "流式请求-应设置超时",
			requestPath:   "/v1/messages",
			body:          []byte(`{"stream":true}`),
			wantStreaming: true,
		},
		{
			name:          "非流式请求-无超时",
			requestPath:   "/v1/messages",
			body:          []byte(`{"stream":false}`),
			wantStreaming: false,
		},
		{
			name:          "Gemini流式-路径识别",
			requestPath:   "/v1beta/models/gemini:streamGenerateContent",
			body:          []byte(`{}`),
			wantStreaming: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			// 移除defer reqCtx.Close()（Close方法已删除）
			reqCtx := srv.newRequestContext(ctx, tt.requestPath, tt.body)

			if reqCtx.isStreaming != tt.wantStreaming {
				t.Errorf("isStreaming = %v, want %v", reqCtx.isStreaming, tt.wantStreaming)
			}

			// 验证上下文创建成功
			if reqCtx.ctx == nil {
				t.Error("reqCtx.ctx should not be nil")
			}

			// 移除cancel字段验证（cancel已删除）
		})
	}
}

// TestBuildProxyRequest 测试请求构建
func TestBuildProxyRequest(t *testing.T) {
	store, _ := storage.CreateSQLiteStore(":memory:", nil)
	srv := NewServer(store)

	cfg := &model.Config{
		ID:          1,
		Name:        "test",
		URL:         "https://api.example.com",
		ChannelType: "anthropic",
	}

	reqCtx := &requestContext{
		ctx:       context.Background(),
		startTime: time.Now(),
	}

	req, err := srv.buildProxyRequest(
		reqCtx,
		cfg,
		"sk-test-key",
		http.MethodPost,
		[]byte(`{"model":"claude-3"}`),
		http.Header{"User-Agent": []string{"test"}},
		"",
		"/v1/messages",
	)

	if err != nil {
		t.Fatalf("buildProxyRequest failed: %v", err)
	}

	// 验证 URL
	if req.URL.String() != "https://api.example.com/v1/messages" {
		t.Errorf("URL = %s, want https://api.example.com/v1/messages", req.URL.String())
	}

	// 验证认证头
	if req.Header.Get("x-api-key") != "sk-test-key" {
		t.Errorf("x-api-key = %s, want sk-test-key", req.Header.Get("x-api-key"))
	}

	// 验证请求头复制
	if req.Header.Get("User-Agent") != "test" {
		t.Errorf("User-Agent not copied")
	}
}

// TestHandleRequestError 测试错误处理
func TestHandleRequestError(t *testing.T) {
	store, _ := storage.CreateSQLiteStore(":memory:", nil)
	srv := NewServer(store)

	cfg := &model.Config{ID: 1}

	tests := []struct {
		name         string
		err          error
		isStreaming  bool
		wantContains string
	}{
		{
			name:         "超时错误-流式请求",
			err:          context.DeadlineExceeded,
			isStreaming:  true,
			wantContains: "upstream timeout",
		},
		{
			name:         "超时错误-非流式请求",
			err:          context.DeadlineExceeded,
			isStreaming:  false,
			wantContains: "context deadline exceeded",
		},
		{
			name:         "其他网络错误",
			err:          &net.OpError{Op: "dial", Err: &net.DNSError{}},
			isStreaming:  false,
			wantContains: "dial",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqCtx := &requestContext{
				startTime:   time.Now(),
				isStreaming: tt.isStreaming,
			}

			result, duration, err := srv.handleRequestError(reqCtx, cfg, tt.err)

			if err == nil {
				t.Error("expected error, got nil")
			}

			if !strings.Contains(err.Error(), tt.wantContains) {
				t.Errorf("error = %v, should contain %s", err, tt.wantContains)
			}

			// ErrCodeNetworkRetryable = -1 是合法的内部标识符
			// 对于某些网络错误（如DNS错误），无法映射到标准HTTP状态码
			// 使用负值避免与HTTP状态码混淆
			if result.Status == ErrCodeNetworkRetryable {
				// 检查是否为网络操作错误
				var netOpErr *net.OpError
				if !errors.As(tt.err, &netOpErr) {
					t.Errorf("expected network error, got status=%d", result.Status)
				}
			} else if result.Status <= 0 {
				t.Errorf("unexpected negative status code: %d", result.Status)
			}

			if duration < 0 {
				t.Error("duration should be >= 0")
			}
		})
	}
}

// TestForwardOnceAsync_Integration 集成测试
func TestForwardOnceAsync_Integration(t *testing.T) {
	// 创建测试服务器
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证认证头
		if r.Header.Get("x-api-key") != "sk-test" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		// 成功响应
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"test","model":"claude-3"}`))
	}))
	defer upstream.Close()

	// 创建代理服务器
	store, _ := storage.CreateSQLiteStore(":memory:", nil)
	srv := NewServer(store)

	cfg := &model.Config{
		ID:   1,
		Name: "test",
		URL:  upstream.URL,
	}

	// 测试成功请求
	t.Run("成功请求", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		result, duration, err := srv.forwardOnceAsync(
			context.Background(),
			cfg,
			"sk-test", // 正确的key
			http.MethodPost,
			[]byte(`{"model":"claude-3"}`),
			http.Header{},
			"",
			"/v1/messages",
			recorder,
		)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if result.Status != 200 {
			t.Errorf("status = %d, want 200", result.Status)
		}

		if duration <= 0 {
			t.Error("duration should be > 0")
		}

		if result.FirstByteTime <= 0 {
			t.Error("firstByteTime should be > 0")
		}
	})

	// 测试认证失败
	t.Run("认证失败", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		result, _, err := srv.forwardOnceAsync(
			context.Background(),
			cfg,
			"sk-wrong", // 错误的key
			http.MethodPost,
			[]byte(`{"model":"claude-3"}`),
			http.Header{},
			"",
			"/v1/messages",
			recorder,
		)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if result.Status != 401 {
			t.Errorf("status = %d, want 401", result.Status)
		}

		if !strings.Contains(string(result.Body), "unauthorized") {
			t.Error("response should contain 'unauthorized'")
		}
	})
}

// TestClientCancelClosesUpstream 测试客户端取消时上游连接立即关闭（方案1验证）
// 验证：客户端499取消 → resp.Body.Close() → 上游Read被中断
func TestClientCancelClosesUpstream(t *testing.T) {
	// 通道：用于同步上游服务器的状态
	upstreamStarted := make(chan struct{})
	upstreamClosed := make(chan struct{})

	// 创建模拟上游服务器：缓慢发送流式数据
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter不支持Flush")
			return
		}

		// 发送第一块数据，通知测试客户端已开始接收
		w.Write([]byte("data: chunk1\n\n"))
		flusher.Flush()
		close(upstreamStarted)

		// 尝试继续发送数据（模拟长时间流式响应）
		// 如果连接被关闭，Write会失败
		for i := 2; i <= 100; i++ {
			time.Sleep(50 * time.Millisecond)
			data := []byte(fmt.Sprintf("data: chunk%d\n\n", i))
			_, err := w.Write(data)
			if err != nil {
				// 连接已关闭！这是我们期望的结果
				close(upstreamClosed)
				return
			}
			flusher.Flush()
		}

		// 如果循环结束，说明连接没有被关闭（测试失败）
		t.Error("上游服务器完成了所有发送，连接未被关闭")
	}))
	defer upstream.Close()

	// 创建代理服务器
	store, _ := storage.CreateSQLiteStore(":memory:", nil)
	srv := NewServer(store)

	cfg := &model.Config{
		ID:   1,
		Name: "test",
		URL:  upstream.URL,
	}

	// 创建可取消的context
	ctx, cancel := context.WithCancel(context.Background())

	// 启动代理请求（goroutine中执行，因为会阻塞到取消）
	resultChan := make(chan struct {
		result   *fwResult
		duration float64
		err      error
	}, 1)

	go func() {
		recorder := httptest.NewRecorder()
		result, duration, err := srv.forwardOnceAsync(
			ctx,
			cfg,
			"sk-test",
			http.MethodPost,
			[]byte(`{"stream":true}`),
			http.Header{},
			"",
			"/v1/messages",
			recorder,
		)
		resultChan <- struct {
			result   *fwResult
			duration float64
			err      error
		}{result, duration, err}
	}()

	// 等待上游开始发送数据
	select {
	case <-upstreamStarted:
		// 上游已开始发送
	case <-time.After(2 * time.Second):
		t.Fatal("超时：上游未开始发送数据")
	}

	// 模拟客户端取消（499场景）
	cancel()

	// 验证上游连接在短时间内被关闭
	select {
	case <-upstreamClosed:
		// ✅ 成功！上游检测到连接关闭
		t.Log("✅ 客户端取消后，上游连接立即关闭（预期行为）")
	case <-time.After(500 * time.Millisecond):
		t.Error("❌ 客户端取消后500ms，上游仍在发送数据（连接未关闭）")
	}

	// 验证forwardOnceAsync返回context.Canceled错误
	select {
	case res := <-resultChan:
		if res.err == nil {
			t.Error("期望返回错误（context.Canceled）")
		}
		if !errors.Is(res.err, context.Canceled) && res.result != nil && res.result.Status != 499 {
			t.Errorf("期望context.Canceled或499，实际: err=%v, status=%d", res.err, res.result.Status)
		}
		t.Logf("forwardOnceAsync返回: err=%v, status=%d", res.err, res.result.Status)
	case <-time.After(2 * time.Second):
		t.Error("超时：forwardOnceAsync未返回")
	}
}
