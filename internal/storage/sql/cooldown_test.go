package sql_test

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/model"
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

/**
 * @brief 验证仅匹配启用 OAuth 渠道的完整凭证原文才能写入冷却。
 * @param t 测试上下文。
 */
func TestCooldown_ChannelCooldownIfOAuthCredential(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	const credential = `{"access_token":"Current","refresh_token":"grant"}`
	channel, err := store.CreateConfig(ctx, &model.Config{
		Name: "oauth", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
		URLs: model.ChannelURLs{{URL: "https://example.test", Protocols: []string{"codex"}}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	for _, expected := range []string{`{"access_token":"current","refresh_token":"grant"}`, credential + " "} {
		if duration, err := store.CooldownChannelIfOAuthCredential(ctx, channel.ID, channel.AuthType, expected, now, 401, time.Hour); err != nil || duration != 0 {
			t.Fatalf("different credential cooldown=(%v, %v), want (0, nil)", duration, err)
		}
	}
	cooldowns, err := store.GetAllChannelCooldowns(ctx)
	if err != nil || !cooldowns[channel.ID].IsZero() {
		t.Fatalf("different credential wrote cooldown: %v, %v", cooldowns, err)
	}
	first, err := store.CooldownChannelIfOAuthCredential(ctx, channel.ID, channel.AuthType, credential, now, 500, 0)
	if err != nil || first <= 0 {
		t.Fatalf("first conditional backoff=(%v, %v)", first, err)
	}
	second, err := store.CooldownChannelIfOAuthCredential(ctx, channel.ID, channel.AuthType, credential, now, 500, 0)
	if err != nil || second <= first {
		t.Fatalf("second conditional backoff=(%v, %v), want >%v", second, err, first)
	}
	cooldowns, err = store.GetAllChannelCooldowns(ctx)
	if err != nil || !cooldowns[channel.ID].Equal(now.Add(second)) {
		t.Fatalf("conditional backoff not stored: %v, %v", cooldowns, err)
	}
}

/**
 * @brief 验证同轮 Accio 冷却只查询一次，确认窗口只校准该轮模型的到期时间。
 * @param t 测试上下文。
 */
func TestCooldown_AccioQuotaWindow(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()
	const credential = `{"type":"accio","region":"global","access_token":"old","user_id":"account"}`
	channel, err := store.CreateConfig(ctx, &model.Config{
		Name: "Accio", AuthType: model.AuthTypeAccioOAuth, OAuthCredential: credential,
		URLs: model.ChannelURLs{{URL: "https://example.test"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second).Add(time.Millisecond)
	provisional, probe, err := store.BeginAccioQuotaCooldown(ctx, channel.ID, credential, "first", now)
	if err != nil || !probe || !provisional.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("begin=(%v, %v, %v)", provisional, probe, err)
	}
	joined, probe, err := store.BeginAccioQuotaCooldown(ctx, channel.ID, credential, "second", now.Add(time.Second))
	if err != nil || probe || !joined.Equal(provisional) {
		t.Fatalf("join=(%v, %v, %v), want existing window %v and no probe", joined, probe, err, provisional)
	}
	if err := store.SetModelCooldown(ctx, channel.ID, "ordinary", provisional); err != nil {
		t.Fatal(err)
	}
	if until, probe, err := store.BeginAccioQuotaCooldown(ctx, channel.ID, credential, "replaced", now); err != nil || probe || !until.Equal(provisional) {
		t.Fatalf("begin model cooled again by ordinary failure: (%v, %v, %v)", until, probe, err)
	}
	if err := store.SetModelCooldown(ctx, channel.ID, "replaced", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if until, probe, err := store.BeginAccioQuotaCooldown(ctx, channel.ID, credential, "replaced", now); err != nil || probe || !until.IsZero() {
		t.Fatalf("preserve longer model cooldown: begin=(%v, %v, %v)", until, probe, err)
	}
	channels, err := store.GetAllChannelCooldowns(ctx)
	if err != nil || !channels[channel.ID].IsZero() {
		t.Fatalf("temporary model cooldown affected channel: %v, %v", channels, err)
	}
	resetAt := now.Add(3 * time.Minute).Truncate(time.Second)
	if ok, err := store.ConfirmAccioQuotaWindow(ctx, channel.ID, credential, "first", provisional, resetAt); err != nil || !ok {
		t.Fatalf("confirm=(%v, %v)", ok, err)
	}
	models, err := store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Time{
		"first": resetAt, "second": resetAt, model.AccioQuotaWindowModel: resetAt,
		"ordinary": provisional.Truncate(time.Second), "replaced": now.Add(time.Hour).Truncate(time.Second),
	} {
		if got := models[channel.ID][name]; !got.Equal(want) {
			t.Errorf("model %q cooldown=%v, want %v", name, got, want)
		}
	}
	channels, err = store.GetAllChannelCooldowns(ctx)
	if err != nil || !channels[channel.ID].Equal(resetAt) {
		t.Fatalf("confirmed channel cooldown=%v, want %v: %v", channels[channel.ID], resetAt, err)
	}
	if until, probe, err := store.BeginAccioQuotaCooldown(ctx, channel.ID, credential, "third", now); err != nil || probe || !until.IsZero() {
		t.Fatalf("begin in confirmed window=(%v, %v, %v)", until, probe, err)
	}
	if err := store.ResetAllCooldowns(ctx, channel.ID); err != nil {
		t.Fatal(err)
	}
	models, err = store.GetAllModelCooldowns(ctx)
	if err != nil || len(models[channel.ID]) != 0 {
		t.Fatalf("manual reset left model or account cooldowns: %v, %v", models, err)
	}
}

/**
 * @brief 验证重建同秒窗口、更新凭证和清理模型均使旧查询失效。
 * @param t 测试上下文。
 */
func TestCooldown_AccioQuotaWindowRejectsStaleResults(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()
	const oldCredential = `{"region":"global","access_token":"old"}`
	const newCredential = `{"region":"global","access_token":"new"}`
	channel, err := store.CreateConfig(ctx, &model.Config{
		Name: "Accio", AuthType: model.AuthTypeAccioOAuth, OAuthCredential: oldCredential,
		URLs: model.ChannelURLs{{URL: "https://example.test"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	oldUntil, probe, err := store.BeginAccioQuotaCooldown(ctx, channel.ID, oldCredential, "first", now)
	if err != nil || !probe {
		t.Fatalf("begin old round=(%v, %v, %v)", oldUntil, probe, err)
	}
	if err := store.ResetAllCooldowns(ctx, channel.ID); err != nil {
		t.Fatal(err)
	}
	newUntil, probe, err := store.BeginAccioQuotaCooldown(ctx, channel.ID, oldCredential, "first", now.Add(time.Millisecond))
	if err != nil || !probe || newUntil.Unix() != oldUntil.Unix() {
		t.Fatalf("begin same-second replacement=(%v, %v, %v), old until %v", newUntil, probe, err, oldUntil)
	}
	resetAt := now.Add(time.Hour)
	if ok, err := store.ConfirmAccioQuotaWindow(ctx, channel.ID, oldCredential, "first", oldUntil, resetAt); err != nil || ok {
		t.Fatalf("previous round overwrote same-second replacement: (%v, %v)", ok, err)
	}
	if ok, err := store.ReauthorizeAccioCredential(ctx, channel.ID, oldCredential, newCredential); err != nil || !ok {
		t.Fatalf("reauthorize=(%v, %v)", ok, err)
	}
	if ok, err := store.ConfirmAccioQuotaWindow(ctx, channel.ID, oldCredential, "first", newUntil, resetAt); err != nil || ok {
		t.Fatalf("old credential overwrote reauthorized channel: (%v, %v)", ok, err)
	}
	newUntil, probe, err = store.BeginAccioQuotaCooldown(ctx, channel.ID, newCredential, "first", now.Add(2*time.Millisecond))
	if err != nil || !probe {
		t.Fatalf("begin reauthorized round=(%v, %v, %v)", newUntil, probe, err)
	}
	if err := store.ResetModelCooldown(ctx, channel.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ConfirmAccioQuotaWindow(ctx, channel.ID, newCredential, "first", newUntil, resetAt); err != nil || ok {
		t.Fatalf("stale query restored cleared model: (%v, %v)", ok, err)
	}
}

/**
 * @brief 验证额度元数据更新不改变身份，重新授权清额度冷却并保留普通模型冷却。
 * @param t 测试上下文。
 */
func TestCooldown_AccioQuotaWindowPreservesIndependentCooldowns(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()
	const credential = `{"region":"global","access_token":"old"}`
	const metadataChange = `{"oauth_usage":"new usage","access_token":"old","region":"global"}`
	const reauthorized = `{"region":"global","access_token":"new"}`
	channel, err := store.CreateConfig(ctx, &model.Config{
		Name: "Accio", AuthType: model.AuthTypeAccioOAuth, OAuthCredential: credential,
		URLs: model.ChannelURLs{{URL: "https://example.test"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	independent := now.Add(time.Hour)
	independentChannel := now.Add(7 * time.Minute)
	if err := store.SetModelCooldown(ctx, channel.ID, "ordinary", independent); err != nil {
		t.Fatal(err)
	}
	if err := store.SetChannelCooldown(ctx, channel.ID, independentChannel); err != nil {
		t.Fatal(err)
	}
	provisional, probe, err := store.BeginAccioQuotaCooldown(ctx, channel.ID, credential, "first", now)
	if err != nil || !probe {
		t.Fatalf("begin=(%v, %v, %v)", provisional, probe, err)
	}
	if ok, err := store.CompareAndSwapOAuthCredential(ctx, channel.ID, channel.AuthType, credential, metadataChange); err != nil || !ok {
		t.Fatalf("update metadata=(%v, %v)", ok, err)
	}
	if ok, err := store.ConfirmAccioQuotaWindow(ctx, channel.ID, credential, "first", provisional, now.Add(3*time.Minute)); err != nil || !ok {
		t.Fatalf("metadata update prevented confirm: (%v, %v)", ok, err)
	}
	channelCooldowns, err := store.GetAllChannelCooldowns(ctx)
	if err != nil || !channelCooldowns[channel.ID].Equal(independentChannel) {
		t.Fatalf("independent channel cooldown=%v, want %v: %v", channelCooldowns[channel.ID], independentChannel, err)
	}
	if ok, err := store.ReauthorizeAccioCredential(ctx, channel.ID, metadataChange, reauthorized); err != nil || !ok {
		t.Fatalf("reauthorize=(%v, %v)", ok, err)
	}
	models, err := store.GetAllModelCooldowns(ctx)
	if err != nil || len(models[channel.ID]) != 1 || !models[channel.ID]["ordinary"].Equal(independent) {
		t.Fatalf("model cooldowns after reauthorization: %v, %v", models[channel.ID], err)
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
		wg.Go(func() {
			<-start
			duration, err := store.BumpModelCooldown(ctx, channelID, "upstream-model", now, 502)
			if err != nil {
				errs <- err
				return
			}
			durations <- duration
		})
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
	slices.Sort(got)
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
