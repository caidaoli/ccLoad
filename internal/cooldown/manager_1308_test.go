package cooldown

import (
	"bytes"
	"ccLoad/internal/model"
	"context"
	"log"
	"testing"
	"time"
)

func TestHandleError_1308Error(t *testing.T) {
	// 创建临时数据库
	store, cleanup := setupTestStore(t)
	defer cleanup()

	// 创建测试渠道
	ctx := context.Background()
	cfg := createTestChannel(t, store, "test-1308")

	// 创建2个API Key
	for i := 0; i < 2; i++ {
		_ = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID: cfg.ID,
			KeyIndex:  i,
			APIKey:    "sk-test-key-" + string(rune('0'+i)),
		})
	}

	// 创建Manager
	manager := NewManager(store, nil)

	t.Run("1308错误-自动解析重置时间", func(t *testing.T) {
		// 模拟1308错误响应
		errorBody := []byte(`{"type":"error","error":{"type":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2025-12-09 18:08:11 重置。"},"request_id":"xxx"}`)

		// 抑制日志输出
		var buf bytes.Buffer
		oldOutput := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(oldOutput)

		// 处理错误
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       0,
			StatusCode:     429,
			ErrorBody:      errorBody,
			IsNetworkError: false,
			Headers:        nil,
		})

		// 验证返回的Action
		if action != ActionRetryKey {
			t.Errorf("Expected ActionRetryKey, got %v", action)
		}

		// 查询API Key列表验证冷却状态
		keys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("Failed to get API keys: %v", err)
		}

		if len(keys) == 0 {
			t.Fatal("No API keys found")
		}

		// 验证Key 0的冷却时间
		key0 := keys[0]
		expectedTime, _ := time.ParseInLocation("2006-01-02 15:04:05", "2025-12-09 18:08:11", time.Local)

		// 由于时间是Unix秒，可能有秒级误差
		if key0.CooldownUntil == 0 {
			t.Error("Key cooldown was not set")
		}

		actualTime := time.Unix(key0.CooldownUntil, 0)
		timeDiff := actualTime.Sub(expectedTime).Abs()

		if timeDiff > 2*time.Second {
			t.Errorf("Cooldown time mismatch: got %v, want %v, diff=%v",
				actualTime.Format("2006-01-02 15:04:05"),
				expectedTime.Format("2006-01-02 15:04:05"),
				timeDiff)
		}

		t.Logf("[INFO] Key已正确禁用至: %s", actualTime.Format("2006-01-02 15:04:05"))
	})

	t.Run("普通429错误-使用指数退避", func(t *testing.T) {
		// 重置Key冷却状态
		if err := store.ResetKeyCooldown(ctx, cfg.ID, 1); err != nil {
			t.Fatalf("Failed to reset key cooldown: %v", err)
		}

		// 模拟普通429错误（非1308）
		errorBody := []byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`)

		// 抑制日志输出
		var buf bytes.Buffer
		oldOutput := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(oldOutput)

		// 记录处理前的时间
		beforeTime := time.Now()

		// 处理错误
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       1,
			StatusCode:     429,
			ErrorBody:      errorBody,
			IsNetworkError: false,
			Headers:        nil,
		})

		// 验证返回的Action
		if action != ActionRetryKey {
			t.Errorf("Expected ActionRetryKey, got %v", action)
		}

		// 查询API Key列表验证冷却状态
		keys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("Failed to get API keys: %v", err)
		}

		// 验证Key 1的冷却时间
		var key1 *model.APIKey
		for i := range keys {
			if keys[i].KeyIndex == 1 {
				key1 = keys[i]
				break
			}
		}

		if key1 == nil {
			t.Fatal("Key 1 not found")
		}

		if key1.CooldownUntil == 0 {
			t.Fatal("Key cooldown not set")
		}

		cooldownTime := time.Unix(key1.CooldownUntil, 0)
		duration := cooldownTime.Sub(beforeTime)

		// 验证冷却时间在合理范围内（应该是几秒到几分钟）
		// 注意：429错误第一次触发时，初始冷却时间可能较短（几秒）
		if duration < 1*time.Second || duration > 10*time.Minute {
			t.Errorf("Unexpected cooldown duration: %v", duration)
		}

		t.Logf("[INFO] 使用指数退避策略，冷却时间: %.1f分钟", duration.Minutes())
	})

	t.Run("1308错误-无效时间格式回退到指数退避", func(t *testing.T) {
		// 重置Key冷却状态
		if err := store.ResetKeyCooldown(ctx, cfg.ID, 0); err != nil {
			t.Fatalf("Failed to reset key cooldown: %v", err)
		}

		// 模拟1308错误但时间格式错误
		errorBody := []byte(`{"type":"error","error":{"type":"1308","message":"错误但没有正确的时间格式"},"request_id":"xxx"}`)

		// 抑制日志输出
		var buf bytes.Buffer
		oldOutput := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(oldOutput)

		// 记录处理前的时间
		beforeTime := time.Now()

		// 处理错误
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       0,
			StatusCode:     429,
			ErrorBody:      errorBody,
			IsNetworkError: false,
			Headers:        nil,
		})

		// 验证返回的Action
		if action != ActionRetryKey {
			t.Errorf("Expected ActionRetryKey, got %v", action)
		}

		// 查询API Key列表验证冷却状态
		keys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("Failed to get API keys: %v", err)
		}

		// 验证Key 0的冷却时间
		key0 := keys[0]

		if key0.CooldownUntil == 0 {
			t.Fatal("Key cooldown not set")
		}

		cooldownTime := time.Unix(key0.CooldownUntil, 0)
		duration := cooldownTime.Sub(beforeTime)

		// 验证回退到指数退避策略（冷却时间在合理范围内）
		// 注意：429错误第一次触发时，初始冷却时间可能较短（几秒）
		if duration < 1*time.Second || duration > 10*time.Minute {
			t.Errorf("Unexpected cooldown duration: %v", duration)
		}

		t.Logf("[INFO] 回退到指数退避策略，冷却时间: %.1f分钟", duration.Minutes())
	})

	t.Run("单Key渠道的1308错误-应该冷却Channel并使用精确时间", func(t *testing.T) {
		// 创建单Key渠道
		singleKeyCfg := createTestChannel(t, store, "single-key-1308")
		_ = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID: singleKeyCfg.ID,
			KeyIndex:  0,
			APIKey:    "sk-single-key",
		})

		// 模拟1308错误响应
		errorBody := []byte(`{"type":"error","error":{"type":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2025-12-09 18:08:11 重置。"},"request_id":"xxx"}`)

		// 抑制日志输出
		var buf bytes.Buffer
		oldOutput := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(oldOutput)

		// 处理错误
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      singleKeyCfg.ID,
			KeyIndex:       0,
			StatusCode:     429,
			ErrorBody:      errorBody,
			IsNetworkError: false,
			Headers:        nil,
		})

		// 验证返回的Action应该是RetryKey（虽然会升级为Channel级，但1308有精确时间时保持Key级）
		if action != ActionRetryKey {
			t.Errorf("Expected ActionRetryKey for single-key 1308, got %v", action)
		}

		// 验证Key的冷却时间
		keys, err := store.GetAPIKeys(ctx, singleKeyCfg.ID)
		if err != nil {
			t.Fatalf("Failed to get API keys: %v", err)
		}

		if len(keys) == 0 {
			t.Fatal("No API keys found")
		}

		key0 := keys[0]
		expectedTime, _ := time.ParseInLocation("2006-01-02 15:04:05", "2025-12-09 18:08:11", time.Local)

		if key0.CooldownUntil == 0 {
			t.Error("Key cooldown was not set")
		}

		actualTime := time.Unix(key0.CooldownUntil, 0)
		timeDiff := actualTime.Sub(expectedTime).Abs()

		if timeDiff > 2*time.Second {
			t.Errorf("Cooldown time mismatch: got %v, want %v, diff=%v",
				actualTime.Format("2006-01-02 15:04:05"),
				expectedTime.Format("2006-01-02 15:04:05"),
				timeDiff)
		}

		t.Logf("[INFO] 单Key渠道1308错误已正确处理，禁用至: %s", actualTime.Format("2006-01-02 15:04:05"))
	})

	t.Run("code字段格式的1308错误-应该正确识别和冷却", func(t *testing.T) {
		// 重置Key冷却状态
		if err := store.ResetKeyCooldown(ctx, cfg.ID, 0); err != nil {
			t.Fatalf("Failed to reset key cooldown: %v", err)
		}

		// 模拟使用code字段的1308错误（非Anthropic格式）
		errorBody := []byte(`{"error":{"code":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2025-12-21 15:00:05 重置。"},"request_id":"202512211335142b05cc4f9bbb4e6c"}`)

		// 抑制日志输出
		var buf bytes.Buffer
		oldOutput := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(oldOutput)

		// 处理错误
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       0,
			StatusCode:     597,
			ErrorBody:      errorBody,
			IsNetworkError: false,
			Headers:        nil,
		})

		// 验证返回的Action
		if action != ActionRetryKey {
			t.Errorf("Expected ActionRetryKey, got %v", action)
		}

		// 查询API Key列表验证冷却状态
		keys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("Failed to get API keys: %v", err)
		}

		if len(keys) == 0 {
			t.Fatal("No API keys found")
		}

		// 验证Key 0的冷却时间
		key0 := keys[0]
		expectedTime, _ := time.ParseInLocation("2006-01-02 15:04:05", "2025-12-21 15:00:05", time.Local)

		// 由于时间是Unix秒，可能有秒级误差
		if key0.CooldownUntil == 0 {
			t.Error("Key cooldown was not set")
		}

		actualTime := time.Unix(key0.CooldownUntil, 0)
		timeDiff := actualTime.Sub(expectedTime).Abs()

		if timeDiff > 2*time.Second {
			t.Errorf("Cooldown time mismatch: got %v, want %v, diff=%v",
				actualTime.Format("2006-01-02 15:04:05"),
				expectedTime.Format("2006-01-02 15:04:05"),
				timeDiff)
		}

		t.Logf("[INFO] code字段格式的1308错误已正确识别，Key禁用至: %s", actualTime.Format("2006-01-02 15:04:05"))
	})
}
