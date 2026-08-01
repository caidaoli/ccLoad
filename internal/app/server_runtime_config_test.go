package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestLoadServerRuntimeConfigUpstreamConnectionMaxAge(t *testing.T) {
	t.Parallel()

	cfg := loadServerRuntimeConfig(newStubConfigService(map[string]string{
		"upstream_connection_reuse_limit_seconds": "540",
	}))

	if cfg.UpstreamConnectionMaxAge != 9*time.Minute {
		t.Fatalf("UpstreamConnectionMaxAge=%v, want 9m", cfg.UpstreamConnectionMaxAge)
	}

	disabled := loadServerRuntimeConfig(newStubConfigService(map[string]string{
		"upstream_connection_reuse_limit_seconds": "0",
	}))
	if disabled.UpstreamConnectionMaxAge != 0 {
		t.Fatalf("disabled UpstreamConnectionMaxAge=%v, want 0", disabled.UpstreamConnectionMaxAge)
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
	limits := newRequestBodyLimits(1024, 8192)
	if got := limits.maxForPath("/v1/messages"); got != 1024 {
		t.Fatalf("maxProxyBodyBytes(/v1/messages)=%d, want 1024", got)
	}
	if got := limits.maxForPath("/v1/images/generations"); got != 8192 {
		t.Fatalf("maxProxyBodyBytes(/v1/images/generations)=%d, want 8192", got)
	}

	limits = newRequestBodyLimits(0, -5)
	if got := limits.maxForPath("/v1/messages"); got != int64(config.DefaultMaxBodyBytes) {
		t.Fatalf("non-positive limit=%d, want default %d", got, config.DefaultMaxBodyBytes)
	}
	if got := limits.maxForPath("/v1/images/edits"); got != int64(config.DefaultMaxImageBodyBytes) {
		t.Fatalf("non-positive image limit=%d, want default %d", got, config.DefaultMaxImageBodyBytes)
	}
}

func TestNewServerParallelConstructionKeepsRuntimeConfigIsolated(t *testing.T) {
	t.Setenv("CCLOAD_PASS", "parallel-construction-test-password")

	type serverCase struct {
		bodyLimit int
		authDelay int
		store     storage.Store
		server    *Server
	}
	cases := []serverCase{
		{bodyLimit: 128, authDelay: 7},
		{bodyLimit: 1024, authDelay: 11},
	}

	ctx := context.Background()
	for i := range cases {
		store, err := storage.CreateSQLiteStore(":memory:")
		if err != nil {
			t.Fatalf("CreateSQLiteStore case %d: %v", i, err)
		}
		cases[i].store = store
		if err = store.BatchUpdateSettings(ctx, map[string]string{
			"max_body_bytes":        fmt.Sprint(cases[i].bodyLimit),
			"cooldown_auth_seconds": fmt.Sprint(cases[i].authDelay),
		}); err != nil {
			t.Fatalf("BatchUpdateSettings case %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for i := range cases {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			cases[index].server = NewServer(cases[index].store)
		}(i)
	}
	wg.Wait()
	t.Cleanup(func() {
		for i := range cases {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := cases[i].server.Shutdown(shutdownCtx); err != nil {
				t.Errorf("Shutdown case %d: %v", i, err)
			}
			cancel()
		}
	})

	body := []byte(`{"model":"missing","messages":[{"role":"user","content":"` + strings.Repeat("x", 256) + `"}]}`)
	for i := range cases {
		req := newRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ginCtx, recorder := newTestContext(t, req)
		cases[i].server.HandleProxyRequest(ginCtx)
		if cases[i].bodyLimit < len(body) {
			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("case %d status=%d, want %d for %d-byte body with %d-byte limit",
					i, recorder.Code, http.StatusRequestEntityTooLarge, len(body), cases[i].bodyLimit)
			}
		} else if recorder.Code == http.StatusRequestEntityTooLarge {
			t.Fatalf("case %d unexpectedly rejected %d-byte body with %d-byte limit",
				i, len(body), cases[i].bodyLimit)
		}

		channel, err := cases[i].store.CreateConfig(ctx, &model.Config{
			Name: "parallel-runtime-config", URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateConfig case %d: %v", i, err)
		}
		duration, err := cases[i].store.BumpChannelCooldown(ctx, channel.ID, time.Now(), http.StatusUnauthorized)
		if err != nil {
			t.Fatalf("BumpChannelCooldown case %d: %v", i, err)
		}
		if want := time.Duration(cases[i].authDelay) * time.Second; duration != want {
			t.Fatalf("case %d auth cooldown=%v, want %v", i, duration, want)
		}
	}
}
