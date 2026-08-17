package oauthcost

import (
	"testing"
	"time"
)

func TestMonthlyQuotaRolloverClampsToAnchorDay(t *testing.T) {
	t.Parallel()
	jan31 := time.Date(2027, time.January, 31, 8, 0, 0, 0, time.UTC)
	usage := Reconcile(nil, nil, &jan31, jan31.Add(-24*time.Hour))
	if usage == nil || usage.Monthly == nil || usage.Monthly.ResetDay != 31 {
		t.Fatalf("initial monthly usage = %#v", usage)
	}
	for _, test := range []struct {
		at        time.Time
		wantStart time.Time
		wantReset time.Time
	}{
		{at: jan31, wantStart: jan31, wantReset: time.Date(2027, time.February, 28, 8, 0, 0, 0, time.UTC)},
		{at: time.Date(2027, time.February, 28, 8, 0, 0, 0, time.UTC), wantStart: time.Date(2027, time.February, 28, 8, 0, 0, 0, time.UTC), wantReset: time.Date(2027, time.March, 31, 8, 0, 0, 0, time.UTC)},
		{at: time.Date(2027, time.March, 31, 8, 0, 0, 0, time.UTC), wantStart: time.Date(2027, time.March, 31, 8, 0, 0, 0, time.UTC), wantReset: time.Date(2027, time.April, 30, 8, 0, 0, 0, time.UTC)},
	} {
		changed, err := AddStandardCost(usage, test.at, 1)
		if err != nil || !changed {
			t.Fatalf("AddStandardCost(%s) = (%t, %v)", test.at, changed, err)
		}
		if usage.Monthly.StartedAt != test.wantStart.Unix() || usage.Monthly.ResetAt != test.wantReset.Unix() ||
			usage.Monthly.StandardCostMicroUSD != 1 {
			t.Fatalf("monthly window after %s = %#v", test.at, usage.Monthly)
		}
	}

	leapJan31 := time.Date(2028, time.January, 31, 8, 0, 0, 0, time.UTC)
	leap := Reconcile(nil, nil, &leapJan31, leapJan31.Add(-time.Hour))
	if changed, err := AddStandardCost(leap, leapJan31, 1); err != nil || !changed {
		t.Fatalf("leap rollover = (%t, %v)", changed, err)
	}
	wantLeapReset := time.Date(2028, time.February, 29, 8, 0, 0, 0, time.UTC)
	if leap.Monthly.ResetAt != wantLeapReset.Unix() {
		t.Fatalf("leap reset = %s, want %s", time.Unix(leap.Monthly.ResetAt, 0), wantLeapReset)
	}
}

func TestManualResetCutoffSurvivesQuotaRefresh(t *testing.T) {
	t.Parallel()
	periodStart := time.Date(2026, time.August, 13, 0, 0, 0, 0, time.UTC)
	manualReset := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	usage := &Usage{Weekly: &Window{
		StartedAt: periodStart.Unix(), ResetAt: periodStart.Add(7 * 24 * time.Hour).Unix(),
		StandardCostMicroUSD: 10_000_000,
	}}
	usage = Reset(usage, manualReset, 250_000)
	upstreamReset := manualReset.Add(7 * 24 * time.Hour)
	usage = Reconcile(usage, &upstreamReset, nil, manualReset.Add(time.Second))
	if usage.Weekly.CountFromAt != manualReset.Unix() || usage.Weekly.StandardCostMicroUSD != 250_000 {
		t.Fatalf("manual reset cutoff was not preserved: %#v", usage.Weekly)
	}
	if changed, err := AddStandardCost(usage, manualReset.Add(-time.Second), 1_000_000); err != nil || changed {
		t.Fatalf("late old-period log = (%t, %v), want ignored", changed, err)
	}
	if changed, err := AddStandardCost(usage, manualReset.Add(time.Second), 500_000); err != nil || !changed {
		t.Fatalf("new-period log = (%t, %v), want accumulated", changed, err)
	}
	if usage.Weekly.StandardCostMicroUSD != 750_000 {
		t.Fatalf("manual reset cost = %d, want 750000", usage.Weekly.StandardCostMicroUSD)
	}
}

func TestMonthlyQuotaRefreshAdvancesClampedResetWithOriginalAnchor(t *testing.T) {
	t.Parallel()
	for _, year := range []int{2027, 2028} {
		jan31 := time.Date(year, time.January, 31, 8, 0, 0, 0, time.UTC)
		februaryReset := addMonthsClamped(jan31, 1, 31)
		usage := &Usage{Monthly: &Window{
			StartedAt: jan31.Unix(), ResetAt: februaryReset.Unix(), ResetDay: 31,
			StandardCostMicroUSD: 4_000_000,
		}}
		usage = Reconcile(usage, nil, &februaryReset, februaryReset)
		wantReset := time.Date(year, time.March, 31, 8, 0, 0, 0, time.UTC)
		if usage.Monthly.StartedAt != februaryReset.Unix() || usage.Monthly.ResetAt != wantReset.Unix() ||
			usage.Monthly.ResetDay != 31 || usage.Monthly.StandardCostMicroUSD != 0 {
			t.Fatalf("year %d reconciled monthly window = %#v", year, usage.Monthly)
		}
	}
}
