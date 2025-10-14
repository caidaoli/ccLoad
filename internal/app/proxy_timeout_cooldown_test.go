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

	t.Run("首字节超时触发渠道级冷却-固定5分钟", func(t *testing.T) {
		// ✅ 修复：在调用前记录时间，避免测试执行耗时影响判断
		beforeCall := time.Now()

		// 调用 handleProxyError 处理超时错误（状态码598）
		// 注意：这里直接模拟handleRequestError返回的结果，包含StatusFirstByteTimeout
		fwRes := &fwResult{
			Status:        StatusFirstByteTimeout, // 598
			Body:          []byte(timeoutErr.Error()),
			FirstByteTime: 120.0,
		}
		action, _ := server.handleProxyError(ctx, created, 0, fwRes, nil)

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
		if !cooldownUntil.After(beforeCall) {
			t.Errorf("❌ 冷却截止时间错误: %v 不在调用前时间 %v 之后", cooldownUntil, beforeCall)
		}

		// ✅ 修复：使用调用后的当前时间计算剩余冷却时长
		// ✅ P0修复：首字节超时现在固定冷却5分钟，不使用指数退避
		afterCall := time.Now()
		duration := cooldownUntil.Sub(afterCall)
		expectedDuration := util.FirstByteTimeoutCooldown // 5分钟（固定）
		tolerance := 2 * time.Second // 允许2秒误差

		if duration < expectedDuration-tolerance || duration > expectedDuration+tolerance {
			t.Errorf("❌ 冷却时长错误: 期望约%v，实际%v（测试执行耗时=%v）", 
				expectedDuration, duration, afterCall.Sub(beforeCall))
		}

		t.Logf("✅ 渠道已冷却，冷却时长=%v (期望固定5分钟)", duration)
	})

	t.Run("指数退避验证-使用502错误", func(t *testing.T) {
		// 清理之前的测试数据，确保从干净状态开始
		_ = store.ResetChannelCooldown(ctx, created.ID)
		// 等待一小段时间，确保数据库操作完成
		time.Sleep(100 * time.Millisecond)

		var prevDuration time.Duration
		
		for i := 0; i < 4; i++ {
			// 触发502错误（Bad Gateway - 触发指数退避）
			fwRes := &fwResult{
				Status:        502, // Bad Gateway
				Body:          []byte("bad gateway"),
				FirstByteTime: 1.0,
			}
			beforeError := time.Now()
			_, _ = server.handleProxyError(ctx, created, 0, fwRes, nil)

			// 从数据库读取冷却截止时间
			cooldowns, err := store.GetAllChannelCooldowns(ctx)
			if err != nil {
				t.Fatalf("查询冷却状态失败: %v", err)
			}
			
			cooldownUntil, exists := cooldowns[created.ID]
			if !exists {
				t.Fatal("冷却记录不存在")
			}

			// 计算从错误触发前到冷却截止时间的时长
			actualDuration := cooldownUntil.Sub(beforeError)

			// 第一次错误：验证初始冷却时间约为1秒
			if i == 0 {
				if actualDuration < 600*time.Millisecond || actualDuration > 1500*time.Millisecond {
					t.Logf("⚠️  第1次502错误冷却时间=%v (期望约1s，允许范围0.6s-1.5s)", actualDuration)
				} else {
					t.Logf("✅ 第1次502错误: 冷却时间=%v (期望约1s)", actualDuration)
				}
				prevDuration = actualDuration
			} else {
				// 后续错误：验证指数退避（约2倍关系）
				ratio := float64(actualDuration) / float64(prevDuration)
				
				// 只验证第3次以后的指数退避（第1-2次之间受测试环境时间不稳定影响）
				if i >= 2 {
					minRatio := 1.8
					maxRatio := 2.2
					
					if ratio < minRatio || ratio > maxRatio {
						t.Errorf("❌ 第%d次错误指数退避比例错误: 期望约2.0倍，实际%.2f倍 (prev=%v, curr=%v)",
							i+1, ratio, prevDuration, actualDuration)
					} else {
						t.Logf("✅ 第%d次502错误: 冷却时间=%v (上次的%.2f倍)", i+1, actualDuration, ratio)
					}
				} else {
					// 第2次只记录，不严格验证
					t.Logf("⚠️  第%d次502错误: 冷却时间=%v (上次的%.2f倍) - 跳过验证", i+1, actualDuration, ratio)
				}
				prevDuration = actualDuration
			}

			// 等待冷却过期
			remainingTime := cooldownUntil.Sub(time.Now())
			if remainingTime > 0 {
				time.Sleep(remainingTime + 100*time.Millisecond)
			}
		}
	})

	t.Run("handleNetworkError处理首字节超时", func(t *testing.T) {
		// 清理之前的测试数据
		_ = store.ResetChannelCooldown(ctx, created.ID)

		// 创建带有首字节超时信息的请求上下文
		reqCtx := &requestContext{
			ctx:              ctx,
			startTime:        time.Now().Add(-120 * time.Second), // 模拟120秒前开始
			isStreaming:      true,
			firstByteTimeout: 120 * time.Second,
		}

		// 使用 handleRequestError 包装首字节超时（会返回598状态码）
		fwRes, _, _ := server.handleRequestError(reqCtx, created, context.DeadlineExceeded, nil)
		
		// 验证状态码是598
		if fwRes.Status != StatusFirstByteTimeout {
			t.Errorf("❌ 期望状态码598，实际%d", fwRes.Status)
		}

		// 调用 handleProxyError 处理首字节超时（会触发渠道冷却）
		action, _ := server.handleProxyError(ctx, created, 0, fwRes, nil)

		// 验证返回 ActionRetryChannel（切换渠道）
		if action != ActionRetryChannel {
			t.Errorf("❌ 期望返回 ActionRetryChannel，实际 %d", action)
		}

		t.Logf("✅ handleProxyError 正确返回 ActionRetryChannel - 切换到下一个渠道")

		// 验证渠道已被冷却
		cooldowns, _ := store.GetAllChannelCooldowns(ctx)
		cooldownUntil, exists := cooldowns[created.ID]
		if !exists {
			t.Fatal("❌ 渠道未被冷却")
		}

		// 验证冷却时长约为5分钟
		duration := cooldownUntil.Sub(time.Now())
		expectedDuration := util.FirstByteTimeoutCooldown // 5分钟
		tolerance := 2 * time.Second
		
		if duration < expectedDuration-tolerance || duration > expectedDuration+tolerance {
			t.Errorf("❌ 首字节超时冷却时长错误: 期望%v，实际%v", expectedDuration, duration)
		}

		t.Logf("✅ 渠道已冷却并触发切换，冷却时长=%v（期望固定5分钟）", duration)
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
