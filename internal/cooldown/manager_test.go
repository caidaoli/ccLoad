package cooldown

import (
	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
	"context"
	"os"
	"testing"
	"time"
)

// TestNewManager 测试管理器创建
func TestNewManager(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	manager := NewManager(store)
	if manager == nil {
		t.Fatal("NewManager should not return nil")
	}
	if manager.store == nil {
		t.Error("Manager.store should not be nil")
	}
}

// TestHandleError_ClientError 测试客户端错误处理（不冷却）
func TestHandleError_ClientError(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store)
	ctx := context.Background()

	// 创建测试渠道
	cfg := createTestChannel(t, store, "test-client-error")

	testCases := []struct {
		name       string
		statusCode int
		errorBody  []byte
	}{
		{"404未找到", 404, []byte(`{"error":"not found"}`)},
		{"405方法不允许", 405, []byte(`{"error":"method not allowed"}`)},
		// 注意：400 Bad Request 被分类为Key级错误（API Key格式错误），不是客户端错误
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			action, err := manager.HandleError(ctx, cfg.ID, 0, tc.statusCode, tc.errorBody, false)

			if err != nil {
				t.Errorf("HandleError should not return error for client errors: %v", err)
			}

			if action != ActionReturnClient {
				t.Errorf("Expected ActionReturnClient for %d, got %v", tc.statusCode, action)
			}

			// 验证未冷却
			channelCfg, _ := store.GetConfig(ctx, cfg.ID)
			if channelCfg.CooldownUntil > 0 {
				t.Errorf("Client error should not trigger cooldown")
			}
		})
	}
}

// TestHandleError_KeyLevelError 测试Key级错误处理
func TestHandleError_KeyLevelError(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store)
	ctx := context.Background()

	// 创建多Key渠道（3个Key）
	cfg := createTestChannel(t, store, "test-key-error")
	for i := 0; i < 3; i++ {
		_ = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-key-" + string(rune('0'+i)),
			KeyStrategy: "sequential",
		})
	}

	testCases := []struct {
		name       string
		statusCode int
		errorBody  []byte
	}{
		{"401未授权", 401, []byte(`{"error":{"type":"authentication_error"}}`)},
		{"403禁止访问", 403, []byte(`{"error":{"type":"permission_error"}}`)},
		{"429限流", 429, []byte(`{"error":{"type":"rate_limit_error"}}`)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			keyIndex := 0
			action, err := manager.HandleError(ctx, cfg.ID, keyIndex, tc.statusCode, tc.errorBody, false)

			if err != nil {
				t.Errorf("HandleError failed: %v", err)
			}

			if action != ActionRetryKey {
				t.Errorf("Expected ActionRetryKey for %d, got %v", tc.statusCode, action)
			}

			// 验证Key被冷却
			cooldownUntil, exists := store.GetKeyCooldownUntil(ctx, cfg.ID, keyIndex)
			if !exists || cooldownUntil.Before(time.Now()) {
				t.Errorf("Key should be cooled down for status %d", tc.statusCode)
			}

			// 验证渠道未被冷却
			channelCfg, _ := store.GetConfig(ctx, cfg.ID)
			if channelCfg.CooldownUntil > 0 && time.Unix(channelCfg.CooldownUntil, 0).After(time.Now()) {
				t.Errorf("Channel should not be cooled down for key-level error")
			}
		})
	}
}

