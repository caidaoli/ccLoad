package app

import (
	"context"
	"net/http"
	"testing"

	"ccLoad/internal/model"
)

func TestAdminAPI_ChannelKeys_ResponseShape_Empty(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "Test",
		URL:          "https://example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{},
		ChannelType:  "anthropic",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/1/keys", nil))

	server.handleGetChannelKeys(c, created.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[[]*model.APIKey](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data == nil {
		t.Fatalf("data is null, want []")
	}
}

func TestAdminAPI_GetModels_ResponseShape_Empty(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/models?range=today", nil))

	server.HandleGetModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[[]string](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data == nil {
		t.Fatalf("data is null, want []")
	}
}

func TestAdminAPI_GetLogs_ResponseShape_Empty(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/logs?range=today&limit=10&offset=0", nil))

	server.HandleErrors(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[[]*model.LogEntry](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data == nil {
		t.Fatalf("data is null, want []")
	}
}

func TestAdminAPI_GetStats_ResponseShape_Empty(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/stats?range=today", nil))

	server.HandleStats(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	type statsResp struct {
		Stats []model.StatsEntry `json:"stats"`
	}
	resp := mustParseAPIResponse[statsResp](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data.Stats == nil {
		t.Fatalf("stats is null, want []")
	}
}

func TestAdminAPI_GetMetrics_ResponseShape(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/metrics?range=today&bucket_min=5", nil))

	server.HandleMetrics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[[]model.MetricPoint](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data == nil {
		t.Fatalf("data is null, want []")
	}
}
