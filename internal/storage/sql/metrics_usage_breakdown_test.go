package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestGetUsageBreakdown(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
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
	apiKeyID := createTestChannel(t, ctx, store, "plain-api")
	const deletedChannelID = 9999

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
			ChannelID:      zai.ID,
			Model:          "glm-4.6",
			ClientProtocol: "openai",
			StatusCode:     200,
			InputTokens:    1,
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
			ChannelID:      apiKeyID,
			Model:          "gpt-4",
			ClientProtocol: "openai",
			StatusCode:     200,
			InputTokens:    50,
			Cost:           0.5,
			CostMultiplier: 1,
			LogSource:      model.LogSourceProxy,
		},
		{
			Time:           model.JSONTime{Time: now},
			ChannelID:      deletedChannelID,
			Model:          "gpt-4",
			ClientProtocol: "openai",
			StatusCode:     200,
			LogSource:      model.LogSourceProxy,
		},
		{
			Time:           model.JSONTime{Time: now},
			ChannelID:      apiKeyID,
			Model:          "gpt-4",
			ClientProtocol: "openai",
			StatusCode:     200,
			LogSource:      model.LogSourceScheduledCheck,
		},
	}); err != nil {
		t.Fatalf("BatchAddLogs: %v", err)
	}

	rows, err := store.GetUsageBreakdown(ctx, start, end, &model.LogFilter{LogSource: model.LogSourceProxy})
	if err != nil {
		t.Fatalf("GetUsageBreakdown: %v", err)
	}
	type rowKey struct {
		protocol  string
		channelID int64
		model     string
	}
	byKey := map[rowKey]model.UsageBreakdownRow{}
	for _, row := range rows {
		byKey[rowKey{row.ClientProtocol, row.ChannelID, row.Model}] = row
	}
	if len(byKey) != 5 || len(rows) != 5 {
		t.Fatalf("want 5 protocol×channel×model rows, got %+v", rows)
	}

	zaiRow := byKey[rowKey{"anthropic", zai.ID, "glm-4.6"}]
	if zaiRow.ChannelName != "zai-plan" || zaiRow.AuthType != model.AuthTypeZAIOAuth {
		t.Fatalf("channel identity not joined: %+v", zaiRow)
	}
	if zaiRow.TotalRequests != 1 || zaiRow.SuccessRequests != 1 || zaiRow.ErrorRequests != 0 {
		t.Fatalf("499 must not count as a request: %+v", zaiRow)
	}
	if zaiRow.TotalInputTokens != 10 || zaiRow.TotalOutputTokens != 20 ||
		zaiRow.TotalCacheReadTokens != 3 || zaiRow.TotalCacheCreationTokens != 4 {
		t.Fatalf("unexpected zai tokens: %+v", zaiRow)
	}
	if zaiRow.TotalCost != 0.05 || zaiRow.EffectiveCost != 0.10 {
		t.Fatalf("unexpected zai cost: %+v", zaiRow)
	}
	if row := byKey[rowKey{"openai", zai.ID, "glm-4.6"}]; row.TotalRequests != 1 || row.TotalInputTokens != 1 {
		t.Fatalf("client protocols must stay separate rows: %+v", row)
	}

	xaiRow := byKey[rowKey{"openai", xai.ID, "grok-4"}]
	if xaiRow.AuthType != model.AuthTypeXAIOAuth || xaiRow.TotalRequests != 1 || xaiRow.ErrorRequests != 1 {
		t.Fatalf("unexpected xai row: %+v", xaiRow)
	}
	if row := byKey[rowKey{"openai", apiKeyID, "gpt-4"}]; row.AuthType != model.AuthTypeAPIKey || row.TotalRequests != 1 {
		t.Fatalf("scheduled checks must stay out of proxy usage: %+v", row)
	}
	deleted := byKey[rowKey{"openai", deletedChannelID, "gpt-4"}]
	if deleted.TotalRequests != 1 || deleted.ChannelName != "" || deleted.AuthType != "" {
		t.Fatalf("deleted channel must keep usage without identity: %+v", deleted)
	}
}
