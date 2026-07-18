package cooldown

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"
)

// TestNewManager 测试管理器创建
func TestNewManager(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	manager := NewManager(store, nil)
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
	manager := NewManager(store, nil)
	ctx := context.Background()

	// 创建测试渠道
	cfg := createTestChannel(t, store, "test-client-error")

	testCases := []struct {
		name       string
		statusCode int
		errorBody  []byte
	}{
		{"406不可接受", 406, []byte(`{"error":"not acceptable"}`)},
		// 注意：405/404 已改为渠道级错误（上游endpoint配置问题）
		// 注意：400 已改为渠道级错误（代理场景下视为上游异常）
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			action := manager.HandleError(ctx, ErrorInput{
				ChannelID:      cfg.ID,
				KeyIndex:       0,
				StatusCode:     tc.statusCode,
				ErrorBody:      tc.errorBody,
				IsNetworkError: false,
				Headers:        nil,
			})

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
	manager := NewManager(store, nil)
	ctx := context.Background()

	// 创建多Key渠道（3个Key）
	cfg := createTestChannel(t, store, "test-key-error")
	keys := make([]*model.APIKey, 3)
	for i := 0; i < 3; i++ {
		keys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		}
	}
	_ = store.CreateAPIKeysBatch(ctx, keys)

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
			action := manager.HandleError(ctx, ErrorInput{
				ChannelID:      cfg.ID,
				KeyIndex:       keyIndex,
				StatusCode:     tc.statusCode,
				ErrorBody:      tc.errorBody,
				IsNetworkError: false,
				Headers:        nil,
			})

			if action != ActionRetryKey {
				t.Errorf("Expected ActionRetryKey for %d, got %v", tc.statusCode, action)
			}

			// 验证Key被冷却
			cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, keyIndex)
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
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-channel-error")

	testCases := []struct {
		name       string
		statusCode int
		errorBody  []byte
	}{
		{"404未找到", 404, []byte(`{"error":"not found"}`)},
		{"405方法不允许", 405, []byte(`{"error":"method not allowed"}`)}, // 上游endpoint配置错误
		{"500内部错误", 500, []byte(`{"error":"internal server error"}`)},
		{"502网关错误", 502, []byte(`{"error":"bad gateway"}`)},
		{"503服务不可用", 503, []byte(`{"error":"service unavailable"}`)},
		{"504网关超时", 504, []byte(`{"error":"gateway timeout"}`)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// 先重置冷却
			_ = store.ResetChannelCooldown(ctx, cfg.ID)

			action := manager.HandleError(ctx, ErrorInput{
				ChannelID:      cfg.ID,
				KeyIndex:       -1,
				StatusCode:     tc.statusCode,
				ErrorBody:      tc.errorBody,
				IsNetworkError: false,
				Headers:        nil,
			})

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
	manager := NewManager(store, nil)
	ctx := context.Background()

	// 创建单Key渠道
	cfg := createTestChannel(t, store, "test-single-key")
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-only-key",
		KeyStrategy: model.KeyStrategySequential,
	}})

	// 401认证错误本应是Key级，但单Key渠道应升级为渠道级
	action := manager.HandleError(ctx, ErrorInput{
		ChannelID:      cfg.ID,
		KeyIndex:       0,
		StatusCode:     401,
		ErrorBody:      []byte(`{"error":{"type":"authentication_error"}}`),
		IsNetworkError: false,
		Headers:        nil,
	})

	// [INFO] 关键断言：单Key渠道应升级为渠道级错误
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
	manager := NewManager(store, nil)
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
			name:           "其他网络错误(502)",
			statusCode:     502,
			expectedAction: ActionRetryChannel,
			description:    "Other network errors should be channel-level",
		},
	}

	// 为测试连接重置场景，创建多Key渠道
	netKeys := make([]*model.APIKey, 2)
	for i := 0; i < 2; i++ {
		netKeys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-net-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		}
	}
	_ = store.CreateAPIKeysBatch(ctx, netKeys)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// 重置冷却
			_ = store.ResetChannelCooldown(ctx, cfg.ID)

			action := manager.HandleError(ctx, ErrorInput{
				ChannelID:      cfg.ID,
				KeyIndex:       0,
				StatusCode:     tc.statusCode,
				ErrorBody:      nil,
				IsNetworkError: true,
				Headers:        nil,
			})

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
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-clear-channel")

	// 先触发冷却
	_ = manager.HandleError(ctx, ErrorInput{
		ChannelID:      cfg.ID,
		KeyIndex:       -1,
		StatusCode:     500,
		ErrorBody:      nil,
		IsNetworkError: false,
		Headers:        nil,
	})

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
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-clear-key")
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{
			ChannelID:   cfg.ID,
			KeyIndex:    0,
			APIKey:      "sk-test-clear",
			KeyStrategy: model.KeyStrategySequential,
		},
		{
			ChannelID:   cfg.ID,
			KeyIndex:    1,
			APIKey:      "sk-test-clear-2",
			KeyStrategy: model.KeyStrategySequential,
		},
	})

	// 先触发Key冷却
	_ = manager.HandleError(ctx, ErrorInput{
		ChannelID:      cfg.ID,
		KeyIndex:       0,
		StatusCode:     401,
		ErrorBody:      []byte(`{"error":{"type":"authentication_error"}}`),
		IsNetworkError: false,
		Headers:        nil,
	})

	// 验证已冷却
	cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
	if !exists || cooldownUntil.Before(time.Now()) {
		t.Fatal("Key should be cooled down")
	}

	// 清除冷却
	err := manager.ClearKeyCooldown(ctx, cfg.ID, 0)
	if err != nil {
		t.Fatalf("ClearKeyCooldown failed: %v", err)
	}

	// 验证已清除
	_, exists = getKeyCooldownUntil(ctx, store, cfg.ID, 0)
	if exists {
		t.Error("Key cooldown should be cleared")
	}
}

