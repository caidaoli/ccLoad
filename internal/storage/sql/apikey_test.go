package sql_test

import (
	"context"
	"testing"

	"ccLoad/internal/model"
)

func TestAPIKey_CreateAndGet(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "apikey.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "apikey-test-channel")

	// 批量创建 API Keys
	keys := []*model.APIKey{
		{ChannelID: channelID, KeyIndex: 0, APIKey: "sk-key-0", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID, KeyIndex: 1, APIKey: "sk-key-1", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID, KeyIndex: 2, APIKey: "sk-key-2", KeyStrategy: model.KeyStrategySequential},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
		t.Fatalf("create api keys batch: %v", err)
	}

	// 获取单个 API Key
	key, err := store.GetAPIKey(ctx, channelID, 1)
	if err != nil {
		t.Fatalf("get api key: %v", err)
	}
	if key.APIKey != "sk-key-1" {
		t.Errorf("api key: got %q, want %q", key.APIKey, "sk-key-1")
	}
	if key.KeyIndex != 1 {
		t.Errorf("key index: got %d, want %d", key.KeyIndex, 1)
	}

	// 获取渠道所有 API Keys
	allKeys, err := store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("get api keys: %v", err)
	}
	if len(allKeys) != 3 {
		t.Errorf("expected 3 keys, got %d", len(allKeys))
	}
}

func TestAPIKey_UpdateStrategy(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "strategy.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "strategy-test-channel")

	// 创建 API Key
	keys := []*model.APIKey{
		{ChannelID: channelID, KeyIndex: 0, APIKey: "sk-key", KeyStrategy: model.KeyStrategySequential},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
		t.Fatalf("create api keys batch: %v", err)
	}

	// 更新策略
	if err := store.UpdateAPIKeysStrategy(ctx, channelID, model.KeyStrategyRoundRobin); err != nil {
		t.Fatalf("update api keys strategy: %v", err)
	}

	// 验证更新
	key, err := store.GetAPIKey(ctx, channelID, 0)
	if err != nil {
		t.Fatalf("get api key: %v", err)
	}
	if key.KeyStrategy != model.KeyStrategyRoundRobin {
		t.Errorf("strategy: got %q, want %q", key.KeyStrategy, model.KeyStrategyRoundRobin)
	}
}

func TestAPIKey_Delete(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "delete.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "delete-test-channel")

	// 创建多个 API Keys
	keys := []*model.APIKey{
		{ChannelID: channelID, KeyIndex: 0, APIKey: "sk-key-0", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID, KeyIndex: 1, APIKey: "sk-key-1", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID, KeyIndex: 2, APIKey: "sk-key-2", KeyStrategy: model.KeyStrategySequential},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
		t.Fatalf("create api keys batch: %v", err)
	}

	// 删除中间的 key
	if err := store.DeleteAPIKey(ctx, channelID, 1); err != nil {
		t.Fatalf("delete api key: %v", err)
	}

	// 验证删除
	allKeys, err := store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("get api keys: %v", err)
	}
	if len(allKeys) != 2 {
		t.Errorf("expected 2 keys after delete, got %d", len(allKeys))
	}
}

func TestAPIKey_CompactIndices(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "compact.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "compact-test-channel")

	// 创建 3 个 API Keys: indices 0, 1, 2
	keys := []*model.APIKey{
		{ChannelID: channelID, KeyIndex: 0, APIKey: "sk-key-0", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID, KeyIndex: 1, APIKey: "sk-key-1", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID, KeyIndex: 2, APIKey: "sk-key-2", KeyStrategy: model.KeyStrategySequential},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
		t.Fatalf("create api keys batch: %v", err)
	}

	// 删除 index=1 的 key
	if err := store.DeleteAPIKey(ctx, channelID, 1); err != nil {
		t.Fatalf("delete api key: %v", err)
	}

	// 压缩索引：将 index=2 移动到 index=1
	if err := store.CompactKeyIndices(ctx, channelID, 1); err != nil {
		t.Fatalf("compact key indices: %v", err)
	}

	// 验证压缩结果
	allKeys, err := store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("get api keys: %v", err)
	}
	if len(allKeys) != 2 {
		t.Errorf("expected 2 keys, got %d", len(allKeys))
	}

	// 检查索引是连续的
	for i, key := range allKeys {
		if key.KeyIndex != i {
			t.Errorf("key %d: expected index %d, got %d", i, i, key.KeyIndex)
		}
	}
}

func TestAPIKey_DeleteAll(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "delete_all.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "delete-all-test-channel")

	// 创建多个 API Keys
	keys := []*model.APIKey{
		{ChannelID: channelID, KeyIndex: 0, APIKey: "sk-key-0", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID, KeyIndex: 1, APIKey: "sk-key-1", KeyStrategy: model.KeyStrategySequential},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
		t.Fatalf("create api keys batch: %v", err)
	}

	// 删除所有
	if err := store.DeleteAllAPIKeys(ctx, channelID); err != nil {
		t.Fatalf("delete all api keys: %v", err)
	}

	// 验证全部删除
	allKeys, err := store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("get api keys: %v", err)
	}
	if len(allKeys) != 0 {
		t.Errorf("expected 0 keys, got %d", len(allKeys))
	}
}

