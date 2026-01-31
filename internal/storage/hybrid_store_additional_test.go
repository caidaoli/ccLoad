package storage

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestHybridStore_WrapperCoverage(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	h := NewHybridStore(sqlite, mysql)
	defer func() { _ = h.Close() }()

	ctx := context.Background()

	// === Channel Management wrappers ===
	c1, err := h.CreateConfig(ctx, &model.Config{
		Name:        "c1",
		ChannelType: "openai",
		URL:         "https://example.com",
		Priority:    10,
		Enabled:     true,
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig c1 failed: %v", err)
	}
	c2, err := h.CreateConfig(ctx, &model.Config{
		Name:        "c2",
		ChannelType: "anthropic",
		URL:         "https://example.com",
		Priority:    20,
		Enabled:     false,
		ModelEntries: []model.ModelEntry{
			{Model: "claude-3-5-sonnet-latest"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig c2 failed: %v", err)
	}

	byType, err := h.GetEnabledChannelsByType(ctx, "openai")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByType failed: %v", err)
	}
	if len(byType) != 1 || byType[0].ID != c1.ID {
		t.Fatalf("GetEnabledChannelsByType got %#v, want only c1", byType)
	}

	byModel, err := h.GetEnabledChannelsByModel(ctx, "gpt-4o")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel failed: %v", err)
	}
	if len(byModel) != 1 || byModel[0].ID != c1.ID {
		t.Fatalf("GetEnabledChannelsByModel got %#v, want only c1", byModel)
	}

	affected, err := h.BatchUpdatePriority(ctx, []struct {
		ID       int64
		Priority int
	}{
		{ID: c1.ID, Priority: 101},
		{ID: c2.ID, Priority: 202},
	})
	if err != nil {
		t.Fatalf("BatchUpdatePriority failed: %v", err)
	}
	if affected != 2 {
		t.Fatalf("BatchUpdatePriority affected=%d, want 2", affected)
	}

	// === API Key Management wrappers ===
	if err := h.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: c1.ID, KeyIndex: 0, APIKey: "k0", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: c1.ID, KeyIndex: 1, APIKey: "k1", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	keys, err := h.GetAPIKeys(ctx, c1.ID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("GetAPIKeys got len=%d err=%v, want 2,nil", len(keys), err)
	}
	if _, err := h.GetAPIKey(ctx, c1.ID, 0); err != nil {
		t.Fatalf("GetAPIKey failed: %v", err)
	}
	allKeys, err := h.GetAllAPIKeys(ctx)
	if err != nil {
		t.Fatalf("GetAllAPIKeys failed: %v", err)
	}
	if len(allKeys[c1.ID]) != 2 {
		t.Fatalf("GetAllAPIKeys for c1 got %d, want 2", len(allKeys[c1.ID]))
	}

	if err := h.UpdateAPIKeysStrategy(ctx, c1.ID, model.KeyStrategyRoundRobin); err != nil {
		t.Fatalf("UpdateAPIKeysStrategy failed: %v", err)
	}

	if err := h.DeleteAPIKey(ctx, c1.ID, 0); err != nil {
		t.Fatalf("DeleteAPIKey failed: %v", err)
	}
	if err := h.CompactKeyIndices(ctx, c1.ID, 0); err != nil {
		t.Fatalf("CompactKeyIndices failed: %v", err)
	}
	if err := h.DeleteAllAPIKeys(ctx, c1.ID); err != nil {
		t.Fatalf("DeleteAllAPIKeys failed: %v", err)
	}

	// === Cooldown Management wrappers ===
	until := time.Now().Add(2 * time.Minute)
	if err := h.SetChannelCooldown(ctx, c1.ID, until); err != nil {
		t.Fatalf("SetChannelCooldown failed: %v", err)
	}
	if _, err := h.GetAllChannelCooldowns(ctx); err != nil {
		t.Fatalf("GetAllChannelCooldowns failed: %v", err)
	}
	if _, err := h.BumpChannelCooldown(ctx, c1.ID, time.Now(), 500); err != nil {
		t.Fatalf("BumpChannelCooldown failed: %v", err)
	}
	if err := h.ResetChannelCooldown(ctx, c1.ID); err != nil {
		t.Fatalf("ResetChannelCooldown failed: %v", err)
	}

	// key cooldown 需要 key 存在
	if err := h.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: c1.ID, KeyIndex: 0, APIKey: "k0", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch (for cooldown) failed: %v", err)
	}
	if err := h.SetKeyCooldown(ctx, c1.ID, 0, until); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}
	if _, err := h.GetAllKeyCooldowns(ctx); err != nil {
		t.Fatalf("GetAllKeyCooldowns failed: %v", err)
	}
	if _, err := h.BumpKeyCooldown(ctx, c1.ID, 0, time.Now(), 429); err != nil {
		t.Fatalf("BumpKeyCooldown failed: %v", err)
	}
	if err := h.ResetKeyCooldown(ctx, c1.ID, 0); err != nil {
		t.Fatalf("ResetKeyCooldown failed: %v", err)
	}

	// === Logs / Metrics / Stats wrappers ===
	now := time.Now()
	if err := h.BatchAddLogs(ctx, []*model.LogEntry{
		{Time: model.JSONTime{Time: now}, ChannelID: c1.ID, Model: "gpt-4o", StatusCode: 200, Duration: 0.1, Cost: 0.01},
		{Time: model.JSONTime{Time: now}, ChannelID: c1.ID, Model: "gpt-4o", StatusCode: 500, Duration: 0.2, Cost: 0.02},
	}); err != nil {
		t.Fatalf("BatchAddLogs failed: %v", err)
	}

	since := now.Add(-time.Hour)
	until2 := now.Add(time.Second)

	if _, err := h.ListLogsRange(ctx, since, until2, 10, 0, nil); err != nil {
		t.Fatalf("ListLogsRange failed: %v", err)
	}
	if _, err := h.CountLogs(ctx, since, nil); err != nil {
		t.Fatalf("CountLogs failed: %v", err)
	}
	if _, err := h.CountLogsRange(ctx, since, until2, nil); err != nil {
		t.Fatalf("CountLogsRange failed: %v", err)
	}
	if _, err := h.AggregateRangeWithFilter(ctx, since, until2, time.Minute, nil); err != nil {
		t.Fatalf("AggregateRangeWithFilter failed: %v", err)
	}
	if _, err := h.GetDistinctModels(ctx, since, until2, ""); err != nil {
		t.Fatalf("GetDistinctModels failed: %v", err)
	}
	if _, err := h.GetStats(ctx, since, until2, nil, true); err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}
	if _, err := h.GetStatsLite(ctx, since, until2, nil); err != nil {
		t.Fatalf("GetStatsLite failed: %v", err)
	}
	if _, err := h.GetRPMStats(ctx, since, until2, nil, true); err != nil {
		t.Fatalf("GetRPMStats failed: %v", err)
	}
	if _, err := h.GetChannelSuccessRates(ctx, since); err != nil {
		t.Fatalf("GetChannelSuccessRates failed: %v", err)
	}
	rows, err := h.GetHealthTimeline(ctx, "SELECT 1")
	if err != nil {
		t.Fatalf("GetHealthTimeline failed: %v", err)
	}
	_ = rows.Close()
	if _, err := h.GetTodayChannelCosts(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatalf("GetTodayChannelCosts failed: %v", err)
	}

	if err := h.CleanupLogsBefore(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CleanupLogsBefore failed: %v", err)
	}

	// === Auth Token wrappers ===
	tokenHash := model.HashToken("t1")
	at := &model.AuthToken{
		Token:       tokenHash,
		Description: "d1",
		IsActive:    true,
	}
	if err := h.CreateAuthToken(ctx, at); err != nil {
		t.Fatalf("CreateAuthToken failed: %v", err)
	}
	if _, err := h.GetAuthToken(ctx, at.ID); err != nil {
		t.Fatalf("GetAuthToken failed: %v", err)
	}
	if _, err := h.GetAuthTokenByValue(ctx, tokenHash); err != nil {
		t.Fatalf("GetAuthTokenByValue failed: %v", err)
	}
	if _, err := h.ListAuthTokens(ctx); err != nil {
		t.Fatalf("ListAuthTokens failed: %v", err)
	}
	if _, err := h.ListActiveAuthTokens(ctx); err != nil {
		t.Fatalf("ListActiveAuthTokens failed: %v", err)
	}
	at.Description = "d2"
	at.IsActive = false
	if err := h.UpdateAuthToken(ctx, at); err != nil {
		t.Fatalf("UpdateAuthToken failed: %v", err)
	}
	if err := h.UpdateTokenLastUsed(ctx, tokenHash, time.Now()); err != nil {
		t.Fatalf("UpdateTokenLastUsed failed: %v", err)
	}
	if err := h.UpdateTokenStats(ctx, tokenHash, true, 0.2, false, 0, 10, 20, 0, 0, 0.01); err != nil {
		t.Fatalf("UpdateTokenStats failed: %v", err)
	}
	// 仅覆盖转发逻辑：stats 由 SQLite 查询，RPM 计算也在 SQLite 层执行
	statsMap, err := h.GetAuthTokenStatsInRange(ctx, since, until2)
	if err != nil {
		t.Fatalf("GetAuthTokenStatsInRange failed: %v", err)
	}
	if err := h.FillAuthTokenRPMStats(ctx, statsMap, since, until2, false); err != nil {
		t.Fatalf("FillAuthTokenRPMStats failed: %v", err)
	}
	if err := h.DeleteAuthToken(ctx, at.ID); err != nil {
		t.Fatalf("DeleteAuthToken failed: %v", err)
	}

	// === System Settings wrappers ===
	if err := h.UpdateSetting(ctx, "log_retention_days", "9"); err != nil {
		t.Fatalf("UpdateSetting failed: %v", err)
	}
	if _, err := h.GetSetting(ctx, "log_retention_days"); err != nil {
		t.Fatalf("GetSetting failed: %v", err)
	}
	if _, err := h.ListAllSettings(ctx); err != nil {
		t.Fatalf("ListAllSettings failed: %v", err)
	}
	if err := h.BatchUpdateSettings(ctx, map[string]string{
		"log_retention_days": "7",
	}); err != nil {
		t.Fatalf("BatchUpdateSettings failed: %v", err)
	}

	// === Admin sessions wrappers (SQLite only) ===
	if err := h.CreateAdminSession(ctx, "adm", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateAdminSession failed: %v", err)
	}
	if _, exists, err := h.GetAdminSession(ctx, "adm"); err != nil || !exists {
		t.Fatalf("GetAdminSession exists=%v err=%v, want true,nil", exists, err)
	}
	if err := h.DeleteAdminSession(ctx, "adm"); err != nil {
		t.Fatalf("DeleteAdminSession failed: %v", err)
	}
	if err := h.CleanExpiredSessions(ctx); err != nil {
		t.Fatalf("CleanExpiredSessions failed: %v", err)
	}
	if _, err := h.LoadAllSessions(ctx); err != nil {
		t.Fatalf("LoadAllSessions failed: %v", err)
	}

	if err := h.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestHybridStore_AddLog_SyncsToMySQL(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	h := NewHybridStore(sqlite, mysql)
	defer func() { _ = h.Close() }()

	ctx := context.Background()
	now := time.Now()

	// 添加单条日志
	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: now},
		ChannelID:  1,
		Model:      "gpt-4o",
		StatusCode: 200,
		Duration:   0.5,
		Cost:       0.1,
	}
	if err := h.AddLog(ctx, entry); err != nil {
		t.Fatalf("AddLog failed: %v", err)
	}

	// 等待同步
	time.Sleep(100 * time.Millisecond)

	// 验证 MySQL 收到日志
	logs, err := mysql.ListLogs(ctx, now.Add(-time.Minute), 10, 0, nil)
	if err != nil {
		t.Fatalf("mysql.ListLogs failed: %v", err)
	}
	if len(logs) == 0 {
		t.Error("expected log to be synced to MySQL")
	}
}

