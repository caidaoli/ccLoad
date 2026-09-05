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
		SampledUpstreamUsedPercent: float64Pointer(80), StandardCostMicroUSD: 10_000_000,
	}}}
	usage = Reset(usage, manualReset, map[string]int64{FamilyAll: 250_000})
	upstreamReset := manualReset.Add(7 * 24 * time.Hour)
	usage = Reconcile(usage, []Sample{{
		Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60,
		ResetAt: upstreamReset, UsedPercent: float64Pointer(80), SampledAt: manualReset.Add(-time.Second),
	}}, manualReset.Add(time.Second))
	if w := usage.Windows[0]; w.CountFromAt != manualReset.Unix() || w.StandardCostMicroUSD != 250_000 ||
		w.SampledUpstreamUsedPercent != nil || w.SampledUpstreamAtUnixNano != manualReset.UnixNano() {
		t.Fatalf("late pre-reset sample changed manual reset: %#v", w)
	}
	usage = Reconcile(usage, []Sample{{
		Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60,
		ResetAt: upstreamReset, UsedPercent: float64Pointer(5), SampledAt: manualReset.Add(2 * time.Second),
	}}, manualReset.Add(2*time.Second))
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

func float64Pointer(value float64) *float64 {
	return &value
}

func TestCodexWeeklyCostSurvivesWindowRoleChanges(t *testing.T) {
	t.Parallel()
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "partial"}[partial], func(t *testing.T) {
			reconcileUsage := Reconcile
			if partial {
				reconcileUsage = ReconcilePartial
			}
			resetAt := time.Date(2026, time.September, 5, 6, 0, 0, 0, time.UTC)
			initial := []Sample{
				{Key: "codex|primary", Family: FamilyCodex, WindowSeconds: 18000, ResetAt: resetAt.Add(time.Hour), UsedPercent: float64Pointer(80)},
				{Key: "codex|secondary", Family: FamilyCodex, WindowSeconds: 604800, ResetAt: resetAt.Add(2 * 24 * time.Hour), UsedPercent: float64Pointer(100)},
			}
			usage := Reconcile(nil, initial, resetAt.Add(-time.Minute))
			usage = Reset(usage, resetAt, nil)
			addCost := func(at time.Time, amount int64, wantChanged bool) {
				t.Helper()
				if changed, err := AddStandardCost(usage, at, "gpt-5.6-sol", amount); err != nil || changed != wantChanged {
					t.Fatalf("AddStandardCost(%s, %d) = (%t, %v)", at, amount, changed, err)
				}
			}
			addCost(resetAt.Add(30*time.Minute), 12_252_287, true)
			observedAt := resetAt.Add(33 * time.Minute)
			weekly := Sample{
				Key: "codex|primary", Family: FamilyCodex, WindowSeconds: 604800,
				ResetAt: observedAt.Add(7 * 24 * time.Hour), UsedPercent: float64Pointer(5), SampledAt: observedAt,
			}
			usage = reconcileUsage(usage, []Sample{weekly}, observedAt)
			if len(usage.Windows) != 1 || usage.Windows[0].Key != "codex|primary" ||
				usage.Windows[0].StandardCostMicroUSD != 12_252_287 || usage.Windows[0].CountFromAt != resetAt.Unix() {
				t.Fatalf("weekly role change lost the reset-period cost: %#v", usage)
			}
			addCost(resetAt.Add(-time.Second), 1_000_000, false)
			addCost(observedAt.Add(time.Second), 100, true)
			addCost(observedAt.Add(time.Minute), 18_829_059, true)
			wantCost := int64(31_081_446)
			if got := Find(usage, "codex|primary").StandardCostMicroUSD; got != wantCost {
				t.Fatalf("weekly cost = %d, want %d", got, wantCost)
			}
			// A replay of the old two-window layout cannot resurrect a second weekly counter.
			for i := range initial {
				initial[i].SampledAt = resetAt.Add(time.Minute)
			}
			usage = reconcileUsage(usage, initial, observedAt.Add(2*time.Minute))
			if len(usage.Windows) != 1 || Find(usage, "codex|primary").StandardCostMicroUSD != wantCost {
				t.Fatalf("stale layout changed weekly cost: %#v", usage)
			}
			// If the 5h limit returns, the weekly cost moves back to secondary once.
			returnedAt := observedAt.Add(3 * time.Minute)
			weekly.Key, weekly.SampledAt = "codex|secondary", returnedAt
			usage = reconcileUsage(usage, []Sample{
				{Key: "codex|primary", Family: FamilyCodex, WindowSeconds: 18000, ResetAt: returnedAt.Add(5 * time.Hour), UsedPercent: float64Pointer(0), SampledAt: returnedAt},
				weekly,
			}, returnedAt)
			if len(usage.Windows) != 2 || Find(usage, "codex|primary").StandardCostMicroUSD != 0 ||
				Find(usage, "codex|secondary").StandardCostMicroUSD != wantCost {
				t.Fatalf("returning 5h limit changed weekly cost: %#v", usage)
			}
			addCost(returnedAt.Add(time.Second), 200, true)
			if Find(usage, "codex|primary").StandardCostMicroUSD != 200 ||
				Find(usage, "codex|secondary").StandardCostMicroUSD != wantCost+200 {
				t.Fatalf("cost was duplicated after moving weekly back: %#v", usage)
			}
		})
	}
}

