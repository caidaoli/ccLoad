package app

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"time"
)

// traceCollector HTTP 追踪数据收集器
// ✅ P1-1 重构 (2025-01-XX): 从 forwardOnceAsync 提取，遵循SRP原则
type traceCollector struct {
	dnsStart, connStart, tlsStart time.Time
	DNS, Connect, TLS, WroteReq   float64
}

// attachTrace 附加 HTTP 追踪到上下文（如果启用）
// 性能优化：仅在 CCLOAD_ENABLE_TRACE=1 时启用，节省 0.5-1ms/请求
func (tc *traceCollector) attachTrace(ctx context.Context, startTime time.Time) context.Context {
	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			tc.dnsStart = time.Now()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if !tc.dnsStart.IsZero() {
				tc.DNS = time.Since(tc.dnsStart).Seconds()
			}
		},
		ConnectStart: func(network, addr string) {
			tc.connStart = time.Now()
		},
		ConnectDone: func(network, addr string, err error) {
			if !tc.connStart.IsZero() {
				tc.Connect = time.Since(tc.connStart).Seconds()
			}
		},
		TLSHandshakeStart: func() {
			tc.tlsStart = time.Now()
		},
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			if !tc.tlsStart.IsZero() {
				tc.TLS = time.Since(tc.tlsStart).Seconds()
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			tc.WroteReq = time.Since(startTime).Seconds()
		},
	}
	return httptrace.WithClientTrace(ctx, trace)
}

// toBreakdown 转换为 traceBreakdown 结构
func (tc *traceCollector) toBreakdown(firstByteTime float64) *traceBreakdown {
	return &traceBreakdown{
		DNS:       tc.DNS,
		Connect:   tc.Connect,
		TLS:       tc.TLS,
		WroteReq:  tc.WroteReq,
		FirstByte: firstByteTime,
	}
}
