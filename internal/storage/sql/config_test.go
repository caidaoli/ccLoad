package sql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	sqlstore "ccLoad/internal/storage/sql"

	_ "modernc.org/sqlite"
)

func TestConfig_CreateAndGet(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "config.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	// 创建渠道
	cfg := &model.Config{
		Name: "test-channel",
		URLs: model.ChannelURLs{
			{URL: "https://api.openai.com", Protocols: []string{"openai", "codex"}},
			{URL: "https://api.openai.com/v1/responses", Exact: true, Protocols: []string{"codex"}},
		},
		Priority:                10,
		Enabled:                 true,
		Websockets:              true,
		ProtocolTransformMode:   model.ProtocolTransformModeLocal,
		RetryOtherKeysOnFailure: true,
		AvailableTimeStart:      "22:00",
		AvailableTimeEnd:        "08:00",
		RPMLimit:                60,
		MaxConcurrency:          3,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4"},
			{Model: "gpt-3.5-turbo"},
		},
		CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
			Enabled: true, Name: "Rate limit", Priority: 0, StatusCodes: []int{429},
			Scope: model.CooldownScopeKey, Mode: model.CooldownModeFixed, CooldownSeconds: 90,
		}}},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if created.ID == 0 {
		t.Error("expected non-zero ID")
	}

	// 获取渠道
	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if got.Name != "test-channel" {
		t.Errorf("name: got %q, want %q", got.Name, "test-channel")
	}
	if urls := got.GetURLs(); len(urls) != 2 || urls[0] != "https://api.openai.com" || urls[1] != "https://api.openai.com/v1/responses#" {
		t.Errorf("urls: got %v", urls)
	}
	if len(got.URLs[0].Protocols) != 2 || got.URLs[0].Protocols[0] != "openai" || got.URLs[1].Protocols[0] != "codex" {
		t.Errorf("URL protocols: got %+v", got.URLs)
	}
	if got.Priority != 10 {
		t.Errorf("priority: got %d, want %d", got.Priority, 10)
	}
	if !got.Enabled {
		t.Error("expected enabled=true")
	}
	if !got.Websockets {
		t.Error("expected websockets=true")
	}
	if got.GetProtocolTransformMode() != model.ProtocolTransformModeLocal {
		t.Fatalf("protocol_transform_mode=%q, want local", got.GetProtocolTransformMode())
	}
	if !got.RetryOtherKeysOnFailure {
		t.Error("expected retry_other_keys_on_failure=true")
	}
	if got.AvailableTimeStart != "22:00" || got.AvailableTimeEnd != "08:00" {
		t.Errorf("available time: got %q-%q, want 22:00-08:00", got.AvailableTimeStart, got.AvailableTimeEnd)
	}
	if got.RPMLimit != 60 {
		t.Errorf("rpm_limit: got %d, want 60", got.RPMLimit)
	}
	if got.MaxConcurrency != 3 {
		t.Errorf("max_concurrency: got %d, want 3", got.MaxConcurrency)
	}
	if len(got.ModelEntries) != 2 {
		t.Errorf("model entries count: got %d, want 2", len(got.ModelEntries))
	}
	if got.CooldownDetectionRules == nil || len(got.CooldownDetectionRules.Rules) != 1 {
		t.Fatalf("cooldown detection rules = %#v, want one persisted rule", got.CooldownDetectionRules)
	}
	rule := got.CooldownDetectionRules.Rules[0]
	if !rule.Enabled || rule.Name != "Rate limit" || rule.Priority != 0 || rule.Scope != model.CooldownScopeKey || rule.Mode != model.CooldownModeFixed || rule.CooldownSeconds != 90 {
		t.Fatalf("persisted cooldown detection rule = %#v", rule)
	}

	// 获取不存在的渠道
	_, err = store.GetConfig(ctx, 99999)
	if err == nil {
		t.Error("expected error for non-existent config")
	}
}

