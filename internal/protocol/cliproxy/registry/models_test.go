package registry

import (
	"slices"
	"testing"
)

func TestLookupThinkingLevels(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{"gpt-6-chat-thinking", []string{"low", "medium", "high", "xhigh", "max"}},
		{"claude-sonnet-5-5", []string{"low", "medium", "high", "xhigh", "max"}},
		{"claude-opus-5-5-high", []string{"low", "medium", "high"}},
		{"claude-sonnet-5-5-high", []string{"low", "medium", "high"}},
		// Catalogs Lookup leaves out still provide levels.
		{"kimi-k3", []string{"low", "high", "max"}},
		{"grok-4.7", []string{"low", "medium", "high", "xhigh"}},
		// The gemini catalog lists only minimal,high; aistudio's entry is fuller.
		{"gemini-3.1-flash-lite-preview", []string{"minimal", "low", "medium", "high"}},
		{"gemini-2.5-pro", nil},
		{"no-such-model", nil},
		{"", nil},
	} {
		if got := LookupThinkingLevels(tc.model); !slices.Equal(got, tc.want) {
			t.Errorf("LookupThinkingLevels(%q)=%v, want %v", tc.model, got, tc.want)
		}
	}
	if info := LookupModelInfo("kimi-k3"); info != nil {
		t.Fatalf("Lookup must keep excluding catalogs without an executor, got %+v", info)
	}
}

func TestCatalogModelIDsCodexChatThinking(t *testing.T) {
	for _, catalog := range []string{"codex-free", "codex-team", "codex-plus", "codex-pro"} {
		if ids := CatalogModelIDs(catalog); !slices.Contains(ids, "gpt-6-chat-thinking") {
			t.Errorf("CatalogModelIDs(%q)=%v, missing gpt-6-chat-thinking", catalog, ids)
		}
	}
}

func TestLookupModelInfoGeminiVariants(t *testing.T) {
	for _, tc := range []struct {
		model     string
		catalog   string
		levels    []string
		budgetMax int
	}{
		{"gemini-4-hydrogen", "gemini", []string{"low", "medium", "high", "xhigh"}, 0},
		{"gemini-4-hydrogen-xhigh", "antigravity", []string{"low", "medium", "high", "xhigh"}, 65535},
		{"gemini-3.8-flash-tiered", "antigravity", []string{"low", "medium", "high"}, 65535},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if !slices.Contains(CatalogModelIDs(tc.catalog), tc.model) {
				t.Fatalf("catalog %q does not list %q", tc.catalog, tc.model)
			}
			for _, provider := range []string{tc.catalog, "gemini"} {
				info := LookupModelInfo(tc.model, provider)
				if info == nil || info.Thinking == nil {
					t.Fatalf("provider %q missing capabilities for %q: %+v", provider, tc.model, info)
				}
				if info.ContextLength != 1048576 || info.MaxCompletionTokens != 65536 {
					t.Fatalf("provider %q limits = %d/%d, want 1048576/65536", provider, info.ContextLength, info.MaxCompletionTokens)
				}
				if !slices.Equal(info.Thinking.Levels, tc.levels) || info.Thinking.Max != tc.budgetMax ||
					info.Thinking.DynamicAllowed != (tc.budgetMax != 0) {
					t.Fatalf("provider %q thinking = %+v, want levels %v and budget max %d", provider, info.Thinking, tc.levels, tc.budgetMax)
				}
				if !slices.Equal(info.InputModalities, []string{"text", "image", "audio", "video"}) {
					t.Fatalf("provider %q input modalities = %v", provider, info.InputModalities)
				}
				if info.NativeCapabilities == nil || info.NativeCapabilities.WebSearch == nil || !*info.NativeCapabilities.WebSearch {
					t.Fatalf("provider %q missing native search capability", provider)
				}
			}
			if got := LookupThinkingLevels(tc.model); !slices.Equal(got, tc.levels) {
				t.Fatalf("thinking levels = %v, want %v", got, tc.levels)
			}
			if info := LookupModelInfo(tc.model, "claude"); info != nil {
				t.Fatalf("Gemini variant leaked into Claude provider: %+v", info)
			}
		})
	}
	if info := LookupModelInfo("gemini-4-hydrogen", "antigravity"); info != nil {
		t.Fatalf("Google model ID leaked into Antigravity provider: %+v", info)
	}
}
