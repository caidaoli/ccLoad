package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 流式传输数据结构
// ============================================================================

// streamReadStats 流式传输统计信息
type streamReadStats struct {
	readCount  int
	totalBytes int64
}

// firstByteDetector 检测首字节读取时间和传输统计的Reader包装器
type firstByteDetector struct {
	io.ReadCloser
	stats       *streamReadStats
	onFirstRead func()
}

// Read 实现io.Reader接口，记录读取统计
func (r *firstByteDetector) Read(p []byte) (n int, err error) {
	n, err = r.ReadCloser.Read(p)
	if n > 0 {
		// 记录统计信息
		if r.stats != nil {
			r.stats.readCount++
			r.stats.totalBytes += int64(n)
		}
		// 触发首次读取回调
		if r.onFirstRead != nil {
			r.onFirstRead()
			r.onFirstRead = nil // 只触发一次
		}
	}
	return
}

// idleTimeoutReader 流读取空闲超时包装器
// 如果长时间没有数据到达，主动关闭连接
type idleTimeoutReader struct {
	io.ReadCloser
	timeout       time.Duration
	timer         *time.Timer
	timerMu       sync.Mutex
	timedOut      atomic.Bool    // 标记是否已超时
	onIdleTimeout func()          // 超时回调（用于记录日志）
}

// ErrStreamIdleTimeout 流读取空闲超时错误
var ErrStreamIdleTimeout = errors.New("stream idle timeout: no data received within configured duration")

// Read 实现io.Reader接口，每次读取后重置超时定时器
func (r *idleTimeoutReader) Read(p []byte) (n int, err error) {
	r.resetTimer()
	n, err = r.ReadCloser.Read(p)
	if n > 0 {
		r.resetTimer() // 读取成功，重置定时器
	}
	// 如果已超时，返回明确的超时错误而不是EOF
	if r.timedOut.Load() && err == io.EOF {
		err = ErrStreamIdleTimeout
	}
	return
}

// resetTimer 重置空闲定时器
func (r *idleTimeoutReader) resetTimer() {
	r.timerMu.Lock()
	defer r.timerMu.Unlock()

	if r.timer != nil {
		r.timer.Stop()
	}

	r.timer = time.AfterFunc(r.timeout, func() {
		r.timedOut.Store(true) // 标记为超时
		if r.onIdleTimeout != nil {
			r.onIdleTimeout()
		}
		r.ReadCloser.Close() // 超时后关闭底层连接，触发Read返回错误
	})
}

// Close 关闭Reader并停止定时器
func (r *idleTimeoutReader) Close() error {
	r.timerMu.Lock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.timerMu.Unlock()
	return r.ReadCloser.Close()
}

// ============================================================================
// 流式传输核心函数
// ============================================================================

// streamCopy 流式复制（支持flusher与ctx取消）
// 从proxy.go提取，遵循SRP原则
// 简化实现：直接循环读取与写入，避免为每次读取创建goroutine导致泄漏
// 首字节超时依赖于上游握手/响应头阶段的超时控制（Transport 配置），此处不再重复实现
func streamCopy(ctx context.Context, src io.Reader, dst http.ResponseWriter, onData func([]byte) error) error {
	buf := make([]byte, StreamBufferSize)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
			if onData != nil {
				if hookErr := onData(buf[:n]); hookErr != nil {
					// 钩子错误不中断流传输（容错设计）
					// 错误日志由钩子内部自行处理
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// streamCopySSE SSE专用流式复制（使用小缓冲区优化延迟）
// ✅ SSE优化（2025-10-17）：4KB缓冲区降低首Token延迟60~80%
// ✅ 支持数据钩子（2025-11）：允许SSE usage解析器增量处理数据流
// 设计原则：SSE事件通常200B-2KB，小缓冲区避免事件积压
func streamCopySSE(ctx context.Context, src io.Reader, dst http.ResponseWriter, onData func([]byte) error) error {
	buf := make([]byte, SSEBufferSize) // 4KB SSE专用缓冲区
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
			// 触发数据钩子（例如：SSE usage解析）
			if onData != nil {
				if hookErr := onData(buf[:n]); hookErr != nil {
					// 钩子错误不中断流传输（容错设计）
					// 实际场景：解析失败不应影响用户接收响应
					// 错误已在钩子内部记录日志
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