func TestCodexWeeklyRoleChangeStillResetsNewPeriods(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 5, 6, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		used     float64
		resetAt  time.Time
		wantCost int64
	}{
		{name: "same_period", used: 60, resetAt: now.Add(6 * 24 * time.Hour), wantCost: 900_000},
		{name: "upstream_reset", used: 5, resetAt: now.Add(6 * 24 * time.Hour)},
		{name: "next_period", used: 60, resetAt: now.Add(13 * 24 * time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := Reconcile(nil, []Sample{
				{Key: "codex|primary", WindowSeconds: 18000, ResetAt: now.Add(4 * time.Hour), UsedPercent: float64Pointer(80)},
				{Key: "codex|secondary", WindowSeconds: 604800, ResetAt: now.Add(6 * 24 * time.Hour), UsedPercent: float64Pointer(60)},
			}, now)
			if _, err := AddStandardCost(usage, now, "gpt-5.6-sol", 900_000); err != nil {
				t.Fatal(err)
			}
			usage = Reconcile(usage, []Sample{{
				Key: "codex|primary", Family: FamilyCodex, WindowSeconds: 604800,
				ResetAt: test.resetAt, UsedPercent: &test.used, SampledAt: now.Add(time.Minute),
			}}, now.Add(time.Minute))
			if len(usage.Windows) != 1 || usage.Windows[0].StandardCostMicroUSD != test.wantCost {
				t.Fatalf("weekly cost after %s = %#v, want %d", test.name, usage, test.wantCost)
			}
		})
	}
}

func TestCodexExplicitWeeklyWindowsStayIndependent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 5, 6, 0, 0, 0, time.UTC)
	secondary := Sample{Key: "codex|secondary", Family: FamilyCodex, WindowSeconds: 604800,
		ResetAt: now.Add(6 * 24 * time.Hour), UsedPercent: float64Pointer(50), SampledAt: now}
	usage := Reconcile(nil, []Sample{secondary}, now)
	if _, err := AddStandardCost(usage, now, "gpt-5.6-sol", 900_000); err != nil {
		t.Fatal(err)
	}
	primary := secondary
	primary.Key, primary.UsedPercent = "codex|primary", float64Pointer(5)
	primary.SampledAt, secondary.SampledAt = now.Add(time.Minute), now.Add(time.Minute)
	usage = Reconcile(usage, []Sample{primary, secondary}, now.Add(time.Minute))
	if len(usage.Windows) != 2 || Find(usage, "codex|primary").StandardCostMicroUSD != 0 ||
		Find(usage, "codex|secondary").StandardCostMicroUSD != 900_000 {
		t.Fatalf("two explicit weekly windows were merged: %#v", usage)
	}
	if _, err := AddStandardCost(usage, now.Add(time.Minute), "gpt-5.6-sol", 100_000); err != nil {
		t.Fatal(err)
	}
	primary.SampledAt = now.Add(2 * time.Minute)
	usage = Reconcile(usage, []Sample{primary}, primary.SampledAt)
	if len(usage.Windows) != 1 || usage.Windows[0].StandardCostMicroUSD != 100_000 {
		t.Fatalf("existing primary borrowed the omitted secondary cost: %#v", usage)
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

func TestReconcileKeepsOmittedWindowsSampledAfterSnapshot(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, time.September, 5, 0, 0, 0, 0, time.UTC)
	quotaAt, passiveAt, completedAt := base.Add(time.Minute), base.Add(2*time.Minute), base.Add(3*time.Minute)
	main := Sample{Key: "codex|primary", Family: FamilyCodex, WindowSeconds: 604800,
		ResetAt: base.Add(24 * time.Hour), UsedPercent: float64Pointer(20), SampledAt: base}
	retired := Sample{Key: "old|weekly", WindowSeconds: 604800,
		ResetAt: base.Add(24 * time.Hour), UsedPercent: float64Pointer(20), SampledAt: base}
	usage := Reconcile(nil, []Sample{main, retired}, base)
	spark := Sample{Key: "codex-spark|primary", Family: FamilySpark, WindowSeconds: 18000,
		ResetAt: base.Add(time.Hour), UsedPercent: float64Pointer(30), SampledAt: passiveAt}
	usage = ReconcilePartial(usage, []Sample{spark}, passiveAt)
	if _, err := AddStandardCost(usage, passiveAt, "gpt-5.3-codex-spark", 2_000_000); err != nil {
		t.Fatal(err)
	}
	main.SampledAt = quotaAt
	untimed := Sample{Key: "untimed|weekly", WindowSeconds: 604800, ResetAt: main.ResetAt}
	invalid := Sample{Key: "invalid|weekly", Family: "unknown", WindowSeconds: 604800,
		ResetAt: main.ResetAt, SampledAt: base.Add(-time.Minute)}
	got := Reconcile(usage, []Sample{main, untimed, invalid}, completedAt)
	if Find(got, retired.Key) != nil || Find(got, main.Key) == nil || Find(got, untimed.Key) == nil {
		t.Fatalf("complete snapshot did not reconcile older windows: %+v", got)
	}
	if w := Find(got, spark.Key); w == nil || w.StandardCostMicroUSD != 2_000_000 ||
		w.SampledUpstreamAtUnixNano != passiveAt.UnixNano() || w.ResetAt != spark.ResetAt.Unix() {
		t.Fatalf("older complete snapshot deleted newer Spark cost: %+v", w)
	}
	main.SampledAt = completedAt
	got = Reconcile(got, []Sample{main}, completedAt)
	if Find(got, spark.Key) != nil {
		t.Fatal("a subsequent fresh complete snapshot must retire the omitted Spark window")
	}
}

