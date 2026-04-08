package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
)

func createScheduledCheckChannel(t *testing.T, srv *Server, cfg *model.Config, keys ...*model.APIKey) *model.Config {
	t.Helper()

	created, err := srv.store.CreateConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	if len(keys) == 0 {
		return created
	}

	prepared := make([]*model.APIKey, 0, len(keys))
	for i, key := range keys {
		prepared = append(prepared, &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    i,
			APIKey:      key.APIKey,
			KeyStrategy: key.KeyStrategy,
		})
	}
	if err := srv.store.CreateAPIKeysBatch(context.Background(), prepared); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	return created
}

func TestNormalizeChannelCheckIntervalHours(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int
		want int
	}{
		{name: "positive_kept", in: 5, want: 5},
		{name: "zero_disables", in: 0, want: 0},
		{name: "negative_clamped_to_zero", in: -3, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeChannelCheckIntervalHours(tt.in); got != tt.want {
				t.Fatalf("normalizeChannelCheckIntervalHours(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestExecuteChannelTest_SuccessResetsCooldowns(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	created := createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "scheduled-success",
		URL:                   upstream.URL,
		ChannelType:           "openai",
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	}, &model.APIKey{APIKey: "sk-success", KeyStrategy: model.KeyStrategySequential})

	coolUntil := time.Now().Add(5 * time.Minute)
	if err := srv.store.SetChannelCooldown(ctx, created.ID, coolUntil); err != nil {
		t.Fatalf("SetChannelCooldown failed: %v", err)
	}
	if err := srv.store.SetKeyCooldown(ctx, created.ID, 0, coolUntil); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	result := srv.executeChannelTest(ctx, created, 0, "sk-success", &testRequestOpenAI)
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("expected success result, got %+v", result)
	}

	channelCooldowns, err := srv.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns failed: %v", err)
	}
	if until, ok := channelCooldowns[created.ID]; ok && until.After(time.Now()) {
		t.Fatalf("expected channel cooldown cleared, got %v", until)
	}

	apiKey, err := srv.store.GetAPIKey(ctx, created.ID, 0)
	if err != nil {
		t.Fatalf("GetAPIKey failed: %v", err)
	}
	if apiKey.CooldownUntil != 0 {
		t.Fatalf("expected key cooldown cleared, got %d", apiKey.CooldownUntil)
	}
	if got, _ := result["message"].(string); got == "" {
		t.Fatalf("expected success message, got %+v", result)
	}
}

func TestExecuteChannelTest_FailureAppliesCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"upstream failed"}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	created := createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "scheduled-failure",
		URL:                   upstream.URL,
		ChannelType:           "openai",
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	}, &model.APIKey{APIKey: "sk-failure", KeyStrategy: model.KeyStrategySequential})

	result := srv.executeChannelTest(ctx, created, 0, "sk-failure", &testRequestOpenAI)
	if success, _ := result["success"].(bool); success {
		t.Fatalf("expected failed result, got %+v", result)
	}
	if got, _ := result["cooldown_action"].(string); got != "channel_cooldown_applied" {
		t.Fatalf("expected channel cooldown action, got %+v", result)
	}

	channelCooldowns, err := srv.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns failed: %v", err)
	}
	until, ok := channelCooldowns[created.ID]
	if !ok || !until.After(time.Now()) {
		t.Fatalf("expected channel cooldown applied, got %v", until)
	}
}

var testRequestOpenAI = testutil.TestChannelRequest{
	Model:       "gpt-4o-mini",
	ChannelType: "openai",
	Content:     "hello",
}

