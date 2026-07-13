package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

type tokenLogMetadataErrorStore struct {
	storage.Store
	err error
}

func (s tokenLogMetadataErrorStore) GetAllAPIKeys(context.Context) (map[int64][]*model.APIKey, error) {
	return nil, s.err
}

func TestDashboardLogsForceTokenScopeAndExposeSafeChannelFields(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	ctx := context.Background()
	secretChannel, err := store.CreateConfig(ctx, &model.Config{
		Name: "secret-channel", URL: "https://secret-upstream.example", Priority: 10,
		ChannelType: "openai", Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.6"}},
	})
	if err != nil {
		t.Fatalf("create secret channel: %v", err)
	}
	const googleKey = "AIzaSyD_ActualGoogleChannelSecret1234567890"
	const relayKey = "relayOpaqueSecretValue1234567890"
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: secretChannel.ID, KeyIndex: 0, APIKey: googleKey},
		{ChannelID: secretChannel.ID, KeyIndex: 1, APIKey: relayKey},
	}); err != nil {
		t.Fatalf("create channel keys: %v", err)
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
			Message:              "upstream https://secret-upstream.example rejected " + googleKey + " and " + relayKey + " on secret-channel",
			AuthTokenID:          42,
			AuthTokenDescription: "owner",
			APIKeyUsed:           googleKey,
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
		if err := store.AddLog(ctx, entry); err != nil {
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
	for _, secret := range []string{"secret-upstream.example", googleKey, relayKey, "secret-channel"} {
		if strings.Contains(message, secret) {
			t.Fatalf("safe log message exposed %q: %q", secret, message)
		}
	}
	if !strings.Contains(message, "rejected") {
		t.Fatalf("safe log message removed non-sensitive diagnostics: %q", message)
	}
	for _, key := range []string{"api_key_used", "api_key_hash", "auth_token_id", "client_ip", "base_url"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("safe log response exposed %q", key)
		}
	}
}

func TestDashboardLogsFailClosedWhenPersistedKeyNoLongerExists(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	channel, err := store.CreateConfig(ctx, &model.Config{
		Name:         "rotated-key-channel",
		URL:          "https://rotated-key.example",
		Priority:     10,
		ChannelType:  "openai",
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.6"}},
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	const retiredKey = "retiredOpaqueSecretValue1234567890"
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: channel.ID,
		KeyIndex:  0,
		APIKey:    retiredKey,
	}}); err != nil {
		t.Fatalf("create channel key: %v", err)
	}
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:           model.JSONTime{Time: time.Now()},
		Model:          "gpt-5.6",
		ChannelID:      channel.ID,
		StatusCode:     http.StatusBadGateway,
		Message:        "upstream echoed " + retiredKey,
		APIKeyUsed:     retiredKey,
		AuthTokenID:    42,
		CostMultiplier: 1,
	}); err != nil {
		t.Fatalf("add log: %v", err)
	}
	if err := store.DeleteAllAPIKeys(ctx, channel.ID); err != nil {
		t.Fatalf("delete channel keys: %v", err)
	}
	const legacyKey = "legacyOpaqueSecretWithoutPersistedHash9876543210"
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:           model.JSONTime{Time: time.Now()},
		Model:          "gpt-5.6",
		ChannelID:      channel.ID,
		StatusCode:     http.StatusBadGateway,
		Message:        "legacy upstream echoed " + legacyKey,
		AuthTokenID:    42,
		CostMultiplier: 1,
	}); err != nil {
		t.Fatalf("add legacy log without key hash: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/dashboard/logs?range=today", nil))
	c.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleErrors(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	response := mustParseAPIResponse[[]struct {
		Message string `json:"message"`
	}](t, w.Body.Bytes())
	if len(response.Data) != 2 {
		t.Fatalf("logs=%d, want 2", len(response.Data))
	}
	for _, entry := range response.Data {
		if entry.Message != "[redacted]" {
			t.Fatalf("message=%q, want fail-closed redaction", entry.Message)
		}
	}
}

