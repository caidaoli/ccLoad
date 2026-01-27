package storage

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

// createTestSQLiteStore 创建测试用的 SQLite store
func createTestSQLiteStore(t *testing.T) *sqlstore.SQLStore {
	t.Helper()
	tmpDB := t.TempDir() + "/hybrid_test.db"
	store, err := CreateSQLiteStore(tmpDB)
	if err != nil {
		t.Fatalf("创建测试 SQLite 失败: %v", err)
	}
	// 类型断言获取底层 SQLStore
	return store.(*sqlstore.SQLStore)
}

func TestHybridStore_BasicOperations(t *testing.T) {
	// 创建两个独立的 SQLite 作为 "SQLite 主存储" 和 "MySQL 备份存储"
	sqlite := createTestSQLiteStore(t)
	mysql := createTestSQLiteStore(t) // 用 SQLite 模拟 MySQL
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	// 测试 CreateConfig
	cfg := &model.Config{
		Name:        "test-channel",
		ChannelType: "openai",
		URL:         "https://api.openai.com",
		Priority:    100,
		Enabled:     true,
	}

	created, err := hybrid.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig 失败: %v", err)
	}
	if created.ID == 0 {
		t.Error("创建的配置 ID 不应为 0")
	}

	// 测试 GetConfig（应该从 SQLite 读取）
	got, err := hybrid.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig 失败: %v", err)
	}
	if got.Name != cfg.Name {
		t.Errorf("GetConfig 返回名称不匹配: got %s, want %s", got.Name, cfg.Name)
	}

	// 测试 ListConfigs
	list, err := hybrid.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("ListConfigs 返回数量不匹配: got %d, want 1", len(list))
	}

	// 测试 UpdateConfig
	cfg.Name = "updated-channel"
	updated, err := hybrid.UpdateConfig(ctx, created.ID, cfg)
	if err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}
	if updated.Name != "updated-channel" {
		t.Errorf("UpdateConfig 返回名称不匹配: got %s, want updated-channel", updated.Name)
	}

	// 等待异步同步完成
	time.Sleep(100 * time.Millisecond)

	// 验证 MySQL 备份存储也有数据（异步同步）
	mysqlCfg, err := mysql.GetConfig(ctx, created.ID)
	if err != nil {
		t.Logf("MySQL 同步可能还在进行中: %v", err)
	} else if mysqlCfg.Name != "updated-channel" {
		t.Errorf("MySQL 备份数据不匹配: got %s, want updated-channel", mysqlCfg.Name)
	}

	// 测试 DeleteConfig
	err = hybrid.DeleteConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("DeleteConfig 失败: %v", err)
	}

	// 验证删除成功
	_, err = hybrid.GetConfig(ctx, created.ID)
	if err == nil {
		t.Error("删除后 GetConfig 应该返回错误")
	}
}

func TestHybridStore_SyncQueueLen(t *testing.T) {
	sqlite := createTestSQLiteStore(t)
	mysql := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	// 初始队列应该为空
	if qLen := hybrid.SyncQueueLen(); qLen != 0 {
		t.Errorf("初始队列长度应为 0, got %d", qLen)
	}
}

func TestHybridStore_AddLog(t *testing.T) {
	sqlite := createTestSQLiteStore(t)
	mysql := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	// 添加日志
	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		ChannelID:  1,
		Model:      "gpt-4",
		StatusCode: 200,
		Duration:   1.5,
	}

	err := hybrid.AddLog(ctx, entry)
	if err != nil {
		t.Fatalf("AddLog 失败: %v", err)
	}

	// 验证 SQLite 有数据
	logs, err := hybrid.ListLogs(ctx, time.Now().Add(-1*time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("ListLogs 失败: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("ListLogs 返回数量不匹配: got %d, want 1", len(logs))
	}
}

func TestHybridStore_GracefulClose(t *testing.T) {
	sqlite := createTestSQLiteStore(t)
	mysql := createTestSQLiteStore(t)

	hybrid := NewHybridStore(sqlite, mysql)

	ctx := context.Background()

	// 添加一些数据触发同步任务
	for i := 0; i < 10; i++ {
		entry := &model.LogEntry{
			Time:       model.JSONTime{Time: time.Now()},
			ChannelID:  int64(i),
			Model:      "gpt-4",
			StatusCode: 200,
			Duration:   1.5,
		}
		_ = hybrid.AddLog(ctx, entry)
	}

	// 关闭应该等待同步任务完成
	err := hybrid.Close()
	if err != nil {
		t.Errorf("Close 失败: %v", err)
	}

	// 多次关闭应该是幂等的
	err = hybrid.Close()
	if err != nil {
		t.Errorf("第二次 Close 失败: %v", err)
	}
}
