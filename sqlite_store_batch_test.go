package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// BenchmarkFetchChannelNamesBatch 基准测试：批量查询渠道名称性能
// 对比场景：N+1查询 vs 全表查询+内存过滤
func BenchmarkFetchChannelNamesBatch(b *testing.B) {
	ctx := context.Background()
	store, cleanup := setupTestStore(b)
	defer cleanup()

	// 创建测试渠道数据
	channelIDs := make(map[int64]bool)
	for i := 1; i <= 100; i++ {
		cfg := &Config{
			Name:     fmt.Sprintf("TestChannel-%d", i),
			APIKey:   fmt.Sprintf("sk-test-%d", i),
			URL:      "https://api.example.com",
			Priority: 10,
			Models:   []string{"test-model"},
			Enabled:  true,
		}
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			b.Fatalf("创建测试渠道失败: %v", err)
		}
		channelIDs[created.ID] = true
	}

	b.ResetTimer()

	// 批量查询基准测试
	for i := 0; i < b.N; i++ {
		_, err := store.fetchChannelNamesBatch(ctx, channelIDs)
		if err != nil {
			b.Fatalf("批量查询失败: %v", err)
		}
	}
}

// setupTestStore 创建测试用SQLite存储（内存模式）
func setupTestStore(t testing.TB) (*SQLiteStore, func()) {
	t.Helper()

	store, err := NewSQLiteStore(":memory:", nil)
	if err != nil {
		t.Fatalf("创建测试存储失败: %v", err)
	}

	cleanup := func() {
		if err := store.Close(); err != nil {
			t.Errorf("关闭存储失败: %v", err)
		}
	}

	return store, cleanup
}

// TestFetchChannelNamesBatch_Correctness 正确性测试：验证批量查询结果
func TestFetchChannelNamesBatch_Correctness(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	// 创建测试渠道
	expectedNames := make(map[int64]string)
	channelIDs := make(map[int64]bool)

	for i := 1; i <= 10; i++ {
		cfg := &Config{
			Name:     fmt.Sprintf("Channel-%d", i),
			APIKey:   fmt.Sprintf("sk-key-%d", i),
			URL:      "https://api.test.com",
			Priority: i,
			Models:   []string{"model-a"},
			Enabled:  true,
		}
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建渠道失败: %v", err)
		}
		expectedNames[created.ID] = created.Name
		channelIDs[created.ID] = true
	}

	// 批量查询
	actualNames, err := store.fetchChannelNamesBatch(ctx, channelIDs)
	if err != nil {
		t.Fatalf("批量查询失败: %v", err)
	}

	// 验证结果完整性
	if len(actualNames) != len(expectedNames) {
		t.Errorf("结果数量不匹配: 期望 %d, 实际 %d", len(expectedNames), len(actualNames))
	}

	// 验证每个渠道名称
	for id, expectedName := range expectedNames {
		actualName, exists := actualNames[id]
		if !exists {
			t.Errorf("渠道 ID %d 未找到", id)
			continue
		}
		if actualName != expectedName {
			t.Errorf("渠道 ID %d 名称不匹配: 期望 %s, 实际 %s", id, expectedName, actualName)
		}
	}
}

// TestFetchChannelNamesBatch_EmptyInput 边界测试：空输入
func TestFetchChannelNamesBatch_EmptyInput(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	emptyMap := make(map[int64]bool)
	result, err := store.fetchChannelNamesBatch(ctx, emptyMap)

	if err != nil {
		t.Errorf("空输入不应返回错误: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("空输入应返回空结果，实际长度: %d", len(result))
	}
}

// TestListLogs_BatchQuery 集成测试：验证ListLogs使用批量查询
func TestListLogs_BatchQuery(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	// 创建多个渠道
	channelIDs := make([]int64, 0, 5)
	for i := 1; i <= 5; i++ {
		cfg := &Config{
			Name:     fmt.Sprintf("LogChannel-%d", i),
			APIKey:   fmt.Sprintf("sk-log-%d", i),
			URL:      "https://api.test.com",
			Priority: i,
			Models:   []string{"test-model"},
			Enabled:  true,
		}
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建渠道失败: %v", err)
		}
		channelIDs = append(channelIDs, created.ID)
	}

	// 创建测试日志（分布在不同渠道）
	for i, channelID := range channelIDs {
		entry := &LogEntry{
			Time:       JSONTime{time.Now()},
			Model:      "test-model",
			ChannelID:  &channelID,
			StatusCode: 200,
			Message:    fmt.Sprintf("Test log %d", i),
			Duration:   0.5,
		}
		if err := store.AddLog(ctx, entry); err != nil {
			t.Fatalf("添加日志失败: %v", err)
		}
	}

	// 查询日志（应使用批量查询获取渠道名称）
	logs, err := store.ListLogs(ctx, time.Now().Add(-1*time.Hour), 100, 0, nil)
	if err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}

	// 验证所有日志都包含渠道名称
	for _, log := range logs {
		if log.ChannelID == nil {
			continue // 跳过系统日志
		}
		if log.ChannelName == "" {
			t.Errorf("日志 ID %d 缺少渠道名称（渠道ID: %d）", log.ID, *log.ChannelID)
		}
		// 验证名称格式
		expectedPrefix := "LogChannel-"
		if !strings.HasPrefix(log.ChannelName, expectedPrefix) {
			t.Errorf("渠道名称格式错误: %s (期望前缀: %s)", log.ChannelName, expectedPrefix)
		}
	}
}
