package zaiauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestService(t *testing.T, handler http.Handler) (*Service, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	service := NewService(server.Client())
	service.OAuthBaseURL = server.URL + "/api/v1"
	service.BizBaseURL = server.URL
	service.CodingModelsURL = server.URL + "/api/coding/paas/v4/models"
	service.ModelsURL = server.URL + "/api/paas/v4/models"
	service.AgentConfigsURL = server.URL + "/api/v1/agent/configs"
	service.CommunityURL = server.URL + "/api.json"
	service.QuotaLimitURL = server.URL + "/api/monitor/usage/quota/limit"
	return service, server
}

func TestInitFlowAndPollFollowZCodeContract(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer poll-token" ||
			r.Header.Get("User-Agent") != "ZCode/"+AppVersion ||
			r.Header.Get("X-ZCode-App-Version") != AppVersion {
			t.Errorf("unexpected identity headers: %v", r.Header)
		}
		switch r.URL.Path {
		case "/api/v1/oauth/cli/init":
			body, _ := io.ReadAll(r.Body)
			var payload map[string]string
			if json.Unmarshal(body, &payload) != nil || payload["provider"] != OAuthProvider {
				t.Errorf("init body = %s", body)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"flow_id":"flow-1","poll_token":"poll-token","authorize_url":"https://zcode.z.ai/oauth/x","expires_at":1800000000,"poll_interval_sec":2}}`)
		case "/api/v1/oauth/cli/poll/flow-1":
			_, _ = io.WriteString(w, `{"code":0,"data":{"status":"ready","token":"jwt","user":{"user_id":"u-1","email":"user@example.com","name":"User"},"zai":{"access_token":"zai-access"}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))

	flow, err := service.InitFlow(context.Background(), "poll-token")
	if err != nil {
		t.Fatalf("InitFlow() error = %v", err)
	}
	if flow.FlowID != "flow-1" || flow.AuthorizeURL != "https://zcode.z.ai/oauth/x" || flow.PollIntervalSec != 2 {
		t.Fatalf("flow = %+v", flow)
	}
	result, err := service.Poll(context.Background(), flow.FlowID, "poll-token")
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if result.Status != PollReady || result.AccessToken != "zai-access" || result.JWTToken != "jwt" ||
		result.Identity.UserID != "u-1" || result.Identity.Email != "user@example.com" {
		t.Fatalf("result = %+v", result)
	}
}

func TestPollReportsPendingAndFailed(t *testing.T) {
	t.Parallel()
	status := "pending"
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"status":"`+status+`"}}`)
	}))
	result, err := service.Poll(context.Background(), "flow-1", "poll-token")
	if err != nil || result.Status != PollPending {
		t.Fatalf("pending poll = %+v err = %v", result, err)
	}
	status = "failed"
	result, err = service.Poll(context.Background(), "flow-1", "poll-token")
	if err != nil || result.Status != PollFailed {
		t.Fatalf("failed poll = %+v err = %v", result, err)
	}
}

// The hosted CLI OAuth endpoint is enabled per ZCode release. An empty 404 must
// surface as an upstream availability fact so the admin UI can offer the
// Coding Plan key path instead of reporting a ccLoad bug.
func TestInitFlowReportsUnavailableOnEmptyNotFound(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	_, err := service.InitFlow(context.Background(), "poll-token")
	if !errors.Is(err, ErrOAuthFlowUnavailable) {
		t.Fatalf("InitFlow() error = %v", err)
	}
}

func TestInitFlowRejectsBusinessErrors(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":1001,"msg":"provider disabled"}`)
	}))
	_, err := service.InitFlow(context.Background(), "poll-token")
	if err == nil || !strings.Contains(err.Error(), "provider disabled") {
		t.Fatalf("InitFlow() error = %v", err)
	}
}

