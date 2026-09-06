package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
)

func createScheduledCheckChannel(t *testing.T, srv *Server, cfg *model.Config, keys ...*model.APIKey) *model.Config {
	t.Helper()

	if cfg.ScheduledCheckIntervalMinutes == 0 {
		cfg.ScheduledCheckIntervalMinutes = 1
	}
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

func TestDailyScheduledChecksIndependentChannelsAndChanges(t *testing.T) {
	var slowCalls, fastCalls atomic.Int32
	started := make(chan string, 10)
	release := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/slow") {
			slowCalls.Add(1)
			started <- "slow"
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		} else {
			fastCalls.Add(1)
			started <- "fast"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	srv := newInMemoryServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fast *model.Config
	for _, name := range []string{"slow", "fast"} {
		cfg := createScheduledCheckChannel(t, srv, &model.Config{
			Name: name, URLs: model.ChannelURLs{{URL: upstream.URL + "/" + name}}, Enabled: true,
			ScheduledCheckEnabled: true, ScheduledCheckIntervalMinutes: 360, ScheduledCheckStartTime: "08:30",
			ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		}, &model.APIKey{APIKey: "sk-test"})
		if name == "fast" {
			fast = cfg
		}
	}
	now := time.Now().AddDate(1, 0, 0)
	now = time.Date(now.Year(), now.Month(), now.Day(), 8, 30, 0, 0, time.Local)
	done := make(chan error, 1)
	go func() { done <- srv.runScheduledChannelChecks(ctx, now) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("slow channel blocked another channel")
		}
	}
	// Fast detection must finish before the next simulated tick.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		if _, running := srv.scheduledChannelChecksRunning.Load(fast.ID); !running {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("fast detection did not finish")
		case <-time.After(time.Millisecond):
		}
	}
	if err := srv.runScheduledChannelChecks(ctx, now.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if slowCalls.Load() != 1 || fastCalls.Load() != 2 {
		t.Fatalf("overlap or blocking: slow=%d fast=%d", slowCalls.Load(), fastCalls.Load())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := srv.runScheduledChannelChecks(ctx, now.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if slowCalls.Load() != 1 || fastCalls.Load() != 2 {
		t.Fatal("missed schedules were replayed")
	}
	fast.ScheduledCheckStartTime, fast.ScheduledCheckIntervalMinutes = "15:30", 120
	if _, err := srv.store.UpdateConfig(ctx, fast.ID, fast); err != nil {
		t.Fatal(err)
	}
	if err := srv.runScheduledChannelChecks(ctx, now.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if fastCalls.Load() != 3 {
		t.Fatal("updated schedule did not take effect")
	}
}

func TestExecuteChannelTest_SuccessResetsCooldowns(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	created := createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "scheduled-success",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
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
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"upstream failed"}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	created := createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "scheduled-failure",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
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
	Model:          "gpt-4o-mini",
	ClientProtocol: "openai",
	Content:        "hello",
}

func TestRunScheduledChannelChecks_CodexOAuthWithoutAPIKeys(t *testing.T) {
	for _, tc := range []struct {
		name         string
		expired      bool
		invalid      bool
		refreshFails bool
		wantSkip     string
	}{
		{name: "current credential"},
		{name: "expired credential", expired: true},
		{name: "invalid credential", invalid: true, wantSkip: "加载 Codex OAuth 凭证失败"},
		{name: "refresh failure", expired: true, refreshFails: true, wantSkip: "加载 Codex OAuth 凭证失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var inferenceCalls, refreshCalls atomic.Int32
			wantToken := "at-scheduled-current"
			if tc.expired {
				wantToken = "at-scheduled-refreshed"
			}
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshCalls.Add(1)
					if err := r.ParseForm(); err != nil {
						t.Errorf("parse refresh request: %v", err)
					}
					if r.Form.Get("refresh_token") != "rt-scheduled-current" {
						t.Errorf("unexpected refresh token: %q", r.Form.Get("refresh_token"))
					}
					w.Header().Set("Content-Type", "application/json")
					if tc.refreshFails {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
						return
					}
					_, _ = io.WriteString(w, `{"access_token":"at-scheduled-refreshed","refresh_token":"rt-scheduled-rotated","expires_in":3600}`)
					return
				}
				inferenceCalls.Add(1)
				if r.URL.Path != "/backend-api/codex/responses" {
					t.Errorf("unexpected inference path: %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
					t.Errorf("Authorization = %q, want Bearer %s", got, wantToken)
				}
				if got := r.Header.Get("Chatgpt-Account-Id"); got != "account-scheduled" {
					t.Errorf("account ID = %q", got)
				}
				var body struct {
					Model  string `json:"model"`
					Stream bool   `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode inference request: %v", err)
				}
				if body.Model != "gpt-5.6-luna" || !body.Stream {
					t.Errorf("unexpected Codex request: %+v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
				_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_scheduled\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n")
			}))
			srv := newInMemoryServer(t)
			srv.codexCredentials.service.TokenURL = upstream.URL + "/oauth/token"
			ctx := context.Background()
			expires := time.Now().Add(time.Hour)
			if tc.expired {
				expires = time.Now().Add(-time.Hour)
			}
			credentialJSON, err := (&codexauth.Credential{
				Type: "codex", AccessToken: "at-scheduled-current", RefreshToken: "rt-scheduled-current",
				AccountID: "account-scheduled", Expired: expires.UTC().Format(time.RFC3339),
			}).JSON()
			if err != nil {
				t.Fatal(err)
			}
			if tc.invalid {
				credentialJSON = "invalid credential"
			}
			cfg := createScheduledCheckChannel(t, srv, &model.Config{
				Name: "scheduled-codex", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credentialJSON,
				URLs:                  model.ChannelURLs{{URL: upstream.URL + "/backend-api/codex/responses", Exact: true, Protocols: []string{"codex"}}},
				ProtocolTransformMode: model.ProtocolTransformModeLocal,
				Enabled:               true, ScheduledCheckEnabled: true, ScheduledCheckModel: "gpt-5.6-luna",
				ModelEntries: []model.ModelEntry{{Model: "gpt-5.4-mini"}, {Model: "gpt-5.6-luna"}},
			})
			if err := srv.runScheduledChannelChecks(ctx, time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			wantRefreshCalls := int32(0)
			if tc.expired {
				wantRefreshCalls = 1
			}
			if got := refreshCalls.Load(); got != wantRefreshCalls {
				t.Errorf("refresh calls = %d, want %d", got, wantRefreshCalls)
			}
			logs, err := srv.store.ListLogs(ctx, time.Now().Add(-time.Minute), 20, 0, &model.LogFilter{LogSource: model.LogSourceScheduledCheck})
			if err != nil || len(logs) != 1 {
				t.Fatalf("scheduled logs = %d, err = %v", len(logs), err)
			}
			entry := logs[0]
			if entry.ChannelID != cfg.ID || entry.Model != "gpt-5.6-luna" || entry.APIKeyUsed != "" {
				t.Fatalf("unexpected OAuth detection log: %+v", entry)
			}
			if tc.wantSkip != "" {
				if inferenceCalls.Load() != 0 || entry.StatusCode != 0 || !strings.Contains(entry.Message, tc.wantSkip) {
					t.Fatalf("expected credential skip, calls = %d, log = %+v", inferenceCalls.Load(), entry)
				}
				return
			}
			if inferenceCalls.Load() != 1 || entry.StatusCode != http.StatusOK || entry.InputTokens != 10 || entry.OutputTokens != 5 {
				t.Fatalf("expected successful OAuth detection, calls = %d, log = %+v", inferenceCalls.Load(), entry)
			}
			storedKeys, err := srv.store.GetAPIKeys(ctx, cfg.ID)
			if err != nil || len(storedKeys) != 0 {
				t.Fatalf("OAuth detection persisted API keys: %v, err = %v", storedKeys, err)
			}
			if tc.expired {
				persisted, err := srv.store.GetConfig(ctx, cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				credential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
				if err != nil || credential.AccessToken != wantToken || credential.RefreshToken != "rt-scheduled-rotated" {
					t.Fatalf("refreshed credential was not persisted: %v", err)
				}
			}
		})
	}
}

func TestRunScheduledChannelChecks_UsesScheduledCheckModelAndAvailableKey(t *testing.T) {
	var (
		eligibleCalls int
		eligibleModel string
		eligibleAuth  string
		disabledCalls int
	)

	eligibleUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	disabledUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		disabledCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer disabledUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	eligible := createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "eligible-channel",
		URLs:                  model.ChannelURLs{{URL: eligibleUpstream.URL}},
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
		URLs:                  model.ChannelURLs{{URL: disabledUpstream.URL}},
		Enabled:               false,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	}, &model.APIKey{APIKey: "sk-disabled", KeyStrategy: model.KeyStrategySequential})

	if err := srv.runScheduledChannelChecks(ctx, time.Now().Add(time.Minute)); err != nil {
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
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	}, &model.APIKey{APIKey: "sk-success", KeyStrategy: model.KeyStrategySequential})

	createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "scheduled-log-skip",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	})

	if err := srv.runScheduledChannelChecks(ctx, time.Now().Add(time.Minute)); err != nil {
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
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	created := createScheduledCheckChannel(t, srv, &model.Config{
		Name:                  "all-keys-cooldown",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
	}, &model.APIKey{APIKey: "sk-only", KeyStrategy: model.KeyStrategySequential})

	if err := srv.store.SetKeyCooldown(ctx, created.ID, 0, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	if err := srv.runScheduledChannelChecks(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("runScheduledChannelChecks failed: %v", err)
	}
	if called != 0 {
		t.Fatalf("expected no upstream call when all keys cooled down, got %d", called)
	}
}
