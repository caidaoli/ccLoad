package oauthcost

import (
	"testing"
	"time"
)

func TestMonthlyQuotaRolloverClampsToAnchorDay(t *testing.T) {
	t.Parallel()
	jan31 := time.Date(2027, time.January, 31, 8, 0, 0, 0, time.UTC)
	usage := Reconcile(nil, []Sample{{
		Key: "test|monthly", WindowSeconds: 30 * 24 * 60 * 60,
		ResetAt: jan31,
	}}, jan31.Add(-24*time.Hour))
	if usage == nil || len(usage.Windows) != 1 || usage.Windows[0].ResetDay != 31 {
		t.Fatalf("initial monthly usage = %#v", usage)
	}
	w := usage.Windows[0]
	for _, test := range []struct {
		at        time.Time
		wantStart time.Time
		wantReset time.Time
	}{
		{at: jan31, wantStart: jan31, wantReset: time.Date(2027, time.February, 28, 8, 0, 0, 0, time.UTC)},
		{at: time.Date(2027, time.February, 28, 8, 0, 0, 0, time.UTC), wantStart: time.Date(2027, time.February, 28, 8, 0, 0, 0, time.UTC), wantReset: time.Date(2027, time.March, 31, 8, 0, 0, 0, time.UTC)},
		{at: time.Date(2027, time.March, 31, 8, 0, 0, 0, time.UTC), wantStart: time.Date(2027, time.March, 31, 8, 0, 0, 0, time.UTC), wantReset: time.Date(2027, time.April, 30, 8, 0, 0, 0, time.UTC)},
	} {
		changed, err := AddStandardCost(usage, test.at, "", 1)
		if err != nil || !changed {
			t.Fatalf("AddStandardCost(%s) = (%t, %v)", test.at, changed, err)
		}
		if w.StartedAt != test.wantStart.Unix() || w.ResetAt != test.wantReset.Unix() ||
			w.StandardCostMicroUSD != 1 {
			t.Fatalf("monthly window after %s = %#v", test.at, w)
		}
	}

	leapJan31 := time.Date(2028, time.January, 31, 8, 0, 0, 0, time.UTC)
	leap := Reconcile(nil, []Sample{{
		Key: "test|monthly", WindowSeconds: 30 * 24 * 60 * 60,
		ResetAt: leapJan31,
	}}, leapJan31.Add(-time.Hour))
	if changed, err := AddStandardCost(leap, leapJan31, "", 1); err != nil || !changed {
		t.Fatalf("leap rollover = (%t, %v)", changed, err)
	}
	wantLeapReset := time.Date(2028, time.February, 29, 8, 0, 0, 0, time.UTC)
	if leap.Windows[0].ResetAt != wantLeapReset.Unix() {
		t.Fatalf("leap reset = %s, want %s", time.Unix(leap.Windows[0].ResetAt, 0), wantLeapReset)
	}
}

func TestManualResetCutoffSurvivesQuotaRefresh(t *testing.T) {
	t.Parallel()
	periodStart := time.Date(2026, time.August, 13, 0, 0, 0, 0, time.UTC)
	manualReset := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := &Usage{Windows: []*Window{{
		Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60,
		StartedAt: periodStart.Unix(), ResetAt: periodStart.Add(7 * 24 * time.Hour).Unix(),
		StandardCostMicroUSD: 10_000_000,
	}}}
	usage = Reset(usage, manualReset, map[string]int64{FamilyAll: 250_000})
	upstreamReset := manualReset.Add(7 * 24 * time.Hour)
	usage = Reconcile(usage, []Sample{{
		Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60,
		ResetAt: upstreamReset,
	}}, manualReset.Add(time.Second))
	w := usage.Windows[0]
	if w.CountFromAt != manualReset.Unix() || w.StandardCostMicroUSD != 250_000 {
		t.Fatalf("manual reset cutoff was not preserved: %#v", w)
	}
	if changed, err := AddStandardCost(usage, manualReset.Add(-time.Second), "", 1_000_000); err != nil || changed {
		t.Fatalf("late old-period log = (%t, %v), want ignored", changed, err)
	}
	if changed, err := AddStandardCost(usage, manualReset.Add(time.Second), "", 500_000); err != nil || !changed {
		t.Fatalf("new-period log = (%t, %v), want accumulated", changed, err)
	}
	if w.StandardCostMicroUSD != 750_000 {
		t.Fatalf("manual reset cost = %d, want 750000", w.StandardCostMicroUSD)
	}
}

