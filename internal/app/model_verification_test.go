package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestExtractReportedModel(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "openai json",
			body: `{"id":"chatcmpl_test","model":"gpt-5.5","choices":[]}`,
			want: "gpt-5.5",
		},
		{
			name: "gemini json",
			body: `{"modelVersion":"gemini-3-pro-preview","candidates":[]}`,
			want: "gemini-3-pro-preview",
		},
		{
			name: "codex sse",
			body: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5.3-codex-spark\"}}\n\ndata: [DONE]\n",
			want: "gpt-5.3-codex-spark",
		},
		{
			name: "anthropic sse",
			body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-6\"}}\n",
			want: "claude-sonnet-4-6",
		},
		{
			name: "missing model",
			body: `{"id":"response_without_model"}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractReportedModel(tt.body); got != tt.want {
				t.Fatalf("extractReportedModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompareModelIdentity(t *testing.T) {
	tests := []struct {
		name      string
		protocol  string
		effective string
		reported  string
		want      modelIdentityRelation
	}{
		{
			name:      "same OpenAI name",
			protocol:  "openai",
			effective: "gpt-5.5",
			reported:  "GPT-5.5",
			want:      modelIdentityExact,
		},
		{
			name:      "Gemini latest resolves to concrete revision",
			protocol:  "gemini",
			effective: "gemini-2.5-pro-latest",
			reported:  "gemini-2.5-pro-001",
			want:      modelIdentityProviderAlias,
		},
		{
			name:      "Gemini model resource prefix",
			protocol:  "gemini",
			effective: "gemini-2.5-pro-latest",
			reported:  "models/gemini-2.5-pro-001",
			want:      modelIdentityProviderAlias,
		},
		{
			name:      "Gemini latest resolves to preview revision",
			protocol:  "gemini",
			effective: "gemini-2.5-pro-latest",
			reported:  "gemini-2.5-pro-preview-2025-06-05",
			want:      modelIdentityProviderAlias,
		},
		{
			name:      "Anthropic latest resolves to dated release",
			protocol:  "anthropic",
			effective: "claude-3-5-sonnet-latest",
			reported:  "claude-3-5-sonnet-20241022",
			want:      modelIdentityProviderAlias,
		},
		{
			name:      "different concrete revisions remain unknown",
			protocol:  "gemini",
			effective: "gemini-2.5-pro-001",
			reported:  "gemini-2.5-pro-002",
			want:      modelIdentityUnknown,
		},
		{
			name:      "opaque gateway aliases are unknown",
			protocol:  "openai",
			effective: "gateway-alias-a",
			reported:  "gateway-target-b",
			want:      modelIdentityUnknown,
		},
		{
			name:      "different known OpenAI model lines conflict",
			protocol:  "openai",
			effective: "gpt-5.5",
			reported:  "gpt-5.3-codex-spark",
			want:      modelIdentityConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compareModelIdentity(tt.protocol, tt.effective, tt.reported); got != tt.want {
				t.Fatalf("compareModelIdentity() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestModelCatalogContainsProviderAlias(t *testing.T) {
	snapshot := modelVerificationCatalogSnapshot{
		models: map[string]struct{}{
			"gemini-2.5-pro-001": {},
		},
	}
	if !modelCatalogContains(snapshot, "gemini", "gemini-2.5-pro-latest") {
		t.Fatal("Gemini latest alias should match the concrete catalog entry")
	}
}

func TestNewModelVerificationUsesEvidenceInsteadOfProof(t *testing.T) {
	verification := newModelVerification("openai", "gpt-5.5", "gpt-5.5", map[string]any{
		"upstream_response_body": `{"model":"gpt-5.3-codex-spark"}`,
	})
	if verification.Verdict != modelVerificationVerdictMismatch {
		t.Fatalf("verdict = %q, want %q", verification.Verdict, modelVerificationVerdictMismatch)
	}
	if verification.ReportedModel != "gpt-5.3-codex-spark" {
		t.Fatalf("reported model = %q", verification.ReportedModel)
	}
	if verification.Limitation != "metadata_is_not_proof" {
		t.Fatalf("limitation = %q", verification.Limitation)
	}

	alias := newModelVerification("gemini", "gemini-2.5-pro-latest", "gemini-2.5-pro-latest", map[string]any{
		"upstream_response_body": `{"modelVersion":"gemini-2.5-pro-001"}`,
	})
	if alias.Verdict != modelVerificationVerdictConsistent {
		t.Fatalf("alias verdict = %q, want %q", alias.Verdict, modelVerificationVerdictConsistent)
	}

	unknown := newModelVerification("openai", "gateway-alias-a", "gateway-alias-a", map[string]any{
		"upstream_response_body": `{"model":"gateway-target-b"}`,
	})
	if unknown.Verdict != modelVerificationVerdictUnverified {
		t.Fatalf("unknown verdict = %q, want %q", unknown.Verdict, modelVerificationVerdictUnverified)
	}

	bridge := newModelVerification("openai", "gpt-5.5", "gpt-5.5", map[string]any{
		"base_url": "https://chatgpt.com/backend-api/codex/responses",
	})
	if bridge.Source != modelVerificationSourceLikelyWebBridge {
		t.Fatalf("source = %q, want %q", bridge.Source, modelVerificationSourceLikelyWebBridge)
	}
	if bridge.SourceConfidence != 90 {
		t.Fatalf("source confidence = %d, want 90", bridge.SourceConfidence)
	}
}

func TestAttachModelVerificationSkipsCatalogAfterFailedRequest(t *testing.T) {
	result := map[string]any{
		"success": false,
		"error":   "upstream unavailable",
	}
	(&Server{}).attachModelVerification(context.Background(), &model.Config{ID: 1}, "openai", "gpt-5.5", "gpt-5.5", "http://127.0.0.1:1", "sk-test", result)

	verification, ok := result["model_verification"].(*modelVerification)
	if !ok {
		t.Fatalf("model_verification type = %T", result["model_verification"])
	}
	if verification.Catalog.Attempted {
		t.Fatal("catalog probe must not run after a failed channel test")
	}
}

func TestAttachModelVerificationKeepsSuccessWhenCatalogUnavailable(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	result := map[string]any{
		"success":                true,
		"upstream_response_body": `{"model":"gpt-5.5"}`,
	}
	srv.attachModelVerification(context.Background(), &model.Config{ID: 1}, "openai", "gpt-5.5", "gpt-5.5", upstream.URL, "sk-test", result)

	if success, _ := result["success"].(bool); !success {
		t.Fatalf("catalog failure changed the channel test result: %+v", result)
	}
	verification, ok := result["model_verification"].(*modelVerification)
	if !ok {
		t.Fatalf("model_verification type = %T", result["model_verification"])
	}
	if !verification.Catalog.Attempted || verification.Catalog.Available || verification.Catalog.ErrorCode != "unavailable" {
		t.Fatalf("catalog result = %+v", verification.Catalog)
	}
}

func TestAttachModelVerificationDoesNotProbeExactUpstreamURL(t *testing.T) {
	var catalogRequests atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogRequests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	result := map[string]any{
		"success":                true,
		"upstream_response_body": `{"model":"gpt-5.5"}`,
	}
	srv.attachModelVerification(
		context.Background(),
		&model.Config{ID: 1},
		"openai",
		"gpt-5.5",
		"gpt-5.5",
		upstream.URL+"/v1/chat/completions#",
		"sk-test",
		result,
	)

	verification := result["model_verification"].(*modelVerification)
	if !verification.Catalog.Attempted || verification.Catalog.ErrorCode != "unsupported" {
		t.Fatalf("catalog result = %+v", verification.Catalog)
	}
	if got := catalogRequests.Load(); got != 0 {
		t.Fatalf("catalog requests = %d, want 0", got)
	}
}

func TestModelVerificationCatalogUsesChannelProxy(t *testing.T) {
	var proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		if r.Method != http.MethodGet || r.URL.Scheme != "http" || r.URL.Host != "catalog.invalid" || r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected proxied request: method=%s url=%s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5"}]}`))
	}))
	defer proxy.Close()

	srv := newInMemoryServer(t)
	result := map[string]any{
		"success":                true,
		"upstream_response_body": `{"model":"gpt-5.5"}`,
	}
	srv.attachModelVerification(context.Background(), &model.Config{
		ID:       1,
		ProxyURL: proxy.URL,
	}, "openai", "gpt-5.5", "gpt-5.5", "http://catalog.invalid", "sk-test", result)

	verification := result["model_verification"].(*modelVerification)
	if !verification.Catalog.Available {
		t.Fatalf("catalog result = %+v", verification.Catalog)
	}
	if got := proxyRequests.Load(); got != 1 {
		t.Fatalf("proxy requests = %d, want 1", got)
	}
}