// TestHandleError_EdgeCases 测试边界条件
func TestHandleError_EdgeCases(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	t.Run("不存在的渠道", func(t *testing.T) {
		// 冷却失败不应返回错误，而是记录警告
		// 设计原则: 数据库错误不应阻塞用户请求，系统应降级服务
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      99999,
			KeyIndex:       0,
			StatusCode:     500,
			ErrorBody:      nil,
			IsNetworkError: false,
			Headers:        nil,
		})
		// 冷却失败时，保守策略返回 ActionRetryChannel
		if action != ActionRetryChannel {
			t.Errorf("Expected ActionRetryChannel when cooldown fails, got %v", action)
		}
	})

	t.Run("负数keyIndex", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-negative-key")
		// 负数keyIndex表示网络错误，不应该尝试冷却Key
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       -1,
			StatusCode:     500,
			ErrorBody:      nil,
			IsNetworkError: false,
			Headers:        nil,
		})
		if action != ActionRetryChannel {
			t.Errorf("Expected ActionRetryChannel for channel-level error")
		}
	})

	t.Run("nil错误体", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-nil-body")
		// nil错误体应该使用基础分类
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       -1,
			StatusCode:     500,
			ErrorBody:      nil,
			IsNetworkError: false,
			Headers:        nil,
		})
		if action != ActionRetryChannel {
			t.Error("Should classify 500 as channel-level even with nil body")
		}
	})

	t.Run("空错误体", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-empty-body")
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       -1,
			StatusCode:     503,
			ErrorBody:      []byte{},
			IsNetworkError: false,
			Headers:        nil,
		})
		if action != ActionRetryChannel {
			t.Error("Should classify 503 as channel-level")
		}
	})
}