func TestReconcilePartialKeepsOmittedQuotaFamilies(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 30, 21, 0, 0, 0, time.UTC)
	mainUsed := 20.0
	sparkUsed := 40.0
	usage := Reconcile(nil, []Sample{
		{Key: "codex|primary", Family: FamilyCodex, WindowSeconds: 7 * 24 * 60 * 60,
			ResetAt: now.Add(6 * 24 * time.Hour), UsedPercent: &mainUsed, SampledAt: now},
		{Key: "gpt-5.3-codex-spark|primary", Family: FamilySpark, WindowSeconds: 5 * 60 * 60,
			ResetAt: now.Add(4 * time.Hour), UsedPercent: &sparkUsed, SampledAt: now},
	}, now)
	if changed, err := AddStandardCost(usage, now, "gpt-5.4", 500_000); err != nil || !changed {
		t.Fatalf("seed Codex cost = (%t, %v)", changed, err)
	}
	if changed, err := AddStandardCost(usage, now, "gpt-5.3-codex-spark", 300_000); err != nil || !changed {
		t.Fatalf("seed Spark cost = (%t, %v)", changed, err)
	}

	sparkUsed = 41
	usage = ReconcilePartial(usage, []Sample{{
		Key: "gpt-5.3-codex-spark|primary", Family: FamilySpark, WindowSeconds: 5 * 60 * 60,
		ResetAt: now.Add(4 * time.Hour), UsedPercent: &sparkUsed, SampledAt: now.Add(time.Minute),
	}}, now.Add(time.Minute))
	main := Find(usage, "codex|primary")
	spark := Find(usage, "gpt-5.3-codex-spark|primary")
	if main == nil || main.StandardCostMicroUSD != 500_000 || main.SampledUpstreamUsedPercent == nil ||
		*main.SampledUpstreamUsedPercent != mainUsed {
		t.Fatalf("partial Spark sample retired main Codex window: %#v", main)
	}
	if spark == nil || spark.StandardCostMicroUSD != 300_000 || spark.SampledUpstreamUsedPercent == nil ||
		*spark.SampledUpstreamUsedPercent != sparkUsed {
		t.Fatalf("partial Spark sample did not update Spark window: %#v", spark)
	}
}

func TestReconcilePartialRollbackResetsOnlySampledSparkWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 30, 21, 0, 0, 0, time.UTC)
	mainUsed := 73.0
	sparkUsed := 80.0
	usage := Reconcile(nil, []Sample{
		{Key: "codex|primary", Family: FamilyCodex, WindowSeconds: 7 * 24 * 60 * 60,
			ResetAt: now.Add(6 * 24 * time.Hour), UsedPercent: &mainUsed, SampledAt: now},
		{Key: "gpt-5.3-codex-spark|primary", Family: FamilySpark, WindowSeconds: 5 * 60 * 60,
			ResetAt: now.Add(4 * time.Hour), UsedPercent: &sparkUsed, SampledAt: now},
	}, now)
	if changed, err := AddStandardCost(usage, now, "gpt-5.4", 700_000); err != nil || !changed {
		t.Fatalf("seed Codex cost = (%t, %v)", changed, err)
	}
	if changed, err := AddStandardCost(usage, now, "gpt-5.3-codex-spark", 900_000); err != nil || !changed {
		t.Fatalf("seed Spark cost = (%t, %v)", changed, err)
	}

	sampledAt := now.Add(time.Hour)
	sparkUsed = 5
	usage = ReconcilePartial(usage, []Sample{{
		Key: "gpt-5.3-codex-spark|primary", Family: FamilySpark, WindowSeconds: 5 * 60 * 60,
		ResetAt: sampledAt.Add(3 * time.Hour), UsedPercent: &sparkUsed, SampledAt: sampledAt,
	}}, sampledAt.Add(time.Minute))
	main := Find(usage, "codex|primary")
	spark := Find(usage, "gpt-5.3-codex-spark|primary")
	if main == nil || main.StandardCostMicroUSD != 700_000 || main.SampledUpstreamUsedPercent == nil ||
		*main.SampledUpstreamUsedPercent != mainUsed {
		t.Fatalf("Spark rollback reset main Codex window: %#v", main)
	}
	if spark == nil || spark.StandardCostMicroUSD != 0 || spark.CountFromAt != sampledAt.Unix() ||
		spark.SampledUpstreamUsedPercent == nil || *spark.SampledUpstreamUsedPercent != sparkUsed {
		t.Fatalf("Spark rollback did not reset only Spark window: %#v", spark)
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
		{FamilyCodexReserve, "gpt-5.4", false},
	}
	for _, tc := range tests {
		if got := FamilyMatches(tc.family, tc.model); got != tc.want {
			t.Errorf("FamilyMatches(%q, %q) = %t, want %t", tc.family, tc.model, got, tc.want)
		}
	}
}

func TestCodexWindowFamiliesSeparateSpark(t *testing.T) {
	t.Parallel()
	if got := WindowFamily(ProviderCodex, "codex", "secondary"); got != FamilyCodex {
		t.Fatalf("Codex main window family = %q, want %q", got, FamilyCodex)
	}
	if got := WindowFamily(ProviderCodex, "codex-spark", "secondary"); got != FamilySpark {
		t.Fatalf("Codex Spark window family = %q, want %q", got, FamilySpark)
	}
	if got := WindowFamily(ProviderCodex, "gpt-reserve", "primary"); got != FamilyCodexReserve {
		t.Fatalf("Codex reserve window family = %q, want %q", got, FamilyCodexReserve)
	}
	if FamilyMatches(FamilyCodex, "gpt-5.3-codex-spark") {
		t.Fatal("Codex main family must not match Spark")
	}
	if !FamilyMatches(FamilyCodex, "gpt-5.4") {
		t.Fatal("Codex main family must match regular Codex models")
	}
}

func TestCodexReserveWindowDoesNotAccumulateRegularModels(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := &Usage{Windows: []*Window{{
		// 这是修复前已经落盘的窗口：即使它的旧 family 是 codex，
		// 后续普通 Codex 请求也不能继续污染 gpt-reserve。
		Key: "gpt-reserve|primary", Family: FamilyCodex, WindowSeconds: 7 * 24 * 60 * 60,
		StartedAt: now.Add(-time.Hour).Unix(), ResetAt: now.Add(6 * 24 * time.Hour).Unix(),
		StandardCostMicroUSD: 9_953_296,
	}}}
	if changed, err := AddStandardCost(usage, now, "gpt-5.6-sol", 1_000_000); err != nil || changed {
		t.Fatalf("gpt-reserve window accepted regular cost = (%t, %v)", changed, err)
	}
	if got := usage.Windows[0].StandardCostMicroUSD; got != 9_953_296 {
		t.Fatalf("historical gpt-reserve cost changed to %d", got)
	}

	// 新采样统一使用独立族，即使调用方漏填 Family 也不能回退到普通 Codex。
	usage = Reconcile(nil, []Sample{{
		Key: "gpt-reserve|primary", WindowSeconds: 7 * 24 * 60 * 60,
		ResetAt: now.Add(6 * 24 * time.Hour),
	}}, now)
	window := Find(usage, "gpt-reserve|primary")
	if window == nil || window.Family != FamilyCodexReserve {
		t.Fatalf("gpt-reserve sample family = %#v, want %q", window, FamilyCodexReserve)
	}
	if changed, err := AddStandardCost(usage, now, "gpt-5.6-sol", 1_000_000); err != nil || changed {
		t.Fatalf("new gpt-reserve window accepted regular cost = (%t, %v)", changed, err)
	}
}

