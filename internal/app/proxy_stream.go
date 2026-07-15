package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
)

var (
	errAbortStreamBeforeWrite = errors.New("abort stream before first client write")
	errStopStreamAfterWrite   = errors.New("stop stream after current client write")
)

type stopStreamAfterWriteError struct {
	writeBytes int
}

func (e *stopStreamAfterWriteError) Error() string {
	return errStopStreamAfterWrite.Error()
}

func (e *stopStreamAfterWriteError) Unwrap() error {
	return errStopStreamAfterWrite
}

// maxTrackedSSELineBytes bounds the framing state; response.completed event lines are tiny.
const maxTrackedSSELineBytes = 256

// responsesCompletedDetector recognizes a complete Responses terminal SSE event while
// retaining only bounded line state. Payload bytes continue through the normal chunk copier.
type responsesCompletedDetector struct {
	line              []byte
	lineTooLong       bool
	eventType         string
	skipLF            bool
	preferCRLF        bool
	pendingTerminalCR bool
}

func newResponsesCompletedDetector() *responsesCompletedDetector {
	return &responsesCompletedDetector{line: make([]byte, 0, maxTrackedSSELineBytes)}
}

func (d *responsesCompletedDetector) Feed(data []byte) bool {
	_, completed := d.TerminalBoundary(data)
	return completed
}

// TerminalBoundary returns the byte offset immediately after the terminal event delimiter.
func (d *responsesCompletedDetector) TerminalBoundary(data []byte) (int, bool) {
	if d.pendingTerminalCR {
		d.pendingTerminalCR = false
		if len(data) > 0 && data[0] == '\n' {
			return 1, true
		}
		return 0, true
	}

	for i, b := range data {
		if d.skipLF {
			d.skipLF = false
			if b == '\n' {
				d.preferCRLF = true
				continue
			}
			d.preferCRLF = false
		}

		switch b {
		case '\r':
			if d.finishLine() {
				end := i + 1
				if end < len(data) {
					if data[end] == '\n' {
						return end + 1, true
					}
					return end, true
				}
				if d.preferCRLF {
					d.pendingTerminalCR = true
					return 0, false
				}
				return end, true
			}
			d.skipLF = true
		case '\n':
			d.preferCRLF = false
			if d.finishLine() {
				return i + 1, true
			}
		default:
			if len(d.line) < maxTrackedSSELineBytes {
				d.line = append(d.line, b)
			} else {
				d.lineTooLong = true
			}
		}
	}
	return 0, false
}

func (d *responsesCompletedDetector) finishLine() bool {
	defer func() {
		d.line = d.line[:0]
		d.lineTooLong = false
	}()

	if !d.lineTooLong && len(d.line) == 0 {
		completed := d.eventType == "response.completed"
		d.eventType = ""
		return completed
	}
	if d.lineTooLong {
		if bytes.HasPrefix(d.line, []byte("event:")) {
			d.eventType = ""
		}
		return false
	}
	if after, ok := bytes.CutPrefix(d.line, []byte("event:")); ok {
		// SSE removes at most one leading ASCII space after the colon.
		if len(after) > 0 && after[0] == ' ' {
			after = after[1:]
		}
		d.eventType = string(after)
	}
	return false
}

// ============================================================================
// 流式传输数据结构
// ============================================================================

// streamReadStats 流式传输统计信息
type streamReadStats struct {
	readCount    int
	totalBytes   int64
	firstByteSec float64 // 首字节读取耗时（秒），attachFirstByteDetector 写入
}

// firstByteDetector 检测首字节读取时间和传输统计的Reader包装器
type firstByteDetector struct {
	io.ReadCloser
	stats       *streamReadStats
	onFirstRead func()
	onBytesRead func(int64) // 可选：每次读取后的回调（nil 时不触发）
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
		// 触发字节读取回调（可选）
		if r.onBytesRead != nil {
			r.onBytesRead(int64(n))
		}
	}
	return
}

// ============================================================================
// 流式传输核心函数
// ============================================================================