// TestHandleError_RateLimitClassification 测试429错误的智能分类
// 验证基于headers和响应体的429错误分类
func TestHandleError_RateLimitClassification(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	// 创建多Key渠道
	cfg := createTestChannel(t, store, "test-429-classification")
	rateKeys := make([]*model.APIKey, 3)
	for i := 0; i < 3; i++ {
		rateKeys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-ratelimit-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		}
	}
	_ = store.CreateAPIKeysBatch(ctx, rateKeys)

	testCases := []struct {
		name           string
		headers        map[string][]string
		responseBody   []byte
		expectedAction Action
		description    string
	}{
		{
			name: "429-Retry-After大于60秒",
			headers: map[string][]string{
				"Retry-After": {"120"},
			},
			responseBody:   []byte(`{"error":{"type":"rate_limit_error"}}`),
			expectedAction: ActionRetryChannel,
			description:    "Retry-After > 60s indicates account/IP level rate limit",
		},
		{
			name: "429-Retry-After小于60秒",
			headers: map[string][]string{
				"Retry-After": {"30"},
			},
			responseBody:   []byte(`{"error":{"type":"rate_limit_error"}}`),
			expectedAction: ActionRetryKey,
			description:    "Retry-After <= 60s indicates key-level rate limit",
		},
		{
			name: "429-Retry-After为HTTP日期",
			headers: map[string][]string{
				"Retry-After": {"Wed, 29 Oct 2025 12:00:00 GMT"},
			},
			responseBody:   []byte(`{"error":{"type":"rate_limit_error"}}`),
			expectedAction: ActionRetryChannel,
			description:    "HTTP date format typically indicates long-term rate limit",
		},
		{
			name: "429-X-RateLimit-Scope-global",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"global"},
			},
			responseBody:   []byte(`{"error":{"type":"rate_limit_error"}}`),
			expectedAction: ActionRetryChannel,
			description:    "Global scope indicates channel-level rate limit",
		},
		{
			name: "429-X-RateLimit-Scope-ip",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"ip"},
			},
			responseBody:   []byte(`{"error":{"type":"rate_limit_error"}}`),
			expectedAction: ActionRetryChannel,
			description:    "IP scope indicates channel-level rate limit",
		},
		{
			name: "429-X-RateLimit-Scope-account",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"account"},
			},
			responseBody:   []byte(`{"error":{"type":"rate_limit_error"}}`),
			expectedAction: ActionRetryChannel,
			description:    "Account scope indicates channel-level rate limit",
		},
		{
			name: "429-响应体包含ip-rate-limit",
			headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			responseBody:   []byte(`{"error":{"message":"IP rate limit exceeded"}}`),
			expectedAction: ActionRetryChannel,
			description:    "Response body with 'ip rate limit' indicates channel-level",
		},
		{
			name: "429-响应体包含account-rate-limit",
			headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			responseBody:   []byte(`{"error":{"message":"Account rate limit exceeded"}}`),
			expectedAction: ActionRetryChannel,
			description:    "Response body with 'account rate limit' indicates channel-level",
		},
		{
			name: "429-响应体包含global-rate-limit",
			headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			responseBody:   []byte(`{"error":{"message":"Global rate limit exceeded"}}`),
			expectedAction: ActionRetryChannel,
			description:    "Response body with 'global rate limit' indicates channel-level",
		},
		{
			name: "429-无特殊headers和响应体",
			headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			responseBody:   []byte(`{"error":{"type":"rate_limit_error"}}`),
			expectedAction: ActionRetryKey,
			description:    "Default to key-level when no special indicators present",
		},
		{
			name:           "429-nil-headers",
			headers:        nil,
			responseBody:   []byte(`{"error":{"type":"rate_limit_error"}}`),
			expectedAction: ActionRetryKey,
			description:    "Nil headers should default to key-level",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// 重置冷却状态
			_ = store.ResetChannelCooldown(ctx, cfg.ID)
			for i := 0; i < 3; i++ {
				_ = store.ResetKeyCooldown(ctx, cfg.ID, i)
			}

			action := manager.HandleError(ctx, ErrorInput{
				ChannelID:      cfg.ID,
				KeyIndex:       0,
				StatusCode:     429,
				ErrorBody:      tc.responseBody,
				IsNetworkError: false,
				Headers:        tc.headers,
			})

			if action != tc.expectedAction {
				t.Errorf("%s: expected %v, got %v", tc.description, tc.expectedAction, action)
			}

			// 验证冷却状态
			switch tc.expectedAction {
			case ActionRetryChannel:
				channelCfg, _ := store.GetConfig(ctx, cfg.ID)
				if channelCfg.CooldownUntil == 0 || time.Unix(channelCfg.CooldownUntil, 0).Before(time.Now()) {
					t.Errorf("Channel should be cooled down for %s", tc.name)
				}
			case ActionRetryKey:
				cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
				if !exists || cooldownUntil.Before(time.Now()) {
					t.Errorf("Key should be cooled down for %s", tc.name)
				}
			}

			t.Logf("[INFO] %s: %s", tc.name, tc.description)
		})
	}
}

