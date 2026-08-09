package anthropicauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthorizationLinkUsesAnthropicPKCEContract(t *testing.T) {
	service := NewService(http.DefaultClient)
	link, err := service.AuthorizationLink("state-1", PKCE{Verifier: "verifier", Challenge: "challenge"})
	if err != nil {
		t.Fatalf("AuthorizationLink() error = %v", err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != AuthorizationURL || query.Get("code") != "true" ||
		query.Get("client_id") != ClientID || query.Get("redirect_uri") != RedirectURI || query.Get("scope") != Scope ||
		query.Get("code_challenge_method") != "S256" || query.Get("state") != "state-1" {
		t.Fatalf("authorization URL = %s", link)
	}
}

func TestExchangeAndRefreshUseJSONAndRotatedRefreshToken(t *testing.T) {
	requests := make(chan map[string]string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json, text/plain, */*" ||
			r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "axios/1.13.6" {
			t.Errorf("unexpected request: method=%s headers=%v", r.Method, r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600,"scope":"user:inference","organization":{"uuid":"org"},"account":{"uuid":"account","email_address":"user@example.com"}}`)
	}))
	defer server.Close()

	service := NewService(server.Client())
	service.TokenURL = server.URL
	service.Now = func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }
	credential, err := service.ExchangeCode(context.Background(), "code-1", "state-1", PKCE{Verifier: "verifier", Challenge: "challenge"})
	if err != nil {
		t.Fatalf("ExchangeCode() error = %v", err)
	}
	exchange := <-requests
	if exchange["grant_type"] != "authorization_code" || exchange["state"] != "state-1" || exchange["code_verifier"] != "verifier" {
		t.Fatalf("exchange payload = %v", exchange)
	}
	if credential.RefreshToken != "rotated-refresh" || credential.Expired != "2030-01-01T01:00:00Z" || credential.AccountUUID != "account" {
		t.Fatalf("credential = %+v", credential)
	}
	if _, err := service.Refresh(context.Background(), "old-refresh"); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	refresh := <-requests
	if refresh["grant_type"] != "refresh_token" || refresh["refresh_token"] != "old-refresh" || len(refresh) != 3 {
		t.Fatalf("refresh payload = %v", refresh)
	}
}

func TestCookieAuthExchangesSessionKeyWithoutPersistingItInTokenRequest(t *testing.T) {
	const sessionKey = "sk-ant-sid01-cookie-secret"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/organizations":
			cookie, err := r.Cookie("sessionKey")
			if err != nil || cookie.Value != sessionKey {
				t.Errorf("organization cookie = %v, err = %v", cookie, err)
			}
			if r.Header.Get("User-Agent") != cookieBrowserUA || r.Header.Get("Sec-CH-UA") == "" {
				t.Errorf("organization browser headers = %v", r.Header)
			}
			_, _ = io.WriteString(w, `[{"uuid":"personal-org"},{"uuid":"team-org","raven_type":"team"}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth/team-org/authorize":
			cookie, err := r.Cookie("sessionKey")
			if err != nil || cookie.Value != sessionKey {
				t.Errorf("authorization cookie = %v, err = %v", cookie, err)
			}
			if r.Header.Get("Origin") != "https://claude.ai" || r.Header.Get("Referer") != "https://claude.ai/new" {
				t.Errorf("authorization headers = %v", r.Header)
			}
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode authorization request: %v", err)
			}
			if payload["client_id"] != ClientID || payload["organization_uuid"] != "team-org" ||
				payload["scope"] != CookieScope || payload["code_challenge_method"] != "S256" ||
				payload["state"] == "" || payload["code_challenge"] == "" {
				t.Errorf("authorization payload = %v", payload)
			}
			redirect := RedirectURI + "?code=cookie-auth-code&state=" + url.QueryEscape(payload["state"])
			_ = json.NewEncoder(w).Encode(map[string]string{"redirect_uri": redirect})
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			if _, err := r.Cookie("sessionKey"); err == nil {
				t.Error("token request leaked sessionKey Cookie")
			}
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), sessionKey) {
				t.Fatal("token request body leaked sessionKey")
			}
			var payload map[string]string
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("decode token request: %v", err)
			}
			if payload["code"] != "cookie-auth-code" || payload["state"] == "" || payload["code_verifier"] == "" {
				t.Errorf("token payload = %v", payload)
			}
			_, _ = io.WriteString(w, `{"access_token":"cookie-access","refresh_token":"cookie-refresh","token_type":"Bearer","expires_in":3600,"scope":"user:inference","account":{"uuid":"cookie-account","email_address":"cookie@example.com"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := NewService(server.Client())
	service.ClaudeWebURL = server.URL
	service.TokenURL = server.URL + "/token"
	credential, err := service.CookieAuth(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("CookieAuth() error = %v", err)
	}
	if requests.Load() != 3 || credential.AccessToken != "cookie-access" ||
		credential.RefreshToken != "cookie-refresh" || credential.OrgUUID != "team-org" ||
		credential.AccountUUID != "cookie-account" || credential.EmailAddress != "cookie@example.com" {
		t.Fatalf("credential = %+v, requests = %d", credential, requests.Load())
	}
}

func TestCookieAuthDoesNotFollowRedirectsWithSessionKey(t *testing.T) {
	const sessionKey = "sk-ant-sid01-redirect-secret"
	var redirected atomic.Bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer redirectTarget.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer server.Close()

	service := NewService(server.Client())
	service.ClaudeWebURL = server.URL
	_, err := service.CookieAuth(context.Background(), sessionKey)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("CookieAuth() error = %v", err)
	}
	if strings.Contains(err.Error(), sessionKey) || redirected.Load() {
		t.Fatalf("CookieAuth leaked sessionKey or followed redirect: err=%v redirected=%v", err, redirected.Load())
	}
}
