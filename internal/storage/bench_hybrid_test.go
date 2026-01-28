package storage_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/joho/godotenv"
)

// 本文件包含混合存储模式的性能对比测试
//
// 测试场景：
//   - SQLite（本地）vs MySQL（远程）的读写延迟对比
//   - 混合模式（读 SQLite + 写 MySQL）的性能表现
//
// 运行方式：
//   go test -tags go_json -bench=BenchmarkHybrid -benchtime=3s ./internal/storage/...
//
// 环境变量（从 .env 读取）：
//   - CCLOAD_MYSQL: MySQL DSN（必需）

func init() {
	// 尝试从项目根目录加载 .env
	for _, path := range []string{".env", "../../.env", "../../../.env"} {
		if err := godotenv.Load(path); err == nil {
			break
		}
	}
}

// skipIfNoMySQL 如果没有配置 MySQL 则跳过测试
func skipIfNoMySQL(b *testing.B) string {
	dsn := os.Getenv("CCLOAD_MYSQL")
	if dsn == "" {
		b.Skip("跳过: 需要设置 CCLOAD_MYSQL 环境变量")
	}
	return dsn
}

// createBenchSQLite 创建临时 SQLite 存储
func createBenchSQLite(b *testing.B) storage.Store {
	b.Helper()
	tmp := b.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "bench.db"))
	if err != nil {
		b.Fatalf("创建 SQLite 失败: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

// createBenchMySQL 创建 MySQL 存储（复用连接）
func createBenchMySQL(b *testing.B, dsn string) storage.Store {
	b.Helper()
	store, err := storage.CreateMySQLStoreForTest(dsn)
	if err != nil {
		b.Fatalf("创建 MySQL 失败: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

// ============================================================================
// 渠道配置读取性能对比
// ============================================================================

func BenchmarkHybrid_ListConfigs_SQLite(b *testing.B) {
	store := createBenchSQLite(b)
	ctx := context.Background()

	// 准备测试数据
	for i := 0; i < 10; i++ {
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:        fmt.Sprintf("bench-channel-%d", i),
			ChannelType: "openai",
			URL:         "https://api.openai.com",
			Priority:    100,
			Enabled:     true,
		})
		if err != nil {
			b.Fatalf("创建渠道失败: %v", err)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := store.ListConfigs(ctx)
		if err != nil {
			b.Fatalf("ListConfigs 失败: %v", err)
		}
	}
}

func BenchmarkHybrid_ListConfigs_MySQL(b *testing.B) {
	dsn := skipIfNoMySQL(b)
	store := createBenchMySQL(b, dsn)
	ctx := context.Background()

	// 使用已有数据（避免污染生产数据库）
	// 如果数据库为空，结果可能不准确

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := store.ListConfigs(ctx)
		if err != nil {
			b.Fatalf("ListConfigs 失败: %v", err)
		}
	}
}

// ============================================================================
// 日志写入性能对比
// ============================================================================

func BenchmarkHybrid_AddLog_SQLite(b *testing.B) {
	store := createBenchSQLite(b)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		err := store.AddLog(ctx, &model.LogEntry{
			Time:       model.JSONTime{Time: time.Now()},
			ChannelID:  1,
			Model:      "gpt-4",
			StatusCode: 200,
			Duration:   1.5,
		})
		if err != nil {
			b.Fatalf("AddLog 失败: %v", err)
		}
	}
}

func BenchmarkHybrid_AddLog_MySQL(b *testing.B) {
	dsn := skipIfNoMySQL(b)
	store := createBenchMySQL(b, dsn)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		err := store.AddLog(ctx, &model.LogEntry{
			Time:       model.JSONTime{Time: time.Now()},
			ChannelID:  1,
			Model:      "bench-model",
			StatusCode: 200,
			Duration:   1.5,
			Message:    "benchmark test",
		})
		if err != nil {
			b.Fatalf("AddLog 失败: %v", err)
		}
	}
}

// ============================================================================
// 日志查询性能对比
// ============================================================================

func BenchmarkHybrid_ListLogs_SQLite(b *testing.B) {
	store := createBenchSQLite(b)
	ctx := context.Background()

	// 准备测试数据
	for i := 0; i < 100; i++ {
		_ = store.AddLog(ctx, &model.LogEntry{
			Time:       model.JSONTime{Time: time.Now().Add(-time.Duration(i) * time.Minute)},
			ChannelID:  int64(i % 5),
			Model:      "gpt-4",
			StatusCode: 200,
			Duration:   1.5,
		})
	}

	since := time.Now().Add(-24 * time.Hour)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := store.ListLogs(ctx, since, 50, 0, nil)
		if err != nil {
			b.Fatalf("ListLogs 失败: %v", err)
		}
	}
}

