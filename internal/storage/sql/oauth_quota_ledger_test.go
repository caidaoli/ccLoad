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
	"ccLoad/internal/storage"
	sqlstore "ccLoad/internal/storage/sql"
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
	rows, err := store.ListOAuthQuotaLedgerReplica(context.Background(), channelID, 0, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestOAuthQuotaLedger_AggregatesEligibleLogsBySecondAndModel(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, "oauth-quota-ledger.db")
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
	wantSlices := []sqlstore.OAuthQuotaLedgerSlice{{ChannelID: antigravity, SliceStart: base.Unix()}, {ChannelID: codex, SliceStart: base.Unix()}}
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
	store := newTestStore(t, "oauth-quota-ledger-rounding.db")
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
