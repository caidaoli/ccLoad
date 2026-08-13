package sql_test

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/storage"
)

func TestCooldown_ChannelCooldown(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "cooldown.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "test-channel-cooldown")

	// 初始状态：无冷却
	cooldowns, err := store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("get all channel cooldowns: %v", err)
	}
	if _, exists := cooldowns[channelID]; exists {
		t.Error("expected no cooldown initially")
	}

	// BumpChannelCooldown：触发第一次冷却（500 错误默认 2 分钟起步）
	now := time.Now()
	duration, err := store.BumpChannelCooldown(ctx, channelID, now, 500)
	if err != nil {
		t.Fatalf("bump channel cooldown: %v", err)
	}
	if duration < time.Second {
		t.Errorf("expected duration >= 1s, got %v", duration)
	}

	// 验证冷却已设置
	cooldowns, err = store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("get all channel cooldowns: %v", err)
	}
	until, exists := cooldowns[channelID]
	if !exists {
		t.Error("expected cooldown to be set")
	}
	if until.Before(now) {
		t.Errorf("cooldown until should be in future: got %v, now %v", until, now)
	}

	// BumpChannelCooldown：第二次触发，应指数退避
	duration2, err := store.BumpChannelCooldown(ctx, channelID, now, 500)
	if err != nil {
		t.Fatalf("bump channel cooldown second time: %v", err)
	}
	if duration2 <= duration {
		t.Errorf("expected exponential backoff: first=%v, second=%v", duration, duration2)
	}

	// SetChannelCooldown：手动设置冷却
	futureTime := time.Now().Add(10 * time.Minute)
	if err := store.SetChannelCooldown(ctx, channelID, futureTime); err != nil {
		t.Fatalf("set channel cooldown: %v", err)
	}
	cooldowns, err = store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("get all channel cooldowns after set: %v", err)
	}
	// 验证设置成功（允许1秒误差）
	if cooldowns[channelID].Sub(futureTime).Abs() > time.Second {
		t.Errorf("expected cooldown until ~%v, got %v", futureTime, cooldowns[channelID])
	}

	// ResetChannelCooldown：重置冷却
	if err := store.ResetChannelCooldown(ctx, channelID); err != nil {
		t.Fatalf("reset channel cooldown: %v", err)
	}
	cooldowns, err = store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("get all channel cooldowns after reset: %v", err)
	}
	if _, exists := cooldowns[channelID]; exists {
		t.Error("expected cooldown to be cleared after reset")
	}
}

func TestCooldown_KeyCooldown(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "key_cooldown.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "test-key-cooldown")
	createTestAPIKey(t, ctx, store, channelID, 0)

	// 初始状态：无冷却
	allKeyCooldowns, err := store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("get all key cooldowns: %v", err)
	}
	if len(allKeyCooldowns) > 0 {
		t.Errorf("expected no key cooldowns initially, got %d", len(allKeyCooldowns))
	}

	// BumpKeyCooldown：触发第一次冷却（429 错误默认 1 分钟）
	now := time.Now()
	duration, err := store.BumpKeyCooldown(ctx, channelID, 0, now, 429)
	if err != nil {
		t.Fatalf("bump key cooldown: %v", err)
	}
	if duration < time.Second {
		t.Errorf("expected duration >= 1s, got %v", duration)
	}

	// 验证冷却已设置
	allKeyCooldowns, err = store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("get all key cooldowns after bump: %v", err)
	}
	if allKeyCooldowns[channelID] == nil {
		t.Error("expected channel in cooldowns map")
	} else if until, exists := allKeyCooldowns[channelID][0]; !exists {
		t.Error("expected key 0 cooldown to be set")
	} else if until.Before(now) {
		t.Errorf("cooldown until should be in future: got %v, now %v", until, now)
	}

	// BumpKeyCooldown：第二次触发，应指数退避
	duration2, err := store.BumpKeyCooldown(ctx, channelID, 0, now, 429)
	if err != nil {
		t.Fatalf("bump key cooldown second time: %v", err)
	}
	if duration2 <= duration {
		t.Errorf("expected exponential backoff: first=%v, second=%v", duration, duration2)
	}

	// SetKeyCooldown：手动设置冷却
	futureTime := time.Now().Add(5 * time.Minute)
	if err := store.SetKeyCooldown(ctx, channelID, 0, futureTime); err != nil {
		t.Fatalf("set key cooldown: %v", err)
	}
	allKeyCooldowns, err = store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("get all key cooldowns after set: %v", err)
	}
	if until, exists := allKeyCooldowns[channelID][0]; !exists {
		t.Error("expected cooldown after set")
	} else if until.Sub(futureTime).Abs() > time.Second {
		t.Errorf("expected cooldown until ~%v, got %v", futureTime, until)
	}

	// ResetKeyCooldown：重置单个 key 冷却
	if err := store.ResetKeyCooldown(ctx, channelID, 0); err != nil {
		t.Fatalf("reset key cooldown: %v", err)
	}
	allKeyCooldowns, err = store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("get all key cooldowns after reset: %v", err)
	}
	if len(allKeyCooldowns[channelID]) > 0 {
		t.Error("expected cooldown to be cleared after reset")
	}
}