func TestModelVerificationCatalogRespectsChannelRPMLimit(t *testing.T) {
	var catalogRequests atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			catalogRequests.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	cfg := &model.Config{ID: 1, RPMLimit: 1}
	normalReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new normal request: %v", err)
	}
	normalResp, err := srv.doUpstreamRequest(cfg, normalReq)
	if err != nil {
		t.Fatalf("normal channel request: %v", err)
	}
	if normalResp == nil || normalResp.Body == nil {
		t.Fatal("normal channel request returned no body")
	}
	_ = normalResp.Body.Close()

	result := map[string]any{
		"success":                true,
		"upstream_response_body": `{"model":"gpt-5.5"}`,
	}
	srv.attachModelVerification(context.Background(), cfg, "openai", "gpt-5.5", "gpt-5.5", upstream.URL, "sk-test", result)

	if success, _ := result["success"].(bool); !success {
		t.Fatalf("catalog limit changed channel test result: %+v", result)
	}
	verification := result["model_verification"].(*modelVerification)
	if verification.Catalog.ErrorCode != "rate_limited" {
		t.Fatalf("catalog error = %q, want rate_limited", verification.Catalog.ErrorCode)
	}
	if got := catalogRequests.Load(); got != 0 {
		t.Fatalf("catalog requests = %d, want 0", got)
	}
}

