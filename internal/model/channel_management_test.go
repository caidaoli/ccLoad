package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestChannelManagementEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()
	userID := int64(42)
	lastCheckin := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	used := 12.5
	limit := 50.0

	tests := []struct {
		name     string
		profile  string
		settings ChannelManagementSettings
	}{
		{
			name:    "new api",
			profile: ChannelManagementProfileNewAPI,
			settings: ChannelManagementSettings{
				BaseURL: "https://new-api.example.com/", AccessToken: "new-api-private-token", UserID: &userID,
				DailyCheckinEnabled: true, DailyCheckinTime: "09:00",
			},
		},
		{
			name:    "sub2api",
			profile: ChannelManagementProfileSub2API,
			settings: ChannelManagementSettings{
				BaseURL: "http://sub2api.example.com/", AccessToken: "sub2api-private-token",
			},
		},
		{
			name:    "sub2api pro",
			profile: ChannelManagementProfileSub2APIPro,
			settings: ChannelManagementSettings{
				BaseURL: "https://sub2api-pro.example.com/", AccessToken: "sub2api-pro-private-token",
				DailyCheckinEnabled: true, DailyCheckinTime: "23:59",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			envelope := &ChannelManagementEnvelope{
				Kind: ChannelManagementKind, Version: ChannelManagementVersion, Profile: tt.profile, Settings: tt.settings,
				State: ChannelManagementState{
					LastScheduledDay: "2026-08-25", LastCheckinAt: &lastCheckin, LastCheckinStatus: "success",
					LastBalance: &ChannelManagementBalanceSnapshot{
						BalanceUSD: &limit, SampledAt: lastCheckin,
						Subscriptions: []ChannelManagementSubscriptionSnapshot{{ID: 7, Name: "main", Window: "monthly", UsedUSD: &used, LimitUSD: &limit}},
					},
				},
			}
			raw, err := envelope.Marshal()
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			parsed, err := ParseChannelManagementEnvelope(raw)
			if err != nil {
				t.Fatalf("ParseChannelManagementEnvelope() error = %v", err)
			}
			if parsed.Profile != tt.profile || parsed.Settings.BaseURL != strings.TrimSuffix(tt.settings.BaseURL, "/") || parsed.Settings.AccessToken != tt.settings.AccessToken {
				t.Fatalf("round trip envelope = %#v", parsed)
			}
			if parsed.State.LastBalance == nil || len(parsed.State.LastBalance.Subscriptions) != 1 || parsed.State.LastBalance.Subscriptions[0].ID != 7 {
				t.Fatalf("round trip state = %#v", parsed.State)
			}
		})
	}
}

func TestChannelManagementEnvelopeRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	positiveUserID := int64(1)
	zeroUserID := int64(0)
	negativeUserID := int64(-1)

	valid := func() ChannelManagementEnvelope {
		return ChannelManagementEnvelope{
			Kind: ChannelManagementKind, Version: ChannelManagementVersion, Profile: ChannelManagementProfileNewAPI,
			Settings: ChannelManagementSettings{BaseURL: "https://panel.example.com", AccessToken: "private-token", UserID: &positiveUserID},
		}
	}
	tests := []struct {
		name   string
		mutate func(*ChannelManagementEnvelope)
	}{
		{name: "wrong kind", mutate: func(e *ChannelManagementEnvelope) { e.Kind = "oauth" }},
		{name: "wrong version", mutate: func(e *ChannelManagementEnvelope) { e.Version = 2 }},
		{name: "unknown profile", mutate: func(e *ChannelManagementEnvelope) { e.Profile = "other" }},
		{name: "empty base URL", mutate: func(e *ChannelManagementEnvelope) { e.Settings.BaseURL = "" }},
		{name: "empty token", mutate: func(e *ChannelManagementEnvelope) { e.Settings.AccessToken = "" }},
		{name: "unsupported scheme", mutate: func(e *ChannelManagementEnvelope) { e.Settings.BaseURL = "ftp://panel.example.com" }},
		{name: "missing host", mutate: func(e *ChannelManagementEnvelope) { e.Settings.BaseURL = "https://" }},
		{name: "userinfo", mutate: func(e *ChannelManagementEnvelope) { e.Settings.BaseURL = "https://user:pass@panel.example.com" }},
		{name: "path", mutate: func(e *ChannelManagementEnvelope) { e.Settings.BaseURL = "https://panel.example.com/api" }},
		{name: "query", mutate: func(e *ChannelManagementEnvelope) { e.Settings.BaseURL = "https://panel.example.com/?a=b" }},
		{name: "fragment", mutate: func(e *ChannelManagementEnvelope) { e.Settings.BaseURL = "https://panel.example.com/#fragment" }},
		{name: "empty fragment", mutate: func(e *ChannelManagementEnvelope) { e.Settings.BaseURL = "https://panel.example.com/#" }},
		{name: "zero new api user ID", mutate: func(e *ChannelManagementEnvelope) { e.Settings.UserID = &zeroUserID }},
		{name: "negative new api user ID", mutate: func(e *ChannelManagementEnvelope) { e.Settings.UserID = &negativeUserID }},
		{name: "invalid checkin time", mutate: func(e *ChannelManagementEnvelope) { e.Settings.DailyCheckinTime = "9:00" }},
		{name: "enabled checkin without time", mutate: func(e *ChannelManagementEnvelope) { e.Settings.DailyCheckinEnabled = true }},
		{name: "sub2api user ID", mutate: func(e *ChannelManagementEnvelope) { e.Profile = ChannelManagementProfileSub2API }},
		{name: "sub2api daily enabled", mutate: func(e *ChannelManagementEnvelope) {
			e.Profile = ChannelManagementProfileSub2API
			e.Settings.UserID = nil
			e.Settings.DailyCheckinEnabled = true
			e.Settings.DailyCheckinTime = "09:00"
		}},
		{name: "sub2api daily time", mutate: func(e *ChannelManagementEnvelope) {
			e.Profile = ChannelManagementProfileSub2API
			e.Settings.UserID = nil
			e.Settings.DailyCheckinTime = "09:00"
		}},
		{name: "sub2api pro user ID", mutate: func(e *ChannelManagementEnvelope) { e.Profile = ChannelManagementProfileSub2APIPro }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			envelope := valid()
			tt.mutate(&envelope)
			if err := envelope.Validate(); err == nil {
				t.Fatalf("Validate() accepted %#v", envelope)
			}
		})
	}
}

func TestParseChannelManagementEnvelopeIsStrict(t *testing.T) {
	t.Parallel()
	valid := `{"kind":"channel_management","version":1,"profile":"sub2api","settings":{"base_url":"https://panel.example.com/","access_token":"private-token"},"state":{}}`
	parsed, err := ParseChannelManagementEnvelope(valid)
	if err != nil {
		t.Fatalf("ParseChannelManagementEnvelope() error = %v", err)
	}
	if parsed.Settings.BaseURL != "https://panel.example.com" {
		t.Fatalf("normalized base URL = %q", parsed.Settings.BaseURL)
	}
	for _, raw := range []string{
		`{"kind":"channel_management","version":1,"profile":"sub2api","settings":{"base_url":"https://panel.example.com","access_token":"private-token","unexpected":true},"state":{}}`,
		valid + ` {}`,
		``,
	} {
		if _, err := ParseChannelManagementEnvelope(raw); err == nil {
			t.Fatalf("ParseChannelManagementEnvelope() accepted %q", raw)
		}
	}
}

func TestConfigJSONDoesNotExposeChannelManagementEnvelope(t *testing.T) {
	t.Parallel()
	privateEnvelope := `{"kind":"channel_management","version":1,"profile":"sub2api","settings":{"base_url":"https://panel.example.com","access_token":"management-token-secret"},"state":{}}`
	cfg := &Config{Name: "managed", AuthType: AuthTypeAPIKey, OAuthCredential: privateEnvelope}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), "channel_management") || strings.Contains(string(raw), "management-token-secret") || strings.Contains(string(raw), "oauth_credential") {
		t.Fatalf("Config JSON leaked private management data: %s", raw)
	}
	if cfg.UsesOAuth() {
		t.Fatal("API Key channel with management envelope must not use OAuth")
	}
}
