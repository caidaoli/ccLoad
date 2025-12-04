package sqlite

import (
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"context"
	"os"
	"sync"
	"sync/atomic"
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
			name:           "429限流错误-初始冷却10秒",
			statusCode:     429,
			expectedMinDur: 10 * time.Second,
			expectedMaxDur: 10 * time.Second,
		},
		{
			name:           "500服务器错误-初始冷却2分钟",
			statusCode:     500,
			expectedMinDur: 2 * time.Minute,
			expectedMaxDur: 2 * time.Minute,
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
			cfg := &model.Config{
				Name:    "test-channel",
				URL:     "https://api.example.com",
				Enabled: true,
			}
			created, err := store.CreateConfig(ctx, cfg)
			if err != nil {
				t.Fatalf("创建测试渠道失败: %v", err)
			}

			// 触发首次错误冷却
			duration, err := store.BumpChannelCooldown(ctx, created.ID, now, tt.statusCode)
			if err != nil {
				t.Fatalf("BumpCooldownOnError失败: %v", err)
			}

			// 验证冷却时长
			if duration < tt.expectedMinDur || duration > tt.expectedMaxDur {
				t.Errorf("状态码%d的初始冷却时间错误: 期望%v，实际%v",
					tt.statusCode, tt.expectedMinDur, duration)
			}

			// 验证数据库中的冷却截止时间
			until, exists := getChannelCooldownUntil(ctx, store, created.ID)
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
	cfg := &model.Config{
		Name:    "test-channel-backoff",
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
		duration, err := store.BumpChannelCooldown(ctx, created.ID, now, 401)
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
	cfg := &model.Config{
		Name:    "multi-key-channel",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 创建3个API Keys
	for i, key := range []string{"sk-key1", "sk-key2", "sk-key3"} {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    i,
			APIKey:      key,
			KeyStrategy: "sequential",
		})
		if err != nil {
			t.Fatalf("创建API Key %d失败: %v", i, err)
		}
	}

	// 测试Key 0的401错误冷却
	duration, err := store.BumpKeyCooldown(ctx, created.ID, 0, now, 401)
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
	until, exists := getKeyCooldownUntil(ctx, store, created.ID, 0)
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
	cfg := &model.Config{
		Name:    "mixed-errors-channel",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 场景：先遇到500错误（2分钟起），然后遇到401错误（应该还是5分钟）
	duration1, err := store.BumpChannelCooldown(ctx, created.ID, now, 500)
	if err != nil {
		t.Fatalf("首次500错误失败: %v", err)
	}

	if duration1 != 2*time.Minute {
		t.Errorf("500错误初始冷却时间错误: 期望2分钟，实际%v", duration1)
	}

	// 模拟时间推移后遇到401错误
	now2 := now.Add(3 * time.Minute)
	duration2, err := store.BumpChannelCooldown(ctx, created.ID, now2, 401)
	if err != nil {
		t.Fatalf("后续401错误失败: %v", err)
	}

	// 因为之前有2分钟的冷却记录，新的401错误应该基于历史记录进行指数退避
	// 预期: 2min * 2 = 4min（但401首次应该是5分钟）
	// 实际逻辑：有历史记录则基于历史翻倍，无历史则按状态码初始化
	// 这里因为有历史duration_ms，所以是翻倍逻辑：2min * 2 = 4min
	expectedDuration := 4 * time.Minute
	tolerance := 100 * time.Millisecond

	if duration2 < expectedDuration-tolerance || duration2 > expectedDuration+tolerance {
		t.Errorf("混合错误场景冷却时间错误: 期望%v，实际%v",
			expectedDuration, duration2)
	}

	t.Logf("✅ 500错误(2min) → 401错误(%v) - 使用指数退避而非重置", duration2)
}

// TestConcurrentCooldownUpdates 验证并发场景下冷却机制的数据一致性
func TestConcurrentCooldownUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过并发测试（使用 -short 标志）")
	}

	store, cleanup := setupAuthErrorTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试渠道
	cfg := &model.Config{
		Name:    "concurrent-test",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 并发触发10次401错误（足以验证并发安全性）
	const concurrency = 10
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.BumpChannelCooldown(ctx, created.ID, time.Now(), 401)
		}()
	}
	wg.Wait()

	// 验证数据一致性
	until, exists := getChannelCooldownUntil(ctx, store, created.ID)
	if !exists {
		t.Fatal("冷却记录不存在")
	}

	duration := time.Until(until)
	minDuration := util.AuthErrorInitialCooldown - 1*time.Second
	maxDuration := util.MaxCooldownDuration + 1*time.Second

	if duration < minDuration || duration > maxDuration {
		t.Errorf("并发场景冷却时间异常: %v (期望范围: %v - %v)",
			duration, minDuration, maxDuration)
	}

	t.Logf("✅ 并发测试通过: %d个并发更新，最终冷却时间=%v", concurrency, duration)
}

