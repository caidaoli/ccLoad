package antigravityauth

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/oauthcost"
)

func TestParseCredentialAndRefreshMerge(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	credential, err := ParseCredential([]byte(`{"type":"antigravity","access_token":" at ","refresh_token":" rt ","expires_in":3600,"timestamp":` +
		fmtInt(now.UnixMilli()) + `,"email":" user@example.com ","project_id":" project-1 ","paid_tier":{"id":" g1-pro-tier ","name":" Google AI Pro "}}`))
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" || credential.ProjectID != "project-1" ||
		credential.PaidTier == nil || credential.PaidTier.ID != "g1-pro-tier" || credential.PaidTier.Name != "Google AI Pro" {
		t.Fatalf("credential = %#v", credential)
	}
	credential.OAuthUsage = []byte(`{"sampled_at":"2030-01-01T00:00:00Z"}`)
	credential.QuotaCostUsage = &oauthcost.Usage{Windows: []*oauthcost.Window{{
		Key: "gemini models|gemini-weekly", Family: oauthcost.FamilyGemini, WindowSeconds: 7 * 24 * 60 * 60,
		StartedAt: now.Unix(), ResetAt: now.Add(7 * 24 * time.Hour).Unix(), StandardCostMicroUSD: 4_500_000,
	}}}
	needsRefresh, err := credential.NeedsRefresh(now, 2*time.Hour)
	if err != nil || !needsRefresh {
		t.Fatalf("NeedsRefresh = (%v, %v)", needsRefresh, err)
	}
	refreshed := &Credential{Type: ChannelType, AccessToken: "new-at", ExpiresIn: 3600, Timestamp: now.UnixMilli(), Expired: now.Add(time.Hour).Format(time.RFC3339)}
	merged, err := credential.MergeRefresh(refreshed)
	if err != nil {
		t.Fatalf("MergeRefresh: %v", err)
	}
	if merged.RefreshToken != "rt" || merged.Email != "user@example.com" || merged.ProjectID != "project-1" ||
		merged.PaidTier == nil || merged.PaidTier.DisplayName() != "Google AI Pro" ||
		string(merged.OAuthUsage) != `{"sampled_at":"2030-01-01T00:00:00Z"}` ||
		merged.QuotaCostUsage == nil || len(merged.QuotaCostUsage.Windows) != 1 ||
		merged.QuotaCostUsage.Windows[0].StandardCostMicroUSD != 4_500_000 {
		t.Fatalf("merged = %#v", merged)
	}
	raw, err := merged.JSON()
	if err != nil || !strings.Contains(raw, `"project_id":"project-1"`) ||
		!strings.Contains(raw, `"paid_tier":{"id":"g1-pro-tier","name":"Google AI Pro"}`) {
		t.Fatalf("JSON = (%s, %v)", raw, err)
	}
}

func TestPaidTierDisplayNameNormalizesFreeTier(t *testing.T) {
	if got := (&PaidTier{ID: "free-tier", Name: "Antigravity Starter Quota"}).DisplayName(); got != "Antigravity Free" {
		t.Fatalf("free tier display name = %q", got)
	}
	if got := (&PaidTier{ID: "g1-pro-tier", Name: "Google AI Pro"}).DisplayName(); got != "Google AI Pro" {
		t.Fatalf("paid tier display name = %q", got)
	}
}

func TestParseCredentialRejectsInvalidImport(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"type":"codex","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"}`,
		`{"type":"antigravity","access_token":"at","refresh_token":"rt","expired":"bad"}`,
		`{"type":"antigravity","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"} {}`,
	} {
		if _, err := ParseCredential([]byte(raw)); err == nil {
			t.Fatalf("ParseCredential(%q) succeeded", raw)
		}
	}
}

func fmtInt(value int64) string {
	return fmt.Sprintf("%d", value)
}
