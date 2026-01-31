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
		c, w := newTestContext(t, newJSONRequest(http.MethodPost, "/admin/channels/models/fetch", payload))

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
