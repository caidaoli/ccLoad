package codexauth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestServiceAuthorizationAndTokenContracts(t *testing.T) {
	t.Parallel()

	var grants []url.Values
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		grants = append(grants, r.PostForm)
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
	if credential.Email != "user@example.com" || credential.AccountID != "acct-test" || credential.PlanType != "plus" {
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
	if got := grants[0].Get("grant_type"); got != "authorization_code" {
		t.Fatalf("exchange grant_type = %q", got)
	}
	if got := grants[0].Get("code_verifier"); got != pkce.Verifier {
		t.Fatalf("code_verifier = %q", got)
	}
	if got := grants[1].Get("grant_type"); got != "refresh_token" {
		t.Fatalf("refresh grant_type = %q", got)
	}
	if got := grants[1].Get("refresh_token"); got != "rt-1" {
		t.Fatalf("refresh_token = %q", got)
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

func testIDToken(t *testing.T) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"user@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct-test","chatgpt_plan_type":"plus"}}`))
	return "header." + payload + ".signature"
}