func TestCodexReserveWindowMigratesAndResetsHistoricalCost(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := &Usage{Windows: []*Window{{
		Key: "gpt-reserve|primary", Family: FamilyCodex,
		WindowSeconds: 7 * 24 * 60 * 60,
		StartedAt:     now.Add(-time.Hour).Unix(), ResetAt: now.Add(6 * 24 * time.Hour).Unix(),
		StandardCostMicroUSD: 9_953_296,
	}}}

	// A fresh sample migrates the legacy family while retaining historical data
	// until the explicit reset path clears it.
	usage = Reconcile(usage, []Sample{{
		Key: "gpt-reserve|primary", WindowSeconds: 7 * 24 * 60 * 60,
		ResetAt: now.Add(6 * 24 * time.Hour),
	}}, now)
	window := Find(usage, "gpt-reserve|primary")
	if window == nil || window.Family != FamilyCodexReserve || window.StandardCostMicroUSD != 9_953_296 {
		t.Fatalf("gpt-reserve migration = %#v, want reserve family with historical cost", window)
	}

	reset := Reset(usage, now.Add(time.Minute), map[string]int64{FamilyCodex: 123})
	window = Find(reset, "gpt-reserve|primary")
	if window == nil || window.Family != FamilyCodexReserve || window.StandardCostMicroUSD != 0 {
		t.Fatalf("gpt-reserve reset = %#v, want reserve family with zero cost", window)
	}
	if err := Validate(reset); err != nil {
		t.Fatalf("reset reserve usage is invalid: %v", err)
	}
}

