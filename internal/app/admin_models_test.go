package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

func TestAdminModels_FetchModelsPreview(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`))
	}))
	t.Cleanup(upstream.Close)

	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	t.Run("invalid request", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/models/fetch", []byte(`{}`)))

		server.HandleFetchModelsPreview(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("success", func(t *testing.T) {
		payload := map[string]any{
			"channel_type": " openai ",
			"url":          upstream.URL,
			"api_key":      "sk-test",
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/fetch", payload))

		server.HandleFetchModelsPreview(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool                `json:"success"`
			Data    FetchModelsResponse `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success || resp.Data.Source != "api" || len(resp.Data.Models) != 2 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		if resp.Data.Models[0].RedirectModel != resp.Data.Models[0].Model {
			t.Fatalf("expected redirect_model filled, got %+v", resp.Data.Models[0])
		}
		if gotAuth != "Bearer sk-test" {
			t.Fatalf("Authorization=%q, want %q", gotAuth, "Bearer sk-test")
		}
	})
}

func TestAdminModels_HandleFetchModels(t *testing.T) {
	// upstream: 先返回成功，再返回错误
	var call int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		call++
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
			return
		}
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	// 需要 channelCache
	server.channelCache = storage.NewChannelCache(store, time.Minute)

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "c1",
		URL:          upstream.URL,
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "sk-test", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	t.Run("success", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/1/models/fetch", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleFetchModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp struct {
			Success bool                `json:"success"`
			Data    FetchModelsResponse `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success || len(resp.Data.Models) != 1 || resp.Data.Models[0].Model != "gpt-4o" {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})

	t.Run("upstream error returns 200 with success=false", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/1/models/fetch", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleFetchModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
		}
		var resp struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if resp.Success || resp.Error == "" {
			t.Fatalf("expected success=false with error, got %+v", resp)
		}
	})
}

func TestAdminModels_HandleBatchRefreshModels(t *testing.T) {
	t.Run("merge mode partial success", func(t *testing.T) {
		// channel1: 返回 m1,m2（新增1个）
		upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
		}))
		t.Cleanup(upstream1.Close)

		// channel2: 返回 x1（无变化）
		upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"x1"}]}`))
		}))
		t.Cleanup(upstream2.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		c1, err := store.CreateConfig(ctx, &model.Config{
			Name:         "c1",
			URL:          upstream1.URL,
			Priority:     1,
			ChannelType:  "openai",
			ModelEntries: []model.ModelEntry{{Model: "m1"}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig c1 failed: %v", err)
		}
		c2, err := store.CreateConfig(ctx, &model.Config{
			Name:         "c2",
			URL:          upstream2.URL,
			Priority:     1,
			ChannelType:  "openai",
			ModelEntries: []model.ModelEntry{{Model: "x1"}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig c2 failed: %v", err)
		}
		c3, err := store.CreateConfig(ctx, &model.Config{
			Name:         "c3-no-key",
			URL:          upstream2.URL,
			Priority:     1,
			ChannelType:  "openai",
			ModelEntries: []model.ModelEntry{{Model: "y1"}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig c3 failed: %v", err)
		}

		if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: c1.ID, KeyIndex: 0, APIKey: "k1", KeyStrategy: model.KeyStrategySequential},
			{ChannelID: c2.ID, KeyIndex: 0, APIKey: "k2", KeyStrategy: model.KeyStrategySequential},
		}); err != nil {
			t.Fatalf("CreateAPIKeysBatch failed: %v", err)
		}

		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/refresh-batch", map[string]any{
			"channel_ids": []int64{c1.ID, c2.ID, c3.ID},
			"mode":        "merge",
		}))
		server.HandleBatchRefreshModels(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Updated   int `json:"updated"`
				Unchanged int `json:"unchanged"`
				Failed    int `json:"failed"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success {
			t.Fatalf("expected success=true, body=%s", w.Body.String())
		}
		if resp.Data.Updated != 1 || resp.Data.Unchanged != 1 || resp.Data.Failed != 1 {
			t.Fatalf("unexpected summary: %+v", resp.Data)
		}

		got1, err := store.GetConfig(ctx, c1.ID)
		if err != nil {
			t.Fatalf("GetConfig c1 failed: %v", err)
		}
		got2, err := store.GetConfig(ctx, c2.ID)
		if err != nil {
			t.Fatalf("GetConfig c2 failed: %v", err)
		}
		if len(got1.ModelEntries) != 2 {
			t.Fatalf("c1 model count=%d, want 2", len(got1.ModelEntries))
		}
		if len(got2.ModelEntries) != 1 {
			t.Fatalf("c2 model count=%d, want 1", len(got2.ModelEntries))
		}
	})

	t.Run("replace mode", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"new-1"}]}`))
		}))
		t.Cleanup(upstream.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:        "replace-channel",
			URL:         upstream.URL,
			Priority:    1,
			ChannelType: "openai",
			ModelEntries: []model.ModelEntry{
				{Model: "old-1"},
				{Model: "old-2"},
			},
			Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateConfig failed: %v", err)
		}
		if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "k", KeyStrategy: model.KeyStrategySequential},
		}); err != nil {
			t.Fatalf("CreateAPIKeysBatch failed: %v", err)
		}

		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/refresh-batch", map[string]any{
			"channel_ids": []int64{cfg.ID},
			"mode":        "replace",
		}))
		server.HandleBatchRefreshModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		got, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		if len(got.ModelEntries) != 1 || got.ModelEntries[0].Model != "new-1" {
			t.Fatalf("unexpected models after replace: %#v", got.ModelEntries)
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		server, _, cleanup := setupAdminTestServer(t)
		defer cleanup()

		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/models/refresh-batch", []byte(`{"channel_ids":[1],"mode":"xxx"}`)))
		server.HandleBatchRefreshModels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})
}
