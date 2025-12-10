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
// 设计原则：
// - 流式请求：使用 firstByteTimeout（首字节超时），之后不限制
// - 非流式请求：使用 nonStreamTimeout（整体超时），超时主动关闭上游连接
func (s *Server) newRequestContext(parentCtx context.Context, requestPath string, body []byte) *requestContext {
	isStreaming := isStreamingRequest(requestPath, body)

	ctx := parentCtx
	var cancel context.CancelFunc

	if isStreaming {
		// 流式请求：首字节超时（定时器实现，首字节到达后停止）
		if s.firstByteTimeout > 0 {
			ctx, cancel = context.WithCancel(parentCtx)
		}
	} else {
		// 非流式请求：整体超时（context.WithTimeout，超时自动关闭连接）
		if s.nonStreamTimeout > 0 {
			ctx, cancel = context.WithTimeout(parentCtx, s.nonStreamTimeout)
		}
	}

	reqCtx := &requestContext{
		ctx:         ctx,
		cancel:      cancel,
		startTime:   time.Now(),
		isStreaming: isStreaming,
	}

	// 流式请求的首字节超时定时器
	if isStreaming && s.firstByteTimeout > 0 {
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
