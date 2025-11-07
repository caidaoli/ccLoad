package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/app"
	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
)

// contextKey 自定义类型用于 context key，避免 SA1029 警告
type contextKey string

const testingContextKey contextKey = "testing"

// TestConcurrentKeySelection 测试高并发Key选择时的数据竞争和正确性
// 场景：1000个并发请求同时选择Key
// 验证：无数据竞争、Key分布合理、无意外错误
func TestConcurrentKeySelection(t *testing.T) {
	// 创建临时数据库
	store, cleanup := setupTestStore(t)
	defer cleanup()

	// 设置testing context以启用同步更新模式，确保测试的准确性
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建测试渠道（10个Key）
	channelID := createTestChannelWithKeys(t, store, 10, "round_robin")

	// 获取渠道配置
	cfg, err := store.GetConfig(ctx, channelID)
	if err != nil {
		t.Fatalf("Failed to get config: %v", err)
	}

	// ✅ Linus风格：轮询指针内存化后无需初始化

	// 初始化KeySelector
	var keyCooldownGauge atomic.Int64
	selector := app.NewKeySelector(&keyCooldownGauge) // 移除store参数

	// 预先查询apiKeys，避免并发重复查询
	apiKeys, err := store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("Failed to get API keys: %v", err)
	}

	// 并发测试参数
	concurrency := 1000
	var wg sync.WaitGroup
	errors := make(chan error, concurrency)
	selectedKeys := make(chan int, concurrency)

	// 启动并发Key选择
	startTime := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)
			if err != nil {
				errors <- fmt.Errorf("goroutine %d: %w", idx, err)
				return
			}

			// 验证返回值
			if keyIndex < 0 || keyIndex >= 10 {
				errors <- fmt.Errorf("goroutine %d: invalid keyIndex %d", idx, keyIndex)
				return
			}
			if apiKey == "" {
				errors <- fmt.Errorf("goroutine %d: empty apiKey", idx)
				return
			}

			selectedKeys <- keyIndex
		}(i)
	}

	wg.Wait()
	close(errors)
	close(selectedKeys)

	duration := time.Since(startTime)

	// 收集错误
	var errorList []error
	for err := range errors {
		errorList = append(errorList, err)
	}

	// 统计Key分布
	keyDistribution := make(map[int]int)
	for keyIndex := range selectedKeys {
		keyDistribution[keyIndex]++
	}

	// 验证结果
	t.Logf("并发测试完成: %d 个请求, 耗时 %v", concurrency, duration)
	t.Logf("平均延迟: %v/请求", duration/time.Duration(concurrency))

	if len(errorList) > 0 {
		t.Errorf("发现 %d 个错误:", len(errorList))
		for i, err := range errorList {
			if i < 10 { // 仅打印前10个错误
				t.Errorf("  - %v", err)
			}
		}
		if len(errorList) > 10 {
			t.Errorf("  ... 省略 %d 个错误", len(errorList)-10)
		}
	}

	// 验证Key分布（round_robin策略应该相对均匀）
	t.Logf("Key分布统计:")
	for keyIndex := 0; keyIndex < 10; keyIndex++ {
		count := keyDistribution[keyIndex]
		percentage := float64(count) / float64(concurrency) * 100
		t.Logf("  Key %d: %d 次 (%.1f%%)", keyIndex, count, percentage)
	}

	// 验证所有Key都被使用过（round_robin策略）
	for keyIndex := 0; keyIndex < 10; keyIndex++ {
		if keyDistribution[keyIndex] == 0 {
			t.Errorf("Key %d 从未被选中（round_robin策略应该均匀分布）", keyIndex)
		}
	}
}