func TestConfig_OAuthCredentialRoundTripAndPrivateJSON(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, "codex-credential.db")
	ctx := context.Background()
	credential := `{"type":"codex","access_token":"at-secret","refresh_token":"rt-secret","expired":"2030-01-01T00:00:00Z"}`

	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "codex-user@example.com", AuthType: model.AuthTypeCodexOAuth,
		OAuthCredential: credential, URLs: model.ChannelURLs{{URL: "https://chatgpt.com/backend-api/codex", Protocols: []string{"codex"}}},
		Websockets: true, Enabled: true, ModelEntries: []model.ModelEntry{{Model: "*"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig() error = %v", err)
	}
	if created.GetAuthType() != model.AuthTypeCodexOAuth || created.OAuthCredential != credential || !created.Websockets {
		t.Fatalf("created Codex channel = %#v", created)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "forbidden"}}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("CreateAPIKeysBatch() error = %v, want read-only rejection", err)
	}
	raw, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), "at-secret") || strings.Contains(string(raw), "rt-secret") || strings.Contains(string(raw), "oauth_credential") {
		t.Fatalf("admin JSON leaked credential: %s", raw)
	}

	updatedCredential := `{"type":"codex","access_token":"new-at","refresh_token":"new-rt","expired":"2031-01-01T00:00:00Z"}`
	updated, err := store.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, credential, updatedCredential)
	if err != nil || !updated {
		t.Fatalf("CompareAndSwapOAuthCredential() = (%v, %v), want (true, nil)", updated, err)
	}
	updated, err = store.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, updatedCredential, updatedCredential)
	if err != nil || !updated {
		t.Fatalf("same-value CompareAndSwapOAuthCredential() = (%v, %v), want (true, nil)", updated, err)
	}
	staleCredential := `{"type":"codex","access_token":"stale-at","refresh_token":"stale-rt","expired":"2032-01-01T00:00:00Z"}`
	caseChangedExpected := strings.Replace(updatedCredential, "new-at", "NEW-AT", 1)
	updated, err = store.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, caseChangedExpected, staleCredential)
	if err != nil || updated {
		t.Fatalf("case-changed CompareAndSwapOAuthCredential() = (%v, %v), want (false, nil)", updated, err)
	}
	updated, err = store.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, updatedCredential+" ", staleCredential)
	if err != nil || updated {
		t.Fatalf("space-suffixed CompareAndSwapOAuthCredential() = (%v, %v), want (false, nil)", updated, err)
	}
	updated, err = store.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, credential, staleCredential)
	if err != nil || updated {
		t.Fatalf("stale CompareAndSwapOAuthCredential() = (%v, %v), want (false, nil)", updated, err)
	}
	updated, err = store.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeAntigravityOAuth, updatedCredential, staleCredential)
	if err != nil || updated {
		t.Fatalf("wrong provider CompareAndSwapOAuthCredential() = (%v, %v), want (false, nil)", updated, err)
	}
	updated, err = store.CompareAndSwapOAuthCredential(ctx, created.ID+99999, model.AuthTypeCodexOAuth, updatedCredential, staleCredential)
	if err != nil || updated {
		t.Fatalf("missing channel CompareAndSwapOAuthCredential() = (%v, %v), want (false, nil)", updated, err)
	}
	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if got.OAuthCredential != updatedCredential {
		t.Fatalf("credential=%q, want updated payload", got.OAuthCredential)
	}
	if _, err := store.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeAPIKey, updatedCredential, staleCredential); err == nil {
		t.Fatal("CompareAndSwapOAuthCredential() accepted API key auth type")
	}
	if _, err := store.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, "", staleCredential); err == nil {
		t.Fatal("CompareAndSwapOAuthCredential() accepted empty expected credential")
	}
	if _, err := store.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, updatedCredential, ""); err == nil {
		t.Fatal("CompareAndSwapOAuthCredential() accepted empty next credential")
	}
}

func TestConfig_CreateWithExistingExplicitOAuthIDCannotReplaceCredential(t *testing.T) {
	store := newTestStore(t, "explicit-oauth-credential.db")
	ctx := context.Background()
	winner := `{"type":"codex","access_token":"at-winner","refresh_token":"rt-winner","expired":"2031-01-01T00:00:00Z"}`
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "codex-explicit", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: winner,
		URLs:    model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "gpt-test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateConfig(ctx, &model.Config{
		ID: created.ID, Name: created.Name, AuthType: model.AuthTypeCodexOAuth,
		OAuthCredential: `{"type":"codex","access_token":"at-stale","refresh_token":"rt-stale","expired":"2032-01-01T00:00:00Z"}`,
		URLs:            model.ChannelURLs{{URL: "https://stale.example.com", Protocols: []string{"codex"}}},
		Enabled:         true, ModelEntries: []model.ModelEntry{{Model: "gpt-stale"}},
	})
	if err == nil {
		t.Fatal("CreateConfig() replaced an existing OAuth channel by explicit ID")
	}
	persisted, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.OAuthCredential != winner {
		t.Fatalf("OAuth credential = %q, want CAS winner", persisted.OAuthCredential)
	}
}

func TestConfig_DeleteConfigIfOAuthSnapshotMatches(t *testing.T) {
	store := newTestStore(t, "conditional-oauth-delete.db")
	ctx := context.Background()
	original := `{"type":"codex","access_token":"old-at","refresh_token":"old-rt"}`
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "conditional-oauth-delete", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: original,
		URLs:    model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "gpt-test"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	staleSnapshot := created.Clone()
	staleSnapshot.OAuthCredential = original + " "
	deleted, err := store.DeleteConfigIfOAuthSnapshotMatches(ctx, staleSnapshot)
	if err != nil || deleted {
		t.Fatalf("stale conditional delete = (%v, %v), want (false, nil)", deleted, err)
	}
	if _, err := store.GetConfig(ctx, created.ID); err != nil {
		t.Fatalf("stale cleanup deleted current channel: %v", err)
	}

	updated := `{"type":"codex","access_token":"new-at","refresh_token":"new-rt"}`
	if swapped, err := store.CompareAndSwapOAuthCredential(
		ctx, created.ID, model.AuthTypeCodexOAuth, original, updated,
	); err != nil || !swapped {
		t.Fatalf("credential update = (%v, %v)", swapped, err)
	}
	deleted, err = store.DeleteConfigIfOAuthSnapshotMatches(ctx, created)
	if err != nil || deleted {
		t.Fatalf("old snapshot conditional delete = (%v, %v), want (false, nil)", deleted, err)
	}
	updatedSnapshot, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEdit := updatedSnapshot.Clone()
	updatedSnapshot.URLs = model.ChannelURLs{{URL: "https://healthy.example.com", Protocols: []string{"codex"}}}
	updatedSnapshot.ModelEntries = []model.ModelEntry{{Model: "gpt-next"}}
	if _, err := store.UpdateConfig(ctx, created.ID, updatedSnapshot); err != nil {
		t.Fatal(err)
	}
	deleted, err = store.DeleteConfigIfOAuthSnapshotMatches(ctx, beforeEdit)
	if err != nil || deleted {
		t.Fatalf("edited routing snapshot conditional delete = (%v, %v), want (false, nil)", deleted, err)
	}
	updatedSnapshot, err = store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err = store.DeleteConfigIfOAuthSnapshotMatches(ctx, updatedSnapshot)
	if err != nil || !deleted {
		t.Fatalf("matching conditional delete = (%v, %v), want (true, nil)", deleted, err)
	}
	if _, err := store.GetConfig(ctx, created.ID); err == nil {
		t.Fatal("matching conditional delete kept the channel")
	}
}

