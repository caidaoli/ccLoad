package integration_test

import (
	"ccLoad/internal/storage"
	"context"
	"path/filepath"
	"testing"
)

// setupTestStore 创建测试用的 SQLite Store
func setupTestStore(t *testing.T) (storage.Store, func()) {
	t.Helper()

	// 创建临时目录和数据库文件
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := storage.CreateSQLiteStore(dbPath, nil)
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
func setupTestStoreWithContext(t *testing.T) (storage.Store, context.Context, func()) {
	t.Helper()

	store, cleanup := setupTestStore(t)
	ctx := context.Background()

	return store, ctx, cleanup
}
