package sql_test

import (
	"context"
	"database/sql"
	"encoding/json"
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
		Name:           "openai-main",
		URL:            "https://example.com",
		Priority:       10,
		Enabled:        true,
		ChannelType:    "openai",
		CostMultiplier: 0.85,
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
		{Time: model.JSONTime{Time: now}, ChannelID: openaiCfg.ID, Model: "gpt-4o", StatusCode: 200, Duration: 0.1, IsStreaming: true, FirstByteTime: 0.01, InputTokens: 10, OutputTokens: 20, Cost: 0.01, LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: now}, ChannelID: openaiCfg.ID, Model: "gpt-4o", StatusCode: 500, Duration: 0.2, IsStreaming: false, InputTokens: 1, OutputTokens: 2, Cost: 0.02, LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: now}, ChannelID: openaiCfg.ID, Model: "gpt-4o", StatusCode: 499, Duration: 0.3, IsStreaming: true, FirstByteTime: 0.02, InputTokens: 999, OutputTokens: 999, Cost: 9.99, LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: now}, ChannelID: anthCfg.ID, Model: "claude-3-5-sonnet-latest", StatusCode: 200, Duration: 0.4, IsStreaming: false, InputTokens: 3, OutputTokens: 4, Cost: 0.03, LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: now}, ChannelID: openaiCfg.ID, Model: "gpt-4o", StatusCode: 200, Duration: 0.05, IsStreaming: false, InputTokens: 100, OutputTokens: 200, Cost: 1.23, LogSource: model.LogSourceManualTest},
	}); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	// GetDistinctModels：无过滤 + 按渠道类型过滤（覆盖 fetchChannelIDsByType）
	modelsAll, err := store.GetDistinctModels(ctx, start, end, "", nil)
	if err != nil {
		t.Fatalf("GetDistinctModels(all) failed: %v", err)
	}
	if len(modelsAll) < 2 {
		t.Fatalf("GetDistinctModels(all) got %v, want >=2", modelsAll)
	}
	modelsOpenAI, err := store.GetDistinctModels(ctx, start, end, "openai", nil)
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
			if e.CostMultiplier == nil || *e.CostMultiplier != 0.85 {
				t.Fatalf("expected cost_multiplier=0.85 in stats entry, got %+v", e)
			}

			encoded, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("marshal stats entry failed: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(encoded, &payload); err != nil {
				t.Fatalf("unmarshal stats entry failed: %v", err)
			}
			if got, ok := payload["channel_type"].(string); !ok || got != "openai" {
				t.Fatalf("expected channel_type=openai in stats payload, got %+v", payload)
			}
			if got, ok := payload["cost_multiplier"].(float64); !ok || got != 0.85 {
				t.Fatalf("expected cost_multiplier=0.85 in stats payload, got %+v", payload)
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

	manualStats, err := store.GetStats(ctx, start, end, &model.LogFilter{LogSource: model.LogSourceManualTest}, false)
	if err != nil {
		t.Fatalf("GetStats(manual_test) failed: %v", err)
	}
	if len(manualStats) != 1 || manualStats[0].Total != 1 || manualStats[0].Success != 1 {
		t.Fatalf("manual test stats=%+v, want one success record", manualStats)
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
	hlRows, err := ss.GetHealthTimeline(ctx, model.HealthTimelineParams{
		SinceMs: 0, UntilMs: time.Now().UnixMilli(), BucketMs: 60000,
	})
	if err != nil {
		t.Fatalf("GetHealthTimeline failed: %v", err)
	}
	_ = hlRows

	// CleanupLogsBefore：删除所有日志
	if err := store.CleanupLogsBefore(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CleanupLogsBefore failed: %v", err)
	}
}

func TestMetrics_LastSuccessAndLastFailedRequest(t *testing.T) {
	store := newTestStore(t, "metrics_last_success.db")
	ctx := context.Background()

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:        "last-success-channel",
		URL:         "https://example.com",
		Priority:    10,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	successAt := now.Add(-30 * time.Second)
	failedAt := now.Add(-10 * time.Second)
	cancelledAt := now.Add(-5 * time.Second)

	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{Time: model.JSONTime{Time: successAt}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 200, Message: "ok", LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: failedAt}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 429, Message: "rate limit exceeded", LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: failedAt}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 500, Message: "upstream failed later", LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: cancelledAt}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 499, Message: "client cancelled", LogSource: model.LogSourceProxy},
	}); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	stats, err := store.GetStats(ctx, now.Add(-time.Minute), now.Add(time.Minute), nil, false)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}

	for _, e := range stats {
		if e.ChannelID == nil || int64(*e.ChannelID) != cfg.ID || e.Model != "gpt-4o" {
			continue
		}
		if e.LastSuccessAt == nil || *e.LastSuccessAt != successAt.UnixMilli() {
			t.Fatalf("LastSuccessAt=%v, want %d", e.LastSuccessAt, successAt.UnixMilli())
		}
		if e.LastSuccessID == nil || *e.LastSuccessID <= 0 {
			t.Fatalf("LastSuccessID=%v, want positive id", e.LastSuccessID)
		}
		wantLastRequestAt := failedAt.UnixMilli()
		if e.LastRequestAt == nil || *e.LastRequestAt != wantLastRequestAt {
			t.Fatalf("LastRequestAt=%v, want %d", e.LastRequestAt, wantLastRequestAt)
		}
		if e.LastRequestID == nil || *e.LastRequestID <= 0 {
			t.Fatalf("LastRequestID=%v, want positive id", e.LastRequestID)
		}
		if e.LastRequestStatus == nil || *e.LastRequestStatus != 500 {
			t.Fatalf("LastRequestStatus=%v, want 500", e.LastRequestStatus)
		}
		if e.LastRequestMessage != "upstream failed later" {
			t.Fatalf("LastRequestMessage=%q, want upstream failed later", e.LastRequestMessage)
		}
		return
	}

	t.Fatalf("stats missing channel %d model gpt-4o: %+v", cfg.ID, stats)
}