func TestConfig_CreateWithExplicitIDRejectsOAuthAcrossExistingAPIKey(t *testing.T) {
	store := newTestStore(t, "explicit-api-key-to-oauth.db")
	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "api-key-winner", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://api-key.example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateConfig(ctx, &model.Config{
		ID: created.ID, Name: "oauth-stale", AuthType: model.AuthTypeCodexOAuth,
		OAuthCredential: `{"type":"codex","access_token":"at-stale","refresh_token":"rt-stale","expired":"2031-01-01T00:00:00Z"}`,
		URLs:            model.ChannelURLs{{URL: "https://oauth.example.com", Protocols: []string{"codex"}}},
		Enabled:         true,
	})
	if err == nil {
		t.Fatal("CreateConfig() replaced an existing API-key channel with incoming OAuth state")
	}
	persisted, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.AuthType != model.AuthTypeAPIKey || persisted.Name != "api-key-winner" || persisted.OAuthCredential != "" {
		t.Fatalf("persisted config = %#v, want original API-key channel", persisted)
	}
}

func TestConfig_ListConfigs(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "list.db")

	ctx := context.Background()

	// 创建多个渠道
	for i := 1; i <= 3; i++ {
		cfg := &model.Config{
			Name:     fmt.Sprintf("channel-%c", rune('A'+i-1)),
			URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
			Priority: i * 10,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: fmt.Sprintf("model-%c", rune('a'+i-1))},
			},
		}
		if _, err := store.CreateConfig(ctx, cfg); err != nil {
			t.Fatalf("create config %d: %v", i, err)
		}
	}

	// 列出所有渠道
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	if len(configs) != 3 {
		t.Errorf("expected 3 configs, got %d", len(configs))
	}
	for _, cfg := range configs {
		if cfg.GetProtocolTransformMode() != model.ProtocolTransformModeAuto {
			t.Fatalf("channel %d protocol_transform_mode=%q, want auto", cfg.ID, cfg.GetProtocolTransformMode())
		}
	}

	// 验证按优先级降序排列
	for i := 1; i < len(configs); i++ {
		if configs[i-1].Priority < configs[i].Priority {
			t.Errorf("configs not sorted by priority DESC: %d < %d",
				configs[i-1].Priority, configs[i].Priority)
		}
	}
}

func TestConfig_UpdateChannelEnabledOnlyTouchesEnabled(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "update-enabled.db")
	ctx := context.Background()

	created, err := store.CreateConfig(ctx, &model.Config{
		Name:     "toggle-only",
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority: 42,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "gemini-2.5-pro", RedirectModel: "gemini-2.5-pro"},
			{Model: "gemini-2.5-flash", RedirectModel: "flash-upstream"},
		},
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-toggle",
		KeyStrategy: model.KeyStrategyRoundRobin,
	}}); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	updated, err := store.UpdateChannelEnabled(ctx, created.ID, false)
	if err != nil {
		t.Fatalf("update enabled: %v", err)
	}
	if updated.Enabled {
		t.Fatalf("updated response should be disabled")
	}

	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if got.Enabled {
		t.Fatalf("stored channel should be disabled")
	}
	if got.Name != created.Name || got.GetURLs()[0] != created.GetURLs()[0] || got.Priority != created.Priority {
		t.Fatalf("non-enabled fields changed: got=%+v want=%+v", got, created)
	}
	if len(got.ModelEntries) != 2 || got.ModelEntries[1].RedirectModel != "flash-upstream" {
		t.Fatalf("model entries changed: %#v", got.ModelEntries)
	}

	keys, err := store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("get api keys: %v", err)
	}
	if len(keys) != 1 || keys[0].APIKey != "sk-toggle" || keys[0].KeyStrategy != model.KeyStrategyRoundRobin {
		t.Fatalf("api keys changed: %#v", keys)
	}
}

func TestConfig_UpdateConfig(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "update.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	// 创建渠道
	cfg := &model.Config{
		Name:     "original-name",
		URLs:     model.ChannelURLs{{URL: "https://old.api.com"}},
		Priority: 1,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "old-model"},
		},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	// 更新渠道
	created.Name = "updated-name"
	created.URLs = model.ChannelURLs{{URL: "https://new.api.com"}}
	created.Priority = 100
	created.Enabled = false
	created.RetryOtherKeysOnFailure = true
	created.ModelEntries = []model.ModelEntry{
		{Model: "new-model-1"},
		{Model: "new-model-2"},
	}
	created.CooldownDetectionRules = &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
		Enabled: true, Name: "Reset time", Priority: 0, MessagePattern: `reset at (?P<until>\d{4}-\d{2}-\d{2})`,
		Scope: model.CooldownScopeChannel, Mode: model.CooldownModeResetTime,
		TimeCapture: "until", TimeFormat: model.CooldownTimeFormatDateTime, TimeLayout: "2006-01-02", Timezone: "UTC",
	}}}

	if _, err := store.UpdateConfig(ctx, created.ID, created); err != nil {
		t.Fatalf("update config: %v", err)
	}

	// 验证更新
	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("get config after update: %v", err)
	}
	if got.Name != "updated-name" {
		t.Errorf("name: got %q, want %q", got.Name, "updated-name")
	}
	if urls := got.GetURLs(); len(urls) != 1 || urls[0] != "https://new.api.com" {
		t.Errorf("urls: got %v, want [https://new.api.com]", urls)
	}
	if got.Priority != 100 {
		t.Errorf("priority: got %d, want %d", got.Priority, 100)
	}
	if got.Enabled {
		t.Error("expected enabled=false")
	}
	if !got.RetryOtherKeysOnFailure {
		t.Error("expected retry_other_keys_on_failure=true")
	}
	if len(got.ModelEntries) != 2 {
		t.Errorf("model entries count: got %d, want 2", len(got.ModelEntries))
	}
	if got.CooldownDetectionRules == nil || len(got.CooldownDetectionRules.Rules) != 1 || got.CooldownDetectionRules.Rules[0].Mode != model.CooldownModeResetTime || got.CooldownDetectionRules.Rules[0].TimeFormat != model.CooldownTimeFormatDateTime {
		t.Fatalf("updated cooldown detection rules = %#v", got.CooldownDetectionRules)
	}
}

