package app

import (
	"math"
	"testing"
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
