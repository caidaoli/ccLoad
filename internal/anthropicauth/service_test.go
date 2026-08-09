package anthropicauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