func TestHandleError_Structured429QuotaCooldown(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-structured-429-quota")
	keys := make([]*model.APIKey, 2)
	for i := 0; i < 2; i++ {
		keys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-quota-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		}
	}
	_ = store.CreateAPIKeysBatch(ctx, keys)

	t.Run("DAILY_LIMIT_EXCEEDED cools key until next local day", func(t *testing.T) {
		before := time.Now()
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       0,
			StatusCode:     429,
			ErrorBody:      []byte(`{"code":"USAGE_LIMIT_EXCEEDED","message":"error: code=429 reason=\"DAILY_LIMIT_EXCEEDED\" message=\"daily usage limit exceeded\" metadata=map[]"}`),
			IsNetworkError: false,
			Headers:        nil,
		})
		after := time.Now()

		if action != ActionRetryKey {
			t.Fatalf("expected ActionRetryKey, got %v", action)
		}

		cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
		if !exists {
			t.Fatal("expected key cooldown")
		}

		if !sameTimeSecond(cooldownUntil, nextLocalMidnight(before)) &&
			!sameTimeSecond(cooldownUntil, nextLocalMidnight(after)) {
			t.Fatalf("cooldownUntil=%s, want next local midnight from %s or %s",
				cooldownUntil.Format(time.RFC3339),
				before.Format(time.RFC3339),
				after.Format(time.RFC3339))
		}
	})

	t.Run("API_KEY_QUOTA_EXHAUSTED cools key for thirty minutes", func(t *testing.T) {
		before := time.Now()
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       1,
			StatusCode:     429,
			ErrorBody:      []byte(`{"code":"API_KEY_QUOTA_EXHAUSTED","message":"API key 额度已用完"}`),
			IsNetworkError: false,
			Headers:        nil,
		})

		if action != ActionRetryKey {
			t.Fatalf("expected ActionRetryKey, got %v", action)
		}

		cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 1)
		if !exists {
			t.Fatal("expected key cooldown")
		}

		duration := cooldownUntil.Sub(before)
		if duration < 29*time.Minute+55*time.Second || duration > 30*time.Minute+5*time.Second {
			t.Fatalf("cooldown duration=%v, want about 30m", duration)
		}
	})
}

func TestHandleError_ModelCooldownResetSeconds(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	body := []byte(`{"error":{"code":"model_cooldown","message":"All credentials for model gpt-5.5 are cooling down via provider codex","model":"gpt-5.5","provider":"codex","reset_seconds":13792,"reset_time":"3h49m51s"}}`)

	t.Run("多Key渠道冷却当前Key到reset_seconds", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-model-cooldown-multi-key")
		_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{
				ChannelID:   cfg.ID,
				KeyIndex:    0,
				APIKey:      "sk-model-cooldown-0",
				KeyStrategy: model.KeyStrategySequential,
			},
			{
				ChannelID:   cfg.ID,
				KeyIndex:    1,
				APIKey:      "sk-model-cooldown-1",
				KeyStrategy: model.KeyStrategySequential,
			},
		})

		before := time.Now()
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       0,
			StatusCode:     429,
			ErrorBody:      body,
			IsNetworkError: false,
			Headers:        nil,
		})
		after := time.Now()

		if action != ActionRetryKey {
			t.Fatalf("expected ActionRetryKey, got %v", action)
		}

		cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
		if !exists {
			t.Fatal("expected key cooldown")
		}

		if !cooldownWithinResetSeconds(cooldownUntil, before, after, 13792) {
			t.Fatalf("key cooldownUntil=%s, want reset_seconds based cooldown",
				cooldownUntil.Format(time.RFC3339))
		}

		channelCfg, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("get config: %v", err)
		}
		if channelCfg.CooldownUntil > 0 && time.Unix(channelCfg.CooldownUntil, 0).After(time.Now()) {
			t.Fatal("multi-key model_cooldown should not cool channel")
		}
	})

	t.Run("单Key渠道冷却渠道到reset_seconds", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-model-cooldown-single-key")
		_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
			ChannelID:   cfg.ID,
			KeyIndex:    0,
			APIKey:      "sk-model-cooldown-single",
			KeyStrategy: model.KeyStrategySequential,
		}})

		before := time.Now()
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       0,
			StatusCode:     429,
			ErrorBody:      body,
			IsNetworkError: false,
			Headers:        nil,
		})
		after := time.Now()

		if action != ActionRetryChannel {
			t.Fatalf("expected ActionRetryChannel, got %v", action)
		}

		channelCfg, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("get config: %v", err)
		}

		channelCooldownUntil := time.Unix(channelCfg.CooldownUntil, 0)
		if channelCfg.CooldownUntil == 0 || !cooldownWithinResetSeconds(channelCooldownUntil, before, after, 13792) {
			t.Fatalf("channel cooldownUntil=%s, want reset_seconds based cooldown",
				channelCooldownUntil.Format(time.RFC3339))
		}
	})
}

