package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/oauthcost"
)

func TestParseCredentialNormalizesCLIProxyPayload(t *testing.T) {
	claims, err := json.Marshal(map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":                   "user-1",
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
	if credential.ChatGPTUserID != "user-1" || credential.AccountID != "account-1" ||
		credential.Email != "user@example.com" || credential.PlanType != "plus" {
		t.Fatalf("ID token metadata was not populated: %#v", credential)
	}
	until, ok := credential.SubscriptionActiveUntil()
	wantUntil := time.Date(2030, 2, 3, 4, 5, 6, 0, time.UTC)
	if !ok || !until.Equal(wantUntil) {
		t.Fatalf("SubscriptionActiveUntil() = (%v, %v), want (%v, true)", until, ok, wantUntil)
	}
	info := credential.DecodedIDToken()
	if info == nil || info.ChatGPTUserID != "user-1" || info.ChatGPTAccountID != "account-1" || info.PlanType != "plus" ||
		info.ChatGPTSubscriptionActiveStart != "2030-01-03T04:05:06Z" ||
		info.ChatGPTSubscriptionActiveUntil != "2030-02-03T04:05:06Z" {
		t.Fatalf("DecodedIDToken() = %#v", info)
	}
	encoded, err := credential.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if !strings.Contains(encoded, `"access_token":"at"`) || !strings.Contains(encoded, `"refresh_token":"rt"`) ||
		!strings.Contains(encoded, `"chatgpt_user_id":"user-1"`) {
		t.Fatalf("canonical JSON = %s", encoded)
	}
}

func TestCredentialRefreshWindowAndMerge(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 0, 0, 0, time.UTC)
	current := &Credential{
		AccessToken: "old-at", RefreshToken: "old-rt", IDToken: "old-id",
		ChatGPTUserID: "user-1", AccountID: "account-1", Email: "user@example.com", Type: ChannelType,
		Expired: now.Add(4 * time.Minute).Format(time.RFC3339), PlanType: "plus",
		AccountFedRAMP: true,
		PassiveUsage: &PassiveUsage{
			Windows: []PassiveUsageWindow{{
				Scope: "codex", LimitName: "codex", Kind: "primary", UsedPercent: 6,
				LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(7 * 24 * time.Hour).Unix(),
				SampledAt: now.Format(time.RFC3339Nano),
			}},
			SampledAt: now.Format(time.RFC3339Nano),
		},
		OAuthUsage: json.RawMessage(`{"sampled_at":"2030-01-02T03:00:00Z"}`),
		QuotaCostUsage: &oauthcost.Usage{Windows: []*oauthcost.Window{{
			Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60,
			StartedAt: now.Unix(), ResetAt: now.Add(7 * 24 * time.Hour).Unix(), StandardCostMicroUSD: 2_500_000,
		}}},
		QuotaOverdraft: &QuotaOverdraft{
			Enabled: true, ActiveUntil: now.Add(2 * time.Hour).Unix(), SuccessfulRequests: 2, CostMicroUSD: 1250,
		},
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
	if merged.RefreshToken != "old-rt" || merged.ChatGPTUserID != "user-1" ||
		merged.AccountID != "account-1" || merged.AccessToken != "new-at" ||
		merged.PassiveUsage == nil || len(merged.PassiveUsage.Windows) != 1 || merged.PassiveUsage.Windows[0].UsedPercent != 6 ||
		string(merged.OAuthUsage) != `{"sampled_at":"2030-01-02T03:00:00Z"}` ||
		merged.QuotaCostUsage == nil || len(merged.QuotaCostUsage.Windows) != 1 ||
		merged.QuotaCostUsage.Windows[0].StandardCostMicroUSD != 2_500_000 ||
		merged.QuotaOverdraft == nil || !merged.QuotaOverdraft.Enabled ||
		merged.QuotaOverdraft.ActiveUntil != now.Add(2*time.Hour).Unix() ||
		merged.QuotaOverdraft.SuccessfulRequests != 2 || merged.QuotaOverdraft.CostMicroUSD != 1250 ||
		!merged.AccountFedRAMP {
		t.Fatalf("merged credential = %#v", merged)
	}
	current.PassiveUsage.Windows[0].UsedPercent = 99
	current.QuotaCostUsage.Windows[0].StandardCostMicroUSD = 99
	current.QuotaOverdraft.SuccessfulRequests = 99
	if merged.PassiveUsage.Windows[0].UsedPercent != 6 {
		t.Fatalf("merged passive usage shares mutable state with the old credential: %#v", merged.PassiveUsage)
	}
	if merged.QuotaOverdraft.SuccessfulRequests != 2 {
		t.Fatalf("merged quota overdraft shares mutable state with the old credential: %#v", merged.QuotaOverdraft)
	}
	if merged.QuotaCostUsage.Windows[0].StandardCostMicroUSD != 2_500_000 {
		t.Fatalf("merged quota cost usage shares mutable state with the old credential: %#v", merged.QuotaCostUsage)
	}
}

func TestPersonalAccessTokenCredentialHasNoOAuthRefreshLifecycle(t *testing.T) {
	credential := &Credential{
		Type:          ChannelType,
		AuthMode:      AuthModePersonalAccessToken,
		AccessToken:   " at-static ",
		RefreshToken:  "must-not-survive",
		IDToken:       "must-not-survive",
		Expired:       "2030-01-02T03:04:05Z",
		LastRefresh:   "2030-01-02T02:04:05Z",
		Email:         " user@example.com ",
		ChatGPTUserID: " user-1 ",
		AccountID:     " account-1 ",
		PlanType:      " plus ",
	}

	raw, err := credential.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if !credential.IsPersonalAccessToken() || credential.AccessToken != "at-static" {
		t.Fatalf("normalized PAT credential = %#v", credential)
	}
	for _, forbidden := range []string{"refresh_token", "id_token", "expired", "last_refresh"} {
		if strings.Contains(raw, `"`+forbidden+`"`) {
			t.Fatalf("PAT JSON contains OAuth-only field %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(raw, `"auth_mode":"personalAccessToken"`) {
		t.Fatalf("PAT JSON = %s", raw)
	}
	needsRefresh, err := credential.NeedsRefresh(time.Now(), 5*time.Minute)
	if err != nil || needsRefresh {
		t.Fatalf("NeedsRefresh() = (%v, %v), want (false, nil)", needsRefresh, err)
	}
	if _, err := credential.MergeRefresh(&Credential{}); err == nil {
		t.Fatal("MergeRefresh() accepted a personal access token")
	}
}

func TestParseCredentialRejectsInvalidImport(t *testing.T) {
	tests := []string{
		`{}`,
		`{"type":"api_key","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"}`,
		`{"type":"codex","access_token":"at","refresh_token":"rt","expired":"bad"}`,
		`{"type":"codex","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z","quota_overdraft":{"successful_requests":-1}}`,
		`{"type":"codex","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z","quota_overdraft":{"active_until":-1}}`,
		`{"type":"codex","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"} {}`,
		`{"type":"codex","auth_mode":"personalAccessToken","access_token":"not-an-at-token"}`,
		`{"type":"codex","auth_mode":"unknown","access_token":"at-token"}`,
	}
	for _, raw := range tests {
		if _, err := ParseCredential([]byte(raw)); err == nil {
			t.Fatalf("ParseCredential(%q) succeeded", raw)
		}
	}
}
