package sqlite_test

import (
	"ccLoad/internal/storage"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"context"
	"testing"
	"time"
)

// TestCooldownConsistency_401Error 验证401错误时Key级别和渠道级别冷却时间一致性
// 设计目标：确保相同错误码在不同级别产生相同的冷却时长
func TestCooldownConsistency_401Error(t *testing.T) {
	tmpDB := t.TempDir() + "/test-cooldown-consistency.db"
	store, err := storage.CreateSQLiteStore(tmpDB, nil)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()

	// 测试场景：首次401错误
	t.Run("初始401错误冷却时间一致性", func(t *testing.T) {
		// 创建两个独立的测试渠道
		channelCfg := &model.Config{
			Name:    "channel-level-test",
			URL:     "https://api.example.com",
			Enabled: true,
		}
		channelCreated, err := store.CreateConfig(ctx, channelCfg)
		if err != nil {
			t.Fatalf("创建渠道测试配置失败: %v", err)
		}

		keyCfg := &model.Config{
			Name:    "key-level-test",
			URL:     "https://api.example.com",
			Enabled: true,
		}
		keyCreated, err := store.CreateConfig(ctx, keyCfg)
		if err != nil {
			t.Fatalf("创建Key测试配置失败: %v", err)
		}

		// 为Key测试渠道创建2个API Keys
		for i, key := range []string{"sk-key1", "sk-key2"} {
			err = store.CreateAPIKey(ctx, &model.APIKey{
				ChannelID:   keyCreated.ID,
				KeyIndex:    i,
				APIKey:      key,
				KeyStrategy: "sequential",
			})
			if err != nil {
				t.Fatalf("创建API Key %d失败: %v", i, err)
			}
		}

		// 触发渠道级401错误
		channelDuration, err := store.BumpChannelCooldown(ctx, channelCreated.ID, now, 401)
		if err != nil {
			t.Fatalf("渠道级BumpCooldownOnError失败: %v", err)
		}

		// 触发Key级401错误
		keyDuration, err := store.BumpKeyCooldown(ctx, keyCreated.ID, 0, now, 401)
		if err != nil {
			t.Fatalf("Key级BumpKeyCooldownOnError失败: %v", err)
		}

		// 验证冷却时长完全一致
		if channelDuration != keyDuration {
			t.Errorf("❌ 401错误冷却时间不一致:\n  渠道级: %v\n  Key级: %v",
				channelDuration, keyDuration)
		}

		// 验证都是5分钟（util.AuthErrorInitialCooldown）
		expectedDuration := util.AuthErrorInitialCooldown
		tolerance := 10 * time.Millisecond

		if abs(channelDuration-expectedDuration) > tolerance {
			t.Errorf("渠道级冷却时间错误: 期望%v，实际%v", expectedDuration, channelDuration)
		}

		if abs(keyDuration-expectedDuration) > tolerance {
			t.Errorf("Key级冷却时间错误: 期望%v，实际%v", expectedDuration, keyDuration)
		}

		t.Logf("✅ 401错误冷却时间一致: 渠道级=%v, Key级=%v (期望%v)",
			channelDuration, keyDuration, expectedDuration)
	})

	// 测试场景：指数退避序列一致性
	t.Run("401错误指数退避序列一致性", func(t *testing.T) {
		// 创建两个测试渠道
		channelCfg := &model.Config{
			Name:    "channel-backoff-test",
			URL:     "https://api.example.com",
			Enabled: true,
		}
		channelCreated, err := store.CreateConfig(ctx, channelCfg)
		if err != nil {
			t.Fatalf("创建渠道测试配置失败: %v", err)
		}

		keyCfg := &model.Config{
			Name:    "key-backoff-test",
			URL:     "https://api.example.com",
			Enabled: true,
		}
		keyCreated, err := store.CreateConfig(ctx, keyCfg)
		if err != nil {
			t.Fatalf("创建Key测试配置失败: %v", err)
		}

		// 为Key测试渠道创建2个API Keys
		for i, key := range []string{"sk-key1", "sk-key2"} {
			err = store.CreateAPIKey(ctx, &model.APIKey{
				ChannelID:   keyCreated.ID,
				KeyIndex:    i,
				APIKey:      key,
				KeyStrategy: "sequential",
			})
			if err != nil {
				t.Fatalf("创建API Key %d失败: %v", i, err)
			}
		}

		// 预期序列：5min → 10min → 20min → 30min
		expectedSequence := []time.Duration{
			5 * time.Minute,
			10 * time.Minute,
			20 * time.Minute,
			30 * time.Minute,
		}

		currentTime := now
		for i, expected := range expectedSequence {
			// 渠道级错误
			channelDuration, err := store.BumpChannelCooldown(ctx, channelCreated.ID, currentTime, 401)
			if err != nil {
				t.Fatalf("第%d次渠道级错误失败: %v", i+1, err)
			}

			// Key级错误
			keyDuration, err := store.BumpKeyCooldown(ctx, keyCreated.ID, 0, currentTime, 401)
			if err != nil {
				t.Fatalf("第%d次Key级错误失败: %v", i+1, err)
			}

			// 验证一致性
			if channelDuration != keyDuration {
				t.Errorf("❌ 第%d次错误冷却时间不一致:\n  渠道级: %v\n  Key级: %v",
					i+1, channelDuration, keyDuration)
			}

			// 验证符合预期
			tolerance := 100 * time.Millisecond
			if abs(channelDuration-expected) > tolerance {
				t.Errorf("第%d次错误冷却时间错误: 期望%v，渠道级%v，Key级%v",
					i+1, expected, channelDuration, keyDuration)
			}

			t.Logf("✅ 第%d次401错误: 渠道级=%v, Key级=%v (期望%v)",
				i+1, channelDuration, keyDuration, expected)

			// 推进时间（确保不被当作同一次错误）
			currentTime = currentTime.Add(expected + 1*time.Second)
		}
	})

	// 测试场景：403错误一致性
	t.Run("403错误冷却时间一致性", func(t *testing.T) {
		channelCfg := &model.Config{
			Name:    "channel-403-test",
			URL:     "https://api.example.com",
			Enabled: true,
		}
		channelCreated, err := store.CreateConfig(ctx, channelCfg)
		if err != nil {
			t.Fatalf("创建渠道测试配置失败: %v", err)
		}

		keyCfg := &model.Config{
			Name:    "key-403-test",
			URL:     "https://api.example.com",
			Enabled: true,
		}
		keyCreated, err := store.CreateConfig(ctx, keyCfg)
		if err != nil {
			t.Fatalf("创建Key测试配置失败: %v", err)
		}

		// 为Key测试渠道创建2个API Keys
		for i, key := range []string{"sk-key1", "sk-key2"} {
			err = store.CreateAPIKey(ctx, &model.APIKey{
				ChannelID:   keyCreated.ID,
				KeyIndex:    i,
				APIKey:      key,
				KeyStrategy: "sequential",
			})
			if err != nil {
				t.Fatalf("创建API Key %d失败: %v", i, err)
			}
		}

		// 触发403错误
		channelDuration, _ := store.BumpChannelCooldown(ctx, channelCreated.ID, now, 403)
		keyDuration, _ := store.BumpKeyCooldown(ctx, keyCreated.ID, 0, now, 403)

		if channelDuration != keyDuration {
			t.Errorf("❌ 403错误冷却时间不一致: 渠道级=%v, Key级=%v",
				channelDuration, keyDuration)
		}

		if channelDuration != util.AuthErrorInitialCooldown {
			t.Errorf("403错误初始冷却时间错误: 期望%v，实际%v",
				util.AuthErrorInitialCooldown, channelDuration)
		}

		t.Logf("✅ 403错误冷却时间一致: 渠道级=%v, Key级=%v",
			channelDuration, keyDuration)
	})

	// 测试场景：其他错误码一致性（429/500）
	t.Run("其他错误码冷却时间一致性", func(t *testing.T) {
		testCases := []struct {
			name       string
			statusCode int
			expected   time.Duration
		}{
			{"429限流错误", 429, util.OtherErrorInitialCooldown},
			{"500服务器错误", 500, util.ServerErrorInitialCooldown},
			{"502网关错误", 502, util.ServerErrorInitialCooldown},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				channelCfg := &model.Config{
					Name:    "channel-" + tc.name,
					URL:     "https://api.example.com",
					Enabled: true,
				}
				channelCreated, _ := store.CreateConfig(ctx, channelCfg)

				keyCfg := &model.Config{
					Name:    "key-" + tc.name,
					URL:     "https://api.example.com",
					Enabled: true,
				}
				keyCreated, _ := store.CreateConfig(ctx, keyCfg)

				// 为Key测试渠道创建2个API Keys
				for i, key := range []string{"sk-key1", "sk-key2"} {
					_ = store.CreateAPIKey(ctx, &model.APIKey{
						ChannelID:   keyCreated.ID,
						KeyIndex:    i,
						APIKey:      key,
						KeyStrategy: "sequential",
					})
				}

				channelDuration, _ := store.BumpChannelCooldown(ctx, channelCreated.ID, now, tc.statusCode)
				keyDuration, _ := store.BumpKeyCooldown(ctx, keyCreated.ID, 0, now, tc.statusCode)

				if channelDuration != keyDuration {
					t.Errorf("❌ %s冷却时间不一致: 渠道级=%v, Key级=%v",
						tc.name, channelDuration, keyDuration)
				}

				if channelDuration != tc.expected {
					t.Errorf("%s初始冷却时间错误: 期望%v，实际%v",
						tc.name, tc.expected, channelDuration)
				}

				t.Logf("✅ %s一致: 渠道级=%v, Key级=%v",
					tc.name, channelDuration, keyDuration)
			})
		}
	})
}

// abs 计算time.Duration的绝对值
func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

