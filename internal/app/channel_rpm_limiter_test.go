package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/model"
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

func TestChannelRPMLimiterRemoveChannelClearsRequests(t *testing.T) {
	clock := &channelRPMFakeClock{now: time.Unix(1000, 0)}
	limiter := newChannelRPMLimiter(clock.Now)

	if !limiter.allow(7, 1) {
		t.Fatal("first request rejected")
	}
	if limiter.allow(7, 1) {
		t.Fatal("second request allowed before removal")
	}

	limiter.RemoveChannel(7)
	if !limiter.allow(7, 1) {
		t.Fatal("request rejected after channel RPM state removal")
	}
}

func TestChannelRPMLimiterCleanupExpiredRemovesEmptyChannels(t *testing.T) {
	clock := &channelRPMFakeClock{now: time.Unix(1000, 0)}
	limiter := newChannelRPMLimiter(clock.Now)

	if !limiter.allow(7, 1) {
		t.Fatal("first request rejected")
	}

	clock.Advance(time.Minute + time.Second)
	limiter.CleanupExpired()

	limiter.mu.Lock()
	_, exists := limiter.requests[7]
	limiter.mu.Unlock()
	if exists {
		t.Fatal("expired channel RPM state was not removed")
	}
}

func TestDeleteChannelByIDRemovesChannelRPMState(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	cfg, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:        "rpm-channel",
		ChannelType: "openai",
		URL:         "https://example.com",
		Priority:    1,
		Enabled:     true,
		RPMLimit:    1,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	if !srv.channelRPMLimiter.allow(cfg.ID, cfg.RPMLimit) {
		t.Fatal("first request rejected")
	}

	deleted, err := srv.deleteChannelByID(ctx, cfg.ID)
	if err != nil || !deleted {
		t.Fatalf("deleteChannelByID deleted=%v err=%v, want true,nil", deleted, err)
	}

	srv.channelRPMLimiter.mu.Lock()
	_, exists := srv.channelRPMLimiter.requests[cfg.ID]
	srv.channelRPMLimiter.mu.Unlock()
	if exists {
		t.Fatal("deleteChannelByID did not remove channel RPM state")
	}
}
