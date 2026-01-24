package app

import (
	"context"
	"math"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestFillHealthTimeline_UsesSecondsForAvgTimes(t *testing.T) {
	store, err := storage.CreateSQLiteStore(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	s := &Server{store: store}

	now := time.Now().Truncate(time.Second)
	startTime := now.Add(-24 * time.Hour)
	endTime := now

	channelID64 := int64(1)
	channelID := int(channelID64)
	modelName := "claude-test"

	logTime := now.Add(-12 * time.Hour)
	if err := store.AddLog(context.Background(), &model.LogEntry{
		Time:          model.JSONTime{Time: logTime},
		Model:         modelName,
		ActualModel:   modelName,
		ChannelID:     channelID64,
		StatusCode:    200,
		Message:       "ok",
		Duration:      2.3,
		IsStreaming:   true,
		FirstByteTime: 1.5,
	}); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}

	stats := []model.StatsEntry{
		{
			ChannelID: ptrInt(channelID),
			Model:     modelName,
		},
	}
	filter := &model.LogFilter{
		ChannelID: ptrInt64(channelID64),
		Model:     modelName,
	}

	s.fillHealthTimeline(context.Background(), stats, startTime, endTime, filter, false)

	if len(stats) == 0 {
		t.Fatal("stats 切片为空")
	}
	if len(stats[0].HealthTimeline) != 48 {
		t.Fatalf("期望 health timeline 长度=48，实际=%d", len(stats[0].HealthTimeline))
	}

	var found bool
	for _, point := range stats[0].HealthTimeline {
		if point.SuccessCount == 1 && point.ErrorCount == 0 {
			found = true
			if math.Abs(point.AvgFirstByteTime-1.5) > 1e-9 {
				t.Fatalf("AvgFirstByteTime 期望≈1.5(秒)，实际=%v", point.AvgFirstByteTime)
			}
			if math.Abs(point.AvgDuration-2.3) > 1e-9 {
				t.Fatalf("AvgDuration 期望≈2.3(秒)，实际=%v", point.AvgDuration)
			}
			break
		}
	}
	if !found {
		t.Fatalf("未找到包含写入日志的时间桶")
	}
}

func ptrInt64(v int64) *int64 { return &v }

func ptrInt(v int) *int { return &v }
