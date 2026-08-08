package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
)

func TestXAIChannelResponsesExposeOnlySafeOAuthMetadata(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	credential := &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth",
		AccessToken: "xai-access-secret", RefreshToken: "xai-refresh-secret", IDToken: "xai-id-secret",
		Expired: "2030-01-01T00:00:00Z", Email: "safe@example.com", Subject: "private-subject",
		SubscriptionTier: "supergrok", EntitlementStatus: "active",
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xAI safe response", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: credentialJSON,
		URLs:    model.ChannelURLs{{URL: "https://cli-chat-proxy.grok.com", Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	assertSafe := func(name, body string) {
		t.Helper()
		for _, secret := range []string{"xai-access-secret", "xai-refresh-secret", "xai-id-secret", "private-subject", credentialJSON} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s leaked %q: %s", name, secret, body)
			}
		}
	}

	listContext, listResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.HandleChannels(listContext)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	assertSafe("list", listResponse.Body.String())
	list := mustParseAPIResponse[[]ChannelWithCooldown](t, listResponse.Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].XAIEmail != "safe@example.com" ||
		list.Data[0].GetAuthType() != model.AuthTypeXAIOAuth ||
		list.Data[0].XAISubscriptionTier != "supergrok" || list.Data[0].XAIEntitlementStatus != "active" {
		t.Fatalf("list metadata=%+v", list.Data)
	}

	path := fmt.Sprintf("/admin/channels/%d", created.ID)
	detailContext, detailResponse := newTestContext(t, newRequest(http.MethodGet, path, nil))
	detailContext.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	server.HandleChannelByID(detailContext)
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailResponse.Code, detailResponse.Body.String())
	}
	assertSafe("detail", detailResponse.Body.String())
	detail := mustParseAPIResponse[ChannelWithCooldown](t, detailResponse.Body.Bytes())
	if detail.Data.GetAuthType() != model.AuthTypeXAIOAuth || detail.Data.XAIEmail != "safe@example.com" ||
		detail.Data.XAISubscriptionTier != "supergrok" || detail.Data.XAIEntitlementStatus != "active" {
		t.Fatalf("detail metadata=%+v", detail.Data)
	}

	editorContext, editorResponse := newTestContext(t, newRequest(http.MethodGet, path+"/editor", nil))
	editorContext.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	server.HandleChannelEditor(editorContext)
	if editorResponse.Code != http.StatusOK {
		t.Fatalf("editor status=%d body=%s", editorResponse.Code, editorResponse.Body.String())
	}
	assertSafe("editor", editorResponse.Body.String())
	editor := mustParseAPIResponse[struct {
		Channel ChannelWithCooldown `json:"channel"`
		Keys    []*model.APIKey     `json:"keys"`
	}](t, editorResponse.Body.Bytes())
	if editor.Data.Keys == nil || len(editor.Data.Keys) != 0 {
		t.Fatalf("editor keys=%#v, want []", editor.Data.Keys)
	}
	if editor.Data.Channel.GetAuthType() != model.AuthTypeXAIOAuth || editor.Data.Channel.XAIEmail != "safe@example.com" ||
		editor.Data.Channel.XAISubscriptionTier != "supergrok" || editor.Data.Channel.XAIEntitlementStatus != "active" {
		t.Fatalf("editor metadata=%+v", editor.Data.Channel)
	}

	update := map[string]any{
		"name": "xAI safe response updated", "auth_type": model.AuthTypeXAIOAuth,
		"oauth_credential": `{"access_token":"replacement-secret"}`,
		"urls":             []map[string]any{{"url": "https://cli-chat-proxy.grok.com", "protocols": []string{"codex"}}},
		"models":           []map[string]any{{"model": "grok-4.5"}}, "enabled": true,
	}
	updateContext, updateResponse := newTestContext(t, newJSONRequest(t, http.MethodPut, path, update))
	updateContext.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	server.HandleChannelByID(updateContext)
	if updateResponse.Code != http.StatusConflict {
		t.Fatalf("credential update status=%d body=%s", updateResponse.Code, updateResponse.Body.String())
	}
	persisted, err := store.GetConfig(context.Background(), created.ID)
	if err != nil || persisted.OAuthCredential != credentialJSON {
		t.Fatalf("persisted credential changed: err=%v config=%+v", err, persisted)
	}
}

