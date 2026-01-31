package sql_test

import (
	"context"
	"path/filepath"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func newTestStore(t testing.TB, dbFile string) storage.Store {
	t.Helper()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, dbFile))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	return store
}

func createTestChannel(t testing.TB, ctx context.Context, store storage.Store, name string) int64 {
	t.Helper()

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:        name,
		URL:         "https://api.example.com",
		Priority:    1,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4"},
		},
	})
	if err != nil {
		t.Fatalf("create test channel: %v", err)
	}
	return cfg.ID
}

func createTestAPIKey(t testing.TB, ctx context.Context, store storage.Store, channelID int64, keyIndex int) {
	t.Helper()

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: channelID, KeyIndex: keyIndex, APIKey: "sk-test-key", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("create test api key: %v", err)
	}
}

func countAPIKeys(allKeys map[int64][]*model.APIKey) int {
	total := 0
	for _, keys := range allKeys {
		total += len(keys)
	}
	return total
}
