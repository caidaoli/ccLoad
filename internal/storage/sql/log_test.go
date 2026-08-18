package sql_test

import (
	"context"
	"encoding/json"
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
		monthly := oauthcost.Find(got.QuotaCostUsage, "codex|monthly")
		if weekly == nil || monthly == nil {
			t.Fatalf("quota cost usage missing: %#v", got.QuotaCostUsage)
		}
		if weekly.StandardCostMicroUSD != want || monthly.StandardCostMicroUSD != want {
			t.Fatalf("quota costs = weekly %d monthly %d, want %d",
				weekly.StandardCostMicroUSD, monthly.StandardCostMicroUSD, want)
		}
		return got
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
