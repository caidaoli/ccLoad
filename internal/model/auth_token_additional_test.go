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
		ID:                123,
		Token:             "hash",
		IsActive:          true,
		CostUsedMicroUSD:  250_000, // $0.25
		CostLimitMicroUSD: 2_000_000,
		AllowedModels:     []string{"gpt-4"},
	}

	b, err := json.Marshal(token)
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	var got struct {
		CostUsedUSD  float64 `json:"cost_used_usd"`
		CostLimitUSD float64 `json:"cost_limit_usd"`
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
}
