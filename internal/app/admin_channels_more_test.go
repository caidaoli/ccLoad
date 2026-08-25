package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
)

type failChannelDeleteStore struct {
	storage.Store
	failID int64
}

func (s *failChannelDeleteStore) DeleteConfig(ctx context.Context, id int64) error {
	if id == s.failID {
		return fmt.Errorf("forced delete failure for channel %d", id)
	}
	return s.Store.DeleteConfig(ctx, id)
}

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
	editor := mustParseAPIResponse[struct {
		Channel         ChannelWithCooldown `json:"channel"`
		Keys            []*model.APIKey     `json:"keys"`
		OAuthCredential json.RawMessage     `json:"oauth_credential"`
	}](t, editorResponse.Body.Bytes())
	if editor.Data.Keys == nil || len(editor.Data.Keys) != 0 {
		t.Fatalf("editor keys=%#v, want []", editor.Data.Keys)
	}
	if string(editor.Data.OAuthCredential) != credentialJSON {
		t.Fatalf("editor oauth_credential=%s, want canonical credential %s", editor.Data.OAuthCredential, credentialJSON)
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

func TestXAIChannelUpdateAllowsVersionedProviderBaseURL(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	credential := &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth",
		AccessToken: "xai-access", RefreshToken: "xai-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), Email: "xai@example.com",
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateConfig(context.Background(), newXAIOAuthChannel("xAI models", credentialJSON))
	if err != nil {
		t.Fatal(err)
	}

	models := append([]model.ModelEntry(nil), created.ModelEntries...)
	models = append(models, model.ModelEntry{Model: "grok-manual"})
	path := fmt.Sprintf("/admin/channels/%d", created.ID)
	update := map[string]any{
		"name":                    created.Name,
		"auth_type":               model.AuthTypeXAIOAuth,
		"urls":                    created.URLs,
		"models":                  models,
		"enabled":                 true,
		"protocol_transform_mode": model.ProtocolTransformModeLocal,
	}
	updateContext, updateResponse := newTestContext(t, newJSONRequest(t, http.MethodPut, path, update))
	updateContext.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	server.HandleChannelByID(updateContext)
	if updateResponse.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updateResponse.Code, updateResponse.Body.String())
	}

	persisted, err := store.GetConfig(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.SupportsModel("grok-manual") {
		t.Fatalf("manual model was not persisted: %+v", persisted.ModelEntries)
	}
	if len(persisted.URLs) != 1 || persisted.URLs[0].URL != xaiauth.CLIBaseURL || persisted.URLs[0].Exact {
		t.Fatalf("xAI base URL changed: %+v", persisted.URLs)
	}
	if persisted.OAuthCredential != credentialJSON {
		t.Fatal("xAI OAuth credential changed while saving models")
	}
}

func TestDeleteChannelClearsAnthropicCredentialCache(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), AccountUUID: "account-1",
	}
	raw, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newAnthropicOAuthChannel("Anthropic-delete", raw))
	if err != nil {
		t.Fatal(err)
	}
	server.anthropicCredentials = newAnthropicCredentialManager(
		anthropicauth.NewService(http.DefaultClient), store, nil, nil,
	)
	server.anthropicCredentials.cache(channel.ID, credential)
	deleted, err := server.deleteChannelByID(context.Background(), channel.ID)
	if err != nil || !deleted {
		t.Fatalf("deleteChannelByID() = %v, %v", deleted, err)
	}
	server.anthropicCredentials.mu.RLock()
	_, cached := server.anthropicCredentials.entries[channel.ID]
	server.anthropicCredentials.mu.RUnlock()
	if cached {
		t.Fatal("deleted Anthropic credential remained cached")
	}
}

