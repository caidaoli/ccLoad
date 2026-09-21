package sql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	sqlstore "ccLoad/internal/storage/sql"
)

func newJSONTime(t time.Time) model.JSONTime {
	return model.JSONTime{Time: t}
}

func TestLog_AddAndList(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "logs.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "log-test-channel")

	now := time.Now()
	log := &model.LogEntry{
		Time:           newJSONTime(now),
		Model:          "gpt-4",
		ActualModel:    "gpt-4-sent",
		ResponseModel:  "gpt-4-served",
		ChannelID:      channelID,
		ClientProtocol: "openai",
		StatusCode:     200,
		Message:        "success",
		Duration:       1.5,
		IsStreaming:    false,
		APIKeyUsed:     "abcd...efgh",
	}
	if err := store.AddLog(ctx, log); err != nil {
		t.Fatalf("add log: %v", err)
	}
	// AddLog 方法不返回 ID，不需要检查

	since := now.Add(-1 * time.Hour)
	logs, err := store.ListLogs(ctx, since, 10, 0, nil)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("expected 1 log, got %d", len(logs))
	}
	if len(logs) > 0 && logs[0].Model != "gpt-4" {
		t.Errorf("model: got %q, want %q", logs[0].Model, "gpt-4")
	}
	if len(logs) > 0 && logs[0].ClientProtocol != "openai" {
		t.Errorf("client_protocol: got %q, want openai", logs[0].ClientProtocol)
	}
	if len(logs) > 0 && (logs[0].ActualModel != "gpt-4-sent" || logs[0].ResponseModel != "gpt-4-served") {
		t.Errorf("stored model metadata: actual=%q response=%q", logs[0].ActualModel, logs[0].ResponseModel)
	}
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:           newJSONTime(now.Add(time.Millisecond)),
		Model:          "gpt-4",
		ChannelID:      channelID,
		ClientProtocol: "anthropic",
		StatusCode:     200,
		Message:        "other protocol",
	}); err != nil {
		t.Fatalf("add second log: %v", err)
	}
	filtered, total, err := store.ListLogsRangeWithCount(ctx, now.Add(-time.Hour), now.Add(time.Hour), 10, 0, &model.LogFilter{ClientProtocol: "openai"})
	if err != nil {
		t.Fatalf("list filtered logs: %v", err)
	}
	if len(filtered) != 1 || total != 1 || filtered[0].ClientProtocol != "openai" {
		t.Fatalf("filtered logs=%+v total=%d, want one openai log", filtered, total)
	}
}

// 生产渠道 526（Antigravity）的真实额度采样快照：四个窗口分属 Gemini 与
// Claude/GPT 两个模型族，是 bootstrap 与分族累加的重放输入。
const prodAntigravityOAuthUsage = `{
	"requested_at": "2026-08-17T10:44:54.310977442Z",
	"sampled_at": "2026-08-17T10:44:54.744271195Z",
	"summary": {
		"provider": "antigravity",
		"windows": [
			{"limit_name":"Gemini Models","kind":"gemini-weekly","used_percent":68.84384,"remaining_percent":31.15616,"limit_window_seconds":604800,"reset_at":1787319675},
			{"limit_name":"Gemini Models","kind":"gemini-5h","used_percent":8.13866,"remaining_percent":91.86134,"limit_window_seconds":18000,"reset_at":1786969942},
			{"limit_name":"Claude and GPT models","kind":"3p-weekly","used_percent":0,"remaining_percent":100,"limit_window_seconds":604800,"reset_at":1787369662},
			{"limit_name":"Claude and GPT models","kind":"3p-5h","used_percent":0,"remaining_percent":100,"limit_window_seconds":18000,"reset_at":1786981494}
		]
	}
}`

// 窗口边界只来自上游额度采样，但采样一旦落盘就必须立刻可用于累加：
// 凭证已持久化 oauth_usage 却还要等人工刷新才建计数器，中间的消耗会被静默丢弃。
func TestLog_BootstrapsOAuthQuotaWindowsFromSampledUsage(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "oauth-quota-bootstrap.db")
	ctx := context.Background()
	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired:    time.Unix(1787369662, 0).UTC().Format(time.RFC3339),
		OAuthUsage: json.RawMessage(prodAntigravityOAuthUsage),
		// 生产上这里是空对象：采样已落盘，计数器却从未建立。
		QuotaCostUsage: &oauthcost.Usage{},
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "oauth-quota-bootstrap", AuthType: model.AuthTypeAntigravityOAuth,
		OAuthCredential: credentialJSON,
		URLs:            model.ChannelURLs{{URL: "https://daily-cloudcode-pa.googleapis.com"}},
		Enabled:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	windowCost := func(key string) int64 {
		t.Helper()
		cfg, getErr := store.GetConfig(ctx, created.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		got, parseErr := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		window := oauthcost.Find(got.QuotaCostUsage, key)
		if window == nil {
			t.Fatalf("quota cost window %q missing: %#v", key, got.QuotaCostUsage)
		}
		return window.StandardCostMicroUSD
	}

	// 渠道 526 的真实日志：Gemini 与 Claude 消耗必须落进各自的模型族窗口。
	geminiInFiveHour := time.UnixMilli(1786951952200).UTC()
	geminiAfterFiveHour := time.UnixMilli(1787015339534).UTC()
	claudeLog := time.UnixMilli(1787016073365).UTC()
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{Time: newJSONTime(geminiInFiveHour), ChannelID: created.ID, Model: "gemini-3.6-flash",
			ActualModel: "gemini-3.6-flash-high", StatusCode: http.StatusOK, Cost: 0.03110985},
		{Time: newJSONTime(geminiAfterFiveHour), ChannelID: created.ID, Model: "gemini-3.7-flash",
			ActualModel: "gemini-3.7-flash-high", StatusCode: http.StatusOK, Cost: 0.002982},
		{Time: newJSONTime(claudeLog), ChannelID: created.ID, Model: "claude-opus-4-6",
			ActualModel: "claude-opus-4-6-thinking", StatusCode: http.StatusOK, Cost: 0.044465},
	}); err != nil {
		t.Fatal(err)
	}

	// 周窗口覆盖全部三条日志的时间点，按族各收各的。
	if got := windowCost("gemini models|gemini-weekly"); got != 31110+2982 {
		t.Fatalf("gemini weekly cost = %d, want %d", got, 31110+2982)
	}
	if got := windowCost("claude and gpt models|3p-weekly"); got != 44465 {
		t.Fatalf("3p weekly cost = %d, want 44465", got)
	}
	// 5h 窗口在第二条 Gemini 日志前已滚转，只保留新周期内的消耗。
	if got := windowCost("gemini models|gemini-5h"); got != 2982 {
		t.Fatalf("gemini 5h cost = %d, want 2982", got)
	}
	if got := windowCost("claude and gpt models|3p-5h"); got != 44465 {
		t.Fatalf("3p 5h cost = %d, want 44465", got)
	}
}