func TestCooldown_ModelCooldown(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "model_cooldown.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "test-model-cooldown")
	now := time.Now()

	duration, err := store.BumpModelCooldown(ctx, channelID, "upstream-model-a", now, 502)
	if err != nil {
		t.Fatalf("bump model cooldown: %v", err)
	}
	if duration != 2*time.Minute {
		t.Fatalf("first model cooldown duration=%v, want %v", duration, 2*time.Minute)
	}

	// 即使上一轮已经过期，失败历史仍应继续退避，直到成功或显式重置。
	duration, err = store.BumpModelCooldown(ctx, channelID, "upstream-model-a", now.Add(3*time.Minute), 503)
	if err != nil {
		t.Fatalf("bump model cooldown second time: %v", err)
	}
	if duration != 4*time.Minute {
		t.Fatalf("second model cooldown duration=%v, want %v", duration, 4*time.Minute)
	}

	if err := store.ResetModelCooldown(ctx, channelID, "upstream-model-a"); err != nil {
		t.Fatalf("reset bumped model cooldown: %v", err)
	}
	duration, err = store.BumpModelCooldown(ctx, channelID, "upstream-model-a", now.Add(8*time.Minute), 502)
	if err != nil {
		t.Fatalf("bump reset model cooldown: %v", err)
	}
	if duration != 2*time.Minute {
		t.Fatalf("reset model cooldown duration=%v, want %v", duration, 2*time.Minute)
	}
	if err := store.ResetModelCooldown(ctx, channelID, "upstream-model-a"); err != nil {
		t.Fatalf("reset model cooldown before fixed deadline: %v", err)
	}
	until := time.Now().Add(10 * time.Minute)

	if err := store.SetModelCooldown(ctx, channelID, "upstream-model-a", until); err != nil {
		t.Fatalf("set model cooldown: %v", err)
	}
	cooldowns, err := store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("get model cooldowns: %v", err)
	}
	got, exists := cooldowns[channelID]["upstream-model-a"]
	if !exists {
		t.Fatal("expected model cooldown")
	}
	if got.Sub(until).Abs() > time.Second {
		t.Fatalf("model cooldown until=%v, want %v", got, until)
	}

	if err := store.ResetModelCooldown(ctx, channelID, "upstream-model-a"); err != nil {
		t.Fatalf("reset model cooldown: %v", err)
	}
	cooldowns, err = store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("get model cooldowns after reset: %v", err)
	}
	if _, exists := cooldowns[channelID]["upstream-model-a"]; exists {
		t.Fatal("expected model cooldown to be cleared")
	}

	if err := store.SetModelCooldown(ctx, channelID, "  ", until); err == nil {
		t.Fatal("expected empty model to be rejected")
	}

	if err := store.SetModelCooldown(ctx, channelID, "upstream-model-b", until); err != nil {
		t.Fatalf("set model cooldown before channel delete: %v", err)
	}
	if err := store.DeleteConfig(ctx, channelID); err != nil {
		t.Fatalf("delete channel: %v", err)
	}
	cooldowns, err = store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("get model cooldowns after channel delete: %v", err)
	}
	if _, exists := cooldowns[channelID]; exists {
		t.Fatal("expected channel deletion to remove model cooldowns")
	}
}

func TestCooldown_ConcurrentModelCooldown(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(filepath.Join(t.TempDir(), "concurrent_model_cooldown.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "test-concurrent-model-cooldown")
	now := time.Now()
	start := make(chan struct{})
	durations := make(chan time.Duration, 2)
	errs := make(chan error, 2)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			duration, err := store.BumpModelCooldown(ctx, channelID, "upstream-model", now, 502)
			if err != nil {
				errs <- err
				return
			}
			durations <- duration
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(durations)

	for err := range errs {
		t.Fatalf("concurrent model cooldown: %v", err)
	}
	got := make([]time.Duration, 0, 2)
	for duration := range durations {
		got = append(got, duration)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	want := []time.Duration{2 * time.Minute, 4 * time.Minute}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("concurrent model cooldown durations=%v, want %v", got, want)
	}
}

func TestCooldown_BumpChannelCooldown_NotFound(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "notfound.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	// 对不存在的渠道触发冷却应返回错误
	_, err = store.BumpChannelCooldown(ctx, 99999, time.Now(), 500)
	if err == nil {
		t.Error("expected error for non-existent channel")
	}
}

func TestCooldown_BumpKeyCooldown_NotFound(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "key_notfound.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	// 对不存在的 key 触发冷却应返回错误
	_, err = store.BumpKeyCooldown(ctx, 99999, 0, time.Now(), 429)
	if err == nil {
		t.Error("expected error for non-existent key")
	}
}
