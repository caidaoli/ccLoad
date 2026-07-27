package util

import (
	"testing"
	"time"
)

func TestApplyCooldownSettings(t *testing.T) {
	origAuth := AuthErrorInitialCooldown
	origTimeout := TimeoutErrorCooldown
	origServer := ServerErrorInitialCooldown
	origRateLimit := RateLimitErrorCooldown
	origMax := MaxCooldownDuration
	origMin := MinCooldownDuration
	t.Cleanup(func() {
		AuthErrorInitialCooldown = origAuth
		TimeoutErrorCooldown = origTimeout
		ServerErrorInitialCooldown = origServer
		RateLimitErrorCooldown = origRateLimit
		MaxCooldownDuration = origMax
		MinCooldownDuration = origMin
	})

	// 先重置到一组可预测值，避免受其他用例影响
	AuthErrorInitialCooldown = 5 * time.Minute
	TimeoutErrorCooldown = 1 * time.Minute
	ServerErrorInitialCooldown = 2 * time.Minute
	RateLimitErrorCooldown = 1 * time.Minute
	MaxCooldownDuration = 30 * time.Minute
	MinCooldownDuration = 10 * time.Second

	// 非正值（0/负数）表示"未配置"，必须保留原值而非清零
	ApplyCooldownSettings(CooldownSettings{
		AuthSec:      7,
		TimeoutSec:   0,
		ServerSec:    9,
		RateLimitSec: -1,
		MaxSec:       1800,
		MinSec:       11,
	})

	if AuthErrorInitialCooldown != 7*time.Second {
		t.Fatalf("AuthErrorInitialCooldown=%v, want %v", AuthErrorInitialCooldown, 7*time.Second)
	}
	if TimeoutErrorCooldown != 1*time.Minute {
		t.Fatalf("TimeoutErrorCooldown=%v, want unchanged %v", TimeoutErrorCooldown, 1*time.Minute)
	}
	if ServerErrorInitialCooldown != 9*time.Second {
		t.Fatalf("ServerErrorInitialCooldown=%v, want %v", ServerErrorInitialCooldown, 9*time.Second)
	}
	if RateLimitErrorCooldown != 1*time.Minute {
		t.Fatalf("RateLimitErrorCooldown=%v, want unchanged %v", RateLimitErrorCooldown, 1*time.Minute)
	}
	if MaxCooldownDuration != 1800*time.Second {
		t.Fatalf("MaxCooldownDuration=%v, want %v", MaxCooldownDuration, 1800*time.Second)
	}
	if MinCooldownDuration != 11*time.Second {
		t.Fatalf("MinCooldownDuration=%v, want %v", MinCooldownDuration, 11*time.Second)
	}
}
