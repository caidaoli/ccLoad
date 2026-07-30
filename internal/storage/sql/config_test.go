package sql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	_ "modernc.org/sqlite"
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
		Name: "test-channel",
		URLs: model.ChannelURLs{
			{URL: "https://api.openai.com", Protocols: []string{"openai", "codex"}},
			{URL: "https://api.openai.com/v1/responses", Exact: true, Protocols: []string{"codex"}},
		},
		Priority:              10,
		Enabled:               true,
		ChannelType:           "openai",
		Websockets:            true,
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		RPMLimit:              60,
		MaxConcurrency:        3,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4"},
			{Model: "gpt-3.5-turbo"},
		},
		CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
			Enabled: true, Name: "Rate limit", Priority: 0, StatusCodes: []int{429},
			Scope: model.CooldownScopeKey, Mode: model.CooldownModeFixed, CooldownSeconds: 90,
		}}},
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
	if urls := got.GetURLs(); len(urls) != 2 || urls[0] != "https://api.openai.com" || urls[1] != "https://api.openai.com/v1/responses#" {
		t.Errorf("urls: got %v", urls)
	}
	if len(got.URLs[0].Protocols) != 2 || got.URLs[0].Protocols[0] != "openai" || got.URLs[1].Protocols[0] != "codex" {
		t.Errorf("URL protocols: got %+v", got.URLs)
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
	if !got.Websockets {
		t.Error("expected websockets=true")
	}
	if got.GetProtocolTransformMode() != model.ProtocolTransformModeLocal {
		t.Fatalf("protocol_transform_mode=%q, want local", got.GetProtocolTransformMode())
	}
	if got.RPMLimit != 60 {
		t.Errorf("rpm_limit: got %d, want 60", got.RPMLimit)
	}
	if got.MaxConcurrency != 3 {
		t.Errorf("max_concurrency: got %d, want 3", got.MaxConcurrency)
	}
	if len(got.ModelEntries) != 2 {
		t.Errorf("model entries count: got %d, want 2", len(got.ModelEntries))
	}
	if got.CooldownDetectionRules == nil || len(got.CooldownDetectionRules.Rules) != 1 {
		t.Fatalf("cooldown detection rules = %#v, want one persisted rule", got.CooldownDetectionRules)
	}
	rule := got.CooldownDetectionRules.Rules[0]
	if !rule.Enabled || rule.Name != "Rate limit" || rule.Priority != 0 || rule.Scope != model.CooldownScopeKey || rule.Mode != model.CooldownModeFixed || rule.CooldownSeconds != 90 {
		t.Fatalf("persisted cooldown detection rule = %#v", rule)
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
			URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
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
	for _, cfg := range configs {
		if cfg.GetProtocolTransformMode() != model.ProtocolTransformModeAuto {
			t.Fatalf("channel %d protocol_transform_mode=%q, want auto", cfg.ID, cfg.GetProtocolTransformMode())
		}
	}

	// 验证按优先级降序排列
	for i := 1; i < len(configs); i++ {
		if configs[i-1].Priority < configs[i].Priority {
			t.Errorf("configs not sorted by priority DESC: %d < %d",
				configs[i-1].Priority, configs[i].Priority)
		}
	}
}

func TestConfig_UpdateChannelEnabledOnlyTouchesEnabled(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "update-enabled.db")
	ctx := context.Background()

	created, err := store.CreateConfig(ctx, &model.Config{
		Name:        "toggle-only",
		URLs:        model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:    42,
		Enabled:     true,
		ChannelType: "gemini",
		ModelEntries: []model.ModelEntry{
			{Model: "gemini-2.5-pro", RedirectModel: "gemini-2.5-pro"},
			{Model: "gemini-2.5-flash", RedirectModel: "flash-upstream"},
		},
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-toggle",
		KeyStrategy: model.KeyStrategyRoundRobin,
	}}); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	updated, err := store.UpdateChannelEnabled(ctx, created.ID, false)
	if err != nil {
		t.Fatalf("update enabled: %v", err)
	}
	if updated.Enabled {
		t.Fatalf("updated response should be disabled")
	}

	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if got.Enabled {
		t.Fatalf("stored channel should be disabled")
	}
	if got.Name != created.Name || got.GetURLs()[0] != created.GetURLs()[0] || got.Priority != created.Priority || got.ChannelType != created.ChannelType {
		t.Fatalf("non-enabled fields changed: got=%+v want=%+v", got, created)
	}
	if len(got.ModelEntries) != 2 || got.ModelEntries[1].RedirectModel != "flash-upstream" {
		t.Fatalf("model entries changed: %#v", got.ModelEntries)
	}

	keys, err := store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("get api keys: %v", err)
	}
	if len(keys) != 1 || keys[0].APIKey != "sk-toggle" || keys[0].KeyStrategy != model.KeyStrategyRoundRobin {
		t.Fatalf("api keys changed: %#v", keys)
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
		URLs:     model.ChannelURLs{{URL: "https://old.api.com"}},
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
	created.URLs = model.ChannelURLs{{URL: "https://new.api.com"}}
	created.Priority = 100
	created.Enabled = false
	created.ModelEntries = []model.ModelEntry{
		{Model: "new-model-1"},
		{Model: "new-model-2"},
	}
	created.CooldownDetectionRules = &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
		Enabled: true, Name: "Reset time", Priority: 0, MessagePattern: `reset at (?P<until>\d{4}-\d{2}-\d{2})`,
		Scope: model.CooldownScopeChannel, Mode: model.CooldownModeResetTime,
		TimeCapture: "until", TimeFormat: model.CooldownTimeFormatDateTime, TimeLayout: "2006-01-02", Timezone: "UTC",
	}}}

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
	if urls := got.GetURLs(); len(urls) != 1 || urls[0] != "https://new.api.com" {
		t.Errorf("urls: got %v, want [https://new.api.com]", urls)
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
	if got.CooldownDetectionRules == nil || len(got.CooldownDetectionRules.Rules) != 1 || got.CooldownDetectionRules.Rules[0].Mode != model.CooldownModeResetTime || got.CooldownDetectionRules.Rules[0].TimeFormat != model.CooldownTimeFormatDateTime {
		t.Fatalf("updated cooldown detection rules = %#v", got.CooldownDetectionRules)
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
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
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

func TestConfig_DeleteConfig_RemovesLogsAndDebugLogs(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "delete-logs.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:    "delete-with-logs",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	now := time.Now()
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{{
		Time:       model.JSONTime{Time: now},
		Model:      "model-1",
		ChannelID:  created.ID,
		StatusCode: 200,
		DebugData: &model.DebugLogEntry{
			CreatedAt:   now.Unix(),
			ReqMethod:   "POST",
			ReqURL:      "https://api.example.com/v1/messages",
			ReqHeaders:  "{}",
			ReqBody:     []byte(`{"model":"model-1"}`),
			RespStatus:  200,
			RespHeaders: "{}",
			RespBody:    []byte(`{"ok":true}`),
		},
	}}); err != nil {
		t.Fatalf("add log with debug data: %v", err)
	}

	logs, err := store.ListLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), 10, 0, nil)
	if err != nil {
		t.Fatalf("list logs before delete: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log before delete, got %d", len(logs))
	}
	logID := logs[0].ID
	debugLog, err := store.GetDebugLogByLogID(ctx, logID)
	if err != nil {
		t.Fatalf("get debug log before delete: %v", err)
	}
	if debugLog == nil {
		t.Fatalf("expected debug log before delete")
	}

	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	channelID := created.ID
	count, err := store.CountLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), &model.LogFilter{ChannelID: &channelID})
	if err != nil {
		t.Fatalf("count logs after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected channel logs removed, got %d", count)
	}
	debugLog, err = store.GetDebugLogByLogID(ctx, logID)
	if err != nil {
		t.Fatalf("get debug log after delete: %v", err)
	}
	if debugLog != nil {
		t.Fatalf("expected debug log removed after deleting channel")
	}
}

