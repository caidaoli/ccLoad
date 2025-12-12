package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
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
			if result.Status == util.ErrCodeNetworkRetryable {
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
		// [INFO] 成功！上游检测到连接关闭
		t.Log("[INFO] 客户端取消后，上游连接立即关闭（预期行为）")
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

// TestNoGoroutineLeak 验证无 goroutine 泄漏（Go 1.21+ context.AfterFunc）
// 测试场景：
// 1. 正常请求完成 - 定时器/context 应被清理
// 2. 客户端取消（499） - AfterFunc 触发，但无泄漏
// 3. 首字节超时 - 定时器触发，context 取消
func TestNoGoroutineLeak(t *testing.T) {
	store, _ := storage.CreateSQLiteStore(":memory:", nil)
	srv := NewServer(store)

	// 等待 Server 初始化完成（连接池、后台任务等）
	time.Sleep(100 * time.Millisecond)
	runtime.GC()

	// 记录初始 goroutine 数量（在 Server 初始化之后）
	before := runtime.NumGoroutine()
	t.Logf("测试开始前 goroutine 数量: %d", before)

	// 场景1：正常请求（100次循环）
	t.Run("正常请求无泄漏", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{"result":"ok"}`))
		}))
		defer upstream.Close()

		cfg := &model.Config{ID: 1, URL: upstream.URL}

		for i := 0; i < 100; i++ {
			recorder := httptest.NewRecorder()
			_, _, _ = srv.forwardOnceAsync(
				context.Background(),
				cfg,
				"sk-test",
				http.MethodPost,
				[]byte(`{}`),
				http.Header{},
				"",
				"/v1/messages",
				recorder,
			)
		}

		runtime.GC()
		time.Sleep(100 * time.Millisecond) // 等待清理
		after := runtime.NumGoroutine()
		t.Logf("100次正常请求后 goroutine 数量: %d (增加: %d)", after, after-before)

		// 容忍5个辅助 goroutine（GC、网络连接池等）
		if after > before+5 {
			t.Errorf("❌ Goroutine 泄漏: %d -> %d (增加 %d)", before, after, after-before)
		}
	})

	// 场景2：客户端取消（50次循环）
	t.Run("客户端取消无泄漏", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond) // 模拟慢响应
			w.WriteHeader(200)
			w.Write([]byte(`{"result":"ok"}`))
		}))
		defer upstream.Close()

		cfg := &model.Config{ID: 1, URL: upstream.URL}

		for i := 0; i < 50; i++ {
			ctx, cancel := context.WithCancel(context.Background())
			recorder := httptest.NewRecorder()

			// 50ms 后取消请求
			go func() {
				time.Sleep(50 * time.Millisecond)
				cancel()
			}()

			srv.forwardOnceAsync(ctx, cfg, "sk-test", http.MethodPost, []byte(`{}`), http.Header{}, "", "/v1/messages", recorder)
		}

		runtime.GC()
		time.Sleep(200 * time.Millisecond) // 等待所有请求结束
		after := runtime.NumGoroutine()
		t.Logf("50次取消请求后 goroutine 数量: %d (增加: %d)", after, after-before)

		if after > before+5 {
			t.Errorf("❌ Goroutine 泄漏: %d -> %d (增加 %d)", before, after, after-before)
		}
	})

	// 场景3：首字节超时（20次循环）
	t.Run("首字节超时无泄漏", func(t *testing.T) {
		srv.firstByteTimeout = 50 * time.Millisecond // 设置超时

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond) // 故意超时
			w.WriteHeader(200)
		}))
		defer upstream.Close()

		cfg := &model.Config{ID: 1, URL: upstream.URL}

		for i := 0; i < 20; i++ {
			recorder := httptest.NewRecorder()
			srv.forwardOnceAsync(
				context.Background(),
				cfg,
				"sk-test",
				http.MethodPost,
				[]byte(`{"stream":true}`), // 流式请求
				http.Header{},
				"",
				"/v1/messages",
				recorder,
			)
		}

		srv.firstByteTimeout = 0 // 恢复默认
		runtime.GC()
		time.Sleep(300 * time.Millisecond) // 等待所有超时清理
		after := runtime.NumGoroutine()
		t.Logf("20次超时请求后 goroutine 数量: %d (增加: %d)", after, after-before)

		if after > before+5 {
			t.Errorf("❌ Goroutine 泄漏: %d -> %d (增加 %d)", before, after, after-before)
		}
	})
}
