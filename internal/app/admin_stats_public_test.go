package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"ccLoad/internal/version"
)

func TestAdminStats_PublicAndCooldownEndpoints(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	anth, err := store.CreateConfig(ctx, &model.Config{
		Name:         "anth",
		URL:          "https://example.com",
		Priority:     1,
		ChannelType:  "anthropic",
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig anthropic failed: %v", err)
	}
	oai, err := store.CreateConfig(ctx, &model.Config{
		Name:         "oai",
		URL:          "https://example.com",
		Priority:     1,
		ChannelType:  "openai",
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig openai failed: %v", err)
	}

	now := time.Now()
	logs := []*model.LogEntry{
		{
			Time:                     model.JSONTime{Time: now},
			Model:                    "m1",
			ChannelID:                anth.ID,
			StatusCode:               200,
			Message:                  "ok",
			Duration:                 0.1,
			IsStreaming:              true,
			FirstByteTime:            0.01,
			InputTokens:              10,
			OutputTokens:             20,
			CacheReadInputTokens:     3,
			Cache5mInputTokens:       1,
			Cache1hInputTokens:       2,
			CacheCreationInputTokens: 3, // 兼容字段：确保统计链路覆盖
			Cost:                     0.01,
		},
		{
			Time:                 model.JSONTime{Time: now},
			Model:                "m1",
			ChannelID:            oai.ID,
			StatusCode:           500,
			Message:              "fail",
			Duration:             0.2,
			IsStreaming:          false,
			InputTokens:          7,
			OutputTokens:         8,
			CacheReadInputTokens: 99, // openai 类型不应计入缓存统计
			Cost:                 0.02,
		},
	}
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	t.Run("getChannelTypesMapCached respects TTL", func(t *testing.T) {
		m1, err := server.getChannelTypesMapCached(ctx)
		if err != nil {
			t.Fatalf("getChannelTypesMapCached failed: %v", err)
		}
		if m1[anth.ID] != "anthropic" || m1[oai.ID] != "openai" {
			t.Fatalf("unexpected types: %#v", m1)
		}

		cfg, err := store.GetConfig(ctx, anth.ID)
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		cfg.ChannelType = "codex"
		if _, err := store.UpdateConfig(ctx, anth.ID, cfg); err != nil {
			t.Fatalf("UpdateConfig failed: %v", err)
		}

		// TTL 未过期，应该返回旧值（缓存命中）
		m2, err := server.getChannelTypesMapCached(ctx)
		if err != nil {
			t.Fatalf("getChannelTypesMapCached failed: %v", err)
		}
		if m2[anth.ID] != "anthropic" {
			t.Fatalf("expected cached type anthropic, got %q", m2[anth.ID])
		}

		// 手动让缓存过期，强制刷新
		server.channelTypesCacheTime = time.Now().Add(-2 * channelTypesCacheTTL)
		m3, err := server.getChannelTypesMapCached(ctx)
		if err != nil {
			t.Fatalf("getChannelTypesMapCached failed: %v", err)
		}
		if m3[anth.ID] != "codex" {
			t.Fatalf("expected refreshed type codex, got %q", m3[anth.ID])
		}
	})

	t.Run("HandlePublicSummary", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/public/summary?range=today", nil))

		server.HandlePublicSummary(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				TotalRequests   int                    `json:"total_requests"`
				SuccessRequests int                    `json:"success_requests"`
				ErrorRequests   int                    `json:"error_requests"`
				ByType          map[string]TypeSummary `json:"by_type"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success {
			t.Fatalf("expected success=true, body=%s", w.Body.String())
		}
		if resp.Data.TotalRequests != 2 || resp.Data.SuccessRequests != 1 || resp.Data.ErrorRequests != 1 {
			t.Fatalf("unexpected totals: %+v", resp.Data)
		}

		typKey := "anthropic"
		if _, ok := resp.Data.ByType["codex"]; ok {
			typKey = "codex"
		}
		anthTS, ok := resp.Data.ByType[typKey]
		if !ok {
			t.Fatalf("expected %s in by_type: %#v", typKey, resp.Data.ByType)
		}
		if anthTS.TotalRequests != 1 || anthTS.SuccessRequests != 1 || anthTS.ErrorRequests != 0 {
			t.Fatalf("unexpected anthropic summary: %+v", anthTS)
		}
		if anthTS.TotalInputTokens != 10 || anthTS.TotalOutputTokens != 20 {
			t.Fatalf("unexpected anthropic tokens: %+v", anthTS)
		}
		if anthTS.TotalCacheReadTokens != 3 || anthTS.TotalCacheCreationTokens == 0 {
			t.Fatalf("unexpected anthropic cache: %+v", anthTS)
		}

		oaiTS, ok := resp.Data.ByType["openai"]
		if !ok {
			t.Fatalf("expected openai in by_type: %#v", resp.Data.ByType)
		}
		if oaiTS.TotalRequests != 1 || oaiTS.SuccessRequests != 0 || oaiTS.ErrorRequests != 1 {
			t.Fatalf("unexpected openai summary: %+v", oaiTS)
		}
		if oaiTS.TotalInputTokens != 7 || oaiTS.TotalOutputTokens != 8 {
			t.Fatalf("unexpected openai tokens: %+v", oaiTS)
		}
		if oaiTS.TotalCacheReadTokens != 0 {
			t.Fatalf("expected openai cache tokens excluded, got %+v", oaiTS)
		}
	})

	t.Run("HandleCooldownStats", func(t *testing.T) {
		until := time.Now().Add(2 * time.Minute)
		if err := store.SetChannelCooldown(ctx, anth.ID, until); err != nil {
			t.Fatalf("SetChannelCooldown failed: %v", err)
		}
		// Key 冷却写在 api_keys 表上，必须先有 Key 记录
		if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: anth.ID, KeyIndex: 0, APIKey: "k0", KeyStrategy: model.KeyStrategySequential},
		}); err != nil {
			t.Fatalf("CreateAPIKeysBatch failed: %v", err)
		}
		if err := store.SetKeyCooldown(ctx, anth.ID, 0, until); err != nil {
			t.Fatalf("SetKeyCooldown failed: %v", err)
		}

		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/cooldown/stats", nil))

		server.HandleCooldownStats(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				ChannelCooldowns int `json:"channel_cooldowns"`
				KeyCooldowns     int `json:"key_cooldowns"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success || resp.Data.ChannelCooldowns != 1 || resp.Data.KeyCooldowns != 1 {
			t.Fatalf("unexpected cooldown stats: %+v", resp)
		}
	})

	t.Run("HandleGetChannelTypes", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/public/channel-types", nil))

		server.HandleGetChannelTypes(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
		}

		// 验证缓存头（编译时常量，缓存24小时）
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
			t.Fatalf("Cache-Control=%q, want %q", cc, "public, max-age=86400")
		}

		var resp struct {
			Success bool                     `json:"success"`
			Data    []util.ChannelTypeConfig `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success || len(resp.Data) == 0 {
			t.Fatalf("unexpected channel types resp: %+v", resp)
		}
	})

	t.Run("HandlePublicVersion", func(t *testing.T) {
		origVersion := version.Version
		t.Cleanup(func() { version.Version = origVersion })
		version.Version = "test-ver"

		c, w := newTestContext(t, newRequest(http.MethodGet, "/public/version", nil))

		server.HandlePublicVersion(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		// 验证缓存头（版本信息缓存5分钟）
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
			t.Fatalf("Cache-Control=%q, want %q", cc, "public, max-age=300")
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Version string `json:"version"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success {
			t.Fatalf("expected success=true, body=%s", w.Body.String())
		}
		if resp.Data.Version != "test-ver" {
			t.Fatalf("version=%v, want %q", resp.Data.Version, "test-ver")
		}
	})
}
