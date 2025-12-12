package app

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// contextKey 自定义类型用于 context key，避免 SA1029 警告
type contextKey string

const testingContextKey contextKey = "testing"

// TestSelectAvailableKey_SingleKey 测试单Key场景
func TestSelectAvailableKey_SingleKey(t *testing.T) {
	store, cleanup := setupTestKeyStore(t)
	defer cleanup()

	var cooldownGauge atomic.Int64
	selector := NewKeySelector(&cooldownGauge) // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:     "single-key-channel",
		URL:      "https://api.com",
		Priority: 100,
		Models:   []string{"test-model"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建单个API Key
	err = store.CreateAPIKey(ctx, &model.APIKey{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-single-key",
		KeyStrategy: model.KeyStrategySequential,
	})
	if err != nil {
		t.Fatalf("创建API Key失败: %v", err)
	}

	// 预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("首次选择", func(t *testing.T) {
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 0 {
			t.Errorf("期望keyIndex=0，实际%d", keyIndex)
		}

		if apiKey != "sk-single-key" {
			t.Errorf("期望apiKey=sk-single-key，实际%s", apiKey)
		}

		t.Logf("[INFO] 单Key场景选择正确: keyIndex=%d", keyIndex)
	})

	t.Run("排除唯一Key后无可用Key", func(t *testing.T) {
		excludeKeys := map[int]bool{0: true}
		_, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err == nil {
			t.Error("期望返回错误（唯一Key已被排除），但成功返回")
		}

		t.Logf("[INFO] 单Key被排除后正确返回错误: %v", err)
	})
}

// TestSelectAvailableKey_SingleKeyCooldown 测试单Key冷却场景（修复Bug验证）
func TestSelectAvailableKey_SingleKeyCooldown(t *testing.T) {
	store, cleanup := setupTestKeyStore(t)
	defer cleanup()

	var cooldownGauge atomic.Int64
	selector := NewKeySelector(&cooldownGauge)
	ctx := context.WithValue(context.Background(), testingContextKey, true)
	now := time.Now()

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:     "single-key-cooldown-channel",
		URL:      "https://api.com",
		Priority: 100,
		Models:   []string{"test-model"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建单个API Key
	err = store.CreateAPIKey(ctx, &model.APIKey{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-single-cooldown-key",
		KeyStrategy: model.KeyStrategySequential,
	})
	if err != nil {
		t.Fatalf("创建API Key失败: %v", err)
	}

	// 冷却这个唯一的Key
	_, err = store.BumpKeyCooldown(ctx, cfg.ID, 0, now, 401)
	if err != nil {
		t.Fatalf("冷却Key失败: %v", err)
	}

	// 预先查询apiKeys（在冷却之后，包含冷却状态）
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("单Key冷却后应返回错误", func(t *testing.T) {
		_, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err == nil {
			t.Error("期望返回错误（单Key在冷却中），但成功返回")
		}

		// 验证错误消息包含冷却信息
		if !strings.Contains(err.Error(), "cooldown") {
			t.Errorf("错误消息应包含'cooldown'，实际: %v", err)
		}

		t.Logf("[INFO] 单Key冷却后正确返回错误: %v", err)
	})
}

// TestSelectAvailableKey_Sequential 测试顺序策略
func TestSelectAvailableKey_Sequential(t *testing.T) {
	store, cleanup := setupTestKeyStore(t)
	defer cleanup()

	var cooldownGauge atomic.Int64
	selector := NewKeySelector(&cooldownGauge) // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:     "sequential-channel",
		URL:      "https://api.com",
		Priority: 100,
		Models:   []string{"test-model"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建3个API Keys（顺序策略）
	for i := 0; i < 3; i++ {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-seq-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		})
		if err != nil {
			t.Fatalf("创建API Key %d失败: %v", i, err)
		}
	}

	// 预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("首次选择返回第一个Key", func(t *testing.T) {
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 0 {
			t.Errorf("顺序策略首次应返回keyIndex=0，实际%d", keyIndex)
		}

		if apiKey != "sk-seq-key-0" {
			t.Errorf("期望apiKey=sk-seq-key-0，实际%s", apiKey)
		}

		t.Logf("[INFO] 顺序策略首次选择正确: keyIndex=%d", keyIndex)
	})

	t.Run("排除第一个Key后返回第二个", func(t *testing.T) {
		excludeKeys := map[int]bool{0: true}
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 1 {
			t.Errorf("排除Key0后应返回keyIndex=1，实际%d", keyIndex)
		}

		if apiKey != "sk-seq-key-1" {
			t.Errorf("期望apiKey=sk-seq-key-1，实际%s", apiKey)
		}

		t.Logf("[INFO] 顺序策略排除后选择正确: keyIndex=%d", keyIndex)
	})

	t.Run("排除前两个Key后返回第三个", func(t *testing.T) {
		excludeKeys := map[int]bool{0: true, 1: true}
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 2 {
			t.Errorf("排除Key0和Key1后应返回keyIndex=2，实际%d", keyIndex)
		}

		if apiKey != "sk-seq-key-2" {
			t.Errorf("期望apiKey=sk-seq-key-2，实际%s", apiKey)
		}

		t.Logf("[INFO] 顺序策略多次排除后选择正确: keyIndex=%d", keyIndex)
	})

	t.Run("所有Key被排除后返回错误", func(t *testing.T) {
		excludeKeys := map[int]bool{0: true, 1: true, 2: true}
		_, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err == nil {
			t.Error("期望返回错误（所有Key已被排除），但成功返回")
		}

		t.Logf("[INFO] 所有Key被排除后正确返回错误: %v", err)
	})
}

