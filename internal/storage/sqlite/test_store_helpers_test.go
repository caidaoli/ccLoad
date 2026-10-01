package sqlite_test

import (
	"testing"

	"ccLoad/internal/storage"
)

func setupSQLiteTestStore(t testing.TB) (storage.Store, func()) {
	t.Helper()

	store, err := storage.CreateSQLiteStore(":memory:")
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
