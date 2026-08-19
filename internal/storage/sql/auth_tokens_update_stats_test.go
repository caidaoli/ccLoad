package sql_test

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
)

func floatNear(a, b, epsilon float64) bool {
	return math.Abs(a-b) < epsilon
}

func TestUpdateTokenStats_SingleUpdateSemantics(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "token_stats.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	tokenHash := "token_stats_hash"

	if err := store.CreateAuthToken(ctx, &model.AuthToken{
		Token:             tokenHash,
		Description:       "test",
		CreatedAt:         time.Now(),
		IsActive:          true,
		CostLimitMicroUSD: 0,
	}); err != nil {
		t.Fatalf("create auth token: %v", err)
	}

	// 失败请求：只累加失败次数；平均值仍应更新；token与费用不应累加。
	if err := store.UpdateTokenStats(ctx, tokenHash, false, 2.0, false, 0, 10, 20, 3, 4, 1.23, 0.25, time.Now()); err != nil {
		t.Fatalf("update token stats (failure): %v", err)
	}

	got, err := store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.SuccessCount != 0 || got.FailureCount != 1 {
		t.Fatalf("unexpected counts after failure: success=%d failure=%d", got.SuccessCount, got.FailureCount)
	}
	if got.PromptTokensTotal != 0 || got.CompletionTokensTotal != 0 || got.CacheReadTokensTotal != 0 || got.CacheCreationTokensTotal != 0 {
		t.Fatalf("unexpected token totals after failure: prompt=%d completion=%d cache_read=%d cache_create=%d",
			got.PromptTokensTotal, got.CompletionTokensTotal, got.CacheReadTokensTotal, got.CacheCreationTokensTotal)
	}
	if got.TotalCostUSD != 0 || got.EffectiveCostUSD != 0 || got.CostUsedMicroUSD != 0 {
		t.Fatalf("unexpected cost after failure: total_cost_usd=%v effective_cost_usd=%v cost_used_microusd=%d", got.TotalCostUSD, got.EffectiveCostUSD, got.CostUsedMicroUSD)
	}
	if got.NonStreamCount != 1 || got.NonStreamAvgRT != 2.0 {
		t.Fatalf("unexpected non-stream stats after failure: count=%d avg=%v", got.NonStreamCount, got.NonStreamAvgRT)
	}

	// 成功请求：累加成功次数、token与费用；平均值继续更新。
	completedAt := time.Now()
	if err := store.UpdateTokenStats(ctx, tokenHash, true, 4.0, false, 0, 10, 20, 3, 4, 0.5, 0.25, completedAt); err != nil {
		t.Fatalf("update token stats (success): %v", err)
	}

	got, err = store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.SuccessCount != 1 || got.FailureCount != 1 {
		t.Fatalf("unexpected counts after success: success=%d failure=%d", got.SuccessCount, got.FailureCount)
	}
	if got.PromptTokensTotal != 10 || got.CompletionTokensTotal != 20 || got.CacheReadTokensTotal != 3 || got.CacheCreationTokensTotal != 4 {
		t.Fatalf("unexpected token totals after success: prompt=%d completion=%d cache_read=%d cache_create=%d",
			got.PromptTokensTotal, got.CompletionTokensTotal, got.CacheReadTokensTotal, got.CacheCreationTokensTotal)
	}
	if got.TotalCostUSD != 0.5 {
		t.Fatalf("unexpected total_cost_usd after success: %v", got.TotalCostUSD)
	}
	if got.EffectiveCostUSD != 0.25 {
		t.Fatalf("unexpected effective_cost_usd after success: %v", got.EffectiveCostUSD)
	}
	if got.CostUsedMicroUSD != util.USDToMicroUSD(0.25) {
		t.Fatalf("unexpected cost_used_microusd after success: %d", got.CostUsedMicroUSD)
	}
	if got.NonStreamCount != 2 || got.NonStreamAvgRT != 3.0 {
		t.Fatalf("unexpected non-stream stats after success: count=%d avg=%v", got.NonStreamCount, got.NonStreamAvgRT)
	}
	if got.LastUsedAt == nil || *got.LastUsedAt <= 0 {
		t.Fatalf("expected last_used_at to be set, got=%v", got.LastUsedAt)
	}
	if got.CostDailyUsedMicroUSD != util.USDToMicroUSD(0.25) {
		t.Fatalf("unexpected cost_daily_used_microusd after success: %d", got.CostDailyUsedMicroUSD)
	}
	if got.CostMonthlyUsedMicroUSD != util.USDToMicroUSD(0.25) {
		t.Fatalf("unexpected cost_monthly_used_microusd after success: %d", got.CostMonthlyUsedMicroUSD)
	}
	dayStart, monthStart := model.AuthTokenCostPeriodStarts(completedAt)
	if got.CostDailyPeriodStart != dayStart {
		t.Fatalf("cost_daily_period_start=%d, want %d", got.CostDailyPeriodStart, dayStart)
	}
	if got.CostMonthlyPeriodStart != monthStart {
		t.Fatalf("cost_monthly_period_start=%d, want %d", got.CostMonthlyPeriodStart, monthStart)
	}
}