// TestHandleError_ChannelLevelError 测试渠道级错误处理
func TestHandleError_ChannelLevelError(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-channel-error")

	testCases := []struct {
		name       string
		statusCode int
		errorBody  []byte
	}{
		{"500内部错误", 500, []byte(`{"error":"internal server error"}`)},
		{"502网关错误", 502, []byte(`{"error":"bad gateway"}`)},
		{"503服务不可用", 503, []byte(`{"error":"service unavailable"}`)},
		{"504网关超时", 504, []byte(`{"error":"gateway timeout"}`)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// 先重置冷却
			_ = store.ResetChannelCooldown(ctx, cfg.ID)

			action, err := manager.HandleError(ctx, cfg.ID, -1, tc.statusCode, tc.errorBody, false)

			if err != nil {
				t.Errorf("HandleError failed: %v", err)
			}

			if action != ActionRetryChannel {
				t.Errorf("Expected ActionRetryChannel for %d, got %v", tc.statusCode, action)
			}

			// 验证渠道被冷却
			channelCfg, _ := store.GetConfig(ctx, cfg.ID)
			if channelCfg.CooldownUntil == 0 || time.Unix(channelCfg.CooldownUntil, 0).Before(time.Now()) {
				t.Errorf("Channel should be cooled down for status %d", tc.statusCode)
			}
		})
	}
}

// TestHandleError_SingleKeyUpgrade 测试单Key渠道的Key级错误自动升级
func TestHandleError_SingleKeyUpgrade(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store)
	ctx := context.Background()

	// 创建单Key渠道
	cfg := createTestChannel(t, store, "test-single-key")
	_ = store.CreateAPIKey(ctx, &model.APIKey{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-only-key",
		KeyStrategy: "sequential",
	})

	// 401认证错误本应是Key级，但单Key渠道应升级为渠道级
	action, err := manager.HandleError(ctx, cfg.ID, 0, 401, []byte(`{"error":{"type":"authentication_error"}}`), false)

	if err != nil {
		t.Fatalf("HandleError failed: %v", err)
	}

	// ✅ 关键断言：单Key渠道应升级为渠道级错误
	if action != ActionRetryChannel {
		t.Errorf("Expected ActionRetryChannel for single-key channel, got %v", action)
	}

	// 验证渠道被冷却（而不是Key）
	channelCfg, _ := store.GetConfig(ctx, cfg.ID)
	if channelCfg.CooldownUntil == 0 {
		t.Error("Single-key channel should be cooled down at channel level")
	}
}

// TestHandleError_NetworkError 测试网络错误处理
func TestHandleError_NetworkError(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-network-error")

	testCases := []struct {
		name           string
		statusCode     int
		expectedAction Action
		description    string
	}{
		{
			name:           "首字节超时(598)",
			statusCode:     598,
			expectedAction: ActionRetryChannel,
			description:    "First byte timeout should trigger channel-level cooldown",
		},
		{
			name:           "网关超时(504)",
			statusCode:     504,
			expectedAction: ActionRetryChannel,
			description:    "Gateway timeout should trigger channel-level cooldown",
		},
		{
			name:           "连接重置(ErrCodeNetworkRetryable)",
			statusCode:     -1,
			expectedAction: ActionRetryKey,
			description:    "Other network errors should be key-level",
		},
	}

	// 为测试连接重置场景，创建多Key渠道
	for i := 0; i < 2; i++ {
		_ = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-net-key-" + string(rune('0'+i)),
			KeyStrategy: "sequential",
		})
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// 重置冷却
			_ = store.ResetChannelCooldown(ctx, cfg.ID)

			action, err := manager.HandleError(ctx, cfg.ID, 0, tc.statusCode, nil, true)

			if err != nil {
				t.Errorf("HandleError failed: %v", err)
			}

			if action != tc.expectedAction {
				t.Errorf("%s: expected %v, got %v", tc.description, tc.expectedAction, action)
			}
		})
	}
}

// TestClearChannelCooldown 测试清除渠道冷却
func TestClearChannelCooldown(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-clear-channel")

	// 先触发冷却
	_, _ = manager.HandleError(ctx, cfg.ID, -1, 500, nil, false)

	// 验证已冷却
	channelCfg, _ := store.GetConfig(ctx, cfg.ID)
	if channelCfg.CooldownUntil == 0 {
		t.Fatal("Channel should be cooled down")
	}

	// 清除冷却
	err := manager.ClearChannelCooldown(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("ClearChannelCooldown failed: %v", err)
	}

	// 验证已清除
	channelCfg, _ = store.GetConfig(ctx, cfg.ID)
	if channelCfg.CooldownUntil != 0 {
		t.Error("Channel cooldown should be cleared")
	}
}

