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
		KeyStrategy: "sequential",
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