// TestSelectAvailableKey_RoundRobin 测试轮询策略
func TestSelectAvailableKey_RoundRobin(t *testing.T) {
	store, cleanup := setupTestKeyStore(t)
	defer cleanup()

	var cooldownGauge atomic.Int64
	selector := NewKeySelector(&cooldownGauge) // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:     "roundrobin-channel",
		URL:      "https://api.com",
		Priority: 100,
		Models:   []string{"test-model"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建3个API Keys（轮询策略）
	for i := 0; i < 3; i++ {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-rr-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategyRoundRobin,
		})
		if err != nil {
			t.Fatalf("创建API Key %d失败: %v", i, err)
		}
	}

	// 预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("连续调用应轮询返回不同Key", func(t *testing.T) {
		// [INFO] Linus风格：轮询指针内存化后，起始位置不确定（每次测试可能不同）
		// 验证策略：确保5次调用真正轮询（没有连续重复，且访问了所有Key）

		var selectedKeys []int
		keysSeen := make(map[int]bool)

		for i := 0; i < 5; i++ {
			keyIndex, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)
			if err != nil {
				t.Fatalf("第%d次SelectAvailableKey失败: %v", i+1, err)
			}
			selectedKeys = append(selectedKeys, keyIndex)
			keysSeen[keyIndex] = true
		}

		// 验证1：5次调用应访问所有3个Key
		if len(keysSeen) != 3 {
			t.Errorf("轮询失败: 只访问了%d个Key，期望3个。序列: %v", len(keysSeen), selectedKeys)
		}

		// 验证2：没有连续两次选择同一个Key（真正轮询）
		for i := 1; i < len(selectedKeys); i++ {
			if selectedKeys[i] == selectedKeys[i-1] {
				t.Errorf("轮询失败: 连续选择了相同Key=%d", selectedKeys[i])
			}
		}

		t.Logf("[INFO] 轮询策略正确: %v", selectedKeys)
	})

	t.Run("排除当前Key后跳到下一个", func(t *testing.T) {
		// [INFO] 内存化后无需重置索引

		// 第一次排除Key0
		excludeKeys := map[int]bool{0: true}
		keyIndex, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 1 {
			t.Errorf("排除Key0后应返回keyIndex=1，实际%d", keyIndex)
		}

		t.Logf("[INFO] 轮询策略排除后选择正确: keyIndex=%d", keyIndex)
	})
}