// TestConcurrentKeyCooldownUpdates 验证Key级别并发冷却的数据一致性
func TestConcurrentKeyCooldownUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过并发测试（使用 -short 标志）")
	}

	store, cleanup := setupAuthErrorTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 创建多Key渠道
	cfg := &model.Config{
		Name:    "concurrent-key-test",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 创建3个API Keys
	for i, key := range []string{"sk-key1", "sk-key2", "sk-key3"} {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    i,
			APIKey:      key,
			KeyStrategy: "sequential",
		})
		if err != nil {
			t.Fatalf("创建API Key %d失败: %v", i, err)
		}
	}

	// 使用信号量控制并发度为2，避免过多BUSY错误
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	var successCount int32

	// 每个Key更新3次，共9次操作
	for keyIndex := 0; keyIndex < 3; keyIndex++ {
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				_, err := store.BumpKeyCooldown(ctx, created.ID, idx, time.Now(), 401)
				if err == nil {
					atomic.AddInt32(&successCount, 1)
				}
			}(keyIndex)
		}
	}
	wg.Wait()

	t.Logf("✅ 并发更新完成: 成功次数=%d/9", successCount)

	// 验证每个Key的冷却状态
	for keyIndex := 0; keyIndex < 3; keyIndex++ {
		until, exists := getKeyCooldownUntil(ctx, store, created.ID, keyIndex)
		if !exists {
			t.Errorf("Key %d 冷却记录不存在", keyIndex)
			continue
		}

		duration := time.Until(until)
		minDuration := util.AuthErrorInitialCooldown - 1*time.Second
		maxDuration := util.MaxCooldownDuration + 1*time.Second

		if duration < minDuration || duration > maxDuration {
			t.Errorf("Key %d 并发场景冷却时间异常: %v (期望范围: %v - %v)",
				keyIndex, duration, minDuration, maxDuration)
		}
	}
}

// TestRaceConditionDetection 竞态条件检测测试
// 使用 go test -race 运行此测试
func TestRaceConditionDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过竞态检测测试（使用 -short 标志）")
	}

	store, cleanup := setupAuthErrorTestStore(t)
	defer cleanup()

	ctx := context.Background()

	cfg := &model.Config{
		Name:    "race-test",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 创建2个API Keys
	for i, key := range []string{"sk-key1", "sk-key2"} {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    i,
			APIKey:      key,
			KeyStrategy: "sequential",
		})
		if err != nil {
			t.Fatalf("创建API Key %d失败: %v", i, err)
		}
	}

	// 并发场景：同时读写冷却状态（降低并发度）
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(3)

		// 写操作：更新渠道冷却
		go func() {
			defer wg.Done()
			_, _ = store.BumpChannelCooldown(ctx, created.ID, time.Now(), 401)
		}()

		// 写操作：更新Key冷却
		go func() {
			defer wg.Done()
			_, _ = store.BumpKeyCooldown(ctx, created.ID, 0, time.Now(), 401)
		}()

		// 读操作：获取渠道配置
		go func() {
			defer wg.Done()
			_, _ = store.GetConfig(ctx, created.ID)
		}()
	}

	wg.Wait()
	t.Log("✅ 竞态检测测试通过（使用 go test -race 运行以检测竞态条件）")
}

// setupAuthErrorTestStore 创建临时测试数据库（专用于认证错误测试）
func setupAuthErrorTestStore(t *testing.T) (*SQLiteStore, func()) {
	t.Helper()

	// ✅ 修复：禁用内存模式，避免Redis强制检查
	// 使用临时文件数据库确保测试稳定性
	oldValue := os.Getenv("CCLOAD_USE_MEMORY_DB")
	os.Setenv("CCLOAD_USE_MEMORY_DB", "false")

	// 使用t.TempDir()创建临时目录，测试结束自动清理
	tmpDB := t.TempDir() + "/test-auth-error.db"
	store, err := NewSQLiteStore(tmpDB, nil)
	if err != nil {
		os.Setenv("CCLOAD_USE_MEMORY_DB", oldValue)
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


// getChannelCooldownUntil 获取渠道冷却截止时间（测试辅助函数）
func getChannelCooldownUntil(ctx context.Context, store *SQLiteStore, channelID int64) (time.Time, bool) {
	cfg, err := store.GetConfig(ctx, channelID)
	if err != nil || cfg == nil {
		return time.Time{}, false
	}
	if cfg.CooldownUntil == 0 {
		return time.Time{}, false
	}
	until := time.Unix(cfg.CooldownUntil, 0)
	// 只有未过期的冷却才返回true
	return until, time.Now().Before(until)
}

// getKeyCooldownUntil 获取Key冷却截止时间（测试辅助函数）
func getKeyCooldownUntil(ctx context.Context, store *SQLiteStore, channelID int64, keyIndex int) (time.Time, bool) {
	key, err := store.GetAPIKey(ctx, channelID, keyIndex)
	if err != nil || key == nil {
		return time.Time{}, false
	}
	if key.CooldownUntil == 0 {
		return time.Time{}, false
	}
	until := time.Unix(key.CooldownUntil, 0)
	// 只有未过期的冷却才返回true
	return until, time.Now().Before(until)
}