func TestUpdateTokenStats_UsesCompletionPeriodWithoutRegressingNewerWindows(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "token_period_stats.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	tokenHash := "period_token_hash"
	if err := store.CreateAuthToken(ctx, &model.AuthToken{
		Token:       tokenHash,
		Description: "period test",
		CreatedAt:   time.Now(),
		IsActive:    true,
	}); err != nil {
		t.Fatalf("create auth token: %v", err)
	}
	now := time.Now()
	oldCompletedAt := now.AddDate(0, 0, -1)
	oldDayStart, oldMonthStart := model.AuthTokenCostPeriodStarts(oldCompletedAt)
	currentDayStart, currentMonthStart := model.AuthTokenCostPeriodStarts(now)

	if err := store.UpdateTokenStats(ctx, tokenHash, true, 1.0, false, 0, 1, 1, 0, 0, 0.1, 0.1, oldCompletedAt); err != nil {
		t.Fatalf("update old-period token stats: %v", err)
	}

	got, err := store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.CostDailyPeriodStart != oldDayStart || got.CostDailyUsedMicroUSD != util.USDToMicroUSD(0.1) {
		t.Fatalf("old completion assigned to daily period (%d,%d), want (%d,%d)",
			got.CostDailyPeriodStart, got.CostDailyUsedMicroUSD, oldDayStart, util.USDToMicroUSD(0.1))
	}
	if got.CostMonthlyPeriodStart != oldMonthStart || got.CostMonthlyUsedMicroUSD != util.USDToMicroUSD(0.1) {
		t.Fatalf("old completion assigned to monthly period (%d,%d), want (%d,%d)",
			got.CostMonthlyPeriodStart, got.CostMonthlyUsedMicroUSD, oldMonthStart, util.USDToMicroUSD(0.1))
	}
	if got.LastUsedAt == nil || *got.LastUsedAt != oldCompletedAt.UnixMilli() {
		t.Fatalf("last_used_at=%v, want %d", got.LastUsedAt, oldCompletedAt.UnixMilli())
	}

	if err := store.UpdateTokenStats(ctx, tokenHash, true, 1.0, false, 0, 1, 1, 0, 0, 0.2, 0.2, now); err != nil {
		t.Fatalf("update current-period token stats: %v", err)
	}
	// 再迟到一个旧周期事件：总额仍应记账，但当前日/月窗口和 last_used_at 不得倒退。
	if err := store.UpdateTokenStats(ctx, tokenHash, true, 1.0, false, 0, 1, 1, 0, 0, 0.1, 0.1, oldCompletedAt); err != nil {
		t.Fatalf("update delayed old-period token stats: %v", err)
	}
	got, err = store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.CostUsedMicroUSD != util.USDToMicroUSD(0.4) {
		t.Fatalf("total used=%d, want %d", got.CostUsedMicroUSD, util.USDToMicroUSD(0.4))
	}
	if got.CostDailyPeriodStart != currentDayStart || got.CostDailyUsedMicroUSD != util.USDToMicroUSD(0.2) {
		t.Fatalf("current daily period regressed: got (%d,%d), want (%d,%d)",
			got.CostDailyPeriodStart, got.CostDailyUsedMicroUSD, currentDayStart, util.USDToMicroUSD(0.2))
	}
	wantMonthlyUsed := util.USDToMicroUSD(0.2)
	if oldMonthStart == currentMonthStart {
		wantMonthlyUsed = util.USDToMicroUSD(0.4)
	}
	if got.CostMonthlyPeriodStart != currentMonthStart || got.CostMonthlyUsedMicroUSD != wantMonthlyUsed {
		t.Fatalf("current monthly period regressed: got (%d,%d), want (%d,%d)",
			got.CostMonthlyPeriodStart, got.CostMonthlyUsedMicroUSD, currentMonthStart, wantMonthlyUsed)
	}
	if got.LastUsedAt == nil || *got.LastUsedAt != now.UnixMilli() {
		t.Fatalf("last_used_at regressed: got %v, want %d", got.LastUsedAt, now.UnixMilli())
	}
}

func TestUpdateTokenStats_StreamingRequest(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "streaming_stats.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	tokenHash := "streaming_token_hash"

	if err := store.CreateAuthToken(ctx, &model.AuthToken{
		Token:             tokenHash,
		Description:       "streaming test",
		CreatedAt:         time.Now(),
		IsActive:          true,
		CostLimitMicroUSD: 0,
	}); err != nil {
		t.Fatalf("create auth token: %v", err)
	}

	// 第一次流式请求：TTFB = 100ms
	if err := store.UpdateTokenStats(ctx, tokenHash, true, 0, true, 100.0, 10, 20, 0, 0, 0.1, 0.1, time.Now()); err != nil {
		t.Fatalf("update token stats (streaming 1): %v", err)
	}

	got, err := store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.StreamCount != 1 || got.StreamAvgTTFB != 100.0 {
		t.Fatalf("unexpected stream stats after first request: count=%d avg=%v", got.StreamCount, got.StreamAvgTTFB)
	}
	if got.NonStreamCount != 0 {
		t.Fatalf("non-stream count should remain 0 for streaming request: %d", got.NonStreamCount)
	}

	// 第二次流式请求：TTFB = 200ms，期望平均值 = (100+200)/2 = 150
	if err := store.UpdateTokenStats(ctx, tokenHash, true, 0, true, 200.0, 5, 10, 0, 0, 0.05, 0.05, time.Now()); err != nil {
		t.Fatalf("update token stats (streaming 2): %v", err)
	}

	got, err = store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.StreamCount != 2 || got.StreamAvgTTFB != 150.0 {
		t.Fatalf("unexpected stream stats after second request: count=%d avg=%v (expected count=2 avg=150)", got.StreamCount, got.StreamAvgTTFB)
	}

	// 验证累加的 token 数和费用
	if got.PromptTokensTotal != 15 || got.CompletionTokensTotal != 30 {
		t.Fatalf("unexpected token totals: prompt=%d completion=%d", got.PromptTokensTotal, got.CompletionTokensTotal)
	}
	if !floatNear(got.TotalCostUSD, 0.15, 1e-9) {
		t.Fatalf("unexpected total_cost_usd: %v (expected 0.15)", got.TotalCostUSD)
	}
}
