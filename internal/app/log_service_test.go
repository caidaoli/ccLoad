package app

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

type retryTrackingStore struct {
	storage.Store
	attempts int
}

func (s *retryTrackingStore) BatchAddLogs(_ context.Context, _ []*model.LogEntry) error {
	s.attempts++
	return context.DeadlineExceeded
}

// TestAddLogAsync_NormalDelivery 验证正常投递日志到 channel
func TestAddLogAsync_NormalDelivery(t *testing.T) {
	shutdownCh := make(chan struct{})
	isShuttingDown := &atomic.Bool{}
	var wg sync.WaitGroup

	svc := NewLogService(nil, 10, 0, 3, shutdownCh, isShuttingDown, &wg)

	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		Model:      "test-model",
		StatusCode: 200,
		Message:    "test",
	}

	svc.AddLogAsync(entry)

	// 应该能从 logChan 中取到
	select {
	case got := <-svc.logChan:
		if got.Model != "test-model" {
			t.Fatalf("期望 model=test-model, 实际=%s", got.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("超时：日志未投递到 channel")
	}
}

// TestAddLogAsync_ChannelFull_DropsBehavior 验证 channel 满时日志被丢弃并计数
func TestAddLogAsync_ChannelFull_Drops(t *testing.T) {
	shutdownCh := make(chan struct{})
	isShuttingDown := &atomic.Bool{}
	var wg sync.WaitGroup

	// buffer size = 1，只能容纳1条
	svc := NewLogService(nil, 1, 0, 3, shutdownCh, isShuttingDown, &wg)

	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		Model:      "test",
		StatusCode: 200,
	}

	// 先填满 channel
	svc.AddLogAsync(entry)

	// 第二条应该被 drop
	svc.AddLogAsync(entry)
	svc.AddLogAsync(entry)

	dropCount := svc.logDropCount.Load()
	if dropCount < 1 {
		t.Fatalf("期望 drop count >= 1, 实际=%d", dropCount)
	}
}

// TestAddLogAsync_AfterShutdown_Noop 验证 shutdown 后不再投递日志
func TestAddLogAsync_AfterShutdown_Noop(t *testing.T) {
	shutdownCh := make(chan struct{})
	isShuttingDown := &atomic.Bool{}
	var wg sync.WaitGroup

	svc := NewLogService(nil, 10, 0, 3, shutdownCh, isShuttingDown, &wg)

	// 标记为关闭状态
	isShuttingDown.Store(true)

	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		Model:      "should-not-appear",
		StatusCode: 200,
	}

	svc.AddLogAsync(entry)

	// channel 应该为空
	select {
	case <-svc.logChan:
		t.Fatal("shutdown 后不应有日志投递到 channel")
	default:
		// 正确：channel 为空
	}
}

// TestAddLogAsync_DropCountSampling 验证丢弃计数的采样日志逻辑
func TestAddLogAsync_DropCountAccumulates(t *testing.T) {
	shutdownCh := make(chan struct{})
	isShuttingDown := &atomic.Bool{}
	var wg sync.WaitGroup

	// buffer size = 0，所有日志都会被 drop
	svc := NewLogService(nil, 0, 0, 3, shutdownCh, isShuttingDown, &wg)

	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		Model:      "test",
		StatusCode: 200,
	}

	for i := 0; i < 25; i++ {
		svc.AddLogAsync(entry)
	}

	dropCount := svc.logDropCount.Load()
	if dropCount != 25 {
		t.Fatalf("期望 drop count = 25, 实际=%d", dropCount)
	}
}

func TestFlushLogs_ShutdownDisablesRetries(t *testing.T) {
	shutdownCh := make(chan struct{})
	isShuttingDown := &atomic.Bool{}
	isShuttingDown.Store(true)
	var wg sync.WaitGroup

	store := &retryTrackingStore{}
	svc := NewLogService(store, 10, 0, 3, shutdownCh, isShuttingDown, &wg)

	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		Model:      "test-model",
		StatusCode: 500,
	}
	svc.flushLogs([]*model.LogEntry{entry})

	if store.attempts != 1 {
		t.Fatalf("关停阶段应仅尝试一次刷盘，实际尝试次数=%d", store.attempts)
	}
}

// failThenSucceedStore 前 failN 次返回错误，之后返回 nil
type failThenSucceedStore struct {
	storage.Store
	attempts int
	failN    int
}

func (s *failThenSucceedStore) BatchAddLogs(_ context.Context, _ []*model.LogEntry) error {
	s.attempts++
	if s.attempts <= s.failN {
		return fmt.Errorf("simulated transient error (attempt %d)", s.attempts)
	}
	return nil
}

func TestFlushLogs_RetrySucceeds(t *testing.T) {
	shutdownCh := make(chan struct{})
	isShuttingDown := &atomic.Bool{}
	var wg sync.WaitGroup

	store := &failThenSucceedStore{failN: 1}
	svc := NewLogService(store, 10, 0, 3, shutdownCh, isShuttingDown, &wg)

	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		Model:      "test-model",
		StatusCode: 200,
	}
	svc.flushLogs([]*model.LogEntry{entry})

	if store.attempts != 2 {
		t.Fatalf("期望重试后成功 (attempts=2)，实际=%d", store.attempts)
	}
}

func TestFlushLogs_ShutdownInterruptsBackoff(t *testing.T) {
	shutdownCh := make(chan struct{})
	isShuttingDown := &atomic.Bool{}
	var wg sync.WaitGroup

	store := &retryTrackingStore{}
	// MaxRetries=2 在 config 中，但正常路径会重试。
	// 我们在退避等待期间触发 shutdown，期望只尝试 1 次。
	svc := NewLogService(store, 10, 0, 3, shutdownCh, isShuttingDown, &wg)

	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		Model:      "test-model",
		StatusCode: 500,
	}

	// 在短延迟后关闭 shutdownCh，中断退避等待
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(shutdownCh)
	}()

	start := time.Now()
	svc.flushLogs([]*model.LogEntry{entry})
	elapsed := time.Since(start)

	if store.attempts != 1 {
		t.Fatalf("shutdown 应中断退避，期望 attempts=1，实际=%d", store.attempts)
	}
	// 退避基准 100ms，如果没被中断会等 >=100ms。被中断应远小于 100ms。
	if elapsed > 80*time.Millisecond {
		t.Fatalf("shutdown 应快速中断退避，实际耗时=%v", elapsed)
	}
}
