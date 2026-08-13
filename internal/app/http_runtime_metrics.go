package app

import "sync/atomic"

type httpProxyRuntimeStats struct {
	ActiveRequests       int64  `json:"active_requests"`
	CompletedRequests    uint64 `json:"completed_requests"`
	NonErrorResponses    uint64 `json:"non_error_responses"`
	ClientErrorResponses uint64 `json:"client_error_responses"`
	ServerErrorResponses uint64 `json:"server_error_responses"`
	StreamingRequests    uint64 `json:"streaming_requests"`
	NonStreamingRequests uint64 `json:"non_streaming_requests"`
	RequestBodyBytes     uint64 `json:"request_body_bytes"`
	ResponseBodyBytes    uint64 `json:"response_body_bytes"`
}

// httpProxyRuntimeMetrics tracks client-facing HTTP proxy requests only.
// Responses WebSocket connections are accounted by their dedicated tracker.
type httpProxyRuntimeMetrics struct {
	activeRequests       atomic.Int64
	completedRequests    atomic.Uint64
	nonErrorResponses    atomic.Uint64
	clientErrorResponses atomic.Uint64
	serverErrorResponses atomic.Uint64
	streamingRequests    atomic.Uint64
	nonStreamingRequests atomic.Uint64
	requestBodyBytes     atomic.Uint64
	responseBodyBytes    atomic.Uint64
}

type httpProxyRequestMetrics struct {
	metrics     *httpProxyRuntimeMetrics
	observed    bool
	streaming   bool
	requestBody uint64
}

func (m *httpProxyRuntimeMetrics) begin() httpProxyRequestMetrics {
	m.activeRequests.Add(1)
	return httpProxyRequestMetrics{metrics: m}
}

func (m *httpProxyRequestMetrics) observeRequest(streaming bool, requestBodyBytes int) {
	m.observed = true
	m.streaming = streaming
	if requestBodyBytes > 0 {
		m.requestBody = uint64(requestBodyBytes)
	}
}

func (m *httpProxyRequestMetrics) finish(status, responseBodyBytes int) {
	if m == nil || m.metrics == nil {
		return
	}
	metrics := m.metrics
	metrics.activeRequests.Add(-1)
	metrics.completedRequests.Add(1)
	if m.observed {
		if m.streaming {
			metrics.streamingRequests.Add(1)
		} else {
			metrics.nonStreamingRequests.Add(1)
		}
	}
	metrics.requestBodyBytes.Add(m.requestBody)
	if responseBodyBytes > 0 {
		metrics.responseBodyBytes.Add(uint64(responseBodyBytes))
	}
	switch {
	case status >= 500:
		metrics.serverErrorResponses.Add(1)
	case status >= 400:
		metrics.clientErrorResponses.Add(1)
	default:
		metrics.nonErrorResponses.Add(1)
	}
}

func (m *httpProxyRuntimeMetrics) stats() httpProxyRuntimeStats {
	if m == nil {
		return httpProxyRuntimeStats{}
	}
	return httpProxyRuntimeStats{
		ActiveRequests:       m.activeRequests.Load(),
		CompletedRequests:    m.completedRequests.Load(),
		NonErrorResponses:    m.nonErrorResponses.Load(),
		ClientErrorResponses: m.clientErrorResponses.Load(),
		ServerErrorResponses: m.serverErrorResponses.Load(),
		StreamingRequests:    m.streamingRequests.Load(),
		NonStreamingRequests: m.nonStreamingRequests.Load(),
		RequestBodyBytes:     m.requestBodyBytes.Load(),
		ResponseBodyBytes:    m.responseBodyBytes.Load(),
	}
}
