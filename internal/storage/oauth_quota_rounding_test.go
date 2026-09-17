//go:build postgres_integration || mysql_integration

package storage

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/util"
)

// quotaRoundingTieCosts 的每个值乘 1e6 后都恰好落在半整数上，且整数部分为偶数。
// 此时 half-away-from-zero（Go 的 math.Round）进位、half-even（Postgres/MySQL 的
// ROUND 作用于双精度时的 C rint 语义）舍去，逐条差 1 微美元。
var quotaRoundingTieCosts = []float64{0.0000025, 0.0000045, 0.0000065, 0.0000125, 12.3456785}

// assertOAuthQuotaRoundingMatchesGo 校验 SQL 汇总与增量记账的取整同源。
//
// 汇总少算的偏差对正成本恒为 0 或 -1 微美元、方向单一，不会正负抵消；而手动
// 重置基线（sumOAuthQuotaCostByFamily）与对称差的 added 分支都不经过
// max(已累计, 汇总) 保护，少算会直接落进持久化累计。两条无保护路径各验一次。
func assertOAuthQuotaRoundingMatchesGo(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()

	var wantMicroUSD int64
	for _, cost := range quotaRoundingTieCosts {
		micro, err := util.USDToMicroUSDSafe(cost)
		if err != nil {
			t.Fatalf("USDToMicroUSDSafe(%v): %v", cost, err)
		}
		wantMicroUSD += micro
	}

	windowStart := time.Date(2026, time.September, 17, 6, 0, 0, 0, time.UTC)
	used := 10.0
	sample := oauthcost.Sample{
		Key: "gemini models|quota", Family: oauthcost.FamilyGemini,
		WindowSeconds: 18000, ResetAt: windowStart.Add(5 * time.Hour),
		UsedPercent: &used, SampledAt: windowStart,
	}
	credential := &antigravityauth.Credential{
		Type: "antigravity", AccessToken: "test", RefreshToken: "test",
		Expired:        "2030-01-01T00:00:00Z",
		QuotaCostUsage: oauthcost.Reconcile(nil, []oauthcost.Sample{sample}, windowStart),
	}
	raw, err := credential.JSON()
	if err != nil {
		t.Fatalf("credential.JSON: %v", err)
	}
	channel, err := store.CreateConfig(ctx, &model.Config{
		Name: "quota-rounding", AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: raw,
		URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	for i, cost := range quotaRoundingTieCosts {
		entry := &model.LogEntry{
			Time: model.JSONTime{Time: windowStart.Add(time.Duration(i+1) * time.Minute)},
			// 同一模型即同一 GROUP BY 分组：取整必须发生在组内逐行，
			// 而不是对分组求和后再取整（后者会给出 12345704/12345705）。
			ChannelID: channel.ID, Model: "alias", ActualModel: "gemini-3.8-flash-high", Cost: cost,
		}
		if err := store.AddLog(ctx, entry); err != nil {
			t.Fatalf("AddLog(%v): %v", cost, err)
		}
	}

	// 路径一：手动重置基线。sumOAuthQuotaCostByFamily 的结果直接赋给累计，无 max 保护。
	if err := store.ResetOAuthQuotaCostUsage(ctx, channel.ID, windowStart); err != nil {
		t.Fatalf("ResetOAuthQuotaCostUsage: %v", err)
	}
	cfg, err := store.GetConfig(ctx, channel.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	current, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if got := oauthcost.Find(current.QuotaCostUsage, sample.Key).StandardCostMicroUSD; got != wantMicroUSD {
		t.Fatalf("手动重置基线 = %d, want %d（逐条取整须与 util.USDToMicroUSDSafe 同源）", got, wantMicroUSD)
	}

	// 路径二：周期切换后按当前边界重算。新窗口累计为零，汇总值直接成为持久化结果。
	switched := windowStart.Add(time.Hour)
	sample.ResetAt, sample.SampledAt = switched.Add(4*time.Hour), switched
	current.QuotaCostUsage = oauthcost.Reconcile(current.QuotaCostUsage, []oauthcost.Sample{sample}, switched)
	payload, err := current.JSON()
	if err != nil {
		t.Fatalf("credential.JSON: %v", err)
	}
	updated, costs, err := store.CompareAndSwapOAuthUsage(ctx, channel.ID,
		model.AuthTypeAntigravityOAuth, cfg.OAuthCredential, payload)
	if err != nil || !updated {
		t.Fatalf("CompareAndSwapOAuthUsage = %t, %v", updated, err)
	}
	if got := oauthcost.Find(costs, sample.Key).StandardCostMicroUSD; got != wantMicroUSD {
		t.Fatalf("周期切换重算 = %d, want %d（逐条取整须与 util.USDToMicroUSDSafe 同源）", got, wantMicroUSD)
	}
}