func TestConfig_DeleteConfig(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "delete.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()

	// 创建渠道
	cfg := &model.Config{
		Name:     "to-delete",
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority: 1,
		Enabled:  true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	other, err := store.CreateConfig(ctx, &model.Config{
		Name:    "keep-channel",
		URLs:    model.ChannelURLs{{URL: "https://keep.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create other config: %v", err)
	}

	allowToken := &model.AuthToken{
		Token:                  "delete-config-allow-token",
		Description:            "allow restriction",
		IsActive:               true,
		AllowedChannelIDs:      []int64{created.ID, other.ID},
		ChannelRestrictionMode: model.ChannelRestrictionModeAllow,
	}
	allowOnlyToken := &model.AuthToken{
		Token:                  "delete-config-allow-only-token",
		Description:            "allow only deleted channel",
		IsActive:               true,
		AllowedChannelIDs:      []int64{created.ID},
		ChannelRestrictionMode: model.ChannelRestrictionModeAllow,
	}
	denyToken := &model.AuthToken{
		Token:                  "delete-config-deny-token",
		Description:            "deny restriction",
		IsActive:               true,
		AllowedChannelIDs:      []int64{created.ID},
		ChannelRestrictionMode: model.ChannelRestrictionModeDeny,
	}
	for _, token := range []*model.AuthToken{allowToken, allowOnlyToken, denyToken} {
		if err := store.CreateAuthToken(ctx, token); err != nil {
			t.Fatalf("create auth token %q: %v", token.Description, err)
		}
	}

	// 删除渠道
	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	// 验证已删除
	_, err = store.GetConfig(ctx, created.ID)
	if err == nil {
		t.Error("expected error after delete")
	}

	storedAllow, err := store.GetAuthToken(ctx, allowToken.ID)
	if err != nil {
		t.Fatalf("get allow token after delete: %v", err)
	}
	if !slices.Equal(storedAllow.AllowedChannelIDs, []int64{other.ID}) {
		t.Fatalf("allow token channel ids=%v, want [%d]", storedAllow.AllowedChannelIDs, other.ID)
	}
	storedAllowOnly, err := store.GetAuthToken(ctx, allowOnlyToken.ID)
	if err != nil {
		t.Fatalf("get allow-only token after delete: %v", err)
	}
	if storedAllowOnly.IsActive {
		t.Fatal("allow-only token must be disabled after its last allowed channel is deleted")
	}
	if len(storedAllowOnly.AllowedChannelIDs) != 0 {
		t.Fatalf("allow-only token channel ids=%v, want empty", storedAllowOnly.AllowedChannelIDs)
	}
	storedDeny, err := store.GetAuthToken(ctx, denyToken.ID)
	if err != nil {
		t.Fatalf("get deny token after delete: %v", err)
	}
	if len(storedDeny.AllowedChannelIDs) != 0 {
		t.Fatalf("deny token channel ids=%v, want empty", storedDeny.AllowedChannelIDs)
	}
	if storedDeny.ChannelRestrictionMode != model.ChannelRestrictionModeDeny {
		t.Fatalf("deny token mode=%q, want deny", storedDeny.ChannelRestrictionMode)
	}
	if !storedDeny.IsActive {
		t.Fatal("deny token must remain active after its only denied channel is deleted")
	}
	denyRestriction, err := storedDeny.ChannelRestriction()
	if err != nil {
		t.Fatalf("build deny restriction: %v", err)
	}
	if !denyRestriction.Allows(other.ID) {
		t.Fatalf("deny token must allow remaining channel %d", other.ID)
	}
}

func TestConfig_DeleteConfig_RemovesLogsAndDebugLogs(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "delete-logs.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:    "delete-with-logs",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	now := time.Now()
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{{
		Time:       model.JSONTime{Time: now},
		Model:      "model-1",
		ChannelID:  created.ID,
		StatusCode: 200,
		DebugData: &model.DebugLogEntry{
			CreatedAt:   now.Unix(),
			ReqMethod:   "POST",
			ReqURL:      "https://api.example.com/v1/messages",
			ReqHeaders:  "{}",
			ReqBody:     []byte(`{"model":"model-1"}`),
			RespStatus:  200,
			RespHeaders: "{}",
			RespBody:    []byte(`{"ok":true}`),
		},
	}}); err != nil {
		t.Fatalf("add log with debug data: %v", err)
	}

	logs, err := store.ListLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), 10, 0, nil)
	if err != nil {
		t.Fatalf("list logs before delete: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log before delete, got %d", len(logs))
	}
	logID := logs[0].ID
	debugLog, err := store.GetDebugLogByLogID(ctx, logID)
	if err != nil {
		t.Fatalf("get debug log before delete: %v", err)
	}
	if debugLog == nil {
		t.Fatalf("expected debug log before delete")
	}

	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	channelID := created.ID
	count, err := store.CountLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), &model.LogFilter{ChannelID: &channelID})
	if err != nil {
		t.Fatalf("count logs after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected channel logs removed, got %d", count)
	}
	debugLog, err = store.GetDebugLogByLogID(ctx, logID)
	if err != nil {
		t.Fatalf("get debug log after delete: %v", err)
	}
	if debugLog != nil {
		t.Fatalf("expected debug log removed after deleting channel")
	}
}

