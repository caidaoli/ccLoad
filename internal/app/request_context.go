package app

import (
	"context"
	"time"
)

// requestContext 封装单次请求的上下文和超时控制
// ✅ P1-1 重构 (2025-01-XX): 从 forwardOnceAsync 提取，遵循SRP原则
type requestContext struct {
	ctx              context.Context
	cancel           context.CancelFunc
	startTime        time.Time
	isStreaming      bool
	firstByteTimeout time.Duration
}

// newRequestContext 创建请求上下文（处理超时控制）
// 设计原则：
// - 流式请求启用首字节超时（避免长时间等待）
// - 非流式请求依赖 Transport.ResponseHeaderTimeout
// - 使用 defer cancel() 确保资源正确释放
func (s *Server) newRequestContext(parentCtx context.Context, requestPath string, body []byte) *requestContext {
	isStreaming := isStreamingRequest(requestPath, body)
	reqCtx := &requestContext{
		startTime:        time.Now(),
		isStreaming:      isStreaming,
		firstByteTimeout: s.firstByteTimeout,
	}

	// 流式请求：设置首字节超时
	if isStreaming && s.firstByteTimeout > 0 {
		reqCtx.ctx, reqCtx.cancel = context.WithTimeout(parentCtx, s.firstByteTimeout)
	} else {
		// 非流式请求：使用原始上下文
		reqCtx.ctx = parentCtx
		reqCtx.cancel = func() {} // 空函数，统一接口
	}

	return reqCtx
}

// Duration 返回从请求开始到现在的时间（秒）
func (rc *requestContext) Duration() float64 {
	return time.Since(rc.startTime).Seconds()
}

// Close 清理上下文资源
// 必须在请求完成后调用，避免 context 泄漏
func (rc *requestContext) Close() {
	if rc.cancel != nil {
		rc.cancel()
	}
}
