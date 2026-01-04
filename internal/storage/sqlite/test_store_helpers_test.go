package sqlite_test

import (
	"ccLoad/internal/storage"
	"testing"
)

func setupSQLiteTestStore(t testing.TB, dbFile string) (storage.Store, func()) {
	t.Helper()

	tmpDB := t.TempDir() + "/" + dbFile
	store, err := storage.CreateSQLiteStore(tmpDB, nil)
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