func TestDashboardLogsMetadataFailureReturnsServerError(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	channel, err := store.CreateConfig(ctx, &model.Config{
		Name:         "metadata-error-channel",
		URL:          "https://metadata-error.example",
		Priority:     10,
		ChannelType:  "openai",
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.6"}},
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	const secretMessage = "TOKEN_LOG_METADATA_FAILURE_SECRET"
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:           model.JSONTime{Time: time.Now()},
		Model:          "gpt-5.6",
		ChannelID:      channel.ID,
		StatusCode:     http.StatusBadGateway,
		Message:        secretMessage,
		AuthTokenID:    42,
		CostMultiplier: 1,
	}); err != nil {
		t.Fatalf("add log: %v", err)
	}
	server.store = tokenLogMetadataErrorStore{
		Store: store,
		err:   errors.New("database unavailable"),
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/dashboard/logs?range=today", nil))
	c.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleErrors(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secretMessage) {
		t.Fatalf("metadata error response exposed log message: %s", w.Body.String())
	}
}

func TestDashboardChannelsForceTokenScopeAndHideSensitiveConfig(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	ownerChannel, err := store.CreateConfig(ctx, &model.Config{
		Name:               "owner-channel",
		URL:                "https://owner-upstream.example",
		ProxyURL:           "https://owner-proxy.example",
		Priority:           10,
		ChannelType:        "openai",
		Enabled:            true,
		ModelEntries:       []model.ModelEntry{{Model: "owner-model"}},
		CostMultiplier:     1.5,
		CustomRequestRules: &model.CustomRequestRules{},
	})
	if err != nil {
		t.Fatalf("create owner channel: %v", err)
	}
	foreignChannel, err := store.CreateConfig(ctx, &model.Config{
		Name:         "foreign-channel",
		URL:          "https://foreign-upstream.example",
		Priority:     10,
		ChannelType:  "openai",
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "foreign-model"}},
	})
	if err != nil {
		t.Fatalf("create foreign channel: %v", err)
	}

	now := model.JSONTime{Time: time.Now()}
	for _, entry := range []*model.LogEntry{
		{Time: now, Model: "owner-model", LogSource: model.LogSourceProxy, ChannelID: ownerChannel.ID, StatusCode: http.StatusOK, AuthTokenID: 42},
		{Time: now, Model: "foreign-model", LogSource: model.LogSourceProxy, ChannelID: foreignChannel.ID, StatusCode: http.StatusOK, AuthTokenID: 99},
	} {
		if err := store.AddLog(ctx, entry); err != nil {
			t.Fatalf("add log: %v", err)
		}
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/dashboard/channels?range=today&auth_token_id=99", nil))
	c.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleDashboardChannels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	response := mustParseAPIResponse[[]map[string]json.RawMessage](t, w.Body.Bytes())
	if len(response.Data) != 1 {
		t.Fatalf("channels=%d, want 1", len(response.Data))
	}
	entry := response.Data[0]
	assertJSONNumber(t, entry, "id", float64(ownerChannel.ID))
	assertJSONString(t, entry, "name", "owner-channel")
	assertJSONString(t, entry, "channel_type", "openai")
	for _, key := range []string{"url", "proxy_url", "custom_request_rules", "key_strategy", "key_cooldowns"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("dashboard channel exposed %q", key)
		}
	}
}

