package model

import (
	"testing"
	"time"
)

// 渠道条目字面写成 gpt-5.6-luna(max) 时，对外暴露的名字和选路索引必须一致，
// 否则会出现「模型列表里看得到、请求却没有可用渠道」。
func TestThinkingSuffixEntriesAreRoutableByBaseName(t *testing.T) {
	t.Parallel()

	cfg := &Config{ModelEntries: []ModelEntry{
		{Model: "gpt-5.6-luna(max)", RedirectModel: "gpt-5.6-luna-2026"},
		{Model: "claude-opus-4-6"},
	}}

	models := cfg.GetModels()
	want := []string{"gpt-5.6-luna", "claude-opus-4-6"}
	if len(models) != len(want) {
		t.Fatalf("GetModels() = %v, want %v", models, want)
	}
	for i, name := range want {
		if models[i] != name {
			t.Fatalf("GetModels()[%d] = %q, want %q", i, models[i], name)
		}
	}

	if !cfg.SupportsModel("gpt-5.6-luna") {
		t.Fatal("base name must resolve to the suffixed entry")
	}
	if redirect, ok := cfg.GetRedirectModel("gpt-5.6-luna"); !ok || redirect != "gpt-5.6-luna-2026" {
		t.Fatalf("GetRedirectModel(base) = %q, %v; want the suffixed entry's redirect", redirect, ok)
	}
}

// 显式配置的基名条目不能被后缀条目派生的别名顶掉。
func TestThinkingSuffixAliasDoesNotShadowExplicitEntry(t *testing.T) {
	t.Parallel()

	cfg := &Config{ModelEntries: []ModelEntry{
		{Model: "gpt-5.6-luna", RedirectModel: "explicit"},
		{Model: "gpt-5.6-luna(max)", RedirectModel: "alias"},
	}}

	if redirect, ok := cfg.GetRedirectModel("gpt-5.6-luna"); !ok || redirect != "explicit" {
		t.Fatalf("GetRedirectModel(base) = %q, %v; want explicit", redirect, ok)
	}
	if models := cfg.GetModels(); len(models) != 1 || models[0] != "gpt-5.6-luna" {
		t.Fatalf("GetModels() = %v, want a single deduplicated base name", models)
	}
}

func TestFuzzyMatchModelReturnsBaseNameForSuffixedEntry(t *testing.T) {
	t.Parallel()

	cfg := &Config{ModelEntries: []ModelEntry{
		{Model: "gpt-5.6-luna(max)"},
		{Model: "gpt-5.6-luna"},
	}}

	matched, ok := cfg.FuzzyMatchModel("luna")
	if !ok || matched != "gpt-5.6-luna" {
		t.Fatalf("FuzzyMatchModel() = %q, %v; want gpt-5.6-luna", matched, ok)
	}
}

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
			{Model: "m1", RedirectModel: "upstream-m1", Disabled: true},
			{Model: "m2"},
		},
	}

	if !cfg.SupportsModel("m2") {
		t.Fatal("expected SupportsModel(m2)=true")
	}
	if cfg.SupportsModel("none") {
		t.Fatal("expected SupportsModel(none)=false")
	}
	if cfg.SupportsModel("m1") {
		t.Fatal("disabled model must not be supported")
	}
	if redirect, ok := cfg.GetRedirectModel("m1"); ok || redirect != "" {
		t.Fatalf("disabled model redirect must not resolve, got (%q, %v)", redirect, ok)
	}
	if models := cfg.GetModels(); len(models) != 1 || models[0] != "m2" {
		t.Fatalf("GetModels()=%v, want only enabled model m2", models)
	}
}

func TestConfig_WildcardModelSupportsAnyModelWithoutRedirect(t *testing.T) {
	t.Parallel()
	cfg := &Config{ModelEntries: []ModelEntry{{Model: "*"}}}
	if !cfg.SupportsModel("gpt-5.4") || !cfg.SupportsModel("future-codex-model") {
		t.Fatal("wildcard channel must support arbitrary models")
	}
	if redirect, ok := cfg.GetRedirectModel("gpt-5.4"); ok || redirect != "" {
		t.Fatalf("wildcard must not rewrite model, got (%q, %v)", redirect, ok)
	}
}