func TestReconcileSkipsInvalidDuplicateBeforeMarkingSeen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := Reconcile(nil, []Sample{
		{
			Key: "codex|primary", Family: "invalid",
			WindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(6 * 24 * time.Hour),
		},
		{
			Key: "codex|primary", Family: FamilyCodex,
			WindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(6 * 24 * time.Hour),
		},
	}, now)
	window := Find(usage, "codex|primary")
	if window == nil || window.Family != FamilyCodex {
		t.Fatalf("valid duplicate sample was discarded: %#v", usage)
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
	newerAt := now.Add(time.Minute)
	usage = ReconcilePartial(usage, []Sample{{
		Key: "codex-spark|primary", Family: FamilySpark, WindowSeconds: 18000,
		ResetAt: now.Add(time.Hour), SampledAt: newerAt, UsedPercent: float64Pointer(20),
	}}, newerAt)
	if _, err := AddStandardCost(usage, newerAt, "gpt-5.3-codex-spark", 300_000); err != nil {
		t.Fatal(err)
	}
	// 采样失败/边界缺失不是「窗口消失」，已累计的成本必须留下。
	for _, samples := range [][]Sample{
		nil,
		{{Key: "codex|secondary", WindowSeconds: 604800}},
		{{Key: "", WindowSeconds: 604800, ResetAt: now.Add(5 * 24 * time.Hour)}},
	} {
		kept := Reconcile(usage, samples, now)
		if kept == nil || len(kept.Windows) != 2 {
			t.Fatalf("boundary-less samples %#v dropped counters: %#v", samples, kept)
		}
		if main := Find(kept, "codex|secondary"); main == nil || main.StandardCostMicroUSD != 500_000 {
			t.Fatalf("boundary-less samples dropped older Codex counter: %+v", main)
		}
		if spark := Find(kept, "codex-spark|primary"); spark == nil || spark.StandardCostMicroUSD != 300_000 {
			t.Fatalf("boundary-less samples dropped newer Spark counter: %+v", spark)
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
	// Codex 主窗口不再吞掉单独计量的 Spark 消耗；Spark 窗口单独累计。
	if got := Find(usage, "codex|primary").StandardCostMicroUSD; got != 400_000 {
		t.Fatalf("primary window = %d, want 400000", got)
	}
	if got := Find(usage, "codex-spark|primary").StandardCostMicroUSD; got != 100_000 {
		t.Fatalf("spark window = %d, want 100000", got)
	}
}

func TestLegacyCodexMainWindowExcludesSpark(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := &Usage{Windows: []*Window{{
		// 旧版本把 Codex 主窗口保存为 FamilyAll；key-aware matching 必须
		// 在下一次配额刷新前也阻止 Spark 污染这个窗口。
		Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60,
		StartedAt: now.Add(-time.Hour).Unix(), ResetAt: now.Add(6 * 24 * time.Hour).Unix(),
	}}}
	if changed, err := AddStandardCost(usage, now, "gpt-5.3-codex-spark", 100_000); err != nil || changed {
		t.Fatalf("legacy Codex window accepted Spark cost = (%t, %v)", changed, err)
	}
	if changed, err := AddStandardCost(usage, now, "gpt-5.4", 200_000); err != nil || !changed {
		t.Fatalf("legacy Codex window rejected regular cost = (%t, %v)", changed, err)
	}
	if got := usage.Windows[0].StandardCostMicroUSD; got != 200_000 {
		t.Fatalf("legacy Codex window cost = %d, want 200000", got)
	}
}

func TestReconcileKeepsCostWhenSampledResetJitters(t *testing.T) {
	t.Parallel()
	// 同一个上游周期会被两种精度表达：Codex 响应头给绝对 reset-at，SSE
	// rate_limits 事件只给 resets_in_seconds（换算成 sampledAt+n，每次都不同）。
	// 逐秒比较边界会把每一次相对值采样都当成新周期，把已累计成本清零。
	resetAt := time.Date(2026, time.August, 19, 3, 25, 0, 0, time.UTC)
	start := resetAt.Add(-6 * 24 * time.Hour)
	usage := Reconcile(nil, []Sample{
		{Key: "codex|primary", WindowSeconds: 604800, ResetAt: resetAt},
	}, start)

	total := int64(0)
	for i, jitter := range []time.Duration{0, 7 * time.Second, -3 * time.Second, 41 * time.Second, 0} {
		at := start.Add(time.Duration(i) * time.Minute)
		usage = Reconcile(usage, []Sample{
			{Key: "codex|primary", WindowSeconds: 604800, ResetAt: resetAt.Add(jitter)},
		}, at)
		if changed, err := AddStandardCost(usage, at, "gpt-5.4", 100_000); err != nil || !changed {
			t.Fatalf("sample %d cost = (%t, %v)", i, changed, err)
		}
		total += 100_000
		if got := Find(usage, "codex|primary").StandardCostMicroUSD; got != total {
			t.Fatalf("sample %d (jitter %s) left cost %d, want %d", i, jitter, got, total)
		}
	}
}

func TestReconcileZeroesCostWhenUpstreamUsageRollsBackBeforeResetAt(t *testing.T) {
	t.Parallel()
	observedAt := time.Date(2026, time.August, 24, 2, 29, 7, 0, time.UTC)
	oldResetAt := time.Date(2026, time.August, 30, 1, 25, 0, 0, time.UTC)
	usedBeforeReset := 73.0
	usage := Reconcile(nil, []Sample{{
		Key: "codex|primary", WindowSeconds: 604800, ResetAt: oldResetAt,
		UsedPercent: &usedBeforeReset,
	}}, observedAt.Add(-time.Hour))
	if changed, err := AddStandardCost(usage, observedAt.Add(-time.Hour), "gpt-5.6-sol", 87_704_157); err != nil || !changed {
		t.Fatalf("seed cost = (%t, %v)", changed, err)
	}

	// 上游直接把未耗尽额度恢复为 100%；首次采样时新额度已使用 5%。新 reset_at
	// 只移动了约 25 小时，不能因此继续保留上一周期成本。
	newResetAt := time.Date(2026, time.August, 31, 2, 29, 7, 0, time.UTC)
	usedAfterReset := 5.0
	usage = Reconcile(usage, []Sample{{
		Key: "codex|primary", WindowSeconds: 604800, ResetAt: newResetAt,
		UsedPercent: &usedAfterReset, SampledAt: observedAt,
	}}, observedAt.Add(time.Minute))
	window := Find(usage, "codex|primary")
	if window.StandardCostMicroUSD != 0 || window.CountFromAt != observedAt.Unix() ||
		window.ResetAt != newResetAt.Unix() {
		t.Fatalf("upstream-reset window = %#v", window)
	}
	if changed, err := AddStandardCost(usage, observedAt.Add(-time.Second), "gpt-5.6-sol", 1); err != nil || changed {
		t.Fatalf("late old-period cost = (%t, %v), want ignored", changed, err)
	}
	if changed, err := AddStandardCost(usage, observedAt.Add(time.Second), "gpt-5.6-sol", 500_000); err != nil || !changed {
		t.Fatalf("new-period cost = (%t, %v), want accumulated", changed, err)
	}
}

func TestReconcileDropsOmittedWindowEvenWhenSiblingRollsBack(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 24, 2, 0, 0, 0, time.UTC)
	fiveHourUsed := 40.0
	weeklyUsed := 73.0
	fiveHourResetAt := now.Add(4 * time.Hour)
	weeklyResetAt := now.Add(6 * 24 * time.Hour)
	usage := Reconcile(nil, []Sample{
		{Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: fiveHourResetAt, UsedPercent: &fiveHourUsed},
		{Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60, ResetAt: weeklyResetAt, UsedPercent: &weeklyUsed},
		{Key: "codex|additional", WindowSeconds: 5 * 60 * 60, ResetAt: fiveHourResetAt, UsedPercent: &fiveHourUsed},
	}, now)
	if changed, err := AddStandardCost(usage, now, "gpt-5.6-sol", 1_000_000); err != nil || !changed {
		t.Fatalf("seed cost = (%t, %v)", changed, err)
	}

	sampledAt := now.Add(time.Hour)
	fiveHourUsed = 41
	weeklyUsed = 5
	usage = Reconcile(usage, []Sample{
		{Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: fiveHourResetAt,
			UsedPercent: &fiveHourUsed, SampledAt: sampledAt},
		{Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60, ResetAt: weeklyResetAt.Add(24 * time.Hour),
			UsedPercent: &weeklyUsed, SampledAt: sampledAt},
	}, sampledAt.Add(time.Minute))

	primary := Find(usage, "codex|primary")
	weekly := Find(usage, "codex|secondary")
	if primary == nil || primary.StandardCostMicroUSD != 1_000_000 || primary.CountFromAt != 0 {
		t.Fatalf("unrolled 5-hour window was reset with weekly: %#v", primary)
	}
	if weekly == nil || weekly.StandardCostMicroUSD != 0 || weekly.CountFromAt != sampledAt.Unix() {
		t.Fatalf("weekly rollback did not reset only weekly: %#v", weekly)
	}
	if Find(usage, "codex|additional") != nil {
		t.Fatalf("complete snapshot kept omitted window: %#v", usage.Windows)
	}
}

func TestReconcileIgnoresStaleSiblingWhenAnotherWindowRollsBack(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 24, 2, 0, 0, 0, time.UTC)
	fiveHourResetAt := now.Add(4 * time.Hour)
	weeklyResetAt := now.Add(6 * 24 * time.Hour)
	usage := Reconcile(nil, []Sample{
		{Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: fiveHourResetAt,
			UsedPercent: float64Pointer(80)},
		{Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60, ResetAt: weeklyResetAt,
			UsedPercent: float64Pointer(73)},
	}, now)
	if changed, err := AddStandardCost(usage, now, "gpt-5.6-sol", 1_000_000); err != nil || !changed {
		t.Fatalf("seed cost = (%t, %v)", changed, err)
	}

	rolledAt := now.Add(time.Hour)
	usage = Reconcile(usage, []Sample{
		{Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: fiveHourResetAt,
			UsedPercent: float64Pointer(80), SampledAt: now.Add(-time.Minute)},
		{Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60, ResetAt: rolledAt.Add(7 * 24 * time.Hour),
			UsedPercent: float64Pointer(5), SampledAt: rolledAt},
	}, rolledAt.Add(time.Minute))
	fiveHour := Find(usage, "codex|primary")
	weekly := Find(usage, "codex|secondary")
	if fiveHour == nil || fiveHour.StandardCostMicroUSD != 1_000_000 ||
		fiveHour.SampledUpstreamUsedPercent == nil || *fiveHour.SampledUpstreamUsedPercent != 80 {
		t.Fatalf("stale 5-hour sample was reset with weekly: %#v", fiveHour)
	}
	if weekly == nil || weekly.StandardCostMicroUSD != 0 || weekly.CountFromAt != rolledAt.Unix() {
		t.Fatalf("weekly rollback did not reset weekly: %#v", weekly)
	}
	if changed, err := AddStandardCost(usage, rolledAt.Add(time.Second), "gpt-5.6-sol", 500_000); err != nil || !changed {
		t.Fatalf("post-reset cost = (%t, %v)", changed, err)
	}
	if got := Find(usage, "codex|primary").StandardCostMicroUSD; got != 1_500_000 {
		t.Fatalf("old 5-hour period stopped accumulating: %d", got)
	}
	if got := Find(usage, "codex|secondary").StandardCostMicroUSD; got != 500_000 {
		t.Fatalf("new weekly period did not accumulate: %d", got)
	}

	usage = Reconcile(usage, []Sample{
		{Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: rolledAt.Add(5 * time.Hour),
			UsedPercent: float64Pointer(5), SampledAt: rolledAt.Add(2 * time.Minute)},
		{Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60, ResetAt: rolledAt.Add(7 * 24 * time.Hour),
			UsedPercent: float64Pointer(6), SampledAt: rolledAt.Add(2 * time.Minute)},
	}, rolledAt.Add(2*time.Minute))
	fiveHour = Find(usage, "codex|primary")
	if fiveHour == nil || fiveHour.StandardCostMicroUSD != 0 ||
		fiveHour.CountFromAt != rolledAt.Add(2*time.Minute).Unix() {
		t.Fatalf("fresh 5-hour rollback was not isolated: %#v", fiveHour)
	}
	if got := Find(usage, "codex|secondary").StandardCostMicroUSD; got != 500_000 {
		t.Fatalf("fresh 5-hour sample reset weekly again: %#v", Find(usage, "codex|secondary"))
	}
}

