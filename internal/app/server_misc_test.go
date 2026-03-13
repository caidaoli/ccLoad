package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestServer_SetupRoutes_CORSPreflightBypassesAuth(t *testing.T) {
	srv := newInMemoryServer(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow-origin=%q, want empty", got)
	}
}

func TestServer_SetupRoutes_CORSHeadersOnAuthFailure(t *testing.T) {
	srv := newInMemoryServer(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://example.com")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow-origin=%q, want empty", got)
	}
}

func TestServer_SetupRoutes_V1BetaCORSPreflightBypassesAuth(t *testing.T) {
	srv := newInMemoryServer(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	req := httptest.NewRequest(http.MethodOptions, "/v1beta/models", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow-origin=%q, want empty", got)
	}
}

func TestServer_SetupRoutes_V1BetaCORSHeadersOnAuthFailure(t *testing.T) {
	srv := newInMemoryServer(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models", nil)
	req.Header.Set("Origin", "https://example.com")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow-origin=%q, want empty", got)
	}
}

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

func TestServer_ShutdownCancelsInFlightURLProbe(t *testing.T) {
	srv := newInMemoryServer(t)

	srv.urlSelector.probeTimeout = 5 * time.Second

	started := make(chan struct{}, 2)
	srv.urlSelector.probeDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	channelID := int64(1)
	urls := []string{"https://a.example", "https://b.example"}

	probeDone := make(chan struct{})
	go func() {
		srv.urlSelector.ProbeURLs(srv.baseCtx, channelID, urls)
		close(probeDone)
	}()

	for range len(urls) {
		select {
		case <-started:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("probe dials did not start in time")
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	select {
	case <-probeDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ProbeURLs did not exit promptly after shutdown")
	}

	for _, u := range urls {
		if srv.urlSelector.IsCooledDown(channelID, u) {
			t.Fatalf("expected canceled probe not to cooldown url: %s", u)
		}
	}

	srv.urlSelector.mu.RLock()
	probingLeft := len(srv.urlSelector.probing)
	srv.urlSelector.mu.RUnlock()
	if probingLeft != 0 {
		t.Fatalf("expected probing markers cleared after shutdown cancellation, got %d", probingLeft)
	}
}
