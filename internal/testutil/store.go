package testutil

import (
	"testing"

	"ccLoad/internal/storage"
)

// SetupTestStore 创建一个用于测试的 SQLite 存储实例
// 返回 store 实例和 cleanup 函数
// 使用方式：store, cleanup := testutil.SetupTestStore(t); defer cleanup()
func SetupTestStore(t testing.TB) (storage.Store, func()) {
	t.Helper()

	tmpDB := t.TempDir() + "/test.db"
	store, err := storage.CreateSQLiteStore(tmpDB)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	cleanup := func() {
		if err := store.Close(); err != nil {
			t.Logf("关闭测试数据库失败: %v", err)
		}
	}

	return store, cleanup
}