func TestReconcileKeepsCostWhenUpstreamUsageOnlyAdvances(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 24, 2, 29, 7, 0, time.UTC)
	resetAt := now.Add(6 * 24 * time.Hour)
	usedPercent := 37.0
	usage := Reconcile(nil, []Sample{{
		Key: "codex|primary", WindowSeconds: 604800, ResetAt: resetAt,
		UsedPercent: &usedPercent,
	}}, now)
	if changed, err := AddStandardCost(usage, now, "gpt-5.6-sol", 1_000_000); err != nil || !changed {
		t.Fatalf("seed cost = (%t, %v)", changed, err)
	}
	usedPercent = 38.0
	usage = Reconcile(usage, []Sample{{
		Key: "codex|primary", WindowSeconds: 604800, ResetAt: resetAt.Add(7 * time.Second),
		UsedPercent: &usedPercent,
	}}, now.Add(time.Minute))
	window := Find(usage, "codex|primary")
	if window.StandardCostMicroUSD != 1_000_000 || window.SampledUpstreamUsedPercent == nil ||
		*window.SampledUpstreamUsedPercent != usedPercent {
		t.Fatalf("advancing upstream usage changed cost: %#v", window)
	}
}

func TestReconcilePartialKeepsCostWhenFiveHourResetJitters(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 24, 2, 29, 7, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	usage := Reconcile(nil, []Sample{{
		Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: resetAt,
		UsedPercent: float64Pointer(10), SampledAt: now,
	}}, now)
	if changed, err := AddStandardCost(usage, now, "gpt-5.6-sol", 1_000_000); err != nil || !changed {
		t.Fatalf("seed cost = (%t, %v)", changed, err)
	}

	sampledAt := now.Add(time.Minute)
	usage = ReconcilePartial(usage, []Sample{{
		Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: resetAt.Add(3 * time.Minute),
		UsedPercent: float64Pointer(11), SampledAt: sampledAt,
	}}, sampledAt)
	window := Find(usage, "codex|primary")
	if window == nil || window.StandardCostMicroUSD != 1_000_000 ||
		window.SampledUpstreamUsedPercent == nil || *window.SampledUpstreamUsedPercent != 11 {
		t.Fatalf("reset_at jitter discarded cost: %#v", window)
	}
	if window.ResetAt != resetAt.Unix() {
		t.Fatalf("reset boundary shifted by jitter: got %d, want %d", window.ResetAt, resetAt.Unix())
	}
}