func TestMetrics_LastRequestIDsExposeTieBreakForFrontEndAggregation(t *testing.T) {
	store := newTestStore(t, "metrics_last_request_ids.db")
	ctx := context.Background()

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:        "multi-model-channel",
		URL:         "https://example.com",
		Priority:    10,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
			{Model: "gpt-4.1"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{Time: model.JSONTime{Time: now}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 200, Message: "ok-a", LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: now}, ChannelID: cfg.ID, Model: "gpt-4.1", StatusCode: 500, Message: "fail-b", LogSource: model.LogSourceProxy},
	}); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	stats, err := store.GetStats(ctx, now.Add(-time.Minute), now.Add(time.Minute), nil, false)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}

	idsByModel := make(map[string]int64, 2)
	for _, e := range stats {
		if e.ChannelID == nil || int64(*e.ChannelID) != cfg.ID {
			continue
		}
		if e.LastRequestID == nil || *e.LastRequestID <= 0 {
			t.Fatalf("model %s LastRequestID=%v, want positive id", e.Model, e.LastRequestID)
		}
		idsByModel[e.Model] = *e.LastRequestID
	}

	if len(idsByModel) != 2 {
		t.Fatalf("idsByModel=%v, want 2 models", idsByModel)
	}
	if idsByModel["gpt-4.1"] <= idsByModel["gpt-4o"] {
		t.Fatalf("want gpt-4.1 id > gpt-4o id for same-millisecond tie-break, got %+v", idsByModel)
	}
}

func TestMetrics_LastSuccessAtIgnoresCurrentRange(t *testing.T) {
	store := newTestStore(t, "metrics_last_success_all_time.db")
	ctx := context.Background()

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:        "history-success-channel",
		URL:         "https://example.com",
		Priority:    10,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	oldSuccessAt := now.Add(-48 * time.Hour)
	recentFailedAt := now.Add(-10 * time.Second)

	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{Time: model.JSONTime{Time: oldSuccessAt}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 200, Message: "historic ok", LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: recentFailedAt}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 500, Message: "recent failed", LogSource: model.LogSourceProxy},
	}); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	stats, err := store.GetStats(ctx, now.Add(-time.Minute), now.Add(time.Minute), nil, false)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}

	for _, e := range stats {
		if e.ChannelID == nil || int64(*e.ChannelID) != cfg.ID || e.Model != "gpt-4o" {
			continue
		}
		if e.LastSuccessAt == nil || *e.LastSuccessAt != oldSuccessAt.UnixMilli() {
			t.Fatalf("LastSuccessAt=%v, want %d", e.LastSuccessAt, oldSuccessAt.UnixMilli())
		}
		if e.LastRequestAt == nil || *e.LastRequestAt != recentFailedAt.UnixMilli() {
			t.Fatalf("LastRequestAt=%v, want %d", e.LastRequestAt, recentFailedAt.UnixMilli())
		}
		return
	}

	t.Fatalf("stats missing channel %d model gpt-4o: %+v", cfg.ID, stats)
}

