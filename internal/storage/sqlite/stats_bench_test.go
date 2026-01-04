package sqlite_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func seedLogs(start time.Time, count int, channels int, models []string) []*model.LogEntry {
	logs := make([]*model.LogEntry, 0, count)
	for i := 0; i < count; i++ {
		ts := start.Add(time.Duration(i%86_400) * time.Second)
		channelID := int64((i % channels) + 1)
		status := 200
		if i%17 == 0 {
			status = 500
		} else if i%23 == 0 {
			status = 429
		} else if i%29 == 0 {
			status = 499
		}
		logs = append(logs, &model.LogEntry{
			Time:       model.JSONTime{Time: ts},
			Model:      models[i%len(models)],
			ChannelID:  channelID,
			StatusCode: status,
		})
	}
	return logs
}

func BenchmarkGetStats_Range24h(b *testing.B) {
	store, cleanup := setupSQLiteTestStore(b, "bench-stats-24h.db")
	defer cleanup()

	ctx := context.Background()
	start := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	end := start.Add(24 * time.Hour)

	logs := seedLogs(start, 50_000, 64, []string{"gpt-4o", "claude-3-5-sonnet", "gemini-1.5-pro"})
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		b.Fatalf("seed logs: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.GetStats(ctx, start, end, nil, false); err != nil {
			b.Fatalf("GetStats: %v", err)
		}
	}
}

func BenchmarkGetStats_Today(b *testing.B) {
	store, cleanup := setupSQLiteTestStore(b, "bench-stats-today.db")
	defer cleanup()

	ctx := context.Background()
	start := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	end := time.Now().Truncate(time.Second)

	logs := seedLogs(start, 30_000, 64, []string{"gpt-4o", "claude-3-5-sonnet", "gemini-1.5-pro"})
	if err := store.BatchAddLogs(ctx, logs); err != nil {
		b.Fatalf("seed logs: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.GetStats(ctx, start, end, nil, true); err != nil {
			b.Fatalf("GetStats: %v", err)
		}
	}
}
