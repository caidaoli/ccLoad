package app

import (
	"context"
	"sync/atomic"
	"time"
)

// requestContext 封装单次请求的上下文和超时控制
// 从 forwardOnceAsync 提取，遵循SRP原则
// 补充首字节超时管控（可选）
type requestContext struct {
	ctx               context.Context
	cancel            context.CancelFunc
	startTime         time.Time
	isStreaming       bool
	firstByteTimer    *time.Timer
	firstByteTimedOut atomic.Bool
}

// newRequestContext 创建请求上下文（处理超时控制）
// ✅ 简化设计：默认透传客户端上下文，可选启用首字节超时保护
// 设计原则：
// - 透明代理不应干预客户端的超时设置
// - 可选的首字节超时只在配置时启用
// - 使用 parentCtx 传递客户端的取消信号
func (s *Server) newRequestContext(parentCtx context.Context, requestPath string, body []byte) *requestContext {
	isStreaming := isStreamingRequest(requestPath, body)

	ctx := parentCtx
	var cancel context.CancelFunc

	if s.firstByteTimeout > 0 {
		ctx, cancel = context.WithCancel(parentCtx)
	}

	reqCtx := &requestContext{
		ctx:         ctx,
		cancel:      cancel,
		startTime:   time.Now(),
		isStreaming: isStreaming,
	}

	if s.firstByteTimeout > 0 {
		reqCtx.firstByteTimer = time.AfterFunc(s.firstByteTimeout, func() {
			reqCtx.firstByteTimedOut.Store(true)
			if reqCtx.cancel != nil {
				reqCtx.cancel()
			}
		})
	}

	return reqCtx
}

func (rc *requestContext) stopFirstByteTimer() {
	if rc.firstByteTimer != nil {
		rc.firstByteTimer.Stop()
	}
}

func (rc *requestContext) firstByteTimeoutTriggered() bool {
	return rc.firstByteTimedOut.Load()
}

// Duration 返回从请求开始到现在的时间（秒）
func (rc *requestContext) Duration() float64 {
	return time.Since(rc.startTime).Seconds()
}
