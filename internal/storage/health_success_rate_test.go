package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestGetChannelSuccessRates_IgnoresClientNoise(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	dbPath := filepath.Join(tmpDir, "success_rate.db")
	store, err := storage.CreateSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	cfg := &model.Config{
		Name:     "test-channel",
		URL:      "https://example.com",
		Priority: 10,
		ModelEntries: []model.ModelEntry{
			{Model: "model-a", RedirectModel: ""},
		},
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}

	now := time.Now()
	logs := []*model.LogEntry{
		{Time: model.JSONTime{Time: now.Add(-10 * time.Second)}, ChannelID: created.ID, StatusCode: 200, Message: "ok"},
		{Time: model.JSONTime{Time: now.Add(-9 * time.Second)}, ChannelID: created.ID, StatusCode: 204, Message: "ok"},
		{Time: model.JSONTime{Time: now.Add(-8 * time.Second)}, ChannelID: created.ID, StatusCode: 408, Message: "request timeout"},
		{Time: model.JSONTime{Time: now.Add(-8 * time.Second)}, ChannelID: created.ID, StatusCode: 405, Message: "method not allowed"},
		{Time: model.JSONTime{Time: now.Add(-8 * time.Second)}, ChannelID: created.ID, StatusCode: 502, Message: "bad gateway"},
		{Time: model.JSONTime{Time: now.Add(-7 * time.Second)}, ChannelID: created.ID, StatusCode: 597, Message: "sse error"},
		{Time: model.JSONTime{Time: now.Add(-6 * time.Second)}, ChannelID: created.ID, StatusCode: 404, Message: "client not found"}, // 应被忽略
		{Time: model.JSONTime{Time: now.Add(-5 * time.Second)}, ChannelID: created.ID, StatusCode: 499, Message: "client canceled"},  // 应被忽略
	}
	for _, e := range logs {
		if err := store.AddLog(ctx, e); err != nil {
			t.Fatalf("failed to add log: %v", err)
		}
	}

	rates, err := store.GetChannelSuccessRates(ctx, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("GetChannelSuccessRates error: %v", err)
	}

	// eligible: 200/204/405/502/597 -> 2 successes / 5 total = 0.4
	// 注：408已改为客户端错误，不计入健康度统计
	got, ok := rates[created.ID]
	if !ok {
		t.Fatalf("expected channel %d in rates", created.ID)
	}
	if got.SuccessRate < 0.39 || got.SuccessRate > 0.41 {
		t.Fatalf("expected success rate ~0.4, got %v", got.SuccessRate)
	}
	if got.SampleCount != 5 {
		t.Fatalf("expected sample count 5, got %d", got.SampleCount)
	}
}

func TestGetChannelSuccessRates_NoEligibleResults(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	dbPath := filepath.Join(tmpDir, "success_rate_empty.db")
	store, err := storage.CreateSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	cfg := &model.Config{
		Name:     "test-channel",
		URL:      "https://example.com",
		Priority: 10,
		ModelEntries: []model.ModelEntry{
			{Model: "model-a", RedirectModel: ""},
		},
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}

	now := time.Now()
	// 全部是应被忽略的客户端噪声
	logs := []*model.LogEntry{
		{Time: model.JSONTime{Time: now.Add(-10 * time.Second)}, ChannelID: created.ID, StatusCode: 404, Message: "not found"},
		{Time: model.JSONTime{Time: now.Add(-9 * time.Second)}, ChannelID: created.ID, StatusCode: 415, Message: "unsupported"},
		{Time: model.JSONTime{Time: now.Add(-8 * time.Second)}, ChannelID: created.ID, StatusCode: 499, Message: "client canceled"},
	}
	for _, e := range logs {
		if err := store.AddLog(ctx, e); err != nil {
			t.Fatalf("failed to add log: %v", err)
		}
	}

	rates, err := store.GetChannelSuccessRates(ctx, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("GetChannelSuccessRates error: %v", err)
	}
	if _, ok := rates[created.ID]; ok {
		t.Fatalf("expected no rate for channel %d when only client noise exists", created.ID)
	}
}