// TestClearKeyCooldown 测试清除Key冷却
func TestClearKeyCooldown(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-clear-key")
	_ = store.CreateAPIKey(ctx, &model.APIKey{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-test-clear",
		KeyStrategy: "sequential",
	})
	_ = store.CreateAPIKey(ctx, &model.APIKey{
		ChannelID:   cfg.ID,
		KeyIndex:    1,
		APIKey:      "sk-test-clear-2",
		KeyStrategy: "sequential",
	})

	// 先触发Key冷却
	_, _ = manager.HandleError(ctx, cfg.ID, 0, 401, []byte(`{"error":{"type":"authentication_error"}}`), false)

	// 验证已冷却
	cooldownUntil, exists := store.GetKeyCooldownUntil(ctx, cfg.ID, 0)
	if !exists || cooldownUntil.Before(time.Now()) {
		t.Fatal("Key should be cooled down")
	}

	// 清除冷却
	err := manager.ClearKeyCooldown(ctx, cfg.ID, 0)
	if err != nil {
		t.Fatalf("ClearKeyCooldown failed: %v", err)
	}

	// 验证已清除
	_, exists = store.GetKeyCooldownUntil(ctx, cfg.ID, 0)
	if exists {
		t.Error("Key cooldown should be cleared")
	}
}

// TestHandleError_EdgeCases 测试边界条件
func TestHandleError_EdgeCases(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store)
	ctx := context.Background()

	t.Run("不存在的渠道", func(t *testing.T) {
		// 对不存在的渠道ID执行冷却应该返回错误
		_, err := manager.HandleError(ctx, 99999, 0, 500, nil, false)
		if err == nil {
			t.Error("Expected error for non-existent channel")
		}
	})

	t.Run("负数keyIndex", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-negative-key")
		// 负数keyIndex表示网络错误，不应该尝试冷却Key
		action, err := manager.HandleError(ctx, cfg.ID, -1, 500, nil, false)
		if err != nil {
			t.Errorf("Should handle negative keyIndex: %v", err)
		}
		if action != ActionRetryChannel {
			t.Errorf("Expected ActionRetryChannel for channel-level error")
		}
	})

	t.Run("nil错误体", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-nil-body")
		// nil错误体应该使用基础分类
		action, err := manager.HandleError(ctx, cfg.ID, -1, 500, nil, false)
		if err != nil {
			t.Errorf("Should handle nil error body: %v", err)
		}
		if action != ActionRetryChannel {
			t.Error("Should classify 500 as channel-level even with nil body")
		}
	})

	t.Run("空错误体", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-empty-body")
		action, err := manager.HandleError(ctx, cfg.ID, -1, 503, []byte{}, false)
		if err != nil {
			t.Errorf("Should handle empty error body: %v", err)
		}
		if action != ActionRetryChannel {
			t.Error("Should classify 503 as channel-level")
		}
	})
}

// ========== 辅助函数 ==========

func setupTestStore(t *testing.T) (*sqlite.SQLiteStore, func()) {
	t.Helper()

	// 禁用内存模式，避免Redis强制检查
	oldValue := os.Getenv("CCLOAD_USE_MEMORY_DB")
	os.Setenv("CCLOAD_USE_MEMORY_DB", "false")

	tmpDB := t.TempDir() + "/cooldown_test.db"
	store, err := sqlite.NewSQLiteStore(tmpDB, nil)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}

	cleanup := func() {
		store.Close()
		os.Setenv("CCLOAD_USE_MEMORY_DB", oldValue)
	}

	return store, cleanup
}

func createTestChannel(t *testing.T, store *sqlite.SQLiteStore, name string) *model.Config {
	t.Helper()

	cfg := &model.Config{
		Name:     name,
		URL:      "https://api.example.com",
		Priority: 10,
		Models:   []string{"test-model"},
		Enabled:  true,
	}

	created, err := store.CreateConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Failed to create test channel: %v", err)
	}

	return created
}
