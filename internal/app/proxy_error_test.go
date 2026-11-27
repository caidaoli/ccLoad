package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
)

// Test_HandleProxyError_Basic 基础错误处理测试(不依赖数据库)
func Test_HandleProxyError_Basic(t *testing.T) {
	tests := []struct {
		name           string
		statusCode     int
		expectedAction cooldown.Action
	}{
		{
			name:           "500 server error",
			statusCode:     500,
			expectedAction: cooldown.ActionRetryModel,
		},
		{
			name:           "404 not found - 渠道级",
			statusCode:     404,
			expectedAction: cooldown.ActionRetryChannel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newInMemoryServer(t)

			ctx := context.Background()
			cfg := &model.Config{
				ID:       1,
				Name:     "test",
				URLs:     model.ChannelURLs{{URL: "http://test.example.com"}},
				Priority: 1,
				Enabled:  true,
			}

			res := &fwResult{
				Status: tt.statusCode,
				Body:   []byte(`{"error": "test"}`),
				Header: make(http.Header),
			}
			action := srv.applyCooldownDecision(ctx, cfg, cooldownInputForModel(httpErrorInput(cfg.ID, 0, res), "test-model"))

			if action != tt.expectedAction {
				t.Errorf("期望 action=%v, 实际=%v", tt.expectedAction, action)
			}
		})
	}
}

type failingTokenStatsStore struct {
	storage.Store
	err error
}

func (s *failingTokenStatsStore) UpdateTokenStats(
	context.Context,
	string,
	model.TokenStatsOutcome,
	float64,
	bool,
	float64,
	int64,
	int64,
	int64,
	int64,
	int64,
	float64,
	float64,
	time.Time,
) error {
	return s.err
}

func TestApplyTokenStatsUpdateAddsCostToCacheWhenStoreFails(t *testing.T) {
	const tokenHash = "limited-token"

	auth := newTestAuthService(t)
	auth.authTokenCostLimits[tokenHash] = tokenCostLimit{
		usedMicroUSD:  0,
		limitMicroUSD: 1000,
	}

	srv := &Server{
		store:       &failingTokenStatsStore{err: errors.New("database down")},
		authService: auth,
	}

	srv.applyTokenStatsUpdate(tokenStatsUpdate{
		tokenHash:      tokenHash,
		completedAt:    time.Now(),
		outcome:        model.TokenStatsSuccess,
		costUSD:        0.0002,
		costMultiplier: 1,
	})

	used, limit, exceeded := auth.IsCostLimitExceeded(tokenHash)
	if want := util.USDToMicroUSD(0.0002); used != want {
		t.Fatalf("used=%d, want %d; DB failure must not block in-memory cost accounting", used, want)
	}
	if limit != 1000 || exceeded {
		t.Fatalf("limit/exceeded=(%d,%v), want (1000,false)", limit, exceeded)
	}
}

// Test_HandleNetworkError_Basic 基础网络错误处理测试
func Test_HandleNetworkError_Basic(t *testing.T) {
	srv := newInMemoryServer(t)

	ctx := context.Background()
	cfg := &model.Config{
		ID:       1,
		Name:     "test",
		URLs:     model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority: 1,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "test-model"},
			{Model: "other-model"},
		},
	}

	// 创建测试用的请求上下文
	reqCtx := &proxyRequestContext{
		originalModel: "test-model",
		tokenID:       0,
		clientIP:      "",
	}

	t.Run("context canceled returns client error", func(t *testing.T) {
		result, action := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, context.Canceled, nil, reqCtx, false,
		)

		if result == nil {
			t.Error("期望返回错误结果")
		} else if !result.isClientCanceled {
			t.Error("期望 isClientCanceled=true")
		}
		if action != cooldown.ActionReturnClient {
			t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
		}
	})

	t.Run("network error switches channel", func(t *testing.T) {
		result, action := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, errors.New("connection refused"), nil, reqCtx, false,
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

	modelScopedErrors := []struct {
		name string
		err  error
	}{
		{name: "first byte timeout", err: fmt.Errorf("wrap: %w", util.ErrUpstreamFirstByteTimeout)},
		{name: "connection reset", err: errors.New("read: connection reset by peer")},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
	}
	for _, tt := range modelScopedErrors {
		t.Run(tt.name+" cools model", func(t *testing.T) {
			result, action := srv.handleNetworkError(
				ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, tt.err, nil, reqCtx, false,
			)

			if result == nil {
				t.Fatal("期望返回错误结果")
			}
			if action != cooldown.ActionRetryModel {
				t.Fatalf("action=%v, want ActionRetryModel", action)
			}
		})
	}
}

// Test_HandleProxyError_499 测试499状态码处理
func Test_HandleProxyError_499(t *testing.T) {
	srv := newInMemoryServer(t)

	ctx := context.Background()
	cfg := &model.Config{
		ID:       1,
		Name:     "test",
		URLs:     model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority: 1,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "test-model"},
			{Model: "other-model"},
		},
	}

	t.Run("upstream 499 triggers model retry", func(t *testing.T) {
		res := &fwResult{
			Status: 499,
			Body:   []byte(`{"error": "client closed request"}`),
			Header: make(http.Header),
		}
		input := cooldownInputForModel(httpErrorInput(cfg.ID, 0, res), "test-model")
		action := srv.applyCooldownDecision(ctx, cfg, input)

		if action != cooldown.ActionRetryModel {
			t.Errorf("期望 action=ActionRetryModel, 实际=%v", action)
		}
	})
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
