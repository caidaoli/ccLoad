package sql_test

import (
	"context"
	"testing"

	"ccLoad/internal/model"
)

func TestGroupCRUDPersistsItems(t *testing.T) {
	store := newTestStore(t, "groups.db")
	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "group-channel")

	group, err := store.CreateGroup(ctx, &model.Group{
		Name:              "gpt-5",
		Mode:              model.GroupModeFailover,
		MatchRegex:        "^gpt-5(\\.\\d+)?$",
		FirstTokenTimeOut: 12,
		SessionKeepTime:   180,
		Items: []model.GroupItem{{
			ChannelID: channelID,
			ModelName: "gpt-5.4",
			Priority:  1,
			Weight:    2,
		}},
	})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if group.ID == 0 || len(group.Items) != 1 {
		t.Fatalf("unexpected group: %+v", group)
	}

	got, err := store.GetGroupByName(ctx, "gpt-5")
	if err != nil {
		t.Fatalf("GetGroupByName failed: %v", err)
	}
	if got.Name != "gpt-5" || got.Items[0].ModelName != "gpt-5.4" {
		t.Fatalf("unexpected loaded group: %+v", got)
	}
	if got.MatchRegex != "^gpt-5(\\.\\d+)?$" || got.FirstTokenTimeOut != 12 || got.SessionKeepTime != 180 {
		t.Fatalf("unexpected loaded advanced fields: %+v", got)
	}

	updatedName := "gpt-5-main"
	mode := model.GroupModeWeighted
	updatedRegex := "^gpt-5-(main|backup)$"
	updatedFirstTokenTimeOut := 25
	updatedSessionKeepTime := 360
	updated, err := store.UpdateGroup(ctx, got.ID, &model.GroupUpdateRequest{
		Name:              &updatedName,
		Mode:              &mode,
		MatchRegex:        &updatedRegex,
		FirstTokenTimeOut: &updatedFirstTokenTimeOut,
		SessionKeepTime:   &updatedSessionKeepTime,
		ItemsToUpdate: []model.GroupItemInput{{
			ID:        got.Items[0].ID,
			ChannelID: channelID,
			ModelName: "gpt-5.4",
			Priority:  2,
			Weight:    3,
		}},
	})
	if err != nil {
		t.Fatalf("UpdateGroup failed: %v", err)
	}
	if updated.Name != updatedName || updated.Mode != mode || updated.Items[0].Weight != 3 {
		t.Fatalf("unexpected updated group: %+v", updated)
	}
	if updated.MatchRegex != updatedRegex || updated.FirstTokenTimeOut != updatedFirstTokenTimeOut || updated.SessionKeepTime != updatedSessionKeepTime {
		t.Fatalf("unexpected updated advanced fields: %+v", updated)
	}

	if err := store.DeleteGroup(ctx, updated.ID); err != nil {
		t.Fatalf("DeleteGroup failed: %v", err)
	}
	if _, err := store.GetGroupByName(ctx, updatedName); err == nil {
		t.Fatal("expected deleted group lookup to fail")
	}
}
