package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestHandleDeleteAPIKey(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "ch",
		URL:          "https://example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now()
	keys := []*model.APIKey{
		{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "k0", KeyStrategy: model.KeyStrategySequential, CreatedAt: model.JSONTime{Time: now}, UpdatedAt: model.JSONTime{Time: now}},
		{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "k1", KeyStrategy: model.KeyStrategySequential, CreatedAt: model.JSONTime{Time: now}, UpdatedAt: model.JSONTime{Time: now}},
		{ChannelID: cfg.ID, KeyIndex: 2, APIKey: "k2", KeyStrategy: model.KeyStrategySequential, CreatedAt: model.JSONTime{Time: now}, UpdatedAt: model.JSONTime{Time: now}},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	t.Run("invalid channel id", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/abc/keys/0", nil))
		c.Params = gin.Params{{Key: "id", Value: "abc"}, {Key: "keyIndex", Value: "0"}}

		server.HandleDeleteAPIKey(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid key index", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/1/keys/x", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}, {Key: "keyIndex", Value: "x"}}

		server.HandleDeleteAPIKey(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("key not found", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/1/keys/9", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}, {Key: "keyIndex", Value: "9"}}

		server.HandleDeleteAPIKey(c)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("success compacts indices", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/1/keys/1", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}, {Key: "keyIndex", Value: "1"}}

		server.HandleDeleteAPIKey(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		after, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetAPIKeys failed: %v", err)
		}
		if len(after) != 2 {
			t.Fatalf("keys len=%d, want 2", len(after))
		}
		// 删除索引1后，原 index2 应被压缩成 index1
		if after[0].KeyIndex != 0 || after[1].KeyIndex != 1 {
			t.Fatalf("unexpected indices: %+v", []int{after[0].KeyIndex, after[1].KeyIndex})
		}
		if after[1].APIKey != "k2" {
			t.Fatalf("expected compacted key to be k2 at index1, got %q", after[1].APIKey)
		}
	})
}

func TestHandleAddAndDeleteModels(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "ch",
		URL:          "https://example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	t.Run("add invalid request", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/1/models", []byte(`{"models":[]}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleAddModels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("add invalid model entry", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/1/models", []byte(`{"models":[{"model":""}]}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleAddModels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("add dedup success", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/1/models", []byte(`{"models":[{"model":"m1"},{"model":"m2"}]}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleAddModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		updated, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		if len(updated.ModelEntries) != 2 {
			t.Fatalf("ModelEntries len=%d, want 2", len(updated.ModelEntries))
		}
	})

	t.Run("delete invalid request", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodDelete, "/admin/channels/1/models", []byte(`{}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleDeleteModels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("delete success", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodDelete, "/admin/channels/1/models", []byte(`{"models":["m2","absent"]}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleDeleteModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		updated, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		if len(updated.ModelEntries) != 1 || updated.ModelEntries[0].Model != "m1" {
			t.Fatalf("unexpected remaining models: %#v", updated.ModelEntries)
		}
	})
}

func TestHandleBatchUpdatePriority(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	c1, err := store.CreateConfig(ctx, &model.Config{Name: "c1", URL: "https://x", Priority: 1, ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true})
	if err != nil {
		t.Fatalf("CreateConfig c1 failed: %v", err)
	}
	c2, err := store.CreateConfig(ctx, &model.Config{Name: "c2", URL: "https://x", Priority: 2, ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true})
	if err != nil {
		t.Fatalf("CreateConfig c2 failed: %v", err)
	}

	t.Run("invalid json", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-priority", []byte(`{`)))

		server.HandleBatchUpdatePriority(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("empty updates", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-priority", []byte(`{"updates":[]}`)))

		server.HandleBatchUpdatePriority(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("success", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(http.MethodPost, "/admin/channels/batch-priority", map[string]any{
			"updates": []map[string]any{
				{"id": c1.ID, "priority": 100},
				{"id": c2.ID, "priority": 200},
			},
		}))

		server.HandleBatchUpdatePriority(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		updated1, _ := store.GetConfig(ctx, c1.ID)
		updated2, _ := store.GetConfig(ctx, c2.ID)
		if updated1.Priority != 100 || updated2.Priority != 200 {
			t.Fatalf("priority not updated: got (%d,%d)", updated1.Priority, updated2.Priority)
		}
	})
}
