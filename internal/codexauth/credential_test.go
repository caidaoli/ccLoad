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
		"quota_overdraft": map[string]any{
			"enabled": true, "active_until": 4102444800, "successful_requests": 2, "cost_microusd": 1250,
		},
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
	var canonical map[string]any
	if err := json.Unmarshal([]byte(encoded), &canonical); err != nil {
		t.Fatal(err)
	}
	if canonical["access_token"] != "at" || canonical["refresh_token"] != "rt" ||
		canonical["chatgpt_user_id"] != "user-1" {
		t.Fatalf("canonical JSON = %s", encoded)
	}
	if _, exists := canonical["quota_overdraft"]; exists {
		t.Fatal("canonical credential retained the removed quota overage setting")
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
	}
	needsRefresh, err := current.NeedsRefresh(now, 5*time.Minute)
	if err != nil || !needsRefresh {
		t.Fatalf("NeedsRefresh() = (%v, %v), want (true, nil)", needsRefresh, err)
	}
	refreshed := &Credential{AccessToken: "new-at", Type: ChannelType, Expired: now.Add(time.Hour).Format(time.RFC3339)}
	merged, err := current.MergeRefresh(refreshed, now)
	if err != nil {
		t.Fatalf("MergeRefresh() error = %v", err)
	}
	if merged.RefreshToken != "old-rt" || merged.ChatGPTUserID != "user-1" ||
		merged.AccountID != "account-1" || merged.AccessToken != "new-at" ||
		merged.PassiveUsage == nil || len(merged.PassiveUsage.Windows) != 1 || merged.PassiveUsage.Windows[0].UsedPercent != 6 ||
		string(merged.OAuthUsage) != `{"sampled_at":"2030-01-02T03:00:00Z"}` ||
		merged.QuotaCostUsage == nil || len(merged.QuotaCostUsage.Windows) != 1 ||
		merged.QuotaCostUsage.Windows[0].StandardCostMicroUSD != 2_500_000 ||
		!merged.AccountFedRAMP {
		t.Fatalf("merged credential = %#v", merged)
	}
	current.PassiveUsage.Windows[0].UsedPercent = 99
	current.QuotaCostUsage.Windows[0].StandardCostMicroUSD = 99
	if merged.PassiveUsage.Windows[0].UsedPercent != 6 {
		t.Fatalf("merged passive usage shares mutable state with the old credential: %#v", merged.PassiveUsage)
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
	if _, err := credential.MergeRefresh(&Credential{}, time.Now()); err == nil {
		t.Fatal("MergeRefresh() accepted a personal access token")
	}
}