func TestHandleError_GeminiResourceExhaustedRetryIn(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	body := []byte(`{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details.\n* Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 20, model: gemini-3.5-flash\nPlease retry in 17.409754061s.","status":"RESOURCE_EXHAUSTED"}}`)
	retryAfter := 17*time.Second + 409754061*time.Nanosecond

	t.Run("多Key渠道冷却当前Key到retry-in", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-gemini-resource-exhausted-multi-key")
		_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{
				ChannelID:   cfg.ID,
				KeyIndex:    0,
				APIKey:      "sk-gemini-resource-0",
				KeyStrategy: model.KeyStrategySequential,
			},
			{
				ChannelID:   cfg.ID,
				KeyIndex:    1,
				APIKey:      "sk-gemini-resource-1",
				KeyStrategy: model.KeyStrategySequential,
			},
		})

		before := time.Now()
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       0,
			StatusCode:     429,
			ErrorBody:      body,
			IsNetworkError: false,
			Headers:        nil,
		})
		after := time.Now()

		if action != ActionRetryKey {
			t.Fatalf("expected ActionRetryKey, got %v", action)
		}

		cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
		if !exists {
			t.Fatal("expected key cooldown")
		}
		if !cooldownWithinDuration(cooldownUntil, before, after, retryAfter) {
			t.Fatalf("key cooldownUntil=%s, want retry-in based cooldown",
				cooldownUntil.Format(time.RFC3339))
		}
	})

	t.Run("单Key渠道冷却渠道到retry-in", func(t *testing.T) {
		cfg := createTestChannel(t, store, "test-gemini-resource-exhausted-single-key")
		_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
			ChannelID:   cfg.ID,
			KeyIndex:    0,
			APIKey:      "sk-gemini-resource-single",
			KeyStrategy: model.KeyStrategySequential,
		}})

		before := time.Now()
		action := manager.HandleError(ctx, ErrorInput{
			ChannelID:      cfg.ID,
			KeyIndex:       0,
			StatusCode:     429,
			ErrorBody:      body,
			IsNetworkError: false,
			Headers:        nil,
		})
		after := time.Now()

		if action != ActionRetryChannel {
			t.Fatalf("expected ActionRetryChannel, got %v", action)
		}

		channelCfg, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("get config: %v", err)
		}

		channelCooldownUntil := time.Unix(channelCfg.CooldownUntil, 0)
		if channelCfg.CooldownUntil == 0 || !cooldownWithinDuration(channelCooldownUntil, before, after, retryAfter) {
			t.Fatalf("channel cooldownUntil=%s, want retry-in based cooldown",
				channelCooldownUntil.Format(time.RFC3339))
		}
	})
}

