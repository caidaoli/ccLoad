package storage

import (
	"context"
	"database/sql"
	"fmt"
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

func TestSyncManager_RestoreOnStartup_EmptySourceClearsStaleReplica(t *testing.T) {
	source := createTestStoreForSync(t, "empty_source")
	target := createTestStoreForSync(t, "stale_target")
	defer func() {
		_ = source.Close()
		_ = target.Close()
	}()

	ctx := context.Background()
	if _, err := target.CreateConfig(ctx, &model.Config{
		Name:     "stale-channel",
		URLs:     model.ChannelURLs{{URL: "https://stale.example.com"}},
		Enabled:  true,
		Priority: 1,
		ModelEntries: []model.ModelEntry{{
			Model: "stale-model",
		}},
	}); err != nil {
		t.Fatalf("create stale replica config: %v", err)
	}
	staleToken := &model.AuthToken{
		Token:       model.HashToken("stale-token"),
		Description: "stale-token",
		CreatedAt:   time.Now(),
		IsActive:    true,
	}
	if err := target.CreateAuthToken(ctx, staleToken); err != nil {
		t.Fatalf("create stale replica auth token: %v", err)
	}

	if err := NewSyncManager(source, target).RestoreOnStartup(ctx, 0); err != nil {
		t.Fatalf("restore empty source: %v", err)
	}
	configs, err := target.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("list target configs: %v", err)
	}
	if len(configs) != 0 {
		t.Fatalf("stale replica configs survived empty source: %+v", configs)
	}
	tokens, err := target.ListAuthTokens(ctx)
	if err != nil {
		t.Fatalf("list target auth tokens: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("stale replica auth tokens survived empty source: %+v", tokens)
	}
}

func TestSyncManager_RestoreOnStartup_RestoresChannelURLStates(t *testing.T) {
	source := createTestStoreForSync(t, "url_state_source")
	target := createTestStoreForSync(t, "url_state_target")
	defer func() {
		_ = source.Close()
		_ = target.Close()
	}()

	ctx := context.Background()
	const channelURL = "https://disabled.example.com"
	created, err := source.CreateConfig(ctx, &model.Config{
		Name:     "disabled-url-channel",
		URLs:     model.ChannelURLs{{URL: channelURL}},
		Enabled:  true,
		Priority: 1,
	})
	if err != nil {
		t.Fatalf("create source config: %v", err)
	}
	if err := source.SetURLDisabled(ctx, created.ID, channelURL, true); err != nil {
		t.Fatalf("disable source URL: %v", err)
	}

	if err := NewSyncManager(source, target).RestoreOnStartup(ctx, 0); err != nil {
		t.Fatalf("restore URL state: %v", err)
	}
	disabled, err := target.LoadDisabledURLs(ctx)
	if err != nil {
		t.Fatalf("load restored URL states: %v", err)
	}
	if len(disabled[created.ID]) != 1 || disabled[created.ID][0] != channelURL {
		t.Fatalf("restored URL states=%v, want channel %d URL %q", disabled, created.ID, channelURL)
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
		Name:     "test-channel",
		URLs:     model.ChannelURLs{{URL: "https://api.openai.com"}},
		Priority: 100,
		Enabled:  true,
	}
	created, err := mysql.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试数据失败: %v", err)
	}
	if _, err := mysql.ExecContext(ctx, "UPDATE channels SET oauth_credential = NULL WHERE id = ?", created.ID); err != nil {
		t.Fatalf("预置旧渠道空凭证失败: %v", err)
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
	if restored.OAuthCredential != "" {
		t.Errorf("恢复的 OAuthCredential = %q, want empty", restored.OAuthCredential)
	}
}

func TestSyncManager_RestoreOnStartup_RestoresMoreThanTenThousandConfigRows(t *testing.T) {
	source := createTestStoreForSync(t, "large_source")
	target := createTestStoreForSync(t, "large_target")
	defer func() {
		_ = source.Close()
		_ = target.Close()
	}()

	const modelCount = 10001
	entries := make([]model.ModelEntry, modelCount)
	for i := range entries {
		name := fmt.Sprintf("model-%05d", i)
		entries[i] = model.ModelEntry{Model: name, RedirectModel: name}
	}

	ctx := context.Background()
	if _, err := source.CreateConfig(ctx, &model.Config{
		Name:         "large-channel",
		URLs:         model.ChannelURLs{{URL: "https://large.example.com"}},
		Priority:     1,
		Enabled:      true,
		ModelEntries: entries,
	}); err != nil {
		t.Fatalf("create large source config: %v", err)
	}

	restoreCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := NewSyncManager(source, target).RestoreOnStartup(restoreCtx, 0); err != nil {
		t.Fatalf("restore more than 10000 config rows: %v", err)
	}

	var restored int
	if err := target.QueryRowContext(ctx, "SELECT COUNT(*) FROM channel_models").Scan(&restored); err != nil {
		t.Fatalf("count restored channel models: %v", err)
	}
	if restored != modelCount {
		t.Fatalf("restored channel models=%d, want %d", restored, modelCount)
	}
}

func TestSyncManager_RestoreOnStartup_NormalizesNullCredentialForLegacySQLiteReplica(t *testing.T) {
	source := createTestStoreForSync(t, "nullable_credential_source")
	defer func() { _ = source.Close() }()

	targetPath := t.TempDir() + "/legacy_not_null_replica.db"
	targetDB, err := sql.Open("sqlite", targetPath)
	if err != nil {
		t.Fatalf("open legacy SQLite replica: %v", err)
	}
	if _, err := targetDB.Exec(`
		CREATE TABLE channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			url TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			oauth_credential TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			cooldown_until INTEGER NOT NULL DEFAULT 0,
			cooldown_duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`); err != nil {
		_ = targetDB.Close()
		t.Fatalf("create legacy SQLite channels: %v", err)
	}
	if err := targetDB.Close(); err != nil {
		t.Fatalf("close legacy SQLite setup: %v", err)
	}

	targetStore, err := CreateSQLiteStore(targetPath)
	if err != nil {
		t.Fatalf("migrate legacy SQLite replica: %v", err)
	}
	target := targetStore.(*sqlstore.SQLStore)
	defer func() { _ = target.Close() }()

	ctx := context.Background()
	created, err := source.CreateConfig(ctx, &model.Config{
		Name:     "nullable-credential",
		URLs:     model.ChannelURLs{{URL: "https://legacy.example.com"}},
		Priority: 1,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create source channel: %v", err)
	}
	if _, err := source.ExecContext(ctx, "UPDATE channels SET oauth_credential = NULL WHERE id = ?", created.ID); err != nil {
		t.Fatalf("set source credential NULL: %v", err)
	}

	restoreCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := NewSyncManager(source, target).RestoreOnStartup(restoreCtx, 0); err != nil {
		t.Fatalf("restore into legacy SQLite replica: %v", err)
	}

	var credential string
	if err := target.QueryRowContext(ctx, "SELECT oauth_credential FROM channels WHERE id = ?", created.ID).Scan(&credential); err != nil {
		t.Fatalf("read restored credential: %v", err)
	}
	if credential != "" {
		t.Fatalf("restored credential=%q, want empty", credential)
	}
}

func TestSyncManager_RestoreLogsSnapshot(t *testing.T) {
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

func TestSyncManager_RestoreLogs_DoesNotDuplicateSameLogWithDifferentIDs(t *testing.T) {
	mysql := createTestStoreForSync(t, "mysql_different_ids")
	sqlite := createTestStoreForSync(t, "sqlite_different_ids")
	defer func() {
		_ = mysql.Close()
		_ = sqlite.Close()
	}()

	ctx := context.Background()
	now := time.Now()

	// 先推进 MySQL 自增序列，模拟两库独立分配日志 ID。
	for i := 0; i < 3; i++ {
		if err := mysql.AddLog(ctx, &model.LogEntry{
			Time:       model.JSONTime{Time: now.AddDate(0, 0, -8).Add(time.Duration(i) * time.Minute)},
			ChannelID:  99,
			Model:      "old",
			StatusCode: 200,
			Message:    "outside restore window",
		}); err != nil {
			t.Fatalf("添加窗口外 MySQL 日志: %v", err)
		}
	}

	entry := &model.LogEntry{
		Time:         model.JSONTime{Time: now},
		ChannelID:    280,
		Model:        "gpt-5.6-sol",
		StatusCode:   200,
		Message:      "ok",
		Duration:     1.25,
		InputTokens:  100,
		OutputTokens: 20,
	}
	if err := mysql.AddLog(ctx, entry); err != nil {
		t.Fatalf("添加 MySQL 日志: %v", err)
	}
	if err := sqlite.AddLog(ctx, entry); err != nil {
		t.Fatalf("添加已有 SQLite 日志: %v", err)
	}

	if err := NewSyncManager(mysql, sqlite).RestoreOnStartup(ctx, 7); err != nil {
		t.Fatalf("RestoreOnStartup: %v", err)
	}

	logs, err := sqlite.ListLogs(ctx, now.Add(-time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("查询恢复后的 SQLite 日志: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("同一业务日志被重复恢复: got %d rows, want 1", len(logs))
	}
}

func TestSyncManager_RestoreLogs_RestoresRowsWhenSQLiteIDIsAhead(t *testing.T) {
	mysql := createTestStoreForSync(t, "mysql_id_behind")
	sqlite := createTestStoreForSync(t, "sqlite_id_ahead")
	defer func() {
		_ = mysql.Close()
		_ = sqlite.Close()
	}()

	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 5; i++ {
		if err := sqlite.AddLog(ctx, &model.LogEntry{
			Time:       model.JSONTime{Time: now.AddDate(0, 0, -8).Add(time.Duration(i) * time.Minute)},
			ChannelID:  99,
			Model:      "old-local",
			StatusCode: 200,
			Message:    "outside restore window",
		}); err != nil {
			t.Fatalf("添加窗口外 SQLite 日志: %v", err)
		}
	}

	first := &model.LogEntry{
		Time:       model.JSONTime{Time: now.Add(-time.Hour)},
		ChannelID:  280,
		Model:      "gpt-5.6-sol",
		StatusCode: 200,
		Message:    "first",
	}
	second := &model.LogEntry{
		Time:       model.JSONTime{Time: now},
		ChannelID:  280,
		Model:      "gpt-5.6-sol",
		StatusCode: 500,
		Message:    "second",
	}
	if err := mysql.AddLog(ctx, first); err != nil {
		t.Fatalf("添加第一条 MySQL 日志: %v", err)
	}
	if err := mysql.AddLog(ctx, second); err != nil {
		t.Fatalf("添加第二条 MySQL 日志: %v", err)
	}
	if err := sqlite.AddLog(ctx, first); err != nil {
		t.Fatalf("添加已有 SQLite 日志: %v", err)
	}

	if err := NewSyncManager(mysql, sqlite).RestoreOnStartup(ctx, 7); err != nil {
		t.Fatalf("RestoreOnStartup: %v", err)
	}

	recentLogs, err := sqlite.ListLogs(ctx, now.Add(-2*time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("查询恢复后的 SQLite 日志: %v", err)
	}
	if len(recentLogs) != 2 {
		t.Fatalf("SQLite ID 超前时漏恢复日志: got %d rows, want 2", len(recentLogs))
	}
	oldLogs, err := sqlite.ListLogs(ctx, now.AddDate(0, 0, -9), 20, 0, nil)
	if err != nil {
		t.Fatalf("查询 SQLite 全部日志: %v", err)
	}
	if len(oldLogs) != 7 {
		t.Fatalf("恢复窗口外日志被修改: got %d total rows, want 7", len(oldLogs))
	}
}

func TestSyncManager_RestoreLogsSnapshot_ZeroDays(t *testing.T) {
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

// TestSyncManager_RestoreLogsSnapshot_RefreshesWindow 验证快照恢复会刷新窗口内数据。
func TestSyncManager_RestoreLogsSnapshot_RefreshesWindow(t *testing.T) {
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
