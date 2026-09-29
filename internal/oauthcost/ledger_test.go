package oauthcost

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

type testLog struct {
	at    time.Time
	model string
	cost  int64
}

// countedCost 模拟存储层：日志按秒入桶，取窗口计数区间内的行交给 CostFromLedger。
func countedCost(window *Window, logs ...testLog) int64 {
	counted := CountedRange(window)
	totals := make([]LedgerTotal, 0, len(logs))
	for _, entry := range logs {
		bucket := entry.at.Unix()
		if bucket < counted.From || bucket >= counted.Until {
			continue
		}
		totals = append(totals, LedgerTotal{Model: entry.model, CostMicroUSD: entry.cost})
	}
	return CostFromLedger(window, totals)
}

// costAt 返回 at 时刻视图中 key 窗口的成本；窗口不存在时返回 -1。
func costAt(usage *Usage, key string, at time.Time, logs ...testLog) int64 {
	for _, window := range WindowsAt(usage, at) {
		if window != nil && window.Key == key {
			return countedCost(window, logs...)
		}
	}
	return -1
}

func TestLedgerSecondBucketsAndWindowBoundaries(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	reset := start.Add(5 * time.Hour)
	usage := &Usage{Windows: []*Window{{
		Key: "gemini|five_hour", Family: FamilyGemini,
		WindowSeconds: int64(5 * time.Hour / time.Second),
		StartedAt:     start.Unix(), ResetAt: reset.Unix(),
	}}}
	logs := []testLog{
		{at: start.Add(-time.Millisecond), model: "gemini-x", cost: 10},
		{at: start, model: "gemini-x", cost: 20},
		{at: reset.Add(-time.Millisecond), model: "gemini-x", cost: 30},
		{at: reset, model: "gemini-x", cost: 40},
		{at: start.Add(time.Second), model: "claude-x", cost: 50},
	}
	if got := costAt(usage, "gemini|five_hour", start.Add(time.Hour), logs...); got != 50 {
		t.Fatalf("counted cost = %d, want 50", got)
	}
	if got := costAt(usage, "gemini|five_hour", reset, logs...); got != 40 {
		t.Fatalf("next period cost = %d, want 40", got)
	}
}

func geminiWeeklySample(resetAt time.Time, used float64, at time.Time) []Sample {
	return []Sample{{
		Key: "gemini|weekly", Family: FamilyGemini, WindowSeconds: 7 * 24 * 60 * 60,
		ResetAt: resetAt, UsedPercent: float64Pointer(used), SampledAt: at,
	}}
}

// rollbackFixture 构造一个 40% → 5% 的同周期回退：start 为周期起点，cut 为截断采样时刻。
func rollbackFixture(t *testing.T) (usage *Usage, start, cut time.Time) {
	t.Helper()
	start = time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	resetAt := start.Add(7 * 24 * time.Hour)
	first := start.Add(24 * time.Hour)
	cut = first.Add(time.Hour)
	usage = Reconcile(nil, geminiWeeklySample(resetAt, 40, first), first)
	usage = Reconcile(usage, geminiWeeklySample(resetAt, 5, cut), cut)
	return usage, start, cut
}

func TestCostFromLedgerMatchesFamilyAndBaselineKey(t *testing.T) {
	t.Parallel()
	window := &Window{Key: "gemini models|quota", Family: FamilyGemini, WindowSeconds: 18000, StartedAt: 1, ResetAt: 18001}
	totals := []LedgerTotal{
		{Model: "gemini-3.6-flash-high", CostMicroUSD: 100},
		{Model: "claude-opus-4-6", CostMicroUSD: 1000},
		{WindowKey: "gemini models|quota", CostMicroUSD: 10},
		{WindowKey: "other|quota", CostMicroUSD: 10000},
		{Model: "gemini-3.6-pro", CostMicroUSD: -5},
	}
	if got := CostFromLedger(window, totals); got != 110 {
		t.Fatalf("CostFromLedger = %d, want 110", got)
	}
	saturated := []LedgerTotal{
		{Model: "gemini-a", CostMicroUSD: math.MaxInt64},
		{Model: "gemini-b", CostMicroUSD: 1},
	}
	if got := CostFromLedger(window, saturated); got != math.MaxInt64 {
		t.Fatalf("saturated CostFromLedger = %d, want MaxInt64", got)
	}
	if got := CostFromLedger(nil, totals); got != 0 {
		t.Fatalf("nil window cost = %d, want 0", got)
	}
}

