package sql_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

func TestMetrics_BasicQueriesAndFilters(t *testing.T) {
	store := newTestStore(t, "metrics_basic.db")
	ctx := context.Background()

	// 两个渠道：用于覆盖 type/name 过滤与交集逻辑
	openaiCfg, err := store.CreateConfig(ctx, &model.Config{
		Name:        "openai-main",
		URL:         "https://example.com",
		Priority:    10,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig openai failed: %v", err)
	}
	anthCfg, err := store.CreateConfig(ctx, &model.Config{
		Name:        "anthropic-1",
		URL:         "https://example.com",
		Priority:    20,
		Enabled:     true,
		ChannelType: "anthropic",
		ModelEntries: []model.ModelEntry{
			{Model: "claude-3-5-sonnet-latest"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig anthropic failed: %v", err)
	}

	now := time.Now()
	start := now.Add(-2 * time.Minute)
	end := now.Add(1 * time.Minute)

	// openai: success + error + cancelled(499)
	// anthropic: success
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{Time: model.JSONTime{Time: now}, ChannelID: openaiCfg.ID, Model: "gpt-4o", StatusCode: 200, Duration: 0.1, IsStreaming: true, FirstByteTime: 0.01, InputTokens: 10, OutputTokens: 20, Cost: 0.01},
		{Time: model.JSONTime{Time: now}, ChannelID: openaiCfg.ID, Model: "gpt-4o", StatusCode: 500, Duration: 0.2, IsStreaming: false, InputTokens: 1, OutputTokens: 2, Cost: 0.02},
		{Time: model.JSONTime{Time: now}, ChannelID: openaiCfg.ID, Model: "gpt-4o", StatusCode: 499, Duration: 0.3, IsStreaming: true, FirstByteTime: 0.02, InputTokens: 999, OutputTokens: 999, Cost: 9.99},
		{Time: model.JSONTime{Time: now}, ChannelID: anthCfg.ID, Model: "claude-3-5-sonnet-latest", StatusCode: 200, Duration: 0.4, IsStreaming: false, InputTokens: 3, OutputTokens: 4, Cost: 0.03},
	}); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	// GetDistinctModels：无过滤 + 按渠道类型过滤（覆盖 fetchChannelIDsByType）
	modelsAll, err := store.GetDistinctModels(ctx, start, end, "")
	if err != nil {
		t.Fatalf("GetDistinctModels(all) failed: %v", err)
	}
	if len(modelsAll) < 2 {
		t.Fatalf("GetDistinctModels(all) got %v, want >=2", modelsAll)
	}
	modelsOpenAI, err := store.GetDistinctModels(ctx, start, end, "openai")
	if err != nil {
		t.Fatalf("GetDistinctModels(openai) failed: %v", err)
	}
	if len(modelsOpenAI) != 1 || modelsOpenAI[0] != "gpt-4o" {
		t.Fatalf("GetDistinctModels(openai) got %v, want [gpt-4o]", modelsOpenAI)
	}

	// GetChannelSuccessRates：openai 成功率 1/2（499 不纳入口径）
	rates, err := store.GetChannelSuccessRates(ctx, start)
	if err != nil {
		t.Fatalf("GetChannelSuccessRates failed: %v", err)
	}
	r := rates[openaiCfg.ID]
	if r.SampleCount != 2 || r.SuccessRate < 0.49 || r.SuccessRate > 0.51 {
		t.Fatalf("openai success rate=%v sample=%d, want ~0.5 and 2", r.SuccessRate, r.SampleCount)
	}

	// GetStats：覆盖 applyChannelFilter(nil) + 渠道信息批量填充 + RPM 降级路径
	stats, err := store.GetStats(ctx, start, end, nil, false)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}
	if len(stats) < 2 {
		t.Fatalf("GetStats len=%d, want >=2", len(stats))
	}
	foundOpenAI := false
	for _, e := range stats {
		if e.ChannelID != nil && int64(*e.ChannelID) == openaiCfg.ID && e.Model == "gpt-4o" {
			foundOpenAI = true
			if e.Total != 2 || e.Success != 1 || e.Error != 1 {
				t.Fatalf("openai stats=%+v, want total=2 success=1 error=1 (exclude 499)", e)
			}
			if e.ChannelName == "" || e.ChannelPriority == nil {
				t.Fatalf("expected channel info filled, got %+v", e)
			}
		}
	}
	if !foundOpenAI {
		t.Fatalf("missing openai stats entry")
	}

	// GetStatsLite：轻量版也应可用
	if _, err := store.GetStatsLite(ctx, start, end, nil); err != nil {
		t.Fatalf("GetStatsLite failed: %v", err)
	}

	// GetRPMStats：全局峰值/平均统计
	rpm, err := store.GetRPMStats(ctx, start, end, nil, false)
	if err != nil {
		t.Fatalf("GetRPMStats failed: %v", err)
	}
	if rpm.PeakRPM <= 0 || rpm.AvgRPM <= 0 {
		t.Fatalf("unexpected rpm stats: %+v", rpm)
	}

	// AggregateRangeWithFilter：覆盖 resolveChannelFilter(type+nameLike 交集)
	pts, err := store.AggregateRangeWithFilter(ctx, start, end, time.Minute, &model.LogFilter{
		ChannelType:     "openai",
		ChannelNameLike: "openai",
	})
	if err != nil {
		t.Fatalf("AggregateRangeWithFilter failed: %v", err)
	}
	if len(pts) == 0 {
		t.Fatalf("AggregateRangeWithFilter returned empty points")
	}
	nonEmpty := false
	for _, p := range pts {
		if p.Success > 0 || p.Error > 0 {
			nonEmpty = true
			break
		}
	}
	if !nonEmpty {
		t.Fatalf("expected at least one non-empty metric point")
	}

	// 空结果：触发 buildEmptyMetricPoints 路径
	emptyPts, err := store.AggregateRangeWithFilter(ctx, start, end, time.Minute, &model.LogFilter{
		ChannelType: "does-not-exist",
	})
	if err != nil {
		t.Fatalf("AggregateRangeWithFilter(empty) failed: %v", err)
	}
	if len(emptyPts) == 0 {
		t.Fatalf("expected empty metric points series, got len=0")
	}

	// 触发 QueryBuilder.WhereIn：GetStats 带 type+name 过滤走 applyChannelFilter
	filteredStats, err := store.GetStats(ctx, start, end, &model.LogFilter{
		ChannelType:     "openai",
		ChannelNameLike: "openai",
	}, false)
	if err != nil {
		t.Fatalf("GetStats(filtered) failed: %v", err)
	}
	if len(filteredStats) != 1 {
		t.Fatalf("GetStats(filtered) len=%d, want 1", len(filteredStats))
	}

	// GetTodayChannelCosts：覆盖今日成本聚合
	costs, err := store.GetTodayChannelCosts(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("GetTodayChannelCosts failed: %v", err)
	}
	if _, ok := costs[openaiCfg.ID]; !ok {
		t.Fatalf("expected openai cost entry in map")
	}

	// 覆盖 SQLStore 的底层 DB wrapper：Ping/Query/Exec/BeginTx/GetHealthTimeline
	ss := store.(*sqlstore.SQLStore)
	if err := ss.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
	row := ss.QueryRowContext(ctx, "SELECT 1")
	var one int
	if err := row.Scan(&one); err != nil || one != 1 {
		t.Fatalf("QueryRowContext got (%d,%v), want (1,nil)", one, err)
	}
	rows, err := ss.QueryContext(ctx, "SELECT 1")
	if err != nil {
		t.Fatalf("QueryContext failed: %v", err)
	}
	_ = rows.Close()
	if _, err := ss.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("ExecContext failed: %v", err)
	}
	tx, err := ss.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx failed: %v", err)
	}
	_ = tx.Rollback()
	hlRows, err := ss.GetHealthTimeline(ctx, "SELECT 1")
	if err != nil {
		t.Fatalf("GetHealthTimeline failed: %v", err)
	}
	_ = hlRows.Close()

	// CleanupLogsBefore：删除所有日志
	if err := store.CleanupLogsBefore(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CleanupLogsBefore failed: %v", err)
	}
}