func TestLog_AddLogAndBatchAddLogs_IgnoreDeletedChannel(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "deleted-channel-log.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:    "deleted-channel",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	now := time.Now()
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:       model.JSONTime{Time: now},
		Model:      "stale-single-log",
		ChannelID:  created.ID,
		StatusCode: 200,
	}); err != nil {
		t.Fatalf("add stale single log: %v", err)
	}
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{
			Time:       model.JSONTime{Time: now},
			Model:      "stale-channel-log",
			ChannelID:  created.ID,
			StatusCode: 200,
		},
		{
			Time:       model.JSONTime{Time: now},
			Model:      "system-log",
			ChannelID:  0,
			StatusCode: 503,
		},
	}); err != nil {
		t.Fatalf("batch add logs: %v", err)
	}

	deletedChannelID := created.ID
	deletedCount, err := store.CountLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), &model.LogFilter{ChannelID: &deletedChannelID})
	if err != nil {
		t.Fatalf("count deleted-channel logs: %v", err)
	}
	if deletedCount != 0 {
		t.Fatalf("deleted channel log was inserted after channel deletion: count=%d", deletedCount)
	}

	allLogs, err := store.ListLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), 10, 0, nil)
	if err != nil {
		t.Fatalf("list all logs: %v", err)
	}
	if len(allLogs) != 1 || allLogs[0].ChannelID != 0 {
		t.Fatalf("expected only channel_id=0 log to remain, got %+v", allLogs)
	}
}

func TestConfig_DeleteConfig_PurgesOrphanLogsWhenChannelMissing(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "orphan-channel-log.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	const orphanChannelID int64 = 194
	now := time.Now()
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{
			Time:       model.JSONTime{Time: now},
			Model:      "orphan-channel-log",
			ChannelID:  orphanChannelID,
			StatusCode: 200,
		},
		{
			Time:       model.JSONTime{Time: now},
			Model:      "system-log",
			ChannelID:  0,
			StatusCode: 503,
		},
	}); err != nil {
		t.Fatalf("batch add orphan logs: %v", err)
	}

	if err := store.DeleteConfig(ctx, orphanChannelID); err != nil {
		t.Fatalf("delete missing config: %v", err)
	}

	channelID := orphanChannelID
	count, err := store.CountLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), &model.LogFilter{ChannelID: &channelID})
	if err != nil {
		t.Fatalf("count orphan-channel logs: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected orphan channel logs purged, got %d", count)
	}

	allLogs, err := store.ListLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), 10, 0, nil)
	if err != nil {
		t.Fatalf("list all logs: %v", err)
	}
	if len(allLogs) != 1 || allLogs[0].ChannelID != 0 {
		t.Fatalf("expected only channel_id=0 log to remain, got %+v", allLogs)
	}
}

func TestConfig_DeleteConfig_ImportBatchWithSameIDAcceptsLogs(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "restore-deleted-channel.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:    "deleted-before-restore",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	imported := &model.ChannelWithKeys{
		Config: &model.Config{
			ID:      created.ID,
			Name:    "restored-channel",
			URLs:    model.ChannelURLs{{URL: "https://api-restored.example.com"}},
			Enabled: true,
			ModelEntries: []model.ModelEntry{
				{Model: "restored-model"},
			},
		},
		APIKeys: []model.APIKey{
			{KeyIndex: 0, APIKey: "sk-restored", KeyStrategy: model.KeyStrategySequential},
		},
	}
	createdCount, updatedCount, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{imported})
	if err != nil {
		t.Fatalf("import restored channel: %v", err)
	}
	if createdCount != 1 || updatedCount != 0 {
		t.Fatalf("import counts = (%d,%d), want (1,0)", createdCount, updatedCount)
	}

	now := time.Now()
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:       model.JSONTime{Time: now},
		Model:      "restored-model",
		ChannelID:  created.ID,
		StatusCode: 200,
	}); err != nil {
		t.Fatalf("add restored channel log: %v", err)
	}

	channelID := created.ID
	count, err := store.CountLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), &model.LogFilter{ChannelID: &channelID})
	if err != nil {
		t.Fatalf("count restored channel logs: %v", err)
	}
	if count != 1 {
		t.Fatalf("restored channel log count=%d, want 1", count)
	}
}