func TestHybridStore_SyncQueueLen_Additional(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	h := NewHybridStore(sqlite, mysql)
	defer func() { _ = h.Close() }()

	// 初始队列应为空
	if h.SyncQueueLen() != 0 {
		t.Errorf("expected initial queue len 0, got %d", h.SyncQueueLen())
	}
}

func TestHybridStore_CloneLogEntry(t *testing.T) {
	// 测试 nil 情况
	if cloneLogEntryForSync(nil) != nil {
		t.Error("cloneLogEntryForSync(nil) should return nil")
	}

	// 测试正常克隆
	original := &model.LogEntry{
		ChannelID:  1,
		Model:      "gpt-4o",
		StatusCode: 200,
	}
	clone := cloneLogEntryForSync(original)
	if clone == original {
		t.Error("clone should be a different pointer")
	}
	if clone.Model != original.Model {
		t.Error("clone should have same Model")
	}
}

func TestHybridStore_CloneLogEntries(t *testing.T) {
	// 测试空切片
	if got := cloneLogEntriesForSync(nil); got != nil {
		t.Error("cloneLogEntriesForSync(nil) should return nil")
	}
	if got := cloneLogEntriesForSync([]*model.LogEntry{}); got != nil {
		t.Error("cloneLogEntriesForSync([]) should return nil")
	}

	// 测试正常克隆
	entries := []*model.LogEntry{
		{ChannelID: 1, Model: "a"},
		{ChannelID: 2, Model: "b"},
	}
	clones := cloneLogEntriesForSync(entries)
	if len(clones) != 2 {
		t.Fatalf("expected 2 clones, got %d", len(clones))
	}
	if len(clones) > 0 && len(entries) > 0 && clones[0] == entries[0] {
		t.Error("clones should be different pointers")
	}
}

