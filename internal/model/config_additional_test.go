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

	t.Run("accept_wildcard_in_model", func(t *testing.T) {
		entry := &ModelEntry{Model: "claude-opus-*", RedirectModel: "glm-5.2"}
		if err := entry.Validate(); err != nil {
			t.Fatalf("expected no error for wildcard in model, got %v", err)
		}
	})

	t.Run("reject_wildcard_in_redirect", func(t *testing.T) {
		entry := &ModelEntry{Model: "claude-opus-*", RedirectModel: "glm-*"}
		if err := entry.Validate(); err == nil {
			t.Fatal("expected error for '*' in redirect_model")
		}
	})

	t.Run("reject_questionmark_in_redirect", func(t *testing.T) {
		entry := &ModelEntry{Model: "gpt-?", RedirectModel: "glm-?"}
		if err := entry.Validate(); err == nil {
			t.Fatal("expected error for '?' in redirect_model")
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
	if cfg.EnableTTFBScore {
		t.Fatal("default ttfb score should be disabled")
	}
	if cfg.TTFBPenaltyWeight <= 0 || cfg.TTFBMaxSlowRatio <= 0 || cfg.TTFBMinConfidentSample <= 0 {
		t.Fatalf("unexpected ttfb defaults: %+v", cfg)
	}
}

func TestGetURLs_SingleURL(t *testing.T) {
	c := &Config{URL: "https://api.openai.com"}
	urls := c.GetURLs()
	if len(urls) != 1 || urls[0] != "https://api.openai.com" {
		t.Errorf("expected [https://api.openai.com], got %v", urls)
	}
}

func TestGetURLs_MultipleURLs(t *testing.T) {
	c := &Config{URL: "https://us.api.openai.com\nhttps://eu.api.openai.com"}
	urls := c.GetURLs()
	if len(urls) != 2 {
		t.Fatalf("expected 2 urls, got %d", len(urls))
	}
	if urls[0] != "https://us.api.openai.com" || urls[1] != "https://eu.api.openai.com" {
		t.Errorf("unexpected urls: %v", urls)
	}
}

func TestGetURLs_EmptyLinesIgnored(t *testing.T) {
	c := &Config{URL: "https://a.com\n\n  \nhttps://b.com\n"}
	urls := c.GetURLs()
	if len(urls) != 2 {
		t.Fatalf("expected 2 urls (skip blanks), got %d: %v", len(urls), urls)
	}
}

func TestGetURLs_DuplicateURLsDeduped(t *testing.T) {
	c := &Config{URL: "https://a.com\nhttps://b.com\nhttps://a.com\nhttps://b.com"}
	urls := c.GetURLs()
	if len(urls) != 2 {
		t.Fatalf("expected 2 unique urls, got %d: %v", len(urls), urls)
	}
	if urls[0] != "https://a.com" || urls[1] != "https://b.com" {
		t.Fatalf("unexpected urls order/content: %v", urls)
	}
}

func TestGetURLs_TrailingSlashPreserved(t *testing.T) {
	c := &Config{URL: "https://api.openai.com/v1/"}
	urls := c.GetURLs()
	if urls[0] != "https://api.openai.com/v1/" {
		t.Errorf("trailing slash should be preserved, got %q", urls[0])
	}
}

func TestGetURLs_SingleURLTrimmed(t *testing.T) {
	c := &Config{URL: "  https://api.openai.com/v1  "}
	urls := c.GetURLs()
	if len(urls) != 1 || urls[0] != "https://api.openai.com/v1" {
		t.Fatalf("expected trimmed single url, got %v", urls)
	}
}

func TestGetURLs_WhitespaceOnlyReturnsEmpty(t *testing.T) {
	c := &Config{URL: "\n \n\t\n"}
	urls := c.GetURLs()
	if len(urls) != 0 {
		t.Fatalf("expected empty urls for whitespace-only input, got %v", urls)
	}
}

func TestMatchModelGlob(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"claude-opus-*", "claude-opus-4", true},
		{"claude-opus-*", "claude-opus-5-20250929", true},
		{"claude-opus-*", "claude-opus-", true}, // '*' 匹配空串
		{"claude-opus-*", "claude-opus", false}, // 前缀要求末尾 '-'
		{"claude-opus-*", "claude-sonnet-4", false},
		{"*-opus-*", "claude-opus-4", true},
		{"*-opus-*", "claude-opus", false},
		{"gpt-?-mini", "gpt-5-mini", true},
		{"gpt-?-mini", "gpt-55-mini", false}, // '?' 只吃一个字符
		{"gpt-?-mini", "gpt-mini", false},    // '?' 不匹配空
		{"*", "anything", true},
		{"*", "", true},
		{"?", "x", true},
		{"?", "", false},
		{"a*b?d", "aXxbYd", true},
		{"claude-opus-4", "claude-opus-4", true}, // 无通配精确
		{"claude-opus-4", "claude-opus-5", false},
		{"Claude-*", "claude-opus-4", false}, // 大小写敏感
	} {
		if got := matchModelGlob(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchModelGlob(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestConfig_WildcardModelMatch(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		ModelEntries: []ModelEntry{
			{Model: "claude-opus-4", RedirectModel: "glm-4"},
			{Model: "claude-opus-*", RedirectModel: "glm-5.2"},
			{Model: "gpt-?-mini", RedirectModel: "gpt-5-mini"},
		},
	}

	// SupportsModel：精确优先，未命中再回退到通配模式
	if !cfg.SupportsModel("claude-opus-4") { // 精确
		t.Fatal("expected precise SupportsModel(claude-opus-4)=true")
	}
	if !cfg.SupportsModel("claude-opus-9") { // claude-opus-*
		t.Fatal("expected pattern SupportsModel(claude-opus-9)=true")
	}
	if !cfg.SupportsModel("gpt-7-mini") { // gpt-?-mini
		t.Fatal("expected pattern SupportsModel(gpt-7-mini)=true")
	}
	if cfg.SupportsModel("claude-sonnet-4") {
		t.Fatal("expected SupportsModel(claude-sonnet-4)=false")
	}

	// GetRedirectModel：精确优先于通配模式
	if r, ok := cfg.GetRedirectModel("claude-opus-4"); !ok || r != "glm-4" {
		t.Fatalf("precise redirect expected glm-4, got %q %v", r, ok)
	}
	if r, ok := cfg.GetRedirectModel("claude-opus-9"); !ok || r != "glm-5.2" {
		t.Fatalf("pattern redirect expected glm-5.2, got %q %v", r, ok)
	}
	if r, ok := cfg.GetRedirectModel("gpt-7-mini"); !ok || r != "gpt-5-mini" {
		t.Fatalf("pattern redirect expected gpt-5-mini, got %q %v", r, ok)
	}
	if _, ok := cfg.GetRedirectModel("claude-sonnet-4"); ok {
		t.Fatal("expected no redirect for claude-sonnet-4")
	}
}

func TestConfig_WildcardPassthrough(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		ModelEntries: []ModelEntry{
			{Model: "claude-*"}, // 无重定向：声明支持，直通上游
		},
	}
	if !cfg.SupportsModel("claude-sonnet-4") {
		t.Fatal("expected passthrough pattern SupportsModel=true")
	}
	if r, ok := cfg.GetRedirectModel("claude-sonnet-4"); ok {
		t.Fatalf("passthrough should not redirect, got %q", r)
	}
}

func TestConfig_FuzzyMatchSkipsPattern(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		ModelEntries: []ModelEntry{
			{Model: "claude-opus-*"},     // 通配模式，应被模糊匹配跳过
			{Model: "claude-sonnet-4-5"}, // 精确条目
		},
	}
	matched, ok := cfg.FuzzyMatchModel("sonnet")
	if !ok || matched != "claude-sonnet-4-5" {
		t.Fatalf("expected fuzzy match claude-sonnet-4-5, got %q %v", matched, ok)
	}
	if _, ok := cfg.FuzzyMatchModel("opus"); ok {
		t.Fatal("expected no fuzzy match for opus (pattern entry skipped)")
	}
}
