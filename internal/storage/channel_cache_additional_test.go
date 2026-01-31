package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestChannelCache_GetConfig(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cache_getconfig.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "ch",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	cache := storage.NewChannelCache(store, 10*time.Minute)
	got, err := cache.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if got.ID != created.ID || got.Name != "ch" {
		t.Fatalf("unexpected config: %+v", got)
	}
}

func TestChannelCache_InvalidateCache_ForcesRefresh(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cache_invalidate.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	cache := storage.NewChannelCache(store, 24*time.Hour) // 足够大，确保不自动过期

	// 第一次创建并填充缓存
	if _, err := store.CreateConfig(ctx, &model.Config{
		Name:         "ch1",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateConfig ch1 failed: %v", err)
	}
	got1, err := cache.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel(*) failed: %v", err)
	}
	if len(got1) != 1 {
		t.Fatalf("len=%d, want 1", len(got1))
	}

	// 数据库新增一个渠道，但缓存未失效时不应看见
	if _, err := store.CreateConfig(ctx, &model.Config{
		Name:         "ch2",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateConfig ch2 failed: %v", err)
	}
	got2, err := cache.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel(*) second failed: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("expected cached result len=1, got %d", len(got2))
	}

	// 手动失效后应刷新并返回2条
	cache.InvalidateCache()
	got3, err := cache.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel(*) after invalidate failed: %v", err)
	}
	if len(got3) != 2 {
		t.Fatalf("expected refreshed result len=2, got %d", len(got3))
	}
}

func TestChannelCache_APIKeysCacheAndInvalidation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cache_apikeys.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "ch",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-0", KeyStrategy: model.KeyStrategySequential}, //nolint:gosec
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	cache := storage.NewChannelCache(store, 10*time.Minute)

	keys1, err := cache.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys #1 failed: %v", err)
	}
	if len(keys1) != 1 || keys1[0].KeyIndex != 0 {
		t.Fatalf("unexpected keys1: %+v", keys1)
	}

	// 修改返回值不应污染缓存
	keys1[0].APIKey = "POLLUTED"
	keys2, err := cache.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys #2 failed: %v", err)
	}
	if keys2[0].APIKey == "POLLUTED" {
		t.Fatalf("cache polluted by caller mutation")
	}

	// DB新增key，但未失效时仍返回旧缓存
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-1", KeyStrategy: model.KeyStrategySequential}, //nolint:gosec
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch #2 failed: %v", err)
	}
	keys3, err := cache.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys #3 failed: %v", err)
	}
	if len(keys3) != 1 {
		t.Fatalf("expected cached keys len=1, got %d", len(keys3))
	}

	cache.InvalidateAPIKeysCache(created.ID)
	keys4, err := cache.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys after invalidate failed: %v", err)
	}
	if len(keys4) != 2 {
		t.Fatalf("expected refreshed keys len=2, got %d", len(keys4))
	}

	cache.InvalidateAllAPIKeysCache()
	keys5, err := cache.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys after invalidate-all failed: %v", err)
	}
	if len(keys5) != 2 {
		t.Fatalf("expected keys len=2 after invalidate-all reload, got %d", len(keys5))
	}
}

func TestChannelCache_CooldownCacheAndInvalidation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cache_cooldown.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "ch",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-0", KeyStrategy: model.KeyStrategySequential}, //nolint:gosec
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	cache := storage.NewChannelCache(store, 10*time.Minute)

	now := time.Now()
	until1 := now.Add(1 * time.Minute)
	until2 := now.Add(2 * time.Minute)

	if err := store.SetChannelCooldown(ctx, created.ID, until1); err != nil {
		t.Fatalf("SetChannelCooldown #1 failed: %v", err)
	}
	m1, err := cache.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns #1 failed: %v", err)
	}
	if got := m1[created.ID]; got.Unix() != until1.Unix() {
		t.Fatalf("cooldown #1=%v, want %v", got, until1)
	}

	// 更新DB，但缓存仍有效时应保持旧值
	if err := store.SetChannelCooldown(ctx, created.ID, until2); err != nil {
		t.Fatalf("SetChannelCooldown #2 failed: %v", err)
	}
	m2, err := cache.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns #2 failed: %v", err)
	}
	if got := m2[created.ID]; got.Unix() != until1.Unix() {
		t.Fatalf("expected cached cooldown=%v, got %v", until1, got)
	}

	cache.InvalidateCooldownCache()
	m3, err := cache.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns after invalidate failed: %v", err)
	}
	if got := m3[created.ID]; got.Unix() != until2.Unix() {
		t.Fatalf("expected refreshed cooldown=%v, got %v", until2, got)
	}

	// Key cooldown：同样验证缓存+失效
	// 注意：ChannelCache 的 channel/key 冷却共用 lastUpdate（单缓存域）。
	// 如果不先失效，GetAllKeyCooldowns 可能会“命中”刚刚填充的 channel 冷却缓存，从而返回空的 keys。
	cache.InvalidateCooldownCache()

	keyUntil1 := now.Add(3 * time.Minute)
	keyUntil2 := now.Add(4 * time.Minute)
	if err := store.SetKeyCooldown(ctx, created.ID, 0, keyUntil1); err != nil {
		t.Fatalf("SetKeyCooldown #1 failed: %v", err)
	}
	k1, err := cache.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns #1 failed: %v", err)
	}
	if got := k1[created.ID][0]; got.Unix() != keyUntil1.Unix() {
		t.Fatalf("key cooldown #1=%v, want %v", got, keyUntil1)
	}

	if err := store.SetKeyCooldown(ctx, created.ID, 0, keyUntil2); err != nil {
		t.Fatalf("SetKeyCooldown #2 failed: %v", err)
	}
	k2, err := cache.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns #2 failed: %v", err)
	}
	if got := k2[created.ID][0]; got.Unix() != keyUntil1.Unix() {
		t.Fatalf("expected cached key cooldown=%v, got %v", keyUntil1, got)
	}

	cache.InvalidateCooldownCache()
	k3, err := cache.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns after invalidate failed: %v", err)
	}
	if got := k3[created.ID][0]; got.Unix() != keyUntil2.Unix() {
		t.Fatalf("expected refreshed key cooldown=%v, got %v", keyUntil2, got)
	}
}
