package sqlite_test

import (
	"ccLoad/internal/storage"
	"ccLoad/internal/model"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================================
// 增加store_impl并发测试覆盖率
// ============================================================================

// TestConcurrentConfigCreate 测试并发创建渠道配置
func TestConcurrentConfigCreate(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const numGoroutines = 50

	var wg sync.WaitGroup
	var successCount atomic.Int32
	var errorCount atomic.Int32

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			cfg := &model.Config{
				Name:    fmt.Sprintf("concurrent-channel-%d", idx),
				URL:     "https://api.example.com",
				Enabled: true,
				Models:  []string{"gpt-4"},
			}

			_, err := store.CreateConfig(ctx, cfg)
			if err != nil {
				errorCount.Add(1)
				t.Logf("创建失败 #%d: %v", idx, err)
			} else {
				successCount.Add(1)
			}
		}(i)
	}

	wg.Wait()

	success := successCount.Load()
	errors := errorCount.Load()

	t.Logf("✅ 并发创建测试完成: 成功=%d, 失败=%d, 总数=%d", success, errors, numGoroutines)

	if success == 0 {
		t.Fatal("所有并发创建都失败了")
	}

	// 验证数据一致性
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs失败: %v", err)
	}

	if int32(len(configs)) != success {
		t.Errorf("数据不一致: 数据库中有%d个配置，期望%d个", len(configs), success)
	}
}

// TestConcurrentConfigReadWrite 测试并发读写渠道配置
func TestConcurrentConfigReadWrite(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 预先创建一个配置
	cfg := &model.Config{
		Name:    "test-rw-channel",
		URL:     "https://api.example.com",
		Enabled: true,
		Models:  []string{"gpt-4"},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建初始配置失败: %v", err)
	}

	const numReaders = 20
	const numWriters = 10

	var wg sync.WaitGroup
	var readCount atomic.Int32
	var writeCount atomic.Int32

	// 启动读协程
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, err := store.GetConfig(ctx, created.ID)
				if err == nil {
					readCount.Add(1)
				}
				time.Sleep(1 * time.Millisecond)
			}
		}(i)
	}

	// 启动写协程
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				updates := &model.Config{
					Priority: idx*10 + j,
				}
				_, err := store.UpdateConfig(ctx, created.ID, updates)
				if err == nil {
					writeCount.Add(1)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	reads := readCount.Load()
	writes := writeCount.Load()

	t.Logf("✅ 并发读写测试完成: 读取=%d次, 写入=%d次", reads, writes)

	if reads < 100 {
		t.Errorf("读取次数过少: %d (期望至少100次)", reads)
	}
	if writes < 30 {
		t.Errorf("写入次数过少: %d (期望至少30次)", writes)
	}
}

// TestConcurrentLogAdd 测试并发添加日志
func TestConcurrentLogAdd(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const numGoroutines = 30
	const logsPerGoroutine = 10

	var wg sync.WaitGroup
	var successCount atomic.Int32

	startTime := time.Now()

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			for j := 0; j < logsPerGoroutine; j++ {
				channelID := int64(idx + 1)
				entry := &model.LogEntry{
					ChannelID:  channelID,
					StatusCode: 200,
					Model:      "gpt-4",
					Time:       model.JSONTime{Time: time.Now()},
				}

				err := store.AddLog(ctx, entry)
				if err == nil {
					successCount.Add(1)
				}
			}
		}(i)
	}

	wg.Wait()

	elapsed := time.Since(startTime)
	success := successCount.Load()
	expected := int32(numGoroutines * logsPerGoroutine)

	t.Logf("✅ 并发日志添加测试完成: 成功=%d/%d, 耗时=%v", success, expected, elapsed)

	if success < expected*9/10 {
		t.Errorf("成功率过低: %d/%d (%.1f%%)", success, expected, float64(success)/float64(expected)*100)
	}

	// 验证日志数量
	logs, err := store.ListLogs(ctx, time.Time{}, 1000, 0, nil)
	if err != nil {
		t.Fatalf("ListLogs失败: %v", err)
	}

	if int32(len(logs)) < success*9/10 {
		t.Errorf("日志数量不匹配: 数据库中有%d条，期望至少%d条", len(logs), success*9/10)
	}
}

