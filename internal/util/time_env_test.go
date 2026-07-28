package util

import (
	"testing"
	"time"
)

func TestNewCooldownPolicyUsesOverridesAndDefaults(t *testing.T) {
	policy := NewCooldownPolicy(CooldownSettings{
		AuthSec:      7,
		TimeoutSec:   0,
		ServerSec:    9,
		RateLimitSec: -1,
		MaxSec:       1800,
		MinSec:       11,
	})

	now := time.Now()
	auth, server, timeout, rateLimit := 401, 500, StatusSSEError, 429
	assertDuration := func(name string, got, want time.Duration) {
		t.Helper()
		if got != want {
			t.Fatalf("%s=%v, want %v", name, got, want)
		}
	}
	assertDuration("auth", policy.CalculateBackoffDuration(0, time.Time{}, now, &auth), 7*time.Second)
	assertDuration("server", policy.CalculateBackoffDuration(0, time.Time{}, now, &server), 9*time.Second)
	assertDuration("timeout default", policy.CalculateBackoffDuration(0, time.Time{}, now, &timeout), TimeoutErrorCooldown)
	assertDuration("rate limit default", policy.CalculateBackoffDuration(0, time.Time{}, now, &rateLimit), RateLimitErrorCooldown)
	assertDuration("minimum", policy.CalculateBackoffDuration(1000, time.Time{}, now, &server), 11*time.Second)
	assertDuration("maximum", policy.CalculateBackoffDuration(int64(20*time.Minute/time.Millisecond), time.Time{}, now, &server), 30*time.Minute)
}
