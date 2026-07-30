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

	t.Run("passthrough_backfill_redirect_eq_model_allowed", func(t *testing.T) {
		// 编辑回填：直通条目的 RedirectModel 被填为 Model（可能含通配符），Validate 须放行，
		// 由 ToConfig 在保存时清空为透传。
		entry := &ModelEntry{Model: "gpt-*", RedirectModel: "gpt-*"}
		if err := entry.Validate(); err != nil {
			t.Fatalf("expected no error for redirect==model (passthrough backfill), got %v", err)
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
		{"model-?", "model-模", true},         // '?' 匹配一个多字节字符（按 rune）
		{"model-?", "model-模x", false},       // '?' 只匹配一个字符
		{"模型-*", "模型-pro", true},             // 中文前缀通配
		{"模?-x", "模型-x", true},               // 中文 + '?' 匹配单字符
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

// TestConfig_PrecisePassthroughNotOverriddenByWildcard 验证 P1-1：
// 精确条目为直通（空重定向）时，不得被后续通配重定向覆盖。
func TestConfig_PrecisePassthroughNotOverriddenByWildcard(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		ModelEntries: []ModelEntry{
			{Model: "claude-opus-4", RedirectModel: ""},        // 精确直通
			{Model: "claude-opus-*", RedirectModel: "glm-5.2"}, // 通配重定向
		},
	}
	if r, ok := cfg.GetRedirectModel("claude-opus-4"); ok || r != "" {
		t.Fatalf("precise passthrough must not redirect, got (%q,%v)", r, ok)
	}
	if !cfg.SupportsModel("claude-opus-4") {
		t.Fatal("precise entry must be supported")
	}
	// 通配仍对未精确列出的模型生效
	if r, ok := cfg.GetRedirectModel("claude-opus-9"); !ok || r != "glm-5.2" {
		t.Fatalf("wildcard should redirect claude-opus-9, got (%q,%v)", r, ok)
	}
}

// TestConfig_DefaultCheckModel 验证 P1-2：定时检测留空时选取首个具体可发模型，
// 不把通配字面值（如 gpt-*）当真实模型发给上游。
func TestConfig_DefaultCheckModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []ModelEntry
		want    string
		ok      bool
	}{
		{"precise", []ModelEntry{{Model: "gpt-4.1"}}, "gpt-4.1", true},
		{"wildcard_redirect", []ModelEntry{{Model: "gpt-*", RedirectModel: "glm-5.2"}}, "glm-5.2", true},
		{"passthrough_wildcard", []ModelEntry{{Model: "gpt-*"}}, "", false},
		{"precise_before_wildcard", []ModelEntry{{Model: "gpt-*", RedirectModel: "glm-5.2"}, {Model: "gpt-4.1"}}, "gpt-4.1", true},
		{"all_passthrough_wildcard", []ModelEntry{{Model: "gpt-*"}, {Model: "claude-*"}}, "", false},
		{"empty", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{ModelEntries: tc.entries}
			got, ok := cfg.DefaultCheckModel()
			if got != tc.want || ok != tc.ok {
				t.Fatalf("DefaultCheckModel() = (%q,%v), want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestConfig_GetConcreteModels 验证 P2#2：模型列表 API 须只返回具体模型名，
// 不把通配模式（如 gpt-*）暴露给客户端。
func TestConfig_GetConcreteModels(t *testing.T) {
	t.Parallel()
	cfg := &Config{ModelEntries: []ModelEntry{
		{Model: "gpt-*", RedirectModel: "glm-5.2"},
		{Model: "gpt-4.1"},
		{Model: "claude-?-mini"},
	}}
	got := cfg.GetConcreteModels()
	if len(got) != 1 || got[0] != "gpt-4.1" {
		t.Fatalf("GetConcreteModels() = %v, want [gpt-4.1]", got)
	}
}

// TestConfig_MultipleWildcardOrder 验证多通配条目按声明顺序首个命中。
func TestConfig_MultipleWildcardOrder(t *testing.T) {
	t.Parallel()
	cfg := &Config{ModelEntries: []ModelEntry{
		{Model: "gpt-*", RedirectModel: "A"},
		{Model: "gpt-4*", RedirectModel: "B"},
	}}
	if r, ok := cfg.GetRedirectModel("gpt-4.1"); !ok || r != "A" {
		t.Fatalf("expected first-match A, got (%q,%v)", r, ok)
	}
}
