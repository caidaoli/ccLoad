package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestServer_GetWriteTimeout(t *testing.T) {
	t.Parallel()

	s := &Server{nonStreamTimeout: 10 * time.Second}
	if got := s.GetWriteTimeout(); got != 120*time.Second {
		t.Fatalf("GetWriteTimeout()=%v, want 120s", got)
	}

	s.nonStreamTimeout = 300 * time.Second
	if got := s.GetWriteTimeout(); got != 300*time.Second {
		t.Fatalf("GetWriteTimeout()=%v, want 300s", got)
	}
}

func TestServer_GetConfig_FallbackToStore(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name:         "ch",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	s := &Server{store: store}
	got, err := s.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if got.ID != cfg.ID || got.Name != "ch" {
		t.Fatalf("unexpected config: %+v", got)
	}
}

func TestServer_HandleEventLoggingBatch(t *testing.T) {
	t.Parallel()

	s := &Server{}
	c, w := newTestContext(t, newRequest(http.MethodPost, "/api/event_logging/batch", nil))

	s.HandleEventLoggingBatch(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "{}" {
		t.Fatalf("body=%q, want {}", w.Body.String())
	}
}

func TestServer_GetModelsByChannelType(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := store.CreateConfig(ctx, &model.Config{
		Name:         "a1",
		ChannelType:  "openai",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}, {Model: "m2"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig #1 failed: %v", err)
	}
	_, err = store.CreateConfig(ctx, &model.Config{
		Name:         "a2",
		ChannelType:  "openai",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m2"}, {Model: "m3"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig #2 failed: %v", err)
	}
	_, err = store.CreateConfig(ctx, &model.Config{
		Name:         "b1",
		ChannelType:  "gemini",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "x1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig #3 failed: %v", err)
	}

	server.store = store

	models, err := server.getModelsByChannelType(ctx, "openai")
	if err != nil {
		t.Fatalf("getModelsByChannelType failed: %v", err)
	}
	set := make(map[string]bool)
	for _, m := range models {
		set[m] = true
	}
	for _, must := range []string{"m1", "m2", "m3"} {
		if !set[must] {
			t.Fatalf("models missing %q: %v", must, models)
		}
	}
	if set["x1"] {
		t.Fatalf("unexpected model from other channel type: %v", models)
	}
}

func TestServer_HandleChannelKeys(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.store = store

	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name:         "ch",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{
		{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "sk-1", KeyStrategy: model.KeyStrategySequential}, //nolint:gosec
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	t.Run("invalid_id", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/abc/keys", nil))
		c.Params = gin.Params{{Key: "id", Value: "abc"}}

		server.HandleChannelKeys(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("ok", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/1/keys", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleChannelKeys(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		resp := mustParseAPIResponse[[]*model.APIKey](t, w.Body.Bytes())
		if !resp.Success {
			t.Fatalf("success=false, error=%q", resp.Error)
		}
		if resp.Data == nil || len(resp.Data) != 1 {
			t.Fatalf("keys=%v, want 1", len(resp.Data))
		}
	})
}
