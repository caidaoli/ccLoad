package app

import (
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestFilterGroupItemsIgnoresCooledModelOnly(t *testing.T) {
	now := time.Now()
	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "gpt-5.5", Priority: 1, Weight: 1},
		{ChannelID: 1, ModelName: "gpt-5.4", Priority: 2, Weight: 1},
	}
	cooldowns := map[int64]map[string]time.Time{
		1: {"gpt-5.5": now.Add(time.Minute)},
	}

	filtered := filterGroupItemsByModelCooldown(items, cooldowns, now)
	if len(filtered) != 1 || filtered[0].ModelName != "gpt-5.4" {
		t.Fatalf("expected only gpt-5.4 available, got %+v", filtered)
	}
}

func TestSortGroupItemsFailoverByPriority(t *testing.T) {
	items := []model.GroupItem{
		{ID: 2, ChannelID: 1, ModelName: "b", Priority: 10},
		{ID: 1, ChannelID: 2, ModelName: "a", Priority: 1},
	}

	sorted := orderGroupItems(model.GroupModeFailover, items)
	if sorted[0].ModelName != "a" {
		t.Fatalf("expected priority order, got %+v", sorted)
	}
}