func TestAPIKey_GetAllAPIKeys(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "get_all.db")

	ctx := context.Background()

	// 创建两个渠道
	channelID1 := createTestChannel(t, ctx, store, "channel-1")
	channelID2 := createTestChannel(t, ctx, store, "channel-2")

	// 为每个渠道创建 API Keys
	keys1 := []*model.APIKey{
		{ChannelID: channelID1, KeyIndex: 0, APIKey: "sk-ch1-key-0", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID1, KeyIndex: 1, APIKey: "sk-ch1-key-1", KeyStrategy: model.KeyStrategySequential},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys1); err != nil {
		t.Fatalf("create api keys for channel 1: %v", err)
	}

	keys2 := []*model.APIKey{
		{ChannelID: channelID2, KeyIndex: 0, APIKey: "sk-ch2-key-0", KeyStrategy: model.KeyStrategyRoundRobin},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys2); err != nil {
		t.Fatalf("create api keys for channel 2: %v", err)
	}

	// 获取所有 API Keys（返回 map[channelID][]*APIKey）
	allKeys, err := store.GetAllAPIKeys(ctx)
	if err != nil {
		t.Fatalf("get all api keys: %v", err)
	}
	if countAPIKeys(allKeys) != 3 {
		t.Errorf("expected 3 total keys, got %d", countAPIKeys(allKeys))
	}
}

func TestAPIKey_ImportChannelBatch(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "import.db")

	ctx := context.Background()

	// 批量导入渠道（包含渠道配置和 API Keys）
	channels := []*model.ChannelWithKeys{
		{
			Config: &model.Config{
				Name:        "imported-channel-1",
				URL:         "https://api1.example.com",
				Priority:    10,
				Enabled:     true,
				ChannelType: "openai",
				ModelEntries: []model.ModelEntry{
					{Model: "gpt-4"},
					{Model: "gpt-3.5-turbo"},
				},
			},
			APIKeys: []model.APIKey{
				{KeyIndex: 0, APIKey: "sk-import-key-1", KeyStrategy: model.KeyStrategySequential},
				{KeyIndex: 1, APIKey: "sk-import-key-2", KeyStrategy: model.KeyStrategySequential},
			},
		},
		{
			Config: &model.Config{
				Name:        "imported-channel-2",
				URL:         "https://api2.example.com",
				Priority:    20,
				Enabled:     true,
				ChannelType: "anthropic",
				ModelEntries: []model.ModelEntry{
					{Model: "claude-3"},
				},
			},
			APIKeys: []model.APIKey{
				{KeyIndex: 0, APIKey: "sk-anthropic-key", KeyStrategy: model.KeyStrategyRoundRobin},
			},
		},
	}

	created, updated, err := store.ImportChannelBatch(ctx, channels)
	if err != nil {
		t.Fatalf("import channel batch: %v", err)
	}
	if created != 2 {
		t.Errorf("expected 2 created, got %d", created)
	}
	if updated != 0 {
		t.Errorf("expected 0 updated, got %d", updated)
	}

	// 验证导入结果
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	if len(configs) != 2 {
		t.Errorf("expected 2 channels, got %d", len(configs))
	}

	// 验证 API Keys 也被导入（渠道1=2个，渠道2=1个）
	allKeys, err := store.GetAllAPIKeys(ctx)
	if err != nil {
		t.Fatalf("get all api keys: %v", err)
	}
	if len(allKeys) != 2 {
		t.Fatalf("expected keys for 2 channels, got %d", len(allKeys))
	}
	if countAPIKeys(allKeys) != 3 {
		t.Fatalf("expected 3 api keys total, got %d", countAPIKeys(allKeys))
	}

	idsByName := make(map[string]int64, len(configs))
	for _, cfg := range configs {
		idsByName[cfg.Name] = cfg.ID
	}
	id1 := idsByName["imported-channel-1"]
	id2 := idsByName["imported-channel-2"]
	if id1 == 0 || id2 == 0 {
		t.Fatalf("expected imported channels to have non-zero ids, got id1=%d id2=%d", id1, id2)
	}

	keys1, err := store.GetAPIKeys(ctx, id1)
	if err != nil {
		t.Fatalf("get api keys for imported-channel-1: %v", err)
	}
	if len(keys1) != 2 {
		t.Fatalf("expected 2 keys for imported-channel-1, got %d", len(keys1))
	}
	keys2, err := store.GetAPIKeys(ctx, id2)
	if err != nil {
		t.Fatalf("get api keys for imported-channel-2: %v", err)
	}
	if len(keys2) != 1 {
		t.Fatalf("expected 1 key for imported-channel-2, got %d", len(keys2))
	}
}
