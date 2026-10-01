package sql_test

import (
	"context"
	"math"
	"net/http"
	"reflect"
	"testing"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/storage"
	sqlstore "ccLoad/internal/storage/sql"

	"github.com/tidwall/sjson"
)

func createOAuthLedgerChannel(t *testing.T, ctx context.Context, store storage.Store, name, authType string, now time.Time) int64 {
	t.Helper()
	var raw string
	var err error
	switch authType {
	case model.AuthTypeAntigravityOAuth:
		raw, err = (&antigravityauth.Credential{Type: antigravityauth.ChannelType, AccessToken: "access",
			RefreshToken: "refresh", Expired: now.Add(24 * time.Hour).Format(time.RFC3339)}).JSON()
	case model.AuthTypeCodexOAuth:
		raw, err = (&codexauth.Credential{Type: codexauth.ChannelType, AccessToken: "access",
			RefreshToken: "refresh", Expired: now.Add(24 * time.Hour).Format(time.RFC3339)}).JSON()
	default:
		t.Fatalf("unsupported auth type %q", authType)
	}
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.CreateConfig(ctx, &model.Config{Name: name, AuthType: authType, OAuthCredential: raw,
		URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return cfg.ID
}

func ledgerRows(t *testing.T, store *sqlstore.SQLStore, channelID int64) []sqlstore.OAuthQuotaLedgerRow {
	t.Helper()
	all, err := store.ListOAuthQuotaLedgerRangeReplica(context.Background(), math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	var rows []sqlstore.OAuthQuotaLedgerRow
	for _, row := range all {
		if row.ChannelID == channelID {
			rows = append(rows, row.OAuthQuotaLedgerRow)
		}
	}
	return rows
}

func TestOAuthQuotaLedger_AggregatesEligibleLogsBySecondAndModel(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ss := store.(*sqlstore.SQLStore)
	ctx := context.Background()
	base := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	antigravity := createOAuthLedgerChannel(t, ctx, store, "ledger-antigravity", model.AuthTypeAntigravityOAuth, base)
	codex := createOAuthLedgerChannel(t, ctx, store, "ledger-codex", model.AuthTypeCodexOAuth, base)
	apiKey := createTestChannel(t, ctx, store, "ledger-api-key")
	const missingChannel = int64(987654)

	effects, err := ss.BatchAddLogsWithOAuthQuotaCost(ctx, []*model.LogEntry{
		{Time: newJSONTime(base.Add(100 * time.Millisecond)), ChannelID: antigravity, Model: "gemini-flash-alias",
			ActualModel: "gemini-3.6-flash-high", StatusCode: http.StatusOK, Cost: 0.000123},
		{Time: newJSONTime(base.Add(900 * time.Millisecond)), ChannelID: antigravity, Model: "gemini-3.6-flash-high",
			StatusCode: http.StatusBadGateway, Cost: 0.000002},
		{Time: newJSONTime(base.Add(2 * time.Second)), ChannelID: antigravity, Model: "claude-opus-4-6",
			StatusCode: http.StatusOK, Cost: 0.000045},
		{Time: newJSONTime(base.Add(3 * time.Second)), ChannelID: antigravity, Model: "gemini-3.6-pro",
			LogSource: model.LogSourceJev, StatusCode: http.StatusOK, Cost: 1},
		{Time: newJSONTime(base.Add(4 * time.Second)), ChannelID: antigravity, Model: "gemini-3.6-pro",
			StatusCode: http.StatusOK},
		{Time: newJSONTime(base), ChannelID: codex, Model: "gpt-5.5", StatusCode: http.StatusOK, Cost: 0.25, CodexHasCredits: true},
		{Time: newJSONTime(base.Add(time.Second)), ChannelID: codex, Model: "gpt-5.5", StatusCode: http.StatusOK, Cost: 0.5},
		{Time: newJSONTime(base), ChannelID: apiKey, Model: "gpt-4", StatusCode: http.StatusOK, Cost: 1},
		{Time: newJSONTime(base), ChannelID: missingChannel, Model: "gemini-3.6-pro", StatusCode: http.StatusOK, Cost: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(effects.CredentialChannelIDs, []int64{codex}) {
		t.Fatalf("credential channels = %v, want [%d]", effects.CredentialChannelIDs, codex)
	}
	wantSlices := []int64{base.Unix()}
	if !reflect.DeepEqual(effects.LedgerSlices, wantSlices) {
		t.Fatalf("ledger slices = %#v, want %#v", effects.LedgerSlices, wantSlices)
	}
	wantAntigravity := []sqlstore.OAuthQuotaLedgerRow{
		{BucketAt: base.Unix(), Model: "gemini-3.6-flash-high", CostMicroUSD: 125},
		{BucketAt: base.Add(2 * time.Second).Unix(), Model: "claude-opus-4-6", CostMicroUSD: 45},
	}
	if got := ledgerRows(t, ss, antigravity); !reflect.DeepEqual(got, wantAntigravity) {
		t.Fatalf("antigravity ledger = %#v, want %#v", got, wantAntigravity)
	}
	wantCodex := []sqlstore.OAuthQuotaLedgerRow{{BucketAt: base.Add(time.Second).Unix(), Model: "gpt-5.5", CostMicroUSD: 500_000}}
	if got := ledgerRows(t, ss, codex); !reflect.DeepEqual(got, wantCodex) {
		t.Fatalf("codex ledger = %#v, want %#v", got, wantCodex)
	}
	for _, id := range []int64{apiKey, missingChannel} {
		if got := ledgerRows(t, ss, id); len(got) != 0 {
			t.Fatalf("channel %d must not have ledger rows: %#v", id, got)
		}
	}
	if _, err := ss.AddLogWithOAuthQuotaCost(ctx, &model.LogEntry{Time: newJSONTime(base.Add(500 * time.Millisecond)),
		ChannelID: antigravity, Model: "gemini-3.6-flash-high", StatusCode: http.StatusOK, Cost: 0.00001}); err != nil {
		t.Fatal(err)
	}
	if got := ledgerRows(t, ss, antigravity); len(got) != 2 || got[0].CostMicroUSD != 135 {
		t.Fatalf("accumulated ledger = %#v, want first row 135", got)
	}
	if err := store.CleanupOAuthQuotaLedgerBefore(ctx, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := ledgerRows(t, ss, antigravity); len(got) != 1 || got[0].Model != "claude-opus-4-6" {
		t.Fatalf("ledger after cleanup = %#v", got)
	}
	if got := ledgerRows(t, ss, codex); !reflect.DeepEqual(got, wantCodex) {
		t.Fatalf("codex ledger after cleanup = %#v", got)
	}
	if err := store.DeleteConfig(ctx, codex); err != nil {
		t.Fatal(err)
	}
	if got := ledgerRows(t, ss, codex); len(got) != 0 {
		t.Fatalf("deleted channel ledger = %#v", got)
	}
}

func TestOAuthQuotaLedger_RoundsEachManualTestLog(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ss := store.(*sqlstore.SQLStore)
	ctx := context.Background()
	at := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	channelID := createOAuthLedgerChannel(t, ctx, store, "ledger-rounding", model.AuthTypeAntigravityOAuth, at)

	_, err := ss.BatchAddLogsWithOAuthQuotaCost(ctx, []*model.LogEntry{
		{Time: newJSONTime(at), ChannelID: channelID, Model: "gemini-3.6-pro",
			LogSource: model.LogSourceManualTest, StatusCode: http.StatusOK, Cost: 0.0000005},
		{Time: newJSONTime(at.Add(500 * time.Millisecond)), ChannelID: channelID, Model: "gemini-3.6-pro",
			LogSource: model.LogSourceManualTest, StatusCode: http.StatusOK, Cost: 0.0000005},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []sqlstore.OAuthQuotaLedgerRow{{BucketAt: at.Unix(), Model: "gemini-3.6-pro", CostMicroUSD: 2}}
	if got := ledgerRows(t, ss, channelID); !reflect.DeepEqual(got, want) {
		t.Fatalf("manual test ledger = %#v, want per-log rounded cost %#v", got, want)
	}
}

func TestOAuthQuotaLedger_ViewSumsWithinSecondBoundariesAndIgnoresLegacyCost(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ss := store.(*sqlstore.SQLStore)
	ctx := context.Background()
	start := time.Date(2026, time.September, 17, 6, 0, 0, 0, time.UTC)
	resetAt := start.Add(5 * time.Hour)
	raw, err := (&codexauth.Credential{Type: codexauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: start.Add(24 * time.Hour).Format(time.RFC3339),
		QuotaCostUsage: oauthcost.Reconcile(nil, []oauthcost.Sample{{Key: "codex|primary", Family: oauthcost.FamilyCodex,
			WindowSeconds: 18000, ResetAt: resetAt}}, start)}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.CreateConfig(ctx, &model.Config{Name: "ledger-view", AuthType: model.AuthTypeCodexOAuth,
		OAuthCredential: raw, URLs: model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	legacy := raw
	for path, value := range map[string]int64{
		"quota_cost_usage.windows.0.standard_cost_microusd": 999_999,
		"quota_cost_usage.windows.0.accounted_from":         start.Unix(),
		"quota_cost_usage.windows.0.accounted_until":        start.Add(time.Hour).Unix(),
	} {
		if legacy, err = sjson.Set(legacy, path, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ss.ExecContext(ctx, `UPDATE channels SET oauth_credential = ? WHERE id = ?`, legacy, cfg.ID); err != nil {
		t.Fatal(err)
	}
	logAt := func(at time.Time, modelName string, cost float64) *model.LogEntry {
		return &model.LogEntry{Time: newJSONTime(at), ChannelID: cfg.ID, Model: modelName, StatusCode: http.StatusOK, Cost: cost}
	}
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		logAt(start.Add(-time.Millisecond), "gpt-5.5", 0.000001),
		logAt(start, "gpt-5.5", 0.00001),
		logAt(resetAt.Add(-time.Millisecond), "gpt-5.5", 0.0001),
		logAt(resetAt, "gpt-5.5", 0.001),
		logAt(start.Add(time.Hour), "gpt-5.3-codex-spark", 0.01),
	}); err != nil {
		t.Fatal(err)
	}
	if got := quotaCostAt(t, ctx, store, cfg.ID, "codex|primary", start.Add(time.Hour)); got != 110 {
		t.Fatalf("window cost = %d, want 110", got)
	}
	current, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codexauth.ParseCredential([]byte(current.OAuthCredential)); err != nil {
		t.Fatalf("legacy credential must still parse: %v", err)
	}
}

func TestOAuthQuotaLedger_ViewSplitsSharedScanByWindowRange(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	n := base.Unix()
	nested := createOAuthLedgerChannel(t, ctx, store, "ledger-nested", model.AuthTypeCodexOAuth, base)
	empty := createOAuthLedgerChannel(t, ctx, store, "ledger-empty", model.AuthTypeCodexOAuth, base)
	logAt := func(channelID int64, offset time.Duration, modelName string, cost float64) *model.LogEntry {
		return &model.LogEntry{Time: newJSONTime(base.Add(offset)), ChannelID: channelID, Model: modelName,
			StatusCode: http.StatusOK, Cost: cost}
	}
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		logAt(nested, -5*24*time.Hour, "gpt-5.5", 0.000001),
		logAt(nested, -2*time.Hour, "gpt-5.5", 0.00001),
		logAt(nested, -30*time.Minute, "gpt-5.5", 0.0001),
		logAt(nested, -30*time.Minute, "gpt-5.3-codex-spark", 0.001),
		logAt(nested, -3*24*time.Hour, "gpt-5.3-codex-spark", 0.01),
		logAt(empty, -30*time.Minute, "gpt-5.5", 0.1),
	}); err != nil {
		t.Fatal(err)
	}
	views, err := store.OAuthQuotaCostViews(ctx, map[int64]*oauthcost.Usage{
		nested: {Windows: []*oauthcost.Window{
			{Key: "codex|primary", Family: oauthcost.FamilyCodex, WindowSeconds: 18000, StartedAt: n - 3600, ResetAt: n + 14400},
			{Key: "codex|secondary", Family: oauthcost.FamilyCodex, WindowSeconds: 604800, StartedAt: n - 6*86400, ResetAt: n + 86400},
			{Key: "codex-spark|secondary", Family: oauthcost.FamilySpark, WindowSeconds: 604800, StartedAt: n - 2*86400, ResetAt: n + 5*86400},
		}},
		// 计数起点已到重置点：窗口仍要出现在视图中，成本为 0。
		empty: {Windows: []*oauthcost.Window{
			{Key: "codex|primary", Family: oauthcost.FamilyCodex, WindowSeconds: 18000, StartedAt: n - 3600, ResetAt: n + 14400, CountFromAt: n + 14400},
		}},
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int64{"codex|primary": 100, "codex|secondary": 111, "codex-spark|secondary": 1000} {
		if got := viewCost(views[nested], key); got != want {
			t.Fatalf("%s cost = %d, want %d", key, got, want)
		}
	}
	if got := viewCost(views[empty], "codex|primary"); got != 0 {
		t.Fatalf("empty-range window cost = %d, want 0", got)
	}
}

func TestOAuthQuotaLedger_TransientRollbackRestoresDespitePriceChange(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	resetAt := now.Add(4 * 24 * time.Hour)
	const key = "gemini models|quota"
	samples := func(used float64, at time.Time) []oauthcost.Sample {
		return []oauthcost.Sample{{Key: key, Family: oauthcost.FamilyGemini, WindowSeconds: 604800,
			ResetAt: resetAt, UsedPercent: &used, SampledAt: at}}
	}
	first := now.Add(-3 * time.Hour)
	raw, err := (&antigravityauth.Credential{Type: antigravityauth.ChannelType, AccessToken: "access",
		RefreshToken: "refresh", Expired: now.Add(24 * time.Hour).Format(time.RFC3339),
		QuotaCostUsage: oauthcost.Reconcile(nil, samples(40, first), first)}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(ctx, &model.Config{Name: "ledger-rollback", AuthType: model.AuthTypeAntigravityOAuth,
		OAuthCredential: raw, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	addLog := func(at time.Time, cost float64) {
		t.Helper()
		if err := store.AddLog(ctx, &model.LogEntry{Time: newJSONTime(at), ChannelID: channel.ID,
			Model: "gemini-3.8-flash-high", StatusCode: http.StatusOK, Cost: cost}); err != nil {
			t.Fatal(err)
		}
	}
	cas := func(used float64, at time.Time) int64 {
		t.Helper()
		cfg, err := store.GetConfig(ctx, channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		current, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			t.Fatal(err)
		}
		current.QuotaCostUsage = oauthcost.Reconcile(current.QuotaCostUsage, samples(used, at), at)
		payload, err := current.JSON()
		if err != nil {
			t.Fatal(err)
		}
		updated, err := store.CompareAndSwapOAuthUsage(ctx, channel.ID, model.AuthTypeAntigravityOAuth, cfg.OAuthCredential, payload)
		if err != nil || !updated {
			t.Fatalf("quota CAS = %t, %v", updated, err)
		}
		return quotaCostAt(t, ctx, store, channel.ID, key, at)
	}
	addLog(now.Add(-4*time.Hour), 4)
	cut := now.Add(-2 * time.Hour)
	if got := cas(5, cut); got != 0 {
		t.Fatalf("cost right after cut = %d, want 0", got)
	}
	addLog(cut.Add(30*time.Minute), 4)
	if got := cas(44, now.Add(-time.Hour)); got != 8_000_000 {
		t.Fatalf("cost after transient restore = %d, want 8000000", got)
	}
	if got := quotaCostAt(t, ctx, store, channel.ID, key, now); got != 8_000_000 {
		t.Fatalf("restored view = %d, want 8000000", got)
	}
	cfg, err := store.GetConfig(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if window := oauthcost.Find(persisted.QuotaCostUsage, key); window == nil ||
		oauthcost.CountFrom(window) >= now.Add(-4*time.Hour).Unix() || window.Rollback != nil {
		t.Fatalf("persisted window = %#v, want restored count start without evidence", window)
	}
}
