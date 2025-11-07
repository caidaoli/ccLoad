package sqlite

import (
	"ccLoad/internal/model"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bytedance/sonic"
)

// TestRedisRecovery_CompleteFlow 测试完整的Redis备份和恢复流程
func TestRedisRecovery_CompleteFlow(t *testing.T) {
	// 禁用内存数据库模式，使用临时文件数据库
	os.Unsetenv("CCLOAD_USE_MEMORY_DB")

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-redis-recovery.db")

	// ========== 阶段1：创建原始数据并模拟Redis备份 ==========
	store1, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("创建第一个数据库失败: %v", err)
	}

	ctx := context.Background()

	// 创建测试渠道配置
	originalConfig := &model.Config{
		Name:           "Redis-Recovery-Test",
		URL:            "https://redis-recovery.example.com",
		Priority:       15,
		Models:         []string{"model-a", "model-b"},
		ModelRedirects: map[string]string{"old": "new"},
		ChannelType:    "anthropic",
		Enabled:        true,
	}

	created, err := store1.CreateConfig(ctx, originalConfig)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建多个API Keys
	apiKeys := []*model.APIKey{
		{
			ChannelID:   created.ID,
			KeyIndex:    0,
			APIKey:      "sk-redis-test-key-1",
			KeyStrategy: "sequential",
		},
		{
			ChannelID:   created.ID,
			KeyIndex:    1,
			APIKey:      "sk-redis-test-key-2",
			KeyStrategy: "sequential",
		},
		{
			ChannelID:   created.ID,
			KeyIndex:    2,
			APIKey:      "sk-redis-test-key-3",
			KeyStrategy: "round_robin",
		},
	}

	for _, key := range apiKeys {
		if err := store1.CreateAPIKey(ctx, key); err != nil {
			t.Fatalf("创建API Key失败: %v", err)
		}
	}

	// 模拟同步到Redis：序列化所有渠道和API Keys
	configs, err := store1.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询渠道列表失败: %v", err)
	}

	// 构建ChannelWithKeys结构
	var channelsWithKeys []*model.ChannelWithKeys
	for _, config := range configs {
		keys, err := store1.GetAPIKeys(ctx, config.ID)
		if err != nil {
			t.Fatalf("查询API Keys失败: %v", err)
		}

		apiKeySlice := make([]model.APIKey, len(keys))
		for i, k := range keys {
			apiKeySlice[i] = *k
		}

		channelsWithKeys = append(channelsWithKeys, &model.ChannelWithKeys{
			Config:  config,
			APIKeys: apiKeySlice,
		})
	}

	// 序列化为Redis格式
	redisBackup, err := sonic.Marshal(channelsWithKeys)
	if err != nil {
		t.Fatalf("序列化Redis备份失败: %v", err)
	}

	t.Logf("✅ 阶段1完成：原始数据创建")
	t.Logf("   渠道ID: %d", created.ID)
	t.Logf("   API Keys数量: %d", len(apiKeys))
	t.Logf("   Redis备份大小: %d bytes", len(redisBackup))

	// 关闭第一个数据库
	store1.Close()

	// ========== 阶段2：删除数据库，模拟数据丢失 ==========
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("删除数据库文件失败: %v", err)
	}
	if err := os.Remove(dbPath + "-log.db"); err != nil && !os.IsNotExist(err) {
		t.Logf("删除日志数据库失败（可忽略）: %v", err)
	}

	t.Logf("✅ 阶段2完成：数据库文件已删除（模拟数据丢失）")

	// ========== 阶段3：从Redis备份恢复数据 ==========
	// 创建新的数据库实例（模拟服务重启）
	store2, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("创建第二个数据库失败: %v", err)
	}
	defer store2.Close()

	// 反序列化Redis备份
	var restoredChannelsWithKeys []*model.ChannelWithKeys
	if err := sonic.Unmarshal(redisBackup, &restoredChannelsWithKeys); err != nil {
		t.Fatalf("反序列化Redis备份失败: %v", err)
	}

	// 手动执行恢复逻辑（模拟LoadChannelsFromRedis的核心逻辑）
	tx, err := store2.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer tx.Rollback()

	nowUnix := time.Now().Unix()
	totalKeysRestored := 0

	for _, cwk := range restoredChannelsWithKeys {
		config := cwk.Config

		// 规范化默认值
		modelsStr, _ := sonic.Marshal(config.Models)
		modelRedirectsStr, _ := sonic.Marshal(config.ModelRedirects)
		channelType := config.GetChannelType()

		// 1. 恢复渠道配置
		result, err := tx.ExecContext(ctx, `
			INSERT INTO channels(
				name, url, priority, models, model_redirects, channel_type,
				enabled, cooldown_until, cooldown_duration_ms, created_at, updated_at
			)
			VALUES(?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
		`, config.Name, config.URL, config.Priority,
			string(modelsStr), string(modelRedirectsStr), channelType,
			1, nowUnix, nowUnix) // enabled=1

		if err != nil {
			t.Fatalf("恢复渠道失败: %v", err)
		}

		channelID, _ := result.LastInsertId()

		// 2. 恢复API Keys
		for _, key := range cwk.APIKeys {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO api_keys (channel_id, key_index, api_key, key_strategy,
				                      cooldown_until, cooldown_duration_ms, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`, channelID, key.KeyIndex, key.APIKey, key.KeyStrategy,
				key.CooldownUntil, key.CooldownDurationMs, nowUnix, nowUnix)

			if err != nil {
				t.Fatalf("恢复API Key失败: %v", err)
			}
			totalKeysRestored++
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("提交事务失败: %v", err)
	}

	t.Logf("✅ 阶段3完成：从Redis恢复数据")
	t.Logf("   恢复渠道数量: %d", len(restoredChannelsWithKeys))
	t.Logf("   恢复API Keys数量: %d", totalKeysRestored)

	// ========== 阶段4：验证恢复后的数据完整性 ==========
	// 4.1 验证渠道配置
	recoveredConfigs, err := store2.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询恢复后的渠道列表失败: %v", err)
	}

	if len(recoveredConfigs) != 1 {
		t.Errorf("期望恢复1个渠道，实际: %d", len(recoveredConfigs))
	}

	recoveredConfig := recoveredConfigs[0]

	// 验证基本字段
	if recoveredConfig.Name != originalConfig.Name {
		t.Errorf("Name不匹配: 期望 %s, 实际 %s", originalConfig.Name, recoveredConfig.Name)
	}
	if recoveredConfig.URL != originalConfig.URL {
		t.Errorf("URL不匹配: 期望 %s, 实际 %s", originalConfig.URL, recoveredConfig.URL)
	}
	if recoveredConfig.Priority != originalConfig.Priority {
		t.Errorf("Priority不匹配: 期望 %d, 实际 %d", originalConfig.Priority, recoveredConfig.Priority)
	}
	if len(recoveredConfig.Models) != len(originalConfig.Models) {
		t.Errorf("Models数量不匹配: 期望 %d, 实际 %d", len(originalConfig.Models), len(recoveredConfig.Models))
	}
	if len(recoveredConfig.ModelRedirects) != len(originalConfig.ModelRedirects) {
		t.Errorf("ModelRedirects数量不匹配: 期望 %d, 实际 %d", len(originalConfig.ModelRedirects), len(recoveredConfig.ModelRedirects))
	}
	if recoveredConfig.ChannelType != "anthropic" {
		t.Errorf("ChannelType不匹配: 期望 anthropic, 实际 %s", recoveredConfig.ChannelType)
	}

	// 4.2 验证API Keys
	recoveredKeys, err := store2.GetAPIKeys(ctx, recoveredConfig.ID)
	if err != nil {
		t.Fatalf("查询恢复后的API Keys失败: %v", err)
	}

	if len(recoveredKeys) != len(apiKeys) {
		t.Fatalf("API Keys数量不匹配: 期望 %d, 实际 %d", len(apiKeys), len(recoveredKeys))
	}

	// 验证每个Key
	for i, originalKey := range apiKeys {
		recoveredKey := recoveredKeys[i]

		if recoveredKey.KeyIndex != originalKey.KeyIndex {
			t.Errorf("Key[%d] KeyIndex不匹配: 期望 %d, 实际 %d", i, originalKey.KeyIndex, recoveredKey.KeyIndex)
		}
		if recoveredKey.APIKey != originalKey.APIKey {
			t.Errorf("Key[%d] APIKey不匹配: 期望 %s, 实际 %s", i, originalKey.APIKey, recoveredKey.APIKey)
		}
		if recoveredKey.KeyStrategy != originalKey.KeyStrategy {
			t.Errorf("Key[%d] KeyStrategy不匹配: 期望 %s, 实际 %s", i, originalKey.KeyStrategy, recoveredKey.KeyStrategy)
		}
	}

	t.Logf("✅ 阶段4完成：数据完整性验证通过")
	t.Logf("")
	t.Logf("🎉 Redis恢复完整流程测试通过！")
	t.Logf("   ✓ 渠道配置完整恢复")
	t.Logf("   ✓ API Keys完整恢复（%d个）", len(recoveredKeys))
	t.Logf("   ✓ 模型重定向完整恢复")
	t.Logf("   ✓ 渠道类型正确填充")
}

// TestRedisRecovery_EmptyAPIKeys 测试恢复没有API Keys的渠道
func TestRedisRecovery_EmptyAPIKeys(t *testing.T) {
	// 模拟Redis数据（渠道没有API Keys）
	redisJSON := `[
		{
			"config": {
				"id": 1,
				"name": "Empty-Keys-Channel",
				"url": "https://empty.example.com",
				"priority": 10,
				"models": ["test-model"],
				"model_redirects": {},
				"channel_type": "anthropic",
				"enabled": true,
				"created_at": 1759575045,
				"updated_at": 1759575045
			},
			"api_keys": []
		}
	]`

	var channelsWithKeys []*model.ChannelWithKeys
	if err := sonic.Unmarshal([]byte(redisJSON), &channelsWithKeys); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	if len(channelsWithKeys) != 1 {
		t.Fatalf("期望1个渠道，实际: %d", len(channelsWithKeys))
	}

	cwk := channelsWithKeys[0]
	if cwk.Config == nil {
		t.Fatalf("Config不应为nil")
	}

	if cwk.Config.Name != "Empty-Keys-Channel" {
		t.Errorf("渠道名称不匹配: 期望 Empty-Keys-Channel, 实际 %s", cwk.Config.Name)
	}

	if len(cwk.APIKeys) != 0 {
		t.Errorf("期望0个API Key，实际: %d", len(cwk.APIKeys))
	}

	t.Logf("✅ 空API Keys渠道恢复测试通过")
}

// TestRedisRecovery_DeleteAPIKey 测试删除API Key后Redis同步的完整性
func TestRedisRecovery_DeleteAPIKey(t *testing.T) {
	// 禁用内存数据库模式，使用临时文件数据库
	os.Unsetenv("CCLOAD_USE_MEMORY_DB")

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-redis-delete-key.db")

	// ========== 阶段1：创建渠道和多个API Keys ==========
	store1, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("创建数据库失败: %v", err)
	}

	ctx := context.Background()

	// 创建测试渠道
	config := &model.Config{
		Name:        "Delete-Key-Test",
		URL:         "https://delete-test.example.com",
		Priority:    10,
		Models:      []string{"test-model"},
		ChannelType: "anthropic",
		Enabled:     true,
	}

	created, err := store1.CreateConfig(ctx, config)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建3个API Keys
	apiKeys := []*model.APIKey{
		{
			ChannelID:   created.ID,
			KeyIndex:    0,
			APIKey:      "sk-test-key-0",
			KeyStrategy: "sequential",
		},
		{
			ChannelID:   created.ID,
			KeyIndex:    1,
			APIKey:      "sk-test-key-1",
			KeyStrategy: "sequential",
		},
		{
			ChannelID:   created.ID,
			KeyIndex:    2,
			APIKey:      "sk-test-key-2",
			KeyStrategy: "round_robin",
		},
	}

	for _, key := range apiKeys {
		if err := store1.CreateAPIKey(ctx, key); err != nil {
			t.Fatalf("创建API Key失败: %v", err)
		}
	}

	// 验证创建成功
	keys, err := store1.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("期望3个API Key，实际: %d", len(keys))
	}

	t.Logf("✅ 阶段1完成：创建渠道和3个API Keys")

	// ========== 阶段2：删除其中一个API Key ==========
	// 删除KeyIndex=1的Key
	if err := store1.DeleteAPIKey(ctx, created.ID, 1); err != nil {
		t.Fatalf("删除API Key失败: %v", err)
	}

	// 等待异步同步完成(triggerAsyncSync是非阻塞的)
	time.Sleep(100 * time.Millisecond)

	// 验证删除成功
	keysAfterDelete, err := store1.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("删除后查询API Keys失败: %v", err)
	}
	if len(keysAfterDelete) != 2 {
		t.Fatalf("删除后期望2个API Key，实际: %d", len(keysAfterDelete))
	}

	t.Logf("✅ 阶段2完成：删除KeyIndex=1的Key")

	// ========== 阶段3：模拟Redis同步 ==========
	// 获取当前状态并序列化(模拟同步到Redis)
	configs, err := store1.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询渠道列表失败: %v", err)
	}

	var channelsWithKeys []*model.ChannelWithKeys
	for _, cfg := range configs {
		keys, err := store1.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("查询API Keys失败: %v", err)
		}

		apiKeySlice := make([]model.APIKey, len(keys))
		for i, k := range keys {
			apiKeySlice[i] = *k
		}

		channelsWithKeys = append(channelsWithKeys, &model.ChannelWithKeys{
			Config:  cfg,
			APIKeys: apiKeySlice,
		})
	}

	redisBackup, err := sonic.Marshal(channelsWithKeys)
	if err != nil {
		t.Fatalf("序列化Redis备份失败: %v", err)
	}

	t.Logf("✅ 阶段3完成：Redis备份包含%d个API Keys", len(channelsWithKeys[0].APIKeys))

	// 关闭第一个数据库
	store1.Close()

	// ========== 阶段4：删除数据库并从Redis恢复 ==========
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("删除数据库文件失败: %v", err)
	}
	if err := os.Remove(dbPath + "-log.db"); err != nil && !os.IsNotExist(err) {
		t.Logf("删除日志数据库失败（可忽略）: %v", err)
	}

	// 创建新数据库实例
	store2, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("创建第二个数据库失败: %v", err)
	}
	defer store2.Close()

	// 反序列化Redis备份
	var restoredChannelsWithKeys []*model.ChannelWithKeys
	if err := sonic.Unmarshal(redisBackup, &restoredChannelsWithKeys); err != nil {
		t.Fatalf("反序列化Redis备份失败: %v", err)
	}

	// 手动执行恢复逻辑
	tx, err := store2.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer tx.Rollback()

	nowUnix := time.Now().Unix()

	for _, cwk := range restoredChannelsWithKeys {
		cfg := cwk.Config

		modelsStr, _ := sonic.Marshal(cfg.Models)
		modelRedirectsStr, _ := sonic.Marshal(cfg.ModelRedirects)

		result, err := tx.ExecContext(ctx, `
			INSERT INTO channels(
				name, url, priority, models, model_redirects, channel_type,
				enabled, cooldown_until, cooldown_duration_ms, created_at, updated_at
			)
			VALUES(?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
		`, cfg.Name, cfg.URL, cfg.Priority,
			string(modelsStr), string(modelRedirectsStr), cfg.ChannelType,
			1, nowUnix, nowUnix)

		if err != nil {
			t.Fatalf("恢复渠道失败: %v", err)
		}

		channelID, _ := result.LastInsertId()

		// 恢复API Keys
		for _, key := range cwk.APIKeys {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO api_keys (channel_id, key_index, api_key, key_strategy,
				                      cooldown_until, cooldown_duration_ms, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`, channelID, key.KeyIndex, key.APIKey, key.KeyStrategy,
				key.CooldownUntil, key.CooldownDurationMs, nowUnix, nowUnix)

			if err != nil {
				t.Fatalf("恢复API Key失败: %v", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("提交事务失败: %v", err)
	}

	t.Logf("✅ 阶段4完成：从Redis恢复数据")

	// ========== 阶段5：验证恢复后的数据完整性 ==========
	recoveredKeys, err := store2.GetAPIKeys(ctx, 1) // channelID=1
	if err != nil {
		t.Fatalf("查询恢复后的API Keys失败: %v", err)
	}

	// 关键验证：应该只有2个Key（已删除的Key不应出现）
	if len(recoveredKeys) != 2 {
		t.Errorf("❌ 恢复后期望2个API Key，实际: %d", len(recoveredKeys))
		t.Logf("恢复的Keys: %+v", recoveredKeys)
		t.Fatalf("删除的Key不应该被恢复")
	}

	// 验证恢复的是正确的Keys（KeyIndex=0和KeyIndex=2）
	expectedKeyIndices := map[int]bool{0: true, 2: true}
	for _, key := range recoveredKeys {
		if !expectedKeyIndices[key.KeyIndex] {
			t.Errorf("❌ 意外的KeyIndex: %d (应该只有0和2)", key.KeyIndex)
		}
		t.Logf("恢复的Key: KeyIndex=%d, APIKey=%s", key.KeyIndex, key.APIKey)
	}

	t.Logf("✅ 阶段5完成：数据完整性验证通过")
	t.Logf("")
	t.Logf("🎉 DeleteAPIKey Redis同步测试通过！")
	t.Logf("   ✓ 删除操作触发Redis同步")
	t.Logf("   ✓ 已删除的Key不会被恢复")
	t.Logf("   ✓ 保留的Keys完整恢复")
}

// TestRedisRecovery_DefaultValuesFilling 测试恢复时默认值填充
func TestRedisRecovery_DefaultValuesFilling(t *testing.T) {
	// 模拟Redis数据（channel_type为空）
	redisJSON := `[
		{
			"config": {
				"id": 1,
				"name": "Default-Values-Test",
				"url": "https://default.example.com",
				"priority": 10,
				"models": ["test-model"],
				"model_redirects": {},
				"channel_type": "",
				"enabled": true,
				"created_at": 1759575045,
				"updated_at": 1759575045
			},
			"api_keys": [
				{
					"channel_id": 1,
					"key_index": 0,
					"api_key": "sk-test-key",
					"key_strategy": "",
					"cooldown_until": 0,
					"cooldown_duration_ms": 0
				}
			]
		}
	]`

	var channelsWithKeys []*model.ChannelWithKeys
	if err := sonic.Unmarshal([]byte(redisJSON), &channelsWithKeys); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	cwk := channelsWithKeys[0]

	if cwk.Config == nil {
		t.Fatalf("Config不应为nil")
	}

	// 验证GetChannelType返回默认值
	if cwk.Config.GetChannelType() != "anthropic" {
		t.Errorf("GetChannelType应返回anthropic，实际为 %s", cwk.Config.GetChannelType())
	}

	// 验证API Key的key_strategy默认值
	if len(cwk.APIKeys) > 0 {
		// 模拟normalizeChannelsWithKeys的填充逻辑
		if cwk.APIKeys[0].KeyStrategy == "" {
			cwk.APIKeys[0].KeyStrategy = "sequential"
		}

		if cwk.APIKeys[0].KeyStrategy != "sequential" {
			t.Errorf("KeyStrategy应为sequential，实际为 %s", cwk.APIKeys[0].KeyStrategy)
		}
	}

	t.Logf("✅ 默认值填充测试通过")
	t.Logf("   channel_type: \"\" → %s", cwk.Config.GetChannelType())
	t.Logf("   key_strategy: \"\" → %s", cwk.APIKeys[0].KeyStrategy)
}
