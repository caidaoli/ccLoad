package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bytedance/sonic"

	"ccLoad/internal/model"
	redisSync "ccLoad/internal/storage/redis"
)

// TestAPIKeyRedisSync_CreateAndUpdate 测试CreateAPIKey和UpdateAPIKey的Redis同步
func TestAPIKeyRedisSync_CreateAndUpdate(t *testing.T) {
	// 检查Redis环境变量
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		t.Skip("跳过测试：未配置REDIS_URL环境变量")
		return
	}

	// 创建Redis同步客户端
	rs, err := redisSync.NewRedisSync(redisURL)
	if err != nil {
		t.Skipf("跳过测试：Redis连接失败 (%v)", err)
		return
	}
	defer rs.Close()

	// 创建临时数据库
	tmpDir := t.TempDir()
	dbPath1 := filepath.Join(tmpDir, "test-apikey-sync-1.db")

	store1, err := NewSQLiteStore(dbPath1, rs)
	if err != nil {
		t.Fatalf("创建第一个数据库失败: %v", err)
	}
	defer store1.Close()

	ctx := context.Background()

	// === 阶段1：创建渠道（不含API Keys） ===
	config := &model.Config{
		Name:        "test-channel-apikey-sync",
		URL:         "https://api.example.com",
		Priority:    1,
		Models:      []string{"claude-3-5-sonnet-20241022"},
		ChannelType: "anthropic",
		Enabled:     true,
	}

	createdConfig, err := store1.CreateConfig(ctx, config)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	t.Logf("✅ 阶段1完成：创建渠道 (ID=%d)", createdConfig.ID)

	// 等待异步同步完成
	time.Sleep(100 * time.Millisecond)

	// === 阶段2：使用CreateAPIKey添加API Key ===
	apiKey1 := &model.APIKey{
		ChannelID:   createdConfig.ID,
		KeyIndex:    0,
		APIKey:      "sk-test-create-key-1",
		KeyStrategy: "sequential",
	}

	if err := store1.CreateAPIKey(ctx, apiKey1); err != nil {
		t.Fatalf("CreateAPIKey失败: %v", err)
	}

	t.Logf("✅ 阶段2完成：CreateAPIKey添加Key (KeyIndex=0)")

	// 等待异步同步完成
	time.Sleep(100 * time.Millisecond)

	// === 阶段3：验证Redis中包含新增的API Key ===
	// 直接从Redis读取数据验证
	redisData, err := rs.LoadChannelsWithKeysFromRedis(ctx)
	if err != nil {
		t.Fatalf("从Redis读取失败: %v", err)
	}

	if len(redisData) != 1 {
		t.Fatalf("期望Redis中有1个渠道，实际: %d", len(redisData))
	}

	if len(redisData[0].APIKeys) != 1 {
		t.Fatalf("期望Redis中有1个API Key，实际: %d", len(redisData[0].APIKeys))
	}

	if redisData[0].APIKeys[0].APIKey != "sk-test-create-key-1" {
		t.Errorf("API Key内容不匹配：期望 sk-test-create-key-1，实际 %s", redisData[0].APIKeys[0].APIKey)
	}

	t.Logf("✅ 阶段3完成：Redis正确同步CreateAPIKey操作")

	// === 阶段4：使用UpdateAPIKey修改API Key ===
	apiKey1.APIKey = "sk-test-updated-key-1"
	if err := store1.UpdateAPIKey(ctx, apiKey1); err != nil {
		t.Fatalf("UpdateAPIKey失败: %v", err)
	}

	t.Logf("✅ 阶段4完成：UpdateAPIKey修改Key")

	// 等待异步同步完成
	time.Sleep(100 * time.Millisecond)

	// === 阶段5：验证Redis中包含更新后的API Key ===
	updatedRedisData, err := rs.LoadChannelsWithKeysFromRedis(ctx)
	if err != nil {
		t.Fatalf("从Redis读取更新后数据失败: %v", err)
	}

	if len(updatedRedisData) != 1 || len(updatedRedisData[0].APIKeys) != 1 {
		t.Fatalf("Redis数据结构异常")
	}

	if updatedRedisData[0].APIKeys[0].APIKey != "sk-test-updated-key-1" {
		t.Errorf("API Key更新未同步：期望 sk-test-updated-key-1，实际 %s", updatedRedisData[0].APIKeys[0].APIKey)
	}

	t.Logf("✅ 阶段5完成：Redis正确同步UpdateAPIKey操作")

	// === 阶段6：验证从Redis恢复到新数据库 ===
	dbPath2 := filepath.Join(tmpDir, "test-apikey-sync-2.db")
	store2, err := NewSQLiteStore(dbPath2, rs)
	if err != nil {
		t.Fatalf("创建第二个store失败: %v", err)
	}
	defer store2.Close()

	// 手动恢复逻辑（模拟LoadChannelsFromRedis）
	tx, err := store2.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer tx.Rollback()

	nowUnix := time.Now().Unix()
	for _, cwk := range updatedRedisData {
		cfg := cwk.Config
		modelsStr, _ := sonic.Marshal(cfg.Models)
		modelRedirectsStr, _ := sonic.Marshal(cfg.ModelRedirects)

		result, err := tx.ExecContext(ctx, `
			INSERT INTO channels(name, url, priority, models, model_redirects, channel_type,
			                     enabled, cooldown_until, cooldown_duration_ms, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
		`, cfg.Name, cfg.URL, cfg.Priority, string(modelsStr), string(modelRedirectsStr),
			cfg.GetChannelType(), 1, nowUnix, nowUnix)

		if err != nil {
			t.Fatalf("恢复渠道失败: %v", err)
		}

		channelID, _ := result.LastInsertId()

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

	// 验证恢复的数据
	recoveredKeys, err := store2.GetAPIKeys(ctx, 1) // channelID=1
	if err != nil {
		t.Fatalf("查询恢复的API Keys失败: %v", err)
	}

	if len(recoveredKeys) != 1 {
		t.Fatalf("期望恢复1个API Key，实际恢复%d个", len(recoveredKeys))
	}

	if recoveredKeys[0].APIKey != "sk-test-updated-key-1" {
		t.Errorf("恢复的API Key不匹配：期望 sk-test-updated-key-1，实际 %s", recoveredKeys[0].APIKey)
	}

	t.Logf("✅ 阶段6完成：从Redis恢复数据验证通过")

	// === 最终验证 ===
	t.Log("")
	t.Log("🎉 CreateAPIKey和UpdateAPIKey Redis同步测试通过！")
	t.Log("   ✓ CreateAPIKey触发Redis同步")
	t.Log("   ✓ UpdateAPIKey触发Redis同步")
	t.Log("   ✓ 新增和更新的Key均可从Redis恢复")
}