func TestDashboardChannelFilterOptionsUseBoundToken(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	ownerChannel, err := store.CreateConfig(ctx, &model.Config{
		Name:         "owner-channel",
		URL:          "https://owner-upstream.example",
		Priority:     10,
		ChannelType:  "openai",
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "owner-model"}},
	})
	if err != nil {
		t.Fatalf("create owner channel: %v", err)
	}
	foreignChannel, err := store.CreateConfig(ctx, &model.Config{
		Name:         "foreign-channel",
		URL:          "https://foreign-upstream.example",
		Priority:     10,
		ChannelType:  "openai",
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "foreign-model"}},
	})
	if err != nil {
		t.Fatalf("create foreign channel: %v", err)
	}

	now := model.JSONTime{Time: time.Now()}
	for _, entry := range []*model.LogEntry{
		{Time: now, Model: "owner-model", LogSource: model.LogSourceProxy, ChannelID: ownerChannel.ID, StatusCode: http.StatusOK, AuthTokenID: 42},
		{Time: now, Model: "foreign-model", LogSource: model.LogSourceProxy, ChannelID: foreignChannel.ID, StatusCode: http.StatusOK, AuthTokenID: 99},
	} {
		if err := store.AddLog(ctx, entry); err != nil {
			t.Fatalf("add log: %v", err)
		}
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/dashboard/channels/filter-options?range=today&auth_token_id=99", nil))
	c.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleDashboardChannelFilterOptions(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	data := mustParseAPIResponse[struct {
		ChannelNames []string `json:"channel_names"`
		Models       []string `json:"models"`
	}](t, w.Body.Bytes()).Data
	if !reflect.DeepEqual(data.ChannelNames, []string{"owner-channel"}) {
		t.Fatalf("channel names=%v", data.ChannelNames)
	}
	if !reflect.DeepEqual(data.Models, []string{"owner-model"}) {
		t.Fatalf("models=%v", data.Models)
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
	const sensitiveLastRequestMessage = "SENSITIVE_STATS_SENTINEL https://upstream.example/v1?key=sk-stats-secret"
	for _, entry := range []*model.LogEntry{
		{Time: now, Model: "owner-model", LogSource: model.LogSourceProxy, ChannelID: ownerChannel.ID, StatusCode: 200, Message: sensitiveLastRequestMessage, AuthTokenID: 42},
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
	metricChannels := make(map[string]struct{})
	for _, point := range metrics {
		for channelName := range point.Channels {
			metricChannels[channelName] = struct{}{}
		}
		if _, leaked := point.Channels["foreign-channel"]; leaked {
			t.Fatalf("metrics exposed foreign channel: %v", point.Channels)
		}
	}
	if want := map[string]struct{}{"owner-channel": {}, "owner-channel-2": {}}; !reflect.DeepEqual(metricChannels, want) {
		t.Fatalf("metrics channels=%v, want %v", metricChannels, want)
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
	for _, entry := range statsData.Stats {
		if entry.ChannelID == nil {
			t.Fatal("token stats entry missing channel id")
		}
		if entry.ChannelName == "" {
			t.Fatalf("token stats channel %d missing name", *entry.ChannelID)
		}
		if len(entry.HealthTimeline) != 48 {
			t.Fatalf("token stats channel %d health points=%d, want 48", *entry.ChannelID, len(entry.HealthTimeline))
		}
	}
	if strings.Contains(statsW.Body.String(), sensitiveLastRequestMessage) {
		t.Fatalf("token stats exposed sensitive last request message: %s", statsW.Body.String())
	}
	for _, entry := range statsData.Stats {
		if entry.LastRequestMessage != "" {
			t.Fatalf("token stats last request message=%q, want empty", entry.LastRequestMessage)
		}
	}

	adminStatsCtx, adminStatsW := newTestContext(t, newRequest(http.MethodGet, "/admin/stats?range=today&auth_token_id=42", nil))
	server.HandleStats(adminStatsCtx)
	adminStats := mustParseAPIResponse[struct {
		Stats []model.StatsEntry `json:"stats"`
	}](t, adminStatsW.Body.Bytes()).Data.Stats
	adminOwnerStatsFound := false
	for _, entry := range adminStats {
		if entry.ChannelID != nil && int64(*entry.ChannelID) == ownerChannel.ID {
			adminOwnerStatsFound = true
			if entry.LastRequestMessage != sensitiveLastRequestMessage {
				t.Fatalf("admin stats last request message=%q, want %q", entry.LastRequestMessage, sensitiveLastRequestMessage)
			}
			break
		}
	}
	if !adminOwnerStatsFound {
		t.Fatalf("admin stats missing owner channel %d", ownerChannel.ID)
	}

	filterOptionsCtx, filterOptionsW := newTestContext(t, newRequest(http.MethodGet, "/dashboard/stats/filter-options?range=today", nil))
	filterOptionsCtx.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42})
	server.HandleStatsFilterOptions(filterOptionsCtx)
	filterOptions := mustParseAPIResponse[struct {
		ChannelNames []string `json:"channel_names"`
		Models       []string `json:"models"`
	}](t, filterOptionsW.Body.Bytes()).Data
	if got, want := stringSet(filterOptions.ChannelNames), map[string]struct{}{"owner-channel": {}, "owner-channel-2": {}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filter option channels=%v, want %v", got, want)
	}
	if got, want := stringSet(filterOptions.Models), map[string]struct{}{"owner-model": {}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filter option models=%v, want %v", got, want)
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

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
