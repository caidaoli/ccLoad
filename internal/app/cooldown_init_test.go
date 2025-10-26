package app

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
)

// TestInitCooldownState 初始化时应加载现有冷却状态
func TestInitCooldownState(t *testing.T) {
	store, err := sqlite.NewSQLiteStoreForTest(t.TempDir()+"/t.db", nil)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	srv := NewServer(store)

	// 创建一个渠道并设置冷却
	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, dummyConfig())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	until := time.Now().Add(2 * time.Minute)
	_ = store.SetChannelCooldown(ctx, cfg.ID, until)
	key := dummyAPIKey(cfg.ID, 0)
	_ = store.CreateAPIKey(ctx, &key)
	_ = store.SetKeyCooldown(ctx, cfg.ID, 0, until)

	// 重新初始化冷却集合（模拟服务启动后异步加载）
	srv.initCooldownState(ctx)

	m := srv.GetMetrics()
	if m.ChannelCooldowns < 1 || m.KeyCooldowns < 1 {
		t.Fatalf("cooldown gauges not initialized, got ch=%d key=%d", m.ChannelCooldowns, m.KeyCooldowns)
	}
}

// dummyConfig 提供最小可用的渠道配置
func dummyConfig() *model.Config {
	return &model.Config{
		Name:        "cool-init",
		URL:         "https://api.example.com",
		Priority:    1,
		Models:      []string{"m"},
		ChannelType: "anthropic",
		Enabled:     true,
	}
}

// dummyAPIKey 提供基本 API Key 对象
func dummyAPIKey(channelID int64, idx int) model.APIKey {
	return model.APIKey{
		ChannelID: channelID,
		KeyIndex:  idx,
		APIKey:    "sk-test",
		CreatedAt: model.JSONTime{Time: time.Now()},
		UpdatedAt: model.JSONTime{Time: time.Now()},
	}
}