func TestConfig_AvailableTimeSupportsAllDayAndOvernightWindows(t *testing.T) {
	t.Parallel()

	allDay := &Config{}
	if !allDay.IsAvailableAt(time.Date(2026, 1, 1, 3, 0, 0, 0, time.Local)) {
		t.Fatal("empty availability must allow all times")
	}

	cfg := &Config{AvailableTimeStart: "22:00", AvailableTimeEnd: "08:00"}
	if err := cfg.NormalizeAvailableTime(); err != nil {
		t.Fatalf("normalize overnight window: %v", err)
	}
	for _, tc := range []struct {
		hour int
		want bool
	}{
		{23, true}, {7, true}, {12, false}, {8, false}, {22, true},
	} {
		at := time.Date(2026, 1, 1, tc.hour, 0, 0, 0, time.Local)
		if got := cfg.IsAvailableAt(at); got != tc.want {
			t.Fatalf("availability at %02d:00 = %v, want %v", tc.hour, got, tc.want)
		}
	}
}

func TestConfig_AvailableTimeRejectsPartialOrMalformedWindow(t *testing.T) {
	t.Parallel()
	for _, cfg := range []*Config{
		{AvailableTimeStart: "22:00"},
		{AvailableTimeEnd: "08:00"},
		{AvailableTimeStart: "25:00", AvailableTimeEnd: "08:00"},
	} {
		if err := cfg.NormalizeAvailableTime(); err == nil {
			t.Fatalf("expected invalid availability for %+v", cfg)
		}
	}
}

func TestConfig_AuthTypeIsIndependentFromProtocol(t *testing.T) {
	t.Parallel()
	legacy := &Config{}
	if got := legacy.GetAuthType(); got != AuthTypeAPIKey {
		t.Fatalf("legacy GetAuthType()=%q, want %q", got, AuthTypeAPIKey)
	}
	codex := &Config{AuthType: " CODEX_OAUTH ", OAuthCredential: "secret"}
	if !codex.UsesCodexOAuth() {
		t.Fatal("expected Codex OAuth auth type")
	}
	clone := codex.Clone()
	if clone.AuthType != codex.AuthType || clone.OAuthCredential != "secret" {
		t.Fatalf("Clone() lost private auth state: %#v", clone)
	}
	if got := NormalizeAuthType("codex"); got != "" {
		t.Fatalf("historical protocol value normalized as auth type: %q", got)
	}
	antigravity := &Config{
		AuthType: AuthTypeAntigravityOAuth, OAuthCredential: "gravity-secret",
		AntigravityAccessToken: "gravity-at", AntigravityProjectID: "gravity-project",
	}
	if !antigravity.UsesAntigravityOAuth() || !antigravity.UsesOAuth() {
		t.Fatal("expected Antigravity OAuth auth type")
	}
	gravityClone := antigravity.Clone()
	if gravityClone.OAuthCredential != "gravity-secret" || gravityClone.AntigravityAccessToken != "gravity-at" || gravityClone.AntigravityProjectID != "gravity-project" {
		t.Fatalf("Clone() lost Antigravity auth state: %#v", gravityClone)
	}
	xai := &Config{AuthType: AuthTypeXAIOAuth, OAuthCredential: "xai-secret"}
	if !xai.UsesXAIOAuth() || !xai.UsesOAuth() {
		t.Fatal("expected xAI OAuth auth type")
	}
}

