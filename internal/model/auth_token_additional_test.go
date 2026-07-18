package model

import (
	"encoding/json"
	"math"
	"testing"
)

func TestAuthToken_IsModelAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		allowed      []string
		model        string
		expectedBool bool
	}{
		{name: "empty_allowed_models_allows_any", allowed: nil, model: "gpt-4", expectedBool: true},
		{name: "case_insensitive_match", allowed: []string{"GPT-4", "claude"}, model: "gpt-4", expectedBool: true},
		{name: "no_match", allowed: []string{"gpt-4", "claude"}, model: "gemini", expectedBool: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := &AuthToken{AllowedModels: tt.allowed}
			if got := token.IsModelAllowed(tt.model); got != tt.expectedBool {
				t.Fatalf("IsModelAllowed(%q) = %v, want %v", tt.model, got, tt.expectedBool)
			}
		})
	}
}

func TestNormalizeChannelRestrictionMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mode    string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to allow", mode: "", want: ChannelRestrictionModeAllow},
		{name: "allow", mode: ChannelRestrictionModeAllow, want: ChannelRestrictionModeAllow},
		{name: "deny", mode: ChannelRestrictionModeDeny, want: ChannelRestrictionModeDeny},
		{name: "invalid", mode: "denyy", wantErr: true},
		{name: "non canonical case", mode: "DENY", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeChannelRestrictionMode(tt.mode)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeChannelRestrictionMode(%q) = %q, want error", tt.mode, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeChannelRestrictionMode(%q) failed: %v", tt.mode, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeChannelRestrictionMode(%q) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

func TestChannelRestriction_Allows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      string
		ids       []int64
		channelID int64
		want      bool
	}{
		{name: "nil list allows any", ids: nil, channelID: 42, want: true},
		{name: "empty list allows any", ids: []int64{}, channelID: 42, want: true},
		{name: "allow listed channel", mode: ChannelRestrictionModeAllow, ids: []int64{2, 42}, channelID: 42, want: true},
		{name: "allow rejects missing channel", mode: ChannelRestrictionModeAllow, ids: []int64{2, 7}, channelID: 42, want: false},
		{name: "deny rejects listed channel", mode: ChannelRestrictionModeDeny, ids: []int64{2, 42}, channelID: 42, want: false},
		{name: "deny allows missing channel", mode: ChannelRestrictionModeDeny, ids: []int64{2, 7}, channelID: 42, want: true},
		{name: "empty mode uses allow", mode: "", ids: []int64{2}, channelID: 2, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restriction, err := NewChannelRestriction(tt.mode, tt.ids)
			if err != nil {
				t.Fatalf("NewChannelRestriction failed: %v", err)
			}
			if got := restriction.Allows(tt.channelID); got != tt.want {
				t.Fatalf("Allows(%d) = %v, want %v", tt.channelID, got, tt.want)
			}
		})
	}
}

func TestAuthToken_CostConversions(t *testing.T) {
	t.Parallel()

	token := &AuthToken{
		CostUsedMicroUSD:  1_230_000, // $1.23
		CostLimitMicroUSD: 4_500_000, // $4.50
	}
	if got := token.CostUsedUSD(); math.Abs(got-1.23) > 1e-9 {
		t.Fatalf("CostUsedUSD() = %v, want 1.23", got)
	}
	if got := token.CostLimitUSD(); math.Abs(got-4.5) > 1e-9 {
		t.Fatalf("CostLimitUSD() = %v, want 4.5", got)
	}

	token.SetCostLimitUSD(0)
	if token.CostLimitMicroUSD != 0 {
		t.Fatalf("SetCostLimitUSD(0) should reset to 0 microUSD, got %d", token.CostLimitMicroUSD)
	}

	token.SetCostLimitUSD(1.5)
	if token.CostLimitMicroUSD != 1_500_000 {
		t.Fatalf("SetCostLimitUSD(1.5) microUSD = %d, want 1500000", token.CostLimitMicroUSD)
	}
}

func TestAuthToken_MarshalJSON_ExposesCostFields(t *testing.T) {
	t.Parallel()

	token := AuthToken{
		ID:                     123,
		Token:                  "hash",
		IsActive:               true,
		CostUsedMicroUSD:       250_000, // $0.25
		CostLimitMicroUSD:      2_000_000,
		AllowedModels:          []string{"gpt-4"},
		AllowedChannelIDs:      []int64{11, 22},
		ChannelRestrictionMode: ChannelRestrictionModeDeny,
		MaxConcurrency:         3,
	}

	b, err := json.Marshal(token)
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	var got struct {
		CostUsedUSD            float64 `json:"cost_used_usd"`
		CostLimitUSD           float64 `json:"cost_limit_usd"`
		AllowedChannelID       []int64 `json:"allowed_channel_ids"`
		ChannelRestrictionMode string  `json:"channel_restriction_mode"`
		MaxConcurrency         int     `json:"max_concurrency"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if math.Abs(got.CostUsedUSD-0.25) > 1e-9 {
		t.Fatalf("cost_used_usd = %#v, want 0.25", got.CostUsedUSD)
	}
	if math.Abs(got.CostLimitUSD-2.0) > 1e-9 {
		t.Fatalf("cost_limit_usd = %#v, want 2.0", got.CostLimitUSD)
	}
	if len(got.AllowedChannelID) != 2 || got.AllowedChannelID[0] != 11 || got.AllowedChannelID[1] != 22 {
		t.Fatalf("allowed_channel_ids = %#v, want [11 22]", got.AllowedChannelID)
	}
	if got.ChannelRestrictionMode != ChannelRestrictionModeDeny {
		t.Fatalf("channel_restriction_mode = %#v, want %q", got.ChannelRestrictionMode, ChannelRestrictionModeDeny)
	}
	if got.MaxConcurrency != 3 {
		t.Fatalf("max_concurrency = %#v, want 3", got.MaxConcurrency)
	}
}

func TestAuthToken_MarshalJSON_RejectsInvalidChannelRestrictionMode(t *testing.T) {
	t.Parallel()

	_, err := json.Marshal(AuthToken{ChannelRestrictionMode: "denyy"})
	if err == nil {
		t.Fatal("expected invalid channel_restriction_mode to fail JSON marshaling")
	}
}
