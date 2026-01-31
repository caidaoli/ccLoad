package model

import (
	"testing"
	"time"
)

func TestModelEntry_Validate(t *testing.T) {
	t.Parallel()

	t.Run("trim_and_accept", func(t *testing.T) {
		entry := &ModelEntry{Model: "  gpt-4  ", RedirectModel: "  "}
		if err := entry.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
		if entry.Model != "gpt-4" {
			t.Fatalf("Model not trimmed: %q", entry.Model)
		}
		if entry.RedirectModel != "" {
			t.Fatalf("RedirectModel not trimmed: %q", entry.RedirectModel)
		}
	})

	t.Run("reject_empty", func(t *testing.T) {
		entry := &ModelEntry{Model: "   "}
		if err := entry.Validate(); err == nil {
			t.Fatal("expected error for empty model")
		}
	})

	t.Run("reject_illegal_model_chars", func(t *testing.T) {
		entry := &ModelEntry{Model: "gpt-4\nx"}
		if err := entry.Validate(); err == nil {
			t.Fatal("expected error for illegal chars in model")
		}
	})

	t.Run("reject_illegal_redirect_chars", func(t *testing.T) {
		entry := &ModelEntry{Model: "gpt-4", RedirectModel: "x\ry"}
		if err := entry.Validate(); err == nil {
			t.Fatal("expected error for illegal chars in redirect_model")
		}
	})
}

func TestConfig_SupportsModel(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ModelEntries: []ModelEntry{
			{Model: "m1"},
			{Model: "m2"},
		},
	}

	if !cfg.SupportsModel("m2") {
		t.Fatal("expected SupportsModel(m2)=true")
	}
	if cfg.SupportsModel("none") {
		t.Fatal("expected SupportsModel(none)=false")
	}
}

func TestConfig_IsCoolingDown(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	cfg := &Config{CooldownUntil: 1001}
	if !cfg.IsCoolingDown(now) {
		t.Fatal("expected cooling down when cooldown_until is in the future")
	}

	cfg.CooldownUntil = 1000
	if cfg.IsCoolingDown(now) {
		t.Fatal("expected not cooling down when cooldown_until equals now")
	}
}

func TestIsValidKeyStrategy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want bool
	}{
		{in: "", want: true},
		{in: KeyStrategySequential, want: true},
		{in: KeyStrategyRoundRobin, want: true},
		{in: "random", want: false},
	} {
		if got := IsValidKeyStrategy(tc.in); got != tc.want {
			t.Fatalf("IsValidKeyStrategy(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAPIKey_IsCoolingDown(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	key := &APIKey{CooldownUntil: 1001}
	if !key.IsCoolingDown(now) {
		t.Fatal("expected cooling down for APIKey")
	}
	key.CooldownUntil = 1000
	if key.IsCoolingDown(now) {
		t.Fatal("expected not cooling down when equals now")
	}
}

func TestDefaultHealthScoreConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultHealthScoreConfig()
	if cfg.Enabled {
		t.Fatal("default health score config should be disabled")
	}
	if cfg.SuccessRatePenaltyWeight <= 0 || cfg.WindowMinutes <= 0 || cfg.UpdateIntervalSeconds <= 0 || cfg.MinConfidentSample <= 0 {
		t.Fatalf("unexpected default config: %+v", cfg)
	}
}