func TestConfig_DeleteConfig_AllowsRecreateWithSameIDAndKeyIndicesInMemoryStore(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:    "to-delete-memory",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-old-0", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-old-1", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("create api keys batch: %v", err)
	}

	if err := store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	recreated, err := store.CreateConfig(ctx, &model.Config{
		ID:      created.ID,
		Name:    "recreated-memory",
		URLs:    model.ChannelURLs{{URL: "https://api-recreated.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("recreate config with explicit id: %v", err)
	}

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: recreated.ID, KeyIndex: 0, APIKey: "sk-new-0", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("create api key after recreate: %v", err)
	}

	keys, err := store.GetAPIKeys(ctx, recreated.ID)
	if err != nil {
		t.Fatalf("get recreated api keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 recreated api key, got %d", len(keys))
	}
}

func TestConfig_GetEnabledChannelsByModel(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "model_query.db")

	ctx := context.Background()

	// 创建启用的渠道支持 gpt-4
	cfg1 := &model.Config{
		Name:     "gpt4-channel",
		URLs:     model.ChannelURLs{{URL: "https://api.openai.com"}},
		Priority: 10,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4"},
			{Model: "gpt-4-turbo"},
		},
	}
	created1, err := store.CreateConfig(ctx, cfg1)
	if err != nil {
		t.Fatalf("create config 1: %v", err)
	}

	// 创建启用的渠道支持 claude
	cfg2 := &model.Config{
		Name:     "claude-channel",
		URLs:     model.ChannelURLs{{URL: "https://api.anthropic.com"}},
		Priority: 20,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "claude-3-opus"},
			{Model: "gpt-4", RedirectModel: "gpt-4-upstream", Disabled: true},
		},
	}
	created2, err := store.CreateConfig(ctx, cfg2)
	if err != nil {
		t.Fatalf("create config 2: %v", err)
	}

	// 创建禁用的渠道支持 gpt-4
	cfg3 := &model.Config{
		Name:     "disabled-channel",
		URLs:     model.ChannelURLs{{URL: "https://api.disabled.com"}},
		Priority: 30,
		Enabled:  false,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4"},
		},
	}
	if _, err := store.CreateConfig(ctx, cfg3); err != nil {
		t.Fatalf("create config 3: %v", err)
	}

	// 为渠道添加 API Key（需要至少有一个 key 才能被选中）
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created1.ID, KeyIndex: 0, APIKey: "sk-key1"},
	}); err != nil {
		t.Fatalf("create api keys batch for channel 1: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created2.ID, KeyIndex: 0, APIKey: "sk-key2"},
	}); err != nil {
		t.Fatalf("create api keys batch for channel 2: %v", err)
	}

	// 查询支持 gpt-4 的启用渠道
	configs, err := store.GetEnabledChannelsByModel(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("get enabled channels by model: %v", err)
	}
	if len(configs) != 1 {
		t.Errorf("expected 1 enabled channel with gpt-4, got %d", len(configs))
	}
	if len(configs) > 0 && configs[0].Name != "gpt4-channel" {
		t.Errorf("expected gpt4-channel, got %s", configs[0].Name)
	}

	persisted, err := store.GetConfig(ctx, created2.ID)
	if err != nil {
		t.Fatalf("get config with disabled model: %v", err)
	}
	if len(persisted.ModelEntries) != 2 || !persisted.ModelEntries[1].Disabled {
		t.Fatalf("disabled model state not persisted: %#v", persisted.ModelEntries)
	}
	if persisted.SupportsModel("gpt-4") {
		t.Fatal("persisted disabled model must not be supported")
	}

	// 通配符查询所有启用渠道
	allConfigs, err := store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("get all enabled channels: %v", err)
	}
	if len(allConfigs) != 2 {
		t.Errorf("expected 2 enabled channels, got %d", len(allConfigs))
	}
}

func TestConfig_GetEnabledChannelsIncludesCooledEnabledChannels(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "enabled_includes_cooled.db")
	ctx := context.Background()

	cooled, err := store.CreateConfig(ctx, &model.Config{
		Name:     "cooled-enabled",
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority: 100,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("create cooled config: %v", err)
	}
	if err := store.SetChannelCooldown(ctx, cooled.ID, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatalf("set channel cooldown: %v", err)
	}

	disabled, err := store.CreateConfig(ctx, &model.Config{
		Name:     "disabled",
		URLs:     model.ChannelURLs{{URL: "https://disabled.example.com"}},
		Priority: 90,
		Enabled:  false,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("create disabled config: %v", err)
	}

	assertHasOnlyCooled := func(name string, configs []*model.Config) {
		t.Helper()
		foundCooled := false
		for _, cfg := range configs {
			switch cfg.ID {
			case cooled.ID:
				foundCooled = true
			case disabled.ID:
				t.Fatalf("%s returned disabled channel: %+v", name, cfg)
			}
		}
		if !foundCooled {
			t.Fatalf("%s did not return cooled enabled channel; got %+v", name, configs)
		}
	}

	byModel, err := store.GetEnabledChannelsByModel(ctx, "gpt-4o")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel: %v", err)
	}
	assertHasOnlyCooled("GetEnabledChannelsByModel", byModel)

	allByModel, err := store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel(*): %v", err)
	}
	assertHasOnlyCooled("GetEnabledChannelsByModel(*)", allByModel)

}

func TestConfig_BatchUpdatePriority(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "priority.db")

	ctx := context.Background()

	// 创建多个渠道
	var ids []int64
	for i := 1; i <= 3; i++ {
		cfg := &model.Config{
			Name:     "channel-" + string(rune('A'+i-1)),
			URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
			Priority: i,
			Enabled:  true,
		}
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("create config %d: %v", i, err)
		}
		ids = append(ids, created.ID)
	}

	// 批量更新优先级
	updates := []struct {
		ID       int64
		Priority int
	}{
		{ids[0], 100},
		{ids[1], 200},
		{ids[2], 300},
	}
	if _, err := store.BatchUpdatePriority(ctx, updates); err != nil {
		t.Fatalf("batch update priority: %v", err)
	}

	// 验证更新
	for i, id := range ids {
		got, err := store.GetConfig(ctx, id)
		if err != nil {
			t.Fatalf("get config %d: %v", i, err)
		}
		expected := (i + 1) * 100
		if got.Priority != expected {
			t.Errorf("config %d priority: got %d, want %d", i, got.Priority, expected)
		}
	}
}