func TestModelVerificationCatalogRespectsChannelConcurrencyLimit(t *testing.T) {
	var catalogRequests atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	cfg := &model.Config{ID: 1, MaxConcurrency: 1}
	release, err := srv.acquireChannelConcurrencySlot(cfg)
	if err != nil {
		t.Fatalf("acquire channel concurrency slot: %v", err)
	}
	defer release()

	result := map[string]any{
		"success":                true,
		"upstream_response_body": `{"model":"gpt-5.5"}`,
	}
	srv.attachModelVerification(context.Background(), cfg, "openai", "gpt-5.5", "gpt-5.5", upstream.URL, "sk-test", result)

	verification := result["model_verification"].(*modelVerification)
	if verification.Catalog.ErrorCode != "concurrency_limited" {
		t.Fatalf("catalog error = %q, want concurrency_limited", verification.Catalog.ErrorCode)
	}
	if got := catalogRequests.Load(); got != 0 {
		t.Fatalf("catalog requests = %d, want 0", got)
	}
}

func TestModelVerificationCatalogCachesBatchProbe(t *testing.T) {
	var catalogRequests atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		catalogRequests.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5"}]}`))
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	cfg := &model.Config{ID: 1}
	const batchSize = 12
	start := make(chan struct{})
	verifications := make(chan *modelVerification, batchSize)
	var wg sync.WaitGroup

	for range batchSize {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result := map[string]any{
				"success":                true,
				"upstream_response_body": `{"model":"gpt-5.5"}`,
			}
			srv.attachModelVerification(context.Background(), cfg, "openai", "gpt-5.5", "gpt-5.5", upstream.URL, "sk-test", result)
			verifications <- result["model_verification"].(*modelVerification)
		}()
	}
	close(start)
	wg.Wait()
	close(verifications)

	for verification := range verifications {
		if !verification.Catalog.Available {
			t.Fatalf("catalog result = %+v", verification.Catalog)
		}
	}
	if got := catalogRequests.Load(); got != 1 {
		t.Fatalf("catalog requests = %d, want 1", got)
	}

	result := map[string]any{
		"success":                true,
		"upstream_response_body": `{"model":"gpt-5.5"}`,
	}
	srv.attachModelVerification(context.Background(), cfg, "openai", "gpt-5.5", "gpt-5.5", upstream.URL, "sk-next-key", result)
	if got := catalogRequests.Load(); got != 2 {
		t.Fatalf("catalog requests after credential change = %d, want 2", got)
	}
}

func TestHandleChannelTestModelVerification(t *testing.T) {
	modelCatalogRequests := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			modelCatalogRequests++
			if got := r.Header.Get("Authorization"); got != "Bearer sk-test-key" {
				t.Errorf("catalog authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl_test","object":"chat.completion","model":"gpt-5.3-codex-spark","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "model-verification",
		URL:          upstream.URL,
		ChannelType:  "openai",
		Priority:     1,
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.5"}},
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: created.ID,
		KeyIndex:  0,
		APIKey:    "sk-test-key",
	}}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":        "gpt-5.5",
		"channel_type": "openai",
		"verify_model": true,
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}
	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); !success {
		t.Fatalf("test should remain successful, data=%+v", response.Data)
	}
	verification, ok := response.Data["model_verification"].(map[string]any)
	if !ok {
		t.Fatalf("model_verification = %T, data=%+v", response.Data["model_verification"], response.Data)
	}
	if got, _ := verification["verdict"].(string); got != modelVerificationVerdictMismatch {
		t.Fatalf("verdict = %q, want %q", got, modelVerificationVerdictMismatch)
	}
	if got, _ := verification["reported_model"].(string); got != "gpt-5.3-codex-spark" {
		t.Fatalf("reported_model = %q", got)
	}
	catalog, ok := verification["catalog"].(map[string]any)
	if !ok {
		t.Fatalf("catalog = %T", verification["catalog"])
	}
	if available, _ := catalog["available"].(bool); !available {
		t.Fatalf("catalog should be available: %+v", catalog)
	}
	if listed, _ := catalog["effective_model_listed"].(bool); !listed {
		t.Fatalf("effective model should be listed: %+v", catalog)
	}
	if modelCatalogRequests != 1 {
		t.Fatalf("catalog requests = %d, want 1", modelCatalogRequests)
	}

	c, w = newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":        "gpt-5.5",
		"channel_type": "openai",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}
	srv.HandleChannelTest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("default test status = %d, body=%s", w.Code, w.Body.String())
	}
	defaultResponse := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if _, exists := defaultResponse.Data["model_verification"]; exists {
		t.Fatalf("model verification must stay opt-in, data=%+v", defaultResponse.Data)
	}
	if modelCatalogRequests != 1 {
		t.Fatalf("default test made an unexpected catalog request; got %d, want 1", modelCatalogRequests)
	}
}