func TestParseCredentialRejectsInvalidImport(t *testing.T) {
	tests := []string{
		`{}`,
		`{"type":"api_key","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"}`,
		`{"type":"codex","access_token":"at","refresh_token":"rt","expired":"bad"}`,
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

func TestMergeRefreshObservesQuotaIdentity(t *testing.T) {
	now := time.Date(2030, time.March, 4, 5, 0, 0, 0, time.UTC)
	window := func() *oauthcost.Window {
		return &oauthcost.Window{
			Key: "codex|primary", Family: oauthcost.FamilyCodex, WindowSeconds: 7 * 24 * 60 * 60,
			StartedAt: now.Add(-24 * time.Hour).Unix(), ResetAt: now.Add(6 * 24 * time.Hour).Unix(),
			StandardCostMicroUSD: 2_500_000,
		}
	}
	newCurrent := func(usage *oauthcost.Usage) *Credential {
		return &Credential{
			AccessToken: "old-at", RefreshToken: "old-rt", Type: ChannelType,
			ChatGPTUserID: "user-1", AccountID: "account-1", PlanType: "plus",
			Expired:        now.Add(time.Minute).Format(time.RFC3339),
			OAuthUsage:     json.RawMessage(`{"sampled_at":"2030-03-04T04:00:00Z"}`),
			PassiveUsage:   &PassiveUsage{SampledAt: now.Format(time.RFC3339Nano)},
			QuotaCostUsage: usage,
		}
	}
	refreshed := func(accountID, planType string) *Credential {
		return &Credential{
			AccessToken: "new-at", Type: ChannelType, AccountID: accountID, PlanType: planType,
			Expired: now.Add(time.Hour).Format(time.RFC3339),
		}
	}
	for _, tc := range []struct {
		name         string
		usage        *oauthcost.Usage
		refreshed    *Credential
		wantIdentity string
		wantEpochAt  int64
		wantCost     int64 // <0 表示窗口必须被丢弃
		wantSnapshot bool  // oauth_usage 与 passive_usage 是否保留
	}{
		{
			name:      "same identity keeps everything",
			usage:     &oauthcost.Usage{Identity: "account-1|plus", Windows: []*oauthcost.Window{window()}},
			refreshed: refreshed("account-1", "plus"), wantIdentity: "account-1|plus", wantCost: 2_500_000, wantSnapshot: true,
		},
		{
			name:      "plan change starts a new epoch",
			usage:     &oauthcost.Usage{Identity: "account-1|plus", Windows: []*oauthcost.Window{window()}},
			refreshed: refreshed("account-1", "pro"), wantIdentity: "account-1|pro", wantEpochAt: now.Unix(), wantCost: -1,
		},
		{
			name:      "account change starts a new epoch",
			usage:     &oauthcost.Usage{Identity: "account-1|plus", Windows: []*oauthcost.Window{window()}},
			refreshed: refreshed("account-2", "plus"), wantIdentity: "account-2|plus", wantEpochAt: now.Unix(), wantCost: -1,
		},
		{
			name:      "legacy state adopts the old identity before comparing",
			usage:     &oauthcost.Usage{Windows: []*oauthcost.Window{window()}},
			refreshed: refreshed("account-1", "pro"), wantIdentity: "account-1|pro", wantEpochAt: now.Unix(), wantCost: -1,
		},
		{
			name:      "refresh without claims only inherits",
			usage:     &oauthcost.Usage{Windows: []*oauthcost.Window{window()}},
			refreshed: refreshed("", ""), wantIdentity: "account-1|plus", wantCost: 2_500_000, wantSnapshot: true,
		},
		{
			name:      "epoch opened by the usage poll only records the identity",
			usage:     &oauthcost.Usage{EpochAt: now.Add(-time.Hour).Unix(), Windows: []*oauthcost.Window{window()}},
			refreshed: refreshed("account-1", "pro"), wantIdentity: "account-1|pro", wantEpochAt: now.Add(-time.Hour).Unix(), wantCost: 2_500_000, wantSnapshot: true,
		},
		{
			name:      "poll epoch does not hide a later account change",
			usage:     &oauthcost.Usage{AccountID: "account-1", EpochAt: now.Add(-time.Hour).Unix(), Windows: []*oauthcost.Window{window()}},
			refreshed: refreshed("account-2", "pro"), wantIdentity: "account-2|pro", wantEpochAt: now.Unix(), wantCost: -1,
		},
		{
			name:      "account change without plan claims still starts an epoch",
			usage:     &oauthcost.Usage{AccountID: "account-1", Windows: []*oauthcost.Window{window()}},
			refreshed: refreshed("account-2", ""), wantIdentity: "", wantEpochAt: now.Unix(), wantCost: -1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := newCurrent(tc.usage).MergeRefresh(tc.refreshed, now)
			if err != nil {
				t.Fatalf("MergeRefresh() error = %v", err)
			}
			usage := merged.QuotaCostUsage
			if usage == nil || usage.Identity != tc.wantIdentity || usage.EpochAt != tc.wantEpochAt {
				t.Fatalf("quota cost usage = %#v, want identity %q epoch %d", usage, tc.wantIdentity, tc.wantEpochAt)
			}
			got := oauthcost.Find(usage, "codex|primary")
			if tc.wantCost < 0 {
				if got != nil {
					t.Fatalf("window survived a new epoch: %#v", got)
				}
			} else if got == nil || got.StandardCostMicroUSD != tc.wantCost {
				t.Fatalf("window = %#v, want cost %d", got, tc.wantCost)
			}
			if tc.wantSnapshot && (len(merged.OAuthUsage) == 0 || merged.PassiveUsage == nil) {
				t.Fatalf("snapshots were dropped without a new epoch: oauth_usage=%s passive_usage=%#v", merged.OAuthUsage, merged.PassiveUsage)
			}
			if !tc.wantSnapshot && (len(merged.OAuthUsage) != 0 || merged.PassiveUsage != nil) {
				t.Fatalf("snapshots survived a new epoch: oauth_usage=%s passive_usage=%#v", merged.OAuthUsage, merged.PassiveUsage)
			}
		})
	}
}

func TestMergeRefreshWaitsForClaimsAfterPoll(t *testing.T) {
	t.Parallel()
	at := time.Date(2030, 3, 4, 5, 0, 0, 0, time.UTC)
	for _, account := range []string{"account-1", "account-2"} {
		t.Run(account, func(t *testing.T) {
			current := &Credential{
				Type: ChannelType, AccessToken: "old-at", RefreshToken: "rt", AccountID: "account-1",
				PlanType: "plus", Expired: at.Add(time.Hour).Format(time.RFC3339),
			}
			current.RestartQuotaEpochFromPoll(at)
			current.RestartQuotaEpochFromPoll(at.Add(time.Second))
			epoch := current.QuotaCostUsage.EpochTime()
			current.QuotaCostUsage.Windows = []*oauthcost.Window{{
				Key: "codex|primary", WindowSeconds: 18000, StartedAt: at.Unix(),
				ResetAt: at.Add(5 * time.Hour).Unix(), StandardCostMicroUSD: 500_000,
			}}
			for i, plan := range []string{"plus", "", "plus", "pro"} {
				raw, err := current.JSON()
				if err != nil {
					t.Fatal(err)
				}
				current, err = ParseCredential([]byte(raw))
				if err != nil {
					t.Fatal(err)
				}
				current, err = current.MergeRefresh(&Credential{
					Type: ChannelType, AccessToken: "new-at", AccountID: "account-1", PlanType: plan,
					Expired: at.Add(time.Hour).Format(time.RFC3339),
				}, at.Add(time.Duration(i+2)*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				if !current.QuotaCostUsage.EpochTime().Equal(epoch) || len(current.QuotaCostUsage.Windows) != 1 || current.QuotaCostUsage.Windows[0].StandardCostMicroUSD != 500_000 {
					t.Fatalf("claims %q restarted poll epoch: %#v", plan, current.QuotaCostUsage)
				}
			}
			if current.QuotaCostUsage.Identity != "account-1|pro" || current.QuotaIdentityBeforePoll != "" {
				t.Fatalf("new claims were not adopted: %#v", current.QuotaCostUsage)
			}
			// 完成补记后，下一次真实身份变化仍重置；等待期间换账号也一样。
			if account == "account-2" {
				current.RestartQuotaEpochFromPoll(at.Add(10 * time.Second))
			}
			plan := "team"
			if account == "account-2" {
				plan = ""
			}
			if !current.ObserveQuotaIdentity(account, plan, at.Add(11*time.Second)) || len(current.QuotaCostUsage.Windows) != 0 || current.QuotaIdentityBeforePoll != "" {
				t.Fatal("later identity change did not restart epoch")
			}
		})
	}
}