func TestLog_AddLogAndBatchAddLogs_IgnoreDeletedChannel(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "deleted-channel-log.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:    "deleted-channel",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	now := time.Now()
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:       model.JSONTime{Time: now},
		Model:      "stale-single-log",
		ChannelID:  created.ID,
		StatusCode: 200,
	}); err != nil {
		t.Fatalf("add stale single log: %v", err)
	}
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{
			Time:       model.JSONTime{Time: now},
			Model:      "stale-channel-log",
			ChannelID:  created.ID,
			StatusCode: 200,
		},
		{
			Time:       model.JSONTime{Time: now},
			Model:      "system-log",
			ChannelID:  0,
			StatusCode: 503,
		},
	}); err != nil {
		t.Fatalf("batch add logs: %v", err)
	}

	deletedChannelID := created.ID
	deletedCount, err := store.CountLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), &model.LogFilter{ChannelID: &deletedChannelID})
	if err != nil {
		t.Fatalf("count deleted-channel logs: %v", err)
	}
	if deletedCount != 0 {
		t.Fatalf("deleted channel log was inserted after channel deletion: count=%d", deletedCount)
	}

	allLogs, err := store.ListLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), 10, 0, nil)
	if err != nil {
		t.Fatalf("list all logs: %v", err)
	}
	if len(allLogs) != 1 || allLogs[0].ChannelID != 0 {
		t.Fatalf("expected only channel_id=0 log to remain, got %+v", allLogs)
	}
}

