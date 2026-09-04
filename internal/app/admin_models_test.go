package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
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

	t.Run("per-key discovery returns union and scoped results without secrets", func(t *testing.T) {
		perKeyUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Header.Get("Authorization") {
			case "Bearer sk-a":
				_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"common"}]}`))
			case "Bearer sk-b":
				_, _ = w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"common"}]}`))
			default:
				http.Error(w, "invalid api key sk-bad", http.StatusUnauthorized)
			}
		}))
		t.Cleanup(perKeyUpstream.Close)

		payload := map[string]any{
			"protocol": "openai",
			"urls":     []map[string]any{{"url": perKeyUpstream.URL, "protocols": []string{"openai"}}},
			"api_keys": []string{"sk-a", "sk-bad", "sk-b"},
			"per_key":  true,
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/fetch", payload))
		server.HandleFetchModelsPreview(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200 body=%s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "sk-a") || strings.Contains(w.Body.String(), "sk-b") {
			t.Fatalf("response leaked API key: %s", w.Body.String())
		}
		resp := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
		if !resp.Success || len(resp.Data.Models) != 3 || len(resp.Data.KeyModels) != 3 {
			t.Fatalf("unexpected per-key response: %s", w.Body.String())
		}
		if resp.Data.KeyModels[0].KeyIndex != 0 || len(resp.Data.KeyModels[0].Models) != 2 {
			t.Fatalf("key 0 result=%+v", resp.Data.KeyModels[0])
		}
		if resp.Data.KeyModels[1].KeyIndex != 1 || resp.Data.KeyModels[1].Error == "" {
			t.Fatalf("key 1 failure=%+v", resp.Data.KeyModels[1])
		}
		if resp.Data.KeyModels[2].KeyIndex != 2 || len(resp.Data.KeyModels[2].Models) != 2 {
			t.Fatalf("key 2 result=%+v", resp.Data.KeyModels[2])
		}
	})

	t.Run("per-key discovery fails when every key fails", func(t *testing.T) {
		failedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "invalid api key sk-secret", http.StatusUnauthorized)
		}))
		t.Cleanup(failedUpstream.Close)

		payload := map[string]any{
			"protocol": "openai",
			"urls":     []map[string]any{{"url": failedUpstream.URL, "protocols": []string{"openai"}}},
			"api_keys": []string{"sk-bad-a", "sk-bad-b"},
			"per_key":  true,
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/fetch", payload))
		server.HandleFetchModelsPreview(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200 body=%s", w.Code, w.Body.String())
		}
		resp := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
		if resp.Success || !strings.Contains(resp.Error, "所有 API Key 模型探测均失败") {
			t.Fatalf("unexpected response: %s", w.Body.String())
		}
		if strings.Contains(w.Body.String(), "sk-secret") {
			t.Fatalf("response leaked upstream error body: %s", w.Body.String())
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
			"profile":  model.ChannelManagementProfileSub2API,
			"base_url": baseURL,
			"api_key":  "sk-billing-test",
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

		server.HandleFetchKeyRate(c)
		if w.Code != http.StatusOK {
			t.Fatalf("baseURL=%q status=%d, want %d, body=%s", baseURL, w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool                 `json:"success"`
			Data    fetchKeyRateResponse `json:"data"`
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

func TestAdminModels_FetchNewAPIKeyRatePreview(t *testing.T) {
	var calls []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer management-pat" {
			t.Errorf("Authorization=%q, want management PAT", got)
		}
		if got := r.Header.Get("New-API-User"); got != "42" {
			t.Errorf("New-API-User=%q, want 42", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/token/search":
			if got := r.URL.Query().Get("token"); got != "new" {
				t.Errorf("token query=%q, want New API relay token prefix", got)
			}
			_, _ = w.Write([]byte(`{
				"success":true,
				"message":"",
				"data":{"items":[{"key":"abcd**********wxyz","group":"vip"}],"total":1}
			}`))
		case "/api/user/self/groups":
			_, _ = w.Write([]byte(`{
				"success":true,
				"message":"",
				"data":{"default":{"ratio":1,"desc":"Default"},"vip":{"ratio":0.75,"desc":"VIP"}}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()
	userID := int64(42)
	payload := map[string]any{
		"profile":      model.ChannelManagementProfileNewAPI,
		"base_url":     upstream.URL,
		"api_key":      "sk-new-api-key",
		"access_token": "management-pat",
		"user_id":      userID,
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

	server.HandleFetchKeyRate(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp struct {
		Success bool                 `json:"success"`
		Data    fetchKeyRateResponse `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success || resp.Data.EffectiveRateMultiplier != 0.75 {
		t.Fatalf("unexpected resp: %+v, body=%s", resp, w.Body.String())
	}
	wantCalls := []string{"/api/token/search", "/api/user/self/groups"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("upstream calls=%v, want %v", calls, wantCalls)
	}
	if strings.Contains(w.Body.String(), "management-pat") || strings.Contains(w.Body.String(), "sk-new-api-key") {
		t.Fatalf("response leaked a secret: %s", w.Body.String())
	}
}

func TestAdminModels_FetchNewAPIKeyRateRejectsAutoGroup(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/token/search" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{
			"success":true,
			"message":"",
			"data":{"items":[{"group":"auto"}],"total":1}
		}`))
	}))
	t.Cleanup(upstream.Close)

	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()
	payload := map[string]any{
		"profile":      model.ChannelManagementProfileNewAPI,
		"base_url":     upstream.URL,
		"api_key":      "sk-auto-key",
		"access_token": "management-pat",
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

	server.HandleFetchKeyRate(c)
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if resp.Success || resp.Data.Code != sub2APIBillingErrorUnsupported {
		t.Fatalf("unexpected resp: %+v, body=%s", resp, w.Body.String())
	}
}

func TestAdminModels_FetchNewAPIKeyRateUsesOwnerGroupForLegacyKey(t *testing.T) {
	var calls []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/token/search":
			_, _ = w.Write([]byte(`{
				"success":true,
				"message":"",
				"data":{"items":[{"group":""}],"total":1}
			}`))
		case "/api/user/self":
			_, _ = w.Write([]byte(`{
				"success":true,
				"message":"",
				"data":{"group":"default"}
			}`))
		case "/api/user/self/groups":
			_, _ = w.Write([]byte(`{
				"success":true,
				"message":"",
				"data":{"default":{"ratio":1.25,"desc":"Default"}}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()
	payload := map[string]any{
		"profile":      model.ChannelManagementProfileNewAPI,
		"base_url":     upstream.URL,
		"api_key":      "sk-legacy-key",
		"access_token": "management-pat",
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

	server.HandleFetchKeyRate(c)
	var resp struct {
		Success bool                 `json:"success"`
		Data    fetchKeyRateResponse `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success || resp.Data.EffectiveRateMultiplier != 1.25 {
		t.Fatalf("unexpected resp: %+v, body=%s", resp, w.Body.String())
	}
	wantCalls := []string{"/api/token/search", "/api/user/self", "/api/user/self/groups"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("upstream calls=%v, want %v", calls, wantCalls)
	}
}

func TestAdminModels_FetchNewAPIKeyRateRejectsNullRatio(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/token/search":
			_, _ = w.Write([]byte(`{
				"success":true,
				"message":"",
				"data":{"items":[{"group":"vip"}],"total":1}
			}`))
		case "/api/user/self/groups":
			_, _ = w.Write([]byte(`{
				"success":true,
				"message":"",
				"data":{"vip":{"ratio":null,"desc":"VIP"}}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()
	payload := map[string]any{
		"profile":      model.ChannelManagementProfileNewAPI,
		"base_url":     upstream.URL,
		"api_key":      "sk-null-ratio",
		"access_token": "management-pat",
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

	server.HandleFetchKeyRate(c)
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if resp.Success || resp.Data.Code != sub2APIBillingErrorInvalid {
		t.Fatalf("unexpected resp: %+v, body=%s", resp, w.Body.String())
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
			payload := map[string]any{
				"profile":  model.ChannelManagementProfileSub2API,
				"base_url": upstream.URL,
				"api_key":  "sk-request-secret",
			}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

			server.HandleFetchKeyRate(c)
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
	payload := map[string]any{
		"profile":  model.ChannelManagementProfileSub2API,
		"base_url": upstream.URL,
		"api_key":  "sk-redirect-secret",
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/billing/fetch", payload))

	server.HandleFetchKeyRate(c)
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

func TestAdminModels_HandleFetchModels_AntigravityOAuth(t *testing.T) {
	const accessToken = "antigravity-access-token-that-must-not-leak"
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:fetchAvailableModels" {
			t.Fatalf("request = %s %s", r.Method, r.URL.String())
		}
		if r.URL.RawQuery != "" || strings.Contains(r.URL.String(), accessToken) {
			t.Fatalf("模型发现 URL 泄漏凭证: %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+accessToken {
			t.Fatalf("Authorization = %q", got)
		}
		var request struct {
			Project string `json:"project"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.Project != "project-models" {
			t.Fatalf("project = %q", request.Project)
		}
		_, _ = w.Write([]byte(`{"models":{
			"claude-opus-4-6-thinking":{},
			"claude-sonnet-4-6":{},
			"gemini-3.8-flash":{},
			"gemini-3.8-flash-high":{},
			"gemini-3.8-flash-medium":{},
			"gemini-3.7-flash":{},
			"gemini-3.7-flash-high":{},
			"gemini-3.6-flash-high":{},
			"gemini-3-flash":{},
			"gemini-3-flash-agent":{},
			"gemini-3.1-flash-image":{},
			"gemini-pro-agent":{},
			"gemini-3.1-pro-low":{},
			"gpt-oss-120b-medium":{},
			"gemini-3.1-flash-lite":{},
			"gemini-3.5-flash-low":{},
			"gemini-3.5-flash-extra-low":{},
			"gemini-2.5-flash":{}
		}}`))
	}))
	t.Cleanup(upstream.Close)

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.antigravityService = antigravityauth.NewService(upstream.Client())
	server.antigravityCredentials = newAntigravityCredentialManager(server.antigravityService, store, nil, nil)

	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: accessToken, RefreshToken: "refresh-token",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Email: "models@example.com", ProjectID: "project-models",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "Antigravity models", AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: payload,
		URLs:         model.ChannelURLs{{URL: upstream.URL, Protocols: []string{"gemini"}}},
		ModelEntries: []model.ModelEntry{{Model: "existing-model"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/models/fetch", cfg.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}
	server.HandleFetchModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), accessToken) {
		t.Fatalf("response leaked OAuth token: %s", w.Body.String())
	}
	resp := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
	want := []model.ModelEntry{
		{Model: "claude-opus-4-6-thinking", RedirectModel: "claude-opus-4-6-thinking"},
		{Model: "claude-sonnet-4-6", RedirectModel: "claude-sonnet-4-6"},
		{Model: "gemini-3-flash", RedirectModel: "gemini-3-flash"},
		{Model: "gemini-3-flash-agent", RedirectModel: "gemini-3-flash-agent"},
		{Model: "gemini-3.1-flash-image", RedirectModel: "gemini-3.1-flash-image"},
		{Model: "gemini-3.1-flash-lite", RedirectModel: "gemini-3.1-flash-lite"},
		{Model: "gemini-3.1-pro-low", RedirectModel: "gemini-3.1-pro-low"},
		{Model: "gemini-3.5-flash-extra-low", RedirectModel: "gemini-3.5-flash-extra-low"},
		{Model: "gemini-3.5-flash-low", RedirectModel: "gemini-3.5-flash-low"},
		{Model: "gemini-3.6-flash-high", RedirectModel: "gemini-3.6-flash-high"},
		{Model: "gemini-3.7-flash", RedirectModel: "gemini-3.7-flash"},
		{Model: "gemini-3.7-flash-high", RedirectModel: "gemini-3.7-flash-high"},
		{Model: "gemini-3.8-flash", RedirectModel: "gemini-3.8-flash"},
		{Model: "gemini-3.8-flash-high", RedirectModel: "gemini-3.8-flash-high"},
		{Model: "gemini-3.8-flash-medium", RedirectModel: "gemini-3.8-flash-medium"},
		{Model: "gemini-pro-agent", RedirectModel: "gemini-pro-agent"},
		{Model: "gpt-oss-120b-medium", RedirectModel: "gpt-oss-120b-medium"},
	}
	if !resp.Success || !reflect.DeepEqual(resp.Data.Models, want) || resp.Data.Protocol != "gemini" {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}

	batchRequest := map[string]any{"channel_ids": []int64{cfg.ID}, "mode": "replace"}
	batchContext, batchResponse := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/refresh-batch", batchRequest))
	server.HandleBatchRefreshModels(batchContext)
	var batchResult struct {
		Success bool `json:"success"`
		Data    struct {
			Updated int `json:"updated"`
			Failed  int `json:"failed"`
		} `json:"data"`
	}
	mustUnmarshalJSON(t, batchResponse.Body.Bytes(), &batchResult)
	if batchResponse.Code != http.StatusOK || !batchResult.Success || batchResult.Data.Updated != 1 || batchResult.Data.Failed != 0 {
		t.Fatalf("unexpected batch response: %s", batchResponse.Body.String())
	}
	persisted, err := store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantPersisted := make([]string, len(want))
	for i, entry := range want {
		wantPersisted[i] = entry.Model
	}
	if !reflect.DeepEqual(persisted.GetModels(), wantPersisted) {
		t.Fatalf("persisted models = %#v, want %#v", persisted.GetModels(), wantPersisted)
	}
}

func TestAdminModels_HandleFetchModels_AnthropicOAuthIncludesFable51(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)

	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "Anthropic models", AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: "deliberately-not-json",
		URLs:         model.ChannelURLs{{URL: "https://api.anthropic.com", Protocols: []string{"anthropic"}}},
		ModelEntries: []model.ModelEntry{{Model: "existing-model"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/models/fetch", cfg.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}
	server.HandleFetchModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
	if !resp.Success || resp.Data.Protocol != "anthropic" || resp.Data.Source != "predefined" {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
	for _, entry := range resp.Data.Models {
		if entry.Model == "claude-fable-5-1" && entry.RedirectModel == "claude-fable-5-1" {
			return
		}
	}
	t.Fatalf("claude-fable-5-1 missing from fetched models: %#v", resp.Data.Models)
}

func TestAdminModels_HandleFetchModels_AntigravityCapacityDoesNotCooldownURLs(t *testing.T) {
	const capacityBody = `{"error":{"code":503,"message":"No capacity available for model claude-sonnet-4-6 on the server","status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED","domain":"cloudcode-pa.googleapis.com","metadata":{"error_number":"2010","model":"claude-sonnet-4-6"}}]}}`
	var calls atomic.Int32
	newCapacityUpstream := func() *testHTTPServer {
		return newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path != "/v1internal:fetchAvailableModels" {
				t.Fatalf("request path = %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, capacityBody)
		}))
	}
	first := newCapacityUpstream()
	second := newCapacityUpstream()
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.urlSelector = NewURLSelector()
	server.antigravityService = antigravityauth.NewService(first.Client())
	server.antigravityCredentials = newAntigravityCredentialManager(server.antigravityService, store, nil, nil)

	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "at-capacity", RefreshToken: "rt-capacity",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Email: "capacity@example.com", ProjectID: "project-capacity",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "Antigravity capacity models", AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: payload,
		URLs: model.ChannelURLs{
			{URL: first.URL, Protocols: []string{"gemini"}},
			{URL: second.URL, Protocols: []string{"gemini"}},
		},
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-6"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/models/fetch", cfg.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}
	server.HandleFetchModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &response)
	if response.Success || !strings.Contains(response.Error, "MODEL_CAPACITY_EXHAUSTED") {
		t.Fatalf("capacity response was not preserved: %+v", response)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model fetch calls=%d, want 2", got)
	}
	for _, rawURL := range []string{first.URL, second.URL} {
		if server.urlSelector.IsCooledDown(cfg.ID, rawURL) {
			t.Fatalf("model capacity exhaustion must not cool URL %s", rawURL)
		}
	}
}

func TestAdminModels_HandleFetchModels_CodexOAuth(t *testing.T) {
	const accessToken = "codex-access-token-that-must-not-leak"
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.codexCredentials = newCodexCredentialManager(codexauth.NewService(server.client), store, nil, nil)

	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: accessToken, RefreshToken: "refresh-token",
		Expired: time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339), AccountID: "account-models", PlanType: "free",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "Codex models", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: payload,
		URLs:         model.ChannelURLs{{URL: codexUpstreamURL, Exact: true, Protocols: []string{"codex"}}},
		ModelEntries: []model.ModelEntry{{Model: "existing-model"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/models/fetch", cfg.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}
	server.HandleFetchModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), accessToken) {
		t.Fatalf("response leaked OAuth token: %s", w.Body.String())
	}
	want := []model.ModelEntry{
		{Model: "codex-auto-review", RedirectModel: "codex-auto-review"},
		{Model: "gpt-5.4-mini", RedirectModel: "gpt-5.4-mini"},
		{Model: "gpt-5.5", RedirectModel: "gpt-5.5"},
		{Model: "gpt-5.6-luna", RedirectModel: "gpt-5.6-luna"},
		{Model: "gpt-5.6-terra", RedirectModel: "gpt-5.6-terra"},
		{Model: "gpt-image-1.5", RedirectModel: "gpt-image-1.5"},
		{Model: "gpt-image-2", RedirectModel: "gpt-image-2"},
	}
	resp := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
	if !resp.Success || !reflect.DeepEqual(resp.Data.Models, want) || resp.Data.Protocol != "codex" || resp.Data.Source != "predefined" {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}

	batchRequest := map[string]any{"channel_ids": []int64{cfg.ID}, "mode": "replace"}
	batchContext, batchResponse := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/refresh-batch", batchRequest))
	server.HandleBatchRefreshModels(batchContext)
	var batchResult struct {
		Success bool `json:"success"`
		Data    struct {
			Updated int `json:"updated"`
			Failed  int `json:"failed"`
		} `json:"data"`
	}
	mustUnmarshalJSON(t, batchResponse.Body.Bytes(), &batchResult)
	if batchResponse.Code != http.StatusOK || !batchResult.Success || batchResult.Data.Updated != 1 || batchResult.Data.Failed != 0 {
		t.Fatalf("unexpected batch response: %s", batchResponse.Body.String())
	}
	persisted, err := store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantPersisted := make([]string, len(want))
	for i, entry := range want {
		wantPersisted[i] = entry.Model
	}
	if !reflect.DeepEqual(persisted.GetModels(), wantPersisted) {
		t.Fatalf("persisted models = %#v, want %#v", persisted.GetModels(), wantPersisted)
	}
}

func TestAdminModels_HandleFetchModels_XAIOAuthUsesFixedCatalog(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)

	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xAI models", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: "deliberately-not-json",
		URLs:         model.ChannelURLs{{URL: "https://unreachable.invalid", Protocols: []string{"codex"}}},
		ModelEntries: []model.ModelEntry{{Model: "old-model"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/models/fetch?protocol=codex", cfg.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}
	server.HandleFetchModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	wantNames := []string{
		"grok-3-mini",
		"grok-3-mini-fast",
		"grok-4.20-0309-non-reasoning",
		"grok-4.20-0309-reasoning",
		"grok-4.20-multi-agent-0309",
		"grok-4.3",
		"grok-4.5",
		"grok-4.6",
		"grok-build-0.1",
		"grok-composer-2.5-fast",
		"grok-imagine-image",
		"grok-imagine-image-2.0",
		"grok-imagine-image-quality",
	}
	response := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
	if !response.Success || response.Data.Protocol != "codex" || response.Data.Source != "predefined" {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
	gotNames := make([]string, len(response.Data.Models))
	for i, entry := range response.Data.Models {
		gotNames[i] = entry.Model
		if entry.RedirectModel != entry.Model {
			t.Fatalf("model[%d]=%+v, want identity redirect", i, entry)
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("models=%v, want=%v", gotNames, wantNames)
	}
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

func TestAdminModels_HandleFetchModels_AllKeysCoolingUsesEarliestRecovery(t *testing.T) {
	var gotAuth []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4"}]}`))
	}))
	t.Cleanup(upstream.Close)

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "all-keys-cooling-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL, Protocols: []string{"openai"}}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "existing-model"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "sk-late", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "sk-soon", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: cfg.ID, KeyIndex: 2, APIKey: "sk-soon-higher-index", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	lateRecovery := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	soonRecovery := time.Now().Add(2 * time.Minute).Truncate(time.Second)
	if err := store.SetKeyCooldown(ctx, cfg.ID, 0, lateRecovery); err != nil {
		t.Fatalf("SetKeyCooldown(0) failed: %v", err)
	}
	if err := store.SetKeyCooldown(ctx, cfg.ID, 1, soonRecovery); err != nil {
		t.Fatalf("SetKeyCooldown(1) failed: %v", err)
	}
	if err := store.SetKeyCooldown(ctx, cfg.ID, 2, soonRecovery); err != nil {
		t.Fatalf("SetKeyCooldown(2) failed: %v", err)
	}
	before, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys before fetch failed: %v", err)
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
	wantAuth := []string{"Bearer sk-soon"}
	if !reflect.DeepEqual(gotAuth, wantAuth) {
		t.Fatalf("Authorization sequence=%v, want %v", gotAuth, wantAuth)
	}

	after, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys after fetch failed: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("key count changed after model fetch: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if after[i].CooldownUntil != before[i].CooldownUntil {
			t.Fatalf("key %d cooldown changed after model fetch: before=%d after=%d", before[i].KeyIndex, before[i].CooldownUntil, after[i].CooldownUntil)
		}
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

	t.Run("replace mode preserves disabled state by routing model name", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6-luna"}]}`))
		}))
		t.Cleanup(upstream.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:         "routing-identity-channel",
			URLs:         model.ChannelURLs{{URL: upstream.URL}},
			ModelEntries: []model.ModelEntry{{Model: "gpt-5.6-luna(max)", Disabled: true}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig failed: %v", err)
		}
		if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
			ChannelID: cfg.ID, KeyIndex: 0, APIKey: "k", KeyStrategy: model.KeyStrategySequential,
		}}); err != nil {
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
		if len(got.ModelEntries) != 1 || got.ModelEntries[0].Model != "gpt-5.6-luna" || !got.ModelEntries[0].Disabled {
			t.Fatalf("disabled routing identity was lost after replace: %#v", got.ModelEntries)
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

	t.Run("replace mode unions per-key models across groups", func(t *testing.T) {
		var upstreamAuth []string
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			auth := r.Header.Get("Authorization")
			upstreamAuth = append(upstreamAuth, auth)
			switch auth {
			case "Bearer group-a":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"ma-1"}]}`))
			case "Bearer group-b":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"mb-1"}]}`))
			default:
				http.Error(w, "invalid api key", http.StatusUnauthorized)
			}
		}))
		t.Cleanup(upstream.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:         "multi-group-channel",
			URLs:         model.ChannelURLs{{URL: upstream.URL, Protocols: []string{"openai"}}},
			Priority:     1,
			ModelEntries: []model.ModelEntry{{Model: "stale-only"}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig failed: %v", err)
		}
		// group-a 正常可用；group-b 作用域被裁剪空而自动禁用（凭据仍有效）；
		// manual-off 手动禁用，不得参与探测。
		if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "group-a", KeyStrategy: model.KeyStrategySequential},
			{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "group-b", KeyStrategy: model.KeyStrategySequential, Disabled: true, ModelScopeEmpty: true},
			{ChannelID: cfg.ID, KeyIndex: 2, APIKey: "manual-off", KeyStrategy: model.KeyStrategySequential, Disabled: true},
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
		wantModels := []model.ModelEntry{{Model: "ma-1"}, {Model: "mb-1"}}
		if !reflect.DeepEqual(got.ModelEntries, wantModels) {
			t.Fatalf("models=%#v, want %#v", got.ModelEntries, wantModels)
		}

		wantAuth := []string{"Bearer group-a", "Bearer group-b"}
		if !reflect.DeepEqual(upstreamAuth, wantAuth) {
			t.Fatalf("Authorization sequence=%v, want %v", upstreamAuth, wantAuth)
		}

		keys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetAPIKeys failed: %v", err)
		}
		if !reflect.DeepEqual(keys[0].AllowedModels, []string{"ma-1"}) || keys[0].Disabled || keys[0].ModelScopeEmpty {
			t.Fatalf("successful group-a key scope=%+v, want ma-1 and enabled", keys[0])
		}
		if !reflect.DeepEqual(keys[1].AllowedModels, []string{"mb-1"}) || keys[1].Disabled || keys[1].ModelScopeEmpty {
			t.Fatalf("successful scope-auto-disabled key must recover its scope: %+v", keys[1])
		}
		if !keys[2].Disabled {
			t.Fatalf("manually disabled key must stay disabled: %+v", keys[2])
		}
	})

	t.Run("replace mode continues with successful keys when one key probe fails", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			switch r.Header.Get("Authorization") {
			case "Bearer healthy":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
			case "Bearer broken":
				http.Error(w, "temporary upstream failure", http.StatusInternalServerError)
			default:
				http.Error(w, "invalid api key", http.StatusUnauthorized)
			}
		}))
		t.Cleanup(upstream.Close)

		server, store, cleanup := setupAdminTestServer(t)
		defer cleanup()

		ctx := context.Background()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:         "partial-probe-channel",
			URLs:         model.ChannelURLs{{URL: upstream.URL, Protocols: []string{"openai"}}},
			Priority:     1,
			ModelEntries: []model.ModelEntry{{Model: "model-a"}, {Model: "model-b"}, {Model: "stale-model"}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig failed: %v", err)
		}
		if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "healthy", AllowedModels: []string{"model-a"}, KeyStrategy: model.KeyStrategySequential},
			{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "broken", AllowedModels: []string{"model-b"}, KeyStrategy: model.KeyStrategySequential},
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

		var response struct {
			Success bool `json:"success"`
			Data    struct {
				Updated int                      `json:"updated"`
				Failed  int                      `json:"failed"`
				Results []BatchRefreshModelsItem `json:"results"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &response)
		if !response.Success || response.Data.Updated != 1 || response.Data.Failed != 0 ||
			len(response.Data.Results) != 1 || response.Data.Results[0].Status != "updated" || response.Data.Results[0].Warning == "" {
			t.Fatalf("unexpected response: %s", w.Body.String())
		}

		got, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		want := []model.ModelEntry{{Model: "model-a"}}
		if !reflect.DeepEqual(got.ModelEntries, want) {
			t.Fatalf("models=%#v, want %#v (successful model should apply despite failed key)", got.ModelEntries, want)
		}
		keys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetAPIKeys failed: %v", err)
		}
		if !reflect.DeepEqual(keys[0].AllowedModels, []string{"model-a"}) || keys[0].ModelScopeEmpty || keys[0].Disabled {
			t.Fatalf("healthy key scope=%+v, want model-a and enabled", keys[0])
		}
		if len(keys[1].AllowedModels) != 0 || !keys[1].ModelScopeEmpty || !keys[1].Disabled {
			t.Fatalf("failed key must become an explicit empty scope: %+v", keys[1])
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

func TestAvailableModelFetchAPIKeysScopeEmptyFallback(t *testing.T) {
	now := time.Now()
	keys := []*model.APIKey{
		{KeyIndex: 0, APIKey: "manual-off", Disabled: true},
		{KeyIndex: 1, APIKey: "scope-empty", Disabled: true, ModelScopeEmpty: true},
		{KeyIndex: 2, APIKey: "cooling-normal", CooldownUntil: now.Add(5 * time.Minute).Unix()},
		{KeyIndex: 3, APIKey: "normal"},
		{KeyIndex: 4, APIKey: "scope-empty-cooling", Disabled: true, ModelScopeEmpty: true, CooldownUntil: now.Add(5 * time.Minute).Unix()},
	}
	got := availableModelFetchAPIKeys(keys, now)
	gotNames := make([]string, 0, len(got))
	for _, key := range got {
		gotNames = append(gotNames, key.APIKey)
	}
	want := []string{"normal", "scope-empty"}
	if !reflect.DeepEqual(gotNames, want) {
		t.Fatalf("keys=%v, want %v", gotNames, want)
	}

	allCooling := []*model.APIKey{
		{KeyIndex: 0, APIKey: "cool-late", CooldownUntil: now.Add(10 * time.Minute).Unix()},
		{KeyIndex: 1, APIKey: "cool-soon", CooldownUntil: now.Add(3 * time.Minute).Unix()},
		{KeyIndex: 2, APIKey: "scope-empty", Disabled: true, ModelScopeEmpty: true},
	}
	got = availableModelFetchAPIKeys(allCooling, now)
	gotNames = gotNames[:0]
	for _, key := range got {
		gotNames = append(gotNames, key.APIKey)
	}
	want = []string{"cool-soon", "scope-empty"}
	if !reflect.DeepEqual(gotNames, want) {
		t.Fatalf("all-cooling keys=%v, want %v", gotNames, want)
	}
}
