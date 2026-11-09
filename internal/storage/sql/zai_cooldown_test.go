package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

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
