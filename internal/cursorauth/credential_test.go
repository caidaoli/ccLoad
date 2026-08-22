package cursorauth

import (
	"encoding/json"
	"testing"
)

func TestParseCredentialNormalizesSessionTokens(t *testing.T) {
	t.Parallel()
	credential, err := ParseCredential([]byte(`{"access_token":" tok ","refresh_token":" ref ","email":" User@Example.com ","user_id":"auth-1"}`))
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if credential.Type != ChannelType || credential.AccessToken != "tok" || credential.RefreshToken != "ref" {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.Identity().Email != "User@Example.com" || credential.Identity().UserID != "auth-1" {
		t.Fatalf("identity = %+v", credential.Identity())
	}
}

func TestParseCredentialRejectsInvalidPayloads(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":             ``,
		"missing token":     `{"email":"user@example.com"}`,
		"blank token":       `{"access_token":"   "}`,
		"whitespace token":  `{"access_token":"tok en"}`,
		"wrong type":        `{"type":"zai","access_token":"tok"}`,
		"trailing json":     `{"access_token":"tok"} {}`,
		"bad refresh stamp": `{"access_token":"tok","last_refresh":"not-a-time"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseCredential([]byte(payload)); err == nil {
				t.Fatalf("ParseCredential(%s) expected error", payload)
			}
		})
	}
}

func TestMergeRefreshKeepsIdentityAndRotatesToken(t *testing.T) {
	t.Parallel()
	current := &Credential{
		Type: ChannelType, AccessToken: "old", RefreshToken: "refresh", APIKey: "key",
		UserID: "u-1", Email: "user@example.com", Name: "User",
		OAuthUsage: json.RawMessage(`{"kept":true}`),
	}
	merged, err := current.MergeRefresh(&Credential{AccessToken: "new"})
	if err != nil {
		t.Fatalf("MergeRefresh() error = %v", err)
	}
	if merged.AccessToken != "new" || merged.RefreshToken != "refresh" || merged.APIKey != "key" ||
		merged.UserID != "u-1" || merged.Email != "user@example.com" ||
		string(merged.OAuthUsage) != `{"kept":true}` {
		t.Fatalf("merged = %+v", merged)
	}
	merged.OAuthUsage[0] = 'X'
	if string(current.OAuthUsage) != `{"kept":true}` {
		t.Fatalf("current usage mutated: %s", current.OAuthUsage)
	}
}

func TestCredentialJSONRoundTrip(t *testing.T) {
	t.Parallel()
	credential := &Credential{AccessToken: "tok", Email: "user@example.com", LastRefresh: "2026-01-02T03:04:05+08:00"}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	parsed, err := ParseCredential([]byte(payload))
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if parsed.LastRefresh != "2026-01-01T19:04:05Z" || parsed.AccessToken != "tok" {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestIdentityMatchesAccount(t *testing.T) {
	t.Parallel()
	left := Identity{UserID: "auth-1", Email: "a@example.com"}
	if !left.Matches(Identity{UserID: "auth-1", Email: "other@example.com"}) {
		t.Fatal("user id must win")
	}
	if !(Identity{Email: "User@Example.com"}).Matches(Identity{Email: "user@example.com"}) {
		t.Fatal("email match must be case-insensitive")
	}
	if (Identity{}).Matches(Identity{Email: "user@example.com"}) {
		t.Fatal("zero identity must not match")
	}
}
