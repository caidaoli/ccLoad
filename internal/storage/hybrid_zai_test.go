package storage

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

/**
 * @brief 验证条件冷却成功后同步，换账号后的旧凭证不污染本地或副本。
 * @param t 测试上下文。
 */
func TestHybridStore_ConditionalOAuthModelCooldown(t *testing.T) {
	primary, local := createTestSQLiteStore(t), createTestSQLiteStore(t)
	hybrid := NewHybridStore(local, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	ctx := context.Background()
	cfg, err := hybrid.CreateConfig(ctx, &model.Config{Name: "conditional", AuthType: model.AuthTypeZAIOAuth, OAuthCredential: `{"api_key":"old.secret"}`, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute).Truncate(time.Second)
	if ok, err := hybrid.SetModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "glm-Trial", until); err != nil || !ok {
		t.Fatalf("write: %t %v", ok, err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		cooldowns, err := primary.GetAllModelCooldowns(ctx)
		return err == nil && cooldowns[cfg.ID]["glm-Trial"].Equal(until)
	})

	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if until, ok, err := hybrid.BumpModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "recovery-Trial", past, 429); err != nil || !ok || !until.Equal(past.Add(time.Minute)) {
		t.Fatalf("conditional bump: %v %v %v", until, ok, err)
	}
	if candidates, err := hybrid.ListExpiredOAuthModelCooldowns(ctx, cfg.AuthType, "recovery-Trial", time.Now()); err != nil || len(candidates) != 1 || candidates[0].ID != cfg.ID || candidates[0].OAuthCredential != cfg.OAuthCredential {
		t.Fatalf("local recovery candidates: count=%d err=%v", len(candidates), err)
	}
	waitForCondition(t, 3*time.Second, func() bool { return hybrid.RuntimeMetrics().PrimarySyncPending == 0 })
	if changed, err := hybrid.ResetExpiredModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "recovery-Trial", time.Now()); err != nil || !changed {
		t.Fatalf("conditional recovery: %v %v", changed, err)
	}
	if candidates, err := hybrid.ListExpiredOAuthModelCooldowns(ctx, cfg.AuthType, "recovery-Trial", time.Now()); err != nil || len(candidates) != 0 {
		t.Fatalf("recovered candidates: count=%d err=%v", len(candidates), err)
	}
	waitForCondition(t, 3*time.Second, func() bool { return hybrid.RuntimeMetrics().PrimarySyncPending == 0 })
	// 从两个持久化副本观察恢复后的首次失败，不能沿用旧的指数历史。
	for _, store := range []Store{local, primary} {
		now := time.Now()
		until, ok, err := store.BumpModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "recovery-Trial", now, 429)
		if err != nil || !ok || !until.Equal(now.Add(time.Minute)) {
			t.Fatalf("replica recovery: %v %v %v", until, ok, err)
		}
	}
	next := `{"api_key":"new.secret"}`
	if ok, err := hybrid.CompareAndSwapOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, next); err != nil || !ok {
		t.Fatalf("swap: %t %v", ok, err)
	}
	if ok, err := hybrid.SetModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "glm-Trial", until.Add(time.Hour)); err != nil || ok {
		t.Fatalf("stale write: %t %v", ok, err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		current, err := primary.GetConfig(ctx, cfg.ID)
		return err == nil && current.OAuthCredential == next && hybrid.RuntimeMetrics().PrimarySyncPending == 0
	})
	for _, store := range []Store{local, primary} {
		cooldowns, err := store.GetAllModelCooldowns(ctx)
		if err != nil || !cooldowns[cfg.ID]["glm-Trial"].Equal(until) {
			t.Fatalf("rejected write changed cooldown: %v %v", cooldowns, err)
		}
	}
}