func TestLedgerModelPrefersActualModel(t *testing.T) {
	t.Parallel()
	if got := LedgerModel(" alias ", " gemini-3.6-flash-high "); got != "gemini-3.6-flash-high" {
		t.Fatalf("LedgerModel with actual = %q", got)
	}
	if got := LedgerModel(" gemini-3.6-pro ", "  "); got != "gemini-3.6-pro" {
		t.Fatalf("LedgerModel without actual = %q", got)
	}
}

func TestRollbackRestoresWhenReadingReturnsToPreviousLevel(t *testing.T) {
	t.Parallel()
	usage, start, cut := rollbackFixture(t)
	w := Find(usage, "gemini|weekly")
	if w.CountFromAt != cut.Unix() || w.Rollback == nil || CountFrom(w) != cut.Unix() {
		t.Fatalf("rollback did not cut with evidence: %#v", w)
	}
	// 读数一步回到截断前水位（40−ε 以上）：5% 是瞬时低读数，恢复原计数起点。
	latest := cut.Add(2 * time.Hour)
	usage = Reconcile(usage, geminiWeeklySample(start.Add(7*24*time.Hour), 44, latest), latest)
	w = Find(usage, "gemini|weekly")
	if CountFrom(w) != start.Unix() || w.Rollback != nil {
		t.Fatalf("transient rollback was not restored: %#v", w)
	}
	if err := Validate(usage); err != nil {
		t.Fatalf("Validate after restore: %v", err)
	}
}

func TestRollbackConfirmsRealResetWhenReadingClimbsGradually(t *testing.T) {
	t.Parallel()
	usage, start, cut := rollbackFixture(t)
	resetAt := start.Add(7 * 24 * time.Hour)
	// 从 5% 升到 9%：离开截断水位但远低于截断前水位，额度确实重置过，确认截断。
	climbed := cut.Add(time.Hour)
	usage = Reconcile(usage, geminiWeeklySample(resetAt, 9, climbed), climbed)
	w := Find(usage, "gemini|weekly")
	if w.CountFromAt != cut.Unix() || w.Rollback != nil {
		t.Fatalf("gradual climb must confirm the cut: %#v", w)
	}
	// 证据已结案：之后同周期用回 44% 也不能恢复截断前的成本。
	later := cut.Add(3 * time.Hour)
	usage = Reconcile(usage, geminiWeeklySample(resetAt, 44, later), later)
	if w = Find(usage, "gemini|weekly"); w.CountFromAt != cut.Unix() || w.Rollback != nil {
		t.Fatalf("confirmed cut was reopened: %#v", w)
	}
}

func TestRollbackStaysPendingWhileReadingStaysLow(t *testing.T) {
	t.Parallel()
	usage, start, cut := rollbackFixture(t)
	resetAt := start.Add(7 * 24 * time.Hour)
	assertPending := func(stage string) {
		t.Helper()
		w := Find(usage, "gemini|weekly")
		if w.CountFromAt != cut.Unix() || w.Rollback == nil || w.Rollback.PreviousCountFrom != start.Unix() ||
			w.Rollback.PreviousUsedPercent != 40 || w.Rollback.CutUsedPercent != 5 {
			t.Fatalf("%s: evidence changed while pending: %#v", stage, w)
		}
	}
	// 5.8% 仍在截断水位 +ε 以内：继续待定。
	jitter := cut.Add(20 * time.Minute)
	usage = Reconcile(usage, geminiWeeklySample(resetAt, 5.8, jitter), jitter)
	assertPending("jitter")
	// 再次下探到 3%：吸收，截断点与证据都不前移。
	dip := cut.Add(40 * time.Minute)
	usage = Reconcile(usage, geminiWeeklySample(resetAt, 3, dip), dip)
	assertPending("repeated drop")
	latest := cut.Add(2 * time.Hour)
	usage = Reconcile(usage, geminiWeeklySample(resetAt, 44, latest), latest)
	if w := Find(usage, "gemini|weekly"); CountFrom(w) != start.Unix() || w.Rollback != nil {
		t.Fatalf("pending rollback was not restored: %#v", w)
	}
}

func TestRollbackLowBandTakesPriorityOverRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                  string
		previous, cut, latest float64
		recovery              *float64
	}{
		{name: "overlap", previous: 2, cut: 0.8, latest: 1},
		{name: "exact low boundary", previous: 2.5, cut: 1, latest: 2, recovery: float64Pointer(2.25)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
			resetAt := start.Add(7 * 24 * time.Hour)
			first := start.Add(time.Hour)
			cutAt := first.Add(time.Hour)
			usage := Reconcile(nil, geminiWeeklySample(resetAt, tc.previous, first), first)
			usage = Reconcile(usage, geminiWeeklySample(resetAt, tc.cut, cutAt), cutAt)
			latestAt := cutAt.Add(time.Hour)
			usage = Reconcile(usage, geminiWeeklySample(resetAt, tc.latest, latestAt), latestAt)
			w := Find(usage, "gemini|weekly")
			if w == nil || CountFrom(w) != cutAt.Unix() || w.Rollback == nil {
				t.Fatalf("low band must remain pending: %#v", w)
			}
			if tc.recovery != nil {
				recoveredAt := latestAt.Add(time.Hour)
				usage = Reconcile(usage, geminiWeeklySample(resetAt, *tc.recovery, recoveredAt), recoveredAt)
				w = Find(usage, "gemini|weekly")
				if w == nil || CountFrom(w) != start.Unix() || w.Rollback != nil {
					t.Fatalf("reading above low band must restore: %#v", w)
				}
			}
			if err := Validate(usage); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRollbackEvidenceClearedByResetAndEpoch(t *testing.T) {
	t.Parallel()
	usage, start, cut := rollbackFixture(t)
	reset := Reset(usage, cut.Add(time.Hour), nil)
	if w := Find(reset, "gemini|weekly"); w.Rollback != nil || w.CountFromAt != cut.Add(time.Hour).Unix() {
		t.Fatalf("manual reset kept rollback evidence: %#v", w)
	}
	if err := Validate(reset); err != nil {
		t.Fatalf("Validate after reset: %v", err)
	}

	// 纪元越过恢复目标：证据作废，保留截断。
	usage.EpochAt = start.Add(time.Minute).Unix()
	var view *Window
	for _, candidate := range WindowsAt(usage, cut.Add(time.Hour)) {
		if candidate.Key == "gemini|weekly" {
			view = candidate
		}
	}
	if view == nil || view.Rollback != nil || CountFrom(view) != cut.Unix() {
		t.Fatalf("epoch did not supersede rollback evidence: %#v", view)
	}
}

func TestValidateRejectsCorruptRollbackEvidence(t *testing.T) {
	t.Parallel()
	usage, _, cut := rollbackFixture(t)
	for name, mutate := range map[string]func(*Window){
		"cut not lower":          func(w *Window) { w.Rollback.CutUsedPercent = w.Rollback.PreviousUsedPercent },
		"count from not the cut": func(w *Window) { w.CountFromAt = w.Rollback.PreviousCountFrom },
		"previous after cut":     func(w *Window) { w.Rollback.PreviousCountFrom = cut.Unix() },
		"nan percent":            func(w *Window) { w.Rollback.PreviousUsedPercent = math.NaN() },
		"missing previous":       func(w *Window) { w.Rollback.PreviousCountFrom = 0 },
	} {
		corrupt := Clone(usage)
		mutate(Find(corrupt, "gemini|weekly"))
		if err := Validate(corrupt); err == nil {
			t.Fatalf("%s: Validate accepted corrupt evidence", name)
		}
	}
	if err := Validate(usage); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
}

func TestCostViewShape(t *testing.T) {
	t.Parallel()
	usage := &Usage{CreditStandardCostMicroUSD: 7, Windows: []*Window{
		{Key: "gemini|weekly", Family: FamilyGemini, WindowSeconds: 604800, StartedAt: 100, ResetAt: 604900},
		{Key: "claude|weekly", Family: FamilyNonGemini, WindowSeconds: 604800, StartedAt: 100, ResetAt: 604900},
	}}
	view := NewCostView(usage, usage.Windows, [][]LedgerTotal{{{Model: "gemini-x", CostMicroUSD: 5}}})
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Credit  int64 `json:"credit_standard_cost_microusd"`
		Windows []struct {
			Key           string `json:"key"`
			WindowSeconds int64  `json:"window_seconds"`
			ResetAt       int64  `json:"reset_at"`
			Cost          *int64 `json:"standard_cost_microusd"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Credit != 7 || len(decoded.Windows) != 2 {
		t.Fatalf("view JSON = %s", raw)
	}
	first, second := decoded.Windows[0], decoded.Windows[1]
	if first.Key != "gemini|weekly" || first.WindowSeconds != 604800 || first.ResetAt != 604900 ||
		first.Cost == nil || *first.Cost != 5 || second.Cost == nil || *second.Cost != 0 {
		t.Fatalf("view JSON = %s", raw)
	}
	if got := view.FindWindow("claude|weekly"); got == nil || got.StandardCostMicroUSD != 0 {
		t.Fatalf("FindWindow(claude) = %#v", got)
	}
	if view.FindWindow("missing") != nil || view.FindWindow("") != nil || (*CostView)(nil).FindWindow("gemini|weekly") != nil {
		t.Fatal("FindWindow must return nil for unknown keys and nil views")
	}
	if NewCostView(nil, nil, nil) != nil {
		t.Fatal("nil usage must produce nil view")
	}
}
