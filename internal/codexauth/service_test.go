package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestServiceAuthorizationAndTokenContracts(t *testing.T) {
	t.Parallel()

	type tokenRequest struct {
		contentType string
		originator  string
		userAgent   string
		grant       map[string]string
	}
	var grants []tokenRequest
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := tokenRequest{
			contentType: r.Header.Get("Content-Type"),
			originator:  r.Header.Get("Originator"),
			userAgent:   r.Header.Get("User-Agent"),
			grant:       map[string]string{},
		}
		if got.contentType == "application/json" {
			if err := json.NewDecoder(r.Body).Decode(&got.grant); err != nil {
				t.Errorf("decode JSON grant: %v", err)
			}
		} else {
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			for key := range r.PostForm {
				got.grant[key] = r.PostForm.Get(key)
			}
		}
		grants = append(grants, got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":"rt-%d","id_token":%q,"expires_in":3600}`,
			len(grants), len(grants), testIDToken(t))
	}))
	defer tokenServer.Close()

	service := NewService(tokenServer.Client())
	service.AuthorizationURL = "https://auth.example.test/authorize"
	service.TokenURL = tokenServer.URL
	service.ClientID = "client-test"
	service.RedirectURI = "http://localhost:1455/auth/callback"

	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	state, err := GenerateState()
	if err != nil {
		t.Fatalf("GenerateState: %v", err)
	}
	link, err := service.AuthorizationLink(state, pkce)
	if err != nil {
		t.Fatalf("AuthorizationLink: %v", err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse link: %v", err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"client_id":             "client-test",
		"redirect_uri":          service.RedirectURI,
		"state":                 state,
		"code_challenge":        pkce.Challenge,
		"code_challenge_method": "S256",
		"scope":                 "openid email profile offline_access",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("authorization query %s = %q, want %q", key, got, want)
		}
	}

	credential, err := service.ExchangeCode(context.Background(), "code-test", pkce)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if credential.AccessToken != "at-1" || credential.RefreshToken != "rt-1" {
		t.Fatalf("exchange credential = %#v", credential)
	}
	if credential.Email != "user@example.com" || credential.ChatGPTUserID != "user-test" ||
		credential.AccountID != "acct-test" || credential.PlanType != "plus" {
		t.Fatalf("ID token metadata = %#v", credential)
	}

	refreshed, err := service.Refresh(context.Background(), credential.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.AccessToken != "at-2" || refreshed.RefreshToken != "rt-2" {
		t.Fatalf("refresh credential = %#v", refreshed)
	}
	if len(grants) != 2 {
		t.Fatalf("token requests = %d, want 2", len(grants))
	}
	exchange := grants[0]
	if exchange.contentType != "application/x-www-form-urlencoded" ||
		exchange.grant["grant_type"] != "authorization_code" || exchange.grant["code_verifier"] != pkce.Verifier {
		t.Fatalf("exchange request = %#v", exchange)
	}
	// Native Codex refreshes with a JSON body without scope, carrying its client identity.
	refresh := grants[1]
	wantRefresh := map[string]string{"client_id": "client-test", "grant_type": "refresh_token", "refresh_token": "rt-1"}
	if refresh.contentType != "application/json" || !reflect.DeepEqual(refresh.grant, wantRefresh) {
		t.Fatalf("refresh request = %#v, want JSON grant %#v", refresh, wantRefresh)
	}
	for _, request := range grants {
		if request.originator != DefaultOriginator || request.userAgent != DefaultUserAgent {
			t.Fatalf("token request identity = %q / %q", request.originator, request.userAgent)
		}
	}
}

func TestServiceRejectsEmptyAccessToken(t *testing.T) {
	t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"refresh_token":"rt","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	service := NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL

	_, err := service.Refresh(context.Background(), "rt")
	if err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Fatalf("Refresh error = %v, want missing access_token", err)
	}
}

func TestServiceValidatesPersonalAccessTokenWhoAmIContract(t *testing.T) {
	t.Parallel()

	whoamiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer at-test-token" ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("Originator") != DefaultOriginator ||
			r.Header.Get("User-Agent") != DefaultUserAgent {
			t.Fatalf("unexpected whoami request: method=%s headers=%v", r.Method, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"email":"user@example.com",
			"chatgpt_user_id":"user-123",
			"chatgpt_account_id":"acct-123",
			"chatgpt_plan_type":"plus",
			"chatgpt_account_is_fedramp":true
		}`))
	}))
	defer whoamiServer.Close()

	service := NewService(whoamiServer.Client())
	service.WhoAmIURL = whoamiServer.URL
	credential, err := service.ValidatePersonalAccessToken(context.Background(), " at-test-token ")
	if err != nil {
		t.Fatalf("ValidatePersonalAccessToken() error = %v", err)
	}
	if credential.AuthMode != AuthModePersonalAccessToken || credential.AccessToken != "at-test-token" ||
		credential.Email != "user@example.com" || credential.ChatGPTUserID != "user-123" ||
		credential.AccountID != "acct-123" || credential.PlanType != "plus" || !credential.AccountFedRAMP ||
		credential.RefreshToken != "" || credential.Expired != "" {
		t.Fatalf("PAT credential = %#v", credential)
	}
}

func TestServiceRejectsInvalidPersonalAccessTokenResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"invalid"}`},
		{name: "missing identity", status: http.StatusOK, body: `{"email":"user@example.com"}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			service := NewService(server.Client())
			service.WhoAmIURL = server.URL
			if _, err := service.ValidatePersonalAccessToken(context.Background(), "at-test"); err == nil {
				t.Fatal("ValidatePersonalAccessToken() succeeded")
			}
		})
	}

	service := NewService(http.DefaultClient)
	if _, err := service.ValidatePersonalAccessToken(context.Background(), "eyJ.jwt"); err == nil ||
		!strings.Contains(err.Error(), "at-") {
		t.Fatalf("invalid prefix error = %v", err)
	}
}

func testIDToken(t *testing.T) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"user@example.com","https://api.openai.com/auth":{"chatgpt_user_id":"user-test","chatgpt_account_id":"acct-test","chatgpt_plan_type":"plus"}}`))
	return "header." + payload + ".signature"
}
