package app

import (
	"sync"
	"testing"
	"time"
)

type channelRPMFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *channelRPMFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *channelRPMFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestChannelRPMLimiterZeroLimitIsUnlimited(t *testing.T) {
	clock := &channelRPMFakeClock{now: time.Unix(1000, 0)}
	limiter := newChannelRPMLimiter(clock.Now)

	for i := 0; i < 1000; i++ {
		if !limiter.allow(42, 0) {
			t.Fatalf("request %d rejected for zero RPM limit", i+1)
		}
	}
}

func TestChannelRPMLimiterRejectsAfterLimitWithinRollingMinute(t *testing.T) {
	clock := &channelRPMFakeClock{now: time.Unix(1000, 0)}
	limiter := newChannelRPMLimiter(clock.Now)

	if !limiter.allow(7, 2) {
		t.Fatal("first request rejected")
	}
	if !limiter.allow(7, 2) {
		t.Fatal("second request rejected")
	}
	if limiter.allow(7, 2) {
		t.Fatal("third request allowed within the same minute")
	}

	clock.Advance(59 * time.Second)
	if limiter.allow(7, 2) {
		t.Fatal("request allowed before the rolling minute expired")
	}

	clock.Advance(time.Second)
	if !limiter.allow(7, 2) {
		t.Fatal("request rejected after the rolling minute expired")
	}
}

func TestChannelRPMLimiterReportsRetryAfter(t *testing.T) {
	clock := &channelRPMFakeClock{now: time.Unix(1000, 0)}
	limiter := newChannelRPMLimiter(clock.Now)

	if result := limiter.reserve(7, 2); !result.allowed {
		t.Fatalf("first request rejected: %+v", result)
	}
	if result := limiter.reserve(7, 2); !result.allowed {
		t.Fatalf("second request rejected: %+v", result)
	}

	result := limiter.reserve(7, 2)
	if result.allowed {
		t.Fatal("third request allowed within the rolling minute")
	}
	if result.retryAfter != time.Minute {
		t.Fatalf("retryAfter=%v, want %v", result.retryAfter, time.Minute)
	}

	clock.Advance(59 * time.Second)
	result = limiter.reserve(7, 2)
	if result.allowed {
		t.Fatal("request allowed before rolling minute expired")
	}
	if result.retryAfter != time.Second {
		t.Fatalf("retryAfter=%v, want %v", result.retryAfter, time.Second)
	}
}