func TestHandleError_FreeTierBudgetExceededWrappedIn500CoolsKeyThirtyMinutes(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-free-tier-budget-exceeded")
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{
			ChannelID:   cfg.ID,
			KeyIndex:    0,
			APIKey:      "sk-free-tier-0",
			KeyStrategy: model.KeyStrategySequential,
		},
		{
			ChannelID:   cfg.ID,
			KeyIndex:    1,
			APIKey:      "sk-free-tier-1",
			KeyStrategy: model.KeyStrategySequential,
		},
	})

	before := time.Now()
	action := manager.HandleError(ctx, ErrorInput{
		ChannelID:  cfg.ID,
		KeyIndex:   0,
		StatusCode: 500,
		ErrorBody:  []byte(`{"type":"error","error":{"type":"api_error","message":"403 {\"error\":{\"code\":\"FREE_TIER_BUDGET_EXCEEDED\",\"message\":\"Free tier monthly spend limit exceeded. Please upgrade to a paid plan to continue using this service.\"}}"}}`),
	})

	if action != ActionRetryKey {
		t.Fatalf("expected ActionRetryKey, got %v", action)
	}

	cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
	if !exists {
		t.Fatal("expected key cooldown")
	}

	duration := cooldownUntil.Sub(before)
	if duration < 29*time.Minute+55*time.Second || duration > 30*time.Minute+5*time.Second {
		t.Fatalf("cooldown duration=%v, want about 30m", duration)
	}

	channelCfg, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if channelCfg.CooldownUntil > 0 && time.Unix(channelCfg.CooldownUntil, 0).After(time.Now()) {
		t.Fatalf("channel should not be cooled for wrapped free tier quota error")
	}
}

func TestHandleError_FreeTierBudgetExceededSSEErrorCoolsKeyThirtyMinutes(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-free-tier-budget-exceeded-sse")
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{
			ChannelID:   cfg.ID,
			KeyIndex:    0,
			APIKey:      "sk-free-tier-sse-0",
			KeyStrategy: model.KeyStrategySequential,
		},
		{
			ChannelID:   cfg.ID,
			KeyIndex:    1,
			APIKey:      "sk-free-tier-sse-1",
			KeyStrategy: model.KeyStrategySequential,
		},
	})

	before := time.Now()
	action := manager.HandleError(ctx, ErrorInput{
		ChannelID:  cfg.ID,
		KeyIndex:   0,
		StatusCode: util.StatusSSEError,
		ErrorBody:  []byte(`{"type":"error","error":{"type":"api_error","message":"403 {\"error\":{\"code\":\"FREE_TIER_BUDGET_EXCEEDED\",\"message\":\"Free tier monthly spend limit exceeded. Please upgrade to a paid plan to continue using this service.\"}}"}}`),
	})

	if action != ActionRetryKey {
		t.Fatalf("expected ActionRetryKey, got %v", action)
	}

	cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
	if !exists {
		t.Fatal("expected key cooldown")
	}

	duration := cooldownUntil.Sub(before)
	if duration < 29*time.Minute+55*time.Second || duration > 30*time.Minute+5*time.Second {
		t.Fatalf("cooldown duration=%v, want about 30m", duration)
	}

	channelCfg, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if channelCfg.CooldownUntil > 0 && time.Unix(channelCfg.CooldownUntil, 0).After(time.Now()) {
		t.Fatalf("channel should not be cooled for SSE free tier quota error")
	}
}

func TestHandleError_Structured429QuotaSingleKeyStaysKeyCooldown(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-single-key-structured-429-quota")
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-single-quota",
		KeyStrategy: model.KeyStrategySequential,
	}})

	action := manager.HandleError(ctx, ErrorInput{
		ChannelID:      cfg.ID,
		KeyIndex:       0,
		StatusCode:     429,
		ErrorBody:      []byte(`{"code":"API_KEY_QUOTA_EXHAUSTED","message":"API key 额度已用完"}`),
		IsNetworkError: false,
		Headers:        nil,
	})

	if action != ActionRetryKey {
		t.Fatalf("expected ActionRetryKey, got %v", action)
	}

	cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
	if !exists || cooldownUntil.Before(time.Now()) {
		t.Fatal("expected key cooldown")
	}

	channelCfg, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if channelCfg.CooldownUntil > 0 && time.Unix(channelCfg.CooldownUntil, 0).After(time.Now()) {
		t.Fatalf("channel should not be cooled for structured key quota error")
	}
}

