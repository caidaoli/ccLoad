package integration_test

import (
	"ccLoad/internal/storage/sqlite"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// setupTestStore 创建测试用的 SQLite Store
// 强制使用文件模式，避免 Redis 依赖
// 集成测试应该能够在没有外部依赖（Redis）的情况下运行
func setupTestStore(t *testing.T) (*sqlite.SQLiteStore, func()) {
	t.Helper()

	// 强制禁用内存模式，使用临时文件数据库
	// 这样可以避免集成测试依赖 Redis
	os.Setenv("CCLOAD_USE_MEMORY_DB", "false")
	defer os.Unsetenv("CCLOAD_USE_MEMORY_DB")

	// 创建临时目录和数据库文件
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := sqlite.NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	// 返回清理函数
	cleanup := func() {
		if err := store.Close(); err != nil {
			t.Logf("⚠️  关闭数据库失败: %v", err)
		}
	}

	return store, cleanup
}

// setupTestStoreWithContext 创建测试用的 Store 和 Context
func setupTestStoreWithContext(t *testing.T) (*sqlite.SQLiteStore, context.Context, func()) {
	t.Helper()

	store, cleanup := setupTestStore(t)
	ctx := context.Background()

	return store, ctx, cleanup
}
