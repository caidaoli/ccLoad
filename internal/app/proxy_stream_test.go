package app

import (
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockSlowReader 模拟慢速上游（发送一部分数据后停止）
type mockSlowReader struct {
	data     string
	pos      int
	stopAt   int // 发送stopAt字节后阻塞
	isClosed atomic.Bool
}

func (r *mockSlowReader) Read(p []byte) (n int, err error) {
	if r.isClosed.Load() {
		return 0, io.ErrClosedPipe
	}
	if r.pos >= r.stopAt {
		// 模拟上游僵死：循环检查直到被关闭
		for !r.isClosed.Load() {
			time.Sleep(10 * time.Millisecond)
		}
		return 0, io.ErrClosedPipe
	}
	n = copy(p, r.data[r.pos:r.stopAt])
	r.pos += n
	return n, nil
}

func (r *mockSlowReader) Close() error {
	r.isClosed.Store(true)
	return nil
}

// TestIdleTimeoutReader_NormalFlow 测试正常流式传输（不超时）
func TestIdleTimeoutReader_NormalFlow(t *testing.T) {
	data := "event: message\ndata: hello world\n\n"
	reader := &idleTimeoutReader{
		ReadCloser: io.NopCloser(strings.NewReader(data)),
		timeout:    100 * time.Millisecond,
	}
	defer reader.Close()

	buf := make([]byte, 4096)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes, got %d", len(data), n)
	}
}

// TestIdleTimeoutReader_Timeout 测试空闲超时触发
func TestIdleTimeoutReader_Timeout(t *testing.T) {
	mockReader := &mockSlowReader{
		data:   "event: message\ndata: partial",
		pos:    0,
		stopAt: 10, // 只发送前10字节，然后阻塞
	}

	timeoutTriggered := atomic.Bool{}
	reader := &idleTimeoutReader{
		ReadCloser: mockReader,
		timeout:    200 * time.Millisecond,
		onIdleTimeout: func() {
			timeoutTriggered.Store(true)
		},
	}
	defer reader.Close()

	// 第一次读取：成功读取10字节
	buf := make([]byte, 4096)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("first read failed: %v", err)
	}
	if n != 10 {
		t.Fatalf("expected 10 bytes, got %d", n)
	}

	// 第二次读取：应该在200ms后超时并返回ErrStreamIdleTimeout
	start := time.Now()
	_, err = reader.Read(buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, ErrStreamIdleTimeout) && err != io.ErrClosedPipe {
		t.Fatalf("expected ErrStreamIdleTimeout or ErrClosedPipe, got: %v", err)
	}
	if elapsed < 200*time.Millisecond {
		t.Fatalf("timeout triggered too early: %v", elapsed)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("timeout triggered too late: %v", elapsed)
	}
	if !timeoutTriggered.Load() {
		t.Fatal("onIdleTimeout callback not triggered")
	}
}

// TestIdleTimeoutReader_MultipleReads 测试连续读取重置定时器
func TestIdleTimeoutReader_MultipleReads(t *testing.T) {
	data := "0123456789abcdefghijklmnopqrstuvwxyz"
	reader := &idleTimeoutReader{
		ReadCloser: io.NopCloser(strings.NewReader(data)),
		timeout:    100 * time.Millisecond,
	}
	defer reader.Close()

	// 连续快速读取，每次间隔50ms（小于100ms超时）
	buf := make([]byte, 10)
	for i := 0; i < 3; i++ {
		time.Sleep(50 * time.Millisecond)
		n, err := reader.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("read %d failed: %v", i, err)
		}
		if n == 0 {
			break
		}
	}
}
