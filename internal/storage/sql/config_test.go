package sql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestConfig_CreateAndGet(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "config.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	// 创建渠道
	cfg := &model.Config{
		Name:        "test-channel",
		URL:         "https://api.openai.com",
		Priority:    10,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4"},
			{Model: "gpt-3.5-turbo"},
		},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if created.ID == 0 {
		t.Error("expected non-zero ID")
	}

	// 获取渠道
	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if got.Name != "test-channel" {
		t.Errorf("name: got %q, want %q", got.Name, "test-channel")
	}
	if got.URL != "https://api.openai.com" {
		t.Errorf("url: got %q, want %q", got.URL, "https://api.openai.com")
	}
	if got.Priority != 10 {
		t.Errorf("priority: got %d, want %d", got.Priority, 10)
	}
	if !got.Enabled {
		t.Error("expected enabled=true")
	}
	if got.ChannelType != "openai" {
		t.Errorf("channel_type: got %q, want %q", got.ChannelType, "openai")
	}
	if len(got.ModelEntries) != 2 {
		t.Errorf("model entries count: got %d, want 2", len(got.ModelEntries))
	}

	// 获取不存在的渠道
	_, err = store.GetConfig(ctx, 99999)
	if err == nil {
		t.Error("expected error for non-existent config")
	}
}

func TestConfig_ListConfigs(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "list.db")

	ctx := context.Background()

	// 创建多个渠道
	for i := 1; i <= 3; i++ {
		cfg := &model.Config{
			Name:     fmt.Sprintf("channel-%c", rune('A'+i-1)),
			URL:      "https://api.example.com",
			Priority: i * 10,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: fmt.Sprintf("model-%c", rune('a'+i-1))},
			},
		}
		if _, err := store.CreateConfig(ctx, cfg); err != nil {
			t.Fatalf("create config %d: %v", i, err)
		}
	}

	// 列出所有渠道
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	if len(configs) != 3 {
		t.Errorf("expected 3 configs, got %d", len(configs))
	}

	// 验证按优先级降序排列
	for i := 1; i < len(configs); i++ {
		if configs[i-1].Priority < configs[i].Priority {
			t.Errorf("configs not sorted by priority DESC: %d < %d",
				configs[i-1].Priority, configs[i].Priority)
		}
	}
}

func TestConfig_UpdateConfig(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "update.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	// 创建渠道
	cfg := &model.Config{
		Name:     "original-name",
		URL:      "https://old.api.com",
		Priority: 1,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "old-model"},
		},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	// 更新渠道
	created.Name = "updated-name"
	created.URL = "https://new.api.com"
	created.Priority = 100
	created.Enabled = false
	created.ModelEntries = []model.ModelEntry{
		{Model: "new-model-1"},
		{Model: "new-model-2"},
	}

	if _, err := store.UpdateConfig(ctx, created.ID, created); err != nil {
		t.Fatalf("update config: %v", err)
	}

	// 验证更新
	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("get config after update: %v", err)
	}
	if got.Name != "updated-name" {
		t.Errorf("name: got %q, want %q", got.Name, "updated-name")
	}
	if got.URL != "https://new.api.com" {
		t.Errorf("url: got %q, want %q", got.URL, "https://new.api.com")
	}
	if got.Priority != 100 {
		t.Errorf("priority: got %d, want %d", got.Priority, 100)
	}
	if got.Enabled {
		t.Error("expected enabled=false")
	}
	if len(got.ModelEntries) != 2 {
		t.Errorf("model entries count: got %d, want 2", len(got.ModelEntries))
	}
}

func TestConfig_DeleteConfig(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "delete.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	// 创建渠道
	cfg := &model.Config{
		Name:     "to-delete",
		URL:      "https://api.example.com",
		Priority: 1,
		Enabled:  true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	// 删除渠道
	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	// 验证已删除
	_, err = store.GetConfig(ctx, created.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestConfig_GetEnabledChannelsByModel(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "model_query.db")

	ctx := context.Background()

	// 创建启用的渠道支持 gpt-4
	cfg1 := &model.Config{
		Name:     "gpt4-channel",
		URL:      "https://api.openai.com",
		Priority: 10,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4"},
			{Model: "gpt-4-turbo"},
		},
	}
	created1, err := store.CreateConfig(ctx, cfg1)
	if err != nil {
		t.Fatalf("create config 1: %v", err)
	}

	// 创建启用的渠道支持 claude
	cfg2 := &model.Config{
		Name:     "claude-channel",
		URL:      "https://api.anthropic.com",
		Priority: 20,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "claude-3-opus"},
		},
	}
	created2, err := store.CreateConfig(ctx, cfg2)
	if err != nil {
		t.Fatalf("create config 2: %v", err)
	}

	// 创建禁用的渠道支持 gpt-4
	cfg3 := &model.Config{
		Name:     "disabled-channel",
		URL:      "https://api.disabled.com",
		Priority: 30,
		Enabled:  false,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4"},
		},
	}
	if _, err := store.CreateConfig(ctx, cfg3); err != nil {
		t.Fatalf("create config 3: %v", err)
	}

	// 为渠道添加 API Key（需要至少有一个 key 才能被选中）
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created1.ID, KeyIndex: 0, APIKey: "sk-key1"},
	}); err != nil {
		t.Fatalf("create api keys batch for channel 1: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created2.ID, KeyIndex: 0, APIKey: "sk-key2"},
	}); err != nil {
		t.Fatalf("create api keys batch for channel 2: %v", err)
	}

	// 查询支持 gpt-4 的启用渠道
	configs, err := store.GetEnabledChannelsByModel(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("get enabled channels by model: %v", err)
	}
	if len(configs) != 1 {
		t.Errorf("expected 1 enabled channel with gpt-4, got %d", len(configs))
	}
	if len(configs) > 0 && configs[0].Name != "gpt4-channel" {
		t.Errorf("expected gpt4-channel, got %s", configs[0].Name)
	}

	// 通配符查询所有启用渠道
	allConfigs, err := store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("get all enabled channels: %v", err)
	}
	if len(allConfigs) != 2 {
		t.Errorf("expected 2 enabled channels, got %d", len(allConfigs))
	}
}