func TestMonthlyQuotaRefreshAdvancesClampedResetWithOriginalAnchor(t *testing.T) {
	t.Parallel()
	for _, year := range []int{2027, 2028} {
		jan31 := time.Date(year, time.January, 31, 8, 0, 0, 0, time.UTC)
		februaryReset := addMonthsClamped(jan31, 1, 31)
		usage := &Usage{Windows: []*Window{{
			Key: "test|monthly", WindowSeconds: 30 * 24 * 60 * 60,
			StartedAt: jan31.Unix(), ResetAt: februaryReset.Unix(), ResetDay: 31,
			StandardCostMicroUSD: 4_000_000,
		}}}
		usage = Reconcile(usage, []Sample{{
			Key: "test|monthly", WindowSeconds: 30 * 24 * 60 * 60,
			ResetAt: februaryReset,
		}}, februaryReset)
		w := usage.Windows[0]
		wantReset := time.Date(year, time.March, 31, 8, 0, 0, 0, time.UTC)
		if w.StartedAt != februaryReset.Unix() || w.ResetAt != wantReset.Unix() ||
			w.ResetDay != 31 || w.StandardCostMicroUSD != 0 {
			t.Fatalf("year %d reconciled monthly window = %#v", year, w)
		}
	}
}

func TestMultiWindowFamilyAccumulation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := Reconcile(nil, []Sample{
		{Key: "gemini models|gemini-weekly", Family: FamilyGemini, WindowSeconds: 604800, ResetAt: now.Add(5 * 24 * time.Hour)},
		{Key: "gemini models|gemini-5h", Family: FamilyGemini, WindowSeconds: 18000, ResetAt: now.Add(3 * time.Hour)},
		{Key: "claude and gpt models|3p-weekly", Family: FamilyNonGemini, WindowSeconds: 604800, ResetAt: now.Add(6 * 24 * time.Hour)},
		{Key: "claude and gpt models|3p-5h", Family: FamilyNonGemini, WindowSeconds: 18000, ResetAt: now.Add(4 * time.Hour)},
	}, now)
	if usage == nil || len(usage.Windows) != 4 {
		t.Fatalf("expected 4 windows, got %d", len(usage.Windows))
	}

	// Gemini cost goes only to gemini windows
	changed, err := AddStandardCost(usage, now, "gemini-3.6-flash-high", 500_000)
	if err != nil || !changed {
		t.Fatalf("gemini cost = (%t, %v)", changed, err)
	}
	geminiWeekly := Find(usage, "gemini models|gemini-weekly")
	gemini5h := Find(usage, "gemini models|gemini-5h")
	nonGeminiWeekly := Find(usage, "claude and gpt models|3p-weekly")
	nonGemini5h := Find(usage, "claude and gpt models|3p-5h")

	if geminiWeekly.StandardCostMicroUSD != 500_000 || gemini5h.StandardCostMicroUSD != 500_000 {
		t.Fatalf("gemini windows should accumulate: weekly=%d, 5h=%d",
			geminiWeekly.StandardCostMicroUSD, gemini5h.StandardCostMicroUSD)
	}
	if nonGeminiWeekly.StandardCostMicroUSD != 0 || nonGemini5h.StandardCostMicroUSD != 0 {
		t.Fatalf("non-gemini windows should not accumulate: weekly=%d, 5h=%d",
			nonGeminiWeekly.StandardCostMicroUSD, nonGemini5h.StandardCostMicroUSD)
	}

	// Claude cost goes only to non-gemini windows
	changed, err = AddStandardCost(usage, now, "claude-sonnet-4", 300_000)
	if err != nil || !changed {
		t.Fatalf("claude cost = (%t, %v)", changed, err)
	}
	if nonGeminiWeekly.StandardCostMicroUSD != 300_000 || nonGemini5h.StandardCostMicroUSD != 300_000 {
		t.Fatalf("non-gemini windows after claude cost: weekly=%d, 5h=%d",
			nonGeminiWeekly.StandardCostMicroUSD, nonGemini5h.StandardCostMicroUSD)
	}
	if geminiWeekly.StandardCostMicroUSD != 500_000 {
		t.Fatalf("gemini weekly should not change: %d", geminiWeekly.StandardCostMicroUSD)
	}
}

func TestFamilyAllAccumulatesEverything(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := Reconcile(nil, []Sample{
		{Key: "codex|secondary", WindowSeconds: 604800, ResetAt: now.Add(5 * 24 * time.Hour)},
	}, now)
	changed, err := AddStandardCost(usage, now, "gemini-3.6-flash-high", 100)
	if err != nil || !changed {
		t.Fatalf("gemini model with FamilyAll = (%t, %v)", changed, err)
	}
	changed, err = AddStandardCost(usage, now, "claude-sonnet-4", 200)
	if err != nil || !changed {
		t.Fatalf("claude model with FamilyAll = (%t, %v)", changed, err)
	}
	if usage.Windows[0].StandardCostMicroUSD != 300 {
		t.Fatalf("FamilyAll total = %d, want 300", usage.Windows[0].StandardCostMicroUSD)
	}
}

