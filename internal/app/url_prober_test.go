package app

import (
	"testing"
	"time"
)

func TestURLProber_SkipsRecentlyUsedURLs(t *testing.T) {
	sel := NewURLSelector()
	sel.RecordLatency(1, "https://a.com", 100*time.Millisecond) // 刚用过

	p := &URLProber{
		selector:   sel,
		idleThresh: 5 * time.Minute,
	}

	if p.shouldProbe(1, "https://a.com") {
		t.Error("should skip recently used URL")
	}
}

func TestURLProber_ProbesIdleURLs(t *testing.T) {
	sel := NewURLSelector()
	// 无任何记录的URL -> 应该探测

	p := &URLProber{
		selector:   sel,
		idleThresh: 5 * time.Minute,
	}

	if !p.shouldProbe(1, "https://a.com") {
		t.Error("should probe URL with no history")
	}
}
