package app

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestDashboardLogsForceTokenScopeAndExposeSafeChannelFields(t *testing.T) {
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
	entry := response.Data[0]
	assertJSONNumber(t, entry, "channel_id", float64(secretChannel.ID))
	assertJSONString(t, entry, "channel_name", "secret-channel")
	assertJSONString(t, entry, "channel_type", "openai")
	assertJSONString(t, entry, "log_source", model.LogSourceProxy)
	assertJSONString(t, entry, "model", "gpt-5.6")
	var effectiveCost float64
	if err := json.Unmarshal(entry["effective_cost"], &effectiveCost); err != nil {
		t.Fatalf("decode effective cost: %v", err)
	}
	if effectiveCost != 0 {
		t.Fatalf("effective_cost=%v, want 0 for a free channel", effectiveCost)
	}
	var message string
	if err := json.Unmarshal(entry["message"], &message); err != nil {
		t.Fatalf("decode safe message: %v", err)
	}
	for _, secret := range []string{"secret-upstream.example", "sk-secret", "secret-channel"} {
		if strings.Contains(message, secret) {
			t.Fatalf("safe log message exposed %q: %q", secret, message)
		}
	}
	for _, key := range []string{"api_key_used", "api_key_hash", "auth_token_id", "client_ip", "base_url"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("safe log response exposed %q", key)
		}
	}
}

func TestDashboardModelsMetricsAndStatsExposeOnlyScopedChannels(t *testing.T) {
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
	wantChannels := map[int64]string{ownerChannel.ID: "owner-channel", ownerChannel2.ID: "owner-channel-2"}
	if got := channelNameMap(models.Channels); !reflect.DeepEqual(got, wantChannels) {
		t.Fatalf("models channels=%v, want %v", got, wantChannels)
	}

	metricsCtx, metricsW := newTestContext(t, newRequest(http.MethodGet, "/dashboard/metrics?range=today&bucket_min=5", nil))
	metricsCtx.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleMetrics(metricsCtx)
	metrics := mustParseAPIResponse[[]model.MetricPoint](t, metricsW.Body.Bytes()).Data
	if len(metrics) == 0 {
		t.Fatal("expected owner metric point")
	}
	for _, point := range metrics {
		if _, leaked := point.Channels["foreign-channel"]; leaked {
			t.Fatalf("metrics exposed foreign channel: %v", point.Channels)
		}
	}

	statsCtx, statsW := newTestContext(t, newRequest(http.MethodGet, "/dashboard/stats?range=today", nil))
	statsCtx.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleStats(statsCtx)
	statsData := mustParseAPIResponse[struct {
		Stats []model.StatsEntry `json:"stats"`
	}](t, statsW.Body.Bytes()).Data
	if got := statsChannelNameMap(statsData.Stats); !reflect.DeepEqual(got, wantChannels) {
		t.Fatalf("stats channels=%v, want %v", got, wantChannels)
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
	if got := channelNameMap(bootstrap.Channels); !reflect.DeepEqual(got, wantChannels) {
		t.Fatalf("bootstrap channels=%v, want %v", got, wantChannels)
	}
	if len(bootstrap.AuthTokens) != 0 {
		t.Fatalf("bootstrap auth tokens=%d, want 0", len(bootstrap.AuthTokens))
	}
}

func assertJSONNumber(t testing.TB, entry map[string]json.RawMessage, key string, want float64) {
	t.Helper()
	var got float64
	if err := json.Unmarshal(entry[key], &got); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s=%v, want %v", key, got, want)
	}
}

func assertJSONString(t testing.TB, entry map[string]json.RawMessage, key, want string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(entry[key], &got); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s=%q, want %q", key, got, want)
	}
}

func channelNameMap(channels []model.ChannelNameID) map[int64]string {
	result := make(map[int64]string, len(channels))
	for _, channel := range channels {
		result[channel.ID] = channel.Name
	}
	return result
}

func statsChannelNameMap(stats []model.StatsEntry) map[int64]string {
	result := make(map[int64]string, len(stats))
	for _, entry := range stats {
		if entry.ChannelID != nil {
			result[int64(*entry.ChannelID)] = entry.ChannelName
		}
	}
	return result
}
