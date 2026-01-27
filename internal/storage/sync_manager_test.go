package storage

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

// createTestStoreForSync 创建测试用的存储
func createTestStoreForSync(t *testing.T, suffix string) *sqlstore.SQLStore {
	t.Helper()
	tmpDB := t.TempDir() + "/sync_" + suffix + ".db"
	store, err := CreateSQLiteStore(tmpDB)
	if err != nil {
		t.Fatalf("创建测试存储失败: %v", err)
	}
	return store.(*sqlstore.SQLStore)
}

func TestSyncManager_RestoreOnStartup_EmptyMySQL(t *testing.T) {
	// 模拟空的 MySQL（无数据需要恢复）
	mysql := createTestStoreForSync(t, "mysql_empty")
	sqlite := createTestStoreForSync(t, "sqlite_empty")
	defer func() {
		_ = mysql.Close()
		_ = sqlite.Close()
	}()

	sm := NewSyncManager(mysql, sqlite)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 空数据库恢复应该成功
	err := sm.RestoreOnStartup(ctx, 7)
	if err != nil {
		t.Fatalf("RestoreOnStartup 失败: %v", err)
	}
}

func TestSyncManager_RestoreOnStartup_WithData(t *testing.T) {
	// 创建 MySQL（源）和 SQLite（目标）
	mysql := createTestStoreForSync(t, "mysql_data")
	sqlite := createTestStoreForSync(t, "sqlite_data")
	defer func() {
		_ = mysql.Close()
		_ = sqlite.Close()
	}()

	ctx := context.Background()

	// 在 MySQL 中创建测试数据
	cfg := &model.Config{
		Name:        "test-channel",
		ChannelType: "openai",
		URL:         "https://api.openai.com",
		Priority:    100,
		Enabled:     true,
	}
	created, err := mysql.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试数据失败: %v", err)
	}

	// 验证 SQLite 中没有数据
	_, err = sqlite.GetConfig(ctx, created.ID)
	if err == nil {
		t.Fatal("SQLite 中不应该有数据")
	}

	// 执行恢复
	sm := NewSyncManager(mysql, sqlite)
	restoreCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	err = sm.RestoreOnStartup(restoreCtx, 0) // 0 = 不恢复日志
	if err != nil {
		t.Fatalf("RestoreOnStartup 失败: %v", err)
	}

	// 验证 SQLite 中有数据了
	restored, err := sqlite.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("恢复后获取配置失败: %v", err)
	}
	if restored.Name != cfg.Name {
		t.Errorf("恢复的配置名称不匹配: got %s, want %s", restored.Name, cfg.Name)
	}
}

func TestSyncManager_RestoreLogsIncremental(t *testing.T) {
	mysql := createTestStoreForSync(t, "mysql_logs")
	sqlite := createTestStoreForSync(t, "sqlite_logs")
	defer func() {
		_ = mysql.Close()
		_ = sqlite.Close()
	}()

	ctx := context.Background()

	// 在 MySQL 中添加日志
	now := time.Now()
	for i := 0; i < 5; i++ {
		entry := &model.LogEntry{
			Time:       model.JSONTime{Time: now.Add(-time.Duration(i) * time.Hour)},
			ChannelID:  1,
			Model:      "gpt-4",
			StatusCode: 200,
			Duration:   1.5,
		}
		if err := mysql.AddLog(ctx, entry); err != nil {
			t.Fatalf("添加日志失败: %v", err)
		}
	}

	// 验证 MySQL 有日志
	mysqlLogs, err := mysql.ListLogs(ctx, now.Add(-24*time.Hour), 100, 0, nil)
	if err != nil {
		t.Fatalf("查询 MySQL 日志失败: %v", err)
	}
	if len(mysqlLogs) != 5 {
		t.Fatalf("MySQL 日志数量不匹配: got %d, want 5", len(mysqlLogs))
	}

	// 执行恢复（包含日志）
	sm := NewSyncManager(mysql, sqlite)
	restoreCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	err = sm.RestoreOnStartup(restoreCtx, 7) // 恢复最近 7 天日志
	if err != nil {
		t.Fatalf("RestoreOnStartup 失败: %v", err)
	}

	// 验证 SQLite 有日志了
	sqliteLogs, err := sqlite.ListLogs(ctx, now.Add(-24*time.Hour), 100, 0, nil)
	if err != nil {
		t.Fatalf("查询 SQLite 日志失败: %v", err)
	}
	if len(sqliteLogs) != 5 {
		t.Errorf("SQLite 日志数量不匹配: got %d, want 5", len(sqliteLogs))
	}
}

func TestSyncManager_RestoreLogsIncremental_ZeroDays(t *testing.T) {
	mysql := createTestStoreForSync(t, "mysql_nologs")
	sqlite := createTestStoreForSync(t, "sqlite_nologs")
	defer func() {
		_ = mysql.Close()
		_ = sqlite.Close()
	}()

	ctx := context.Background()

	// 在 MySQL 中添加日志
	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		ChannelID:  1,
		Model:      "gpt-4",
		StatusCode: 200,
		Duration:   1.5,
	}
	if err := mysql.AddLog(ctx, entry); err != nil {
		t.Fatalf("添加日志失败: %v", err)
	}

	// 执行恢复（logDays=0，不恢复日志）
	sm := NewSyncManager(mysql, sqlite)
	restoreCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	err := sm.RestoreOnStartup(restoreCtx, 0) // 0 = 不恢复日志
	if err != nil {
		t.Fatalf("RestoreOnStartup 失败: %v", err)
	}

	// 验证 SQLite 没有日志（因为 logDays=0）
	sqliteLogs, err := sqlite.ListLogs(ctx, time.Now().Add(-24*time.Hour), 100, 0, nil)
	if err != nil {
		t.Fatalf("查询 SQLite 日志失败: %v", err)
	}
	if len(sqliteLogs) != 0 {
		t.Errorf("SQLite 不应该有日志（logDays=0），got %d", len(sqliteLogs))
	}
}