func BenchmarkHybrid_ListLogs_MySQL(b *testing.B) {
	dsn := skipIfNoMySQL(b)
	store := createBenchMySQL(b, dsn)
	ctx := context.Background()

	since := time.Now().Add(-24 * time.Hour)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := store.ListLogs(ctx, since, 50, 0, nil)
		if err != nil {
			b.Fatalf("ListLogs 失败: %v", err)
		}
	}
}

// ============================================================================
// 统计查询性能对比（复杂聚合）
// ============================================================================

func BenchmarkHybrid_GetStats_SQLite(b *testing.B) {
	store := createBenchSQLite(b)
	ctx := context.Background()

	// 准备测试数据
	for i := 0; i < 200; i++ {
		_ = store.AddLog(ctx, &model.LogEntry{
			Time:         model.JSONTime{Time: time.Now().Add(-time.Duration(i) * time.Minute)},
			ChannelID:    int64(i % 5),
			Model:        fmt.Sprintf("model-%d", i%3),
			StatusCode:   200,
			Duration:     float64(i%10) * 0.5,
			InputTokens:  100 + i,
			OutputTokens: 50 + i,
			Cost:         float64(i) * 0.001,
		})
	}

	now := time.Now()
	startTime := now.Add(-24 * time.Hour)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := store.GetStats(ctx, startTime, now, nil, true)
		if err != nil {
			b.Fatalf("GetStats 失败: %v", err)
		}
	}
}

func BenchmarkHybrid_GetStats_MySQL(b *testing.B) {
	dsn := skipIfNoMySQL(b)
	store := createBenchMySQL(b, dsn)
	ctx := context.Background()

	now := time.Now()
	startTime := now.Add(-24 * time.Hour)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := store.GetStats(ctx, startTime, now, nil, true)
		if err != nil {
			b.Fatalf("GetStats 失败: %v", err)
		}
	}
}

// ============================================================================
// 并发读取性能对比
// ============================================================================

func BenchmarkHybrid_ListConfigs_SQLite_Parallel(b *testing.B) {
	store := createBenchSQLite(b)
	ctx := context.Background()

	// 准备测试数据
	for i := 0; i < 10; i++ {
		_, _ = store.CreateConfig(ctx, &model.Config{
			Name:        fmt.Sprintf("bench-parallel-%d", i),
			ChannelType: "openai",
			URL:         "https://api.openai.com",
			Priority:    100,
			Enabled:     true,
		})
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := store.ListConfigs(ctx)
			if err != nil {
				b.Errorf("ListConfigs 失败: %v", err)
			}
		}
	})
}

func BenchmarkHybrid_ListConfigs_MySQL_Parallel(b *testing.B) {
	dsn := skipIfNoMySQL(b)
	store := createBenchMySQL(b, dsn)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := store.ListConfigs(ctx)
			if err != nil {
				b.Errorf("ListConfigs 失败: %v", err)
			}
		}
	})
}

// ============================================================================
// 并发日志写入性能对比
// ============================================================================

func BenchmarkHybrid_AddLog_SQLite_Parallel(b *testing.B) {
	store := createBenchSQLite(b)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := store.AddLog(ctx, &model.LogEntry{
				Time:       model.JSONTime{Time: time.Now()},
				ChannelID:  1,
				Model:      "gpt-4",
				StatusCode: 200,
				Duration:   1.5,
			})
			if err != nil {
				b.Errorf("AddLog 失败: %v", err)
			}
		}
	})
}

func BenchmarkHybrid_AddLog_MySQL_Parallel(b *testing.B) {
	dsn := skipIfNoMySQL(b)
	store := createBenchMySQL(b, dsn)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := store.AddLog(ctx, &model.LogEntry{
				Time:       model.JSONTime{Time: time.Now()},
				ChannelID:  1,
				Model:      "bench-parallel",
				StatusCode: 200,
				Duration:   1.5,
			})
			if err != nil {
				b.Errorf("AddLog 失败: %v", err)
			}
		}
	})
}
