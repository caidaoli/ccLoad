package xaiauth_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/xaiauth"
)

func jwt(claims map[string]any) string {
	raw, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestCredentialNormalizeCanonicalizesCompatibleExpiryAndIdentity(t *testing.T) {
	t.Parallel()
	raw := `{"access_token":"` + jwt(map[string]any{"email": "access@example.com", "sub": "access-sub"}) + `","refresh_token":"refresh-secret","id_token":"` + jwt(map[string]any{"email": "id@example.com"}) + `","email":"forged@example.com","sub":"forged-sub","expires_at":1893456000,"client_id":" custom-client ","scope":" openid ","token_endpoint":"https://auth.x.ai/oauth2/token"}`
	credential, err := xaiauth.ParseCredential([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if credential.Type != xaiauth.ChannelType || credential.AuthKind != "oauth" {
		t.Fatalf("canonical markers = %q/%q", credential.Type, credential.AuthKind)
	}
	if credential.Expired != "2030-01-01T00:00:00Z" || credential.ClientID != "custom-client" || credential.Scope != "openid" {
		t.Fatalf("credential not normalized: %+v", credential.Redacted())
	}
	identity := credential.Identity()
	if identity.Email != "id@example.com" || identity.Subject != "access-sub" {
		t.Fatalf("ID token must win per claim and access token must fill gaps: %+v", identity)
	}
	if credential.Email != identity.Email || credential.Subject != identity.Subject {
		t.Fatalf("canonical fields disagree with token identity: fields=%q/%q identity=%+v", credential.Email, credential.Subject, identity)
	}
	encoded, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, `"expired":"2030-01-01T00:00:00Z"`) {
		t.Fatalf("canonical JSON missing expiry: %s", encoded)
	}
}

func TestCredentialNormalizeSupportsLastRefreshExpiresInAndRefreshMerge(t *testing.T) {
	t.Parallel()
	old := &xaiauth.Credential{
		AccessToken: "old-access", RefreshToken: "old-refresh", IDToken: "old-id",
		ExpiresIn: 3600, LastRefresh: "2030-01-01T00:00:00Z", Email: "old@example.com", Subject: "old-sub",
		ClientID: "custom-client", Scope: "old-scope", TeamID: "team", SubscriptionTier: "pro", EntitlementStatus: "active",
	}
	if err := old.Normalize(); err != nil {
		t.Fatal(err)
	}
	refreshed := &xaiauth.Credential{AccessToken: "new-access", ExpiresIn: 7200, LastRefresh: "2030-01-02T00:00:00Z"}
	merged, err := old.MergeRefresh(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if merged.RefreshToken != "old-refresh" || merged.IDToken != "old-id" || merged.Email != "old@example.com" || merged.ClientID != "custom-client" || merged.SubscriptionTier != "pro" {
		t.Fatalf("stable fields not preserved: %+v", merged.Redacted())
	}
	if merged.Expired != "2030-01-02T02:00:00Z" {
		t.Fatalf("expiry = %q", merged.Expired)
	}
	needs, err := merged.NeedsRefresh(time.Date(2030, 1, 2, 1, 56, 0, 0, time.UTC), xaiauth.RefreshLead)
	if err != nil || !needs {
		t.Fatalf("NeedsRefresh = %v, %v", needs, err)
	}
}

func TestCredentialRejectsIncompleteAndDoesNotLeakSecrets(t *testing.T) {
	t.Parallel()
	secret := "very-secret-token"
	_, err := xaiauth.ParseCredential([]byte(`{"access_token":"` + secret + `","expired":"2030-01-01T00:00:00Z"}`))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error: %v", err)
	}
	c := &xaiauth.Credential{AccessToken: secret, RefreshToken: "refresh-secret", Expired: "2030-01-01T00:00:00Z"}
	if strings.Contains(c.Redacted(), secret) || strings.Contains(c.Redacted(), "refresh-secret") {
		t.Fatalf("redacted credential leaked: %s", c.Redacted())
	}
}

func TestParseCredentialRejectsEveryNonWhitespaceTail(t *testing.T) {
	t.Parallel()
	base := `{"access_token":"access","refresh_token":"refresh","expired":"2030-01-01T00:00:00Z"}`
	for _, tail := range []string{`{}`, `true`, `[]`, `garbage`} {
		if _, err := xaiauth.ParseCredential([]byte(base + tail)); err == nil {
			t.Errorf("accepted trailing %q", tail)
		}
	}
	if _, err := xaiauth.ParseCredential([]byte(base + " \n\t")); err != nil {
		t.Fatalf("whitespace tail rejected: %v", err)
	}
}

func TestMergeRefreshUsesOnlyNewIdentitySourcesBeforeOldIdentity(t *testing.T) {
	t.Parallel()
	old := &xaiauth.Credential{
		AccessToken: "old-access", RefreshToken: "old-refresh",
		IDToken: jwt(map[string]any{"email": "old@example.com", "sub": "old-sub"}),
		Email:   "old@example.com", Subject: "old-sub", Expired: "2030-01-01T00:00:00Z",
	}
	refreshed := &xaiauth.Credential{
		AccessToken: jwt(map[string]any{"email": "access-new@example.com", "sub": "access-new-sub"}),
		IDToken:     jwt(map[string]any{"email": "id-new@example.com"}),
		Email:       "explicit@example.com", Subject: "explicit-sub",
		RefreshToken: "new-refresh", Expired: "2030-01-02T00:00:00Z",
	}
	merged, err := old.MergeRefresh(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	want := xaiauth.Identity{Email: "id-new@example.com", Subject: "access-new-sub"}
	if merged.Email != want.Email || merged.Subject != want.Subject || merged.Identity() != want {
		t.Fatalf("canonical identity mismatch: fields=%q/%q Identity=%+v", merged.Email, merged.Subject, merged.Identity())
	}
}
