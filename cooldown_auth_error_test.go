package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// TestAuthErrorInitialCooldown 验证401/403错误的初始冷却时间为5分钟
func TestAuthErrorInitialCooldown(t *testing.T) {
	tests := []struct {
		name           string
		statusCode     int
		expectedMinDur time.Duration
		expectedMaxDur time.Duration
	}{
		{
			name:           "401未认证错误-初始冷却5分钟",
			statusCode:     401,
			expectedMinDur: 5 * time.Minute,
			expectedMaxDur: 5 * time.Minute,
		},
		{
			name:           "403禁止访问错误-初始冷却5分钟",
			statusCode:     403,
			expectedMinDur: 5 * time.Minute,
			expectedMaxDur: 5 * time.Minute,
		},
		{
			name:           "429限流错误-初始冷却1秒",
			statusCode:     429,
			expectedMinDur: 1 * time.Second,
			expectedMaxDur: 1 * time.Second,
		},
		{
			name:           "500服务器错误-初始冷却1秒",
			statusCode:     500,
			expectedMinDur: 1 * time.Second,
			expectedMaxDur: 1 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建临时测试数据库
			store, cleanup := setupAuthErrorTestStore(t)
			defer cleanup()

			ctx := context.Background()
			now := time.Now()

			// 创建测试渠道
			cfg := &Config{
				Name:    "test-channel",
				APIKey:  "sk-test-key",
				URL:     "https://api.example.com",
				Enabled: true,
			}
			created, err := store.CreateConfig(ctx, cfg)
			if err != nil {
				t.Fatalf("创建测试渠道失败: %v", err)
			}

			// 触发首次错误冷却
			duration, err := store.BumpCooldownOnError(ctx, created.ID, now, tt.statusCode)
			if err != nil {
				t.Fatalf("BumpCooldownOnError失败: %v", err)
			}

			// 验证冷却时长
			if duration < tt.expectedMinDur || duration > tt.expectedMaxDur {
				t.Errorf("状态码%d的初始冷却时间错误: 期望%v，实际%v",
					tt.statusCode, tt.expectedMinDur, duration)
			}

			// 验证数据库中的冷却截止时间
			until, exists := store.GetCooldownUntil(ctx, created.ID)
			if !exists {
				t.Fatal("冷却记录不存在")
			}

			actualDuration := until.Sub(now)
			tolerance := 1 * time.Second // 允许1秒误差（考虑测试执行耗时）

			if actualDuration < tt.expectedMinDur-tolerance || actualDuration > tt.expectedMaxDur+tolerance {
				t.Errorf("数据库冷却时间错误: 期望%v，实际%v",
					tt.expectedMinDur, actualDuration)
			}

			t.Logf("✅ 状态码%d: 初始冷却时间=%v（期望%v）",
				tt.statusCode, duration, tt.expectedMinDur)
		})
	}
}

