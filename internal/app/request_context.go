package app

import (
	"context"
	"time"
)

// requestContext 封装单次请求的上下文和超时控制
// ✅ P1-1 重构 (2025-01-XX): 从 forwardOnceAsync 提取，遵循SRP原则
// ✅ P0修复(2025-10-29): 移除误导性的cancel字段和Close方法
type requestContext struct {
	ctx         context.Context
	startTime   time.Time
	isStreaming bool
}

// newRequestContext 创建请求上下文（处理超时控制）
// ✅ 简化设计：移除应用层超时，使用客户端超时（透明代理原则）
// 设计原则：
// - 透明代理不应干预客户端的超时设置
// - 仅依赖底层网络超时（DialContext、TLSHandshakeTimeout）
// - 使用 parentCtx 传递客户端的取消信号
// ✅ P0修复(2025-10-29): 移除空cancel函数，避免误导性设计
func (s *Server) newRequestContext(parentCtx context.Context, requestPath string, body []byte) *requestContext {
	isStreaming := isStreamingRequest(requestPath, body)
	reqCtx := &requestContext{
		startTime:   time.Now(),
		isStreaming: isStreaming,
		ctx:         parentCtx,
	}

	return reqCtx
}

// Duration 返回从请求开始到现在的时间（秒）
func (rc *requestContext) Duration() float64 {
	return time.Since(rc.startTime).Seconds()
}
