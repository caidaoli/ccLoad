package storage

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestCreateMySQLStoreForTest_InvalidDSNFastFail(t *testing.T) {
	// 缺少 "/" 的 DSN：应在 driver 解析阶段快速失败，不进行网络连接。
	store, err := CreateMySQLStoreForTest("invalid-dsn")
	if err == nil || store != nil {
		if store != nil {
			_ = store.Close()
		}
		t.Fatalf("expected error and nil store")
	}
}

func TestMigrateMySQL_FailsOnSQLiteDB(t *testing.T) {
	// 用 SQLite DB 调 migrateMySQL：必然失败（DDL 方言不匹配），但能覆盖 MySQL 迁移入口的错误路径。
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/migrate_mysql_fail.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := migrateMySQL(ctx, db); err == nil {
		t.Fatalf("expected migrateMySQL to fail on sqlite db")
	}
}