func TestHandleError_ChineseRelativeQuotaCooldown(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-chinese-relative-quota")
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{
			ChannelID:   cfg.ID,
			KeyIndex:    0,
			APIKey:      "sk-relative-quota-0",
			KeyStrategy: model.KeyStrategySequential,
		},
		{
			ChannelID:   cfg.ID,
			KeyIndex:    1,
			APIKey:      "sk-relative-quota-1",
			KeyStrategy: model.KeyStrategySequential,
		},
	})

	before := time.Now()
	action := manager.HandleError(ctx, ErrorInput{
		ChannelID:      cfg.ID,
		KeyIndex:       0,
		StatusCode:     402,
		ErrorBody:      []byte(`{"error":"已达到用量上限，将在明天凌晨3点13分（北京时间）恢复"}`),
		IsNetworkError: false,
		Headers:        nil,
	})
	after := time.Now()

	if action != ActionRetryKey {
		t.Fatalf("expected ActionRetryKey, got %v", action)
	}

	cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
	if !exists {
		t.Fatal("expected key cooldown")
	}

	beforeExpected := nextBeijingTime(before, 3, 13)
	afterExpected := nextBeijingTime(after, 3, 13)
	if !sameTimeSecond(cooldownUntil, beforeExpected) &&
		!sameTimeSecond(cooldownUntil, afterExpected) {
		t.Fatalf("cooldownUntil=%s, want %s or %s",
			cooldownUntil.Format(time.RFC3339),
			beforeExpected.Format(time.RFC3339),
			afterExpected.Format(time.RFC3339))
	}
}

func TestHandleError_GlobalFixedWindowQuotaCoolsChannelUntilRetryClock(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-global-fixed-window-quota")
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{
			ChannelID:   cfg.ID,
			KeyIndex:    0,
			APIKey:      "sk-global-fixed-window-0",
			KeyStrategy: model.KeyStrategySequential,
		},
		{
			ChannelID:   cfg.ID,
			KeyIndex:    1,
			APIKey:      "sk-global-fixed-window-1",
			KeyStrategy: model.KeyStrategySequential,
		},
	})

	before := time.Now()
	action := manager.HandleError(ctx, ErrorInput{
		ChannelID:      cfg.ID,
		KeyIndex:       0,
		StatusCode:     429,
		ErrorBody:      []byte(`{"error":{"message":"当前公益站使用人数较多，本时段全站额度已用完，请在 明天 12:00 后再试。（traceid: 29038189-54e3-472e-b821-e7a5ebef3795）","type":"rate_limit_error","param":null,"code":"global_fixed_window_quota_exhausted","trace_id":"29038189-54e3-472e-b821-e7a5ebef3795"}}`),
		IsNetworkError: false,
		Headers:        nil,
	})
	after := time.Now()

	if action != ActionRetryChannel {
		t.Fatalf("expected ActionRetryChannel, got %v", action)
	}

	if cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0); exists && cooldownUntil.After(time.Now()) {
		t.Fatalf("global fixed-window quota should not cool key, got %s", cooldownUntil.Format(time.RFC3339))
	}

	channelCfg, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}

	channelCooldownUntil := time.Unix(channelCfg.CooldownUntil, 0)
	beforeExpected := nextBeijingTime(before, 12, 0)
	afterExpected := nextBeijingTime(after, 12, 0)
	if channelCfg.CooldownUntil == 0 ||
		(!sameTimeSecond(channelCooldownUntil, beforeExpected) &&
			!sameTimeSecond(channelCooldownUntil, afterExpected)) {
		t.Fatalf("channel cooldownUntil=%s, want %s or %s",
			channelCooldownUntil.Format(time.RFC3339),
			beforeExpected.Format(time.RFC3339),
			afterExpected.Format(time.RFC3339))
	}
}