func TestLog_OAuthQuotaResetUsesIncrementalRounding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cost float64
		want int64
	}{
		{name: "round each up", cost: 0.0000006, want: 2},
		{name: "round each down", cost: 0.0000004, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t, "quota-reset-rounding.db")
			ctx := context.Background()
			base := time.Date(2026, time.September, 5, 0, 0, 0, 0, time.UTC)
			credential := &codexauth.Credential{
				Type: "codex", AccessToken: "access", RefreshToken: "refresh", Expired: base.Add(time.Hour).Format(time.RFC3339),
				QuotaCostUsage: oauthcost.Reconcile(nil, []oauthcost.Sample{{
					Key: "codex|primary", Family: oauthcost.FamilyCodex, WindowSeconds: 604800, ResetAt: base.Add(24 * time.Hour),
				}}, base),
			}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := store.CreateConfig(ctx, &model.Config{
				Name: "quota-rounding", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: raw,
				URLs: model.ChannelURLs{{URL: "https://api.example.com", Protocols: []string{"codex"}}}, Enabled: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 2; i++ {
				if err := store.AddLog(ctx, &model.LogEntry{
					Time: newJSONTime(base.Add(time.Duration(i) * time.Second)), ChannelID: cfg.ID,
					Model: "gpt-5.6-sol", StatusCode: http.StatusOK, Cost: tc.cost,
				}); err != nil {
					t.Fatal(err)
				}
			}
			assertCost := func() {
				t.Helper()
				gotCfg, err := store.GetConfig(ctx, cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				got, err := codexauth.ParseCredential([]byte(gotCfg.OAuthCredential))
				if err != nil {
					t.Fatal(err)
				}
				w := oauthcost.Find(got.QuotaCostUsage, "codex|primary")
				if w == nil || w.StandardCostMicroUSD != tc.want {
					t.Fatalf("cost = %+v, want %d microUSD", w, tc.want)
				}
			}
			assertCost()
			if err := store.ResetOAuthQuotaCostUsage(ctx, cfg.ID, base); err != nil {
				t.Fatal(err)
			}
			assertCost()
		})
	}
}