func TestConfig_GetEnabledChannelsByType(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "type_query.db")

	ctx := context.Background()

	// 创建 openai 类型渠道
	cfg1 := &model.Config{
		Name:        "openai-channel",
		URL:         "https://api.openai.com",
		Priority:    10,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4"},
		},
	}
	created1, err := store.CreateConfig(ctx, cfg1)
	if err != nil {
		t.Fatalf("create openai config: %v", err)
	}

	// 创建 anthropic 类型渠道
	cfg2 := &model.Config{
		Name:        "anthropic-channel",
		URL:         "https://api.anthropic.com",
		Priority:    20,
		Enabled:     true,
		ChannelType: "anthropic",
		ModelEntries: []model.ModelEntry{
			{Model: "claude-3"},
		},
	}
	created2, err := store.CreateConfig(ctx, cfg2)
	if err != nil {
		t.Fatalf("create anthropic config: %v", err)
	}

	// 添加 API Key
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created1.ID, KeyIndex: 0, APIKey: "sk-openai"},
	})
	_ = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created2.ID, KeyIndex: 0, APIKey: "sk-anthropic"},
	})

	// 按类型查询
	openaiChannels, err := store.GetEnabledChannelsByType(ctx, "openai")
	if err != nil {
		t.Fatalf("get openai channels: %v", err)
	}
	if len(openaiChannels) != 1 {
		t.Errorf("expected 1 openai channel, got %d", len(openaiChannels))
	}

	anthropicChannels, err := store.GetEnabledChannelsByType(ctx, "anthropic")
	if err != nil {
		t.Fatalf("get anthropic channels: %v", err)
	}
	if len(anthropicChannels) != 1 {
		t.Errorf("expected 1 anthropic channel, got %d", len(anthropicChannels))
	}
}

func TestConfig_BatchUpdatePriority(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "priority.db")

	ctx := context.Background()

	// 创建多个渠道
	var ids []int64
	for i := 1; i <= 3; i++ {
		cfg := &model.Config{
			Name:     "channel-" + string(rune('A'+i-1)),
			URL:      "https://api.example.com",
			Priority: i,
			Enabled:  true,
		}
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("create config %d: %v", i, err)
		}
		ids = append(ids, created.ID)
	}

	// 批量更新优先级
	updates := []struct {
		ID       int64
		Priority int
	}{
		{ids[0], 100},
		{ids[1], 200},
		{ids[2], 300},
	}
	if _, err := store.BatchUpdatePriority(ctx, updates); err != nil {
		t.Fatalf("batch update priority: %v", err)
	}

	// 验证更新
	for i, id := range ids {
		got, err := store.GetConfig(ctx, id)
		if err != nil {
			t.Fatalf("get config %d: %v", i, err)
		}
		expected := (i + 1) * 100
		if got.Priority != expected {
			t.Errorf("config %d priority: got %d, want %d", i, got.Priority, expected)
		}
	}
}

func TestConfig_ModelRedirect(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "redirect.db")

	ctx := context.Background()

	// 创建带模型重定向的渠道
	cfg := &model.Config{
		Name:        "redirect-channel",
		URL:         "https://api.example.com",
		Priority:    10,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4", RedirectModel: "gpt-4-turbo"},
			{Model: "gpt-3.5-turbo"},
		},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	// 验证模型重定向被保存
	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}

	var foundRedirect bool
	for _, entry := range got.ModelEntries {
		if entry.Model == "gpt-4" && entry.RedirectModel == "gpt-4-turbo" {
			foundRedirect = true
			break
		}
	}
	if !foundRedirect {
		t.Error("expected to find gpt-4 -> gpt-4-turbo redirect")
	}
}