// TestAPIKeyRedisSync_MultipleKeys 测试多个API Key的同步
func TestAPIKeyRedisSync_MultipleKeys(t *testing.T) {
	// 检查Redis环境变量
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		t.Skip("跳过测试：未配置REDIS_URL环境变量")
		return
	}

	// 创建Redis同步客户端
	rs, err := redisSync.NewRedisSync(redisURL)
	if err != nil {
		t.Skipf("跳过测试：Redis连接失败 (%v)", err)
		return
	}
	defer rs.Close()

	// 创建临时数据库
	tmpDir := t.TempDir()
	dbPath1 := filepath.Join(tmpDir, "test-multi-keys-1.db")

	store1, err := NewSQLiteStore(dbPath1, rs)
	if err != nil {
		t.Fatalf("创建第一个数据库失败: %v", err)
	}
	defer store1.Close()

	ctx := context.Background()

	// 创建渠道
	config := &model.Config{
		Name:        "test-multi-keys-sync",
		URL:         "https://api.example.com",
		Priority:    1,
		Models:      []string{"claude-3-5-sonnet-20241022"},
		ChannelType: "anthropic",
		Enabled:     true,
	}

	createdConfig, err := store1.CreateConfig(ctx, config)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 连续添加3个API Key
	for i := 0; i < 3; i++ {
		apiKey := &model.APIKey{
			ChannelID:   createdConfig.ID,
			KeyIndex:    i,
			APIKey:      "sk-test-multi-" + string(rune('A'+i)),
			KeyStrategy: "round_robin",
		}

		if err := store1.CreateAPIKey(ctx, apiKey); err != nil {
			t.Fatalf("CreateAPIKey[%d]失败: %v", i, err)
		}
	}

	t.Logf("✅ 创建3个API Keys完成")

	// 等待异步同步
	time.Sleep(150 * time.Millisecond)

	// 从Redis读取验证
	redisData, err := rs.LoadChannelsWithKeysFromRedis(ctx)
	if err != nil {
		t.Fatalf("从Redis读取失败: %v", err)
	}

	if len(redisData) != 1 {
		t.Fatalf("期望Redis中有1个渠道，实际: %d", len(redisData))
	}

	if len(redisData[0].APIKeys) != 3 {
		t.Fatalf("期望Redis中有3个API Keys，实际: %d", len(redisData[0].APIKeys))
	}

	// 验证每个Key的内容
	for i, key := range redisData[0].APIKeys {
		expectedKey := "sk-test-multi-" + string(rune('A'+i))
		if key.APIKey != expectedKey {
			t.Errorf("Key[%d]不匹配：期望 %s，实际 %s", i, expectedKey, key.APIKey)
		}
		if key.KeyStrategy != "round_robin" {
			t.Errorf("Key[%d]策略不匹配：期望 round_robin，实际 %s", i, key.KeyStrategy)
		}
	}

	t.Log("🎉 多Key同步测试通过！")
}
