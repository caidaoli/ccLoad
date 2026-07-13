package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestDashboardLogsForceTokenScopeAndRemoveSensitiveFields(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	secretChannel, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "secret-channel", URL: "https://secret-upstream.example", Priority: 10,
		ChannelType: "openai", Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.6"}},
	})
	if err != nil {
		t.Fatalf("create secret channel: %v", err)
	}

	now := model.JSONTime{Time: time.Now()}
	for _, entry := range []*model.LogEntry{
		{
			Time:                 now,
			Model:                "gpt-5.6",
			ActualModel:          "gpt-5.6-2026-07-01",
			LogSource:            model.LogSourceProxy,
			ChannelID:            secretChannel.ID,
			ChannelName:          "secret-channel",
			StatusCode:           http.StatusOK,
			Message:              "upstream https://secret-upstream.example rejected sk-secret...last on secret-channel",
			AuthTokenID:          42,
			AuthTokenDescription: "owner",
			APIKeyUsed:           "sk-secret...last",
			APIKeyHash:           "secret-hash",
			ClientIP:             "10.0.0.1",
			BaseURL:              "https://secret-upstream.example",
			Cost:                 1.25,
			CostMultiplier:       0,
		},
		{
			Time:        now,
			Model:       "foreign-model",
			LogSource:   model.LogSourceProxy,
			StatusCode:  http.StatusOK,
			AuthTokenID: 99,
		},
	} {
		if err := store.AddLog(context.Background(), entry); err != nil {
			t.Fatalf("add log: %v", err)
		}
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/dashboard/logs?range=today&auth_token_id=99", nil))
	c.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleErrors(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	var response struct {
		Success bool                         `json:"success"`
		Data    []map[string]json.RawMessage `json:"data"`
		Count   int                          `json:"count"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &response)
	if !response.Success || response.Count != 1 || len(response.Data) != 1 {
		t.Fatalf("unexpected scoped response: success=%v count=%d len=%d", response.Success, response.Count, len(response.Data))
	}
	if _, ok := response.Data[0]["model"]; !ok {
		t.Fatal("safe log response missing model")
	}
	var effectiveCost float64
	if err := json.Unmarshal(response.Data[0]["effective_cost"], &effectiveCost); err != nil {
		t.Fatalf("decode effective cost: %v", err)
	}
	if effectiveCost != 0 {
		t.Fatalf("effective_cost=%v, want 0 for a free channel", effectiveCost)
	}
	var message string
	if err := json.Unmarshal(response.Data[0]["message"], &message); err != nil {
		t.Fatalf("decode safe message: %v", err)
	}
	for _, secret := range []string{"secret-upstream.example", "sk-secret", "secret-channel"} {
		if strings.Contains(message, secret) {
			t.Fatalf("safe log message exposed %q: %q", secret, message)
		}
	}
	for _, key := range []string{
		"channel_id", "channel_name", "api_key_used", "api_key_hash",
		"auth_token_id", "auth_token_description", "client_ip", "base_url",
	} {
		if _, ok := response.Data[0][key]; ok {
			t.Fatalf("safe log response exposed %q", key)
		}
	}
}

func TestDashboardModelsAndMetricsHideForeignTokensAndChannels(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	ownerChannel, err := store.CreateConfig(ctx, &model.Config{
		Name: "owner-channel", URL: "https://owner.example", Priority: 10,
		ChannelType: "openai", Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "owner-model"}},
	})
	if err != nil {
		t.Fatalf("create owner channel: %v", err)
	}
	foreignChannel, err := store.CreateConfig(ctx, &model.Config{
		Name: "foreign-channel", URL: "https://foreign.example", Priority: 10,
		ChannelType: "openai", Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "foreign-model"}},
	})
	if err != nil {
		t.Fatalf("create foreign channel: %v", err)
	}
	ownerChannel2, err := store.CreateConfig(ctx, &model.Config{
		Name: "owner-channel-2", URL: "https://owner-2.example", Priority: 5,
		ChannelType: "anthropic", Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "owner-model"}},
	})
	if err != nil {
		t.Fatalf("create second owner channel: %v", err)
	}

	now := model.JSONTime{Time: time.Now()}
	for _, entry := range []*model.LogEntry{
		{Time: now, Model: "owner-model", LogSource: model.LogSourceProxy, ChannelID: ownerChannel.ID, StatusCode: 200, AuthTokenID: 42},
		{Time: now, Model: "owner-model", LogSource: model.LogSourceProxy, ChannelID: ownerChannel2.ID, StatusCode: 200, AuthTokenID: 42},
		{Time: now, Model: "foreign-model", LogSource: model.LogSourceProxy, ChannelID: foreignChannel.ID, StatusCode: 200, AuthTokenID: 99},
	} {
		if err := store.AddLog(ctx, entry); err != nil {
			t.Fatalf("add log: %v", err)
		}
	}

	modelsCtx, modelsW := newTestContext(t, newRequest(http.MethodGet, "/dashboard/models?range=today", nil))
	modelsCtx.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleGetModels(modelsCtx)
	models := mustParseAPIResponse[ModelsChannelsResponse](t, modelsW.Body.Bytes()).Data
	if len(models.Models) != 1 || models.Models[0] != "owner-model" {
		t.Fatalf("models=%v, want [owner-model]", models.Models)
	}
	if len(models.Channels) != 0 {
		t.Fatalf("channels=%v, want hidden", models.Channels)
	}

	metricsCtx, metricsW := newTestContext(t, newRequest(http.MethodGet, "/dashboard/metrics?range=today&bucket_min=5", nil))
	metricsCtx.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleMetrics(metricsCtx)
	metrics := mustParseAPIResponse[[]model.MetricPoint](t, metricsW.Body.Bytes()).Data
	if len(metrics) == 0 {
		t.Fatal("expected owner metric point")
	}
	for _, point := range metrics {
		if len(point.Channels) != 0 {
			t.Fatalf("metric point exposed channels: %v", point.Channels)
		}
	}

	statsCtx, statsW := newTestContext(t, newRequest(http.MethodGet, "/dashboard/stats?range=today", nil))
	statsCtx.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleStats(statsCtx)
	statsData := mustParseAPIResponse[struct {
		Stats []model.StatsEntry `json:"stats"`
	}](t, statsW.Body.Bytes()).Data
	if len(statsData.Stats) != 1 {
		t.Fatalf("stats entries=%d, want one model aggregate", len(statsData.Stats))
	}
	if statsData.Stats[0].ChannelID != nil || statsData.Stats[0].ChannelName != "" || statsData.Stats[0].ChannelType != "" {
		t.Fatalf("stats exposed channel identity: %+v", statsData.Stats[0])
	}

	summaryCtx, summaryW := newTestContext(t, newRequest(http.MethodGet, "/dashboard/summary?range=today", nil))
	summaryCtx.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandlePublicSummary(summaryCtx)
	summary := mustParseAPIResponse[struct {
		TotalRequests int `json:"total_requests"`
	}](t, summaryW.Body.Bytes()).Data
	if summary.TotalRequests != 2 {
		t.Fatalf("summary total=%d, want owner total 2", summary.TotalRequests)
	}

	bootstrapCtx, bootstrapW := newTestContext(t, newRequest(http.MethodGet, "/dashboard/logs/bootstrap?range=today", nil))
	bootstrapCtx.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleLogsBootstrap(bootstrapCtx)
	bootstrap := mustParseAPIResponse[struct {
		Models     []string              `json:"models"`
		Channels   []model.ChannelNameID `json:"channels"`
		AuthTokens []json.RawMessage     `json:"auth_tokens"`
	}](t, bootstrapW.Body.Bytes()).Data
	if len(bootstrap.Models) != 1 || bootstrap.Models[0] != "owner-model" {
		t.Fatalf("bootstrap models=%v, want [owner-model]", bootstrap.Models)
	}
	if len(bootstrap.Channels) != 0 || len(bootstrap.AuthTokens) != 0 {
		t.Fatalf("bootstrap exposed channels/tokens: channels=%v tokens=%d", bootstrap.Channels, len(bootstrap.AuthTokens))
	}
}
