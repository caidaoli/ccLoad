package app

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

func TestAdminModels_FetchModelsPreview(t *testing.T) {
	var gotAuth string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			"protocol": " openai ",
			"urls":     []map[string]any{{"url": upstream.URL}},
			"api_keys": []string{"sk-test"},
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

	t.Run("multiple keys fall back after key error", func(t *testing.T) {
		var authSequence []string
		multiKeyUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			authSequence = append(authSequence, auth)
			if auth == "Bearer sk-bad" {
				http.Error(w, "rate limit", http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4"}]}`))
		}))
		t.Cleanup(multiKeyUpstream.Close)

		payload := map[string]any{
			"protocol": "openai",
			"urls":     []map[string]any{{"url": multiKeyUpstream.URL, "protocols": []string{"openai"}}},
			"api_keys": []string{"sk-bad", "sk-good"},
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/fetch", payload))
		server.HandleFetchModelsPreview(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		resp := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
		if !resp.Success || len(resp.Data.Models) != 1 || resp.Data.Models[0].Model != "gpt-5.4" {
			t.Fatalf("unexpected response: %s", w.Body.String())
		}
		wantAuth := []string{"Bearer sk-bad", "Bearer sk-good"}
		if !reflect.DeepEqual(authSequence, wantAuth) {
			t.Fatalf("Authorization sequence=%v, want %v", authSequence, wantAuth)
		}
	})

	t.Run("normalization options preserve upstream model names", func(t *testing.T) {
		normalizationUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"source/OpenAI/GPT-4O"},{"id":"vendor/Claude-SONNET"}]}`))
		}))
		t.Cleanup(normalizationUpstream.Close)

		payload := map[string]any{
			"protocol":                  "openai",
			"urls":                      []map[string]any{{"url": normalizationUpstream.URL}},
			"api_keys":                  []string{"sk-test"},
			"lowercase_models":          true,
			"strip_model_source_prefix": true,
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
		want := []model.ModelEntry{
			{Model: "gpt-4o", RedirectModel: "source/OpenAI/GPT-4O"},
			{Model: "claude-sonnet", RedirectModel: "vendor/Claude-SONNET"},
		}
		if !resp.Success || !reflect.DeepEqual(resp.Data.Models, want) {
			t.Fatalf("models=%#v, want %#v, body=%s", resp.Data.Models, want, w.Body.String())
		}
	})

}

func TestAdminModels_FetchSub2APIBillingPreview(t *testing.T) {
	var gotAuth string
	var gotAccept string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sub2api/billing" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object":"sub2api.key_billing",
			"schema_version":1,
			"billing_scope":"token",
			"group_rate_multiplier":1.2,
			"user_rate_multiplier":0.8,
			"resolved_rate_multiplier":0.8,
			"peak_rate_enabled":true,
			"effective_rate_multiplier":1.2,
			"observed_at":"2026-08-02T10:00:00Z"
		}`))
	}))
	t.Cleanup(upstream.Close)

	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	for _, baseURL := range []string{upstream.URL, upstream.URL + "/v1/"} {
		payload := map[string]any{
			"base_url": baseURL,
			"api_key":  "sk-billing-test",
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

		server.HandleFetchSub2APIBilling(c)
		if w.Code != http.StatusOK {
			t.Fatalf("baseURL=%q status=%d, want %d, body=%s", baseURL, w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool                        `json:"success"`
			Data    fetchSub2APIBillingResponse `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success || resp.Data.EffectiveRateMultiplier != 1.2 {
			t.Fatalf("baseURL=%q unexpected resp: %+v", baseURL, resp)
		}
	}

	if gotAuth != "Bearer sk-billing-test" {
		t.Fatalf("Authorization=%q, want %q", gotAuth, "Bearer sk-billing-test")
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept=%q, want application/json", gotAccept)
	}
}

func TestAdminModels_FetchSub2APIBillingRejectsUntrustedResponses(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode string
	}{
		{
			name:     "invalid key",
			status:   http.StatusUnauthorized,
			body:     `{"error":{"message":"sk-upstream-secret"}}`,
			wantCode: sub2APIBillingErrorAuthentication,
		},
		{
			name:     "unsupported upstream",
			status:   http.StatusNotFound,
			body:     `not a Sub2API server`,
			wantCode: sub2APIBillingErrorUnsupported,
		},
		{
			name:     "key without billing group",
			status:   http.StatusForbidden,
			body:     `{"error":{"type":"permission_error"}}`,
			wantCode: sub2APIBillingErrorPermission,
		},
		{
			name:     "method unsupported",
			status:   http.StatusMethodNotAllowed,
			body:     `method not allowed`,
			wantCode: sub2APIBillingErrorUnsupported,
		},
		{
			name:   "inconsistent resolved rate",
			status: http.StatusOK,
			body: `{
				"object":"sub2api.key_billing",
				"schema_version":1,
				"billing_scope":"token",
				"group_rate_multiplier":0.5,
				"resolved_rate_multiplier":0.8,
				"effective_rate_multiplier":0.8,
				"observed_at":"2026-08-02T10:00:00Z"
			}`,
			wantCode: sub2APIBillingErrorInvalid,
		},
		{
			name:   "negative effective rate",
			status: http.StatusOK,
			body: `{
				"object":"sub2api.key_billing",
				"schema_version":1,
				"billing_scope":"token",
				"group_rate_multiplier":0.5,
				"resolved_rate_multiplier":0.5,
				"effective_rate_multiplier":-1,
				"observed_at":"2026-08-02T10:00:00Z"
			}`,
			wantCode: sub2APIBillingErrorInvalid,
		},
		{
			name:   "invalid observation time",
			status: http.StatusOK,
			body: `{
				"object":"sub2api.key_billing",
				"schema_version":1,
				"billing_scope":"token",
				"group_rate_multiplier":0.5,
				"resolved_rate_multiplier":0.5,
				"effective_rate_multiplier":0.5,
				"observed_at":"yesterday"
			}`,
			wantCode: sub2APIBillingErrorInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(upstream.Close)

			server, _, cleanup := setupAdminTestServer(t)
			defer cleanup()
			payload := map[string]any{"base_url": upstream.URL, "api_key": "sk-request-secret"}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

			server.HandleFetchSub2APIBilling(c)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
			}
			var resp struct {
				Success bool `json:"success"`
				Data    struct {
					Code string `json:"code"`
				} `json:"data"`
			}
			mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
			if resp.Success || resp.Data.Code != tt.wantCode {
				t.Fatalf("unexpected resp: %+v, body=%s", resp, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "sk-upstream-secret") || strings.Contains(w.Body.String(), "sk-request-secret") {
				t.Fatalf("response leaked a secret: %s", w.Body.String())
			}
		})
	}
}

func TestAdminModels_FetchSub2APIBillingDoesNotFollowRedirects(t *testing.T) {
	redirectTargetCalled := false
	redirectTarget := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object":"sub2api.key_billing",
			"schema_version":1,
			"billing_scope":"token",
			"group_rate_multiplier":0.5,
			"resolved_rate_multiplier":0.5,
			"effective_rate_multiplier":0.5,
			"observed_at":"2026-08-02T10:00:00Z"
		}`))
	}))
	t.Cleanup(redirectTarget.Close)

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/v1/sub2api/billing", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(upstream.Close)

	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()
	payload := map[string]any{"base_url": upstream.URL, "api_key": "sk-redirect-secret"}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

	server.HandleFetchSub2APIBilling(c)
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if resp.Success || resp.Data.Code != sub2APIBillingErrorAPI {
		t.Fatalf("unexpected resp: %+v, body=%s", resp, w.Body.String())
	}
	if redirectTargetCalled {
		t.Fatal("billing probe followed an upstream redirect")
	}
}