func TestConfig_BatchPatchConfigs(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "batch-patch.db")
	ctx := context.Background()
	create := func(name, mode, scheduledModel string, models []model.ModelEntry) *model.Config {
		t.Helper()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:                    name,
			URLs:                    model.ChannelURLs{{URL: "https://" + name + ".example.com"}},
			Priority:                7,
			Enabled:                 true,
			ProtocolTransformMode:   mode,
			ScheduledCheckEnabled:   true,
			ScheduledCheckModel:     scheduledModel,
			CostMultiplier:          1,
			DailyCostLimit:          5,
			RPMLimit:                10,
			MaxConcurrency:          2,
			ModelEntries:            models,
			RetryOtherKeysOnFailure: true,
		})
		if err != nil {
			t.Fatalf("CreateConfig(%s): %v", name, err)
		}
		return cfg
	}

	first := create("batch-first", model.ProtocolTransformModeAuto, "upstream-a", []model.ModelEntry{
		{Model: "alias-a", RedirectModel: "upstream-a", Disabled: true},
	})
	second := create("batch-second", model.ProtocolTransformModeLocal, "model-x", []model.ModelEntry{
		{Model: "model-x"},
	})

	multiplier := 0.5
	priority := -12
	dailyCostLimit := 25.5
	rpmLimit := 120
	maxConcurrency := 8
	mode := model.ProtocolTransformModeLocal
	result, err := store.BatchPatchConfigs(ctx, []int64{first.ID, second.ID, first.ID, 99999}, model.BatchConfigPatch{
		Priority:              &priority,
		CostMultiplier:        &multiplier,
		DailyCostLimit:        &dailyCostLimit,
		RPMLimit:              &rpmLimit,
		MaxConcurrency:        &maxConcurrency,
		ProtocolTransformMode: &mode,
		ModelImportMode:       model.ModelImportModeAppend,
		ModelEntries: []model.ModelEntry{
			{Model: "ALIAS-A", RedirectModel: "ignored-duplicate"},
			{Model: "model-b", RedirectModel: "upstream-b"},
		},
	})
	if err != nil {
		t.Fatalf("BatchPatchConfigs append: %v", err)
	}
	if result.Updated != 2 || result.Unchanged != 0 || len(result.NotFound) != 1 || result.NotFound[0] != 99999 {
		t.Fatalf("unexpected append result: %+v", result)
	}

	for _, channelID := range []int64{first.ID, second.ID} {
		got, err := store.GetConfig(ctx, channelID)
		if err != nil {
			t.Fatalf("GetConfig(%d): %v", channelID, err)
		}
		if got.Priority != priority || got.CostMultiplier != multiplier || got.DailyCostLimit != dailyCostLimit ||
			got.RPMLimit != rpmLimit || got.MaxConcurrency != maxConcurrency || got.GetProtocolTransformMode() != mode {
			t.Fatalf("channel %d advanced fields = (%v, %v, %d, %d, %q)", channelID,
				got.CostMultiplier, got.DailyCostLimit, got.RPMLimit, got.MaxConcurrency, got.GetProtocolTransformMode())
		}
		if !got.RetryOtherKeysOnFailure {
			t.Fatalf("channel %d unrelated fields changed: %+v", channelID, got)
		}
		if channelID == first.ID {
			if len(got.ModelEntries) != 2 || got.ModelEntries[0].Model != "alias-a" || !got.ModelEntries[0].Disabled || got.ModelEntries[1].Model != "model-b" {
				t.Fatalf("channel %d models=%+v", channelID, got.ModelEntries)
			}
		} else if len(got.ModelEntries) != 3 || got.ModelEntries[1].Model != "ALIAS-A" || got.ModelEntries[2].Model != "model-b" || got.ModelEntries[2].RedirectModel != "upstream-b" {
			t.Fatalf("channel %d models=%+v", channelID, got.ModelEntries)
		}
	}

	zeroRPM := 0
	limitOnlyResult, err := store.BatchPatchConfigs(ctx, []int64{first.ID}, model.BatchConfigPatch{RPMLimit: &zeroRPM})
	if err != nil {
		t.Fatalf("BatchPatchConfigs RPM only: %v", err)
	}
	if limitOnlyResult.Updated != 1 || limitOnlyResult.Unchanged != 0 {
		t.Fatalf("unexpected RPM-only result: %+v", limitOnlyResult)
	}
	firstAfterRPM, err := store.GetConfig(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstAfterRPM.RPMLimit != 0 || firstAfterRPM.MaxConcurrency != maxConcurrency ||
		firstAfterRPM.DailyCostLimit != dailyCostLimit || firstAfterRPM.CostMultiplier != multiplier ||
		firstAfterRPM.Priority != priority {
		t.Fatalf("RPM-only patch changed unrelated limits: %+v", firstAfterRPM)
	}

	replaceResult, err := store.BatchPatchConfigs(ctx, []int64{first.ID, second.ID}, model.BatchConfigPatch{
		ModelImportMode: model.ModelImportModeReplace,
		ModelEntries:    []model.ModelEntry{{Model: "replacement"}},
	})
	if err != nil {
		t.Fatalf("BatchPatchConfigs replace: %v", err)
	}
	if replaceResult.Updated != 2 || replaceResult.Unchanged != 0 {
		t.Fatalf("unexpected replace result: %+v", replaceResult)
	}
	for _, channelID := range []int64{first.ID, second.ID} {
		got, err := store.GetConfig(ctx, channelID)
		if err != nil {
			t.Fatalf("GetConfig(%d) after replace: %v", channelID, err)
		}
		if len(got.ModelEntries) != 1 || got.ModelEntries[0].Model != "replacement" {
			t.Fatalf("channel %d replacement models=%+v", channelID, got.ModelEntries)
		}
		if got.ScheduledCheckModel != "" {
			t.Fatalf("channel %d scheduled_check_model=%q, want empty", channelID, got.ScheduledCheckModel)
		}
	}

	unchanged, err := store.BatchPatchConfigs(ctx, []int64{first.ID, second.ID}, model.BatchConfigPatch{
		ModelImportMode: model.ModelImportModeReplace,
		ModelEntries:    []model.ModelEntry{{Model: "replacement"}},
	})
	if err != nil {
		t.Fatalf("BatchPatchConfigs unchanged: %v", err)
	}
	if unchanged.Updated != 0 || unchanged.Unchanged != 2 {
		t.Fatalf("unexpected unchanged result: %+v", unchanged)
	}
}