func streamCopyWithBufferSize(ctx context.Context, src io.Reader, dst http.ResponseWriter, onData func([]byte) error, bufSize int) error {
	stopCloseOnCancel := closeReaderOnContextCancel(ctx, src)
	defer stopCloseOnCancel()

	buf := make([]byte, bufSize)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := src.Read(buf)
		if n > 0 {
			stopAfterWrite := false
			writeBytes := n
			// [FIX] 2026-01: 先 Feed 数据到 parser，再写入客户端
			// 原因：即使写入失败（客户端断开），也需要检测流结束标志（如 response.completed）
			// 这样当上游完整返回但客户端取消时，可以正确识别为"流完整"而非 499
			if onData != nil {
				if hookErr := onData(buf[:n]); hookErr != nil {
					if errors.Is(hookErr, errAbortStreamBeforeWrite) {
						return hookErr
					}
					if errors.Is(hookErr, errStopStreamAfterWrite) {
						stopAfterWrite = true
						var stopErr *stopStreamAfterWriteError
						if errors.As(hookErr, &stopErr) && stopErr.writeBytes >= 0 && stopErr.writeBytes <= n {
							writeBytes = stopErr.writeBytes
						}
					}
					_ = hookErr // 钩子错误不中断流传输（容错设计）
				}
			}
			if _, writeErr := dst.Write(buf[:writeBytes]); writeErr != nil {
				return writeErr
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
			if stopAfterWrite {
				return nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			// [FIX] 检查 context 是否在 Read 期间被取消
			// 场景：客户端取消请求 → HTTP/2 流关闭 → Read 返回 "http2: response body closed"
			// 此时应返回 context.Canceled，让上层正确识别为客户端断开（499）而非上游错误（502）
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

func closeReaderOnContextCancel(ctx context.Context, src io.Reader) func() {
	closer, ok := src.(io.Closer)
	if !ok {
		return func() {}
	}
	stop := context.AfterFunc(ctx, func() {
		_ = closer.Close()
	})
	return func() {
		_ = stop()
	}
}

// deferredResponseWriter 延迟提交响应头，允许在首个可见输出前中止本次流并切换到其他上游。
type deferredResponseWriter struct {
	target    http.ResponseWriter
	header    http.Header
	status    int
	committed bool
	buffer    bytes.Buffer
}

func newDeferredResponseWriter(target http.ResponseWriter) *deferredResponseWriter {
	return &deferredResponseWriter{
		target: target,
		header: make(http.Header),
	}
}

func (w *deferredResponseWriter) Header() http.Header {
	return w.header
}

func (w *deferredResponseWriter) WriteHeader(status int) {
	if w.committed {
		return
	}
	w.status = status
}

func (w *deferredResponseWriter) Write(p []byte) (int, error) {
	if !w.committed {
		return w.buffer.Write(p)
	}
	return w.target.Write(p)
}

func (w *deferredResponseWriter) Flush() {
	if !w.committed {
		return
	}
	if flusher, ok := w.target.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *deferredResponseWriter) Commit() error {
	if w.committed {
		return nil
	}
	for key, values := range w.header {
		dstValues := append([]string(nil), values...)
		w.target.Header()[key] = dstValues
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.target.WriteHeader(status)
	w.committed = true
	if w.buffer.Len() > 0 {
		if _, err := w.target.Write(w.buffer.Bytes()); err != nil {
			return err
		}
		w.buffer.Reset()
	}
	return nil
}

func (w *deferredResponseWriter) Committed() bool {
	return w.committed
}

// streamCopy 流式复制（支持flusher与ctx取消）
// 从proxy.go提取，遵循SRP原则
// 简化实现：直接循环读取与写入，避免为每次读取创建goroutine导致泄漏
// 首字节超时由 requestContext 统一管控（firstByteTimeout + context.AfterFunc 关闭 body），此处不再重复实现
func streamCopy(ctx context.Context, src io.Reader, dst http.ResponseWriter, onData func([]byte) error) error {
	return streamCopyWithBufferSize(ctx, src, dst, onData, StreamBufferSize)
}

// streamCopySSE SSE专用流式复制（使用小缓冲区优化延迟）
// [INFO] SSE优化（2025-10-17）：4KB缓冲区降低首Token延迟60~80%
// [INFO] 支持数据钩子（2025-11）：允许SSE usage解析器增量处理数据流
// 设计原则：SSE事件通常200B-2KB，小缓冲区避免事件积压
func streamCopySSE(ctx context.Context, src io.Reader, dst http.ResponseWriter, onData func([]byte) error) error {
	return streamCopyWithBufferSize(ctx, src, dst, onData, SSEBufferSize)
}

func streamTransformSSEEvents(
	ctx context.Context,
	src io.Reader,
	dst http.ResponseWriter,
	onRawEvent func([]byte) error,
	transform func([]byte) ([][]byte, error),
) error {
	stopCloseOnCancel := closeReaderOnContextCancel(ctx, src)
	defer stopCloseOnCancel()

	reader := bufio.NewReader(src)
	var eventBuf bytes.Buffer

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			eventBuf.Write(line)
			if bytes.Equal(bytes.TrimRight(line, "\r\n"), []byte{}) {
				rawEvent := append([]byte(nil), eventBuf.Bytes()...)
				if len(rawEvent) > 0 {
					if onRawEvent != nil {
						if hookErr := onRawEvent(rawEvent); hookErr != nil {
							if errors.Is(hookErr, errAbortStreamBeforeWrite) {
								return hookErr
							}
							_ = hookErr
						}
					}
					if transform != nil {
						chunks, transformErr := transform(rawEvent)
						if transformErr != nil {
							return transformErr
						}
						for _, chunk := range chunks {
							if len(chunk) == 0 {
								continue
							}
							if _, writeErr := dst.Write(chunk); writeErr != nil {
								return writeErr
							}
							if flusher, ok := dst.(http.Flusher); ok {
								flusher.Flush()
							}
						}
					}
				}
				eventBuf.Reset()
			}
		}

		if err != nil {
			if err == io.EOF {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}
