package app

import (
	"bufio"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// errorReader 模拟返回特定错误的 Reader
type errorReader struct {
	err error
}

func (r *errorReader) Read(_ []byte) (int, error) {
	return 0, r.err
}

type blockingReadCloser struct {
	closeOnce sync.Once
	readOnce  sync.Once
	entered   chan struct{}
	closed    chan struct{}
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{
		entered: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (r *blockingReadCloser) Read(_ []byte) (int, error) {
	r.readOnce.Do(func() {
		close(r.entered)
	})
	<-r.closed
	return 0, errors.New("read closed")
}

func (r *blockingReadCloser) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
	})
	return nil
}

// TestStreamCopySSE_ContextCanceledDuringRead 测试在 Read 期间 context 被取消的场景
// 场景：客户端取消请求 → HTTP/2 流关闭 → Read 返回 "http2: response body closed"
// 期望：返回 context.Canceled 而非原始错误，让上层正确识别为客户端断开（499）
func TestStreamCopySSE_ContextCanceledDuringRead(t *testing.T) {
	tests := []struct {
		name        string
		readErr     error
		ctxCanceled bool
		wantErr     error
		reason      string
	}{
		{
			name:        "http2_closed_with_ctx_canceled",
			readErr:     errors.New("http2: response body closed"),
			ctxCanceled: true,
			wantErr:     context.Canceled,
			reason:      "context 已取消时，应返回 context.Canceled 而非 http2 错误",
		},
		{
			name:        "http2_closed_without_ctx_canceled",
			readErr:     errors.New("http2: response body closed"),
			ctxCanceled: false,
			wantErr:     errors.New("http2: response body closed"),
			reason:      "context 未取消时，应返回原始错误",
		},
		{
			name:        "stream_error_with_ctx_canceled",
			readErr:     errors.New("stream error: stream ID 7; INTERNAL_ERROR"),
			ctxCanceled: true,
			wantErr:     context.Canceled,
			reason:      "context 已取消时，stream error 也应转换为 context.Canceled",
		},
		{
			name:        "network_error_with_ctx_canceled",
			readErr:     errors.New("connection reset by peer"),
			ctxCanceled: true,
			wantErr:     context.Canceled,
			reason:      "context 已取消时，网络错误应转换为 context.Canceled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tt.ctxCanceled {
				cancel() // 模拟客户端取消
			} else {
				defer cancel()
			}

			// 创建模拟 Reader 返回指定错误
			reader := &errorReader{err: tt.readErr}
			recorder := newRecorder()

			// 调用 streamCopySSE
			err := streamCopySSE(ctx, reader, recorder, nil)

			if tt.ctxCanceled {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("%s: got err=%v, want context.Canceled", tt.reason, err)
				}
			} else {
				if err == nil || err.Error() != tt.readErr.Error() {
					t.Errorf("%s: got err=%v, want %v", tt.reason, err, tt.readErr)
				}
			}
		})
	}
}

// TestStreamCopy_ContextCanceledDuringRead 测试非 SSE 流复制在 Read 期间 context 被取消的场景
func TestStreamCopy_ContextCanceledDuringRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端取消

	reader := &errorReader{err: errors.New("http2: response body closed")}
	recorder := newRecorder()

	err := streamCopy(ctx, reader, recorder, nil)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("streamCopy should return context.Canceled when ctx is canceled, got: %v", err)
	}
}

func TestStreamCopy_ClosesReadCloserOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := newBlockingReadCloser()
	done := make(chan error, 1)

	go func() {
		done <- streamCopy(ctx, reader, newRecorder(), nil)
	}()

	select {
	case <-reader.entered:
	case <-time.After(200 * time.Millisecond):
		_ = reader.Close()
		t.Fatal("streamCopy did not enter Read")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("streamCopy err=%v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		_ = reader.Close()
		t.Fatal("streamCopy did not unblock Read after context cancellation")
	}
}

func TestStreamCopy_ClosesWrappedUnderlyingCloserOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	underlying := newBlockingReadCloser()
	reader := bufio.NewReader(underlying)
	wrapped := readerWithCloser{Reader: reader, Closer: underlying}
	done := make(chan error, 1)

	go func() {
		done <- streamCopy(ctx, wrapped, newRecorder(), nil)
	}()

	select {
	case <-underlying.entered:
	case <-time.After(200 * time.Millisecond):
		_ = underlying.Close()
		t.Fatal("streamCopy did not enter wrapped Read")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("streamCopy err=%v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		_ = underlying.Close()
		t.Fatal("streamCopy did not close wrapped underlying reader after context cancellation")
	}
}
