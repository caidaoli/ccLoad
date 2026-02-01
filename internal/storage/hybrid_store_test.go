package storage

import (
	"context"
	"fmt"
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
	return store.(*sqlstore.SQLStore)
}

func TestHybridStore_BasicOperations(t *testing.T) {
	// 创建两个独立的 SQLite：一个模拟 MySQL（主存储），一个作为 SQLite 缓存
	mysql := createTestSQLiteStore(t)  // 用 SQLite 模拟 MySQL（主存储）
	sqlite := createTestSQLiteStore(t) // SQLite 缓存
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	// 测试 CreateConfig - 应该先写 MySQL，再同步到 SQLite
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

	// 验证 MySQL（主存储）有数据
	mysqlCfg, err := mysql.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("MySQL 主存储应该有数据: %v", err)
	}
	if mysqlCfg.Name != cfg.Name {
		t.Errorf("MySQL 数据不匹配: got %s, want %s", mysqlCfg.Name, cfg.Name)
	}

	// 测试 GetConfig（从 SQLite 缓存读取）
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

	// 验证 MySQL 主存储已更新
	mysqlCfg, err = mysql.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("MySQL GetConfig 失败: %v", err)
	}
	if mysqlCfg.Name != "updated-channel" {
		t.Errorf("MySQL 数据未更新: got %s, want updated-channel", mysqlCfg.Name)
	}

	// 测试 DeleteConfig
	err = hybrid.DeleteConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("DeleteConfig 失败: %v", err)
	}

	// 验证 MySQL 主存储已删除
	_, err = mysql.GetConfig(ctx, created.ID)
	if err == nil {
		t.Error("删除后 MySQL 应该返回错误")
	}

	// 验证 SQLite 缓存也已清理
	_, err = hybrid.GetConfig(ctx, created.ID)
	if err == nil {
		t.Error("删除后 SQLite 缓存应该返回错误")
	}
}

func TestHybridStore_AuthToken_IDFromMySQL(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	token := &model.AuthToken{
		Token:       fmt.Sprintf("token-%d", time.Now().UnixNano()),
		Description: "test",
		IsActive:    true,
	}
	if err := hybrid.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("CreateAuthToken 失败: %v", err)
	}
	if token.ID == 0 {
		t.Fatalf("token.ID 不应为 0")
	}

	// ID 来自 MySQL 主存储
	mysqlToken, err := mysql.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("MySQL GetAuthToken 失败: %v", err)
	}
	if mysqlToken.Token != token.Token {
		t.Fatalf("MySQL token 不匹配")
	}

	// SQLite 缓存也应该有相同数据
	sqliteToken, err := sqlite.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("SQLite GetAuthToken 失败: %v", err)
	}
	if sqliteToken.ID != token.ID {
		t.Fatalf("SQLite token ID 不匹配: got %d, want %d", sqliteToken.ID, token.ID)
	}
}

