package app

import (
	"context"
	"net/http"
	"testing"

	"ccLoad/internal/model"
)

func TestAdminGroupsCreateAndList(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	channel, err := store.CreateConfig(ctx, &model.Config{
		Name:        "group-channel",
		URL:         "https://api.example.com",
		Priority:    1,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-5.4"},
		},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	createPayload := map[string]any{
		"name":                 "gpt-5",
		"mode":                 model.GroupModeFailover,
		"match_regex":          "^gpt-5(\\.\\d+)?$",
		"first_token_time_out": 8,
		"session_keep_time":    120,
		"items": []map[string]any{
			{
				"channel_id": channel.ID,
				"model_name": "gpt-5.4",
				"priority":   1,
				"weight":     1,
			},
		},
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/groups", createPayload))
	server.HandleGroups(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}

	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/groups", nil))
	server.HandleGroups(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[[]*model.Group](t, w.Body.Bytes())
	if len(resp.Data) != 1 || resp.Data[0].Name != "gpt-5" {
		t.Fatalf("unexpected groups response: %+v", resp.Data)
	}
	if len(resp.Data[0].Items) != 1 || resp.Data[0].Items[0].ModelName != "gpt-5.4" {
		t.Fatalf("unexpected group items: %+v", resp.Data[0].Items)
	}
	if resp.Data[0].MatchRegex != "^gpt-5(\\.\\d+)?$" || resp.Data[0].FirstTokenTimeOut != 8 || resp.Data[0].SessionKeepTime != 120 {
		t.Fatalf("unexpected group advanced fields: %+v", resp.Data[0])
	}
}

func TestAdminGroupsRejectInvalidMatchRegex(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	createPayload := map[string]any{
		"name":        "broken-group",
		"match_regex": "(",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/groups", createPayload))
	server.HandleGroups(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d body=%s", w.Code, w.Body.String())
	}
}
