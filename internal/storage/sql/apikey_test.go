package sql_test

import (
	"context"
	"strings"
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
		{ChannelID: channelID, KeyIndex: 1, APIKey: "sk-key-1", AllowedModels: []string{"gpt-5", "gpt-4.1"}, KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID, KeyIndex: 2, APIKey: "sk-key-2", ModelScopeEmpty: true, Disabled: true, KeyStrategy: model.KeyStrategySequential},
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
	if len(key.AllowedModels) != 2 || key.AllowedModels[0] != "gpt-5" || key.AllowedModels[1] != "gpt-4.1" {
		t.Errorf("allowed models: got %v, want [gpt-5 gpt-4.1]", key.AllowedModels)
	}

	// 获取渠道所有 API Keys
	allKeys, err := store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("get api keys: %v", err)
	}
	if len(allKeys) != 3 {
		t.Errorf("expected 3 keys, got %d", len(allKeys))
	}
	if !allKeys[2].ModelScopeEmpty || allKeys[2].AllowsModel("gpt-5") {
		t.Fatalf("empty model scope did not persist: %+v", allKeys[2])
	}
}

func TestAPIKey_UpdateAllowedModelsPreservesRuntimeState(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "allowed_models.db")
	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "allowed-models-channel")

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: channelID, KeyIndex: 0, APIKey: "sk-key", Note: "primary",
		KeyStrategy: model.KeyStrategyRoundRobin, Disabled: true,
		CooldownUntil: 1234, CooldownDurationMs: 5678,
	}}); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	if err := store.UpdateAPIKeyModelScopes(ctx, channelID, map[int]model.APIKeyModelScope{
		0: {AllowedModels: []string{"gpt-5", "gpt-4.1"}, Disabled: true},
	}); err != nil {
		t.Fatalf("update allowed models: %v", err)
	}

	got, err := store.GetAPIKey(ctx, channelID, 0)
	if err != nil {
		t.Fatalf("get api key: %v", err)
	}
	if len(got.AllowedModels) != 2 || got.AllowedModels[0] != "gpt-5" || got.AllowedModels[1] != "gpt-4.1" {
		t.Fatalf("allowed models = %v, want [gpt-5 gpt-4.1]", got.AllowedModels)
	}
	if got.Note != "primary" || got.KeyStrategy != model.KeyStrategyRoundRobin || !got.Disabled ||
		got.CooldownUntil != 1234 || got.CooldownDurationMs != 5678 {
		t.Fatalf("runtime state changed after metadata update: %+v", got)
	}

	if err := store.UpdateAPIKeyModelScopes(ctx, channelID, map[int]model.APIKeyModelScope{0: {Disabled: true}}); err != nil {
		t.Fatalf("clear allowed models: %v", err)
	}
	got, err = store.GetAPIKey(ctx, channelID, 0)
	if err != nil {
		t.Fatalf("get unrestricted api key: %v", err)
	}
	if len(got.AllowedModels) != 0 {
		t.Fatalf("allowed models after clear = %v, want unrestricted", got.AllowedModels)
	}
	if got.ModelScopeEmpty {
		t.Fatal("explicit unrestricted scope remained deny-all")
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

func TestAPIKey_NotesPersistAndUpdate(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "notes.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "notes-test-channel")

	keys := []*model.APIKey{
		{ChannelID: channelID, KeyIndex: 0, APIKey: "sk-key-0", Note: "primary", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: channelID, KeyIndex: 1, APIKey: "sk-key-1", Note: "backup", KeyStrategy: model.KeyStrategySequential},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
		t.Fatalf("create api keys batch: %v", err)
	}

	got, err := store.GetAPIKey(ctx, channelID, 1)
	if err != nil {
		t.Fatalf("get api key: %v", err)
	}
	if got.Note != "backup" {
		t.Fatalf("note after create = %q, want %q", got.Note, "backup")
	}

	if err := store.UpdateAPIKeyNotes(ctx, channelID, map[int]string{0: "primary-updated", 1: ""}); err != nil {
		t.Fatalf("update api key notes: %v", err)
	}

	allKeys, err := store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("get api keys: %v", err)
	}
	if allKeys[0].Note != "primary-updated" || allKeys[1].Note != "" {
		t.Fatalf("notes after update = [%q, %q], want [%q, %q]",
			allKeys[0].Note, allKeys[1].Note, "primary-updated", "")
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
				Name:                  "imported-channel-1",
				URLs:                  model.ChannelURLs{{URL: "https://api1.example.com"}},
				Priority:              10,
				Enabled:               true,
				ScheduledCheckEnabled: true,
				CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "Imported cooldown", Priority: 0, StatusCodes: []int{429},
					Scope: model.CooldownScopeKey, Mode: model.CooldownModeFixed, CooldownSeconds: 90,
				}}},
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
				Name:                  "imported-channel-2",
				URLs:                  model.ChannelURLs{{URL: "https://api2.example.com"}},
				Priority:              20,
				Enabled:               true,
				ScheduledCheckEnabled: false,
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

	configsByName := make(map[string]*model.Config, len(configs))
	for _, cfg := range configs {
		configsByName[cfg.Name] = cfg
	}
	if !configsByName["imported-channel-1"].ScheduledCheckEnabled {
		t.Fatalf("expected imported-channel-1 scheduled check enabled")
	}
	importedRules := configsByName["imported-channel-1"].CooldownDetectionRules
	if importedRules == nil || len(importedRules.Rules) != 1 || importedRules.Rules[0].Name != "Imported cooldown" {
		t.Fatalf("expected imported cooldown detection rules, got %#v", importedRules)
	}
	if configsByName["imported-channel-2"].ScheduledCheckEnabled {
		t.Fatalf("expected imported-channel-2 scheduled check disabled")
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

func TestAPIKey_ImportChannelBatchCannotReplaceCodexAuthentication(t *testing.T) {
	store := newTestStore(t, "import-codex-auth-immutable.db")
	ctx := context.Background()
	credential := `{"type":"codex","access_token":"at-secret","refresh_token":"rt-secret","expired":"2030-01-01T00:00:00Z"}`
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "codex-immutable", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
		URLs:    model.ChannelURLs{{URL: "https://chatgpt.com/backend-api/codex/responses", Exact: true, Protocols: []string{"codex"}}},
		Enabled: true, Websockets: true, ModelEntries: []model.ModelEntry{{Model: "*"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig() error = %v", err)
	}

	_, _, err = store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{{
		Config: &model.Config{
			Name: created.Name, AuthType: model.AuthTypeAPIKey,
			URLs: model.ChannelURLs{{URL: "https://api.example.com"}}, Enabled: true,
			ModelEntries: []model.ModelEntry{{Model: "gpt-test"}},
		},
		APIKeys: []model.APIKey{{KeyIndex: 0, APIKey: "must-not-replace-oauth"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "auth_type cannot be changed") {
		t.Fatalf("ImportChannelBatch() error = %v, want immutable auth_type rejection", err)
	}

	persisted, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if !persisted.UsesCodexOAuth() || persisted.OAuthCredential != credential {
		t.Fatalf("Codex channel changed after rejected import: %#v", persisted)
	}
	keys, err := store.GetAPIKeys(ctx, created.ID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("Codex keys after rejected import = (%#v, %v)", keys, err)
	}
}

func TestAPIKey_ImportChannelBatchUpdatesExistingOAuthChannel(t *testing.T) {
	store := newTestStore(t, "import-update-oauth-channel.db")
	ctx := context.Background()
	original := `{"type":"codex","access_token":"at-original","refresh_token":"rt-original","expired":"2031-01-01T00:00:00Z"}`
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "codex-import-update", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: original,
		URLs:    model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "gpt-old"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials := []string{
		`{"type":"codex","access_token":"at-by-name","refresh_token":"rt-by-name","expired":"2032-01-01T00:00:00Z"}`,
		`{"type":"codex","access_token":"at-by-id","refresh_token":"rt-by-id","expired":"2033-01-01T00:00:00Z"}`,
	}
	for index, imported := range []*model.Config{
		{
			Name: created.Name, AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credentials[0],
			URLs:    model.ChannelURLs{{URL: "https://by-name.example.com", Protocols: []string{"codex"}}},
			Enabled: true, ModelEntries: []model.ModelEntry{{Model: "gpt-by-name"}},
		},
		{
			ID: created.ID, Name: created.Name, AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credentials[1],
			URLs:    model.ChannelURLs{{URL: "https://by-id.example.com", Protocols: []string{"codex"}}},
			Enabled: true, ModelEntries: []model.ModelEntry{{Model: "gpt-by-id"}},
		},
	} {
		createdCount, updatedCount, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{{Config: imported}})
		if err != nil {
			t.Fatalf("ImportChannelBatch() error = %v", err)
		}
		if createdCount != 0 || updatedCount != 1 {
			t.Fatalf("ImportChannelBatch() counts = (%d, %d), want (0, 1)", createdCount, updatedCount)
		}
		persisted, err := store.GetConfig(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.OAuthCredential != credentials[index] {
			t.Fatalf("OAuth credential was not updated")
		}
		wantModel := []string{"gpt-by-name", "gpt-by-id"}[index]
		if !persisted.SupportsModel(wantModel) {
			t.Fatalf("OAuth models = %v, want %s", persisted.GetModels(), wantModel)
		}
		if got := persisted.GetURLs()[0]; got != imported.GetURLs()[0] {
			t.Fatalf("OAuth URL = %q, want %q", got, imported.GetURLs()[0])
		}
	}
}

func TestAPIKey_ImportChannelBatchPreservesScheduledCheckWithExplicitID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "import-preserve-id.db")
	ctx := context.Background()

	channel := &model.ChannelWithKeys{
		Config: &model.Config{
			ID:                    42,
			Name:                  "imported-channel-explicit-id",
			URLs:                  model.ChannelURLs{{URL: "https://api.example.com"}},
			Priority:              5,
			Enabled:               true,
			Websockets:            true,
			ScheduledCheckEnabled: true,
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-4o-mini"},
			},
		},
		APIKeys: []model.APIKey{
			{KeyIndex: 0, APIKey: "sk-import-key-explicit", KeyStrategy: model.KeyStrategySequential},
		},
	}

	created, updated, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{channel})
	if err != nil {
		t.Fatalf("import channel batch with explicit id: %v", err)
	}
	if created != 1 || updated != 0 {
		t.Fatalf("expected created=1 updated=0, got created=%d updated=%d", created, updated)
	}

	config, err := store.GetConfig(ctx, channel.Config.ID)
	if err != nil {
		t.Fatalf("get imported config: %v", err)
	}
	if !config.ScheduledCheckEnabled {
		t.Fatalf("expected scheduled_check_enabled to persist for explicit id import")
	}
	if !config.Websockets {
		t.Fatalf("expected websockets to persist for explicit id import")
	}
}

func TestAPIKey_ImportChannelBatchPreservesModelEntryOrder(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "import-preserve-model-order.db")
	ctx := context.Background()

	channel := &model.ChannelWithKeys{
		Config: &model.Config{
			Name:     "imported-channel-order",
			URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
			Priority: 5,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: "z-last"},
				{Model: "a-first"},
			},
		},
		APIKeys: []model.APIKey{
			{KeyIndex: 0, APIKey: "sk-import-key-order", KeyStrategy: model.KeyStrategySequential},
		},
	}

	created, updated, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{channel})
	if err != nil {
		t.Fatalf("import channel batch: %v", err)
	}
	if created != 1 || updated != 0 {
		t.Fatalf("expected created=1 updated=0, got created=%d updated=%d", created, updated)
	}

	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	got := configs[0].ModelEntries
	if len(got) != 2 {
		t.Fatalf("expected 2 model entries, got %+v", got)
	}
	if got[0].Model != "z-last" || got[1].Model != "a-first" {
		t.Fatalf("expected model order [z-last a-first], got %+v", got)
	}
}