func TestHybridStore_ImportChannelBatch(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	channels := []*model.ChannelWithKeys{
		{
			Config: &model.Config{
				Name:        "import-chan",
				ChannelType: "codex",
				URL:         "https://example.com",
				Priority:    10,
				Enabled:     true,
				ModelEntries: []model.ModelEntry{
					{Model: "gpt-4.1"},
				},
			},
			APIKeys: []model.APIKey{
				{KeyIndex: 0, APIKey: "sk-test-0", KeyStrategy: model.KeyStrategySequential},
				{KeyIndex: 1, APIKey: "sk-test-1", KeyStrategy: model.KeyStrategySequential},
			},
		},
	}

	created, updated, err := hybrid.ImportChannelBatch(ctx, channels)
	if err != nil {
		t.Fatalf("ImportChannelBatch 失败: %v", err)
	}
	if created != 1 || updated != 0 {
		t.Fatalf("ImportChannelBatch 计数不符合预期: created=%d updated=%d", created, updated)
	}
	if channels[0].Config.ID == 0 {
		t.Fatalf("导入后 channels[0].Config.ID 不应为 0")
	}
	id := channels[0].Config.ID

	// MySQL 主存储应该有数据
	mysqlCfg, err := mysql.GetConfig(ctx, id)
	if err != nil {
		t.Fatalf("MySQL GetConfig 失败: %v", err)
	}
	if mysqlCfg.Name != "import-chan" {
		t.Fatalf("MySQL 渠道名称不匹配: got %s, want %s", mysqlCfg.Name, "import-chan")
	}

	// SQLite 缓存也应该有数据
	sqliteCfg, err := sqlite.GetConfig(ctx, id)
	if err != nil {
		t.Fatalf("SQLite GetConfig 失败: %v", err)
	}
	if sqliteCfg.Name != "import-chan" {
		t.Fatalf("SQLite 渠道名称不匹配: got %s, want %s", sqliteCfg.Name, "import-chan")
	}

	// 验证 API Keys
	keys, err := mysql.GetAPIKeys(ctx, id)
	if err != nil {
		t.Fatalf("MySQL GetAPIKeys 失败: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("MySQL API Keys 数量不匹配: got %d, want %d", len(keys), 2)
	}
}

func TestHybridStore_LogsAsync_ClonesInputs(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	// logs 写 SQLite + 异步同步到 MySQL
	// AddLog 返回后修改入参对象，不应与后台同步产生数据竞争
	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		ChannelID:  1,
		Model:      "gpt-4",
		StatusCode: 200,
		Duration:   1.5,
	}
	if err := hybrid.AddLog(ctx, entry); err != nil {
		t.Fatalf("AddLog 失败: %v", err)
	}

	// 并发修改入参（测试克隆是否正确）
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				entry.Message = fmt.Sprintf("m-%d", time.Now().UnixNano())
				entry.Duration += 0.001
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	close(stop)

	// 验证 SQLite 有数据
	logs, err := hybrid.ListLogs(ctx, time.Now().Add(-1*time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("ListLogs 失败: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("ListLogs 返回数量不匹配: got %d, want 1", len(logs))
	}
}

func TestHybridStore_SyncQueueLen(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
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
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

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

	// 验证 SQLite 有数据（日志先写 SQLite）
	logs, err := hybrid.ListLogs(ctx, time.Now().Add(-1*time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("ListLogs 失败: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("ListLogs 返回数量不匹配: got %d, want 1", len(logs))
	}

	// 等待异步同步到 MySQL（条件等待，避免固定 sleep 造成漂移/假绿）
	deadline := time.Now().Add(2 * time.Second)
	for {
		mysqlLogs, err := mysql.ListLogs(ctx, time.Now().Add(-1*time.Hour), 10, 0, nil)
		if err != nil {
			t.Fatalf("MySQL ListLogs 失败: %v", err)
		}
		if len(mysqlLogs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 MySQL 异步同步超时：got %d logs, want 1", len(mysqlLogs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHybridStore_GracefulClose(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)

	hybrid := NewHybridStore(sqlite, mysql)

	ctx := context.Background()

	// 添加一些日志触发异步同步任务
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

func TestHybridStore_SQLiteCacheFailureDoesNotBlockWrite(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)

	ctx := context.Background()

	// 创建一个配置
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

	// 关闭 SQLite（模拟缓存失败）
	_ = sqlite.Close()

	// 更新操作应该成功（MySQL 写入成功即可）
	cfg.Name = "updated-channel"
	_, err = hybrid.UpdateConfig(ctx, created.ID, cfg)
	if err != nil {
		t.Fatalf("UpdateConfig 应该成功（MySQL 是主存储）: %v", err)
	}

	// 验证 MySQL 有更新
	mysqlCfg, err := mysql.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("MySQL GetConfig 失败: %v", err)
	}
	if mysqlCfg.Name != "updated-channel" {
		t.Errorf("MySQL 数据未更新: got %s, want updated-channel", mysqlCfg.Name)
	}

	_ = hybrid.Close()
}