func TestConfig_DeleteConfig_PurgesOrphanLogsWhenChannelMissing(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "orphan-channel-log.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	const orphanChannelID int64 = 194
	now := time.Now()
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{
			Time:       model.JSONTime{Time: now},
			Model:      "orphan-channel-log",
			ChannelID:  orphanChannelID,
			StatusCode: 200,
		},
		{
			Time:       model.JSONTime{Time: now},
			Model:      "system-log",
			ChannelID:  0,
			StatusCode: 503,
		},
	}); err != nil {
		t.Fatalf("batch add orphan logs: %v", err)
	}

	if err := store.DeleteConfig(ctx, orphanChannelID); err != nil {
		t.Fatalf("delete missing config: %v", err)
	}

	channelID := orphanChannelID
	count, err := store.CountLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), &model.LogFilter{ChannelID: &channelID})
	if err != nil {
		t.Fatalf("count orphan-channel logs: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected orphan channel logs purged, got %d", count)
	}

	allLogs, err := store.ListLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), 10, 0, nil)
	if err != nil {
		t.Fatalf("list all logs: %v", err)
	}
	if len(allLogs) != 1 || allLogs[0].ChannelID != 0 {
		t.Fatalf("expected only channel_id=0 log to remain, got %+v", allLogs)
	}
}

func TestConfig_DeleteConfig_ImportBatchWithSameIDAcceptsLogs(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "restore-deleted-channel.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:    "deleted-before-restore",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	imported := &model.ChannelWithKeys{
		Config: &model.Config{
			ID:      created.ID,
			Name:    "restored-channel",
			URLs:    model.ChannelURLs{{URL: "https://api-restored.example.com"}},
			Enabled: true,
			ModelEntries: []model.ModelEntry{
				{Model: "restored-model"},
			},
		},
		APIKeys: []model.APIKey{
			{KeyIndex: 0, APIKey: "sk-restored", KeyStrategy: model.KeyStrategySequential},
		},
	}
	createdCount, updatedCount, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{imported})
	if err != nil {
		t.Fatalf("import restored channel: %v", err)
	}
	if createdCount != 1 || updatedCount != 0 {
		t.Fatalf("import counts = (%d,%d), want (1,0)", createdCount, updatedCount)
	}

	now := time.Now()
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:       model.JSONTime{Time: now},
		Model:      "restored-model",
		ChannelID:  created.ID,
		StatusCode: 200,
	}); err != nil {
		t.Fatalf("add restored channel log: %v", err)
	}

	channelID := created.ID
	count, err := store.CountLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), &model.LogFilter{ChannelID: &channelID})
	if err != nil {
		t.Fatalf("count restored channel logs: %v", err)
	}
	if count != 1 {
		t.Fatalf("restored channel log count=%d, want 1", count)
	}
}

