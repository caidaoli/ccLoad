package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestHealthCache_Defaults(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	var wg sync.WaitGroup
	var isShuttingDown atomic.Bool
	stopCh := make(chan struct{})

	h := NewHealthCache(store, model.HealthScoreConfig{Enabled: false}, stopCh, &isShuttingDown, &wg)

	// 未命中默认 100% 成功率（新渠道不惩罚）
	if got := h.GetSuccessRate(123); got != 1.0 {
		t.Fatalf("GetSuccessRate=%v, want 1.0", got)
	}
	if got := h.GetAllSuccessRates(); len(got) != 0 {
		t.Fatalf("GetAllSuccessRates len=%d, want 0", len(got))
	}

	// 仅用于确保未使用变量（server 在此测试无用，但 helper 返回了它）
	_ = server
}

func TestHealthCache_UpdateAndLoop(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "hc",
		URL:          "https://example.com",
		Priority:     1,
		Enabled:      true,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now()
	// 1 成功 + 1 失败（纳入健康度统计口径的 500）
	_ = store.AddLog(ctx, &model.LogEntry{Time: model.JSONTime{Time: now}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 200, Duration: 0.1})
	_ = store.AddLog(ctx, &model.LogEntry{Time: model.JSONTime{Time: now}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 500, Duration: 0.1})

	var wg sync.WaitGroup
	var isShuttingDown atomic.Bool
	stopCh := make(chan struct{})

	h := NewHealthCache(store, model.HealthScoreConfig{
		Enabled:               true,
		WindowMinutes:         60,
		UpdateIntervalSeconds: 1,
	}, stopCh, &isShuttingDown, &wg)

	// 直接调用 update：覆盖更新逻辑且避免 ticker 的不确定性
	h.update()

	stats := h.GetHealthStats(cfg.ID)
	if stats.SampleCount != 2 {
		t.Fatalf("SampleCount=%d, want 2", stats.SampleCount)
	}
	if stats.SuccessRate < 0.49 || stats.SuccessRate > 0.51 {
		t.Fatalf("SuccessRate=%v, want ~0.5", stats.SuccessRate)
	}

	// Start + stop 覆盖 updateLoop 主路径
	h.Start()
	close(stopCh)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("health cache goroutine did not stop")
	}
}

func TestHealthCache_StartSkipsWhenInvalidOrDisabled(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	t.Run("disabled", func(t *testing.T) {
		var wg sync.WaitGroup
		var isShuttingDown atomic.Bool
		stopCh := make(chan struct{})

		h := NewHealthCache(store, model.HealthScoreConfig{Enabled: false}, stopCh, &isShuttingDown, &wg)
		h.Start()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("expected Start() to not spawn goroutine when disabled")
		}
	})

	t.Run("invalid config", func(t *testing.T) {
		var wg sync.WaitGroup
		var isShuttingDown atomic.Bool
		stopCh := make(chan struct{})

		h := NewHealthCache(store, model.HealthScoreConfig{
			Enabled:               true,
			WindowMinutes:         0,
			UpdateIntervalSeconds: 0,
		}, stopCh, &isShuttingDown, &wg)
		h.Start()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("expected Start() to not spawn goroutine on invalid config")
		}
	})
}
