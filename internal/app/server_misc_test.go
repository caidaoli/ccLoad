package app

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

type deadlineRecorderResponseWriter struct {
	header         http.Header
	body           bytes.Buffer
	statusCode     int
	writeDeadline  time.Time
	deadlineCalled bool
}

func (w *deadlineRecorderResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *deadlineRecorderResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func (w *deadlineRecorderResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *deadlineRecorderResponseWriter) SetWriteDeadline(t time.Time) error {
	w.deadlineCalled = true
	w.writeDeadline = t
	return nil
}

func TestDisableResponseWriteTimeoutClearsDeadline(t *testing.T) {
	t.Parallel()

	w := &deadlineRecorderResponseWriter{}
	disableResponseWriteTimeout(w, "非流式")

	if !w.deadlineCalled {
		t.Fatal("SetWriteDeadline was not called")
	}
	if !w.writeDeadline.IsZero() {
		t.Fatalf("writeDeadline=%v, want zero time", w.writeDeadline)
	}
}

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

	s.streamTimeout = 600 * time.Second
	if got := s.GetWriteTimeout(); got != 600*time.Second {
		t.Fatalf("GetWriteTimeout()=%v, want 600s", got)
	}
}

func TestServer_GetWriteTimeout_IncludesProtocolNonStreamTimeout(t *testing.T) {
	t.Parallel()

	s := &Server{
		nonStreamTimeout: 10 * time.Second,
		protocolTimeouts: map[string]protocolTimeoutConfig{
			util.ProtocolOpenAI: {NonStreamTimeout: 300 * time.Second},
		},
	}

	if got := s.GetWriteTimeout(); got != 300*time.Second {
		t.Fatalf("GetWriteTimeout()=%v, want 300s", got)
	}
}

func TestServer_ResolveProtocolTimeouts(t *testing.T) {
	t.Parallel()

	s := &Server{
		firstByteTimeout: 90 * time.Second,
		nonStreamTimeout: 120 * time.Second,
		protocolTimeouts: map[string]protocolTimeoutConfig{
			util.ProtocolAnthropic: {
				FirstByteTimeout: 11 * time.Second,
				NonStreamTimeout: 12 * time.Second,
			},
			util.ProtocolOpenAI: {
				FirstByteTimeout: 21 * time.Second,
				NonStreamTimeout: 22 * time.Second,
			},
		},
	}

	localPlan := protocol.TransformPlan{
		ClientProtocol:   protocol.OpenAI,
		UpstreamProtocol: protocol.Anthropic,
	}
	localTimeouts := s.resolveProtocolTimeouts(localPlan)
	if localTimeouts.FirstByteTimeout != 11*time.Second || localTimeouts.NonStreamTimeout != 12*time.Second {
		t.Fatalf("local timeouts=%+v, want anthropic bucket", localTimeouts)
	}

	upstreamPlan := protocol.TransformPlan{
		ClientProtocol:   protocol.OpenAI,
		UpstreamProtocol: protocol.OpenAI,
	}
	upstreamTimeouts := s.resolveProtocolTimeouts(upstreamPlan)
	if upstreamTimeouts.FirstByteTimeout != 21*time.Second || upstreamTimeouts.NonStreamTimeout != 22*time.Second {
		t.Fatalf("upstream timeouts=%+v, want openai bucket", upstreamTimeouts)
	}
}

func TestServer_ResolveProtocolTimeouts_ZeroProtocolOverrideFallsBackToGlobal(t *testing.T) {
	t.Parallel()

	s := &Server{
		firstByteTimeout: 90 * time.Second,
		nonStreamTimeout: 120 * time.Second,
		protocolTimeouts: map[string]protocolTimeoutConfig{
			util.ProtocolCodex: {},
		},
	}
	plan := protocol.TransformPlan{UpstreamProtocol: protocol.Codex}

	timeouts := s.resolveProtocolTimeouts(plan)
	if timeouts.FirstByteTimeout != 90*time.Second || timeouts.NonStreamTimeout != 120*time.Second {
		t.Fatalf("timeouts=%+v, want global fallback", timeouts)
	}
}

func TestNewServer_ZeroNonStreamTimeoutDisablesTimeout(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := store.UpdateSetting(ctx, "non_stream_timeout", "0"); err != nil {
		_ = store.Close()
		t.Fatalf("UpdateSetting failed: %v", err)
	}

	srv := NewServer(store)
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Server.Shutdown failed: %v", err)
		}
	})

	if srv.nonStreamTimeout != 0 {
		t.Fatalf("nonStreamTimeout=%v, want 0", srv.nonStreamTimeout)
	}
}