func TestReconcileRollbackOnlyResetsSampledWindow(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		reconcile func(*Usage, []Sample, time.Time) *Usage
	}{
		{name: "complete", reconcile: Reconcile},
		{name: "partial", reconcile: ReconcilePartial},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, time.August, 24, 2, 29, 7, 0, time.UTC)
			primaryResetAt := now.Add(4 * time.Hour)
			weeklyResetAt := now.Add(6 * 24 * time.Hour)
			usage := Reconcile(nil, []Sample{
				{Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: primaryResetAt,
					UsedPercent: float64Pointer(80), SampledAt: now},
				{Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60, ResetAt: weeklyResetAt,
					UsedPercent: float64Pointer(30), SampledAt: now},
			}, now)
			if changed, err := AddStandardCost(usage, now, "gpt-5.6-sol", 1_000_000); err != nil || !changed {
				t.Fatalf("seed cost = (%t, %v)", changed, err)
			}

			sampledAt := now.Add(time.Minute)
			usage = test.reconcile(usage, []Sample{
				{Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: sampledAt.Add(5 * time.Hour),
					UsedPercent: float64Pointer(5), SampledAt: sampledAt},
				{Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60, ResetAt: weeklyResetAt,
					UsedPercent: float64Pointer(31), SampledAt: sampledAt},
			}, sampledAt)
			primary := Find(usage, "codex|primary")
			weekly := Find(usage, "codex|secondary")
			if primary == nil || primary.StandardCostMicroUSD != 0 || primary.CountFromAt != sampledAt.Unix() {
				t.Fatalf("sampled 5-hour window was not reset: %#v", primary)
			}
			if weekly == nil || weekly.StandardCostMicroUSD != 1_000_000 || weekly.CountFromAt != 0 {
				t.Fatalf("weekly cost was reset with 5-hour rollback: %#v", weekly)
			}
		})
	}
}

func TestReconcileIgnoresOlderUpstreamUsageSample(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 24, 2, 29, 7, 0, time.UTC)
	resetAt := now.Add(6 * 24 * time.Hour)
	usage := Reconcile(nil, []Sample{{
		Key: "codex|primary", WindowSeconds: 604800, ResetAt: resetAt,
		UsedPercent: float64Pointer(60),
	}}, now)
	if changed, err := AddStandardCost(usage, now, "gpt-5.6-sol", 1_000_000); err != nil || !changed {
		t.Fatalf("seed cost = (%t, %v)", changed, err)
	}

	for _, seconds := range []int64{604800, 18000} {
		next := Reconcile(usage, []Sample{{
			Key: "codex|primary", WindowSeconds: seconds, ResetAt: resetAt.Add(-4 * 24 * time.Hour),
			UsedPercent: float64Pointer(20), SampledAt: now.Add(-time.Second),
		}}, now.Add(time.Minute))
		window := Find(next, "codex|primary")
		if window.StandardCostMicroUSD != 1_000_000 || window.WindowSeconds != 604800 ||
			window.SampledUpstreamUsedPercent == nil || *window.SampledUpstreamUsedPercent != 60 {
			t.Fatalf("older %ds sample reset quota cost: %#v", seconds, window)
		}
	}
}

func TestReconcileZeroesCostWhenPeriodRolls(t *testing.T) {
	t.Parallel()
	resetAt := time.Date(2026, time.August, 19, 3, 25, 0, 0, time.UTC)
	seed := func() *Usage {
		usage := Reconcile(nil, []Sample{
			{Key: "codex|primary", WindowSeconds: 604800, ResetAt: resetAt},
		}, resetAt.Add(-time.Hour))
		if changed, err := AddStandardCost(usage, resetAt.Add(-time.Hour), "gpt-5.4", 900_000); err != nil || !changed {
			t.Fatalf("seed cost = (%t, %v)", changed, err)
		}
		return usage
	}

	// 上游报告下一个周期的边界：容差不能吞掉整整一个窗口的推进。
	ahead := Reconcile(seed(), []Sample{
		{Key: "codex|primary", WindowSeconds: 604800, ResetAt: resetAt.Add(7 * 24 * time.Hour)},
	}, resetAt.Add(-time.Hour))
	if got := Find(ahead, "codex|primary").StandardCostMicroUSD; got != 0 {
		t.Fatalf("rolled window kept cost %d, want 0", got)
	}
	// 本地时间越过 reset 后，即便采样边界不变也必须开新周期。
	after := resetAt.Add(time.Minute)
	rolled := Reconcile(seed(), []Sample{
		{Key: "codex|primary", WindowSeconds: 604800, ResetAt: resetAt},
	}, after)
	window := Find(rolled, "codex|primary")
	if window.StandardCostMicroUSD != 0 || window.ResetAt != resetAt.Add(7*24*time.Hour).Unix() {
		t.Fatalf("expired window = %#v", window)
	}
}

func TestReconcileIgnoresSmallUpstreamUsageRollback(t *testing.T) {
	t.Parallel()
	// 渠道 526 复盘：Google remaining_fraction 浮点抖动使 used% 出现 0.001
	// 级的微回退，零容差判定曾把整周累计清空。微回退必须当噪声处理：
	// 只刷新采样基线，不动成本、count_from_at 和窗口边界。
	now := time.Date(2026, time.August, 24, 2, 29, 7, 0, time.UTC)
	resetAt := now.Add(6 * 24 * time.Hour)
	usage := Reconcile(nil, []Sample{{
		Key: "gemini models|gemini-weekly", Family: FamilyGemini, WindowSeconds: 604800,
		ResetAt: resetAt, UsedPercent: float64Pointer(80.5585),
	}}, now)
	if changed, err := AddStandardCost(usage, now, "gemini-3.6-flash-high", 72_417_000); err != nil || !changed {
		t.Fatalf("seed cost = (%t, %v)", changed, err)
	}

	sampledAt := now.Add(time.Minute)
	usage = Reconcile(usage, []Sample{{
		Key: "gemini models|gemini-weekly", Family: FamilyGemini, WindowSeconds: 604800,
		ResetAt: resetAt.Add(7 * time.Second), UsedPercent: float64Pointer(80.5575), SampledAt: sampledAt,
	}}, sampledAt.Add(time.Minute))
	window := Find(usage, "gemini models|gemini-weekly")
	if window.StandardCostMicroUSD != 72_417_000 || window.CountFromAt != 0 {
		t.Fatalf("small upstream usage drop cleared cost: %#v", window)
	}
	if window.StartedAt != resetAt.Add(-7*24*time.Hour).Unix() || window.ResetAt != resetAt.Unix() {
		t.Fatalf("small upstream usage drop moved boundaries: %#v", window)
	}
	if window.SampledUpstreamUsedPercent == nil || *window.SampledUpstreamUsedPercent != 80.5575 {
		t.Fatalf("small upstream usage drop did not refresh baseline: %#v", window)
	}
	if changed, err := AddStandardCost(usage, sampledAt.Add(time.Second), "gemini-3.6-flash-high", 500_000); err != nil || !changed {
		t.Fatalf("post-noise cost = (%t, %v)", changed, err)
	}
	if got := Find(usage, "gemini models|gemini-weekly").StandardCostMicroUSD; got != 72_917_000 {
		t.Fatalf("post-noise accumulated cost = %d, want 72917000", got)
	}
}

