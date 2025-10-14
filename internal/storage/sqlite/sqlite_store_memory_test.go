package sqlite

import (
	"ccLoad/internal/model"
	"context"
	"os"
	"testing"
	"time"
)

// TestMemoryDBMode 测试内存数据库模式
func TestMemoryDBMode(t *testing.T) {
	// ✅ P1-1 修复：内存模式测试需要 Redis，如果未配置则跳过
	if os.Getenv("REDIS_URL") == "" {
		t.Skip("⚠️  跳过内存数据库测试：需要配置 REDIS_URL 环境变量")
	}

	// 保存原环境变量
	oldValue := os.Getenv("CCLOAD_USE_MEMORY_DB")
	defer func() {
		if oldValue == "" {
			os.Unsetenv("CCLOAD_USE_MEMORY_DB")
		} else {
			os.Setenv("CCLOAD_USE_MEMORY_DB", oldValue)
		}
	}()

	// 启用内存模式
	os.Setenv("CCLOAD_USE_MEMORY_DB", "true")

	// 创建内存数据库实例
	store, err := NewSQLiteStore("/tmp/test-memory.db", nil)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// 测试1: 创建渠道配置
	config := &model.Config{
		Name:     "test-memory-channel",
		URL:      "https://api.example.com",
		Priority: 10,
		Models:   []string{"model-1", "model-2"},
		Enabled:  true,
	}

	created, err := store.CreateConfig(ctx, config)
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	if created.ID == 0 {
		t.Error("Expected non-zero ID for created config")
	}

	// 测试2: 查询渠道配置
	retrieved, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}

	if retrieved.Name != config.Name {
		t.Errorf("Expected name %s, got %s", config.Name, retrieved.Name)
	}

	// 测试3: 列出所有渠道
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}

	if len(configs) != 1 {
		t.Errorf("Expected 1 config, got %d", len(configs))
	}

	// 测试4: 冷却机制（内存模式下应正常工作）
	until := time.Now().Add(5 * time.Second)
	err = store.SetChannelCooldown(ctx, created.ID, until)
	if err != nil {
		t.Fatalf("SetChannelCooldown failed: %v", err)
	}

	cooldownUntil, exists := getChannelCooldownUntil(ctx, store, created.ID)
	if !exists {
		t.Error("Expected cooldown to exist")
	}

	if cooldownUntil.Unix() != until.Unix() {
		t.Errorf("Expected cooldown until %v, got %v", until.Unix(), cooldownUntil.Unix())
	}

	// 测试5: 删除渠道
	err = store.DeleteConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("DeleteConfig failed: %v", err)
	}

	// 验证删除后查询失败
	_, err = store.GetConfig(ctx, created.ID)
	if err == nil {
		t.Error("Expected GetConfig to fail after deletion")
	}
}

// TestFileDBMode 测试文件数据库模式（默认行为）
func TestFileDBMode(t *testing.T) {
	// 保存原环境变量
	oldValue := os.Getenv("CCLOAD_USE_MEMORY_DB")
	defer func() {
		if oldValue == "" {
			os.Unsetenv("CCLOAD_USE_MEMORY_DB")
		} else {
			os.Setenv("CCLOAD_USE_MEMORY_DB", oldValue)
		}
	}()

	// 禁用内存模式（使用默认文件模式）
	os.Setenv("CCLOAD_USE_MEMORY_DB", "false")

	dbPath := "/tmp/test-file-mode.db"
	logDBPath := "/tmp/test-file-mode-log.db"
	defer func() {
		os.Remove(dbPath)
		os.Remove(logDBPath)
	}()

	// 创建文件数据库实例
	store, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}

	ctx := context.Background()

	// 创建测试数据
	config := &model.Config{
		Name:     "test-file-channel",
		URL:      "https://api.example.com",
		Priority: 5,
		Models:   []string{"model-a"},
		Enabled:  true,
	}

	created, err := store.CreateConfig(ctx, config)
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	// 验证数据库文件是否创建
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("Expected database file to exist")
	}

	if _, err := os.Stat(logDBPath); os.IsNotExist(err) {
		t.Error("Expected log database file to exist")
	}

	// 关闭数据库（移除defer，手动关闭）
	store.Close()

	// 重新打开验证持久化
	store2, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer store2.Close()

	retrieved, err := store2.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig after reopen failed: %v", err)
	}

	if retrieved.Name != config.Name {
		t.Errorf("Expected name %s after reopen, got %s", config.Name, retrieved.Name)
	}
}