func TestMetrics_LastRequestAtIgnoresCurrentRange(t *testing.T) {
	store := newTestStore(t, "metrics_last_request_all_time.db")
	ctx := context.Background()

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:        "history-request-channel",
		URL:         "https://example.com",
		Priority:    10,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	inRangeSuccessAt := now.Add(-30 * time.Second)
	latestFailedAt := now.Add(2 * time.Hour)

	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{Time: model.JSONTime{Time: inRangeSuccessAt}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 200, Message: "in-range ok", LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: latestFailedAt}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 502, Message: "latest failed outside range", LogSource: model.LogSourceProxy},
	}); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	stats, err := store.GetStats(ctx, now.Add(-time.Minute), now.Add(time.Minute), nil, false)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}

	for _, e := range stats {
		if e.ChannelID == nil || int64(*e.ChannelID) != cfg.ID || e.Model != "gpt-4o" {
			continue
		}
		if e.LastSuccessAt == nil || *e.LastSuccessAt != inRangeSuccessAt.UnixMilli() {
			t.Fatalf("LastSuccessAt=%v, want %d", e.LastSuccessAt, inRangeSuccessAt.UnixMilli())
		}
		if e.LastRequestAt == nil || *e.LastRequestAt != latestFailedAt.UnixMilli() {
			t.Fatalf("LastRequestAt=%v, want %d", e.LastRequestAt, latestFailedAt.UnixMilli())
		}
		if e.LastRequestStatus == nil || *e.LastRequestStatus != 502 {
			t.Fatalf("LastRequestStatus=%v, want 502", e.LastRequestStatus)
		}
		if e.LastRequestMessage != "latest failed outside range" {
			t.Fatalf("LastRequestMessage=%q, want latest failed outside range", e.LastRequestMessage)
		}
		return
	}

	t.Fatalf("stats missing channel %d model gpt-4o: %+v", cfg.ID, stats)
}

func TestMetrics_LastStateIgnoresStatusCodeFilter(t *testing.T) {
	store := newTestStore(t, "metrics_last_state_status_filter.db")
	ctx := context.Background()

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:        "status-filter-channel",
		URL:         "https://example.com",
		Priority:    10,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	successAt := now.Add(-30 * time.Second)
	failed500At := now.Add(-20 * time.Second)
	failed429At := now.Add(-10 * time.Second)

	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{Time: model.JSONTime{Time: successAt}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 200, Message: "historic ok", LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: failed500At}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 500, Message: "old failed", LogSource: model.LogSourceProxy},
		{Time: model.JSONTime{Time: failed429At}, ChannelID: cfg.ID, Model: "gpt-4o", StatusCode: 429, Message: "latest failed", LogSource: model.LogSourceProxy},
	}); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	statusCode := 500
	stats, err := store.GetStats(ctx, now.Add(-time.Minute), now.Add(time.Minute), &model.LogFilter{
		StatusCode: &statusCode,
		LogSource:  model.LogSourceProxy,
	}, false)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}

	for _, e := range stats {
		if e.ChannelID == nil || int64(*e.ChannelID) != cfg.ID || e.Model != "gpt-4o" {
			continue
		}
		if e.Success != 0 || e.Error != 1 || e.Total != 1 {
			t.Fatalf("filtered stats mismatch: success=%d error=%d total=%d", e.Success, e.Error, e.Total)
		}
		if e.LastSuccessAt == nil || *e.LastSuccessAt != successAt.UnixMilli() {
			t.Fatalf("LastSuccessAt=%v, want %d", e.LastSuccessAt, successAt.UnixMilli())
		}
		if e.LastRequestAt == nil || *e.LastRequestAt != failed429At.UnixMilli() {
			t.Fatalf("LastRequestAt=%v, want %d", e.LastRequestAt, failed429At.UnixMilli())
		}
		if e.LastRequestStatus == nil || *e.LastRequestStatus != 429 {
			t.Fatalf("LastRequestStatus=%v, want 429", e.LastRequestStatus)
		}
		if e.LastRequestMessage != "latest failed" {
			t.Fatalf("LastRequestMessage=%q, want latest failed", e.LastRequestMessage)
		}
		return
	}

	t.Fatalf("stats missing channel %d model gpt-4o: %+v", cfg.ID, stats)
}

func TestGetStats_PreservesZeroCostMultiplierForFreeChannels(t *testing.T) {
	store := newTestStore(t, "metrics_zero_multiplier.db")
	ctx := context.Background()

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:           "free-channel",
		URL:            "https://example.com",
		Priority:       1,
		Enabled:        true,
		ChannelType:    "openai",
		CostMultiplier: 0,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-5.4"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now()
	if err := store.BatchAddLogs(ctx, []*model.LogEntry{
		{
			Time:       model.JSONTime{Time: now},
			ChannelID:  cfg.ID,
			Model:      "gpt-5.4",
			StatusCode: 200,
			Duration:   0.1,
			Cost:       0.02,
			LogSource:  model.LogSourceProxy,
		},
	}); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	stats, err := store.GetStats(ctx, now.Add(-time.Minute), now.Add(time.Minute), nil, false)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("GetStats len=%d, want 1", len(stats))
	}
	if stats[0].CostMultiplier == nil {
		t.Fatalf("expected cost_multiplier=0, got nil")
	}
	if *stats[0].CostMultiplier != 0 {
		t.Fatalf("expected cost_multiplier=0, got %v", *stats[0].CostMultiplier)
	}
	if stats[0].EffectiveCost == nil {
		t.Fatalf("expected effective_cost=0, got nil")
	}
	if *stats[0].EffectiveCost != 0 {
		t.Fatalf("expected effective_cost=0, got %v", *stats[0].EffectiveCost)
	}
}
