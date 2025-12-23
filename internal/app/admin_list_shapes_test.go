package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func parseAPIResponseMap(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// 治本：APIResponse 四字段必须始终存在（success/data/error/count）。
	for _, k := range []string{"success", "data", "error", "count"} {
		if _, ok := resp[k]; !ok {
			t.Fatalf("Expected key %q to exist", k)
		}
	}
	if _, ok := resp["success"].(bool); !ok {
		t.Fatalf("Expected success bool, got %T", resp["success"])
	}
	if _, ok := resp["error"].(string); !ok {
		t.Fatalf("Expected error string, got %T", resp["error"])
	}
	if _, ok := resp["count"].(float64); !ok {
		t.Fatalf("Expected count number, got %T", resp["count"])
	}

	return resp
}

func TestAdminAPI_ChannelKeys_ResponseShape_Empty(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:        "Test",
		URL:         "https://example.com",
		Priority:    10,
		ModelEntries: []model.ModelEntry{},
		ChannelType: "anthropic",
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/channels/1/keys", nil)

	server.handleGetChannelKeys(c, created.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := parseAPIResponseMap(t, w.Body.Bytes())
	data := resp["data"]
	if data == nil {
		t.Fatalf("Expected data to be [], got null")
	}
	if _, ok := data.([]any); !ok {
		t.Fatalf("Expected data to be array, got %T", data)
	}
}

func TestAdminAPI_GetModels_ResponseShape_Empty(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/models?range=today", nil)

	server.HandleGetModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := parseAPIResponseMap(t, w.Body.Bytes())
	data := resp["data"]
	if data == nil {
		t.Fatalf("Expected data to be [], got null")
	}
	if _, ok := data.([]any); !ok {
		t.Fatalf("Expected data to be array, got %T", data)
	}
}

func TestAdminAPI_GetLogs_ResponseShape_Empty(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/logs?range=today&limit=10&offset=0", nil)

	server.HandleErrors(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := parseAPIResponseMap(t, w.Body.Bytes())
	data := resp["data"]
	if data == nil {
		t.Fatalf("Expected data to be [], got null")
	}
	if _, ok := data.([]any); !ok {
		t.Fatalf("Expected data to be array, got %T", data)
	}
}

func TestAdminAPI_GetStats_ResponseShape_Empty(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/stats?range=today", nil)

	server.HandleStats(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := parseAPIResponseMap(t, w.Body.Bytes())
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("Expected data object, got %T", resp["data"])
	}

	stats := data["stats"]
	if stats == nil {
		t.Fatalf("Expected data.stats to be [], got null")
	}
	if _, ok := stats.([]any); !ok {
		t.Fatalf("Expected data.stats to be array, got %T", stats)
	}
}

func TestAdminAPI_GetMetrics_ResponseShape(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/metrics?range=today&bucket_min=5", nil)

	server.HandleMetrics(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := parseAPIResponseMap(t, w.Body.Bytes())
	data := resp["data"]
	if data == nil {
		t.Fatalf("Expected data to be [], got null")
	}
	if _, ok := data.([]any); !ok {
		t.Fatalf("Expected data to be array, got %T", data)
	}
}
