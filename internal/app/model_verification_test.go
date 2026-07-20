package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"

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

func TestNewModelVerificationUsesEvidenceInsteadOfProof(t *testing.T) {
	verification := newModelVerification("gpt-5.5", "gpt-5.5", map[string]any{
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

	bridge := newModelVerification("gpt-5.5", "gpt-5.5", map[string]any{
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
	(&Server{}).attachModelVerification(context.Background(), "openai", "gpt-5.5", "gpt-5.5", "http://127.0.0.1:1", "sk-test", result)

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
	defer upstream.Close()

	result := map[string]any{
		"success":                true,
		"upstream_response_body": `{"model":"gpt-5.5"}`,
	}
	(&Server{}).attachModelVerification(context.Background(), "openai", "gpt-5.5", "gpt-5.5", upstream.URL, "sk-test", result)

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
	defer upstream.Close()

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
