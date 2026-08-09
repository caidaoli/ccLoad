package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseCredentialNormalizesCLIProxyPayload(t *testing.T) {
	claims, err := json.Marshal(map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "account-1",
			"chatgpt_plan_type":                 "plus",
			"chatgpt_subscription_active_start": "2030-01-03T04:05:06Z",
			"chatgpt_subscription_active_until": "2030-02-03T04:05:06Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	idToken := "x." + base64.RawURLEncoding.EncodeToString(claims) + ".y"
	raw, err := json.Marshal(map[string]any{
		"id_token":      idToken,
		"access_token":  " at ",
		"refresh_token": " rt ",
		"expired":       "2030-01-02T03:04:05Z",
		"type":          "codex",
	})
	if err != nil {
		t.Fatal(err)
	}

	credential, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" {
		t.Fatalf("tokens were not normalized: %#v", credential)
	}
	if credential.AccountID != "account-1" || credential.Email != "user@example.com" || credential.PlanType != "plus" {
		t.Fatalf("ID token metadata was not populated: %#v", credential)
	}
	until, ok := credential.SubscriptionActiveUntil()
	wantUntil := time.Date(2030, 2, 3, 4, 5, 6, 0, time.UTC)
	if !ok || !until.Equal(wantUntil) {
		t.Fatalf("SubscriptionActiveUntil() = (%v, %v), want (%v, true)", until, ok, wantUntil)
	}
	info := credential.DecodedIDToken()
	if info == nil || info.ChatGPTAccountID != "account-1" || info.PlanType != "plus" ||
		info.ChatGPTSubscriptionActiveStart != "2030-01-03T04:05:06Z" ||
		info.ChatGPTSubscriptionActiveUntil != "2030-02-03T04:05:06Z" {
		t.Fatalf("DecodedIDToken() = %#v", info)
	}
	encoded, err := credential.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if !strings.Contains(encoded, `"access_token":"at"`) || !strings.Contains(encoded, `"refresh_token":"rt"`) {
		t.Fatalf("canonical JSON = %s", encoded)
	}
}

func TestCredentialRefreshWindowAndMerge(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 0, 0, 0, time.UTC)
	current := &Credential{
		AccessToken: "old-at", RefreshToken: "old-rt", IDToken: "old-id",
		AccountID: "account-1", Email: "user@example.com", Type: ChannelType,
		Expired: now.Add(4 * time.Minute).Format(time.RFC3339), PlanType: "plus",
		PassiveUsage: &PassiveUsage{
			Windows: []PassiveUsageWindow{{
				Scope: "codex", LimitName: "codex", Kind: "primary", UsedPercent: 6,
				LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(7 * 24 * time.Hour).Unix(),
				SampledAt: now.Format(time.RFC3339Nano),
			}},
			SampledAt: now.Format(time.RFC3339Nano),
		},
		OAuthUsage: json.RawMessage(`{"sampled_at":"2030-01-02T03:00:00Z"}`),
	}
	needsRefresh, err := current.NeedsRefresh(now, 5*time.Minute)
	if err != nil || !needsRefresh {
		t.Fatalf("NeedsRefresh() = (%v, %v), want (true, nil)", needsRefresh, err)
	}
	refreshed := &Credential{AccessToken: "new-at", Type: ChannelType, Expired: now.Add(time.Hour).Format(time.RFC3339)}
	merged, err := current.MergeRefresh(refreshed)
	if err != nil {
		t.Fatalf("MergeRefresh() error = %v", err)
	}
	if merged.RefreshToken != "old-rt" || merged.AccountID != "account-1" || merged.AccessToken != "new-at" ||
		merged.PassiveUsage == nil || len(merged.PassiveUsage.Windows) != 1 || merged.PassiveUsage.Windows[0].UsedPercent != 6 ||
		string(merged.OAuthUsage) != `{"sampled_at":"2030-01-02T03:00:00Z"}` {
		t.Fatalf("merged credential = %#v", merged)
	}
	current.PassiveUsage.Windows[0].UsedPercent = 99
	if merged.PassiveUsage.Windows[0].UsedPercent != 6 {
		t.Fatalf("merged passive usage shares mutable state with the old credential: %#v", merged.PassiveUsage)
	}
}

func TestParseCredentialRejectsInvalidImport(t *testing.T) {
	tests := []string{
		`{}`,
		`{"type":"api_key","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"}`,
		`{"type":"codex","access_token":"at","refresh_token":"rt","expired":"bad"}`,
		`{"type":"codex","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"} {}`,
	}
	for _, raw := range tests {
		if _, err := ParseCredential([]byte(raw)); err == nil {
			t.Fatalf("ParseCredential(%q) succeeded", raw)
		}
	}
}