// TestLogDBAlwaysUsesFile 测试日志库始终使用文件模式
func TestLogDBAlwaysUsesFile(t *testing.T) {
	// ✅ P1-1 修复：内存模式测试需要 Redis，如果未配置则跳过
	if os.Getenv("REDIS_URL") == "" {
		t.Skip("⚠️  跳过内存数据库测试：需要配置 REDIS_URL 环境变量")
	}

	// 启用内存模式
	os.Setenv("CCLOAD_USE_MEMORY_DB", "true")
	defer os.Unsetenv("CCLOAD_USE_MEMORY_DB")

	dbPath := "/tmp/test-log-persistence.db"
	logDBPath := "/tmp/test-log-persistence-log.db"
	defer func() {
		os.Remove(dbPath) // 内存模式不会创建主库文件
		os.Remove(logDBPath)
	}()

	store, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}

	ctx := context.Background()

	// 添加日志记录
	logEntry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		Model:      "test-model",
		StatusCode: 200,
		Message:    "Test log entry",
	}

	err = store.AddLog(ctx, logEntry)
	if err != nil {
		t.Fatalf("AddLog failed: %v", err)
	}

	// 等待异步日志写入
	time.Sleep(2 * time.Second)

	// 关闭数据库
	store.Close()

	// 验证日志库文件存在（即使主库使用内存模式）
	if _, err := os.Stat(logDBPath); os.IsNotExist(err) {
		t.Error("Expected log database file to exist even in memory mode")
	}

	// 重新打开验证日志持久化
	store2, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer store2.Close()

	logs, err := store2.ListLogs(ctx, time.Now().Add(-1*time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("ListLogs failed: %v", err)
	}

	if len(logs) == 0 {
		t.Error("Expected log entries to persist in file mode")
	}

	foundTestLog := false
	for _, log := range logs {
		if log.Model == "test-model" && log.Message == "Test log entry" {
			foundTestLog = true
			break
		}
	}

	if !foundTestLog {
		t.Error("Expected to find test log entry after database reopen")
	}
}

// 注：TestGenerateLogDBPath 和 TestMemoryDBDSNNoWAL 已移至 internal/storage/sqlite/db_config_test.go
// 原因：这些测试访问私有函数，应作为白盒测试放在包内部

// BenchmarkMemoryDBQuery 内存数据库查询性能基准测试
func BenchmarkMemoryDBQuery(b *testing.B) {
	os.Setenv("CCLOAD_USE_MEMORY_DB", "true")
	defer os.Unsetenv("CCLOAD_USE_MEMORY_DB")

	store, _ := NewSQLiteStore("/tmp/benchmark-memory.db", nil)
	defer store.Close()

	ctx := context.Background()

	// 创建测试数据
	config := &model.Config{
		Name:     "benchmark-channel",
		URL:      "https://api.example.com",
		Priority: 10,
		Models:   []string{"model-1"},
		Enabled:  true,
	}
	created, _ := store.CreateConfig(ctx, config)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = store.GetConfig(ctx, created.ID)
	}
}

// BenchmarkFileDBQuery 文件数据库查询性能基准测试
func BenchmarkFileDBQuery(b *testing.B) {
	os.Setenv("CCLOAD_USE_MEMORY_DB", "false")
	defer os.Unsetenv("CCLOAD_USE_MEMORY_DB")

	dbPath := "/tmp/benchmark-file.db"
	defer os.Remove(dbPath)
	defer os.Remove("/tmp/benchmark-file-log.db")

	store, _ := NewSQLiteStore(dbPath, nil)
	defer store.Close()

	ctx := context.Background()

	// 创建测试数据
	config := &model.Config{
		Name:     "benchmark-channel",
		URL:      "https://api.example.com",
		Priority: 10,
		Models:   []string{"model-1"},
		Enabled:  true,
	}
	created, _ := store.CreateConfig(ctx, config)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = store.GetConfig(ctx, created.ID)
	}
}
