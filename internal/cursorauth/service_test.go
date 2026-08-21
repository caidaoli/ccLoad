package cursorauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newTestService(t *testing.T, handler http.Handler) (*Service, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	service := NewService(server.Client())
	service.APIBaseURL = server.URL
	service.WebsiteURL = "https://cursor.com"
	return service, server
}

func TestInitFlowBuildsLoginDeepControlURLWithoutNetwork(t *testing.T) {
	t.Parallel()
	service := NewService(http.DefaultClient)
	flow, err := service.InitFlow()
	if err != nil {
		t.Fatalf("InitFlow() error = %v", err)
	}
	parsed, err := url.Parse(flow.AuthorizeURL)
	if err != nil {
		t.Fatalf("AuthorizeURL parse: %v", err)
	}
	if parsed.Host != "cursor.com" || parsed.Path != "/loginDeepControl" {
		t.Fatalf("url = %s", flow.AuthorizeURL)
	}
	query := parsed.Query()
	if query.Get("uuid") != flow.UUID || query.Get("challenge") == "" || query.Get("mode") != "login" ||
		query.Get("redirectTarget") != "cli" || query.Has("verifier") {
		t.Fatalf("query = %s", parsed.RawQuery)
	}
	if flow.Verifier == "" || strings.ContainsAny(flow.Verifier, "+/=") {
		t.Fatalf("verifier = %q", flow.Verifier)
	}
}

func TestPollReportsPendingReadyAndFailed(t *testing.T) {
	t.Parallel()
	var status int
	var body string
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AuthPollPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("uuid") != "u-1" || r.URL.Query().Get("verifier") != "ver" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))

	status, body = http.StatusNotFound, ""
	result, err := service.Poll(context.Background(), "u-1", "ver")
	if err != nil || result.Status != PollPending {
		t.Fatalf("pending = %+v err = %v", result, err)
	}

	status, body = http.StatusOK, `{"accessToken":"tok","refreshToken":"ref"}`
	result, err = service.Poll(context.Background(), "u-1", "ver")
	if err != nil || result.Status != PollReady || result.AccessToken != "tok" || result.RefreshToken != "ref" {
		t.Fatalf("ready = %+v err = %v", result, err)
	}

	status, body = http.StatusForbidden, `{"error":"geo"}`
	result, err = service.Poll(context.Background(), "u-1", "ver")
	if err != nil || result.Status != PollFailed {
		t.Fatalf("forbidden = %+v err = %v", result, err)
	}
}

func TestExchangeAPIKeyAndIdentity(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ExchangeAPIKeyPath:
			if r.Header.Get("Authorization") != "Bearer user-key" {
				t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `{"accessToken":"tok","refreshToken":"ref"}`)
		case GetMeRPC:
			if r.Header.Get("connect-protocol-version") != "1" ||
				r.Header.Get("x-cursor-client-type") != ClientType {
				t.Errorf("headers = %v", r.Header)
			}
			_, _ = io.WriteString(w, `{"authId":"auth-1","email":"user@example.com","firstName":"Ada","lastName":"Lovelace"}`)
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	pair, err := service.ExchangeAPIKey(context.Background(), "user-key")
	if err != nil || pair.AccessToken != "tok" {
		t.Fatalf("pair = %+v err = %v", pair, err)
	}
	identity, name, err := service.FetchIdentity(context.Background(), "tok")
	if err != nil || identity.UserID != "auth-1" || identity.Email != "user@example.com" || name != "Ada Lovelace" {
		t.Fatalf("identity = %+v name = %q err = %v", identity, name, err)
	}
}

func TestListModelsStripsThinkingInfix(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ModelsRPC {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"models":[
				{"modelId":"default","displayModelId":"auto","aliases":["auto"]},
				{"modelId":"claude-sonnet-5-thinking-high","displayModelId":"claude-sonnet-5"},
				{"modelId":"claude-opus-5-thinking-high","displayModelId":"claude-opus-5-thinking-high"}
			]
		}`)
	}))
	models, err := service.ListModels(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	want := []string{"auto", "claude-sonnet-5", "claude-opus-5-high"}
	if strings.Join(models, ",") != strings.Join(want, ",") {
		raw, _ := json.Marshal(models)
		t.Fatalf("models = %s", raw)
	}
}

func TestConnectJSONRejectsUnauthorized(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	if _, _, err := service.FetchIdentity(context.Background(), "tok"); err == nil {
		t.Fatal("unauthorized identity must fail")
	}
}
