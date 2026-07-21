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

func TestFingerprintSimilarity_ModeWeight(t *testing.T) {
	a := make([]int, 0, 40)
	b := make([]int, 0, 40)
	for i := 0; i < 40; i++ {
		a = append(a, 1)
		b = append(b, 100)
	}
	da, db := FingerprintDistribution(a), FingerprintDistribution(b)
	sa, sb := CalculateFingerprintStats(a), CalculateFingerprintStats(b)
	sim := CalculateFingerprintSimilarity(da, db, sa, sb)
	if sim.ModeScore >= 1 {
		t.Fatalf("expected mode score < 1, got %+v", sim)
	}
	if sim.OverallScore >= 0.9 {
		t.Fatalf("expected lower overall, got %+v", sim)
	}
}