func TestReconcileIgnoresSmallUsageDropAcrossWindows(t *testing.T) {
	t.Parallel()
	// 微降不能当成回退；两个窗口同时抖动时两窗成本都要留下。
	now := time.Date(2026, time.August, 24, 2, 0, 0, 0, time.UTC)
	fiveHourResetAt := now.Add(4 * time.Hour)
	weeklyResetAt := now.Add(6 * 24 * time.Hour)
	usage := Reconcile(nil, []Sample{
		{Key: "gemini models|gemini-5h", Family: FamilyGemini, WindowSeconds: 18000,
			ResetAt: fiveHourResetAt, UsedPercent: float64Pointer(40.0)},
		{Key: "gemini models|gemini-weekly", Family: FamilyGemini, WindowSeconds: 604800,
			ResetAt: weeklyResetAt, UsedPercent: float64Pointer(80.5585)},
	}, now)
	if changed, err := AddStandardCost(usage, now, "gemini-3.6-flash-high", 1_000_000); err != nil || !changed {
		t.Fatalf("seed cost = (%t, %v)", changed, err)
	}

	sampledAt := now.Add(time.Hour)
	usage = Reconcile(usage, []Sample{
		{Key: "gemini models|gemini-5h", Family: FamilyGemini, WindowSeconds: 18000,
			ResetAt: fiveHourResetAt, UsedPercent: float64Pointer(39.999), SampledAt: sampledAt},
		{Key: "gemini models|gemini-weekly", Family: FamilyGemini, WindowSeconds: 604800,
			ResetAt: weeklyResetAt.Add(24 * time.Hour), UsedPercent: float64Pointer(80.5575), SampledAt: sampledAt},
	}, sampledAt.Add(time.Minute))
	for key, wantUsed := range map[string]float64{
		"gemini models|gemini-5h":     39.999,
		"gemini models|gemini-weekly": 80.5575,
	} {
		window := Find(usage, key)
		if window == nil || window.StandardCostMicroUSD != 1_000_000 || window.CountFromAt != 0 {
			t.Fatalf("window %q cleared by small usage drop: %#v", key, window)
		}
		if window.SampledUpstreamUsedPercent == nil || *window.SampledUpstreamUsedPercent != wantUsed {
			t.Fatalf("window %q did not refresh baseline: %#v", key, window)
		}
	}
}

func TestReconcileUsageDropAtEpsilonBoundary(t *testing.T) {
	t.Parallel()
	seed := func() *Usage {
		now := time.Date(2026, time.August, 24, 2, 29, 7, 0, time.UTC)
		resetAt := now.Add(6 * 24 * time.Hour)
		usage := Reconcile(nil, []Sample{{
			Key: "codex|primary", Family: FamilyAll, WindowSeconds: 604800,
			ResetAt: resetAt, UsedPercent: float64Pointer(80),
		}}, now)
		if changed, err := AddStandardCost(usage, now, "gpt-5.6-sol", 1_000_000); err != nil || !changed {
			t.Fatalf("seed cost = (%t, %v)", changed, err)
		}
		return usage
	}
	reconcile := func(usage *Usage, usedPercent float64) (*Usage, time.Time) {
		sampledAt := time.Date(2026, time.August, 24, 2, 30, 7, 0, time.UTC)
		usage = Reconcile(usage, []Sample{{
			Key: "codex|primary", Family: FamilyAll, WindowSeconds: 604800,
			ResetAt:     time.Date(2026, time.August, 30, 2, 29, 7, 0, time.UTC),
			UsedPercent: float64Pointer(usedPercent), SampledAt: sampledAt,
		}}, sampledAt)
		return usage, sampledAt
	}

	// 降幅恰好等于容差（80→79）：开区间语义，仍视为噪声，不切断成本。
	usage, _ := reconcile(seed(), 79)
	window := Find(usage, "codex|primary")
	if window.StandardCostMicroUSD != 1_000_000 || window.CountFromAt != 0 {
		t.Fatalf("drop at epsilon bound cleared cost: %#v", window)
	}
	if window.SampledUpstreamUsedPercent == nil || *window.SampledUpstreamUsedPercent != 79 {
		t.Fatalf("drop at epsilon bound did not refresh baseline: %#v", window)
	}

	// 降幅刚过容差（80→78.9）：判定为上游重置，从采样点重新累计。
	usage, sampledAt := reconcile(seed(), 78.9)
	window = Find(usage, "codex|primary")
	if window.StandardCostMicroUSD != 0 || window.CountFromAt != sampledAt.Unix() {
		t.Fatalf("drop beyond epsilon did not reset window: %#v", window)
	}
	if changed, err := AddStandardCost(usage, sampledAt.Add(time.Second), "gpt-5.6-sol", 500); err != nil || !changed {
		t.Fatalf("post-reset cost = (%t, %v)", changed, err)
	}
	if got := Find(usage, "codex|primary").StandardCostMicroUSD; got != 500 {
		t.Fatalf("post-reset cost = %d, want 500", got)
	}
}
