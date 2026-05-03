package sql_test

import (
	"context"
	"testing"
	"time"
)

func TestModelCooldownIsIsolatedByChannelAndModel(t *testing.T) {
	store := newTestStore(t, "model_cooldowns.db")
	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "model-cooldown-channel")
	now := time.Now()

	if _, err := store.BumpModelCooldown(ctx, channelID, "gpt-5.5", now, 503); err != nil {
		t.Fatalf("BumpModelCooldown failed: %v", err)
	}

	cooldowns, err := store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllModelCooldowns failed: %v", err)
	}
	if !cooldowns[channelID]["gpt-5.5"].After(now) {
		t.Fatalf("gpt-5.5 cooldown missing: %+v", cooldowns)
	}
	if _, exists := cooldowns[channelID]["gpt-5.4"]; exists {
		t.Fatalf("gpt-5.4 should not be cooled: %+v", cooldowns[channelID])
	}

	if err := store.ResetModelCooldown(ctx, channelID, "gpt-5.5"); err != nil {
		t.Fatalf("ResetModelCooldown failed: %v", err)
	}
	cooldowns, err = store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllModelCooldowns after reset failed: %v", err)
	}
	if byModel := cooldowns[channelID]; byModel != nil {
		if _, exists := byModel["gpt-5.5"]; exists {
			t.Fatalf("gpt-5.5 cooldown should be reset: %+v", cooldowns)
		}
	}
}