func TestNewServer_LoadsProtocolTimeoutOverrides(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := store.UpdateSetting(ctx, "openai_first_byte_timeout", "9"); err != nil {
		_ = store.Close()
		t.Fatalf("UpdateSetting openai_first_byte_timeout failed: %v", err)
	}
	if err := store.UpdateSetting(ctx, "openai_non_stream_timeout", "33"); err != nil {
		_ = store.Close()
		t.Fatalf("UpdateSetting openai_non_stream_timeout failed: %v", err)
	}

	srv := NewServer(store)
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Server.Shutdown failed: %v", err)
		}
	})

	got := srv.protocolTimeouts[util.ProtocolOpenAI]
	if got.FirstByteTimeout != 9*time.Second || got.NonStreamTimeout != 33*time.Second {
		t.Fatalf("openai timeouts=%+v, want 9s/33s", got)
	}
}

func TestServer_GetConfig_FallbackToStore(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name:         "ch",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
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

func TestServer_HandleChannelKeys(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.store = store

	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name:         "ch",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
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

func TestServer_HandleChannelKeysProjectsOAuthAccessTokens(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	tests := []struct {
		name       string
		authType   string
		credential string
		wantToken  string
		wantNote   string
	}{
		{
			name:       "codex",
			authType:   model.AuthTypeCodexOAuth,
			credential: `{"type":"codex","access_token":"codex-access","refresh_token":"codex-refresh","expired":"2030-01-01T00:00:00Z"}`,
			wantToken:  "codex-access",
			wantNote:   "Codex OAuth AT",
		},
		{
			name:       "antigravity",
			authType:   model.AuthTypeAntigravityOAuth,
			credential: `{"type":"antigravity","access_token":"gravity-access","refresh_token":"gravity-refresh","expired":"2030-01-01T00:00:00Z"}`,
			wantToken:  "gravity-access",
			wantNote:   "Antigravity OAuth AT",
		},
		{
			name:       "xai",
			authType:   model.AuthTypeXAIOAuth,
			credential: `{"type":"xai","auth_kind":"oauth","access_token":"xai-access","refresh_token":"xai-refresh","expired":"2030-01-01T00:00:00Z"}`,
			wantToken:  "xai-access",
			wantNote:   "xAI OAuth AT",
		},
		{
			name:       "anthropic",
			authType:   model.AuthTypeAnthropicOAuth,
			credential: `{"type":"anthropic","access_token":"anthropic-access","refresh_token":"anthropic-refresh","expired":"2030-01-01T00:00:00Z","account_uuid":"account-1"}`,
			wantToken:  "anthropic-access",
			wantNote:   "Anthropic OAuth AT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := store.CreateConfig(context.Background(), &model.Config{
				Name:            tt.name,
				AuthType:        tt.authType,
				OAuthCredential: tt.credential,
				URLs:            model.ChannelURLs{{URL: "https://api.example.com"}},
				Enabled:         true,
			})
			if err != nil {
				t.Fatalf("CreateConfig failed: %v", err)
			}

			id := strconv.FormatInt(cfg.ID, 10)
			c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/"+id+"/keys", nil))
			c.Params = gin.Params{{Key: "id", Value: id}}
			server.HandleChannelKeys(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
			}
			resp := mustParseAPIResponse[[]*model.APIKey](t, w.Body.Bytes())
			if !resp.Success || len(resp.Data) != 1 {
				t.Fatalf("response=%+v, want one OAuth key", resp)
			}
			key := resp.Data[0]
			if key.KeyIndex != 0 || key.APIKey != util.MaskAPIKey(tt.wantToken) || key.Note != tt.wantNote || key.Disabled {
				t.Fatalf("key=%+v, want token=%q note=%q", key, tt.wantToken, tt.wantNote)
			}
			if strings.Contains(w.Body.String(), tt.wantToken) {
				t.Fatalf("OAuth access token leaked in channel keys response: %s", w.Body.String())
			}
			stored, err := store.GetAPIKeys(context.Background(), cfg.ID)
			if err != nil || len(stored) != 0 {
				t.Fatalf("projected OAuth key must not be persisted: keys=%+v err=%v", stored, err)
			}
		})
	}
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