func TestConfig_BatchPatchConfigsUpdatesOAuthModelsWithoutCredentialMutation(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "batch-patch-oauth.db")
	ctx := context.Background()
	credential := `{"type":"codex","access_token":"winner","refresh_token":"winner-rt"}`
	oauth, err := store.CreateConfig(ctx, &model.Config{
		Name: "batch-patch-oauth", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
		URLs: model.ChannelURLs{{URL: "https://oauth.example.com"}}, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "winner-model"}}, ScheduledCheckModel: "winner-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	apiKey, err := store.CreateConfig(ctx, &model.Config{
		Name: "batch-patch-api-key", URLs: model.ChannelURLs{{URL: "https://key.example.com"}}, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "key-model"}}, ScheduledCheckModel: "key-model",
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := store.BatchPatchConfigs(ctx, []int64{apiKey.ID, oauth.ID}, model.BatchConfigPatch{
		ModelImportMode: model.ModelImportModeReplace,
		ModelEntries:    []model.ModelEntry{{Model: "replacement-model"}},
	})
	if err != nil {
		t.Fatalf("BatchPatchConfigs: %v", err)
	}
	if result.Updated != 2 || result.Unchanged != 0 || len(result.NotFound) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	for _, channelID := range []int64{oauth.ID, apiKey.ID} {
		got, getErr := store.GetConfig(ctx, channelID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if len(got.ModelEntries) != 1 || got.ModelEntries[0].Model != "replacement-model" || got.ScheduledCheckModel != "" {
			t.Fatalf("channel %d model state = %+v", channelID, got)
		}
	}
	persistedOAuth, err := store.GetConfig(ctx, oauth.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persistedOAuth.UsesCodexOAuth() || persistedOAuth.OAuthCredential != credential {
		t.Fatalf("OAuth identity changed: auth_type=%q credential=%q", persistedOAuth.GetAuthType(), persistedOAuth.OAuthCredential)
	}
}

func TestConfig_SyncOAuthConfigReplicaIsAtomicAndProviderRestricted(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "sync-oauth-replica.db")
	replica := store.(*sqlstore.SQLStore)
	ctx := context.Background()
	credential := `{"type":"codex","access_token":"old","refresh_token":"old-rt"}`
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "replica-old", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
		URLs: model.ChannelURLs{{URL: "https://old.example.com"}}, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "old-model"}}, ScheduledCheckModel: "old-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongProvider := created.Clone()
	wrongProvider.AuthType = model.AuthTypeAntigravityOAuth
	wrongProvider.OAuthCredential = `{"type":"antigravity","access_token":"wrong"}`
	if err := replica.SyncOAuthConfigReplica(ctx, wrongProvider); err == nil {
		t.Fatal("SyncOAuthConfigReplica accepted a different OAuth provider")
	}

	if _, err := replica.ExecContext(ctx, `
		CREATE TRIGGER reject_replica_model_insert
		BEFORE INSERT ON channel_models
		BEGIN
			SELECT RAISE(FAIL, 'replica models are read only');
		END
	`); err != nil {
		t.Fatal(err)
	}
	winner := created.Clone()
	winner.Name = "replica-winner"
	winner.URLs = model.ChannelURLs{{URL: "https://winner.example.com", Protocols: []string{"codex"}}}
	winner.OAuthCredential = `{"type":"codex","access_token":"winner","refresh_token":"winner-rt"}`
	winner.ModelEntries = []model.ModelEntry{{Model: "winner-model"}}
	winner.ScheduledCheckModel = "winner-model"
	if err := replica.SyncOAuthConfigReplica(ctx, winner); err == nil {
		t.Fatal("SyncOAuthConfigReplica succeeded after model persistence failed")
	}
	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != created.Name || got.OAuthCredential != credential || !got.SupportsModel("old-model") || got.SupportsModel("winner-model") {
		t.Fatalf("failed replica sync was not atomic: %+v", got)
	}
}

func TestConfig_ModelRedirect(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "redirect.db")

	ctx := context.Background()

	// 创建带模型重定向的渠道
	cfg := &model.Config{
		Name:     "redirect-channel",
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority: 10,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4", RedirectModel: "gpt-4-turbo"},
			{Model: "gpt-3.5-turbo"},
		},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	// 验证模型重定向被保存
	got, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}

	var foundRedirect bool
	for _, entry := range got.ModelEntries {
		if entry.Model == "gpt-4" && entry.RedirectModel == "gpt-4-turbo" {
			foundRedirect = true
			break
		}
	}
	if !foundRedirect {
		t.Error("expected to find gpt-4 -> gpt-4-turbo redirect")
	}
}