// TestSelectAvailableKey_KeyCooldown 测试Key冷却过滤
func TestSelectAvailableKey_KeyCooldown(t *testing.T) {
	store, cleanup := setupTestKeyStore(t)
	defer cleanup()

	var cooldownGauge atomic.Int64
	selector := NewKeySelector(&cooldownGauge) // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)
	now := time.Now()

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:     "cooldown-channel",
		URL:      "https://api.com",
		Priority: 100,
		Models:   []string{"test-model"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建3个API Keys
	for i := 0; i < 3; i++ {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-cooldown-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		})
		if err != nil {
			t.Fatalf("创建API Key %d失败: %v", i, err)
		}
	}

	// 冷却Key0
	_, err = store.BumpKeyCooldown(ctx, cfg.ID, 0, now, 401)
	if err != nil {
		t.Fatalf("冷却Key0失败: %v", err)
	}

	// 预先查询apiKeys（在冷却Key0之后，包含冷却状态）
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("冷却的Key被跳过", func(t *testing.T) {
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		// 应该跳过冷却的Key0，返回Key1
		if keyIndex != 1 {
			t.Errorf("期望跳过冷却的Key0返回keyIndex=1，实际%d", keyIndex)
		}

		if apiKey != "sk-cooldown-key-1" {
			t.Errorf("期望apiKey=sk-cooldown-key-1，实际%s", apiKey)
		}

		t.Logf("[INFO] Key冷却过滤正确: 跳过Key0，选择Key1")
	})

	t.Run("冷却多个Key", func(t *testing.T) {
		// 再冷却Key1
		_, err = store.BumpKeyCooldown(ctx, cfg.ID, 1, now, 401)
		if err != nil {
			t.Fatalf("冷却Key1失败: %v", err)
		}

		// 重新查询apiKeys以获取最新冷却状态
		apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("查询API Keys失败: %v", err)
		}

		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		// 应该跳过冷却的Key0和Key1，返回Key2
		if keyIndex != 2 {
			t.Errorf("期望跳过Key0和Key1返回keyIndex=2，实际%d", keyIndex)
		}

		if apiKey != "sk-cooldown-key-2" {
			t.Errorf("期望apiKey=sk-cooldown-key-2，实际%s", apiKey)
		}

		t.Logf("[INFO] 多Key冷却过滤正确: 跳过Key0和Key1，选择Key2")
	})

	t.Run("所有Key冷却后返回错误", func(t *testing.T) {
		// 再冷却Key2
		_, err = store.BumpKeyCooldown(ctx, cfg.ID, 2, now, 401)
		if err != nil {
			t.Fatalf("冷却Key2失败: %v", err)
		}

		// 重新查询apiKeys以获取最新冷却状态
		apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("查询API Keys失败: %v", err)
		}

		_, _, err = selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err == nil {
			t.Error("期望返回错误（所有Key都在冷却），但成功返回")
		}

		t.Logf("[INFO] 所有Key冷却后正确返回错误: %v", err)
	})
}

// TestSelectAvailableKey_CooldownAndExclude 测试冷却与排除组合
func TestSelectAvailableKey_CooldownAndExclude(t *testing.T) {
	store, cleanup := setupTestKeyStore(t)
	defer cleanup()

	var cooldownGauge atomic.Int64
	selector := NewKeySelector(&cooldownGauge) // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)
	now := time.Now()

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:     "combined-channel",
		URL:      "https://api.com",
		Priority: 100,
		Models:   []string{"test-model"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建4个API Keys
	for i := 0; i < 4; i++ {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-combined-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		})
		if err != nil {
			t.Fatalf("创建API Key %d失败: %v", i, err)
		}
	}

	// 冷却Key1
	_, err = store.BumpKeyCooldown(ctx, cfg.ID, 1, now, 401)
	if err != nil {
		t.Fatalf("冷却Key1失败: %v", err)
	}

	// 预先查询apiKeys（在冷却Key1之后，包含冷却状态）
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	// 排除Key0和Key2
	excludeKeys := map[int]bool{0: true, 2: true}

	keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

	if err != nil {
		t.Fatalf("SelectAvailableKey失败: %v", err)
	}

	// 应该跳过排除的Key0和Key2、冷却的Key1，返回Key3
	if keyIndex != 3 {
		t.Errorf("期望返回keyIndex=3（跳过排除和冷却的Key），实际%d", keyIndex)
	}

	if apiKey != "sk-combined-key-3" {
		t.Errorf("期望apiKey=sk-combined-key-3，实际%s", apiKey)
	}

	t.Logf("[INFO] 冷却与排除组合过滤正确: 跳过Key0(排除)、Key1(冷却)、Key2(排除)，选择Key3")
}

