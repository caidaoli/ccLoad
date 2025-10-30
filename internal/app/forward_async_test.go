package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
)

// TestMain 在所有测试运行前设置环境变量
func TestMain(m *testing.M) {
	// 为测试设置必需的环境变量
	os.Setenv("CCLOAD_PASS", "test_password_123")
	os.Setenv("CCLOAD_AUTH", "test_token_456")

	// 运行测试
	code := m.Run()

	// 清理
	os.Unsetenv("CCLOAD_PASS")
	os.Unsetenv("CCLOAD_AUTH")

	os.Exit(code)
}

// TestRequestContextCreation 测试请求上下文创建
func TestRequestContextCreation(t *testing.T) {
	store, _ := sqlite.NewSQLiteStore(":memory:", nil)
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
	store, _ := sqlite.NewSQLiteStore(":memory:", nil)
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
	store, _ := sqlite.NewSQLiteStore(":memory:", nil)
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
	store, _ := sqlite.NewSQLiteStore(":memory:", nil)
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
