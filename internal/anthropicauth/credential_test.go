package anthropicauth

import (
	"strings"
	"testing"
	"time"
)

func TestParseCredentialAcceptsSub2APITimestampsAndCanonicalizes(t *testing.T) {
	raw := `{"access_token":"access","refresh_token":"refresh","expires_in":"28800","expires_at":"1893456000","org_uuid":"org","account_uuid":"account","email_address":"user@example.com"}`
	credential, err := ParseCredential([]byte(raw))
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if credential.Type != ChannelType || credential.ExpiresIn != 28800 || credential.Expired != "2030-01-01T00:00:00Z" {
		t.Fatalf("credential = %+v", credential)
	}
	encoded, err := credential.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if !strings.Contains(encoded, `"expires_in":28800`) || !strings.Contains(encoded, `"expired":"2030-01-01T00:00:00Z"`) {
		t.Fatalf("canonical JSON = %s", encoded)
	}
}

func TestCredentialMergeRefreshPreservesIdentityAndUsesRotatedRefreshToken(t *testing.T) {
	current := &Credential{
		Type: ChannelType, AccessToken: "old-access", RefreshToken: "old-refresh",
		Expired: "2030-01-01T00:00:00Z", Scope: "scope", OrgUUID: "org",
		AccountUUID: "account", EmailAddress: "user@example.com",
	}
	refreshed := &Credential{
		Type: ChannelType, AccessToken: "new-access", RefreshToken: "rotated-refresh",
		Expired: "2030-01-02T00:00:00Z",
	}
	merged, err := current.MergeRefresh(refreshed)
	if err != nil {
		t.Fatalf("MergeRefresh() error = %v", err)
	}
	if merged.RefreshToken != "rotated-refresh" || merged.AccountUUID != "account" || merged.Scope != "scope" {
		t.Fatalf("merged = %+v", merged)
	}
	needsRefresh, err := merged.NeedsRefresh(time.Date(2030, 1, 1, 23, 56, 0, 0, time.UTC), 5*time.Minute)
	if err != nil || !needsRefresh {
		t.Fatalf("NeedsRefresh() = %v, %v", needsRefresh, err)
	}
}

func TestParseCredentialRejectsTrailingJSON(t *testing.T) {
	_, err := ParseCredential([]byte(`{"type":"anthropic"}{}`))
	if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("ParseCredential() error = %v", err)
	}
}