func TestAPIKey_ImportChannelBatchIsolatesChannelManagementEnvelope(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, "import-channel-management-isolation.db")
	ctx := context.Background()
	envelope := `{"kind":"channel_management","version":1,"profile":"sub2api","settings":{"base_url":"https://panel.example.com","access_token":"csv-private-token"},"state":{}}`

	_, _, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{{
		Config: &model.Config{
			Name: "must-reject-private-envelope", AuthType: model.AuthTypeAPIKey, OAuthCredential: envelope,
			URLs: model.ChannelURLs{{URL: "https://api.example.com"}}, Enabled: true,
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "api_key channel cannot contain") {
		t.Fatalf("ImportChannelBatch() envelope error = %v, want API Key private payload rejection", err)
	}

	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "already-managed", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://old.example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.CompareAndSwapChannelManagement(ctx, created.ID, "", envelope)
	if err != nil || !updated {
		t.Fatalf("seed management envelope = (%v, %v)", updated, err)
	}

	for _, imported := range []*model.Config{
		{
			Name: created.Name, AuthType: model.AuthTypeAPIKey,
			URLs: model.ChannelURLs{{URL: "https://by-name.example.com"}}, Enabled: true,
		},
		{
			ID: created.ID, Name: created.Name, AuthType: model.AuthTypeAPIKey,
			URLs: model.ChannelURLs{{URL: "https://by-id.example.com"}}, Enabled: true,
		},
	} {
		createdCount, updatedCount, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{{Config: imported}})
		if err != nil {
			t.Fatalf("ImportChannelBatch() update error = %v", err)
		}
		if createdCount != 0 || updatedCount != 1 {
			t.Fatalf("ImportChannelBatch() counts = (%d, %d), want (0, 1)", createdCount, updatedCount)
		}
		persisted, err := store.GetConfig(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.OAuthCredential != envelope {
			t.Fatalf("API Key management envelope changed during CSV update")
		}
	}

	createdCount, updatedCount, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{{
		Config: &model.Config{
			Name: "new-unmanaged", AuthType: model.AuthTypeAPIKey,
			URLs: model.ChannelURLs{{URL: "https://new.example.com"}}, Enabled: true,
		},
	}})
	if err != nil || createdCount != 1 || updatedCount != 0 {
		t.Fatalf("new ImportChannelBatch() = (%d, %d, %v), want (1, 0, nil)", createdCount, updatedCount, err)
	}
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range configs {
		if cfg.Name == "new-unmanaged" && cfg.OAuthCredential != "" {
			t.Fatalf("new API Key import inherited private envelope")
		}
	}
}
