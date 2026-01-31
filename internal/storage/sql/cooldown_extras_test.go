package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

func TestCooldown_GetKeyCooldownUntil_AndClearAll(t *testing.T) {
	store := newTestStore(t, "cooldown_extras.db")
	ctx := context.Background()

	ss := store.(*sqlstore.SQLStore)

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "c1",
		URL:          "https://example.com",
		Priority:     1,
		Enabled:      true,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "k0", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	until := time.Now().Add(2 * time.Minute)
	if err := store.SetKeyCooldown(ctx, cfg.ID, 0, until); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	gotUntil, ok := ss.GetKeyCooldownUntil(ctx, cfg.ID, 0)
	if !ok || gotUntil.IsZero() {
		t.Fatalf("GetKeyCooldownUntil ok=%v until=%v, want true,non-zero", ok, gotUntil)
	}

	if err := ss.ClearAllKeyCooldowns(ctx, cfg.ID); err != nil {
		t.Fatalf("ClearAllKeyCooldowns failed: %v", err)
	}
	_, ok = ss.GetKeyCooldownUntil(ctx, cfg.ID, 0)
	if ok {
		t.Fatalf("expected key cooldown cleared")
	}
}
