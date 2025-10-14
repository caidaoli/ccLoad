package app

import (
	"context"
	"io"
	"net/http"
	"time"
)

// StreamingDebugMetrics 流式传输调试指标
type StreamingDebugMetrics struct {
	RequestSent      time.Time     // HTTP请求发送时间
	ResponseReceived time.Time     // client.Do() 返回时间（响应头接收完成）
	FirstByteRead    time.Time     // 第一次从resp.Body读取数据的时间
	LastByteRead     time.Time     // 最后一次读取完成的时间
	BytesRead        int64         // 总共读取的字节数
	ReadCount        int           // Read() 调用次数
	TimeToHeader     time.Duration // 响应头接收耗时
	TimeToFirstByte  time.Duration // 首字节实际到达耗时
	TimeToComplete   time.Duration // 总传输耗时
}

// debugStreamCopy 带调试信息的流式复制（用于问题诊断）
// 使用方法：临时替换 streamCopy 函数，记录详细的传输指标
func debugStreamCopy(ctx context.Context, src io.Reader, dst http.ResponseWriter, metrics *StreamingDebugMetrics) error {
	buf := make([]byte, StreamBufferSize)
	readCount := 0
	totalBytes := int64(0)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := src.Read(buf)
		readCount++
		totalBytes += int64(n)

		// 记录首字节实际到达时间
		if readCount == 1 && n > 0 {
			metrics.FirstByteRead = time.Now()
			metrics.TimeToFirstByte = metrics.FirstByteRead.Sub(metrics.RequestSent)
		}

		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
		}

		if err != nil {
			if err == io.EOF {
				metrics.LastByteRead = time.Now()
				metrics.BytesRead = totalBytes
				metrics.ReadCount = readCount
				metrics.TimeToComplete = metrics.LastByteRead.Sub(metrics.RequestSent)
				return nil
			}
			return err
		}
	}
}

// 使用示例（在 handleSuccessResponse 中临时替换）：
/*
metrics := &StreamingDebugMetrics{
	RequestSent:      reqCtx.startTime,
	ResponseReceived: time.Now(),
	TimeToHeader:     time.Since(reqCtx.startTime),
}

// 使用调试版本的streamCopy
streamErr := debugStreamCopy(reqCtx.ctx, resp.Body, w, metrics)

// 记录调试信息
util.SafePrintf("🔍 [流式传输调试] 渠道ID=%d, 响应头耗时=%.3fs, 首字节耗时=%.3fs, 总耗时=%.3fs, 读取次数=%d, 字节数=%d",
	cfg.ID,
	metrics.TimeToHeader.Seconds(),
	metrics.TimeToFirstByte.Seconds(),
	metrics.TimeToComplete.Seconds(),
	metrics.ReadCount,
	metrics.BytesRead,
)
*/
