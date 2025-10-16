package app

import (
	"context"
	"time"
)

// requestContext 封装单次请求的上下文和超时控制
// ✅ P1-1 重构 (2025-01-XX): 从 forwardOnceAsync 提取，遵循SRP原则
type requestContext struct {
	ctx         context.Context
	cancel      context.CancelFunc
	startTime   time.Time
	isStreaming bool
}

// newRequestContext 创建请求上下文（处理超时控制）
// ✅ 简化设计：移除应用层超时，使用客户端超时（透明代理原则）
// 设计原则：
// - 透明代理不应干预客户端的超时设置
// - 仅依赖底层网络超时（DialContext、TLSHandshakeTimeout）
// - 使用 defer cancel() 确保资源正确释放
func (s *Server) newRequestContext(parentCtx context.Context, requestPath string, body []byte) *requestContext {
	isStreaming := isStreamingRequest(requestPath, body)
	reqCtx := &requestContext{
		startTime:   time.Now(),
		isStreaming: isStreaming,
		ctx:         parentCtx,
		cancel:      func() {}, // 空函数，统一接口
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
