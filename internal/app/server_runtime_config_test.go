package app

import (
	"testing"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// newStubConfigService 构造一个只有内存缓存的 ConfigService（不触库）。
func newStubConfigService(values map[string]string) *ConfigService {
	cs := NewConfigService(storage.Store(nil))
	cs.mu.Lock()
	for key, value := range values {
		cs.cache[key] = &model.SystemSetting{Key: key, Value: value}
	}
	cs.loaded = true
	cs.mu.Unlock()
	return cs
}

func TestLoadServerRuntimeConfigLimitsAndCooldown(t *testing.T) {
	t.Parallel()

	cfg := loadServerRuntimeConfig(newStubConfigService(map[string]string{
		"max_concurrency":             "250",
		"max_body_bytes":              "2048",
		"max_image_body_bytes":        "4096",
		"cooldown_auth_seconds":       "77",
		"cooldown_server_seconds":     "88",
		"cooldown_timeout_seconds":    "99",
		"cooldown_rate_limit_seconds": "111",
		"cooldown_min_seconds":        "5",
		"cooldown_max_seconds":        "600",
	}))

	if cfg.MaxConcurrency != 250 {
		t.Fatalf("MaxConcurrency=%d, want 250", cfg.MaxConcurrency)
	}
	if cfg.MaxBodyBytes != 2048 || cfg.MaxImageBodyBytes != 4096 {
		t.Fatalf("body limits=%d/%d, want 2048/4096", cfg.MaxBodyBytes, cfg.MaxImageBodyBytes)
	}
	want := map[string]int{
		"auth": 77, "server": 88, "timeout": 99, "rate": 111, "min": 5, "max": 600,
	}
	got := map[string]int{
		"auth": cfg.Cooldown.AuthSec, "server": cfg.Cooldown.ServerSec,
		"timeout": cfg.Cooldown.TimeoutSec, "rate": cfg.Cooldown.RateLimitSec,
		"min": cfg.Cooldown.MinSec, "max": cfg.Cooldown.MaxSec,
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Fatalf("cooldown %s=%d, want %d", key, got[key], wantVal)
		}
	}
}

func TestLoadServerRuntimeConfigFallsBackOnInvalidValues(t *testing.T) {
	t.Parallel()

	cfg := loadServerRuntimeConfig(newStubConfigService(map[string]string{
		"max_concurrency":       "0",
		"max_body_bytes":        "-1",
		"max_image_body_bytes":  "abc",
		"cooldown_auth_seconds": "0",
	}))

	if cfg.MaxConcurrency != config.DefaultMaxConcurrency {
		t.Fatalf("MaxConcurrency=%d, want default %d", cfg.MaxConcurrency, config.DefaultMaxConcurrency)
	}
	if cfg.MaxBodyBytes != config.DefaultMaxBodyBytes {
		t.Fatalf("MaxBodyBytes=%d, want default %d", cfg.MaxBodyBytes, config.DefaultMaxBodyBytes)
	}
	if cfg.MaxImageBodyBytes != config.DefaultMaxImageBodyBytes {
		t.Fatalf("MaxImageBodyBytes=%d, want default %d", cfg.MaxImageBodyBytes, config.DefaultMaxImageBodyBytes)
	}
	if cfg.Cooldown.AuthSec != config.DefaultCooldownAuthSeconds {
		t.Fatalf("Cooldown.AuthSec=%d, want default %d", cfg.Cooldown.AuthSec, config.DefaultCooldownAuthSeconds)
	}
}

// 上下限倒挂会让指数退避被 max 钳在下限之下，语义不可用，必须整对回退默认值。
func TestLoadServerRuntimeConfigRejectsInvertedCooldownBounds(t *testing.T) {
	t.Parallel()

	cfg := loadServerRuntimeConfig(newStubConfigService(map[string]string{
		"cooldown_min_seconds": "900",
		"cooldown_max_seconds": "60",
	}))

	if cfg.Cooldown.MinSec != config.DefaultCooldownMinSeconds ||
		cfg.Cooldown.MaxSec != config.DefaultCooldownMaxSeconds {
		t.Fatalf("cooldown bounds=%d/%d, want defaults %d/%d",
			cfg.Cooldown.MinSec, cfg.Cooldown.MaxSec,
			config.DefaultCooldownMinSeconds, config.DefaultCooldownMaxSeconds)
	}
}

// Images 路径必须走独立上限：旧的 CCLOAD_MAX_BODY_BYTES 会把 20MB 特例一起吃掉。
func TestMaxProxyBodyBytesUsesSeparateImageLimit(t *testing.T) {
	prevBody := maxBodyBytesLimit.Load()
	prevImage := maxImageBodyBytesLimit.Load()
	t.Cleanup(func() {
		maxBodyBytesLimit.Store(prevBody)
		maxImageBodyBytesLimit.Store(prevImage)
	})

	setMaxBodyBytesLimits(1024, 8192)
	if got := maxProxyBodyBytes("/v1/messages"); got != 1024 {
		t.Fatalf("maxProxyBodyBytes(/v1/messages)=%d, want 1024", got)
	}
	if got := maxProxyBodyBytes("/v1/images/generations"); got != 8192 {
		t.Fatalf("maxProxyBodyBytes(/v1/images/generations)=%d, want 8192", got)
	}

	setMaxBodyBytesLimits(0, -5)
	if got := maxProxyBodyBytes("/v1/messages"); got != int64(config.DefaultMaxBodyBytes) {
		t.Fatalf("non-positive limit=%d, want default %d", got, config.DefaultMaxBodyBytes)
	}
	if got := maxProxyBodyBytes("/v1/images/edits"); got != int64(config.DefaultMaxImageBodyBytes) {
		t.Fatalf("non-positive image limit=%d, want default %d", got, config.DefaultMaxImageBodyBytes)
	}
}
