package antigravityauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestServiceAuthorizationExchangeAndRefreshContracts(t *testing.T) {
	var grants []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			grants = append(grants, r.PostForm)
			_, _ = fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":"rt-%d","expires_in":3600}`, len(grants), len(grants))
		case "/userinfo":
			assertBearer(t, r, "at-1")
			_, _ = w.Write([]byte(`{"email":"user@example.com"}`))
		case "/v1internal:loadCodeAssist":
			assertBearer(t, r, "at-1")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode loadCodeAssist: %v", err)
			}
			_, _ = w.Write([]byte(`{"cloudaicompanionProject":"project-1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := testService(server)
	link, err := service.AuthorizationLink("state-1")
	if err != nil {
		t.Fatalf("AuthorizationLink: %v", err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("access_type") != "offline" || query.Get("prompt") != "consent" || query.Get("state") != "state-1" ||
		query.Get("client_id") != "client-test" || query.Get("redirect_uri") != service.RedirectURI ||
		!strings.Contains(query.Get("scope"), "experimentsandconfigs") {
		t.Fatalf("authorization query = %v", query)
	}

	credential, err := service.ExchangeCode(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if credential.AccessToken != "at-1" || credential.RefreshToken != "rt-1" || credential.Email != "user@example.com" || credential.ProjectID != "project-1" {
		t.Fatalf("credential = %#v", credential)
	}
	refreshed, err := service.Refresh(context.Background(), "rt-1")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.AccessToken != "at-2" || refreshed.RefreshToken != "rt-2" {
		t.Fatalf("refreshed = %#v", refreshed)
	}
	if grants[0].Get("grant_type") != "authorization_code" || grants[0].Get("client_secret") != "secret-test" ||
		grants[1].Get("grant_type") != "refresh_token" || grants[1].Get("refresh_token") != "rt-1" {
		t.Fatalf("grants = %#v", grants)
	}
}

func TestServiceOnboardsWhenProjectIsMissing(t *testing.T) {
	var onboardCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{"currentTier":{"id":"current-tier"}}`))
		case "/v1internal:onboardUser":
			onboardCalls++
			var request struct {
				TierID string `json:"tier_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode onboardUser request: %v", err)
			}
			if request.TierID != "current-tier" || r.Header.Get("X-Goog-Api-Client") != "gl-node/22.21.1" ||
				!strings.Contains(r.Header.Get("User-Agent"), "google-api-nodejs-client/10.3.0") {
				t.Fatalf("onboardUser contract: tier=%q headers=%v", request.TierID, r.Header)
			}
			if onboardCalls == 1 {
				_, _ = w.Write([]byte(`{"done":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"done":true,"response":{"project":{"id":"project-onboard"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	service := testService(server)
	service.Sleep = func(context.Context, time.Duration) error { return nil }
	projectID, err := service.FetchProjectID(context.Background(), "at")
	if err != nil || projectID != "project-onboard" || onboardCalls != 2 {
		t.Fatalf("FetchProjectID = (%q, %v), calls=%d", projectID, err, onboardCalls)
	}
}

func TestServiceCompleteCredentialRefreshesPaidTierFromDailyAPI(t *testing.T) {
	var refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshCalls++
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-expired" {
				t.Fatalf("refresh form = %v", r.Form)
			}
			_, _ = w.Write([]byte(`{"access_token":"at-refreshed","refresh_token":"rt-rotated","expires_in":3600}`))
		case "/daily/v1internal:loadCodeAssist":
			assertBearer(t, r, "at-refreshed")
			if got := r.Header.Get("User-Agent"); got != DefaultUserAgent {
				t.Errorf("User-Agent = %q", got)
			}
			var body map[string]map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode loadCodeAssist: %v", err)
			}
			if body["metadata"]["ideType"] != "ANTIGRAVITY" {
				t.Fatalf("loadCodeAssist body = %#v", body)
			}
			_, _ = w.Write([]byte(`{"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro","description":"not persisted"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := testService(server)
	service.DailyAPIBaseURL = server.URL + "/daily"
	credential, err := service.CompleteCredential(context.Background(), &Credential{
		Type: ChannelType, AccessToken: "at-expired", RefreshToken: "rt-expired",
		Expired: "2000-01-01T00:00:00Z", Email: "user@example.com", ProjectID: "project-1",
	})
	if err != nil {
		t.Fatalf("CompleteCredential: %v", err)
	}
	if refreshCalls != 1 || credential.AccessToken != "at-refreshed" || credential.RefreshToken != "rt-rotated" {
		t.Fatalf("refreshed credential = %#v, refresh calls = %d", credential, refreshCalls)
	}
	if credential.PaidTier == nil || credential.PaidTier.ID != "g1-pro-tier" ||
		credential.PaidTier.DisplayName() != "Google AI Pro" {
		t.Fatalf("paid tier = %#v", credential.PaidTier)
	}
}

func TestServiceCompleteCredentialRefreshesRejectedAccessToken(t *testing.T) {
	var refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshCalls++
			_, _ = w.Write([]byte(`{"access_token":"at-refreshed","refresh_token":"rt-rotated","expires_in":3600}`))
		case "/daily/v1internal:loadCodeAssist":
			if r.Header.Get("Authorization") == "Bearer at-stale" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			assertBearer(t, r, "at-refreshed")
			_, _ = w.Write([]byte(`{"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := testService(server)
	service.DailyAPIBaseURL = server.URL + "/daily"
	credential, err := service.CompleteCredential(context.Background(), &Credential{
		Type: ChannelType, AccessToken: "at-stale", RefreshToken: "rt-current",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Email: "user@example.com", ProjectID: "project-1",
	})
	if err != nil {
		t.Fatalf("CompleteCredential: %v", err)
	}
	if refreshCalls != 1 || credential.AccessToken != "at-refreshed" || credential.RefreshToken != "rt-rotated" {
		t.Fatal("rejected access token was not replaced by one refresh")
	}
}

func TestServiceFetchAvailableModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:fetchAvailableModels" {
			t.Fatalf("request = %s %s", r.Method, r.URL.String())
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("模型发现 URL 不应包含凭证或查询参数: %s", r.URL.String())
		}
		assertBearer(t, r, "at-models-secret")
		if got := r.Header.Get("User-Agent"); got != DefaultUserAgent {
			t.Fatalf("User-Agent = %q", got)
		}
		var request struct {
			Project string `json:"project"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.Project != "project-1" {
			t.Fatalf("project = %q", request.Project)
		}
		_, _ = w.Write([]byte(`{"models":{
			"gemini-3.1-pro-low":{},
			"gemini-3.6-flash-high":{},
			"chat_20706":{}
		}}`))
	}))
	defer server.Close()

	service := testService(server)
	models, err := service.FetchAvailableModels(context.Background(), server.URL, &Credential{
		AccessToken: "at-models-secret",
		ProjectID:   "project-1",
	})
	if err != nil {
		t.Fatalf("FetchAvailableModels: %v", err)
	}
	want := []string{"gemini-3.1-pro-low", "gemini-3.6-flash-high"}
	if fmt.Sprint(models) != fmt.Sprint(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
}

func testService(server *httptest.Server) *Service {
	service := NewService(server.Client())
	service.AuthorizationURL = "https://accounts.example.test/authorize"
	service.TokenURL = server.URL + "/token"
	service.UserInfoURL = server.URL + "/userinfo"
	service.APIBaseURL = server.URL
	service.DailyAPIBaseURL = server.URL
	service.ClientID = "client-test"
	service.ClientSecret = "secret-test"
	service.RedirectURI = "http://localhost:51121/oauth-callback"
	return service
}

func assertBearer(t *testing.T, request *http.Request, wantToken string) {
	t.Helper()
	if got := request.Header.Get("Authorization"); got != "Bearer "+wantToken {
		t.Fatalf("Authorization = %q", got)
	}
}
