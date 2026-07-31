package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/version"
)

func TestAdminStats_PublicAndCooldownEndpoints(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	anth, err := store.CreateConfig(ctx, &model.Config{
		Name:         "anth",
		URLs:         model.ChannelURLs{{URL: "https://example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig anthropic failed: %v", err)
	}
	oai, err := store.CreateConfig(ctx, &model.Config{
		Name:         "oai",
		URLs:         model.ChannelURLs{{URL: "https://example.com"}},
		Priority:     1,
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
			ClientProtocol:           "anthropic",
			LogSource:                model.LogSourceProxy,
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
			ClientProtocol:       "openai",
			LogSource:            model.LogSourceProxy,
			StatusCode:           500,
			Message:              "fail",
			Duration:             0.2,
			IsStreaming:          false,
			InputTokens:          7,
			OutputTokens:         8,
			CacheReadInputTokens: 99, // openai 类型不应计入缓存统计
			Cost:                 0.02,
		},
		{
			Time:           model.JSONTime{Time: now},
			Model:          "m1",
			ChannelID:      anth.ID,
			ClientProtocol: "codex",
			LogSource:      model.LogSourceScheduledCheck,
			StatusCode:     200,
			Message:        "scheduled ok",
			Duration:       0.05,
			InputTokens:    50,
			OutputTokens:   60,
			Cost:           0.5,
		},
		{
			Time:           model.JSONTime{Time: now},
			Model:          "m1",
			ChannelID:      anth.ID,
			LogSource:      model.LogSourceProxy,
			ClientProtocol: "",
			StatusCode:     201,
			Message:        "legacy protocol unknown",
			Duration:       0.05,
			InputTokens:    5,
			OutputTokens:   6,
			Cost:           0.005,
		},
	}
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	t.Run("HandlePublicSummary", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/public/summary?range=today", nil))

		server.HandlePublicSummary(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				TotalRequests    int                                  `json:"total_requests"`
				SuccessRequests  int                                  `json:"success_requests"`
				ErrorRequests    int                                  `json:"error_requests"`
				ByClientProtocol map[string]model.ClientProtocolStats `json:"by_client_protocol"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success {
			t.Fatalf("expected success=true, body=%s", w.Body.String())
		}
		if resp.Data.TotalRequests != 3 || resp.Data.SuccessRequests != 2 || resp.Data.ErrorRequests != 1 {
			t.Fatalf("unexpected totals: %+v", resp.Data)
		}

		anthTS, ok := resp.Data.ByClientProtocol["anthropic"]
		if !ok {
			t.Fatalf("expected anthropic in by_client_protocol: %#v", resp.Data.ByClientProtocol)
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

		oaiTS, ok := resp.Data.ByClientProtocol["openai"]
		if !ok {
			t.Fatalf("expected openai in by_client_protocol: %#v", resp.Data.ByClientProtocol)
		}
		if oaiTS.TotalRequests != 1 || oaiTS.SuccessRequests != 0 || oaiTS.ErrorRequests != 1 {
			t.Fatalf("unexpected openai summary: %+v", oaiTS)
		}
		if oaiTS.TotalInputTokens != 7 || oaiTS.TotalOutputTokens != 8 {
			t.Fatalf("unexpected openai tokens: %+v", oaiTS)
		}
		if oaiTS.TotalCacheReadTokens != 99 {
			t.Fatalf("expected normalized openai cache tokens, got %+v", oaiTS)
		}
		if _, ok := resp.Data.ByClientProtocol[""]; ok {
			t.Fatalf("historical unknown protocol must not be exposed as a card: %#v", resp.Data.ByClientProtocol)
		}
		if _, ok := resp.Data.ByClientProtocol["codex"]; ok {
			t.Fatalf("scheduled checks must not enter client protocol cards: %#v", resp.Data.ByClientProtocol)
		}
	})

	t.Run("HandleGetProtocols", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/public/protocols", nil))

		server.HandleGetProtocols(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
		}

		// 验证缓存头（编译时常量，缓存24小时）
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
			t.Fatalf("Cache-Control=%q, want %q", cc, "public, max-age=86400")
		}

		var resp struct {
			Success bool             `json:"success"`
			Data    []map[string]any `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success || len(resp.Data) == 0 {
			t.Fatalf("unexpected protocols resp: %+v", resp)
		}
		for _, upstreamProtocol := range resp.Data {
			for _, field := range []string{"value", "display_name", "description"} {
				value, ok := upstreamProtocol[field].(string)
				if !ok || value == "" {
					t.Fatalf("protocol missing %s: %#v", field, upstreamProtocol)
				}
			}
			if _, ok := upstreamProtocol["path_patterns"]; ok {
				t.Fatalf("protocol leaked legacy path_patterns: %#v", upstreamProtocol)
			}
			if _, ok := upstreamProtocol["match_type"]; ok {
				t.Fatalf("protocol leaked legacy match_type: %#v", upstreamProtocol)
			}
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