func TestConfig_DeleteConfig_AllowsRecreateWithSameIDAndKeyIndicesInMemoryStore(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:    "to-delete-memory",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-old-0", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-old-1", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("create api keys batch: %v", err)
	}

	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	recreated, err := store.CreateConfig(ctx, &model.Config{
		ID:      created.ID,
		Name:    "recreated-memory",
		URLs:    model.ChannelURLs{{URL: "https://api-recreated.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("recreate config with explicit id: %v", err)
	}

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: recreated.ID, KeyIndex: 0, APIKey: "sk-new-0", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("create api key after recreate: %v", err)
	}

	keys, err := store.GetAPIKeys(ctx, recreated.ID)
	if err != nil {
		t.Fatalf("get recreated api keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 recreated api key, got %d", len(keys))
	}
}

func TestConfig_GetEnabledChannelsByModel(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "model_query.db")

	ctx := context.Background()

	// 创建启用的渠道支持 gpt-4
	cfg1 := &model.Config{
		Name:     "gpt4-channel",
		URLs:     model.ChannelURLs{{URL: "https://api.openai.com"}},
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
		URLs:     model.ChannelURLs{{URL: "https://api.anthropic.com"}},
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
		URLs:     model.ChannelURLs{{URL: "https://api.disabled.com"}},
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

func TestConfig_GetEnabledChannelsIncludesCooledEnabledChannels(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "enabled_includes_cooled.db")
	ctx := context.Background()

	cooled, err := store.CreateConfig(ctx, &model.Config{
		Name:        "cooled-enabled",
		URLs:        model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:    100,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("create cooled config: %v", err)
	}
	if err := store.SetChannelCooldown(ctx, cooled.ID, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatalf("set channel cooldown: %v", err)
	}

	disabled, err := store.CreateConfig(ctx, &model.Config{
		Name:        "disabled",
		URLs:        model.ChannelURLs{{URL: "https://disabled.example.com"}},
		Priority:    90,
		Enabled:     false,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("create disabled config: %v", err)
	}

	assertHasOnlyCooled := func(name string, configs []*model.Config) {
		t.Helper()
		foundCooled := false
		for _, cfg := range configs {
			switch cfg.ID {
			case cooled.ID:
				foundCooled = true
			case disabled.ID:
				t.Fatalf("%s returned disabled channel: %+v", name, cfg)
			}
		}
		if !foundCooled {
			t.Fatalf("%s did not return cooled enabled channel; got %+v", name, configs)
		}
	}

	byModel, err := store.GetEnabledChannelsByModel(ctx, "gpt-4o")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel: %v", err)
	}
	assertHasOnlyCooled("GetEnabledChannelsByModel", byModel)

	allByModel, err := store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel(*): %v", err)
	}
	assertHasOnlyCooled("GetEnabledChannelsByModel(*)", allByModel)

	byType, err := store.GetEnabledChannelsByType(ctx, "openai")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByType: %v", err)
	}
	assertHasOnlyCooled("GetEnabledChannelsByType", byType)

}

func TestConfig_GetEnabledChannelsByType(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "type_query.db")

	ctx := context.Background()

	// 创建 openai 类型渠道
	cfg1 := &model.Config{
		Name:        "openai-channel",
		URLs:        model.ChannelURLs{{URL: "https://api.openai.com"}},
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
		URLs:        model.ChannelURLs{{URL: "https://api.anthropic.com"}},
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
			URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
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
		URLs:        model.ChannelURLs{{URL: "https://api.example.com"}},
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
