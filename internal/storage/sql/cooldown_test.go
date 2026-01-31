package sql_test

import (
	"context"
	"path/filepath"
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

	// BumpChannelCooldown：触发第一次冷却（500错误，初始1秒起步）
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

	// BumpKeyCooldown：触发第一次冷却（429错误，初始1秒）
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

func TestCooldown_AuthErrorBackoff(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "auth_backoff.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "test-auth-backoff")
	createTestAPIKey(t, ctx, store, channelID, 0)

	// 401/403 错误应该从 5 分钟起步（而不是 1 秒）
	now := time.Now()
	duration, err := store.BumpKeyCooldown(ctx, channelID, 0, now, 401)
	if err != nil {
		t.Fatalf("bump key cooldown with 401: %v", err)
	}

	// 认证错误应该是较长的冷却时间
	if duration < 5*time.Minute {
		t.Errorf("expected auth error to have longer cooldown (>=5m), got %v", duration)
	}
}
