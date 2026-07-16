package app

import (
	"math"
	"testing"

	modelpkg "ccLoad/internal/model"
)

func TestEffPriorityBucket_FloatEdge(t *testing.T) {
	// 模拟浮点误差：值略小于整数边界时，不应被截断到前一档。
	scaledPos := math.Nextafter(51, 0) // 50.999999999...
	pPos := scaledPos / 10
	if got := effPriorityBucket(pPos); got != 51 {
		t.Fatalf("expected bucket=51, got %d (p=%v scaled=%v)", got, pPos, pPos*10)
	}

	scaledNeg := math.Nextafter(-51, 0) // -50.999999999...
	pNeg := scaledNeg / 10
	if got := effPriorityBucket(pNeg); got != -51 {
		t.Fatalf("expected bucket=-51, got %d (p=%v scaled=%v)", got, pNeg, pNeg*10)
	}
}

func TestMedianFloat64(t *testing.T) {
	t.Parallel()
	if got := medianFloat64(nil); got != 0 {
		t.Fatalf("empty=%v", got)
	}
	if got := medianFloat64([]float64{1.0}); got != 0 {
		// fewer than 2 peers → 0 for relative scoring
		t.Fatalf("single=%v want 0", got)
	}
	if got := medianFloat64([]float64{1.0, 3.0}); got != 2.0 {
		t.Fatalf("even=%v want 2", got)
	}
	if got := medianFloat64([]float64{1.0, 2.0, 100.0}); got != 2.0 {
		t.Fatalf("odd=%v want 2", got)
	}
}

func TestCalculateEffectivePriority_TTFBPenalty(t *testing.T) {
	t.Parallel()
	s := &Server{}
	cfg := modelpkg.HealthScoreConfig{
		Enabled:                  true,
		SuccessRatePenaltyWeight: 100,
		MinConfidentSample:       20,
		EnableTTFBScore:          true,
		TTFBPenaltyWeight:        20,
		TTFBMaxSlowRatio:         2.0,
		TTFBMinConfidentSample:   10,
	}
	ch := &modelpkg.Config{ID: 1, Priority: 100}

	// Perfect success, same as median → no ttfb penalty
	stats := modelpkg.ChannelHealthStats{
		SuccessRate: 1, SampleCount: 100,
		AvgFirstByteSeconds: 1.0, FirstByteSampleCount: 20,
	}
	got := s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 100 {
		t.Fatalf("equal median: got %v want 100", got)
	}

	// 2x median, full confidence → penalty 20
	stats.AvgFirstByteSeconds = 2.0
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 80 {
		t.Fatalf("2x median: got %v want 80", got)
	}

	// 4x median capped at (s-1)=2 → penalty 40
	stats.AvgFirstByteSeconds = 4.0
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 60 {
		t.Fatalf("4x median capped: got %v want 60", got)
	}

	// Faster than median → no reward
	stats.AvgFirstByteSeconds = 0.5
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 100 {
		t.Fatalf("faster: got %v want 100", got)
	}

	// Low sample halves confidence (5/10)
	stats.AvgFirstByteSeconds = 2.0
	stats.FirstByteSampleCount = 5
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 90 {
		t.Fatalf("half confidence: got %v want 90", got)
	}

	// Disabled ttfb
	cfg.EnableTTFBScore = false
	stats.FirstByteSampleCount = 20
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 100 {
		t.Fatalf("disabled: got %v want 100", got)
	}

	// Fail penalty still applies with ttfb
	cfg.EnableTTFBScore = true
	stats.SuccessRate = 0.8
	stats.SampleCount = 20
	stats.AvgFirstByteSeconds = 2.0
	// fail: 0.2*100*1=20, ttfb:20 → 100-20-20=60
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 60 {
		t.Fatalf("fail+ttfb: got %v want 60", got)
	}

	// medianTTFB=0 disables relative penalty
	stats.SuccessRate = 1
	got = s.calculateEffectivePriority(ch, stats, cfg, 0)
	if got != 100 {
		t.Fatalf("no median: got %v want 100", got)
	}
}
