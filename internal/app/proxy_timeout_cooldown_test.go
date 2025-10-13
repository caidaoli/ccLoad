package app

import (
	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
	"ccLoad/internal/util"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFirstByteTimeoutCooldown 验证首字节超时是否正确执行冷却
func TestFirstByteTimeoutCooldown(t *testing.T) {
	// 创建临时测试数据库
	store, cleanup := setupProxyTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试服务器（简化版，只测试冷却逻辑）
	server := &Server{
		store:            store,
		maxKeyRetries:    3,
		firstByteTimeout: 120 * time.Second,
	}

	// 创建测试渠道
	cfg := &model.Config{
		Name:    "test-timeout-channel",
		URL:     "https://api.example.com",
		Enabled: true,
		Models:  []string{"claude-3-5-sonnet-20241022"},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 创建API Key
	err = store.CreateAPIKey(ctx, &model.APIKey{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-test-timeout",
		KeyStrategy: "sequential",
	})
	if err != nil {
		t.Fatalf("创建API Key失败: %v", err)
	}

	// 模拟首字节超时错误
	timeoutErr := fmt.Errorf("first byte timeout after 120.23s (CCLOAD_FIRST_BYTE_TIMEOUT=2m0s): %w",
		context.DeadlineExceeded)

	t.Run("首字节超时错误分类", func(t *testing.T) {
		// 验证错误分类
		statusCode, shouldRetry := classifyError(timeoutErr)
		if statusCode != 504 {
			t.Errorf("❌ 错误分类失败: 期望状态码504，实际%d", statusCode)
		}
		if !shouldRetry {
			t.Errorf("❌ 错误分类失败: 期望shouldRetry=true，实际false")
		}
		t.Logf("✅ 首字节超时正确分类为 504 Gateway Timeout（可重试）")
	})

	t.Run("首字节超时触发渠道级冷却", func(t *testing.T) {
		now := time.Now()

		// 调用 handleProxyError 处理超时错误
		action, _ := server.handleProxyError(ctx, created, 0, nil, timeoutErr)

		// 验证返回 ActionRetryChannel（切换渠道）
		if action != ActionRetryChannel {
			t.Errorf("❌ 期望返回 ActionRetryChannel，实际 %d", action)
		}
		t.Logf("✅ handleProxyError 正确返回 ActionRetryChannel")

		// 验证渠道已被冷却
		cooldowns, err := store.GetAllChannelCooldowns(ctx)
		if err != nil {
			t.Fatalf("查询冷却状态失败: %v", err)
		}

		cooldownUntil, exists := cooldowns[created.ID]
		if !exists {
			t.Fatal("❌ 渠道未被冷却")
		}

		// 验证冷却截止时间在未来
		if !cooldownUntil.After(now) {
			t.Errorf("❌ 冷却截止时间错误: %v 不在当前时间 %v 之后", cooldownUntil, now)
		}

		// 验证冷却时长约为1秒（504错误的初始冷却时长）
		duration := cooldownUntil.Sub(now)
		expectedDuration := util.OtherErrorInitialCooldown // 1秒
		tolerance := 100 * time.Millisecond

		if duration < expectedDuration-tolerance || duration > expectedDuration+tolerance {
			t.Errorf("❌ 冷却时长错误: 期望约%v，实际%v", expectedDuration, duration)
		}

		t.Logf("✅ 渠道已冷却，冷却时长=%v (期望约%v)", duration, expectedDuration)
	})

	t.Run("指数退避验证", func(t *testing.T) {
		// 清理之前的测试数据
		_ = store.ResetChannelCooldown(ctx, created.ID)

		// 预期的退避序列：1s → 2s → 4s → 8s
		expectedSequence := []time.Duration{
			1 * time.Second,
			2 * time.Second,
			4 * time.Second,
			8 * time.Second,
		}

		currentTime := time.Now()
		for i, expected := range expectedSequence {
			// 触发超时错误
			_, _ = server.handleProxyError(ctx, created, 0, nil, timeoutErr)

			// 验证冷却时长
			cooldowns, _ := store.GetAllChannelCooldowns(ctx)
			cooldownUntil := cooldowns[created.ID]
			duration := cooldownUntil.Sub(currentTime)

			tolerance := 200 * time.Millisecond
			if duration < expected-tolerance || duration > expected+tolerance {
				t.Errorf("❌ 第%d次错误冷却时间错误: 期望约%v，实际%v",
					i+1, expected, duration)
			}

			t.Logf("✅ 第%d次超时: 冷却时间=%v (期望约%v)", i+1, duration, expected)

			// 模拟时间推移（冷却过期后再次尝试）
			currentTime = currentTime.Add(expected + 1*time.Second)
		}
	})

	t.Run("handleNetworkError处理首字节超时", func(t *testing.T) {
		// 清理之前的测试数据
		_ = store.ResetChannelCooldown(ctx, created.ID)

		// 调用 handleNetworkError 处理超时错误
		result, shouldContinue, shouldBreak := server.handleNetworkError(
			ctx, created, 0, "claude-3-5-sonnet-20241022",
			"sk-test-***", 120.5, timeoutErr)

		// 验证返回值
		if result != nil {
			t.Errorf("❌ 期望返回 result=nil，实际非nil")
		}
		if shouldContinue {
			t.Errorf("❌ 期望 shouldContinue=false，实际true")
		}
		if !shouldBreak {
			t.Errorf("❌ 期望 shouldBreak=true（切换渠道），实际false")
		}

		t.Logf("✅ handleNetworkError 正确返回 (nil, false, true) - 切换到下一个渠道")

		// 验证渠道已被冷却
		cooldowns, _ := store.GetAllChannelCooldowns(ctx)
		if _, exists := cooldowns[created.ID]; !exists {
			t.Fatal("❌ 渠道未被冷却")
		}

		t.Logf("✅ 渠道已冷却并触发切换")
	})

	t.Run("非首字节超时不触发渠道冷却", func(t *testing.T) {
		// 清理之前的测试数据
		_ = store.ResetChannelCooldown(ctx, created.ID)

		// 模拟客户端主动取消（不应触发冷却）
		clientCancelErr := context.Canceled

		action, _ := server.handleProxyError(ctx, created, 0, nil, clientCancelErr)

		// 验证返回 ActionReturnClient（不冷却）
		if action != ActionReturnClient {
			t.Errorf("❌ 客户端取消应返回 ActionReturnClient，实际 %d", action)
		}

		// 验证渠道未被冷却
		cooldowns, _ := store.GetAllChannelCooldowns(ctx)
		if _, exists := cooldowns[created.ID]; exists {
			t.Errorf("❌ 客户端取消不应冷却渠道")
		}

		t.Logf("✅ 客户端取消正确处理（不冷却）")
	})
}

// TestFirstByteTimeoutErrorMessage 验证首字节超时错误消息格式
func TestFirstByteTimeoutErrorMessage(t *testing.T) {
	testCases := []struct {
		name           string
		errorMsg       string
		expectedStatus int
		expectedRetry  bool
	}{
		{
			name:           "标准首字节超时错误（CCLOAD_FIRST_BYTE_TIMEOUT）",
			errorMsg:       "first byte timeout after 120.23s (CCLOAD_FIRST_BYTE_TIMEOUT=2m0s): context deadline exceeded",
			expectedStatus: 504,
			expectedRetry:  true,
		},
		{
			name:           "响应头超时（Transport.ResponseHeaderTimeout）",
			errorMsg:       "context deadline exceeded", // ✅ 修复：模拟真实的超时错误
			expectedStatus: 504,
			expectedRetry:  true,
		},
		{
			name:           "普通DeadlineExceeded（P0修复：现在应该重试）",
			errorMsg:       "context deadline exceeded",
			expectedStatus: 504, // ✅ 从499改为504
			expectedRetry:  true, // ✅ 从false改为true
		},
		{
			name:           "客户端主动取消（不应重试）",
			errorMsg:       "context canceled",
			expectedStatus: StatusClientClosedRequest,
			expectedRetry:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if strings.Contains(tc.errorMsg, "canceled") {
				err = context.Canceled
			} else if strings.Contains(tc.errorMsg, "deadline") {
				if strings.Contains(tc.errorMsg, "first byte timeout") || strings.Contains(tc.errorMsg, "awaiting headers") {
					// 包装为 DeadlineExceeded 错误
					err = fmt.Errorf("%s: %w", tc.errorMsg, context.DeadlineExceeded)
				} else {
					err = context.DeadlineExceeded
				}
			} else {
				err = errors.New(tc.errorMsg)
			}

			status, retry := classifyError(err)
			if status != tc.expectedStatus {
				t.Errorf("❌ %s: 期望状态码%d，实际%d", tc.name, tc.expectedStatus, status)
			}
			if retry != tc.expectedRetry {
				t.Errorf("❌ %s: 期望重试=%v，实际%v", tc.name, tc.expectedRetry, retry)
			}
			t.Logf("✅ %s: 状态码=%d, 重试=%v", tc.name, status, retry)
		})
	}
}

// setupProxyTestStore 创建临时测试数据库（专用于proxy测试）
func setupProxyTestStore(t *testing.T) (*sqlite.SQLiteStore, func()) {
	tmpDB := t.TempDir() + "/test-proxy.db"
	store, err := sqlite.NewSQLiteStore(tmpDB, nil)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	cleanup := func() {
		_ = store.Close()
	}

	return store, cleanup
}
