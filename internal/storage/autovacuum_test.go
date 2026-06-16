package storage

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

func TestSQLiteAutoVacuumEnabled(t *testing.T) {
	testDB := t.TempDir() + "/test_autovacuum.db"
	defer func() { _ = os.Remove(testDB) }()

	// 创建 SQLite 存储实例（会触发 migrateSQLite）
	store, err := createSQLiteStore(testDB)
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 获取底层 SQL 连接验证 auto_vacuum 设置
	db, err := sql.Open("sqlite", "file:"+testDB)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	var mode int
	if err := db.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		t.Fatalf("查询 auto_vacuum 失败: %v", err)
	}

	if mode != 2 {
		t.Errorf("期望 auto_vacuum=2 (INCREMENTAL), 实际为 %d", mode)
	}

	t.Logf("✓ auto_vacuum=INCREMENTAL 已启用")
}

func TestSQLiteAutoVacuumOnExistingDB(t *testing.T) {
	testDB := t.TempDir() + "/test_autovacuum_existing.db"
	defer func() { _ = os.Remove(testDB) }()

	// 第一步：创建一个没有 auto_vacuum 的旧数据库
	db, err := sql.Open("sqlite", "file:"+testDB)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}

	ctx := context.Background()
	// 创建一张表和一些数据（模拟旧数据库）
	if _, err := db.ExecContext(ctx, "CREATE TABLE test (id INTEGER PRIMARY KEY, data TEXT)"); err != nil {
		t.Fatalf("创建表失败: %v", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO test (data) VALUES (?)", "test data"); err != nil {
			t.Fatalf("插入数据失败: %v", err)
		}
	}
	_ = db.Close()

	// 第二步：通过 createSQLiteStore 打开（应该自动启用 auto_vacuum）
	store, err := createSQLiteStore(testDB)
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 验证 auto_vacuum 已启用
	db2, err := sql.Open("sqlite", "file:"+testDB)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = db2.Close() }()

	var mode int
	if err := db2.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		t.Fatalf("查询 auto_vacuum 失败: %v", err)
	}

	if mode != 2 {
		t.Errorf("期望已有数据的数据库被迁移到 auto_vacuum=2, 实际为 %d", mode)
	}

	t.Logf("✓ 已有数据的数据库成功启用 auto_vacuum=INCREMENTAL")
}