func TestHandleError_UsageLimitReachedMultiKeyCoolsKey(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-usage-limit-multi-key")
	keys := make([]*model.APIKey, 2)
	for i := 0; i < 2; i++ {
		keys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-usage-" + string(rune('a'+i)),
			KeyStrategy: model.KeyStrategySequential,
		}
	}
	_ = store.CreateAPIKeysBatch(ctx, keys)

	before := time.Now()
	action := manager.HandleError(ctx, ErrorInput{
		ChannelID:      cfg.ID,
		KeyIndex:       0,
		StatusCode:     429,
		ErrorBody:      []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":7260}}`),
		IsNetworkError: false,
		Headers:        nil,
	})

	if action != ActionRetryKey {
		t.Fatalf("expected ActionRetryKey, got %v", action)
	}

	cooldownUntil, exists := getKeyCooldownUntil(ctx, store, cfg.ID, 0)
	if !exists {
		t.Fatal("expected key cooldown")
	}

	duration := cooldownUntil.Sub(before)
	if duration < 7250*time.Second || duration > 7270*time.Second {
		t.Fatalf("cooldown duration=%v, want about 7260s", duration)
	}

	// 渠道不应被冷却
	channelCfg, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if channelCfg.CooldownUntil > 0 && time.Unix(channelCfg.CooldownUntil, 0).After(time.Now()) {
		t.Fatal("channel should not be cooled for multi-key usage limit")
	}
}

func TestHandleError_UsageLimitReachedSingleKeyCoolsChannel(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	ctx := context.Background()

	cfg := createTestChannel(t, store, "test-usage-limit-single-key")
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-single-usage",
		KeyStrategy: model.KeyStrategySequential,
	}})

	before := time.Now()
	action := manager.HandleError(ctx, ErrorInput{
		ChannelID:      cfg.ID,
		KeyIndex:       0,
		StatusCode:     429,
		ErrorBody:      []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":7260}}`),
		IsNetworkError: false,
		Headers:        nil,
	})

	if action != ActionRetryChannel {
		t.Fatalf("expected ActionRetryChannel, got %v", action)
	}

	// 单Key渠道应升级为渠道冷却
	channelCfg, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if channelCfg.CooldownUntil == 0 || time.Unix(channelCfg.CooldownUntil, 0).Before(time.Now()) {
		t.Fatal("channel should be cooled for single-key usage limit")
	}

	channelDuration := time.Unix(channelCfg.CooldownUntil, 0).Sub(before)
	if channelDuration < 7250*time.Second || channelDuration > 7270*time.Second {
		t.Fatalf("channel cooldown duration=%v, want about 7260s", channelDuration)
	}
}

// ========== 辅助函数 ==========

func nextLocalMidnight(now time.Time) time.Time {
	y, m, d := now.In(time.Local).Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.Local)
}

func nextBeijingTime(now time.Time, hour int, minute int) time.Time {
	loc := time.FixedZone("Asia/Shanghai", 8*60*60)
	local := now.In(loc)
	y, m, d := local.Date()
	return time.Date(y, m, d+1, hour, minute, 0, 0, loc)
}

func sameTimeSecond(a, b time.Time) bool {
	return a.Sub(b).Abs() <= 2*time.Second
}

func cooldownWithinResetSeconds(until time.Time, before time.Time, after time.Time, resetSeconds int) bool {
	return cooldownWithinDuration(until, before, after, time.Duration(resetSeconds)*time.Second)
}

func cooldownWithinDuration(until time.Time, before time.Time, after time.Time, duration time.Duration) bool {
	minUntil := before.Add(duration - 2*time.Second)
	maxUntil := after.Add(duration + 2*time.Second)
	return !until.Before(minUntil) && !until.After(maxUntil)
}

// getKeyCooldownUntil 获取指定Key的冷却时间（测试辅助函数）
func getKeyCooldownUntil(ctx context.Context, store storage.Store, channelID int64, keyIndex int) (time.Time, bool) {
	cooldowns, err := store.GetAllKeyCooldowns(ctx)
	if err != nil {
		return time.Time{}, false
	}
	channelCooldowns, ok := cooldowns[channelID]
	if !ok {
		return time.Time{}, false
	}
	until, ok := channelCooldowns[keyIndex]
	return until, ok
}

func setupTestStore(t *testing.T) (storage.Store, func()) {
	return testutil.SetupTestStore(t)
}

func createTestChannel(t *testing.T, store storage.Store, name string) *model.Config {
	t.Helper()

	cfg := &model.Config{
		Name:     name,
		URL:      "https://api.example.com",
		Priority: 10,
		ModelEntries: []model.ModelEntry{
			{Model: "test-model", RedirectModel: ""},
		},
		Enabled: true,
	}

	created, err := store.CreateConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Failed to create test channel: %v", err)
	}

	return created
}
