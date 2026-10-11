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
		{"claude-sonnet-5-5", []string{"low", "medium", "high", "xhigh", "max"}},
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
