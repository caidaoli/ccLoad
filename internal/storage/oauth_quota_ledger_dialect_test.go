package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/util"
)

var ledgerDialectTieCosts = []float64{0.0000025, 0.0000045, 0.0000065, 0.0000125, 12.3456785}

func createLedgerDialectChannel(t *testing.T, ctx context.Context, store Store, name string, usage *oauthcost.Usage) int64 {
	t.Helper()
	raw, err := (&antigravityauth.Credential{Type: "antigravity", AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", QuotaCostUsage: usage}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.CreateConfig(ctx, &model.Config{Name: name, AuthType: model.AuthTypeAntigravityOAuth,
		OAuthCredential: raw, URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return cfg.ID
}

// assertOAuthQuotaLedgerDialect 校验逐条取整、冲突累加和跨分块批量求和。
func assertOAuthQuotaLedgerDialect(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, time.September, 17, 6, 0, 0, 0, time.UTC)
	tieUsage := &oauthcost.Usage{Windows: []*oauthcost.Window{{Key: "gemini models|quota", Family: oauthcost.FamilyGemini,
		WindowSeconds: 18000, StartedAt: base.Unix(), ResetAt: base.Add(5 * time.Hour).Unix()}}}
	tie := createLedgerDialectChannel(t, ctx, store, "ledger-dialect-tie", tieUsage)
	var want int64
	for _, cost := range ledgerDialectTieCosts {
		micro, err := util.USDToMicroUSDSafe(cost)
		if err != nil {
			t.Fatal(err)
		}
		want += micro
		if err := store.AddLog(ctx, &model.LogEntry{Time: model.JSONTime{Time: base.Add(time.Minute)}, ChannelID: tie,
			Model: "alias", ActualModel: "gemini-3.8-flash-high", StatusCode: 200, Cost: cost}); err != nil {
			t.Fatal(err)
		}
	}
	const windowCount = 204
	manyUsage := &oauthcost.Usage{}
	var manyLogs []*model.LogEntry
	many := createLedgerDialectChannel(t, ctx, store, "ledger-dialect-many", nil)
	for i := range windowCount {
		start := base.Add(time.Duration(i) * time.Minute)
		manyUsage.Windows = append(manyUsage.Windows, &oauthcost.Window{Key: fmt.Sprintf("gemini|w%03d", i),
			Family: oauthcost.FamilyGemini, WindowSeconds: 60, StartedAt: start.Unix(), ResetAt: start.Add(time.Minute).Unix()})
		manyLogs = append(manyLogs, &model.LogEntry{Time: model.JSONTime{Time: start.Add(time.Second)}, ChannelID: many,
			Model: "gemini-3.8-flash-high", StatusCode: 200, Cost: float64(i+1) / 1e6})
	}
	other := createLedgerDialectChannel(t, ctx, store, "ledger-dialect-other", nil)
	otherUsage := &oauthcost.Usage{Windows: []*oauthcost.Window{{Key: "gemini|b", Family: oauthcost.FamilyGemini,
		WindowSeconds: 60, StartedAt: base.Unix(), ResetAt: base.Add(time.Minute).Unix()}}}
	manyLogs = append(manyLogs, &model.LogEntry{Time: model.JSONTime{Time: base.Add(30 * time.Second)}, ChannelID: other,
		Model: "gemini-3.8-flash-high", StatusCode: 200, Cost: 0.000777})
	if err := store.BatchAddLogs(ctx, manyLogs); err != nil {
		t.Fatal(err)
	}
	views, err := store.OAuthQuotaCostViews(ctx, map[int64]*oauthcost.Usage{tie: tieUsage, many: manyUsage, other: otherUsage}, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := views[tie].FindWindow("gemini models|quota"); got == nil || got.StandardCostMicroUSD != want {
		t.Fatalf("tie cost = %+v, want %d", got, want)
	}
	if got := views[many]; got == nil || len(got.Windows) != windowCount {
		t.Fatalf("many view = %+v, want %d windows", got, windowCount)
	}
	for i, window := range views[many].Windows {
		if window.Key != fmt.Sprintf("gemini|w%03d", i) || window.StandardCostMicroUSD != int64(i+1) {
			t.Fatalf("window %d = %+v, want cost %d", i, window, i+1)
		}
	}
	if got := views[other].FindWindow("gemini|b"); got == nil || got.StandardCostMicroUSD != 777 {
		t.Fatalf("other channel cost = %+v, want 777", got)
	}
}

func TestOAuthQuotaLedgerDialect_SQLite(t *testing.T) {
	store := createTestSQLiteStore(t)
	t.Cleanup(func() { _ = store.Close() })
	assertOAuthQuotaLedgerDialect(t, store)
}
