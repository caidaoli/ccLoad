package sql_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

/**
 * @brief 历史查询按认证、模型和截止时间筛选，并保留停用渠道及条件恢复边界。
 * @param t 测试上下文。
 */
func TestCooldown_ListExpiredOAuthModelCooldowns(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	cutoff := time.Now().Truncate(time.Second)
	want := make(map[int64]*model.Config)
	for _, tc := range []struct {
		name, auth, model string
		until             time.Time
		enabled, include  bool
	}{
		{"expired", model.AuthTypeZAIOAuth, "glm-Trial", cutoff.Add(-time.Hour), true, true},
		{"boundary-disabled", model.AuthTypeZAIOAuth, "glm-Trial", cutoff, false, true},
		{"future", model.AuthTypeZAIOAuth, "glm-Trial", cutoff.Add(time.Second), true, false},
		{"other-model", model.AuthTypeZAIOAuth, "other-Trial", cutoff, true, false},
		{"other-auth", model.AuthTypeCodexOAuth, "glm-Trial", cutoff, true, false},
		{"no-history", model.AuthTypeZAIOAuth, "", time.Time{}, true, false},
	} {
		cfg, err := store.CreateConfig(ctx, &model.Config{Name: tc.name, AuthType: tc.auth, OAuthCredential: `{"jwt_token":"` + tc.name + `"}`, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: tc.enabled})
		if err != nil {
			t.Fatal(err)
		}
		if tc.model != "" {
			if err := store.SetModelCooldown(ctx, cfg.ID, tc.model, tc.until); err != nil {
				t.Fatal(err)
			}
		}
		if tc.include {
			want[cfg.ID] = cfg
		}
	}
	// 重复读取仍应返回相同历史，查询不能充当清理操作。
	for range 2 {
		configs, err := store.ListExpiredOAuthModelCooldowns(ctx, " ZAI_OAUTH ", " glm-Trial ", cutoff)
		if err != nil {
			t.Fatal(err)
		}
		if len(configs) != len(want) {
			t.Fatalf("candidate count = %d, want %d", len(configs), len(want))
		}
		seen := make(map[int64]bool)
		for _, cfg := range configs {
			expected := want[cfg.ID]
			if expected == nil || seen[cfg.ID] || cfg.AuthType != expected.AuthType || cfg.OAuthCredential != expected.OAuthCredential {
				t.Fatalf("unexpected identity snapshot for channel %d", cfg.ID)
			}
			seen[cfg.ID] = true
		}
	}
	// 查询拿到快照后换凭证，旧快照仍不能删除历史。
	for _, cfg := range want {
		if ok, err := store.CompareAndSwapOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, `{"jwt_token":"renewed"}`); err != nil || !ok {
			t.Fatalf("swap: %v %v", ok, err)
		}
		if changed, err := store.ResetExpiredModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "glm-Trial", cutoff); err != nil || changed {
			t.Fatalf("stale recovery: %v %v", changed, err)
		}
	}
	configs, err := store.ListExpiredOAuthModelCooldowns(ctx, model.AuthTypeZAIOAuth, "glm-Trial", cutoff)
	if err != nil || len(configs) != len(want) {
		t.Fatalf("renewed candidates: count=%d err=%v", len(configs), err)
	}
	for _, cfg := range configs {
		if cfg.OAuthCredential != `{"jwt_token":"renewed"}` {
			t.Fatalf("outdated identity for channel %d", cfg.ID)
		}
		if changed, err := store.ResetExpiredModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "glm-Trial", cutoff); err != nil || !changed {
			t.Fatalf("recovery: %v %v", changed, err)
		}
	}
	if configs, err := store.ListExpiredOAuthModelCooldowns(ctx, model.AuthTypeZAIOAuth, "glm-Trial", cutoff); err != nil || len(configs) != 0 {
		t.Fatalf("remaining candidates: count=%d err=%v", len(configs), err)
	}
	for _, tc := range []struct {
		auth, name string
		cutoff     time.Time
	}{
		{model.AuthTypeAPIKey, "glm-Trial", cutoff},
		{"invalid", "glm-Trial", cutoff},
		{model.AuthTypeZAIOAuth, " ", cutoff},
		{model.AuthTypeZAIOAuth, "glm-Trial", time.Time{}},
	} {
		if _, err := store.ListExpiredOAuthModelCooldowns(ctx, tc.auth, tc.name, tc.cutoff); err == nil {
			t.Fatal("accepted invalid query")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.ListExpiredOAuthModelCooldowns(canceled, model.AuthTypeZAIOAuth, "glm-Trial", cutoff); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query: %v", err)
	}
}