func TestReconcileDropsStaleWindows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := &Usage{Windows: []*Window{
		{Key: "old|window", WindowSeconds: 604800, StartedAt: now.Add(-14 * 24 * time.Hour).Unix(), ResetAt: now.Add(-7 * 24 * time.Hour).Unix(), StandardCostMicroUSD: 999},
		{Key: "keep|window", WindowSeconds: 604800, StartedAt: now.Add(-3 * 24 * time.Hour).Unix(), ResetAt: now.Add(4 * 24 * time.Hour).Unix(), StandardCostMicroUSD: 100},
	}}
	usage = Reconcile(usage, []Sample{
		{Key: "keep|window", WindowSeconds: 604800, ResetAt: now.Add(4 * 24 * time.Hour)},
	}, now)
	if len(usage.Windows) != 1 || usage.Windows[0].Key != "keep|window" {
		t.Fatalf("expected only keep|window, got %#v", usage.Windows)
	}
	if usage.Windows[0].StandardCostMicroUSD != 100 {
		t.Fatalf("kept window cost = %d, want 100", usage.Windows[0].StandardCostMicroUSD)
	}
}

func TestFamilyMatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		family string
		model  string
		want   bool
	}{
		{FamilyAll, "anything", true},
		{FamilyGemini, "gemini-3.6-flash-high", true},
		{FamilyGemini, "claude-sonnet-4", false},
		{FamilyGemini, "vertex-gemini-3-pro", false},
		{FamilyNonGemini, "claude-sonnet-4", true},
		{FamilyNonGemini, "gpt-5.4", true},
		{FamilyNonGemini, "gemini-3.6-flash-high", false},
		{FamilyNonGemini, "", false},
		{FamilySonnet, "claude-sonnet-4", true},
		{FamilySonnet, "claude-opus-5", false},
		{FamilyFable, "claude-fable-5", true},
		{FamilyFable, "claude-sonnet-4", false},
		{FamilySpark, "gpt-5.3-codex-spark", true},
		{FamilySpark, "gpt-5.4", false},
	}
	for _, tc := range tests {
		if got := FamilyMatches(tc.family, tc.model); got != tc.want {
			t.Errorf("FamilyMatches(%q, %q) = %t, want %t", tc.family, tc.model, got, tc.want)
		}
	}
}

func TestReconcileKeepsCountersWhenSamplesCarryNoBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := Reconcile(nil, []Sample{
		{Key: "codex|secondary", WindowSeconds: 604800, ResetAt: now.Add(5 * 24 * time.Hour)},
	}, now)
	if changed, err := AddStandardCost(usage, now, "gpt-5.4", 500_000); err != nil || !changed {
		t.Fatalf("seed cost = (%t, %v)", changed, err)
	}
	// 采样失败/边界缺失不是「窗口消失」，已累计的成本必须留下。
	for _, samples := range [][]Sample{
		nil,
		{{Key: "codex|secondary", WindowSeconds: 604800}},
		{{Key: "", WindowSeconds: 604800, ResetAt: now.Add(5 * 24 * time.Hour)}},
	} {
		kept := Reconcile(usage, samples, now)
		if kept == nil || len(kept.Windows) != 1 || kept.Windows[0].StandardCostMicroUSD != 500_000 {
			t.Fatalf("boundary-less samples %#v dropped counters: %#v", samples, kept)
		}
	}
}

func TestSparkWindowOnlyAccumulatesSparkModels(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := Reconcile(nil, []Sample{
		{Key: "codex|primary", WindowSeconds: 18000, ResetAt: now.Add(3 * time.Hour)},
		{Key: "codex-spark|primary", Family: FamilySpark, WindowSeconds: 18000, ResetAt: now.Add(2 * time.Hour)},
	}, now)
	if changed, err := AddStandardCost(usage, now, "gpt-5.4", 400_000); err != nil || !changed {
		t.Fatalf("non-spark cost = (%t, %v)", changed, err)
	}
	if changed, err := AddStandardCost(usage, now, "gpt-5.3-codex-spark", 100_000); err != nil || !changed {
		t.Fatalf("spark cost = (%t, %v)", changed, err)
	}
	// 主窗口覆盖全部模型，spark 窗口只吃 spark 的消耗。
	if got := Find(usage, "codex|primary").StandardCostMicroUSD; got != 500_000 {
		t.Fatalf("primary window = %d, want 500000", got)
	}
	if got := Find(usage, "codex-spark|primary").StandardCostMicroUSD; got != 100_000 {
		t.Fatalf("spark window = %d, want 100000", got)
	}
}