func TestXAIChannelEditorRejectsMalformedCredential(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	created, err := store.CreateConfig(context.Background(), &model.Config{
		Name:            "xAI malformed credential",
		AuthType:        model.AuthTypeXAIOAuth,
		OAuthCredential: `{"type":"xai","access_token":`,
		URLs:            model.ChannelURLs{{URL: "https://cli-chat-proxy.grok.com", Protocols: []string{"codex"}}},
		Enabled:         true,
		ModelEntries:    []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/admin/channels/%d/editor", created.ID)
	editorContext, editorResponse := newTestContext(t, newRequest(http.MethodGet, path, nil))
	editorContext.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	server.HandleChannelEditor(editorContext)
	if editorResponse.Code != http.StatusInternalServerError {
		t.Fatalf("editor status=%d body=%s", editorResponse.Code, editorResponse.Body.String())
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

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "sk-restricted", AllowedModels: []string{"m1", "M2"}},
		{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "sk-unrestricted"},
		{ChannelID: cfg.ID, KeyIndex: 2, APIKey: "sk-only-removed", AllowedModels: []string{"m2"}},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	if cachedKeys, err := server.getAPIKeys(ctx, cfg.ID); err != nil || len(cachedKeys) != 3 {
		t.Fatalf("prewarm API key cache = (%d, %v), want (3, nil)", len(cachedKeys), err)
	}

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
		keys, err := server.getAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("getAPIKeys after model deletion failed: %v", err)
		}
		if len(keys) != 3 || !slices.Equal(keys[0].AllowedModels, []string{"m1"}) || len(keys[1].AllowedModels) != 0 ||
			len(keys[2].AllowedModels) != 0 || !keys[2].ModelScopeEmpty || !keys[2].Disabled {
			t.Fatalf("unexpected key model scopes after model deletion: %#v", keys)
		}
		available := availableModelFetchAPIKeys(keys, time.Now())
		if slices.ContainsFunc(available, func(key *model.APIKey) bool { return key.KeyIndex == 2 }) {
			t.Fatalf("key whose only allowed model was deleted remains available: %#v", available)
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

func TestHandleBatchClearCooldowns(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.cooldownManager = cooldown.NewManager(store, server)
	server.urlSelector = NewURLSelector()

	ctx := context.Background()
	selected, err := store.CreateConfig(ctx, &model.Config{
		Name: "selected", URLs: model.ChannelURLs{{URL: "https://selected.example"}},
		ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig selected failed: %v", err)
	}
	other, err := store.CreateConfig(ctx, &model.Config{
		Name: "other", URLs: model.ChannelURLs{{URL: "https://other.example"}},
		ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig other failed: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: selected.ID, KeyIndex: 0, APIKey: "sk-selected", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: other.ID, KeyIndex: 0, APIKey: "sk-other", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	cooldownUntil := time.Now().Add(2 * time.Minute)
	for _, channelID := range []int64{selected.ID, other.ID} {
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
	server.urlSelector.RecordLatency(selected.ID, "https://selected.example", 25*time.Millisecond)
	server.urlSelector.DisableURL(selected.ID, "https://selected.example")
	server.urlSelector.CooldownURL(selected.ID, "https://selected.example")
	server.urlSelector.CooldownURL(other.ID, "https://other.example")

	if _, err := server.getAllChannelCooldowns(ctx); err != nil {
		t.Fatalf("prewarm channel cooldowns failed: %v", err)
	}
	if _, err := server.getAllKeyCooldowns(ctx); err != nil {
		t.Fatalf("prewarm key cooldowns failed: %v", err)
	}
	if _, err := server.getAllModelCooldowns(ctx); err != nil {
		t.Fatalf("prewarm model cooldowns failed: %v", err)
	}

	emptyContext, emptyResponse := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-clear-cooldowns", map[string]any{
		"channel_ids": []int64{},
	}))
	server.HandleBatchClearCooldowns(emptyContext)
	if emptyResponse.Code != http.StatusBadRequest {
		t.Fatalf("empty status=%d, want %d", emptyResponse.Code, http.StatusBadRequest)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-clear-cooldowns", map[string]any{
		"channel_ids": []int64{selected.ID, selected.ID, 99999},
	}))
	server.HandleBatchClearCooldowns(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[struct {
		Total         int     `json:"total"`
		Cleared       int     `json:"cleared"`
		NotFound      []int64 `json:"not_found"`
		NotFoundCount int     `json:"not_found_count"`
	}](t, w.Body.Bytes())
	if resp.Data.Total != 2 || resp.Data.Cleared != 1 || resp.Data.NotFoundCount != 1 || len(resp.Data.NotFound) != 1 || resp.Data.NotFound[0] != 99999 {
		t.Fatalf("unexpected summary: %+v", resp.Data)
	}

	selectedConfig, err := store.GetConfig(ctx, selected.ID)
	if err != nil {
		t.Fatalf("GetConfig selected failed: %v", err)
	}
	if selectedConfig.CooldownUntil != 0 || selectedConfig.CooldownDurationMs != 0 {
		t.Fatalf("selected channel cooldown not cleared: %+v", selectedConfig)
	}
	selectedKeys, err := store.GetAPIKeys(ctx, selected.ID)
	if err != nil || len(selectedKeys) != 1 || selectedKeys[0].CooldownUntil != 0 || selectedKeys[0].CooldownDurationMs != 0 {
		t.Fatalf("selected key cooldown not cleared: keys=%+v err=%v", selectedKeys, err)
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
	if _, exists := channelCooldowns[selected.ID]; exists || len(keyCooldowns[selected.ID]) != 0 || len(modelCooldowns[selected.ID]) != 0 {
		t.Fatalf("selected cooldown caches not cleared: channels=%+v keys=%+v models=%+v", channelCooldowns, keyCooldowns, modelCooldowns)
	}
	if _, exists := channelCooldowns[other.ID]; !exists || len(keyCooldowns[other.ID]) != 1 || len(modelCooldowns[other.ID]) != 1 {
		t.Fatalf("other channel cooldowns changed: channels=%+v keys=%+v models=%+v", channelCooldowns, keyCooldowns, modelCooldowns)
	}
	if server.urlSelector.IsCooledDown(selected.ID, "https://selected.example") {
		t.Fatal("selected URL cooldown not cleared")
	}
	if !server.urlSelector.IsCooledDown(other.ID, "https://other.example") {
		t.Fatal("other channel URL cooldown changed")
	}
	selectedStats := server.urlSelector.GetURLStats(selected.ID, []string{"https://selected.example"})
	if len(selectedStats) != 1 || selectedStats[0].LatencyMs <= 0 || !selectedStats[0].Disabled {
		t.Fatalf("clearing cooldown changed selected URL metadata: %+v", selectedStats)
	}
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

	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{name: "negative RPM limit", field: "rpm_limit", value: -1},
		{name: "fractional RPM limit", field: "rpm_limit", value: 1.5},
		{name: "negative max concurrency", field: "max_concurrency", value: -1},
		{name: "negative daily cost limit", field: "daily_cost_limit", value: -0.01},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"channel_ids": []int64{c1.ID}, tc.field: tc.value}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", body))

			server.HandleBatchPatchChannels(c)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}

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
			"priority":                -20,
			"protocol_transform_mode": model.ProtocolTransformModeUpstream,
			"cost_multiplier":         0.25,
			"daily_cost_limit":        12.5,
			"rpm_limit":               60,
			"max_concurrency":         3,
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
			if cfg.Priority != -20 {
				t.Fatalf("channel %d priority=%d, want -20", channelID, cfg.Priority)
			}
			if cfg.CostMultiplier != 0.25 {
				t.Fatalf("channel %d cost_multiplier=%v, want 0.25", channelID, cfg.CostMultiplier)
			}
			if cfg.DailyCostLimit != 12.5 || cfg.RPMLimit != 60 || cfg.MaxConcurrency != 3 {
				t.Fatalf("channel %d limits=(%v, %d, %d), want (12.5, 60, 3)",
					channelID, cfg.DailyCostLimit, cfg.RPMLimit, cfg.MaxConcurrency)
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

	t.Run("imports models into OAuth and API key channels", func(t *testing.T) {
		const credential = `{"type":"antigravity","access_token":"winner","refresh_token":"winner-rt"}`
		oauth, err := store.CreateConfig(ctx, &model.Config{
			Name: "batch-oauth", AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: credential,
			URLs: model.ChannelURLs{{URL: "https://batch-oauth.example.com"}}, Enabled: true,
			ModelEntries: []model.ModelEntry{{Model: "oauth-existing"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		apiKey := createChannel("batch-api-key", model.ProtocolTransformModeAuto)

		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
			"channel_ids":       []int64{oauth.ID, apiKey.ID},
			"model_import_mode": model.ModelImportModeAppend,
			"models":            []model.ModelEntry{{Model: "imported-model", RedirectModel: "upstream-model"}},
		}))

		server.HandleBatchPatchChannels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		for _, channelID := range []int64{oauth.ID, apiKey.ID} {
			persisted, getErr := store.GetConfig(ctx, channelID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if !persisted.SupportsModel("imported-model") {
				t.Fatalf("channel %d models=%+v, want imported-model", channelID, persisted.ModelEntries)
			}
		}
		persistedOAuth, err := store.GetConfig(ctx, oauth.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !persistedOAuth.UsesAntigravityOAuth() || persistedOAuth.OAuthCredential != credential {
			t.Fatalf("OAuth identity changed: auth_type=%q credential=%q", persistedOAuth.GetAuthType(), persistedOAuth.OAuthCredential)
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

func TestHandleBatchDeleteChannels_FinalizesCommittedDeletesBeforeFailure(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	deletedChannel, err := store.CreateConfig(ctx, &model.Config{
		Name: "deleted-before-failure", URLs: model.ChannelURLs{{URL: "https://deleted.example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	failingChannel, err := store.CreateConfig(ctx, &model.Config{
		Name: "delete-fails", URLs: model.ChannelURLs{{URL: "https://failure.example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "m"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	const tokenHash = "batch-partial-delete-token"
	token := &model.AuthToken{
		Token: tokenHash, Description: "disabled after partial delete", IsActive: true,
		AllowedChannelIDs: []int64{deletedChannel.ID}, ChannelRestrictionMode: model.ChannelRestrictionModeAllow,
	}
	if err := store.CreateAuthToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: deletedChannel.ID, KeyIndex: 0, APIKey: "deleted-channel-key",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatal(err)
	}

	failingStore := &failChannelDeleteStore{Store: store, failID: failingChannel.ID}
	server.store = failingStore
	server.channelCache = storage.NewChannelCache(failingStore, time.Hour)
	server.authService = newTestAuthService(t)
	server.authService.store = failingStore
	if err := server.authService.ReloadAuthTokens(); err != nil {
		t.Fatal(err)
	}
	if !server.authService.IsTokenActive(tokenHash) {
		t.Fatal("test token must be active before deletion")
	}
	before, err := server.channelCache.GetEnabledChannelsByModel(ctx, "*")
	if err != nil || len(before) != 2 {
		t.Fatalf("prime channel cache: channels=%d err=%v", len(before), err)
	}
	beforeKeys, err := server.channelCache.GetAPIKeys(ctx, deletedChannel.ID)
	if err != nil || len(beforeKeys) != 1 {
		t.Fatalf("prime API key cache: keys=%d err=%v", len(beforeKeys), err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-delete", map[string]any{
		"channel_ids": []int64{deletedChannel.ID, failingChannel.ID},
	}))
	server.HandleBatchDeleteChannels(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if _, err := store.GetConfig(ctx, deletedChannel.ID); err == nil {
		t.Fatal("first channel delete must remain committed")
	}
	if _, err := store.GetConfig(ctx, failingChannel.ID); err != nil {
		t.Fatalf("failed channel delete must leave channel intact: %v", err)
	}
	storedToken, err := store.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedToken.IsActive || server.authService.IsTokenActive(tokenHash) {
		t.Fatal("token must be disabled in storage and auth cache after its last allowed channel is deleted")
	}
	after, err := server.channelCache.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ID != failingChannel.ID {
		t.Fatalf("channel cache was not finalized after partial delete: %+v", after)
	}
	afterKeys, err := server.channelCache.GetAPIKeys(ctx, deletedChannel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterKeys) != 0 {
		t.Fatalf("API key cache retained %d keys for deleted channel", len(afterKeys))
	}
}
