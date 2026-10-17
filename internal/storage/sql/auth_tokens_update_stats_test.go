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

	// 失败请求：累加失败次数，不计耗时均值；token与费用按调用方传入值累加（已提交流的部分用量）。
	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsFailure, 2.0, false, 0, 10, 20, 3, 4, 1.0, 0.25, time.Now()); err != nil {
		t.Fatalf("update token stats (failure): %v", err)
	}

	got, err := store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.SuccessCount != 0 || got.FailureCount != 1 {
		t.Fatalf("unexpected counts after failure: success=%d failure=%d", got.SuccessCount, got.FailureCount)
	}
	if got.PromptTokensTotal != 10 || got.CompletionTokensTotal != 20 || got.CacheReadTokensTotal != 3 || got.CacheCreationTokensTotal != 4 {
		t.Fatalf("unexpected token totals after failure: prompt=%d completion=%d cache_read=%d cache_create=%d",
			got.PromptTokensTotal, got.CompletionTokensTotal, got.CacheReadTokensTotal, got.CacheCreationTokensTotal)
	}
	if got.TotalCostUSD != 1.0 || got.EffectiveCostUSD != 0.25 || got.CostUsedMicroUSD != util.USDToMicroUSD(0.25) {
		t.Fatalf("unexpected cost after failure: total_cost_usd=%v effective_cost_usd=%v cost_used_microusd=%d", got.TotalCostUSD, got.EffectiveCostUSD, got.CostUsedMicroUSD)
	}
	if got.DurationCount != 0 || got.AvgDuration != 0 || got.SpeedOutputTokens != 0 || got.SpeedDurationSeconds != 0 {
		t.Fatalf("failure should not affect duration/speed stats: duration=(%d,%v) speed=(%d,%v)",
			got.DurationCount, got.AvgDuration, got.SpeedOutputTokens, got.SpeedDurationSeconds)
	}

	// 客户端取消：只累加用量与费用，不计成功/失败次数，也不影响耗时统计。
	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsCanceled, 9.0, false, 0, 10, 20, 3, 4, 0.5, 0.25, time.Now()); err != nil {
		t.Fatalf("update token stats (canceled): %v", err)
	}

	got, err = store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.SuccessCount != 0 || got.FailureCount != 1 {
		t.Fatalf("unexpected counts after cancel: success=%d failure=%d", got.SuccessCount, got.FailureCount)
	}
	if got.PromptTokensTotal != 20 || got.EffectiveCostUSD != 0.5 || got.CostUsedMicroUSD != 2*util.USDToMicroUSD(0.25) {
		t.Fatalf("unexpected usage after cancel: prompt=%d effective_cost_usd=%v cost_used_microusd=%d", got.PromptTokensTotal, got.EffectiveCostUSD, got.CostUsedMicroUSD)
	}
	if got.DurationCount != 0 || got.AvgDuration != 0 || got.SpeedOutputTokens != 0 || got.SpeedDurationSeconds != 0 {
		t.Fatalf("cancel should not affect duration/speed stats: duration=(%d,%v) speed=(%d,%v)",
			got.DurationCount, got.AvgDuration, got.SpeedOutputTokens, got.SpeedDurationSeconds)
	}

	// 成功请求：累加成功次数、token与费用，并计入耗时均值。
	completedAt := time.Now()
	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsSuccess, 4.0, false, 0, 10, 20, 3, 4, 0.5, 0.25, completedAt); err != nil {
		t.Fatalf("update token stats (success): %v", err)
	}

	got, err = store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.SuccessCount != 1 || got.FailureCount != 1 {
		t.Fatalf("unexpected counts after success: success=%d failure=%d", got.SuccessCount, got.FailureCount)
	}
	if got.PromptTokensTotal != 30 || got.CompletionTokensTotal != 60 || got.CacheReadTokensTotal != 9 || got.CacheCreationTokensTotal != 12 {
		t.Fatalf("unexpected token totals after success: prompt=%d completion=%d cache_read=%d cache_create=%d",
			got.PromptTokensTotal, got.CompletionTokensTotal, got.CacheReadTokensTotal, got.CacheCreationTokensTotal)
	}
	if got.TotalCostUSD != 2.0 {
		t.Fatalf("unexpected total_cost_usd after success: %v", got.TotalCostUSD)
	}
	if got.EffectiveCostUSD != 0.75 {
		t.Fatalf("unexpected effective_cost_usd after success: %v", got.EffectiveCostUSD)
	}
	wantUsed := 3 * util.USDToMicroUSD(0.25)
	if got.CostUsedMicroUSD != wantUsed {
		t.Fatalf("unexpected cost_used_microusd after success: %d", got.CostUsedMicroUSD)
	}
	if got.DurationCount != 1 || got.AvgDuration != 4.0 {
		t.Fatalf("unexpected duration stats after success: count=%d avg=%v", got.DurationCount, got.AvgDuration)
	}
	// 非流式请求的生成耗时即总耗时
	if got.SpeedOutputTokens != 20 || got.SpeedDurationSeconds != 4.0 {
		t.Fatalf("unexpected speed stats after success: tokens=%d seconds=%v", got.SpeedOutputTokens, got.SpeedDurationSeconds)
	}
	if got.LastUsedAt == nil || *got.LastUsedAt <= 0 {
		t.Fatalf("expected last_used_at to be set, got=%v", got.LastUsedAt)
	}
	if got.CostDailyUsedMicroUSD != wantUsed {
		t.Fatalf("unexpected cost_daily_used_microusd after success: %d", got.CostDailyUsedMicroUSD)
	}
	if got.CostMonthlyUsedMicroUSD != wantUsed {
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

	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsSuccess, 1.0, false, 0, 1, 1, 0, 0, 0.1, 0.1, oldCompletedAt); err != nil {
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

	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsSuccess, 1.0, false, 0, 1, 1, 0, 0, 0.2, 0.2, now); err != nil {
		t.Fatalf("update current-period token stats: %v", err)
	}
	// 再迟到一个旧周期事件：总额仍应记账，但当前日/月窗口和 last_used_at 不得倒退。
	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsSuccess, 1.0, false, 0, 1, 1, 0, 0, 0.1, 0.1, oldCompletedAt); err != nil {
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

	// 第一次流式请求：首字 1s，耗时 3s
	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsSuccess, 3.0, true, 1.0, 10, 20, 0, 0, 0.1, 0.1, time.Now()); err != nil {
		t.Fatalf("update token stats (streaming 1): %v", err)
	}

	got, err := store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.StreamCount != 1 || got.StreamAvgTTFB != 1.0 {
		t.Fatalf("unexpected stream stats after first request: count=%d avg=%v", got.StreamCount, got.StreamAvgTTFB)
	}
	// 耗时均值覆盖流式请求
	if got.DurationCount != 1 || got.AvgDuration != 3.0 {
		t.Fatalf("unexpected duration stats after streaming request: count=%d avg=%v", got.DurationCount, got.AvgDuration)
	}

	// 第二次流式请求：首字 2s、耗时 5s，期望均值 1.5 / 4
	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsSuccess, 5.0, true, 2.0, 5, 10, 0, 0, 0.05, 0.05, time.Now()); err != nil {
		t.Fatalf("update token stats (streaming 2): %v", err)
	}

	got, err = store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.StreamCount != 2 || got.StreamAvgTTFB != 1.5 || got.DurationCount != 2 || got.AvgDuration != 4.0 {
		t.Fatalf("unexpected timing after second request: ttfb=(%d,%v) duration=(%d,%v), want (2,1.5) (2,4)",
			got.StreamCount, got.StreamAvgTTFB, got.DurationCount, got.AvgDuration)
	}
	// 流式生成耗时扣除首字：(3-1) + (5-2) = 5s，输出 20 + 10
	if got.SpeedOutputTokens != 30 || got.SpeedDurationSeconds != 5.0 {
		t.Fatalf("unexpected speed stats after streaming: tokens=%d seconds=%v, want 30/5", got.SpeedOutputTokens, got.SpeedDurationSeconds)
	}

	// 已出首字的失败流不计入首字与耗时均值
	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsFailure, 60.0, true, 30.0, 0, 0, 0, 0, 0, 0, time.Now()); err != nil {
		t.Fatalf("update token stats (failed stream): %v", err)
	}
	got, err = store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.StreamCount != 2 || got.StreamAvgTTFB != 1.5 || got.DurationCount != 2 || got.AvgDuration != 4.0 {
		t.Fatalf("failed stream affected timing: ttfb=(%d,%v) duration=(%d,%v)",
			got.StreamCount, got.StreamAvgTTFB, got.DurationCount, got.AvgDuration)
	}

	// 验证累加的 token 数和费用
	if got.PromptTokensTotal != 15 || got.CompletionTokensTotal != 30 {
		t.Fatalf("unexpected token totals: prompt=%d completion=%d", got.PromptTokensTotal, got.CompletionTokensTotal)
	}
	if !floatNear(got.TotalCostUSD, 0.15, 1e-9) {
		t.Fatalf("unexpected total_cost_usd: %v (expected 0.15)", got.TotalCostUSD)
	}

	// 扣除首字后不足 1 秒时按总耗时计：+1.5s，输出 +6
	if err := store.UpdateTokenStats(ctx, tokenHash, model.TokenStatsSuccess, 1.5, true, 1.0, 0, 6, 0, 0, 0, 0, time.Now()); err != nil {
		t.Fatalf("update token stats (short generation): %v", err)
	}
	got, err = store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.SpeedOutputTokens != 36 || got.SpeedDurationSeconds != 6.5 {
		t.Fatalf("unexpected speed stats after short generation: tokens=%d seconds=%v, want 36/6.5", got.SpeedOutputTokens, got.SpeedDurationSeconds)
	}
}