func TestRunScheduledChannelChecks_UsesScheduledCheckModelAndAvailableKey(t *testing.T) {
	var (
		eligibleCalls int
		eligibleModel string
		eligibleAuth  string
		disabledCalls int
	)

	eligibleUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eligibleCalls++
		eligibleAuth = r.Header.Get("Authorization")

		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		eligibleModel = payload.Model

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer eligibleUpstream.Close()

	disabledUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		disabledCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer disabledUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	eligible := createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "eligible-channel",
		URL:                   eligibleUpstream.URL,
		ChannelType:           "openai",
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ScheduledCheckModel:   "gpt-4.1",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o-mini"},
			{Model: "gpt-4.1"},
		},
	},
		&model.APIKey{APIKey: "sk-cooled", KeyStrategy: model.KeyStrategyRoundRobin},
		&model.APIKey{APIKey: "sk-available", KeyStrategy: model.KeyStrategyRoundRobin},
	)

	if err := srv.store.SetKeyCooldown(ctx, eligible.ID, 0, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "disabled-channel",
		URL:                   disabledUpstream.URL,
		ChannelType:           "openai",
		Enabled:               false,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	}, &model.APIKey{APIKey: "sk-disabled", KeyStrategy: model.KeyStrategySequential})

	if err := srv.runScheduledChannelChecks(ctx); err != nil {
		t.Fatalf("runScheduledChannelChecks failed: %v", err)
	}

	if eligibleCalls != 1 {
		t.Fatalf("expected eligible channel tested once, got %d", eligibleCalls)
	}
	if disabledCalls != 0 {
		t.Fatalf("expected disabled channel skipped, got %d calls", disabledCalls)
	}
	if eligibleModel != "gpt-4.1" {
		t.Fatalf("expected scheduled check model used, got %q", eligibleModel)
	}
	if eligibleAuth != "Bearer sk-available" {
		t.Fatalf("expected available key selected, got %q", eligibleAuth)
	}
}

func TestRunScheduledChannelChecks_WritesScheduledCheckLogsForRunAndSkip(t *testing.T) {
	called := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Minute)

	createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "scheduled-log-success",
		URL:                   upstream.URL,
		ChannelType:           "openai",
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	}, &model.APIKey{APIKey: "sk-success", KeyStrategy: model.KeyStrategySequential})

	createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "scheduled-log-skip",
		URL:                   upstream.URL,
		ChannelType:           "openai",
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	})

	if err := srv.runScheduledChannelChecks(ctx); err != nil {
		t.Fatalf("runScheduledChannelChecks failed: %v", err)
	}

	logs, err := srv.store.ListLogs(ctx, now, 20, 0, &model.LogFilter{LogSource: model.LogSourceScheduledCheck})
	if err != nil {
		t.Fatalf("ListLogs failed: %v", err)
	}
	if called != 1 {
		t.Fatalf("expected one upstream call, got %d", called)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 scheduled check logs, got %d", len(logs))
	}

	var successLog, skipLog *model.LogEntry
	for _, entry := range logs {
		switch entry.StatusCode {
		case http.StatusOK:
			successLog = entry
		case 0:
			skipLog = entry
		}
	}
	if successLog == nil {
		t.Fatal("expected scheduled check success log")
	}
	if successLog.LogSource != model.LogSourceScheduledCheck {
		t.Fatalf("success log source = %q, want %q", successLog.LogSource, model.LogSourceScheduledCheck)
	}
	if skipLog == nil {
		t.Fatal("expected scheduled check skip log")
	}
	if skipLog.Message == "" {
		t.Fatal("expected skip log message")
	}
}

func TestRunScheduledChannelChecks_SkipsChannelsWithoutRunnableKey(t *testing.T) {
	called := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	created := createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "all-keys-cooldown",
		URL:                   upstream.URL,
		ChannelType:           "openai",
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	}, &model.APIKey{APIKey: "sk-only", KeyStrategy: model.KeyStrategySequential})

	if err := srv.store.SetKeyCooldown(ctx, created.ID, 0, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	if err := srv.runScheduledChannelChecks(ctx); err != nil {
		t.Fatalf("runScheduledChannelChecks failed: %v", err)
	}
	if called != 0 {
		t.Fatalf("expected no upstream call when all keys cooled down, got %d", called)
	}
}

func TestTriggerScheduledChannelChecks_SkipsReentry(t *testing.T) {
	srv := newInMemoryServer(t)
	srv.scheduledChannelChecksRunning.Store(true)

	if started := srv.triggerScheduledChannelChecks(); started {
		t.Fatal("expected triggerScheduledChannelChecks to skip when previous run is active")
	}
}