func TestLog_BatchAccumulatesOAuthQuotaStandardCostByPeriod(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "oauth-quota-cost.db")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	resetAt := now.Add(time.Hour)
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: now.Add(24 * time.Hour).Format(time.RFC3339),
		QuotaCostUsage: &oauthcost.Usage{
			Windows: []*oauthcost.Window{
				{
					Key: "codex|secondary", WindowSeconds: 7 * 24 * 60 * 60,
					StartedAt: resetAt.Add(-7 * 24 * time.Hour).Unix(), ResetAt: resetAt.Unix(),
				},
				{
					// 兼容修复前已落盘的 gpt-reserve family：普通 Codex 日志
					// 不得再把它当成主 Codex 窗口累计。
					Key: "gpt-reserve|primary", Family: oauthcost.FamilyCodex,
					WindowSeconds: 7 * 24 * 60 * 60,
					StartedAt:     resetAt.Add(-7 * 24 * time.Hour).Unix(), ResetAt: resetAt.Unix(),
				},
				{
					Key: "codex|monthly", WindowSeconds: 30 * 24 * 60 * 60,
					StartedAt: resetAt.AddDate(0, -1, 0).Unix(), ResetAt: resetAt.Unix(),
				},
			},
		},
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "oauth-quota-cost", AuthType: model.AuthTypeCodexOAuth,
		OAuthCredential: credentialJSON,
		URLs:            model.ChannelURLs{{URL: "https://api.example.com", Protocols: []string{"codex"}}},
		Enabled:         true,
	})
	if err != nil {
		t.Fatal(err)
	}

	logs := []*model.LogEntry{
		{Time: newJSONTime(now), ChannelID: created.ID, StatusCode: http.StatusOK, Cost: 1.25, CostMultiplier: 10},
		{Time: newJSONTime(now.Add(time.Second)), ChannelID: created.ID, StatusCode: http.StatusNoContent, Cost: 0.75, CostMultiplier: 0.1},
		{Time: newJSONTime(now.Add(2 * time.Second)), ChannelID: created.ID, StatusCode: http.StatusBadGateway, Cost: 9},
		{Time: newJSONTime(now.Add(3 * time.Second)), ChannelID: created.ID, StatusCode: http.StatusOK, Cost: 9, LogSource: model.LogSourceManualTest},
	}
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	assertCosts := func(want int64) *codexauth.Credential {
		t.Helper()
		cfg, getErr := store.GetConfig(ctx, created.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		got, parseErr := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		weekly := oauthcost.Find(got.QuotaCostUsage, "codex|secondary")
		reserve := oauthcost.Find(got.QuotaCostUsage, "gpt-reserve|primary")
		monthly := oauthcost.Find(got.QuotaCostUsage, "codex|monthly")
		if weekly == nil || reserve == nil || monthly == nil {
			t.Fatalf("quota cost usage missing: %#v", got.QuotaCostUsage)
		}
		if weekly.StandardCostMicroUSD != want || monthly.StandardCostMicroUSD != want {
			t.Fatalf("quota costs = weekly %d monthly %d, want %d",
				weekly.StandardCostMicroUSD, monthly.StandardCostMicroUSD, want)
		}
		if reserve.StandardCostMicroUSD != 0 {
			t.Fatalf("gpt-reserve quota cost = %d, want 0", reserve.StandardCostMicroUSD)
		}
		return got
	}
	assertCosts(20_000_000)
	// GPT-5.3-Codex-Spark 使用独立额度，不能污染 Codex 主周/月窗口。
	if err := store.AddLog(ctx, &model.LogEntry{
		Time: newJSONTime(now.Add(4 * time.Second)), Model: "gpt-5.3-codex-spark",
		ChannelID: created.ID, StatusCode: http.StatusOK, Cost: 1.5,
	}); err != nil {
		t.Fatal(err)
	}
	assertCosts(20_000_000)

	if err := store.AddLog(ctx, &model.LogEntry{
		Time: newJSONTime(resetAt), ChannelID: created.ID, StatusCode: http.StatusOK, Cost: 0.5,
	}); err != nil {
		t.Fatal(err)
	}
	rolled := assertCosts(500_000)
	if oauthcost.Find(rolled.QuotaCostUsage, "codex|secondary").StartedAt != resetAt.Unix() ||
		oauthcost.Find(rolled.QuotaCostUsage, "codex|monthly").StartedAt != resetAt.Unix() {
		t.Fatalf("period did not roll at reset: %#v", rolled.QuotaCostUsage)
	}

	if err := store.AddLog(ctx, &model.LogEntry{
		Time: newJSONTime(resetAt.Add(-time.Second)), ChannelID: created.ID, StatusCode: http.StatusOK, Cost: 7,
	}); err != nil {
		t.Fatal(err)
	}
	assertCosts(500_000)

	manualResetAt := resetAt.Add(time.Minute)
	if err := store.AddLog(ctx, &model.LogEntry{
		Time: newJSONTime(manualResetAt.Add(time.Second)), ChannelID: created.ID, StatusCode: http.StatusOK, Cost: 0.25,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetOAuthQuotaCostUsage(ctx, created.ID, manualResetAt); err != nil {
		t.Fatal(err)
	}
	reset := assertCosts(250_000)
	if oauthcost.Find(reset.QuotaCostUsage, "codex|secondary").CountFromAt != manualResetAt.Unix() ||
		oauthcost.Find(reset.QuotaCostUsage, "codex|monthly").CountFromAt != manualResetAt.Unix() {
		t.Fatalf("manual reset cutoff missing: %#v", reset.QuotaCostUsage)
	}
	if err := store.AddLog(ctx, &model.LogEntry{
		Time: newJSONTime(manualResetAt.Add(-time.Second)), ChannelID: created.ID, StatusCode: http.StatusOK, Cost: 7,
	}); err != nil {
		t.Fatal(err)
	}
	assertCosts(250_000)

	db := store.(*sqlstore.SQLStore)
	if _, err := db.ExecContext(ctx, `UPDATE channels SET oauth_credential = ? WHERE id = ?`,
		`{"quota_cost_usage":`, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AddLog(ctx, &model.LogEntry{
		Time: newJSONTime(resetAt.Add(time.Second)), Model: "rollback-marker",
		ChannelID: created.ID, StatusCode: http.StatusOK, Cost: 1,
	}); err == nil {
		t.Fatal("invalid credential should roll back the log and quota update")
	}
	var rollbackLogs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs WHERE model = ?`, "rollback-marker").Scan(&rollbackLogs); err != nil {
		t.Fatal(err)
	}
	if rollbackLogs != 0 {
		t.Fatalf("rolled back log count = %d, want 0", rollbackLogs)
	}
}

func TestLog_AddAndListPersistsReasoningTokens(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "logs_reasoning_tokens.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "log-reasoning-token-channel")

	now := time.Now()
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:            newJSONTime(now),
		Model:           "gpt-5-codex",
		ChannelID:       channelID,
		StatusCode:      200,
		Message:         "success",
		ThinkingEffort:  "xhigh",
		ReasoningTokens: 1234,
	}); err != nil {
		t.Fatalf("add log: %v", err)
	}

	logs, err := store.ListLogs(ctx, now.Add(-time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs)=%d, want 1", len(logs))
	}
	if logs[0].ReasoningTokens != 1234 {
		t.Fatalf("reasoning_tokens=%d, want 1234", logs[0].ReasoningTokens)
	}
}

func TestLog_AddLogPersistsDebugData(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "add_log_debug.db")
	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "add-log-debug-channel")

	now := time.Now()
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:       newJSONTime(now),
		Model:      "gpt-4",
		ChannelID:  channelID,
		StatusCode: 200,
		Message:    "ok",
		DebugData: &model.DebugLogEntry{
			CreatedAt:             now.Unix(),
			ReqMethod:             http.MethodPost,
			ReqURL:                "https://api.example.com/v1/chat/completions",
			ReqHeaders:            `{"Content-Type":"application/json"}`,
			ReqBody:               []byte(`{"contents":[{"role":"user"}]}`),
			RespStatus:            200,
			RespHeaders:           `{"Content-Type":"application/json"}`,
			RespBody:              []byte(`{"candidates":[{"content":"ok"}]}`),
			UpstreamError:         "response stream ended unexpectedly",
			ProtocolTransformed:   true,
			OriginalReqURL:        "/v1/chat/completions",
			OriginalReqHeaders:    `{"X-Client-Trace":"original"}`,
			OriginalReqBody:       []byte(`{"messages":[{"role":"user"}]}`),
			TranslatedRespStatus:  http.StatusOK,
			TranslatedRespHeaders: `{"Content-Type":"application/json"}`,
			TranslatedRespBody:    []byte(`{"choices":[{"message":{"content":"ok"}}]}`),
		},
	}); err != nil {
		t.Fatalf("add log with debug data: %v", err)
	}

	logs, err := store.ListLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), 10, 0, nil)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs)=%d, want 1", len(logs))
	}
	debugLog, err := store.GetDebugLogByLogID(ctx, logs[0].ID)
	if err != nil {
		t.Fatalf("get debug log: %v", err)
	}
	if debugLog == nil {
		t.Fatal("debug log should be persisted for AddLog")
	}
	if debugLog.RespStatus != http.StatusOK {
		t.Fatalf("debug resp status=%d, want 200", debugLog.RespStatus)
	}
	if string(debugLog.RespBody) != `{"candidates":[{"content":"ok"}]}` {
		t.Fatalf("debug resp body=%q", string(debugLog.RespBody))
	}
	if debugLog.UpstreamError != "response stream ended unexpectedly" {
		t.Fatalf("debug upstream error=%q", debugLog.UpstreamError)
	}
	if !debugLog.ProtocolTransformed {
		t.Fatal("debug protocol transform flag was not persisted")
	}
	if string(debugLog.OriginalReqBody) != `{"messages":[{"role":"user"}]}` {
		t.Fatalf("debug original req body=%q", string(debugLog.OriginalReqBody))
	}
	if debugLog.OriginalReqURL != "/v1/chat/completions" || debugLog.OriginalReqHeaders != `{"X-Client-Trace":"original"}` {
		t.Fatalf("debug original request metadata=%+v", debugLog)
	}
	if debugLog.TranslatedRespStatus != http.StatusOK || debugLog.TranslatedRespHeaders != `{"Content-Type":"application/json"}` {
		t.Fatalf("debug translated response metadata=%+v", debugLog)
	}
	if string(debugLog.TranslatedRespBody) != `{"choices":[{"message":{"content":"ok"}}]}` {
		t.Fatalf("debug translated resp body=%q", string(debugLog.TranslatedRespBody))
	}
}

func TestDebugLog_AddPersistsProtocolMetadata(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "add_debug_log.db")
	entry := &model.DebugLogEntry{
		LogID:                 42,
		ReqMethod:             http.MethodPost,
		ReqURL:                "https://upstream.example.com/v1/messages",
		ReqHeaders:            `{}`,
		ReqBody:               []byte(`{"upstream":true}`),
		RespStatus:            http.StatusOK,
		RespHeaders:           `{}`,
		UpstreamError:         "unexpected EOF",
		ProtocolTransformed:   true,
		OriginalReqURL:        "/v1/chat/completions",
		OriginalReqHeaders:    `{"X-Client-Trace":"direct"}`,
		OriginalReqBody:       []byte(`{"client":true}`),
		TranslatedRespStatus:  http.StatusOK,
		TranslatedRespHeaders: `{"Content-Type":"application/json"}`,
		TranslatedRespBody:    []byte(`{"translated":true}`),
	}
	if err := store.AddDebugLog(t.Context(), entry); err != nil {
		t.Fatalf("AddDebugLog: %v", err)
	}

	got, err := store.GetDebugLogByLogID(t.Context(), entry.LogID)
	if err != nil {
		t.Fatalf("GetDebugLogByLogID: %v", err)
	}
	if got == nil {
		t.Fatal("debug log not found")
	}
	if got.OriginalReqURL != entry.OriginalReqURL || got.OriginalReqHeaders != entry.OriginalReqHeaders {
		t.Fatalf("original request metadata=%+v", got)
	}
	if got.TranslatedRespStatus != entry.TranslatedRespStatus || got.TranslatedRespHeaders != entry.TranslatedRespHeaders {
		t.Fatalf("translated response metadata=%+v", got)
	}
	if got.UpstreamError != entry.UpstreamError {
		t.Fatalf("upstream error=%q, want %q", got.UpstreamError, entry.UpstreamError)
	}
}

func TestDebugLog_CleanupBatchDeletesOldestRowsUpToLimit(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "cleanup_debug_log_batch.db")
	ctx := t.Context()
	cutoff := time.Now()

	for i, createdAt := range []time.Time{
		cutoff.Add(-5 * time.Minute),
		cutoff.Add(-4 * time.Minute),
		cutoff.Add(-3 * time.Minute),
		cutoff.Add(-2 * time.Minute),
		cutoff.Add(time.Minute),
	} {
		if err := store.AddDebugLog(ctx, &model.DebugLogEntry{
			LogID:       int64(i + 1),
			CreatedAt:   createdAt.Unix(),
			ReqURL:      "https://upstream.example.com",
			ReqHeaders:  "{}",
			ReqBody:     []byte("request"),
			RespHeaders: "{}",
		}); err != nil {
			t.Fatalf("AddDebugLog(%d): %v", i+1, err)
		}
	}

	deleted, err := store.CleanupDebugLogsBatch(ctx, cutoff, 2)
	if err != nil {
		t.Fatalf("CleanupDebugLogsBatch: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted=%d, want 2", deleted)
	}

	for _, logID := range []int64{1, 2} {
		entry, err := store.GetDebugLogByLogID(ctx, logID)
		if err != nil {
			t.Fatalf("GetDebugLogByLogID(%d): %v", logID, err)
		}
		if entry != nil {
			t.Fatalf("debug log %d still exists after cleanup", logID)
		}
	}
	for _, logID := range []int64{3, 4, 5} {
		entry, err := store.GetDebugLogByLogID(ctx, logID)
		if err != nil {
			t.Fatalf("GetDebugLogByLogID(%d): %v", logID, err)
		}
		if entry == nil {
			t.Fatalf("debug log %d was deleted unexpectedly", logID)
		}
	}

	deleted, err = store.CleanupDebugLogsBatch(ctx, cutoff, 2)
	if err != nil {
		t.Fatalf("second CleanupDebugLogsBatch: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("second deleted=%d, want 2", deleted)
	}
	deleted, err = store.CleanupDebugLogsBatch(ctx, cutoff, 2)
	if err != nil {
		t.Fatalf("third CleanupDebugLogsBatch: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("third deleted=%d, want 0", deleted)
	}
	if entry, err := store.GetDebugLogByLogID(ctx, 5); err != nil || entry == nil {
		t.Fatalf("recent debug log was not preserved: entry=%v err=%v", entry, err)
	}
}

func TestDebugLog_TruncateKeepsTableUsable(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "truncate_debug_logs.db")
	entry := &model.DebugLogEntry{
		LogID:       1,
		ReqURL:      "https://upstream.example.com",
		ReqHeaders:  "{}",
		ReqBody:     []byte("request"),
		RespHeaders: "{}",
	}
	if err := store.AddDebugLog(t.Context(), entry); err != nil {
		t.Fatalf("AddDebugLog: %v", err)
	}
	if err := store.TruncateDebugLogs(t.Context()); err != nil {
		t.Fatalf("TruncateDebugLogs: %v", err)
	}
	if got, err := store.GetDebugLogByLogID(t.Context(), entry.LogID); err != nil || got != nil {
		t.Fatalf("debug log still exists after truncate: entry=%v err=%v", got, err)
	}

	entry.LogID = 2
	if err := store.AddDebugLog(t.Context(), entry); err != nil {
		t.Fatalf("AddDebugLog after truncate: %v", err)
	}
	if got, err := store.GetDebugLogByLogID(t.Context(), entry.LogID); err != nil || got == nil {
		t.Fatalf("debug log table is unusable after truncate: entry=%v err=%v", got, err)
	}
}

func TestLog_BatchAdd(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "batch_logs.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "batch-log-channel")

	now := time.Now()
	logs := []*model.LogEntry{
		{
			Time:       newJSONTime(now),
			Model:      "gpt-4",
			ChannelID:  channelID,
			StatusCode: 200,
			Message:    "success 1",
			Duration:   1.0,
			APIKeyUsed: "key1...1key",
		},
		{
			Time:       newJSONTime(now),
			Model:      "claude-3",
			ChannelID:  channelID,
			StatusCode: 200,
			Message:    "success 2",
			Duration:   2.0,
			APIKeyUsed: "key2...2key",
		},
		{
			Time:       newJSONTime(now),
			Model:      "gpt-4",
			ChannelID:  channelID,
			StatusCode: 500,
			Message:    "error",
			Duration:   0.5,
			APIKeyUsed: "key3...3key",
		},
	}

	if err := store.BatchAddLogs(ctx, logs); err != nil {
		t.Fatalf("batch add logs: %v", err)
	}
	// BatchAddLogs 方法不返回 ID，不需要检查

	since := now.Add(-1 * time.Hour)
	count, err := store.CountLogs(ctx, since, nil)
	if err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 logs, got %d", count)
	}
}

func TestLog_ListRange(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "range_logs.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "range-log-channel")

	now := time.Now()
	logs := []*model.LogEntry{
		{
			Time:       newJSONTime(now.Add(-2 * time.Hour)),
			Model:      "old-model",
			ChannelID:  channelID,
			StatusCode: 200,
			Message:    "old log",
			Duration:   1.0,
			APIKeyUsed: "key1...1key",
		},
		{
			Time:       newJSONTime(now.Add(-30 * time.Minute)),
			Model:      "recent-model",
			ChannelID:  channelID,
			StatusCode: 200,
			Message:    "recent log",
			Duration:   1.0,
			APIKeyUsed: "key2...2key",
		},
	}
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		t.Fatalf("batch add logs: %v", err)
	}

	startTime := now.Add(-1 * time.Hour)
	endTime := now

	rangeLogs, err := store.ListLogsRange(ctx, startTime, endTime, 100, 0, nil)
	if err != nil {
		t.Fatalf("list logs range: %v", err)
	}
	if len(rangeLogs) != 1 {
		t.Errorf("expected 1 log in range, got %d", len(rangeLogs))
	}
	if len(rangeLogs) > 0 && rangeLogs[0].Model != "recent-model" {
		t.Errorf("model: got %q, want %q", rangeLogs[0].Model, "recent-model")
	}

	rangeCount, err := store.CountLogsRange(ctx, startTime, endTime, nil)
	if err != nil {
		t.Fatalf("count logs range: %v", err)
	}
	if rangeCount != 1 {
		t.Errorf("expected 1 log in range count, got %d", rangeCount)
	}
}

func TestLog_Pagination(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "pagination_logs.db")

	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "pagination-channel")

	now := time.Now()
	logs := make([]*model.LogEntry, 10)
	for i := 0; i < 10; i++ {
		logs[i] = &model.LogEntry{
			Time:       newJSONTime(now),
			Model:      "gpt-4",
			ChannelID:  channelID,
			StatusCode: 200,
			Message:    "log " + string(rune('0'+i)),
			Duration:   float64(i),
			APIKeyUsed: "key...key",
		}
	}
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		t.Fatalf("batch add logs: %v", err)
	}

	since := now.Add(-1 * time.Hour)

	page1, err := store.ListLogs(ctx, since, 5, 0, nil)
	if err != nil {
		t.Fatalf("list logs page 1: %v", err)
	}
	if len(page1) != 5 {
		t.Errorf("page 1: expected 5 logs, got %d", len(page1))
	}

	page2, err := store.ListLogs(ctx, since, 5, 5, nil)
	if err != nil {
		t.Fatalf("list logs page 2: %v", err)
	}
	if len(page2) != 5 {
		t.Errorf("page 2: expected 5 logs, got %d", len(page2))
	}

	seen := make(map[int64]struct{}, len(page1))
	for _, entry := range page1 {
		seen[entry.ID] = struct{}{}
	}
	for _, entry := range page2 {
		if _, ok := seen[entry.ID]; ok {
			t.Fatalf("pages should not overlap, overlapping id=%d", entry.ID)
		}
	}
}

func TestLog_ListRangeWithCount_PreservesZeroCostMultiplier(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "logs_zero_multiplier.db")
	ctx := context.Background()
	channelID := createTestChannel(t, ctx, store, "free-log-channel")

	now := time.Now()
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:              newJSONTime(now),
		Model:             "gpt-5.4-mini",
		ChannelID:         channelID,
		StatusCode:        200,
		Message:           "success",
		Duration:          1.2,
		APIKeyUsed:        "key...key",
		Cost:              0.019,
		CostMultiplier:    0,
		UpstreamWebsocket: true,
	}); err != nil {
		t.Fatalf("add log: %v", err)
	}

	startTime := now.Add(-1 * time.Minute)
	endTime := now.Add(1 * time.Minute)

	logs, total, err := store.ListLogsRangeWithCount(ctx, startTime, endTime, 10, 0, nil)
	if err != nil {
		t.Fatalf("ListLogsRangeWithCount failed: %v", err)
	}
	if total != 1 {
		t.Fatalf("total=%d, want 1", total)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs)=%d, want 1", len(logs))
	}
	if logs[0].CostMultiplier != 0 {
		t.Fatalf("cost_multiplier=%v, want 0", logs[0].CostMultiplier)
	}
	if !logs[0].UpstreamWebsocket {
		t.Fatal("upstream_websocket=false, want true")
	}
}

func TestLog_OAuthUsageBoundaryCorrectionKeepsExpiredLogCosts(t *testing.T) {
	for _, seconds := range []int64{604800, 30 * 24 * 60 * 60} {
		t.Run(time.Duration(seconds*int64(time.Second)).String(), func(t *testing.T) {
			store := newTestStore(t, "quota-retention.db")
			ctx := context.Background()
			base := time.Date(2026, time.September, 17, 6, 0, 0, 0, time.UTC)
			used := 10.0
			sample := oauthcost.Sample{Key: "gemini models|quota", Family: oauthcost.FamilyGemini,
				WindowSeconds: seconds, ResetAt: base.Add(24 * time.Hour), UsedPercent: &used, SampledAt: base.Add(-6 * 24 * time.Hour)}
			credential := &antigravityauth.Credential{Type: "antigravity", AccessToken: "test", RefreshToken: "test", Expired: "2030-01-01T00:00:00Z",
				QuotaCostUsage: oauthcost.Reconcile(nil, []oauthcost.Sample{sample}, sample.SampledAt)}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			channel, err := store.CreateConfig(ctx, &model.Config{Name: "quota-retention", AuthType: model.AuthTypeAntigravityOAuth,
				OAuthCredential: raw, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, at := range []time.Time{base.Add(-5 * 24 * time.Hour), base.Add(-time.Minute)} {
				if err := store.AddLog(ctx, &model.LogEntry{Time: newJSONTime(at), ChannelID: channel.ID,
					Model: "alias", ActualModel: "gemini-3.8-flash-high", Cost: 1.25, CostMultiplier: 9}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.CleanupLogsBefore(ctx, base.Add(-2*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			refresh := func(cfg *model.Config, wantUpdated bool, want int64) {
				t.Helper()
				current, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
				if err != nil {
					t.Fatal(err)
				}
				sample.ResetAt, sample.SampledAt = base.Add(25*time.Hour), base
				current.QuotaCostUsage = oauthcost.Reconcile(current.QuotaCostUsage, []oauthcost.Sample{sample}, base)
				payload, err := current.JSON()
				if err != nil {
					t.Fatal(err)
				}
				updated, costs, err := store.CompareAndSwapOAuthUsage(ctx, channel.ID, model.AuthTypeAntigravityOAuth, cfg.OAuthCredential, payload)
				if err != nil || updated != wantUpdated {
					t.Fatalf("quota CAS = %t, %v, want %t", updated, err, wantUpdated)
				}
				if updated && oauthcost.Find(costs, sample.Key).StandardCostMicroUSD != want {
					t.Fatalf("quota cost = %+v, want %d", oauthcost.Find(costs, sample.Key), want)
				}
			}
			cfg, err := store.GetConfig(ctx, channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			// A log committing after the snapshot must force a CAS retry.
			if err := store.AddLog(ctx, &model.LogEntry{Time: newJSONTime(base), ChannelID: channel.ID,
				Model: "gemini-3.8-flash-high", Cost: 0.25}); err != nil {
				t.Fatal(err)
			}
			refresh(cfg, false, 0)
			cfg, err = store.GetConfig(ctx, channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			refresh(cfg, true, 2_750_000)
		})
	}
}

func TestLog_OAuthUsageConfirmsLocallyAdvancedPeriod(t *testing.T) {
	for _, seconds := range []int64{18000, 604800, 30 * 24 * 60 * 60} {
		t.Run(time.Duration(seconds*int64(time.Second)).String(), func(t *testing.T) {
			store := newTestStore(t, "quota-local-rollover.db")
			ctx := context.Background()
			oldReset := time.Date(2026, time.September, 17, 6, 0, 0, 0, time.UTC)
			used := 10.0
			sample := oauthcost.Sample{Key: "gemini models|quota", Family: oauthcost.FamilyGemini,
				WindowSeconds: seconds, ResetAt: oldReset, UsedPercent: &used, SampledAt: oldReset.Add(-time.Hour)}
			credential := &antigravityauth.Credential{Type: "antigravity", AccessToken: "test", RefreshToken: "test", Expired: "2030-01-01T00:00:00Z",
				QuotaCostUsage: oauthcost.Reconcile(nil, []oauthcost.Sample{sample}, sample.SampledAt)}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			channel, err := store.CreateConfig(ctx, &model.Config{Name: "quota-local-rollover", AuthType: model.AuthTypeAntigravityOAuth,
				OAuthCredential: raw, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, at := range []time.Time{oldReset.Add(-30 * time.Minute), oldReset.Add(time.Minute)} {
				if err := store.AddLog(ctx, &model.LogEntry{Time: newJSONTime(at), ChannelID: channel.ID,
					Model: "gemini-3.8-flash-high", Cost: 1.25}); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := store.GetConfig(ctx, channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			current, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
			if err != nil {
				t.Fatal(err)
			}
			// The locally predicted period discarded the first log. Upstream now
			// confirms a period covering both, within the half-window tolerance.
			sample.ResetAt = oldReset.Add(time.Duration(seconds) * time.Second * 3 / 5)
			sample.SampledAt = oldReset.Add(2 * time.Minute)
			current.QuotaCostUsage = oauthcost.Reconcile(current.QuotaCostUsage, []oauthcost.Sample{sample}, sample.SampledAt)
			payload, err := current.JSON()
			if err != nil {
				t.Fatal(err)
			}
			updated, costs, err := store.CompareAndSwapOAuthUsage(ctx, channel.ID, model.AuthTypeAntigravityOAuth, cfg.OAuthCredential, payload)
			if err != nil || !updated {
				t.Fatalf("quota CAS = %t, %v", updated, err)
			}
			if got := oauthcost.Find(costs, sample.Key).StandardCostMicroUSD; got != 2_500_000 {
				t.Fatalf("confirmed quota cost = %d, want 2500000", got)
			}
		})
	}
}

// 本地误滚后上游确认真实周期时，旧周期尾部的日志可能已经超出保留期。
// 对账只能核对边界的对称差；按新边界全量重算会把这段成本直接抹掉。
func TestLog_OAuthUsageConfirmedPeriodKeepsExpiredLocalCosts(t *testing.T) {
	for _, seconds := range []int64{18000, 604800, 30 * 24 * 60 * 60} {
		window := time.Duration(seconds) * time.Second
		t.Run(window.String(), func(t *testing.T) {
			store := newTestStore(t, "quota-local-rollover-expired.db")
			ctx := context.Background()
			oldReset := time.Date(2026, time.September, 17, 6, 0, 0, 0, time.UTC)
			used := 10.0
			sample := oauthcost.Sample{Key: "gemini models|quota", Family: oauthcost.FamilyGemini,
				WindowSeconds: seconds, ResetAt: oldReset, UsedPercent: &used, SampledAt: oldReset.Add(-window / 2)}
			credential := &antigravityauth.Credential{Type: "antigravity", AccessToken: "test", RefreshToken: "test",
				Expired:        "2030-01-01T00:00:00Z",
				QuotaCostUsage: oauthcost.Reconcile(nil, []oauthcost.Sample{sample}, sample.SampledAt)}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			channel, err := store.CreateConfig(ctx, &model.Config{Name: "quota-local-rollover-expired",
				AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: raw,
				URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			add := func(at time.Time) {
				t.Helper()
				if err := store.AddLog(ctx, &model.LogEntry{Time: newJSONTime(at), ChannelID: channel.ID,
					Model: "gemini-3.8-flash-high", Cost: 1}); err != nil {
					t.Fatal(err)
				}
			}
			refresh := func(next oauthcost.Sample, observedAt time.Time) *oauthcost.Usage {
				t.Helper()
				cfg, err := store.GetConfig(ctx, channel.ID)
				if err != nil {
					t.Fatal(err)
				}
				current, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
				if err != nil {
					t.Fatal(err)
				}
				current.QuotaCostUsage = oauthcost.Reconcile(current.QuotaCostUsage, []oauthcost.Sample{next}, observedAt)
				payload, err := current.JSON()
				if err != nil {
					t.Fatal(err)
				}
				updated, costs, err := store.CompareAndSwapOAuthUsage(ctx, channel.ID,
					model.AuthTypeAntigravityOAuth, cfg.OAuthCredential, payload)
				if err != nil || !updated {
					t.Fatalf("quota CAS = %t, %v", updated, err)
				}
				return costs
			}

			// 一次常规刷新把「已计入区间」落盘，随后的边界变化才有对账基准。
			add(oldReset.Add(-window / 3))
			sample.SampledAt = oldReset.Add(-window / 4)
			if got := oauthcost.Find(refresh(sample, sample.SampledAt), sample.Key).StandardCostMicroUSD; got != 1_000_000 {
				t.Fatalf("accounted quota cost = %d, want 1000000", got)
			}

			// 本地时钟越过预测截止点：窗口滚动清零，新周期重新累计。
			add(oldReset)
			add(oldReset.Add(window / 3))
			if err := store.CleanupLogsBefore(ctx, oldReset.Add(window/6)); err != nil {
				t.Fatal(err)
			}

			// 上游确认真实周期比本地预测早结束，起点随之前移。前移进来的那段
			// 日志已被清理，但本地滚动后累计的成本必须原样保留。
			used = 30
			sample.ResetAt = oldReset.Add(window - window/10)
			sample.SampledAt = oldReset.Add(window / 2)
			if got := oauthcost.Find(refresh(sample, sample.SampledAt), sample.Key).StandardCostMicroUSD; got != 2_000_000 {
				t.Fatalf("confirmed quota cost = %d, want 2000000", got)
			}
		})
	}
}

// 上游额度周期以「本周期首个请求」为锚点，新周期的 reset_at 会整段跳变，
// 超出半窗口容差即判定为周期切换，reconcileWindow 直接返回全新窗口。
// 全新窗口的 started_at 早于采样时刻，而累计成本从零起步：这段已落盘的日志
// 必须在对账时补回，否则额度成本会长期少算（线上 Antigravity 5h 窗口即为此
// 现象，周/月窗口只是切换频率低，缺陷路径与窗口长度无关）。
func TestLog_OAuthUsagePeriodSwitchBackfillsLogsBeforeSample(t *testing.T) {
	for _, seconds := range []int64{18000, 604800, 30 * 24 * 60 * 60} {
		window := time.Duration(seconds) * time.Second
		t.Run(window.String(), func(t *testing.T) {
			store := newTestStore(t, "quota-period-switch.db")
			ctx := context.Background()
			oldReset := time.Date(2026, time.September, 17, 6, 0, 0, 0, time.UTC)
			used := 10.0
			sample := oauthcost.Sample{Key: "gemini models|quota", Family: oauthcost.FamilyGemini,
				WindowSeconds: seconds, ResetAt: oldReset, UsedPercent: &used, SampledAt: oldReset.Add(-window / 2)}
			credential := &antigravityauth.Credential{Type: "antigravity", AccessToken: "test", RefreshToken: "test",
				Expired:        "2030-01-01T00:00:00Z",
				QuotaCostUsage: oauthcost.Reconcile(nil, []oauthcost.Sample{sample}, sample.SampledAt)}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			channel, err := store.CreateConfig(ctx, &model.Config{Name: "quota-period-switch",
				AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: raw,
				URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			// 本地按旧截止点滚出新周期并逐条累计；此刻边界都是暂定值。
			for _, at := range []time.Time{oldReset.Add(window / 10), oldReset.Add(window / 5)} {
				if err := store.AddLog(ctx, &model.LogEntry{Time: newJSONTime(at), ChannelID: channel.ID,
					Model: "alias", ActualModel: "gemini-3.8-flash-high", Cost: 1.25}); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := store.GetConfig(ctx, channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			current, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
			if err != nil {
				t.Fatal(err)
			}
			// 上游给出的真实截止点与本地预测相差半个窗口以上：判定周期切换，
			// 新窗口起点回溯到采样之前，覆盖上面两条已落盘的日志。
			observedAt := oldReset.Add(window / 4)
			sample.ResetAt, sample.SampledAt = oldReset.Add(window/2), observedAt
			current.QuotaCostUsage = oauthcost.Reconcile(current.QuotaCostUsage, []oauthcost.Sample{sample}, observedAt)
			if got := oauthcost.Find(current.QuotaCostUsage, sample.Key).StandardCostMicroUSD; got != 0 {
				t.Fatalf("switched window cost = %d, want 0 before reconcile", got)
			}
			payload, err := current.JSON()
			if err != nil {
				t.Fatal(err)
			}
			updated, costs, err := store.CompareAndSwapOAuthUsage(ctx, channel.ID,
				model.AuthTypeAntigravityOAuth, cfg.OAuthCredential, payload)
			if err != nil || !updated {
				t.Fatalf("quota CAS = %t, %v", updated, err)
			}
			if got := oauthcost.Find(costs, sample.Key).StandardCostMicroUSD; got != 2_500_000 {
				t.Fatalf("backfilled quota cost = %d, want 2500000", got)
			}
		})
	}
}

func TestLog_OAuthUsageRespectsResetCutoffs(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "upstream_rollback"
		if manual {
			name = "manual_reset_and_weekly_role_change"
		}
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t, "quota-cutoff.db")
			ctx := context.Background()
			base := time.Date(2026, time.September, 5, 6, 0, 0, 0, time.UTC)
			used := 80.0
			sample := oauthcost.Sample{Key: "codex|secondary", Family: oauthcost.FamilyCodex, WindowSeconds: 604800,
				ResetAt: base.Add(2 * 24 * time.Hour), UsedPercent: &used, SampledAt: base.Add(-time.Hour)}
			credential := &codexauth.Credential{Type: "codex", AccessToken: "test", RefreshToken: "test", Expired: "2030-01-01T00:00:00Z",
				QuotaCostUsage: oauthcost.Reconcile(nil, []oauthcost.Sample{sample}, sample.SampledAt)}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			channel, err := store.CreateConfig(ctx, &model.Config{Name: name, AuthType: model.AuthTypeCodexOAuth,
				OAuthCredential: raw, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			add := func(at time.Time, cost float64) {
				t.Helper()
				if err := store.AddLog(ctx, &model.LogEntry{Time: newJSONTime(at), ChannelID: channel.ID,
					Model: "gpt-5.6-sol", Cost: cost}); err != nil {
					t.Fatal(err)
				}
			}
			add(base.Add(-time.Minute), 2)
			add(base.Add(30*time.Minute), 1.25)
			if manual {
				if err := store.ResetOAuthQuotaCostUsage(ctx, channel.ID, base); err != nil {
					t.Fatal(err)
				}
				sample.Key = "codex|primary"
				sample.ResetAt = base.Add(7*24*time.Hour + 33*time.Minute)
			}
			cfg, err := store.GetConfig(ctx, channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			current, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
			if err != nil {
				t.Fatal(err)
			}
			used = 5
			sample.SampledAt = base.Add(20 * time.Minute)
			current.QuotaCostUsage = oauthcost.Reconcile(current.QuotaCostUsage, []oauthcost.Sample{sample}, base.Add(time.Hour))
			payload, err := current.JSON()
			if err != nil {
				t.Fatal(err)
			}
			updated, costs, err := store.CompareAndSwapOAuthUsage(ctx, channel.ID, model.AuthTypeCodexOAuth, cfg.OAuthCredential, payload)
			if err != nil || !updated {
				t.Fatalf("quota CAS = %t, %v", updated, err)
			}
			if got := oauthcost.Find(costs, sample.Key).StandardCostMicroUSD; got != 1_250_000 {
				t.Fatalf("cost after reset = %d, want 1250000", got)
			}
			// Late logs obey the actual reset cutoff, including manual resets
			// earlier than the newly sampled period's nominal start.
			add(base.Add(10*time.Minute), 0.25)
			want := int64(1_250_000)
			if manual {
				want += 250_000
			}
			cfg, err = store.GetConfig(ctx, channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
			if err != nil {
				t.Fatal(err)
			}
			if got := oauthcost.Find(persisted.QuotaCostUsage, sample.Key).StandardCostMicroUSD; got != want {
				t.Fatalf("cost after late log = %d, want %d", got, want)
			}
		})
	}
}

func TestLog_OAuthQuotaEpochSurvivesBootstrapAndManualReset(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "oauth-quota-epoch.db")
	ctx := context.Background()
	base := time.Date(2030, time.July, 1, 12, 0, 0, 0, time.UTC)
	epochAt := base.Add(time.Hour)
	snapshot, err := json.Marshal(oauthcost.Snapshot{
		SampledAt: base.Format(time.RFC3339Nano),
		Summary: oauthcost.SnapshotSummary{
			Provider: oauthcost.ProviderCodex,
			Windows: []oauthcost.SnapshotWindow{{
				LimitName: "codex", Kind: "primary", LimitWindowSeconds: 7 * 24 * 60 * 60,
				ResetAt: base.Add(6 * 24 * time.Hour).Unix(),
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	newChannel := func(name string, oauthUsage json.RawMessage) int64 {
		t.Helper()
		credential := &codexauth.Credential{
			Type: codexauth.ChannelType, AccessToken: "at-" + name, RefreshToken: "rt-" + name,
			Expired: base.Add(24 * time.Hour).Format(time.RFC3339), AccountID: "account-" + name, PlanType: "plus",
			OAuthUsage: oauthUsage,
		}
		credentialJSON, err := credential.JSON()
		if err != nil {
			t.Fatal(err)
		}
		created, err := store.CreateConfig(ctx, &model.Config{
			Name: name, AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credentialJSON,
			URLs: model.ChannelURLs{{URL: "https://chatgpt.com/backend-api"}}, Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	load := func(channelID int64) *oauthcost.Usage {
		t.Helper()
		cfg, err := store.GetConfig(ctx, channelID)
		if err != nil {
			t.Fatal(err)
		}
		credential, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			t.Fatal(err)
		}
		return credential.QuotaCostUsage
	}
	addLog := func(channelID int64, at time.Time, cost float64) {
		t.Helper()
		if err := store.AddLog(ctx, &model.LogEntry{
			Time: newJSONTime(at), ChannelID: channelID, Model: "gpt-5.6-sol", StatusCode: http.StatusOK, Cost: cost,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 只有采样快照、还没有计数器：手动重置要按快照建窗并从 resetAt 起计数。
	sampled := newChannel("sampled", snapshot)
	if err := store.ResetOAuthQuotaCostUsage(ctx, sampled, epochAt); err != nil {
		t.Fatal(err)
	}
	usage := load(sampled)
	window := oauthcost.Find(usage, "codex|primary")
	if usage == nil || usage.EpochAt != epochAt.Unix() || window == nil ||
		oauthcost.CountFrom(window) != epochAt.Unix() || window.StandardCostMicroUSD != 0 {
		t.Fatalf("usage after reset over a bare snapshot = %#v", usage)
	}
	addLog(sampled, epochAt.Add(-time.Minute), 0.75)
	if got := oauthcost.Find(load(sampled), "codex|primary"); got == nil || got.StandardCostMicroUSD != 0 {
		t.Fatalf("pre-epoch log was counted: %#v", got)
	}
	addLog(sampled, epochAt.Add(time.Minute), 1.25)
	usage = load(sampled)
	if window = oauthcost.Find(usage, "codex|primary"); usage.EpochAt != epochAt.Unix() ||
		window == nil || window.StandardCostMicroUSD != 1_250_000 {
		t.Fatalf("usage after post-epoch log = %#v", usage)
	}

	// 什么都没有也要把纪元落盘。
	bare := newChannel("bare", nil)
	if err := store.ResetOAuthQuotaCostUsage(ctx, bare, epochAt); err != nil {
		t.Fatal(err)
	}
	if usage = load(bare); usage == nil || usage.EpochAt != epochAt.Unix() || len(usage.Windows) != 0 {
		t.Fatalf("usage after reset without windows = %#v", usage)
	}
}

func TestLog_CodexPurchasedCreditsStayOutsideWindows(t *testing.T) {
	t.Parallel()
	for _, withWindows := range []bool{false, true} {
		t.Run(fmt.Sprint("windows=", withWindows), func(t *testing.T) {
			store := newTestStore(t, "codex-credit.db")
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			samples := []oauthcost.Sample{
				{Key: "codex|primary", WindowSeconds: 18000, ResetAt: now.Add(time.Hour)},
				{Key: "codex|secondary", WindowSeconds: 604800, ResetAt: now.Add(time.Hour)},
				{Key: "codex|monthly", WindowSeconds: 2592000, ResetAt: now.Add(time.Hour)},
			}
			credential := &codexauth.Credential{Type: codexauth.ChannelType, AccessToken: "access", RefreshToken: "refresh", Expired: now.Add(24 * time.Hour).Format(time.RFC3339)}
			if withWindows {
				credential.QuotaCostUsage = oauthcost.Reconcile(nil, samples, now)
			}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			channel, err := store.CreateConfig(ctx, &model.Config{Name: "credit", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: raw, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			entries := []*model.LogEntry{
				{Time: newJSONTime(now), ChannelID: channel.ID, Model: "gpt-5.5", Cost: 1.25, CostMultiplier: 10, CodexHasCredits: true},
				{Time: newJSONTime(now.Add(time.Second)), ChannelID: channel.ID, Model: "gpt-reserve", Cost: 0.75, CodexHasCredits: true},
				{Time: newJSONTime(now.Add(2 * time.Second)), ChannelID: channel.ID, Model: "gpt-5.5", Cost: 0.5},
			}
			if err := store.BatchAddLogs(ctx, entries); err != nil {
				t.Fatal(err)
			}
			read := func() (*model.Config, *codexauth.Credential) {
				t.Helper()
				cfg, err := store.GetConfig(ctx, channel.ID)
				if err != nil {
					t.Fatal(err)
				}
				got, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
				if err != nil {
					t.Fatal(err)
				}
				if got.QuotaCostUsage == nil || got.QuotaCostUsage.CreditStandardCostMicroUSD != 2000000 {
					t.Fatalf("credit cost=%+v", got.QuotaCostUsage)
				}
				return cfg, got
			}
			cfg, got := read()
			// Refresh also bootstraps windows when the first credit request preceded any quota sample.
			got.QuotaCostUsage = oauthcost.Reconcile(got.QuotaCostUsage, samples, now)
			raw, err = got.JSON()
			if err != nil {
				t.Fatal(err)
			}
			updated, _, err := store.CompareAndSwapOAuthUsage(ctx, channel.ID, model.AuthTypeCodexOAuth, cfg.OAuthCredential, raw)
			if err != nil || !updated {
				t.Fatalf("refresh=%t, %v", updated, err)
			}
			_, got = read()
			for _, window := range got.QuotaCostUsage.Windows {
				if window.StandardCostMicroUSD != 500000 {
					t.Fatalf("window %s cost=%d", window.Key, window.StandardCostMicroUSD)
				}
			}
			if err := store.ResetOAuthQuotaCostUsage(ctx, channel.ID, now); err != nil {
				t.Fatal(err)
			}
			_, got = read()
			for _, window := range got.QuotaCostUsage.Windows {
				if window.StandardCostMicroUSD != 500000 {
					t.Fatalf("reset window %s cost=%d", window.Key, window.StandardCostMicroUSD)
				}
			}
			logs, err := store.ListLogs(ctx, now.Add(-time.Second), 10, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			var credits int
			for _, entry := range logs {
				if entry.CodexHasCredits {
					credits++
				}
			}
			if len(logs) != 3 || credits != 2 {
				t.Fatalf("persisted logs=%d credit logs=%d", len(logs), credits)
			}
			// A new quota cycle cannot reset the independent credit counter.
			if err := store.AddLog(ctx, &model.LogEntry{Time: newJSONTime(now.Add(2 * time.Hour)), ChannelID: channel.ID, Model: "gpt-5.5", Cost: 0.1}); err != nil {
				t.Fatal(err)
			}
			read()
		})
	}
}