func TestHandleDeleteAPIKey(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "ch",
		URLs:         model.ChannelURLs{{URL: "https://example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now()
	keys := []*model.APIKey{
		{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "k0", KeyStrategy: model.KeyStrategySequential, CreatedAt: model.JSONTime{Time: now}, UpdatedAt: model.JSONTime{Time: now}},
		{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "k1", KeyStrategy: model.KeyStrategySequential, CreatedAt: model.JSONTime{Time: now}, UpdatedAt: model.JSONTime{Time: now}},
		{ChannelID: cfg.ID, KeyIndex: 2, APIKey: "k2", KeyStrategy: model.KeyStrategySequential, CreatedAt: model.JSONTime{Time: now}, UpdatedAt: model.JSONTime{Time: now}},
	}
	if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	t.Run("invalid channel id", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/abc/keys/0", nil))
		c.Params = gin.Params{{Key: "id", Value: "abc"}, {Key: "keyIndex", Value: "0"}}

		server.HandleDeleteAPIKey(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid key index", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/1/keys/x", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}, {Key: "keyIndex", Value: "x"}}

		server.HandleDeleteAPIKey(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("key not found", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/1/keys/9", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}, {Key: "keyIndex", Value: "9"}}

		server.HandleDeleteAPIKey(c)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("success compacts indices", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/1/keys/1", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}, {Key: "keyIndex", Value: "1"}}

		server.HandleDeleteAPIKey(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		after, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetAPIKeys failed: %v", err)
		}
		if len(after) != 2 {
			t.Fatalf("keys len=%d, want 2", len(after))
		}
		// 删除索引1后，原 index2 应被压缩成 index1
		if after[0].KeyIndex != 0 || after[1].KeyIndex != 1 {
			t.Fatalf("unexpected indices: %+v", []int{after[0].KeyIndex, after[1].KeyIndex})
		}
		if after[1].APIKey != "k2" {
			t.Fatalf("expected compacted key to be k2 at index1, got %q", after[1].APIKey)
		}
	})
}

func TestHandleAddAndDeleteModels(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "ch",
		URLs:         model.ChannelURLs{{URL: "https://example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	t.Run("add invalid request", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/1/models", []byte(`{"models":[]}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleAddModels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("add invalid model entry", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/1/models", []byte(`{"models":[{"model":""}]}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleAddModels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("add dedup success", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/1/models", []byte(`{"models":[{"model":"m1"},{"model":"m2"}]}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleAddModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		updated, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		if len(updated.ModelEntries) != 2 {
			t.Fatalf("ModelEntries len=%d, want 2", len(updated.ModelEntries))
		}
	})

	t.Run("delete invalid request", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodDelete, "/admin/channels/1/models", []byte(`{}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleDeleteModels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("delete success", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodDelete, "/admin/channels/1/models", []byte(`{"models":["m2","absent"]}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleDeleteModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		updated, err := store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		if len(updated.ModelEntries) != 1 || updated.ModelEntries[0].Model != "m1" {
			t.Fatalf("unexpected remaining models: %#v", updated.ModelEntries)
		}
	})
}

func TestHandleBatchUpdatePriority(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	c1, err := store.CreateConfig(ctx, &model.Config{Name: "c1", URLs: model.ChannelURLs{{URL: "https://x"}}, Priority: 1, ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true})
	if err != nil {
		t.Fatalf("CreateConfig c1 failed: %v", err)
	}
	c2, err := store.CreateConfig(ctx, &model.Config{Name: "c2", URLs: model.ChannelURLs{{URL: "https://x"}}, Priority: 2, ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true})
	if err != nil {
		t.Fatalf("CreateConfig c2 failed: %v", err)
	}

	t.Run("invalid json", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-priority", []byte(`{`)))

		server.HandleBatchUpdatePriority(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("empty updates", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-priority", []byte(`{"updates":[]}`)))

		server.HandleBatchUpdatePriority(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("success", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-priority", map[string]any{
			"updates": []map[string]any{
				{"id": c1.ID, "priority": 100},
				{"id": c2.ID, "priority": 200},
			},
		}))

		server.HandleBatchUpdatePriority(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		updated1, _ := store.GetConfig(ctx, c1.ID)
		updated2, _ := store.GetConfig(ctx, c2.ID)
		if updated1.Priority != 100 || updated2.Priority != 200 {
			t.Fatalf("priority not updated: got (%d,%d)", updated1.Priority, updated2.Priority)
		}
	})
}

func TestHandleBatchSetEnabled(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.cooldownManager = cooldown.NewManager(store, server)

	ctx := context.Background()
	c1, err := store.CreateConfig(ctx, &model.Config{Name: "c1", URLs: model.ChannelURLs{{URL: "https://x"}}, Priority: 1, ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true})
	if err != nil {
		t.Fatalf("CreateConfig c1 failed: %v", err)
	}
	c2, err := store.CreateConfig(ctx, &model.Config{Name: "c2", URLs: model.ChannelURLs{{URL: "https://x"}}, Priority: 2, ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: false})
	if err != nil {
		t.Fatalf("CreateConfig c2 failed: %v", err)
	}

	t.Run("invalid json", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-enabled", []byte(`{`)))

		server.HandleBatchSetEnabled(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing enabled", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-enabled", []byte(`{"channel_ids":[1]}`)))

		server.HandleBatchSetEnabled(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("empty channel ids", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-enabled", []byte(`{"channel_ids":[],"enabled":true}`)))

		server.HandleBatchSetEnabled(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("partial success", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-enabled", map[string]any{
			"channel_ids": []int64{c1.ID, c2.ID, c2.ID, 99999},
			"enabled":     false,
		}))

		server.HandleBatchSetEnabled(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Updated       int `json:"updated"`
				Unchanged     int `json:"unchanged"`
				NotFoundCount int `json:"not_found_count"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success {
			t.Fatalf("expected success=true, body=%s", w.Body.String())
		}
		if resp.Data.Updated != 1 || resp.Data.Unchanged != 1 || resp.Data.NotFoundCount != 1 {
			t.Fatalf("unexpected summary: %+v", resp.Data)
		}

		updated1, err := store.GetConfig(ctx, c1.ID)
		if err != nil {
			t.Fatalf("GetConfig c1 failed: %v", err)
		}
		updated2, err := store.GetConfig(ctx, c2.ID)
		if err != nil {
			t.Fatalf("GetConfig c2 failed: %v", err)
		}
		if updated1.Enabled {
			t.Fatalf("c1 should be disabled")
		}
		if updated2.Enabled {
			t.Fatalf("c2 should remain disabled")
		}
	})

	t.Run("enable clears all cooldowns", func(t *testing.T) {
		if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: c1.ID, KeyIndex: 0, APIKey: "sk-c1", KeyStrategy: model.KeyStrategySequential},
			{ChannelID: c2.ID, KeyIndex: 0, APIKey: "sk-c2", KeyStrategy: model.KeyStrategySequential},
		}); err != nil {
			t.Fatalf("CreateAPIKeysBatch failed: %v", err)
		}

		cooldownUntil := time.Now().Add(2 * time.Minute)
		for _, channelID := range []int64{c1.ID, c2.ID} {
			if err := store.SetChannelCooldown(ctx, channelID, cooldownUntil); err != nil {
				t.Fatalf("SetChannelCooldown(%d) failed: %v", channelID, err)
			}
			if err := store.SetKeyCooldown(ctx, channelID, 0, cooldownUntil); err != nil {
				t.Fatalf("SetKeyCooldown(%d) failed: %v", channelID, err)
			}
			if err := store.SetModelCooldown(ctx, channelID, "m", cooldownUntil); err != nil {
				t.Fatalf("SetModelCooldown(%d) failed: %v", channelID, err)
			}
		}

		if _, err := server.getAllChannelCooldowns(ctx); err != nil {
			t.Fatalf("prewarm channel cooldowns failed: %v", err)
		}
		if _, err := server.getAllKeyCooldowns(ctx); err != nil {
			t.Fatalf("prewarm key cooldowns failed: %v", err)
		}
		if _, err := server.getAllModelCooldowns(ctx); err != nil {
			t.Fatalf("prewarm model cooldowns failed: %v", err)
		}

		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-enabled", map[string]any{
			"channel_ids": []int64{c1.ID, c2.ID},
			"enabled":     true,
		}))
		server.HandleBatchSetEnabled(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		channelCooldowns, err := server.getAllChannelCooldowns(ctx)
		if err != nil {
			t.Fatalf("get channel cooldowns failed: %v", err)
		}
		keyCooldowns, err := server.getAllKeyCooldowns(ctx)
		if err != nil {
			t.Fatalf("get key cooldowns failed: %v", err)
		}
		modelCooldowns, err := server.getAllModelCooldowns(ctx)
		if err != nil {
			t.Fatalf("get model cooldowns failed: %v", err)
		}
		for _, channelID := range []int64{c1.ID, c2.ID} {
			cfg, err := store.GetConfig(ctx, channelID)
			if err != nil {
				t.Fatalf("GetConfig(%d) failed: %v", channelID, err)
			}
			if !cfg.Enabled || cfg.CooldownUntil != 0 || cfg.CooldownDurationMs != 0 {
				t.Fatalf("channel %d not enabled with cleared cooldown: %+v", channelID, cfg)
			}
			keys, err := store.GetAPIKeys(ctx, channelID)
			if err != nil {
				t.Fatalf("GetAPIKeys(%d) failed: %v", channelID, err)
			}
			if len(keys) != 1 || keys[0].CooldownUntil != 0 || keys[0].CooldownDurationMs != 0 {
				t.Fatalf("channel %d key cooldown not cleared: %+v", channelID, keys)
			}
			if _, ok := channelCooldowns[channelID]; ok {
				t.Fatalf("channel %d remains in channel cooldown cache: %+v", channelID, channelCooldowns)
			}
			if len(keyCooldowns[channelID]) != 0 {
				t.Fatalf("channel %d remains in key cooldown cache: %+v", channelID, keyCooldowns[channelID])
			}
			if len(modelCooldowns[channelID]) != 0 {
				t.Fatalf("channel %d remains in model cooldown cache: %+v", channelID, modelCooldowns[channelID])
			}
		}
	})
}

func TestHandleBatchPatchChannels(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	createChannel := func(name, mode string) *model.Config {
		t.Helper()
		cfg, err := store.CreateConfig(ctx, &model.Config{
			Name:                  name,
			URLs:                  model.ChannelURLs{{URL: "https://" + name + ".example.com"}},
			ProtocolTransformMode: mode,
			ModelEntries:          []model.ModelEntry{{Model: "m"}},
			Enabled:               true,
		})
		if err != nil {
			t.Fatalf("CreateConfig %s failed: %v", name, err)
		}
		return cfg
	}
	c1 := createChannel("protocol-auto", model.ProtocolTransformModeAuto)
	c2 := createChannel("protocol-local", model.ProtocolTransformModeLocal)
	c3 := createChannel("protocol-upstream", model.ProtocolTransformModeUpstream)

	t.Run("invalid json", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-advanced", []byte(`{`)))

		server.HandleBatchPatchChannels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("empty patch", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
			"channel_ids": []int64{c1.ID},
		}))

		server.HandleBatchPatchChannels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
			"channel_ids":             []int64{c1.ID},
			"protocol_transform_mode": "invalid",
		}))

		server.HandleBatchPatchChannels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("negative cost multiplier", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
			"channel_ids":     []int64{c1.ID},
			"cost_multiplier": -0.01,
		}))

		server.HandleBatchPatchChannels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid model import", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
			"channel_ids": []int64{c1.ID},
			"models":      []model.ModelEntry{{Model: "new-model"}},
		}))

		server.HandleBatchPatchChannels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("empty channel ids", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
			"channel_ids":             []int64{},
			"protocol_transform_mode": model.ProtocolTransformModeUpstream,
		}))

		server.HandleBatchPatchChannels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("updates selected channels", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
			"channel_ids":             []int64{c1.ID, c2.ID, c3.ID, c2.ID, 99999},
			"protocol_transform_mode": model.ProtocolTransformModeUpstream,
			"cost_multiplier":         0.25,
			"model_import_mode":       model.ModelImportModeAppend,
			"models": []model.ModelEntry{
				{Model: "m", RedirectModel: "ignored-duplicate"},
				{Model: "new-model", RedirectModel: "upstream-model"},
			},
		}))

		server.HandleBatchPatchChannels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Total         int     `json:"total"`
				Updated       int     `json:"updated"`
				Unchanged     int     `json:"unchanged"`
				NotFound      []int64 `json:"not_found"`
				NotFoundCount int     `json:"not_found_count"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if resp.Data.Total != 4 || resp.Data.Updated != 3 || resp.Data.Unchanged != 0 || resp.Data.NotFoundCount != 1 {
			t.Fatalf("unexpected summary: %+v", resp.Data)
		}
		if len(resp.Data.NotFound) != 1 || resp.Data.NotFound[0] != 99999 {
			t.Fatalf("unexpected not_found: %#v", resp.Data.NotFound)
		}

		for _, channelID := range []int64{c1.ID, c2.ID, c3.ID} {
			cfg, err := store.GetConfig(ctx, channelID)
			if err != nil {
				t.Fatalf("GetConfig(%d) failed: %v", channelID, err)
			}
			if cfg.GetProtocolTransformMode() != model.ProtocolTransformModeUpstream {
				t.Fatalf("channel %d mode=%q, want %q", channelID, cfg.GetProtocolTransformMode(), model.ProtocolTransformModeUpstream)
			}
			if cfg.CostMultiplier != 0.25 {
				t.Fatalf("channel %d cost_multiplier=%v, want 0.25", channelID, cfg.CostMultiplier)
			}
			if len(cfg.ModelEntries) != 2 || cfg.ModelEntries[1].Model != "new-model" || cfg.ModelEntries[1].RedirectModel != "upstream-model" {
				t.Fatalf("channel %d models=%+v", channelID, cfg.ModelEntries)
			}
		}
	})

	t.Run("replace models", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
			"channel_ids":       []int64{c1.ID, c2.ID},
			"model_import_mode": model.ModelImportModeReplace,
			"models":            []model.ModelEntry{{Model: "replacement"}},
		}))

		server.HandleBatchPatchChannels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		for _, channelID := range []int64{c1.ID, c2.ID} {
			cfg, err := store.GetConfig(ctx, channelID)
			if err != nil {
				t.Fatalf("GetConfig(%d): %v", channelID, err)
			}
			if len(cfg.ModelEntries) != 1 || cfg.ModelEntries[0].Model != "replacement" {
				t.Fatalf("channel %d models=%+v", channelID, cfg.ModelEntries)
			}
		}
	})
}

func TestHandleBatchDeleteChannels(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.urlSelector = NewURLSelector()

	ctx := context.Background()
	c1, err := store.CreateConfig(ctx, &model.Config{Name: "c1", URLs: model.ChannelURLs{{URL: "https://x"}}, Priority: 1, ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true})
	if err != nil {
		t.Fatalf("CreateConfig c1 failed: %v", err)
	}
	c2, err := store.CreateConfig(ctx, &model.Config{Name: "c2", URLs: model.ChannelURLs{{URL: "https://y"}}, Priority: 2, ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true})
	if err != nil {
		t.Fatalf("CreateConfig c2 failed: %v", err)
	}

	server.urlSelector.RecordLatency(c1.ID, "https://x", 10*time.Millisecond)
	server.urlSelector.RecordLatency(c2.ID, "https://y", 20*time.Millisecond)
	server.urlSelector.CooldownURL(c1.ID, "https://x")

	t.Run("invalid json", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-delete", []byte(`{`)))

		server.HandleBatchDeleteChannels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("empty channel ids", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/batch-delete", []byte(`{"channel_ids":[]}`)))

		server.HandleBatchDeleteChannels(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("partial success", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-delete", map[string]any{
			"channel_ids": []int64{c1.ID, c2.ID, c2.ID, 99999},
		}))

		server.HandleBatchDeleteChannels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Deleted       int     `json:"deleted"`
				NotFound      []int64 `json:"not_found"`
				NotFoundCount int     `json:"not_found_count"`
				Total         int     `json:"total"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success {
			t.Fatalf("expected success=true, body=%s", w.Body.String())
		}
		if resp.Data.Deleted != 2 || resp.Data.NotFoundCount != 1 || resp.Data.Total != 3 {
			t.Fatalf("unexpected summary: %+v", resp.Data)
		}
		if len(resp.Data.NotFound) != 1 || resp.Data.NotFound[0] != 99999 {
			t.Fatalf("unexpected not_found: %#v", resp.Data.NotFound)
		}

		if _, err := store.GetConfig(ctx, c1.ID); err == nil {
			t.Fatalf("c1 should be deleted")
		}
		if _, err := store.GetConfig(ctx, c2.ID); err == nil {
			t.Fatalf("c2 should be deleted")
		}

		for key := range server.urlSelector.latencies {
			if key.channelID == c1.ID || key.channelID == c2.ID {
				t.Fatalf("expected deleted channel latency state removed, found key=%+v", key)
			}
		}
		for key := range server.urlSelector.cooldowns {
			if key.channelID == c1.ID || key.channelID == c2.ID {
				t.Fatalf("expected deleted channel cooldown state removed, found key=%+v", key)
			}
		}
	})
}