// TestSelectAvailableKey_NoKeys 测试无Key配置场景
func TestSelectAvailableKey_NoKeys(t *testing.T) {
	store, cleanup := setupTestKeyStore(t)
	defer cleanup()

	var cooldownGauge atomic.Int64
	selector := NewKeySelector(&cooldownGauge) // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道（不配置API Keys）
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:     "no-keys-channel",
		URL:      "https://api.com",
		Priority: 100,
		Models:   []string{"test-model"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 预先查询apiKeys（应该为空）
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	_, _, err = selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

	if err == nil {
		t.Error("期望返回错误（渠道未配置API Keys），但成功返回")
	}

	t.Logf("[INFO] 无Key配置场景正确返回错误: %v", err)
}

// TestSelectAvailableKey_DefaultStrategy 测试默认策略
func TestSelectAvailableKey_DefaultStrategy(t *testing.T) {
	store, cleanup := setupTestKeyStore(t)
	defer cleanup()

	var cooldownGauge atomic.Int64
	selector := NewKeySelector(&cooldownGauge) // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:     "default-strategy-channel",
		URL:      "https://api.com",
		Priority: 100,
		Models:   []string{"test-model"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建2个API Keys（不指定策略）
	for i := 0; i < 2; i++ {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-default-key-" + string(rune('0'+i)),
			KeyStrategy: "", // 空策略，应使用默认sequential
		})
		if err != nil {
			t.Fatalf("创建API Key %d失败: %v", i, err)
		}
	}

	// 预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	// 首次选择应返回Key0（默认sequential策略）
	keyIndex, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

	if err != nil {
		t.Fatalf("SelectAvailableKey失败: %v", err)
	}

	if keyIndex != 0 {
		t.Errorf("默认策略首次应返回keyIndex=0，实际%d", keyIndex)
	}

	t.Logf("[INFO] 默认策略（sequential）正确生效")
}

// TestSelectAvailableKey_UnknownStrategy 测试未知策略回退到默认
func TestSelectAvailableKey_UnknownStrategy(t *testing.T) {
	store, cleanup := setupTestKeyStore(t)
	defer cleanup()

	var cooldownGauge atomic.Int64
	selector := NewKeySelector(&cooldownGauge) // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:     "unknown-strategy-channel",
		URL:      "https://api.com",
		Priority: 100,
		Models:   []string{"test-model"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建2个API Keys（使用未知策略）
	for i := 0; i < 2; i++ {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-unknown-key-" + string(rune('0'+i)),
			KeyStrategy: "unknown-strategy", // 未知策略，应回退到sequential
		})
		if err != nil {
			t.Fatalf("创建API Key %d失败: %v", i, err)
		}
	}

	// 预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	// 首次选择应返回Key0（回退到sequential策略）
	keyIndex, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

	if err != nil {
		t.Fatalf("SelectAvailableKey失败: %v", err)
	}

	if keyIndex != 0 {
		t.Errorf("未知策略应回退到sequential，首次应返回keyIndex=0，实际%d", keyIndex)
	}

	t.Logf("[INFO] 未知策略正确回退到默认sequential")
}

// ========== 辅助函数 ==========

func setupTestKeyStore(t *testing.T) (storage.Store, func()) {
	t.Helper()

	tmpDB := t.TempDir() + "/key_selector_test.db"
	store, err := storage.CreateSQLiteStore(tmpDB, nil)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	cleanup := func() {
		store.Close()
	}

	return store, cleanup
}
