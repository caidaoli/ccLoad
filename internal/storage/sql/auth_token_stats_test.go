package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestAuthTokenStatsInRange_AndRPM(t *testing.T) {
	store := newTestStore(t, "auth_token_stats.db")
	ctx := context.Background()

	now := time.Now()
	start := now.Add(-2 * time.Minute)
	end := now.Add(2 * time.Minute)

	// token 1: 1 success(stream) + 1 failure(non-stream) + 1 cancelled(499, should be excluded)
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
			Time:          model.JSONTime{Time: now},
			ChannelID:     1,
			Model:         "m1",
			StatusCode:    499,
			IsStreaming:   true,
			FirstByteTime: 0.1, // 让 AVG 不受 499 干扰（当前实现未排除 499 的 AVG）
			Duration:      0.3,
			AuthTokenID:   1,
			InputTokens:   0,
			OutputTokens:  0,
			Cost:          0,
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
	if len(stats) != 2 {
		t.Fatalf("stats len=%d, want 2", len(stats))
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
	if s1.PromptTokens != 11 || s1.CompletionTokens != 22 {
		t.Fatalf("token1 tokens=(%d,%d), want (11,22)", s1.PromptTokens, s1.CompletionTokens)
	}

	// 计算 RPM（覆盖 peak/avg/recent 逻辑）
	if err := store.FillAuthTokenRPMStats(ctx, stats, start, end, true); err != nil {
		t.Fatalf("FillAuthTokenRPMStats failed: %v", err)
	}
	if stats[1].AvgRPM <= 0 || stats[1].PeakRPM <= 0 {
		t.Fatalf("token1 rpm invalid: %+v", stats[1])
	}
	// recent RPM 只在 isToday=true 时计算；这里日志就在近2分钟，应该 >=1（排除499）
	if stats[1].RecentRPM <= 0 {
		t.Fatalf("token1 recent rpm=%v, want >0", stats[1].RecentRPM)
	}
}
