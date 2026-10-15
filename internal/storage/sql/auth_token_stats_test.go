package sql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestAuthTokenStatsInRange_AndRPM(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := time.Now()
	start := now.Add(-2 * time.Minute)
	end := now.Add(2 * time.Minute)

	// token 1: success(stream) + failure(non-stream) + cancelled(stream/non-stream)
	//          取消不计次数与耗时，但保留已计费用量。
	//          + 1 count_tokens（辅助请求，不计入令牌统计）
	// token 2: 1 success
	logs := []*model.LogEntry{
		{
			Time:          model.JSONTime{Time: now},
			ChannelID:     1,
			Model:         "m1",
			StatusCode:    200,
			IsStreaming:   true,
			FirstByteTime: 0.1,
			Duration:      0.2,
			AuthTokenID:   1,
			InputTokens:   10,
			OutputTokens:  20,
			Cost:          0.01,
		},
		{
			Time:         model.JSONTime{Time: now},
			ChannelID:    1,
			Model:        "m1",
			StatusCode:   500,
			IsStreaming:  false,
			Duration:     1.2,
			AuthTokenID:  1,
			InputTokens:  1,
			OutputTokens: 2,
			Cost:         0.02,
		},
		{
			Time:                     model.JSONTime{Time: now},
			ChannelID:                1,
			Model:                    "m1",
			StatusCode:               499,
			IsStreaming:              true,
			FirstByteTime:            99,
			Duration:                 0.3,
			AuthTokenID:              1,
			InputTokens:              5,
			OutputTokens:             7,
			CacheReadInputTokens:     3,
			CacheCreationInputTokens: 4,
			Cost:                     0.125,
			CostMultiplier:           2,
		},
		{
			Time: model.JSONTime{Time: now}, ChannelID: 1, Model: "m1", StatusCode: 499,
			AuthTokenID: 1, Duration: 100, InputTokens: 8, OutputTokens: 9,
			CacheReadInputTokens: 5, CacheCreationInputTokens: 6, Cost: 0.25, CostMultiplier: 2,
		},
		{
			Time: model.JSONTime{Time: now}, ChannelID: 1, Model: "m1", StatusCode: 499,
			AuthTokenID: 3, IsStreaming: true, FirstByteTime: 99, Duration: 100,
			InputTokens: 5, Cost: 0.125, CostMultiplier: 2,
		},
		{
			Time:        model.JSONTime{Time: now},
			LogSource:   model.LogSourceCountTokens,
			ChannelID:   1,
			Model:       "m1",
			StatusCode:  200,
			Duration:    0.1,
			AuthTokenID: 1,
			InputTokens: 100,
		},
		{
			Time:         model.JSONTime{Time: now},
			ChannelID:    1,
			Model:        "m1",
			StatusCode:   200,
			IsStreaming:  false,
			Duration:     0.4,
			AuthTokenID:  2,
			InputTokens:  3,
			OutputTokens: 4,
			Cost:         0.03,
		},
	}
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	stats, err := store.GetAuthTokenStatsInRange(ctx, start, end)
	if err != nil {
		t.Fatalf("GetAuthTokenStatsInRange failed: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("stats len=%d, want 3", len(stats))
	}

	s1 := stats[1]
	if s1 == nil {
		t.Fatalf("missing token 1 stats")
	}
	if s1.SuccessCount != 1 || s1.FailureCount != 1 {
		t.Fatalf("token1 counts=(%d,%d), want (1,1)", s1.SuccessCount, s1.FailureCount)
	}
	if s1.StreamCount != 1 || s1.NonStreamCount != 1 {
		t.Fatalf("token1 stream/nonstream=(%d,%d), want (1,1)", s1.StreamCount, s1.NonStreamCount)
	}
	if s1.PromptTokens != 24 || s1.CompletionTokens != 38 || s1.CacheReadTokens != 8 || s1.CacheCreationTokens != 10 {
		t.Fatalf("token1 billable tokens lost: %+v", s1)
	}
	if math.Abs(s1.TotalCost-0.405) > 1e-9 || math.Abs(s1.EffectiveCost-0.75) > 1e-9 {
		t.Fatalf("token1 billable costs lost: %+v", s1)
	}
	if s1.StreamAvgTTFB != 0.1 || s1.NonStreamAvgRT != 1.2 {
		t.Fatalf("cancelled latency affected token1 averages: %+v", s1)
	}
	s3 := stats[3]
	if s3 == nil || s3.SuccessCount != 0 || s3.FailureCount != 0 || s3.StreamCount != 0 || s3.NonStreamCount != 0 || s3.StreamAvgTTFB != 0 || s3.NonStreamAvgRT != 0 {
		t.Fatalf("cancelled-only token counts/latency=%+v, want zero", s3)
	}
	if s3.PromptTokens != 5 || s3.TotalCost != 0.125 || s3.EffectiveCost != 0.25 {
		t.Fatalf("cancelled-only token usage lost: %+v", s3)
	}

	// 计算 RPM（覆盖 peak/avg/recent 逻辑）
	if err := store.FillAuthTokenRPMStats(ctx, stats, start, end, true); err != nil {
		t.Fatalf("FillAuthTokenRPMStats failed: %v", err)
	}
	if stats[1].AvgRPM <= 0 || stats[1].PeakRPM != 2 {
		t.Fatalf("token1 rpm invalid: %+v", stats[1])
	}
	// recent RPM 只在 isToday=true 时计算；这里日志就在近2分钟，应该 >=1（排除499）
	if stats[1].RecentRPM <= 0 {
		t.Fatalf("token1 recent rpm=%v, want >0", stats[1].RecentRPM)
	}
}
