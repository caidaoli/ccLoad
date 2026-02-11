package app

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
)

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