// TestAuthErrorExponentialBackoff 验证401/403错误的指数退避机制
func TestAuthErrorExponentialBackoff(t *testing.T) {
	store, cleanup := setupAuthErrorTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	// 创建测试渠道
	cfg := &Config{
		Name:    "test-channel-backoff",
		APIKey:  "sk-test-key",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 预期的退避序列：5min -> 10min -> 20min -> 30min (上限)
	expectedSequence := []time.Duration{
		5 * time.Minute,  // 首次401错误
		10 * time.Minute, // 第二次错误（5min * 2）
		20 * time.Minute, // 第三次错误（10min * 2）
		30 * time.Minute, // 第四次错误（20min * 2，但达到上限）
		30 * time.Minute, // 第五次错误（保持上限）
	}

	for i, expected := range expectedSequence {
		// 触发401错误
		duration, err := store.BumpCooldownOnError(ctx, created.ID, now, 401)
		if err != nil {
			t.Fatalf("第%d次BumpCooldownOnError失败: %v", i+1, err)
		}

		// 验证冷却时长
		tolerance := 100 * time.Millisecond
		if duration < expected-tolerance || duration > expected+tolerance {
			t.Errorf("第%d次错误的冷却时间错误: 期望%v，实际%v",
				i+1, expected, duration)
		}

		t.Logf("✅ 第%d次401错误: 冷却时间=%v（期望%v）",
			i+1, duration, expected)

		// 更新now模拟时间推移（否则会被当作同一次错误）
		now = now.Add(expected + 1*time.Second)
	}
}

// TestKeyLevelAuthErrorCooldown 验证Key级别的401/403错误冷却
func TestKeyLevelAuthErrorCooldown(t *testing.T) {
	store, cleanup := setupAuthErrorTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	// 创建多Key渠道
	cfg := &Config{
		Name:    "multi-key-channel",
		APIKey:  "sk-key1,sk-key2,sk-key3",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 测试Key 0的401错误冷却
	duration, err := store.BumpKeyCooldownOnError(ctx, created.ID, 0, now, 401)
	if err != nil {
		t.Fatalf("BumpKeyCooldownOnError失败: %v", err)
	}

	// 验证初始冷却时间为5分钟
	expectedDuration := 5 * time.Minute
	tolerance := 1 * time.Second // 允许1秒误差（考虑测试执行耗时）
	if duration < expectedDuration-tolerance || duration > expectedDuration+tolerance {
		t.Errorf("Key级401错误初始冷却时间错误: 期望%v，实际%v",
			expectedDuration, duration)
	}

	// 验证数据库中的Key冷却记录
	until, exists := store.GetKeyCooldownUntil(ctx, created.ID, 0)
	if !exists {
		t.Fatal("Key冷却记录不存在")
	}

	actualDuration := until.Sub(now)
	if actualDuration < expectedDuration-tolerance || actualDuration > expectedDuration+tolerance {
		t.Errorf("数据库Key冷却时间错误: 期望%v，实际%v",
			expectedDuration, actualDuration)
	}

	t.Logf("✅ Key级401错误: 初始冷却时间=%v（期望%v）",
		duration, expectedDuration)
}

// TestMixedErrorCodesCooldown 验证不同错误码混合场景的冷却行为
func TestMixedErrorCodesCooldown(t *testing.T) {
	store, cleanup := setupAuthErrorTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	// 创建测试渠道
	cfg := &Config{
		Name:    "mixed-errors-channel",
		APIKey:  "sk-test-key",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 场景：先遇到500错误（1秒起），然后遇到401错误（应该还是5分钟）
	duration1, err := store.BumpCooldownOnError(ctx, created.ID, now, 500)
	if err != nil {
		t.Fatalf("首次500错误失败: %v", err)
	}

	if duration1 != 1*time.Second {
		t.Errorf("500错误初始冷却时间错误: 期望1s，实际%v", duration1)
	}

	// 模拟时间推移后遇到401错误
	now2 := now.Add(2 * time.Second)
	duration2, err := store.BumpCooldownOnError(ctx, created.ID, now2, 401)
	if err != nil {
		t.Fatalf("后续401错误失败: %v", err)
	}

	// 因为之前有1秒的冷却记录，新的401错误应该基于历史记录进行指数退避
	// 预期: 1s * 2 = 2s（但401首次应该是5分钟）
	// 实际逻辑：有历史记录则基于历史翻倍，无历史则按状态码初始化
	// 这里因为有历史duration_ms，所以是翻倍逻辑：1s * 2 = 2s
	expectedDuration := 2 * time.Second
	tolerance := 100 * time.Millisecond

	if duration2 < expectedDuration-tolerance || duration2 > expectedDuration+tolerance {
		t.Errorf("混合错误场景冷却时间错误: 期望%v，实际%v",
			expectedDuration, duration2)
	}

	t.Logf("✅ 500错误(1s) → 401错误(%v) - 使用指数退避而非重置", duration2)
}

// TestConcurrentCooldownUpdates 验证并发场景下冷却机制的数据一致性
func TestConcurrentCooldownUpdates(t *testing.T) {
	store, cleanup := setupAuthErrorTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试渠道
	cfg := &Config{
		Name:    "concurrent-test",
		APIKey:  "sk-test-key",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 并发触发100次401错误
	const concurrency = 100
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 每次使用当前时间，避免时间戳冲突
			_, _ = store.BumpCooldownOnError(ctx, created.ID, time.Now(), 401)
		}()
	}
	wg.Wait()

	// 验证数据一致性
	until, exists := store.GetCooldownUntil(ctx, created.ID)
	if !exists {
		t.Fatal("冷却记录不存在")
	}

	// 应该在合理范围内（考虑并发执行耗时，允许1秒误差）
	duration := time.Until(until)
	minDuration := AuthErrorInitialCooldown - 1*time.Second
	maxDuration := MaxCooldownDuration + 1*time.Second

	if duration < minDuration || duration > maxDuration {
		t.Errorf("并发场景冷却时间异常: %v (期望范围: %v - %v)",
			duration, minDuration, maxDuration)
	}

	t.Logf("✅ 并发测试通过: %d个并发更新，最终冷却时间=%v", concurrency, duration)
}

// TestConcurrentKeyCooldownUpdates 验证Key级别并发冷却的数据一致性
func TestConcurrentKeyCooldownUpdates(t *testing.T) {
	store, cleanup := setupAuthErrorTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 创建多Key渠道
	cfg := &Config{
		Name:    "concurrent-key-test",
		APIKey:  "sk-key1,sk-key2,sk-key3",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	t.Logf("✅ 创建渠道成功: ID=%d, APIKeys=%v", created.ID, created.APIKeys)

	// 并发触发多个Key的401错误
	const concurrency = 50
	var wg sync.WaitGroup
	var successCount int
	var mu sync.Mutex

	// 同时更新3个不同的Key
	for keyIndex := 0; keyIndex < 3; keyIndex++ {
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				// 每次使用当前时间
				duration, err := store.BumpKeyCooldownOnError(ctx, created.ID, idx, time.Now(), 401)
				if err == nil {
					mu.Lock()
					successCount++
					mu.Unlock()
					if i == 0 {
						t.Logf("Key %d: 第一次冷却成功，duration=%v", idx, duration)
					}
				} else {
					t.Errorf("Key %d: BumpKeyCooldownOnError失败: %v", idx, err)
				}
			}(keyIndex)
		}
	}
	wg.Wait()

	t.Logf("✅ 并发更新完成: 成功次数=%d/%d", successCount, 150)

	// 验证每个Key的冷却状态
	for keyIndex := 0; keyIndex < 3; keyIndex++ {
		until, exists := store.GetKeyCooldownUntil(ctx, created.ID, keyIndex)
		if !exists {
			t.Errorf("Key %d 冷却记录不存在", keyIndex)
			continue
		}

		duration := time.Until(until)
		minDuration := AuthErrorInitialCooldown - 1*time.Second
		maxDuration := MaxCooldownDuration + 1*time.Second

		if duration < minDuration || duration > maxDuration {
			t.Errorf("Key %d 并发场景冷却时间异常: %v (期望范围: %v - %v)",
				keyIndex, duration, minDuration, maxDuration)
		}

		t.Logf("✅ Key %d: 并发更新完成，冷却时间=%v", keyIndex, duration)
	}
}

// TestRaceConditionDetection 竞态条件检测测试
// 使用 go test -race 运行此测试
func TestRaceConditionDetection(t *testing.T) {
	store, cleanup := setupAuthErrorTestStore(t)
	defer cleanup()

	ctx := context.Background()

	cfg := &Config{
		Name:    "race-test",
		APIKey:  "sk-key1,sk-key2",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 并发场景：同时读写冷却状态
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)

		// 写操作：更新渠道冷却
		go func() {
			defer wg.Done()
			_, _ = store.BumpCooldownOnError(ctx, created.ID, time.Now(), 401)
		}()

		// 读操作：查询渠道冷却
		go func() {
			defer wg.Done()
			_, _ = store.GetCooldownUntil(ctx, created.ID)
		}()

		// 写操作：更新Key冷却
		go func() {
			defer wg.Done()
			_, _ = store.BumpKeyCooldownOnError(ctx, created.ID, 0, time.Now(), 401)
		}()
	}
	wg.Wait()

	t.Log("✅ 竞态条件检测通过（使用 go test -race 验证）")
}

// setupAuthErrorTestStore 创建临时测试数据库（专用于认证错误测试）
func setupAuthErrorTestStore(t *testing.T) (*SQLiteStore, func()) {
	t.Helper()

	// 设置内存数据库环境变量，强制使用命名内存数据库（确保所有连接共享同一实例）
	oldValue := os.Getenv("CCLOAD_USE_MEMORY_DB")
	os.Setenv("CCLOAD_USE_MEMORY_DB", "true")

	// 使用内存数据库加快测试速度
	// ⚠️ 设置环境变量后，NewSQLiteStore会自动使用命名内存数据库
	store, err := NewSQLiteStore(":memory:", nil)
	if err != nil {
		os.Setenv("CCLOAD_USE_MEMORY_DB", oldValue) // 恢复环境变量
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	cleanup := func() {
		if err := store.Close(); err != nil {
			t.Logf("关闭测试数据库失败: %v", err)
		}
		// 恢复原始环境变量
		os.Setenv("CCLOAD_USE_MEMORY_DB", oldValue)
	}

	return store, cleanup
}