func TestAdminModels_HandleFetchModels(t *testing.T) {
	// upstream: 先返回成功，再返回错误
	var call int
	var gotAuth string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		call++
		gotAuth = r.Header.Get("Authorization")
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
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "sk-disabled", KeyStrategy: model.KeyStrategySequential, Disabled: true},
		{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "sk-test", KeyStrategy: model.KeyStrategySequential},
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
		if gotAuth != "Bearer sk-test" {
			t.Fatalf("Authorization=%q, want %q", gotAuth, "Bearer sk-test")
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

func TestAdminModels_HandleFetchModels_MultiKeyFallback(t *testing.T) {
	var gotAuth []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		gotAuth = append(gotAuth, auth)
		if auth == "Bearer sk-bad" {
			http.Error(w, "invalid api key", http.StatusUnauthorized)
			return
		}
		if auth != "Bearer sk-good" {
			http.Error(w, "unexpected api key", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4"}]}`))
	}))
	t.Cleanup(upstream.Close)

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "multi-key-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL, Protocols: []string{"openai"}}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "existing-model"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "sk-cooling", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "sk-bad", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: cfg.ID, KeyIndex: 2, APIKey: "sk-good", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	if err := store.SetKeyCooldown(ctx, cfg.ID, 0, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/1/models/fetch", nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}
	server.HandleFetchModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
	if !resp.Success || len(resp.Data.Models) != 1 || resp.Data.Models[0].Model != "gpt-5.4" {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
	wantAuth := []string{"Bearer sk-bad", "Bearer sk-good"}
	if !reflect.DeepEqual(gotAuth, wantAuth) {
		t.Fatalf("Authorization sequence=%v, want %v", gotAuth, wantAuth)
	}
}

