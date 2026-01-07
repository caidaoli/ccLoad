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
	}{
		{
			name:           "context canceled",
			err:            context.Canceled,
			expectedAction: cooldown.ActionReturnClient,
		},
		{
			name:           "connection refused",
			err:            errors.New("connection refused"),
			expectedAction: cooldown.ActionRetryChannel,
		},
		{
			name:           "401 unauthorized - 单Key升级为渠道级",
			statusCode:     401,
			expectedAction: cooldown.ActionRetryChannel, // 单Key时升级为渠道级
		},
		{
			name:           "500 server error",
			statusCode:     500,
			expectedAction: cooldown.ActionRetryChannel,
		},
		{
			name:           "404 not found",
			statusCode:     404,
			expectedAction: cooldown.ActionReturnClient,
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

			var action cooldown.Action
			if err != nil {
				reqCtx := &proxyRequestContext{
					originalModel: "test-model",
				}
				_, action = srv.handleNetworkError(ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, err, nil, reqCtx)
			} else {
				action = srv.applyCooldownDecision(ctx, cfg, httpErrorInput(cfg.ID, 0, res))
			}

			if action != tt.expectedAction {
				t.Errorf("期望 action=%v, 实际=%v", tt.expectedAction, action)
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

	// 创建测试用的请求上下文
	reqCtx := &proxyRequestContext{
		originalModel: "test-model",
		tokenID:       0,
		clientIP:      "",
	}

	t.Run("context canceled returns client error", func(t *testing.T) {
		result, action := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, context.Canceled, nil, reqCtx,
		)

		if result == nil {
			t.Error("期望返回错误结果")
		}
		if action != cooldown.ActionReturnClient {
			t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
		}
	})

	t.Run("network error switches channel", func(t *testing.T) {
		result, action := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, errors.New("connection refused"), nil, reqCtx,
		)

		if result == nil {
			t.Error("期望返回错误结果")
		}
		if result != nil && result.status != http.StatusBadGateway {
			t.Errorf("期望 status=502, 实际=%d", result.status)
		}
		if action != cooldown.ActionRetryChannel {
			t.Errorf("期望 action=ActionRetryChannel, 实际=%v", action)
		}
	})

	t.Run("first byte timeout switches channel", func(t *testing.T) {
		err := fmt.Errorf("wrap: %w", util.ErrUpstreamFirstByteTimeout)
		result, action := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, err, nil, reqCtx,
		)

		if result == nil {
			t.Error("期望返回错误结果")
		}
		if result != nil && result.status != util.StatusFirstByteTimeout {
			t.Errorf("期望 status=%d, 实际=%d", util.StatusFirstByteTimeout, result.status)
		}
		if action != cooldown.ActionRetryChannel {
			t.Errorf("期望 action=ActionRetryChannel, 实际=%v", action)
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

	result, action := srv.handleProxySuccess(
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
	if action != cooldown.ActionReturnClient {
		t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
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
		action := srv.applyCooldownDecision(ctx, cfg, httpErrorInput(cfg.ID, 0, res))

		if action != cooldown.ActionRetryChannel {
			t.Errorf("期望 action=ActionRetryChannel, 实际=%v", action)
		}
	})

	t.Run("client canceled returns to client", func(t *testing.T) {
		reqCtx := &proxyRequestContext{
			originalModel: "test-model",
		}
		_, action := srv.handleNetworkError(ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, context.Canceled, nil, reqCtx)

		if action != cooldown.ActionReturnClient {
			t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
		}
	})
}

// Test_HandleNetworkError_499_PreservesTokenStats 测试 499 场景下 token 统计被保留
// [FIX] 2025-12: 修复流式响应中途取消时 token 统计丢失的问题
func Test_HandleNetworkError_499_PreservesTokenStats(t *testing.T) {
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

	// 模拟流式响应中途取消的场景：已解析到 token 统计
	res := &fwResult{
		Status:                   200,
		InputTokens:              100,
		OutputTokens:             50,
		CacheReadInputTokens:     200,
		CacheCreationInputTokens: 30,
		FirstByteTime:            0.1,
	}

	// 创建带有 tokenHash 的请求上下文
	tokenHash := "test-token-hash-499"
	reqCtx := &proxyRequestContext{
		tokenHash:   tokenHash,
		isStreaming: true,
	}

	// 调用 handleNetworkError，传入 res 和 reqCtx
	result, action := srv.handleNetworkError(
		ctx, cfg, 0, "claude-sonnet-4-5", "test-key", 0, "", 0.5, context.Canceled, res, reqCtx,
	)

	// 验证返回值正确
	if result == nil {
		t.Error("期望返回错误结果")
	}
	if result != nil && !result.isClientCanceled {
		t.Error("期望 isClientCanceled=true")
	}
	if action != cooldown.ActionReturnClient {
		t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
	}

	// 验证 hasConsumedTokens 函数
	if !hasConsumedTokens(res) {
		t.Error("hasConsumedTokens 应返回 true")
	}
	if hasConsumedTokens(nil) {
		t.Error("hasConsumedTokens(nil) 应返回 false")
	}
	if hasConsumedTokens(&fwResult{}) {
		t.Error("hasConsumedTokens(空结果) 应返回 false")
	}
}

func TestCooldownWriteContext_DetachesCancelButPreservesValues(t *testing.T) {
	type ctxKey string

	const key ctxKey = "k"
	baseCtx := context.WithValue(context.Background(), key, "v")
	canceledCtx, cancel := context.WithCancel(baseCtx)
	cancel()

	ctx, cancel := cooldownWriteContext(canceledCtx)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatalf("cooldownWriteContext 不应立即继承取消信号: err=%v", ctx.Err())
	default:
	}

	if got := ctx.Value(key); got != "v" {
		t.Fatalf("cooldownWriteContext 应保留 ctx.Value: got=%v", got)
	}
}
