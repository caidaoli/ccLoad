package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestGetAuthTypeStats(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, "metrics_auth_type.db")
	ctx := context.Background()

	zai, err := store.CreateConfig(ctx, &model.Config{
		Name:            "zai-plan",
		URLs:            model.ChannelURLs{{URL: "https://zcode.z.ai/api/v1/ultra-zai/anthropic"}},
		Priority:        1,
		Enabled:         true,
		AuthType:        model.AuthTypeZAIOAuth,
		OAuthCredential: `{"type":"zai","api_key":"id.secret"}`,
		ModelEntries:    []model.ModelEntry{{Model: "glm-4.6"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig zai: %v", err)
	}
	xai, err := store.CreateConfig(ctx, &model.Config{
		Name:            "grok-oauth",
		URLs:            model.ChannelURLs{{URL: "https://api.x.ai/v1"}},
		Priority:        1,
		Enabled:         true,
		AuthType:        model.AuthTypeXAIOAuth,
		OAuthCredential: `{"type":"xai","access_token":"at"}`,
		ModelEntries:    []model.ModelEntry{{Model: "grok-4"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig xai: %v", err)
	}
	cursor, err := store.CreateConfig(ctx, &model.Config{
		Name:            "cursor-cli",
		URLs:            model.ChannelURLs{{URL: "https://api.cursor.com"}},
		Priority:        1,
		Enabled:         true,
		AuthType:        model.AuthTypeCursorOAuth,
		OAuthCredential: `{"type":"cursor","access_token":"at"}`,
		ModelEntries:    []model.ModelEntry{{Model: "cursor-small"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig cursor: %v", err)
	}
	apiKeyID := createTestChannel(t, ctx, store, "plain-api")

	now := time.Now()
	start := now.Add(-time.Minute)
	end := now.Add(time.Minute)
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{
			Time:                     model.JSONTime{Time: now},
			ChannelID:                zai.ID,
			Model:                    "glm-4.6",
			ClientProtocol:           "anthropic",
			StatusCode:               200,
			InputTokens:              10,
			OutputTokens:             20,
			CacheReadInputTokens:     3,
			CacheCreationInputTokens: 4,
			Cost:                     0.05,
			CostMultiplier:           2,
			LogSource:                model.LogSourceProxy,
		},
		{
			Time:           model.JSONTime{Time: now},
			ChannelID:      zai.ID,
			Model:          "glm-4.6",
			ClientProtocol: "anthropic",
			StatusCode:     499,
			LogSource:      model.LogSourceProxy,
		},
		{
			Time:           model.JSONTime{Time: now},
			ChannelID:      xai.ID,
			Model:          "grok-4",
			ClientProtocol: "openai",
			StatusCode:     500,
			InputTokens:    6,
			OutputTokens:   7,
			Cost:           0.01,
			CostMultiplier: 1,
			LogSource:      model.LogSourceProxy,
		},
		{
			Time:           model.JSONTime{Time: now},
			ChannelID:      cursor.ID,
			Model:          "cursor-small",
			ClientProtocol: "anthropic",
			StatusCode:     200,
			InputTokens:    4,
			OutputTokens:   5,
			Cost:           0.03,
			CostMultiplier: 1,
			LogSource:      model.LogSourceProxy,
		},
		{
			Time:           model.JSONTime{Time: now},
			ChannelID:      apiKeyID,
			Model:          "gpt-4",
			ClientProtocol: "openai",
			StatusCode:     200,
			InputTokens:    50,
			Cost:           0.5,
			CostMultiplier: 1,
			LogSource:      model.LogSourceProxy,
		},
	}); err != nil {
		t.Fatalf("BatchAddLogs: %v", err)
	}

	stats, err := store.GetAuthTypeStats(ctx, start, end, nil)
	if err != nil {
		t.Fatalf("GetAuthTypeStats: %v", err)
	}
	byType := map[string]model.AuthTypeStats{}
	for _, entry := range stats {
		byType[entry.AuthType] = entry
	}
	zaiTS, ok := byType[model.AuthTypeZAIOAuth]
	if !ok {
		t.Fatalf("missing zai_oauth: %#v", byType)
	}
	if zaiTS.TotalRequests != 1 || zaiTS.SuccessRequests != 1 || zaiTS.ErrorRequests != 0 {
		t.Fatalf("499 must not count on zai card: %+v", zaiTS)
	}
	if zaiTS.TotalInputTokens != 10 || zaiTS.TotalCacheCreationTokens != 4 {
		t.Fatalf("unexpected zai tokens: %+v", zaiTS)
	}
	if zaiTS.TotalCost != 0.05 || zaiTS.EffectiveCost != 0.10 {
		t.Fatalf("unexpected zai cost: %+v", zaiTS)
	}

	xaiTS, ok := byType[model.AuthTypeXAIOAuth]
	if !ok {
		t.Fatalf("missing xai_oauth: %#v", byType)
	}
	if xaiTS.TotalRequests != 1 || xaiTS.SuccessRequests != 0 || xaiTS.ErrorRequests != 1 {
		t.Fatalf("unexpected xai summary: %+v", xaiTS)
	}

	cursorTS, ok := byType[model.AuthTypeCursorOAuth]
	if !ok {
		t.Fatalf("missing cursor_oauth: %#v", byType)
	}
	if cursorTS.TotalRequests != 1 || cursorTS.SuccessRequests != 1 || cursorTS.TotalInputTokens != 4 {
		t.Fatalf("unexpected cursor summary: %+v", cursorTS)
	}
	if _, ok := byType[model.AuthTypeAPIKey]; ok {
		t.Fatalf("api_key must not appear in auth type overview: %#v", byType)
	}
}