/**
 * @brief 验证条件冷却的完整凭证比较、拒写不覆盖及换账号边界。
 */
func TestCooldown_IfOAuthCredential(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{Name: "conditional", AuthType: model.AuthTypeZAIOAuth, OAuthCredential: `{"api_key":"id.Secret"}`, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute).Truncate(time.Second)
	if ok, err := store.SetModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "glm-Trial", until); err != nil || !ok {
		t.Fatalf("matching identity: %t %v", ok, err)
	}
	for _, tc := range []struct {
		name, auth, credential string
		id                     int64
		wantErr                bool
	}{
		{"case", cfg.AuthType, `{"api_key":"id.secret"}`, cfg.ID, false},
		{"space", cfg.AuthType, cfg.OAuthCredential + " ", cfg.ID, false},
		{"provider", model.AuthTypeCodexOAuth, cfg.OAuthCredential, cfg.ID, false},
		{"missing", cfg.AuthType, cfg.OAuthCredential, cfg.ID + 100, false},
		{"empty", cfg.AuthType, "", cfg.ID, true},
		{"api-key", model.AuthTypeAPIKey, cfg.OAuthCredential, cfg.ID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := store.SetModelCooldownIfOAuthCredential(ctx, tc.id, tc.auth, tc.credential, "glm-Trial", until.Add(time.Hour))
			if ok || (err != nil) != tc.wantErr {
				t.Fatalf("unexpected conditional write: %t %v", ok, err)
			}
		})
	}
	if ok, err := store.CompareAndSwapOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, `{"api_key":"new.secret"}`); err != nil || !ok {
		t.Fatalf("swap: %t %v", ok, err)
	}
	if ok, err := store.SetModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "new-Trial", until); err != nil || ok {
		t.Fatalf("stale write: %t %v", ok, err)
	}
	cooldowns, err := store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cooldowns[cfg.ID]) != 1 || !cooldowns[cfg.ID]["glm-Trial"].Equal(until) {
		t.Fatalf("rejected write changed cooldowns: %v", cooldowns)
	}
}

/**
 * @brief 条件递增复用存储策略，恢复只清旧历史并保护并发失败和换账号。
 * @param t 测试上下文。
 */
func TestCooldown_OAuthBackoffRecovery(t *testing.T) {
	store := newTestStore(t)
	store.ConfigureCooldown(util.CooldownSettings{RateLimitSec: 7, MinSec: 1})
	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{Name: "recovery", AuthType: model.AuthTypeZAIOAuth, OAuthCredential: `{"jwt_token":"old"}`, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	past := now.Add(-time.Hour)
	bump := func(at time.Time, want time.Duration) {
		t.Helper()
		until, ok, err := store.BumpModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "glm-Trial", at, 429)
		if err != nil || !ok || !until.Equal(at.Add(want)) {
			t.Fatalf("bump: until=%v matched=%v err=%v want=%v", until, ok, err, want)
		}
	}
	bump(past, 7*time.Second)
	bump(past.Add(time.Minute), 14*time.Second)
	if changed, err := store.ResetExpiredModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "glm-Trial", now); err != nil || !changed {
		t.Fatalf("recover: %v %v", changed, err)
	}
	bump(now, 7*time.Second)
	if changed, err := store.ResetExpiredModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, cfg.OAuthCredential, "glm-Trial", now); err != nil || changed {
		t.Fatalf("cleared concurrent window: %v %v", changed, err)
	}
	if changed, err := store.ResetExpiredModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, `{"jwt_token":"stale"}`, "glm-Trial", now.Add(time.Hour)); err != nil || changed {
		t.Fatalf("stale recovery: %v %v", changed, err)
	}
	if until, ok, err := store.BumpModelCooldownIfOAuthCredential(ctx, cfg.ID, cfg.AuthType, `{"jwt_token":"stale"}`, "glm-Trial", now, 429); err != nil || ok || !until.IsZero() {
		t.Fatalf("stale bump: %v %v %v", until, ok, err)
	}
	bump(now.Add(time.Minute), 14*time.Second)
}
