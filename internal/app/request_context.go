package app

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"ccLoad/internal/protocol"
	"ccLoad/internal/util"
)

// requestContext 封装单次请求的上下文和超时控制
// 从 forwardOnceAsync 提取，遵循SRP原则
// 补充首字节超时管控（可选）
type requestContext struct {
	ctx                           context.Context
	cancel                        context.CancelFunc // [INFO] 总是非 nil（即使是 noop），调用方无需检查
	startTime                     time.Time
	isStreaming                   bool
	transformPlan                 protocol.TransformPlan
	clientProtocol                protocol.Protocol
	upstreamProtocol              protocol.Protocol
	originalModel                 string
	originalBody                  []byte
	translatedBody                []byte
	firstByteTimeout              time.Duration
	streamTimeout                 time.Duration
	streamIdleTimeout             time.Duration
	nonStreamTimeout              time.Duration
	responsesSSEUpstreamNonStream bool
	codeBuddyOAuth                bool
	antigravityOAuth              bool
	antigravityReplay             *antigravityReplay
	anthropicClaudeCodeWire       bool
	replayBodyRulesApplied        bool // 完整回放体已经过 applyBodyRules；增量体单独判定
	zedWire                       *zedWirePlan
	openCodeResponses             *openCodeResponsesPlan
	anthropicToolAliases          anthropicMCPToolAliases
	executionIdentity             string
	firstByteTimer                *time.Timer
	streamTimer                   *time.Timer
	streamIdleTimer               *time.Timer
	firstByteTimedOut             atomic.Bool
	streamTimedOut                atomic.Bool
	streamIdleTimedOut            atomic.Bool
}

// newRequestContext 创建请求上下文（处理超时控制）
// 设计原则：
// - 流式请求：使用 firstByteTimeout（首字节）、streamTimeout（总时长）和 streamIdleTimeout（上游连续无数据）
// - 非流式请求：使用 nonStreamTimeout（整体超时），超时主动关闭上游连接
// [INFO] Go 1.21+ 改进：总是返回非 nil 的 cancel，调用方无需检查（符合 Go 惯用法）
func (s *Server) newRequestContext(parentCtx context.Context, requestPath string, body []byte) *requestContext {
	return s.newRequestContextWithTimeouts(parentCtx, requestPath, body, protocolTimeoutConfig{
		FirstByteTimeout:  s.firstByteTimeout,
		StreamTimeout:     s.streamTimeout,
		StreamIdleTimeout: s.streamIdleTimeout,
		NonStreamTimeout:  s.nonStreamTimeout,
	})
}

func (s *Server) newRequestContextWithTimeouts(parentCtx context.Context, requestPath string, body []byte, timeouts protocolTimeoutConfig) *requestContext {
	return newRequestContextForStreaming(parentCtx, isStreamingRequest(requestPath, body), timeouts)
}

func newRequestContextForStreaming(parentCtx context.Context, isStreaming bool, timeouts protocolTimeoutConfig) *requestContext {

	// [INFO] 关键改动：总是使用 WithCancel 包裹（即使无超时配置也能正常取消）
	ctx, cancel := context.WithCancel(parentCtx)

	// 非流式请求：在基础 cancel 之上叠加整体超时
	if !isStreaming && timeouts.NonStreamTimeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, timeouts.NonStreamTimeout)
		// 链式 cancel：timeout 触发时也会取消父 context
		originalCancel := cancel
		cancel = func() {
			timeoutCancel()
			originalCancel()
		}
	}

	reqCtx := &requestContext{
		ctx:               ctx,
		cancel:            cancel, // [INFO] 总是非 nil，无需检查
		startTime:         time.Now(),
		isStreaming:       isStreaming,
		firstByteTimeout:  timeouts.FirstByteTimeout,
		streamTimeout:     timeouts.StreamTimeout,
		streamIdleTimeout: timeouts.StreamIdleTimeout,
		nonStreamTimeout:  timeouts.NonStreamTimeout,
	}

	if isStreaming && timeouts.StreamTimeout > 0 {
		reqCtx.streamTimer = time.AfterFunc(timeouts.StreamTimeout, func() {
			reqCtx.streamTimedOut.Store(true)
			cancel()
		})
	}

	// 从发出请求起计时：响应头迟迟不来也算空闲，读到上游字节时重置。
	if isStreaming && timeouts.StreamIdleTimeout > 0 {
		reqCtx.streamIdleTimer = time.AfterFunc(timeouts.StreamIdleTimeout, func() {
			reqCtx.streamIdleTimedOut.Store(true)
			cancel()
		})
	}

	// 流式请求的首字节超时定时器
	if isStreaming && timeouts.FirstByteTimeout > 0 {
		reqCtx.firstByteTimer = time.AfterFunc(timeouts.FirstByteTimeout, func() {
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

func (rc *requestContext) streamTimeoutTriggered() bool {
	return rc.streamTimedOut.Load() || rc.streamIdleTimedOut.Load()
}

// touchStreamIdle 在读到上游数据时重置空闲定时器。
func (rc *requestContext) touchStreamIdle() {
	if rc.streamIdleTimer != nil && !rc.streamIdleTimedOut.Load() {
		rc.streamIdleTimer.Reset(rc.streamIdleTimeout)
	}
}

// streamTimeoutError 描述已触发的流式超时（总时长或空闲），均归为 ErrUpstreamStreamTimeout。
func (rc *requestContext) streamTimeoutError(durationSec float64) error {
	if rc.streamIdleTimedOut.Load() {
		return fmt.Errorf("upstream stream idle timeout after %.2fs (no data for %v): %w",
			durationSec, rc.streamIdleTimeout, util.ErrUpstreamStreamTimeout)
	}
	return fmt.Errorf("upstream stream timeout after %.2fs (threshold=%v): %w",
		durationSec, rc.streamTimeout, util.ErrUpstreamStreamTimeout)
}

// Duration 返回从请求开始到现在的时间
func (rc *requestContext) Duration() time.Duration {
	return time.Since(rc.startTime)
}

// cleanup 统一清理请求上下文资源（定时器 + context）
// [INFO] 符合 Go 惯用法：defer reqCtx.cleanup() 一行搞定
func (rc *requestContext) cleanup() {
	rc.stopFirstByteTimer() // 停止首字节超时定时器
	if rc.streamTimer != nil {
		rc.streamTimer.Stop()
	}
	if rc.streamIdleTimer != nil {
		rc.streamIdleTimer.Stop()
	}
	rc.cancel() // 取消 context（总是非 nil，无需检查）
}