func TestAdminModels_HandleFetchModels_MultiURL(t *testing.T) {
	failCalls := 0
	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(failUpstream.Close)

	okCalls := 0
	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		time.Sleep(15 * time.Millisecond)
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4.1"}]}`))
	}))
	t.Cleanup(okUpstream.Close)

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.urlSelector = NewURLSelector()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "multi-url-channel",
		URLs:         channelURLsForTest(failUpstream.URL, okUpstream.URL),
		Priority:     1,
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
	// 强制第一跳命中失败URL，确保触发fallback与反馈逻辑
	server.urlSelector.CooldownURL(cfg.ID, okUpstream.URL)

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/1/models/fetch", nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}

	server.HandleFetchModels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Success bool                `json:"success"`
		Data    FetchModelsResponse `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success {
		t.Fatalf("expected success=true, body=%s", w.Body.String())
	}
	if len(resp.Data.Models) != 1 || resp.Data.Models[0].Model != "gpt-4.1" {
		t.Fatalf("unexpected models: %+v", resp.Data.Models)
	}
	if failCalls < 1 || okCalls < 1 {
		t.Fatalf("expected both URLs attempted, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
	if !server.urlSelector.IsCooledDown(cfg.ID, failUpstream.URL) {
		t.Fatalf("expected failed URL cooled down, url=%s", failUpstream.URL)
	}
	latency, exists := server.urlSelector.latencies[urlKey{channelID: cfg.ID, url: okUpstream.URL}]
	if !exists || latency == nil || latency.value <= 0 {
		t.Fatalf("expected success URL latency recorded, got=%v", latency)
	}
}

func TestAdminModels_HandleFetchModels_MultiURL_KeyErrorDoesNotCooldownURL(t *testing.T) {
	keyErrCalls := 0
	keyErrUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyErrCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	t.Cleanup(keyErrUpstream.Close)

	okCalls := 0
	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4.1"}]}`))
	}))
	t.Cleanup(okUpstream.Close)

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.urlSelector = NewURLSelector()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "multi-url-key-error",
		URLs:         channelURLsForTest(keyErrUpstream.URL, okUpstream.URL),
		Priority:     1,
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
	// 强制首跳优先命中 keyErrUpstream，覆盖“先401再fallback”的路径。
	server.urlSelector.CooldownURL(cfg.ID, okUpstream.URL)

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/1/models/fetch", nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}

	server.HandleFetchModels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Success bool                `json:"success"`
		Data    FetchModelsResponse `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success {
		t.Fatalf("expected success=true, body=%s", w.Body.String())
	}
	if keyErrCalls < 1 || okCalls < 1 {
		t.Fatalf("expected both URLs attempted, keyErrCalls=%d okCalls=%d", keyErrCalls, okCalls)
	}
	if server.urlSelector.IsCooledDown(cfg.ID, keyErrUpstream.URL) {
		t.Fatalf("expected key-error URL not cooled down, url=%s", keyErrUpstream.URL)
	}
}

func TestAdminModels_HandleBatchRefreshModels(t *testing.T) {
	t.Run("merge mode partial success", func(t *testing.T) {
		// channel1: 返回 m1,m2（新增1个）
		var upstream1Auth []string
		upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			auth := r.Header.Get("Authorization")
			upstream1Auth = append(upstream1Auth, auth)
			if auth == "Bearer bad-k1" {
				http.Error(w, "invalid api key", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
		}))
		t.Cleanup(upstream1.Close)

		// channel2: 返回 x1（无变化）
		upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			URLs:         model.ChannelURLs{{URL: upstream1.URL, Protocols: []string{"openai"}}},
			Priority:     1,
			ModelEntries: []model.ModelEntry{{Model: "m1"}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig c1 failed: %v", err)
		}
		c2, err := store.CreateConfig(ctx, &model.Config{
			Name:         "c2",
			URLs:         model.ChannelURLs{{URL: upstream2.URL}},
			Priority:     1,
			ModelEntries: []model.ModelEntry{{Model: "x1"}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig c2 failed: %v", err)
		}
		c3, err := store.CreateConfig(ctx, &model.Config{
			Name:         "c3-no-key",
			URLs:         model.ChannelURLs{{URL: upstream2.URL}},
			Priority:     1,
			ModelEntries: []model.ModelEntry{{Model: "y1"}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig c3 failed: %v", err)
		}

		if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: c1.ID, KeyIndex: 0, APIKey: "disabled-k1", KeyStrategy: model.KeyStrategySequential, Disabled: true},
			{ChannelID: c1.ID, KeyIndex: 1, APIKey: "bad-k1", KeyStrategy: model.KeyStrategySequential},
			{ChannelID: c1.ID, KeyIndex: 2, APIKey: "k1", KeyStrategy: model.KeyStrategySequential},
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
		wantAuth := []string{"Bearer bad-k1", "Bearer k1"}
		if !reflect.DeepEqual(upstream1Auth, wantAuth) {
			t.Fatalf("Authorization sequence=%v, want %v", upstream1Auth, wantAuth)
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

	t.Run("merge mode skips models already used as redirect targets", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"UPSTREAM-MODEL"}]}`))
		}))
		t.Cleanup(upstream.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:         "redirect-dedup-channel",
			URLs:         model.ChannelURLs{{URL: upstream.URL}},
			Priority:     1,
			ModelEntries: []model.ModelEntry{{Model: "client-alias", RedirectModel: "upstream-model"}},
			Enabled:      true,
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
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success || resp.Data.Updated != 0 || resp.Data.Unchanged != 1 {
			t.Fatalf("unexpected response: %+v body=%s", resp, w.Body.String())
		}

		got, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		want := []model.ModelEntry{{Model: "client-alias", RedirectModel: "upstream-model"}}
		if !reflect.DeepEqual(got.ModelEntries, want) {
			t.Fatalf("models=%#v, want %#v", got.ModelEntries, want)
		}
	})

	t.Run("replace mode", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			Name:     "replace-channel",
			URLs:     model.ChannelURLs{{URL: upstream.URL}},
			Priority: 1,
			ModelEntries: []model.ModelEntry{
				{Model: "new-1", Disabled: true},
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
		if len(got.ModelEntries) != 1 || got.ModelEntries[0].Model != "new-1" || !got.ModelEntries[0].Disabled {
			t.Fatalf("unexpected models after replace: %#v", got.ModelEntries)
		}
	})

	t.Run("replace mode lowercases aliases and preserves upstream model names", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"CamelCase-Model"},{"id":"already-lower"}]}`))
		}))
		t.Cleanup(upstream.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:                  "lowercase-channel",
			URLs:                  model.ChannelURLs{{URL: upstream.URL}},
			Priority:              1,
			ModelEntries:          []model.ModelEntry{{Model: "CamelCase-Model"}},
			ScheduledCheckEnabled: true,
			ScheduledCheckModel:   "CamelCase-Model",
			Enabled:               true,
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
			"channel_ids":      []int64{cfg.ID},
			"mode":             "replace",
			"lowercase_models": true,
		}))
		server.HandleBatchRefreshModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		got, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		wantModels := []model.ModelEntry{
			{Model: "camelcase-model", RedirectModel: "CamelCase-Model"},
			{Model: "already-lower"},
		}
		if !reflect.DeepEqual(got.ModelEntries, wantModels) {
			t.Fatalf("models=%#v, want %#v", got.ModelEntries, wantModels)
		}
		if got.ScheduledCheckModel != "camelcase-model" {
			t.Fatalf("ScheduledCheckModel=%q, want %q", got.ScheduledCheckModel, "camelcase-model")
		}
	})

	t.Run("replace mode strips source prefixes with stable collision handling", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"cloudcompile/Grok-4.5"},{"id":"z-source/Other-Model"},{"id":"x-ai/grok-4.5"},{"id":"a-source/Other-Model"},{"id":"grok-4.5"}]}`))
		}))
		t.Cleanup(upstream.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:                  "strip-prefix-channel",
			URLs:                  model.ChannelURLs{{URL: upstream.URL}},
			Priority:              1,
			ModelEntries:          []model.ModelEntry{{Model: "cloudcompile/Grok-4.5", Disabled: true}},
			ScheduledCheckEnabled: true,
			ScheduledCheckModel:   "cloudcompile/Grok-4.5",
			Enabled:               true,
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
			"channel_ids":               []int64{cfg.ID},
			"mode":                      "replace",
			"lowercase_models":          true,
			"strip_model_source_prefix": true,
		}))
		server.HandleBatchRefreshModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		got, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		wantModels := []model.ModelEntry{
			{Model: "grok-4.5", Disabled: true},
			{Model: "other-model", RedirectModel: "a-source/Other-Model"},
		}
		if !reflect.DeepEqual(got.ModelEntries, wantModels) {
			t.Fatalf("models=%#v, want %#v", got.ModelEntries, wantModels)
		}
		if got.ScheduledCheckModel != "grok-4.5" {
			t.Fatalf("ScheduledCheckModel=%q, want %q", got.ScheduledCheckModel, "grok-4.5")
		}
	})

	t.Run("merge mode normalizes existing aliases and preserves their mappings", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"source/ExistingModel"},{"id":"source/NewModel"}]}`))
		}))
		t.Cleanup(upstream.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:                  "lowercase-merge-channel",
			URLs:                  model.ChannelURLs{{URL: upstream.URL}},
			Priority:              1,
			ModelEntries:          []model.ModelEntry{{Model: "legacy/ExistingModel"}},
			ScheduledCheckEnabled: true,
			ScheduledCheckModel:   "legacy/ExistingModel",
			Enabled:               true,
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
			"channel_ids":               []int64{cfg.ID},
			"mode":                      "merge",
			"lowercase_models":          true,
			"strip_model_source_prefix": true,
		}))
		server.HandleBatchRefreshModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		got, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		wantModels := []model.ModelEntry{
			{Model: "existingmodel", RedirectModel: "legacy/ExistingModel"},
			{Model: "newmodel", RedirectModel: "source/NewModel"},
		}
		if !reflect.DeepEqual(got.ModelEntries, wantModels) {
			t.Fatalf("models=%#v, want %#v", got.ModelEntries, wantModels)
		}
		if got.ScheduledCheckModel != "existingmodel" {
			t.Fatalf("ScheduledCheckModel=%q, want %q", got.ScheduledCheckModel, "existingmodel")
		}
	})

	t.Run("empty upstream model list leaves channel unchanged", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		t.Cleanup(upstream.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:         "empty-list-channel",
			URLs:         model.ChannelURLs{{URL: upstream.URL}},
			Priority:     1,
			ModelEntries: []model.ModelEntry{{Model: "keep-me"}},
			Enabled:      true,
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

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Failed  int                      `json:"failed"`
				Results []BatchRefreshModelsItem `json:"results"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success || resp.Data.Failed != 1 || len(resp.Data.Results) != 1 || resp.Data.Results[0].Status != "failed" {
			t.Fatalf("unexpected response: %+v body=%s", resp, w.Body.String())
		}

		got, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		if !reflect.DeepEqual(got.ModelEntries, []model.ModelEntry{{Model: "keep-me"}}) {
			t.Fatalf("models changed after empty refresh: %#v", got.ModelEntries)
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