// TestConcurrentKeyCooldown 测试并发Key冷却操作的正确性
// 场景：同时冷却和选择Key
// 验证：冷却状态正确、无数据竞争、无死锁
func TestConcurrentKeyCooldown(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试渠道（5个Key）
	channelID := createTestChannelWithKeys(t, store, 5, "sequential")
	cfg, err := store.GetConfig(ctx, channelID)
	if err != nil {
		t.Fatalf("Failed to get config: %v", err)
	}

	var keyCooldownGauge atomic.Int64
	selector := app.NewKeySelector(&keyCooldownGauge) // 移除store参数

	var wg sync.WaitGroup
	errors := make(chan error, 100)

	// 并发场景：50个选择 + 50个冷却
	for i := 0; i < 50; i++ {
		// 选择Key
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// 每次查询最新的apiKeys以获取最新冷却状态
			currentKeys, err := store.GetAPIKeys(ctx, channelID)
			if err != nil {
				errors <- fmt.Errorf("select %d get keys: %w", idx, err)
				return
			}
			_, _, err = selector.SelectAvailableKey(cfg.ID, currentKeys, nil)
			if err != nil {
				errors <- fmt.Errorf("select %d: %w", idx, err)
			}
		}(i)

		// 冷却Key（直接调用store，不再使用已删除的MarkKeyError）
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			keyIndex := idx % 5 // 轮流冷却5个Key
			_, err := store.BumpKeyCooldown(ctx, channelID, keyIndex, time.Now(), 429)
			if err != nil {
				errors <- fmt.Errorf("cooldown %d: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// 收集错误（排除预期的"所有Key冷却"错误）
	var unexpectedErrors []error
	for err := range errors {
		errStr := err.Error()
		// "all API keys are in cooldown" 是预期错误（使用包含匹配，因为可能有前缀）
		if !strings.Contains(errStr, "all API keys are in cooldown or already tried") {
			unexpectedErrors = append(unexpectedErrors, err)
		}
	}

	if len(unexpectedErrors) > 0 {
		t.Errorf("发现 %d 个意外错误:", len(unexpectedErrors))
		for i, err := range unexpectedErrors {
			if i < 5 {
				t.Errorf("  - %v", err)
			}
		}
	}
}

// TestConcurrentChannelOperations 测试并发渠道操作
// 场景：同时创建、更新、删除渠道
// 验证：数据一致性、无数据竞争
func TestConcurrentChannelOperations(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	var wg sync.WaitGroup
	errors := make(chan error, 100)

	// 并发创建10个渠道
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			cfg := &model.Config{
				Name:        fmt.Sprintf("concurrent-channel-%d", idx),
				URL:         "https://api.example.com",
				Priority:    idx,
				Models:      []string{"model-1"},
				ChannelType: "anthropic",
				Enabled:     true,
			}

			if _, err := store.CreateConfig(ctx, cfg); err != nil {
				errors <- fmt.Errorf("create channel %d: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// 验证错误
	for err := range errors {
		t.Errorf("并发创建错误: %v", err)
	}

	// 验证所有渠道都被创建
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("Failed to get configs: %v", err)
	}

	if len(configs) != 10 {
		t.Errorf("Expected 10 channels, got %d", len(configs))
	}
}

// 辅助函数：创建测试Store
func setupTestStore(t *testing.T) (*sqlite.SQLiteStore, func()) {
	// 设置内存模式环境变量（测试使用内存数据库以提高速度）
	t.Setenv("CCLOAD_USE_MEMORY_DB", "true")

	// 使用内存数据库（无需Redis，因为测试数据不需要持久化）
	// ⚠️ 注意：生产环境的内存模式强制要求Redis，但测试环境豁免
	// 解决方案：跳过Redis检查（通过传nil）
	store, err := sqlite.NewSQLiteStore(":memory:", &mockRedisSync{})
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	cleanup := func() {
		_ = store.Close()
	}

	return store, cleanup
}

// mockRedisSync 用于测试的Mock Redis同步实现
// 实现最小接口以绕过Redis强制检查，但不执行实际同步
type mockRedisSync struct{}

func (m *mockRedisSync) IsEnabled() bool {
	return true // 假装Redis已启用，绕过安全检查
}

func (m *mockRedisSync) LoadChannelsWithKeysFromRedis(ctx context.Context) ([]*model.ChannelWithKeys, error) {
	return nil, nil // 测试环境无需从Redis加载
}

func (m *mockRedisSync) SyncAllChannelsWithKeys(ctx context.Context, channels []*model.ChannelWithKeys) error {
	return nil // 测试环境不执行实际同步
}

// Auth Tokens 同步方法 (新增 2025-11)
func (m *mockRedisSync) SyncAllAuthTokens(ctx context.Context, tokens []*model.AuthToken) error {
	return nil // 测试环境不执行实际同步
}

func (m *mockRedisSync) LoadAuthTokensFromRedis(ctx context.Context) ([]*model.AuthToken, error) {
	return nil, nil // 测试环境无需从Redis加载
}

// 辅助函数：创建带多个Key的测试渠道
func createTestChannelWithKeys(t *testing.T, store *sqlite.SQLiteStore, keyCount int, strategy string) int64 {
	ctx := context.Background()

	// 1. 创建渠道配置（不包含APIKeys）
	cfg := &model.Config{
		Name:        "test-concurrent-channel",
		URL:         "https://api.example.com",
		Priority:    10,
		Models:      []string{"test-model"},
		ChannelType: "anthropic",
		Enabled:     true,
	}

	createdCfg, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("Failed to create test channel: %v", err)
	}

	// 2. 为渠道创建多个API Keys
	for i := 0; i < keyCount; i++ {
		apiKey := &model.APIKey{
			ChannelID:   createdCfg.ID,
			KeyIndex:    i,
			APIKey:      fmt.Sprintf("sk-test-key-%d", i),
			KeyStrategy: strategy, // KeyStrategy属于APIKey而非Config
		}
		if err := store.CreateAPIKey(ctx, apiKey); err != nil {
			t.Fatalf("Failed to create API key %d: %v", i, err)
		}
	}

	return createdCfg.ID
}
