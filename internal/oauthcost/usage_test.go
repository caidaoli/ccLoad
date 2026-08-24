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

func TestReconcileUpstreamResetClearsFiveHourAndWeeklyWindowsTogether(t *testing.T) {
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

	// additional 没出现在触发重置的批次里，也必须保留并随账户一起清零。
	for _, key := range []string{"codex|primary", "codex|secondary", "codex|additional"} {
		window := Find(usage, key)
		if window == nil || window.StandardCostMicroUSD != 0 || window.CountFromAt != sampledAt.Unix() {
			t.Fatalf("window %q was not reset with the account: %#v", key, window)
		}
	}
	if changed, err := AddStandardCost(usage, sampledAt.Add(time.Second), "gpt-5.6-sol", 500_000); err != nil || !changed {
		t.Fatalf("post-reset cost = (%t, %v)", changed, err)
	}
	if got := Find(usage, "codex|additional").StandardCostMicroUSD; got != 500_000 {
		t.Fatalf("missing window stopped accumulating after reset: %d", got)
	}
}

func TestReconcileAccountResetIgnoresStaleSiblingWindow(t *testing.T) {
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

	accountResetAt := now.Add(time.Hour)
	usage = Reconcile(usage, []Sample{
		// 被动合并结果仍带着重置前的 5h 样本。
		{Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: fiveHourResetAt,
			UsedPercent: float64Pointer(80), SampledAt: now.Add(-time.Minute)},
		{Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60, ResetAt: accountResetAt.Add(7 * 24 * time.Hour),
			UsedPercent: float64Pointer(5), SampledAt: accountResetAt},
	}, accountResetAt.Add(time.Minute))
	fiveHour := Find(usage, "codex|primary")
	if fiveHour == nil || fiveHour.StandardCostMicroUSD != 0 || fiveHour.CountFromAt != accountResetAt.Unix() ||
		fiveHour.SampledUpstreamUsedPercent != nil || fiveHour.ResetAt != accountResetAt.Add(5*time.Hour).Unix() {
		t.Fatalf("stale sibling sample changed reset state: %#v", fiveHour)
	}
	if changed, err := AddStandardCost(usage, accountResetAt.Add(time.Second), "gpt-5.6-sol", 500_000); err != nil || !changed {
		t.Fatalf("post-reset cost = (%t, %v)", changed, err)
	}

	// 首个新鲜 5h 样本只能建立新基线，不能把重置后的成本再次清空。
	usage = Reconcile(usage, []Sample{
		{Key: "codex|primary", WindowSeconds: 5 * 60 * 60, ResetAt: accountResetAt.Add(5 * time.Hour),
			UsedPercent: float64Pointer(5), SampledAt: accountResetAt.Add(2 * time.Minute)},
		{Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60, ResetAt: accountResetAt.Add(7 * 24 * time.Hour),
			UsedPercent: float64Pointer(6), SampledAt: accountResetAt.Add(2 * time.Minute)},
	}, accountResetAt.Add(2*time.Minute))
	for _, key := range []string{"codex|primary", "codex|secondary"} {
		if window := Find(usage, key); window == nil || window.StandardCostMicroUSD != 500_000 {
			t.Fatalf("fresh window %q triggered a second reset: %#v", key, window)
		}
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

	usage = Reconcile(usage, []Sample{{
		Key: "codex|primary", WindowSeconds: 604800, ResetAt: resetAt.Add(-4 * 24 * time.Hour),
		UsedPercent: float64Pointer(20), SampledAt: now.Add(-time.Second),
	}}, now.Add(time.Minute))
	window := Find(usage, "codex|primary")
	if window.StandardCostMicroUSD != 1_000_000 || window.SampledUpstreamUsedPercent == nil ||
		*window.SampledUpstreamUsedPercent != 60 {
		t.Fatalf("older sample reset quota cost: %#v", window)
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
