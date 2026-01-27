package storage

import (
	"context"
	"errors"
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

// TestSyncManager_RestoreLogsIncremental_TrueIncremental 验证真正的增量恢复：
// SQLite 已有部分数据时，只拉取新增的记录
func TestSyncManager_RestoreLogsIncremental_TrueIncremental(t *testing.T) {
	mysql := createTestStoreForSync(t, "mysql_incr")
	sqlite := createTestStoreForSync(t, "sqlite_incr")
	defer func() {
		_ = mysql.Close()
		_ = sqlite.Close()
	}()

	ctx := context.Background()
	now := time.Now()

	// 第一步：在 MySQL 中添加 3 条日志
	for i := 0; i < 3; i++ {
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

	// 第二步：第一次恢复
	sm := NewSyncManager(mysql, sqlite)
	if err := sm.RestoreOnStartup(ctx, 7); err != nil {
		t.Fatalf("第一次 RestoreOnStartup 失败: %v", err)
	}

	// 验证 SQLite 有 3 条日志
	sqliteLogs, err := sqlite.ListLogs(ctx, now.Add(-24*time.Hour), 100, 0, nil)
	if err != nil {
		t.Fatalf("查询 SQLite 日志失败: %v", err)
	}
	if len(sqliteLogs) != 3 {
		t.Fatalf("第一次恢复后 SQLite 日志数量不匹配: got %d, want 3", len(sqliteLogs))
	}

	// 第三步：在 MySQL 中再添加 2 条新日志
	for i := 0; i < 2; i++ {
		entry := &model.LogEntry{
			Time:       model.JSONTime{Time: now.Add(time.Duration(i+1) * time.Minute)}, // 新增时间更晚
			ChannelID:  2,
			Model:      "gpt-3.5",
			StatusCode: 200,
			Duration:   0.5,
		}
		if err := mysql.AddLog(ctx, entry); err != nil {
			t.Fatalf("添加新日志失败: %v", err)
		}
	}

	// 第四步：第二次恢复（增量）
	sm2 := NewSyncManager(mysql, sqlite)
	if err := sm2.RestoreOnStartup(ctx, 7); err != nil {
		t.Fatalf("第二次 RestoreOnStartup 失败: %v", err)
	}

	// 验证 SQLite 现在有 5 条日志（3 + 2）
	sqliteLogs, err = sqlite.ListLogs(ctx, now.Add(-24*time.Hour), 100, 0, nil)
	if err != nil {
		t.Fatalf("查询 SQLite 日志失败: %v", err)
	}
	if len(sqliteLogs) != 5 {
		t.Fatalf("第二次恢复后 SQLite 日志数量不匹配: got %d, want 5", len(sqliteLogs))
	}

	// 验证原有数据未被删除（检查 channel_id=1 的记录仍然存在）
	count1 := 0
	count2 := 0
	for _, entry := range sqliteLogs {
		switch entry.ChannelID {
		case 1:
			count1++
		case 2:
			count2++
		}
	}
	if count1 != 3 {
		t.Errorf("原有日志（channel_id=1）被意外修改: got %d, want 3", count1)
	}
	if count2 != 2 {
		t.Errorf("新增日志（channel_id=2）数量不对: got %d, want 2", count2)
	}
}

type fakeRowsErrAfterOne struct {
	scanned bool
	err     error
}

func (r *fakeRowsErrAfterOne) Next() bool {
	if r.scanned {
		return false
	}
	r.scanned = true
	return true
}

func (r *fakeRowsErrAfterOne) Scan(dest ...any) error {
	for i := range dest {
		*(dest[i].(*any)) = int64(i + 1)
	}
	return nil
}

func (r *fakeRowsErrAfterOne) Err() error { return r.err }

func TestSyncManager_InsertLogBatch_ChecksRowsErr(t *testing.T) {
	mysql := createTestStoreForSync(t, "mysql_data")
	sqlite := createTestStoreForSync(t, "sqlite_data")
	defer func() {
		_ = mysql.Close()
		_ = sqlite.Close()
	}()

	sm := NewSyncManager(mysql, sqlite)

	_, _, err := sm.insertLogBatchWithLastID(
		context.Background(),
		&fakeRowsErrAfterOne{err: errors.New("driver error")},
		2,
		[]string{"id"},
		[]int{0},
	)
	if err == nil {
		t.Fatalf("期望返回错误，但得到 nil")
	}
}