func TestResolveCodingPlanAPIKeyReusesZCodeKey(t *testing.T) {
	t.Parallel()
	var createdKeys int
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/z/login":
			body, _ := io.ReadAll(r.Body)
			var payload map[string]string
			if json.Unmarshal(body, &payload) != nil || payload["token"] != "zai-access" {
				t.Errorf("login body = %s", body)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"biz-token"}}`)
		case r.URL.Path == "/api/biz/customer/getCustomerInfo":
			if r.Header.Get("Authorization") != "Bearer biz-token" {
				t.Errorf("customer info authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"userId":"u-1","email":"user@example.com","organizations":[{"organizationId":"org-other","organizationName":"Team","projects":[{"projectId":"p-x","projectName":"Other"}]},{"organizationId":"org-1","organizationName":"默认机构","projects":[{"projectId":"p-other","projectName":"Other"},{"projectId":"p-1","projectName":"默认项目"}]}]}}`)
		case r.URL.Path == "/api/biz/v1/organization/org-1/projects/p-1/api_keys" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"code":0,"data":[{"name":"other","apiKey":"other-id"},{"name":"zcode-api-key","apiKey":"key-id"}]}`)
		case r.URL.Path == "/api/biz/v1/organization/org-1/projects/p-1/api_keys" && r.Method == http.MethodPost:
			createdKeys++
			_, _ = io.WriteString(w, `{"code":0,"data":{"apiKey":"created-id"}}`)
		case r.URL.Path == "/api/biz/v1/organization/org-1/projects/p-1/api_keys/copy/key-id":
			_, _ = io.WriteString(w, `{"code":0,"data":{"secretKey":"secret"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	apiKey, identity, err := service.ResolveCodingPlanAPIKey(context.Background(), "zai-access")
	if err != nil {
		t.Fatalf("ResolveCodingPlanAPIKey() error = %v", err)
	}
	if apiKey != "key-id.secret" {
		t.Fatalf("api key = %q", apiKey)
	}
	if identity.UserID != "u-1" || identity.Email != "user@example.com" {
		t.Fatalf("identity = %+v", identity)
	}
	if createdKeys != 0 {
		t.Fatalf("existing ZCode key must be reused, created %d", createdKeys)
	}
}

func TestResolveCodingPlanAPIKeyCreatesMissingKey(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/z/login":
			_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"biz-token"}}`)
		case r.URL.Path == "/api/biz/customer/getCustomerInfo":
			_, _ = io.WriteString(w, `{"code":0,"data":{"organizations":[{"organizationId":"org-1","organizationName":"Org","projects":[{"projectId":"p-1","projectName":"Project"}]}]}}`)
		case strings.HasSuffix(r.URL.Path, "/api_keys") && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"code":0,"data":[]}`)
		case strings.HasSuffix(r.URL.Path, "/api_keys") && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "zcode-api-key") {
				t.Errorf("create body = %s", body)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"apiKey":"created-id"}}`)
		case strings.HasSuffix(r.URL.Path, "/copy/created-id"):
			_, _ = io.WriteString(w, `{"code":0,"data":{"secretKey":"created-secret"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	apiKey, _, err := service.ResolveCodingPlanAPIKey(context.Background(), "zai-access")
	if err != nil {
		t.Fatalf("ResolveCodingPlanAPIKey() error = %v", err)
	}
	if apiKey != "created-id.created-secret" {
		t.Fatalf("api key = %q", apiKey)
	}
}

// The Coding Plan catalog is authoritative and the general API only fills in
// what it misses: models reach the plan before the general API lists them.
func TestListModelsPrefersCodingPlanCatalog(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key.secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/coding/paas/v4/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"glm-5.3"},{"id":"glm-4.7"}]}`)
		case "/api/paas/v4/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"glm-4.7"},{"id":"glm-4.6"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	models, err := service.ListModels(context.Background(), "key.secret")
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 3 || models[0] != "glm-5.3" || models[1] != "glm-4.7" || models[2] != "glm-4.6" {
		t.Fatalf("models = %v", models)
	}
}

func TestListModelsSurvivesOneUnavailableCatalog(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/coding/paas/v4/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":[{"model":"glm-4.7"}]}`)
	}))
	models, err := service.ListModels(context.Background(), "key.secret")
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 1 || models[0] != "glm-4.7" {
		t.Fatalf("models = %v", models)
	}
}

