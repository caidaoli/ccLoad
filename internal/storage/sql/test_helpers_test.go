package sql_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
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
		Name:     name,
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority: 1,
		Enabled:  true,
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

func quotaCostView(t testing.TB, ctx context.Context, store storage.Store, channelID int64, at time.Time) *oauthcost.CostView {
	t.Helper()
	cfg, err := store.GetConfig(ctx, channelID)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OAuthUsage     json.RawMessage  `json:"oauth_usage"`
		QuotaCostUsage *oauthcost.Usage `json:"quota_cost_usage"`
	}
	if err := json.Unmarshal([]byte(cfg.OAuthCredential), &envelope); err != nil {
		t.Fatal(err)
	}
	usage := oauthcost.EffectiveUsage(envelope.QuotaCostUsage, envelope.OAuthUsage)
	views, err := store.OAuthQuotaCostViews(ctx, map[int64]*oauthcost.Usage{channelID: usage}, at)
	if err != nil {
		t.Fatal(err)
	}
	return views[channelID]
}

func viewCost(view *oauthcost.CostView, key string) int64 {
	if window := view.FindWindow(key); window != nil {
		return window.StandardCostMicroUSD
	}
	return -1
}

func quotaCostAt(t testing.TB, ctx context.Context, store storage.Store, channelID int64, key string, at time.Time) int64 {
	t.Helper()
	return viewCost(quotaCostView(t, ctx, store, channelID, at), key)
}
