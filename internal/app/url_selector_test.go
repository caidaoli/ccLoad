package app

import (
	"testing"
	"time"
)

func TestURLSelector_SingleURL(t *testing.T) {
	sel := NewURLSelector()
	url, idx := sel.SelectURL(1, []string{"https://a.com"})
	if url != "https://a.com" || idx != 0 {
		t.Errorf("single URL: expected (https://a.com, 0), got (%s, %d)", url, idx)
	}
}

func TestURLSelector_ColdStart_ReturnsFirst(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com", "https://c.com"}
	url, idx := sel.SelectURL(1, urls)
	if url != "https://a.com" || idx != 0 {
		t.Errorf("cold start: expected first URL, got (%s, %d)", url, idx)
	}
}

func TestURLSelector_SelectsFastest(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://slow.com", "https://fast.com"}
	// 记录延迟: slow=500ms, fast=100ms
	sel.RecordLatency(1, "https://slow.com", 500*time.Millisecond)
	sel.RecordLatency(1, "https://fast.com", 100*time.Millisecond)

	url, _ := sel.SelectURL(1, urls)
	if url != "https://fast.com" {
		t.Errorf("expected fastest URL https://fast.com, got %s", url)
	}
}

func TestURLSelector_SkipsCooledDown(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}
	sel.RecordLatency(1, "https://a.com", 50*time.Millisecond) // a更快
	sel.RecordLatency(1, "https://b.com", 200*time.Millisecond)
	sel.CooldownURL(1, "https://a.com") // 但a被冷却

	url, _ := sel.SelectURL(1, urls)
	if url != "https://b.com" {
		t.Errorf("expected non-cooled URL https://b.com, got %s", url)
	}
}

func TestURLSelector_AllCooledDown_ReturnsBest(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}
	sel.CooldownURL(1, "https://a.com")
	sel.CooldownURL(1, "https://b.com")

	// 所有URL都冷却时，仍然返回第一个（兜底）
	url, _ := sel.SelectURL(1, urls)
	if url == "" {
		t.Error("all cooled: should still return a URL as fallback")
	}
}

func TestURLSelector_CooldownExpires(t *testing.T) {
	sel := NewURLSelector()
	sel.cooldownBase = 10 * time.Millisecond // 测试用短冷却
	urls := []string{"https://a.com", "https://b.com"}
	sel.RecordLatency(1, "https://a.com", 50*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 200*time.Millisecond)
	sel.CooldownURL(1, "https://a.com")

	// 冷却期间选b
	url, _ := sel.SelectURL(1, urls)
	if url != "https://b.com" {
		t.Errorf("during cooldown: expected b, got %s", url)
	}

	// 等待冷却过期
	time.Sleep(15 * time.Millisecond)
	url, _ = sel.SelectURL(1, urls)
	if url != "https://a.com" {
		t.Errorf("after cooldown: expected a (fastest), got %s", url)
	}
}

func TestURLSelector_IndependentChannels(t *testing.T) {
	sel := NewURLSelector()
	sel.RecordLatency(1, "https://a.com", 500*time.Millisecond)
	sel.RecordLatency(2, "https://a.com", 50*time.Millisecond)

	// 渠道1的延迟不影响渠道2
	url, _ := sel.SelectURL(2, []string{"https://a.com", "https://b.com"})
	if url != "https://a.com" {
		t.Errorf("channel isolation: expected a.com for channel 2, got %s", url)
	}
}

func TestURLSelector_ExponentialBackoff(t *testing.T) {
	sel := NewURLSelector()
	sel.cooldownBase = 10 * time.Millisecond

	key := urlKey{channelID: 1, url: "https://a.com"}

	// 第1次冷却: 10ms
	sel.CooldownURL(1, "https://a.com")
	state1 := sel.cooldowns[key]
	if state1.consecutiveFails != 1 {
		t.Errorf("expected 1 fail, got %d", state1.consecutiveFails)
	}

	// 等待冷却过期后再次冷却: 20ms
	time.Sleep(15 * time.Millisecond)
	sel.CooldownURL(1, "https://a.com")
	state2 := sel.cooldowns[key]
	if state2.consecutiveFails != 2 {
		t.Errorf("expected 2 fails, got %d", state2.consecutiveFails)
	}
}
