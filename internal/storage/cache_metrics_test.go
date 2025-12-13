package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestChannelCacheMetrics(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	dbPath := filepath.Join(tmpDir, "metrics.db")
	store, err := storage.CreateSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	cache := storage.NewChannelCache(store, time.Minute)

	cfg := &model.Config{
		Name:     "test-channel",
		URL:      "https://example.com",
		Priority: 10,
		Models:   []string{"model-a"},
		Enabled:  true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}

	now := time.Now()
	apiKey := &model.APIKey{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-test",
		KeyStrategy: model.KeyStrategySequential,
		CreatedAt:   model.JSONTime{Time: now},
		UpdatedAt:   model.JSONTime{Time: now},
	}
	if err := store.CreateAPIKey(ctx, apiKey); err != nil {
		t.Fatalf("failed to create api key: %v", err)
	}

	if _, err := cache.GetAPIKeys(ctx, created.ID); err != nil {
		t.Fatalf("unexpected error getting api keys: %v", err)
	}

	if _, err := cache.GetAPIKeys(ctx, created.ID); err != nil {
		t.Fatalf("unexpected error getting api keys (cached): %v", err)
	}

	cache.InvalidateAPIKeysCache(created.ID)

	stats := cache.GetCacheStats()

	if hits, ok := stats["api_keys_hits"].(uint64); !ok || hits != 1 {
		t.Fatalf("expected 1 api key hit, got %v", stats["api_keys_hits"])
	}
	if misses, ok := stats["api_keys_misses"].(uint64); !ok || misses != 1 {
		t.Fatalf("expected 1 api key miss, got %v", stats["api_keys_misses"])
	}
	if invalidations, ok := stats["api_keys_invalidations"].(uint64); !ok || invalidations != 1 {
		t.Fatalf("expected 1 api key invalidation, got %v", stats["api_keys_invalidations"])
	}
}

// TestChannelCacheDeepCopy 验证缓存返回深拷贝，防止并发污染
// [REGRESSION] 防止回归到浅拷贝实现（只拷贝slice不拷贝对象）
func TestChannelCacheDeepCopy(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	dbPath := filepath.Join(tmpDir, "deepcopy.db")
	store, err := storage.CreateSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	cache := storage.NewChannelCache(store, time.Minute)

	// 创建测试渠道和API Key
	cfg := &model.Config{
		Name:     "test-channel",
		URL:      "https://example.com",
		Priority: 10,
		Models:   []string{"model-a"},
		Enabled:  true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}

	now := time.Now()
	apiKey := &model.APIKey{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-original-key",
		KeyStrategy: model.KeyStrategySequential,
		CreatedAt:   model.JSONTime{Time: now},
		UpdatedAt:   model.JSONTime{Time: now},
	}
	if err := store.CreateAPIKey(ctx, apiKey); err != nil {
		t.Fatalf("failed to create api key: %v", err)
	}

	// 第一次获取（填充缓存）
	keys1, err := cache.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("unexpected error getting api keys: %v", err)
	}
	if len(keys1) != 1 {
		t.Fatalf("expected 1 api key, got %d", len(keys1))
	}

	// 第二次获取（从缓存读取）
	keys2, err := cache.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("unexpected error getting cached api keys: %v", err)
	}
	if len(keys2) != 1 {
		t.Fatalf("expected 1 api key, got %d", len(keys2))
	}

	// 关键验证：修改keys1不应影响keys2
	originalKey := keys2[0].APIKey
	keys1[0].APIKey = "sk-POLLUTED-KEY"
	keys1[0].KeyIndex = 999

	// 第三次获取，验证缓存未被污染
	keys3, err := cache.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("unexpected error getting api keys after modification: %v", err)
	}
	if len(keys3) != 1 {
		t.Fatalf("expected 1 api key, got %d", len(keys3))
	}

	// 验证：keys3应该保留原始值，不受keys1修改的影响
	if keys3[0].APIKey != originalKey {
		t.Fatalf("cache pollution detected! expected key=%q, got key=%q", originalKey, keys3[0].APIKey)
	}
	if keys3[0].KeyIndex != 0 {
		t.Fatalf("cache pollution detected! expected KeyIndex=0, got KeyIndex=%d", keys3[0].KeyIndex)
	}

	// 额外验证：keys2也不应被修改（因为是深拷贝）
	if keys2[0].APIKey != originalKey {
		t.Fatalf("shallow copy detected! keys2 was modified by keys1 mutation")
	}
	if keys2[0].KeyIndex != 0 {
		t.Fatalf("shallow copy detected! keys2 KeyIndex was modified by keys1 mutation")
	}
}
