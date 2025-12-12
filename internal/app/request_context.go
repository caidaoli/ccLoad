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
	cancel            context.CancelFunc // [INFO] 总是非 nil（即使是 noop），调用方无需检查
	startTime         time.Time
	isStreaming       bool
	firstByteTimer    *time.Timer
	firstByteTimedOut atomic.Bool
}

// newRequestContext 创建请求上下文（处理超时控制）
// 设计原则：
// - 流式请求：使用 firstByteTimeout（首字节超时），之后不限制
// - 非流式请求：使用 nonStreamTimeout（整体超时），超时主动关闭上游连接
// [INFO] Go 1.21+ 改进：总是返回非 nil 的 cancel，调用方无需检查（符合 Go 惯用法）
func (s *Server) newRequestContext(parentCtx context.Context, requestPath string, body []byte) *requestContext {
	isStreaming := isStreamingRequest(requestPath, body)

	// [INFO] 关键改动：总是使用 WithCancel 包裹（即使无超时配置也能正常取消）
	ctx, cancel := context.WithCancel(parentCtx)

	// 非流式请求：在基础 cancel 之上叠加整体超时
	if !isStreaming && s.nonStreamTimeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, s.nonStreamTimeout)
		// 链式 cancel：timeout 触发时也会取消父 context
		originalCancel := cancel
		cancel = func() {
			timeoutCancel()
			originalCancel()
		}
	}

	reqCtx := &requestContext{
		ctx:         ctx,
		cancel:      cancel, // [INFO] 总是非 nil，无需检查
		startTime:   time.Now(),
		isStreaming: isStreaming,
	}

	// 流式请求的首字节超时定时器
	if isStreaming && s.firstByteTimeout > 0 {
		reqCtx.firstByteTimer = time.AfterFunc(s.firstByteTimeout, func() {
			reqCtx.firstByteTimedOut.Store(true)
			cancel() // [INFO] 直接调用，无需检查
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

// cleanup 统一清理请求上下文资源（定时器 + context）
// [INFO] 符合 Go 惯用法：defer reqCtx.cleanup() 一行搞定
func (rc *requestContext) cleanup() {
	rc.stopFirstByteTimer() // 停止首字节超时定时器
	rc.cancel()             // 取消 context（总是非 nil，无需检查）
}