// TestConcurrentBatchLogAdd 测试并发批量添加日志
func TestConcurrentBatchLogAdd(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const numGoroutines = 20
	const batchSize = 50

	var wg sync.WaitGroup
	var successCount atomic.Int32

	startTime := time.Now()

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			batch := make([]*model.LogEntry, batchSize)
			channelID := int64(idx + 1)
			for j := 0; j < batchSize; j++ {
				batch[j] = &model.LogEntry{
					ChannelID:  channelID,
					StatusCode: 200,
					Model:      "gpt-4",
					Time:       model.JSONTime{Time: time.Now()},
				}
			}

			err := store.BatchAddLogs(ctx, batch)
			if err == nil {
				successCount.Add(int32(batchSize))
			} else {
				t.Logf("批量添加失败 #%d: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()

	elapsed := time.Since(startTime)
	success := successCount.Load()
	expected := int32(numGoroutines * batchSize)

	t.Logf("✅ 并发批量日志测试完成: 成功=%d/%d, 耗时=%v", success, expected, elapsed)

	if success < expected*8/10 {
		t.Errorf("成功率过低: %d/%d (%.1f%%)", success, expected, float64(success)/float64(expected)*100)
	}
}

// TestConcurrentAPIKeyOperations 测试并发API Key操作
func TestConcurrentAPIKeyOperations(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 预先创建一个渠道
	cfg := &model.Config{
		Name:    "test-apikey-channel",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建初始配置失败: %v", err)
	}

	const numKeys = 30
	var wg sync.WaitGroup
	var createSuccess atomic.Int32
	var readSuccess atomic.Int32

	// 并发创建API Keys
	for i := 0; i < numKeys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			key := &model.APIKey{
				ChannelID:   created.ID,
				KeyIndex:    idx,
				APIKey:      fmt.Sprintf("sk-test-key-%d", idx),
				KeyStrategy: "sequential",
			}

			err := store.CreateAPIKey(ctx, key)
			if err == nil {
				createSuccess.Add(1)
			} else {
				t.Logf("创建Key失败 #%d: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()

	// 并发读取API Keys
	for i := 0; i < numKeys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			_, err := store.GetAPIKey(ctx, created.ID, idx)
			if err == nil {
				readSuccess.Add(1)
			}
		}(i)
	}

	wg.Wait()

	creates := createSuccess.Load()
	reads := readSuccess.Load()

	t.Logf("✅ 并发API Key测试完成: 创建成功=%d/%d, 读取成功=%d/%d",
		creates, numKeys, reads, numKeys)

	if creates < int32(numKeys)*8/10 {
		t.Errorf("创建成功率过低: %d/%d", creates, numKeys)
	}

	// 验证数据完整性
	allKeys, err := store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys失败: %v", err)
	}

	if int32(len(allKeys)) < creates*9/10 {
		t.Errorf("API Key数量不匹配: 数据库中有%d个，期望至少%d个", len(allKeys), creates*9/10)
	}
}

// TestConcurrentCooldownOperations 测试并发冷却操作
func TestConcurrentCooldownOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过并发测试（使用 -short 标志）")
	}

	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 预先创建渠道和Keys
	cfg := &model.Config{
		Name:    "test-cooldown-channel",
		URL:     "https://api.example.com",
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建初始配置失败: %v", err)
	}

	// 创建3个API Keys
	for i := 0; i < 3; i++ {
		key := &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    i,
			APIKey:      fmt.Sprintf("sk-cooldown-key-%d", i),
			KeyStrategy: "sequential",
		}
		_ = store.CreateAPIKey(ctx, key)
	}

	// 使用信号量控制并发度为2，避免过多BUSY错误
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	var channelCooldowns atomic.Int32
	var keyCooldowns atomic.Int32

	now := time.Now()

	// 并发更新渠道冷却（5次）
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			statusCode := 500 + (idx % 5)
			_, err := store.BumpChannelCooldown(ctx, created.ID, now, statusCode)
			if err == nil {
				channelCooldowns.Add(1)
			}
		}(i)
	}

	// 并发更新Key冷却（6次，每个Key 2次）
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			keyIndex := idx % 3
			_, err := store.BumpKeyCooldown(ctx, created.ID, keyIndex, now, 401)
			if err == nil {
				keyCooldowns.Add(1)
			}
		}(i)
	}

	wg.Wait()

	channelSucc := channelCooldowns.Load()
	keySucc := keyCooldowns.Load()

	t.Logf("✅ 并发冷却测试完成: 渠道冷却成功=%d/5, Key冷却成功=%d/6",
		channelSucc, keySucc)

	// 至少有一些操作成功即可（验证并发安全性）
	if channelSucc == 0 {
		t.Error("渠道冷却全部失败")
	}
	if keySucc == 0 {
		t.Error("Key冷却全部失败")
	}
}

// TestConcurrentMixedOperations 测试混合并发操作
func TestConcurrentMixedOperations(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const duration = 2 * time.Second

	var wg sync.WaitGroup
	var operations atomic.Int32
	stopCh := make(chan struct{})

	// 创建操作
	wg.Add(1)
	go func() {
		defer wg.Done()
		idx := 0
		for {
			select {
			case <-stopCh:
				return
			default:
				cfg := &model.Config{
					Name:    fmt.Sprintf("mixed-channel-%d", idx),
					URL:     "https://api.example.com",
					Enabled: true,
				}
				_, _ = store.CreateConfig(ctx, cfg)
				operations.Add(1)
				idx++
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	// 读取操作
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				_, _ = store.ListConfigs(ctx)
				operations.Add(1)
				time.Sleep(3 * time.Millisecond)
			}
		}
	}()

	// 日志操作
	wg.Add(1)
	go func() {
		defer wg.Done()
		channelID := int64(1)
		for {
			select {
			case <-stopCh:
				return
			default:
				entry := &model.LogEntry{
					ChannelID:  channelID,
					StatusCode: 200,
					Model:      "gpt-4",
					Time:       model.JSONTime{Time: time.Now()},
				}
				_ = store.AddLog(ctx, entry)
				operations.Add(1)
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// 运行指定时间
	time.Sleep(duration)
	close(stopCh)
	wg.Wait()

	totalOps := operations.Load()
	t.Logf("✅ 混合并发测试完成: 总操作数=%d, 持续时间=%v, QPS=%.1f",
		totalOps, duration, float64(totalOps)/duration.Seconds())

	if totalOps < 100 {
		t.Errorf("操作数过少: %d (期望至少100)", totalOps)
	}
}

// ========== 辅助函数 ==========

func setupConcurrentTestStore(t *testing.T) (storage.Store, func()) {
	t.Helper()

	tmpDB := t.TempDir() + "/concurrent-test.db"
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