func TestConfig_ProtocolTransformMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mode     string
		wantMode string
	}{
		{name: "default auto", wantMode: ProtocolTransformModeAuto},
		{name: "explicit auto", mode: "AUTO", wantMode: ProtocolTransformModeAuto},
		{name: "upstream", mode: ProtocolTransformModeUpstream, wantMode: ProtocolTransformModeUpstream},
		{name: "local", mode: ProtocolTransformModeLocal, wantMode: ProtocolTransformModeLocal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{ProtocolTransformMode: tt.mode}
			if got := cfg.GetProtocolTransformMode(); got != tt.wantMode {
				t.Fatalf("GetProtocolTransformMode()=%q, want %q", got, tt.wantMode)
			}
			if got := cfg.Clone().GetProtocolTransformMode(); got != tt.wantMode {
				t.Fatalf("Clone mode=%q, want %q", got, tt.wantMode)
			}
		})
	}

	if got := NormalizeProtocolTransformMode("invalid"); got != "" {
		t.Fatalf("NormalizeProtocolTransformMode(invalid)=%q, want empty", got)
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

func TestConfig_ChannelURLs(t *testing.T) {
	t.Parallel()

	c := &Config{URLs: ChannelURLs{
		{URL: "https://api.openai.com", Protocols: []string{"CODEX", "openai", "codex"}},
		{URL: "https://api.example.com/v1/messages", Exact: true},
	}}
	if err := c.URLs.Normalize(); err != nil {
		t.Fatalf("Normalize() unexpected error: %v", err)
	}

	if got := c.URLs[0].Protocols; len(got) != 2 || got[0] != "codex" || got[1] != "openai" {
		t.Fatalf("normalized protocols = %v, want configured order [codex openai]", got)
	}
	if got := c.GetURLs(); len(got) != 2 || got[0] != "https://api.openai.com" || got[1] != "https://api.example.com/v1/messages#" {
		t.Fatalf("GetURLs() = %v", got)
	}
	if !c.URLs[0].SupportsProtocol("openai") || c.URLs[0].SupportsProtocol("anthropic") {
		t.Fatalf("explicit protocol capability not enforced: %+v", c.URLs[0])
	}
	if !c.URLs[1].SupportsProtocol("anthropic") || !c.URLs[1].UsesAutomaticProtocolDetection() {
		t.Fatalf("empty protocols must mean automatic detection: %+v", c.URLs[1])
	}

	clone := c.Clone()
	clone.URLs[0].Protocols[0] = "anthropic"
	if c.URLs[0].Protocols[0] != "codex" {
		t.Fatal("Clone() shares URL protocol storage")
	}
}

func TestChannelURLs_NormalizeRejectsInvalidData(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		urls ChannelURLs
	}{
		{name: "empty URL set", urls: ChannelURLs{}},
		{name: "empty URL", urls: ChannelURLs{{URL: "  "}}},
		{name: "unsupported protocol", urls: ChannelURLs{{URL: "https://api.example.com", Protocols: []string{"grpc"}}}},
		{name: "duplicate runtime URL", urls: ChannelURLs{{URL: "https://api.example.com"}, {URL: " https://api.example.com "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.urls.Normalize(); err == nil {
				t.Fatal("Normalize() expected error")
			}
		})
	}
}

func TestConfig_AnthropicOAuthAuthType(t *testing.T) {
	cfg := &Config{AuthType: AuthTypeAnthropicOAuth}
	if NormalizeAuthType(AuthTypeAnthropicOAuth) != AuthTypeAnthropicOAuth ||
		!cfg.UsesAnthropicOAuth() || !cfg.UsesOAuth() || cfg.UsesXAIOAuth() {
		t.Fatalf("Anthropic OAuth auth type was not isolated: %+v", cfg)
	}
}

func TestConfig_CursorOAuthAuthType(t *testing.T) {
	cfg := &Config{AuthType: AuthTypeCursorOAuth}
	if NormalizeAuthType(AuthTypeCursorOAuth) != AuthTypeCursorOAuth ||
		!cfg.UsesCursorOAuth() || !cfg.UsesOAuth() || cfg.UsesZAIOAuth() {
		t.Fatalf("Cursor OAuth auth type was not isolated: %+v", cfg)
	}
}
