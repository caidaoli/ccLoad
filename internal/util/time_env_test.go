package util

import (
	"testing"
	"time"
)

func TestEnvSecondsFrom(t *testing.T) {
	t.Parallel()

	getenv := func(k string) string {
		switch k {
		case "OK":
			return "12"
		case "BAD":
			return "x"
		case "ZERO":
			return "0"
		case "NEG":
			return "-1"
		default:
			return ""
		}
	}

	if got := envSecondsFrom(getenv, "MISSING"); got != 0 {
		t.Fatalf("missing: got %v, want 0", got)
	}
	if got := envSecondsFrom(getenv, "BAD"); got != 0 {
		t.Fatalf("bad: got %v, want 0", got)
	}
	if got := envSecondsFrom(getenv, "ZERO"); got != 0 {
		t.Fatalf("zero: got %v, want 0", got)
	}
	if got := envSecondsFrom(getenv, "NEG"); got != 0 {
		t.Fatalf("neg: got %v, want 0", got)
	}
	if got := envSecondsFrom(getenv, "OK"); got != 12*time.Second {
		t.Fatalf("ok: got %v, want %v", got, 12*time.Second)
	}
}

func TestApplyCooldownEnvOverrides(t *testing.T) {
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

	// 先重置到一组可预测值，避免受 init() 的环境变量影响
	AuthErrorInitialCooldown = 5 * time.Minute
	TimeoutErrorCooldown = 1 * time.Minute
	ServerErrorInitialCooldown = 2 * time.Minute
	RateLimitErrorCooldown = 1 * time.Minute
	MaxCooldownDuration = 30 * time.Minute
	MinCooldownDuration = 10 * time.Second

	getenv := func(k string) string {
		switch k {
		case "CCLOAD_COOLDOWN_AUTH_SEC":
			return "7"
		case "CCLOAD_COOLDOWN_TIMEOUT_SEC":
			return ""
		case "CCLOAD_COOLDOWN_SERVER_SEC":
			return "9"
		case "CCLOAD_COOLDOWN_RATE_LIMIT_SEC":
			return "x"
		case "CCLOAD_COOLDOWN_MAX_SEC":
			return "1800"
		case "CCLOAD_COOLDOWN_MIN_SEC":
			return "11"
		default:
			return ""
		}
	}

	applyCooldownEnvOverrides(getenv)

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
