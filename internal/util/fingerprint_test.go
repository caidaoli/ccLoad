package util

import (
	"math"
	"testing"
)

func TestParseFingerprintNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"7", 7, true},
		{"  42\n", 42, true},
		{"答案是123", 123, true},
		{"0", 0, false},
		{"356", 0, false},
		{"", 0, false},
		{"no digits", 0, false},
	}
	for _, tc := range cases {
		got, ok := ParseFingerprintNumber(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("ParseFingerprintNumber(%q)=(%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFingerprintDistributionAndStats(t *testing.T) {
	nums := []int{1, 1, 2, 355}
	dist := FingerprintDistribution(nums)
	if len(dist) != FingerprintRange {
		t.Fatalf("len=%d", len(dist))
	}
	if math.Abs(dist[0]-0.5) > 1e-9 || math.Abs(dist[1]-0.25) > 1e-9 || math.Abs(dist[354]-0.25) > 1e-9 {
		t.Fatalf("dist wrong: %v", dist)
	}
	var sum float64
	for _, v := range dist {
		sum += v
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("sum=%v", sum)
	}
	st := CalculateFingerprintStats(nums)
	if st.Mode != 1 || st.ModeCount != 2 || st.Min != 1 || st.Max != 355 || st.Unique != 3 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestFingerprintSimilarity_Identical(t *testing.T) {
	nums := []int{10, 10, 20, 30}
	dist := FingerprintDistribution(nums)
	st := CalculateFingerprintStats(nums)
	sim := CalculateFingerprintSimilarity(dist, dist, st, st)
	if sim.CosineSimilarity < 0.999 || sim.ModeScore != 1 || sim.OverallScore < 0.999 {
		t.Fatalf("identical sim=%+v", sim)
	}
}

func TestFingerprintSimilarity_ModeMatchIsDiagnosticOnly(t *testing.T) {
	a := append(repeatFingerprintNumber(10, 20), repeatFingerprintNumber(20, 19)...)
	b := append(repeatFingerprintNumber(10, 20), repeatFingerprintNumber(300, 19)...)
	da, db := FingerprintDistribution(a), FingerprintDistribution(b)
	sa, sb := CalculateFingerprintStats(a), CalculateFingerprintStats(b)
	sim := CalculateFingerprintSimilarity(da, db, sa, sb)
	if sim.ModeScore != 1 {
		t.Fatalf("expected matching mode diagnostic, got %+v", sim)
	}
	want := sim.CosineSimilarity * math.Exp(-sim.JSDivergence)
	if math.Abs(sim.OverallScore-want) > 1e-12 {
		t.Fatalf("mode match inflated overall score: got %f, want distribution score %f", sim.OverallScore, want)
	}
	if sim.OverallScore >= 0.5 {
		t.Fatalf("mostly different distributions scored too highly: %+v", sim)
	}
}

func TestFingerprintSimilarity_RanksCloserBiasFirst(t *testing.T) {
	baseline := append(repeatFingerprintNumber(37, 60), repeatFingerprintNumber(120, 20)...)
	closer := append(repeatFingerprintNumber(37, 55), repeatFingerprintNumber(120, 25)...)
	different := append(repeatFingerprintNumber(220, 60), repeatFingerprintNumber(300, 20)...)

	baseDist := FingerprintDistribution(baseline)
	baseStats := CalculateFingerprintStats(baseline)
	closerScore := CalculateFingerprintSimilarity(
		FingerprintDistribution(closer),
		baseDist,
		CalculateFingerprintStats(closer),
		baseStats,
	).OverallScore
	differentScore := CalculateFingerprintSimilarity(
		FingerprintDistribution(different),
		baseDist,
		CalculateFingerprintStats(different),
		baseStats,
	).OverallScore
	if closerScore <= differentScore {
		t.Fatalf("closer score=%f, different score=%f", closerScore, differentScore)
	}
}

func repeatFingerprintNumber(number, count int) []int {
	values := make([]int, count)
	for i := range values {
		values[i] = number
	}
	return values
}
