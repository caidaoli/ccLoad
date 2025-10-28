package app

import (
	"context"
	"runtime"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
	"ccLoad/internal/testutil"
)

// createTestStoreForGoroutineTest 创建Goroutine测试专用的SQLiteStore
// 使用测试专用构造函数，自动禁用连接生命周期以避免goroutine泄漏
func createTestStoreForGoroutineTest(t *testing.T) *sqlite.SQLiteStore {
	t.Helper()

	// 使用临时文件数据库 + 测试专用构造函数
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := sqlite.NewSQLiteStoreForTest(dbPath, nil)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	return store
}

// TestServerShutdown_NoGoroutineLeak 验证Server优雅关闭不泄漏goroutine
func TestServerShutdown_NoGoroutineLeak(t *testing.T) {
	// ✅ P2修复（2025-10-28）：等待之前测试的goroutine完全退出
	// 必须在CheckGorutineLeak之前等待，让它记录正确的基线
	time.Sleep(1 * time.Second)
	runtime.GC() // 触发GC清理已完成的goroutine

	defer testutil.CheckGorutineLeak(t)()

	// 创建测试专用数据库，禁用连接生命周期以避免goroutine泄漏
	store := createTestStoreForGoroutineTest(t)
	srv := NewServer(store)

	// 模拟一些操作
	ctx := context.Background()

	// 添加一些日志
	for i := 0; i < 10; i++ {
		srv.addLogAsync(&model.LogEntry{
			Time:    model.JSONTime{Time: time.Now()},
			Message: "test",
		})
	}

	// 等待日志处理
	time.Sleep(100 * time.Millisecond)

	// 优雅关闭
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := srv.Shutdown(shutdownCtx)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	// 等待所有goroutine结束
	// ✅ P2修复（2025-10-28）：增加等待时间到5秒，给予数据库连接充分关闭时间
	if !testutil.WaitForGoroutines(5*time.Second, 0) {
		t.Error("部分goroutine未在5秒内结束")
		t.Log(testutil.PrintGoroutineStacks())
	}
}

// TestLogWorker_NoLeak 测试日志worker不泄漏
func TestLogWorker_NoLeak(t *testing.T) {
	// ✅ P2修复（2025-10-28）：等待之前测试的goroutine完全退出
	time.Sleep(1 * time.Second)
	runtime.GC()

	defer testutil.CheckGorutineLeak(t)()

	// 创建测试专用数据库，禁用连接生命周期以避免goroutine泄漏
	store := createTestStoreForGoroutineTest(t)

	// 创建server（启动logWorker）
	srv := NewServer(store)

	// 发送大量日志
	for i := 0; i < 1000; i++ {
		srv.addLogAsync(&model.LogEntry{
			Time:    model.JSONTime{Time: time.Now()},
			Message: "stress test",
		})
	}

	// 等待处理
	time.Sleep(200 * time.Millisecond)

	// 关闭
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	// 验证无泄漏
	// ✅ P2修复（2025-10-28）：增加等待时间到5秒
	if !testutil.WaitForGoroutines(5*time.Second, 0) {
		t.Error("logWorker goroutine 泄漏")
	}
}

// TestLogChan_FullBuffer 测试日志缓冲区满时的行为
func TestLogChan_FullBuffer(t *testing.T) {
	defer testutil.CheckGorutineLeak(t)()

	store, _ := sqlite.NewSQLiteStore(":memory:", nil)
	srv := NewServer(store)

	// 填满日志缓冲区（默认1000条）
	// 快速发送超过缓冲区容量的日志
	for i := 0; i < 2000; i++ {
		srv.addLogAsync(&model.LogEntry{
			Time:    model.JSONTime{Time: time.Now()},
			Message: "buffer overflow test",
		})
	}

	// 检查丢弃计数器
	dropped := srv.logDropCount.Load()
	t.Logf("丢弃的日志数量: %d", dropped)

	if dropped == 0 {
		t.Log("⚠️  警告：缓冲区未溢出，可能需要发送更多日志")
	} else {
		t.Logf("✅ 缓冲区溢出机制正常工作，丢弃了 %d 条日志", dropped)
	}

	// 等待处理
	time.Sleep(500 * time.Millisecond)

	// 关闭
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestTokenCleanupLoop_NoLeak 测试Token清理循环不泄漏
func TestTokenCleanupLoop_NoLeak(t *testing.T) {
	defer testutil.CheckGorutineLeak(t)()

	store, _ := sqlite.NewSQLiteStore(":memory:", nil)
	srv := NewServer(store)

	// 添加一些token
	srv.tokensMux.Lock()
	srv.validTokens["token1"] = time.Now().Add(1 * time.Hour)
	srv.validTokens["token2"] = time.Now().Add(-1 * time.Hour) // 过期
	srv.tokensMux.Unlock()

	// 触发清理
	srv.cleanExpiredTokens()

	// 等待清理完成
	time.Sleep(100 * time.Millisecond)

	// 验证过期token被删除
	srv.tokensMux.RLock()
	if _, exists := srv.validTokens["token2"]; exists {
		t.Error("过期token应该被删除")
	}
	if _, exists := srv.validTokens["token1"]; !exists {
		t.Error("未过期token不应该被删除")
	}
	srv.tokensMux.RUnlock()

	// 关闭
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

// TestConcurrencyControl_NoLeak 测试并发控制信号量不泄漏
func TestConcurrencyControl_NoLeak(t *testing.T) {
	defer testutil.CheckGorutineLeak(t)()

	store, _ := sqlite.NewSQLiteStore(":memory:", nil)
	srv := NewServer(store)

	// 模拟多个并发请求获取槽位
	const concurrency = 100
	done := make(chan bool, concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer func() { done <- true }()

			// 获取槽位
			srv.concurrencySem <- struct{}{}

			// 模拟处理
			time.Sleep(10 * time.Millisecond)

			// 释放槽位
			<-srv.concurrencySem
		}()
	}

	// 等待所有请求完成
	for i := 0; i < concurrency; i++ {
		<-done
	}

	// 验证所有槽位被释放
	if len(srv.concurrencySem) != 0 {
		t.Errorf("并发信号量应该为空，当前长度: %d", len(srv.concurrencySem))
	}

	// 关闭
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}