func TestHybridStore_EnqueueLogSync_QueueFull(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	h := NewHybridStore(sqlite, mysql)
	// 先停止 worker，让队列积压
	h.stopOnce.Do(func() { close(h.stopCh) })
	h.syncWg.Wait()

	// 填满队列
	for range syncQueueSize {
		h.syncCh <- &syncTask{operation: "test", data: nil}
	}

	// 队列满时应该丢弃任务（不阻塞）
	h.enqueueLogSync(&syncTask{operation: "overflow", data: nil})

	// 验证队列长度仍然是 syncQueueSize
	if len(h.syncCh) != syncQueueSize {
		t.Fatalf("expected queue len %d, got %d", syncQueueSize, len(h.syncCh))
	}

	// 清空队列以便 Close
	for len(h.syncCh) > 0 {
		<-h.syncCh
	}
}

func TestHybridStore_DrainSyncQueue_EmptyQueue(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	h := NewHybridStore(sqlite, mysql)
	// 停止 worker
	h.stopOnce.Do(func() { close(h.stopCh) })
	h.syncWg.Wait()

	// 空队列时 drainSyncQueue 应该立即返回
	h.drainSyncQueue()

	// 队列应该仍然是空的
	if len(h.syncCh) != 0 {
		t.Fatalf("expected empty queue, got len %d", len(h.syncCh))
	}
}

func TestHybridStore_ExecuteSyncTask_UnknownOperation(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	h := NewHybridStore(sqlite, mysql)
	defer func() { _ = h.Close() }()

	// 未知操作应该被忽略（不 panic）
	h.executeSyncTask(&syncTask{operation: "unknown", data: nil})
}
