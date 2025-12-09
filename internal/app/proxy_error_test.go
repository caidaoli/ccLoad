package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

// Test_HandleProxyError_Basic 基础错误处理测试(不依赖数据库)
func Test_HandleProxyError_Basic(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		statusCode     int
		expectedAction cooldown.Action
		shouldRetry    bool
	}{
		{
			name:           "context canceled",
			err:            context.Canceled,
			expectedAction: cooldown.ActionReturnClient,
			shouldRetry:    false,
		},
		{
			name:           "connection refused",
			err:            errors.New("connection refused"),
			expectedAction: cooldown.ActionRetryChannel,
			shouldRetry:    true,
		},
		{
			name:           "401 unauthorized - 单Key升级为渠道级",
			statusCode:     401,
			expectedAction: cooldown.ActionRetryChannel, // 单Key时升级为渠道级
			shouldRetry:    true,
		},
		{
			name:           "500 server error",
			statusCode:     500,
			expectedAction: cooldown.ActionRetryChannel,
			shouldRetry:    true,
		},
		{
			name:           "404 not found",
			statusCode:     404,
			expectedAction: cooldown.ActionReturnClient,
			shouldRetry:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, cleanup := setupTestServer(t)
			defer cleanup()

			// 添加必要的组件
			srv.cooldownManager = cooldown.NewManager(srv.store, nil)

			ctx := context.Background()
			cfg := &model.Config{
				ID:       1,
				Name:     "test",
				URL:      "http://test.example.com",
				Priority: 1,
				Enabled:  true,
			}

			var res *fwResult
			var err error

			if tt.statusCode > 0 {
				res = &fwResult{
					Status: tt.statusCode,
					Body:   []byte(`{"error": "test"}`),
					Header: make(http.Header),
				}
			} else {
				err = tt.err
			}

			action, shouldRetry := srv.handleProxyError(ctx, cfg, 0, res, err)

			if action != tt.expectedAction {
				t.Errorf("期望 action=%v, 实际=%v", tt.expectedAction, action)
			}
			if shouldRetry != tt.shouldRetry {
				t.Errorf("期望 shouldRetry=%v, 实际=%v", tt.shouldRetry, shouldRetry)
			}
		})
	}
}

// Test_HandleNetworkError_Basic 基础网络错误处理测试
func Test_HandleNetworkError_Basic(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	srv.cooldownManager = cooldown.NewManager(srv.store, nil)

	ctx := context.Background()
	cfg := &model.Config{
		ID:       1,
		Name:     "test",
		URL:      "http://test.example.com",
		Priority: 1,
		Enabled:  true,
	}

	t.Run("context canceled returns client error", func(t *testing.T) {
		result, retryKey, retryChannel := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, 0.1, context.Canceled,
		)

		if result == nil {
			t.Error("期望返回错误结果")
		}
		if retryKey {
			t.Error("期望 retryKey=false")
		}
		if retryChannel {
			t.Error("期望 retryChannel=false")
		}
	})

	t.Run("network error switches channel", func(t *testing.T) {
		result, retryKey, retryChannel := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, 0.1, errors.New("connection refused"),
		)

		if result != nil {
			t.Error("期望 result=nil (切换渠道)")
		}
		if retryKey {
			t.Error("期望 retryKey=false")
		}
		if !retryChannel {
			t.Error("期望 retryChannel=true")
		}
	})

	t.Run("first byte timeout switches channel", func(t *testing.T) {
		err := fmt.Errorf("wrap: %w", util.ErrUpstreamFirstByteTimeout)
		result, retryKey, retryChannel := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, 0.1, err,
		)

		if result != nil {
			t.Error("期望 result=nil (切换渠道)")
		}
		if retryKey {
			t.Error("期望 retryKey=false")
		}
		if !retryChannel {
			t.Error("期望 retryChannel=true")
		}
	})
}

// Test_HandleProxySuccess_Basic 基础成功处理测试
func Test_HandleProxySuccess_Basic(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	srv.cooldownManager = cooldown.NewManager(srv.store, nil)

	ctx := context.Background()
	cfg := &model.Config{
		ID:       1,
		Name:     "test",
		URL:      "http://test.example.com",
		Priority: 1,
		Enabled:  true,
	}

	res := &fwResult{
		Status:        200,
		Body:          []byte(`{"content": "success"}`),
		Header:        make(http.Header),
		FirstByteTime: 0.05,
	}

	// 创建测试用的请求上下文（新增参数，2025-11）
	reqCtx := &proxyRequestContext{
		tokenHash: "", // 测试环境无需Token统计
	}

	result, retryKey, retryChannel := srv.handleProxySuccess(
		ctx, cfg, 0, "test-model", "test-key", res, 0.1, reqCtx,
	)

	if result == nil {
		t.Fatal("期望返回成功结果")
	}
	if result.status != 200 {
		t.Errorf("期望 status=200, 实际=%d", result.status)
	}
	if !result.succeeded {
		t.Error("期望 succeeded=true")
	}
	if retryKey {
		t.Error("期望 retryKey=false")
	}
	if retryChannel {
		t.Error("期望 retryChannel=false")
	}
}

// Test_HandleProxyError_499 测试499状态码处理
func Test_HandleProxyError_499(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	srv.cooldownManager = cooldown.NewManager(srv.store, nil)

	ctx := context.Background()
	cfg := &model.Config{
		ID:       1,
		Name:     "test",
		URL:      "http://test.example.com",
		Priority: 1,
		Enabled:  true,
	}

	t.Run("upstream 499 triggers channel retry", func(t *testing.T) {
		res := &fwResult{
			Status: 499,
			Body:   []byte(`{"error": "client closed request"}`),
			Header: make(http.Header),
		}
		action, shouldRetry := srv.handleProxyError(ctx, cfg, 0, res, nil)

		if action != cooldown.ActionRetryChannel {
			t.Errorf("期望 action=ActionRetryChannel, 实际=%v", action)
		}
		if !shouldRetry {
			t.Error("期望 shouldRetry=true")
		}
	})

	t.Run("client canceled returns to client", func(t *testing.T) {
		action, shouldRetry := srv.handleProxyError(ctx, cfg, 0, nil, context.Canceled)

		if action != cooldown.ActionReturnClient {
			t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
		}
		if shouldRetry {
			t.Error("期望 shouldRetry=false")
		}
	})
}
