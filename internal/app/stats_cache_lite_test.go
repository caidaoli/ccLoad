package app

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestStatsCache_GetStatsLite_CachesResult(t *testing.T) {
	tmpDB := t.TempDir() + "/stats_cache_lite_test.db"
	store, err := storage.CreateSQLiteStore(tmpDB)
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 渠道存在性：GetStatsLite 内部会过滤 channel_id > 0，但不会填充 channel 名称。
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name:         "ch",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	sc := NewStatsCache(store)
	defer sc.Close()

	now := time.Now()
	startTime := now.Add(-time.Minute)
	endTime := now

	// 第一次写入：一条成功日志
	if err := store.AddLog(context.Background(), &model.LogEntry{
		Time:       model.JSONTime{Time: now.Add(-time.Second)},
		Model:      "m1",
		ChannelID:  cfg.ID,
		StatusCode: 200,
		Message:    "ok",
		Duration:   0.1,
	}); err != nil {
		t.Fatalf("AddLog #1 failed: %v", err)
	}

	got1, err := sc.GetStatsLite(context.Background(), startTime, endTime, nil)
	if err != nil {
		t.Fatalf("GetStatsLite #1 failed: %v", err)
	}
	if len(got1) != 1 {
		t.Fatalf("len(stats)=%d, want 1", len(got1))
	}
	if got1[0].ChannelID == nil || int64(*got1[0].ChannelID) != cfg.ID {
		t.Fatalf("channel_id=%v, want %d", got1[0].ChannelID, cfg.ID)
	}
	if got1[0].Model != "m1" || got1[0].Total != 1 || got1[0].Success != 1 || got1[0].Error != 0 {
		t.Fatalf("unexpected stats #1: %+v", got1[0])
	}

	// 第二次写入：范围内再写一条失败日志，但第二次 GetStatsLite 应该命中缓存（TTL>0），结果不变。
	if err := store.AddLog(context.Background(), &model.LogEntry{
		Time:       model.JSONTime{Time: now.Add(-500 * time.Millisecond)},
		Model:      "m1",
		ChannelID:  cfg.ID,
		StatusCode: 500,
		Message:    "err",
		Duration:   0.2,
	}); err != nil {
		t.Fatalf("AddLog #2 failed: %v", err)
	}

	got2, err := sc.GetStatsLite(context.Background(), startTime, endTime, nil)
	if err != nil {
		t.Fatalf("GetStatsLite #2 failed: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("len(stats)=%d, want 1", len(got2))
	}
	if got2[0].Total != 1 || got2[0].Success != 1 || got2[0].Error != 0 {
		t.Fatalf("expected cached stats unchanged, got: %+v", got2[0])
	}
}