// models.dev is the keyless tier: it tracks the plan lineup (glm-5.3 landed
// there on release day) and answers newest first.
func TestListCommunityModelsReadsCodingPlanProvider(t *testing.T) {
	t.Parallel()
	payload := `{"zai":{"models":{"glm-5.2":{"release_date":"2026-06-13"}}},` +
		`"zai-coding-plan":{"models":{` +
		`"glm-4.7":{"release_date":"2025-12-22"},` +
		`"glm-5.3":{"release_date":"2026-08-14"},` +
		`"glm-5.2":{"release_date":"2026-06-13"}}}}`
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	models, err := service.ListCommunityModels(context.Background())
	if err != nil {
		t.Fatalf("ListCommunityModels() error = %v", err)
	}
	if len(models) != 3 || models[0] != "glm-5.3" || models[1] != "glm-5.2" || models[2] != "glm-4.7" {
		t.Fatalf("models = %v", models)
	}
}

func TestParseCommunityCatalogRejectsMissingProvider(t *testing.T) {
	t.Parallel()
	if _, err := parseCommunityCatalog([]byte(`{"openai":{"models":{"gpt":{}}}}`), CommunityCatalogProvider); err == nil {
		t.Fatal("a catalog without the provider must be an error")
	}
	if _, err := parseCommunityCatalog([]byte(`{"zai-coding-plan":{"models":{}}}`), CommunityCatalogProvider); err == nil {
		t.Fatal("an empty provider must be an error")
	}
}

func TestListModelsReportsRejectedKey(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	if _, err := service.ListModels(context.Background(), "key.secret"); err == nil {
		t.Fatal("ListModels() expected an error")
	}
}

func TestParseModelCatalogAcceptsUpstreamShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"openai list":      `{"object":"list","data":[{"id":"glm-5.3"}]}`,
		"envelope objects": `{"code":0,"data":[{"modelId":"glm-5.3"}]}`,
		"envelope ids":     `{"code":0,"data":["glm-5.3"]}`,
		"bare list":        `[{"id":"glm-5.3"}]`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			models, err := parseModelCatalog([]byte(payload))
			if err != nil || len(models) != 1 || models[0] != "glm-5.3" {
				t.Fatalf("models = %v err = %v", models, err)
			}
		})
	}
	if _, err := parseModelCatalog([]byte(`{"data":[]}`)); err == nil {
		t.Fatal("an empty catalog must be an error")
	}
}

func TestValidateAPIKeyClassifiesRejection(t *testing.T) {
	t.Parallel()
	status := http.StatusOK
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key.secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(status)
	}))
	if err := service.ValidateAPIKey(context.Background(), "key.secret"); err != nil {
		t.Fatalf("ValidateAPIKey() error = %v", err)
	}
	status = http.StatusUnauthorized
	if err := service.ValidateAPIKey(context.Background(), "key.secret"); err == nil {
		t.Fatal("ValidateAPIKey() expected rejection")
	}
}

func TestResolveProxyBaseURLReadsRoutingTable(t *testing.T) {
	t.Parallel()
	payload := `{"code":0,"data":{"proxyEndpoint":{"mapping":[{"from":"https://open.bigmodel.cn/api/anthropic/v1/messages","to":"https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages"},{"from":"https://api.z.ai/api/anthropic/v1/messages","to":"https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"}]}}}`
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	base, err := service.ResolveProxyBaseURL(context.Background())
	if err != nil {
		t.Fatalf("ResolveProxyBaseURL() error = %v", err)
	}
	if base != CodingPlanProxyBaseURL {
		t.Fatalf("base = %q", base)
	}

	payload = `{"code":0,"data":{"proxyEndpoint":{"mapping":[]}}}`
	if _, err := service.ResolveProxyBaseURL(context.Background()); err == nil {
		t.Fatal("ResolveProxyBaseURL() expected error for an empty table")
	}
}

func TestGeneratePollTokenMatchesZCodeShape(t *testing.T) {
	t.Parallel()
	token, err := GeneratePollToken()
	if err != nil {
		t.Fatalf("GeneratePollToken() error = %v", err)
	}
	if len(token) != pollTokenBytes*2 {
		t.Fatalf("token length = %d", len(token))
	}
	other, _ := GeneratePollToken()
	if token == other {
		t.Fatal("poll tokens must be unguessable")
	}
}
