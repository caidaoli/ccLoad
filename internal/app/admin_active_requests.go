package app

import (
	"net/http"
	"runtime"
	"strconv"
	"time"

	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

type activeRequestsResponse struct {
	Success                   bool             `json:"success"`
	Data                      []*ActiveRequest `json:"data"`
	Error                     string           `json:"error"`
	Count                     int              `json:"count"`
	ActiveRequestTitleEnabled bool             `json:"active_request_title_enabled"`
}

type processRuntimeMetrics struct {
	UptimeSeconds         int64   `json:"uptime_seconds"`
	ConcurrencySlotsInUse int     `json:"concurrency_slots_in_use"`
	MaxConcurrency        int     `json:"max_concurrency"`
	Goroutines            int     `json:"goroutines"`
	CPUUsagePercent       float64 `json:"cpu_usage_percent"`
	CPUUserSeconds        float64 `json:"cpu_user_seconds"`
	CPUSystemSeconds      float64 `json:"cpu_system_seconds"`
	RSSBytes              uint64  `json:"rss_bytes"`
	MaxRSSBytes           uint64  `json:"max_rss_bytes"`
	HeapAllocBytes        uint64  `json:"heap_alloc_bytes"`
	HeapSysBytes          uint64  `json:"heap_sys_bytes"`
	GCCount               uint32  `json:"gc_count"`
	GCPauseTotalNs        uint64  `json:"gc_pause_total_ns"`
	GCCPUPercent          float64 `json:"gc_cpu_percent"`
}

func (s *Server) processRuntimeMetrics(now time.Time) processRuntimeMetrics {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)

	uptime := time.Duration(0)
	if !s.startedAt.IsZero() && now.After(s.startedAt) {
		uptime = now.Sub(s.startedAt)
	}
	metrics := processRuntimeMetrics{
		UptimeSeconds:         int64(uptime.Seconds()),
		ConcurrencySlotsInUse: s.activeRequestCount(),
		MaxConcurrency:        s.maxConcurrency,
		Goroutines:            runtime.NumGoroutine(),
		RSSBytes:              readCurrentRSSBytes(),
		HeapAllocBytes:        memory.HeapAlloc,
		HeapSysBytes:          memory.HeapSys,
		GCCount:               memory.NumGC,
		GCPauseTotalNs:        memory.PauseTotalNs,
		GCCPUPercent:          memory.GCCPUFraction * 100,
	}
	if user, system, maxRSS, ok := readProcessRusage(); ok {
		metrics.CPUUserSeconds = user
		metrics.CPUSystemSeconds = system
		metrics.MaxRSSBytes = maxRSS
		metrics.CPUUsagePercent = s.cpuUsage.percent(user+system, now, uptime.Seconds())
	}
	return metrics
}

// HandleActiveRequests 返回当前进行中的请求列表（内存状态，不持久化）
func (s *Server) HandleActiveRequests(c *gin.Context) {
	var requests []*ActiveRequest
	if s.activeRequests != nil {
		requests = s.activeRequests.List()
	}
	c.JSON(http.StatusOK, activeRequestsResponse{
		Success:                   true,
		Data:                      requests,
		Count:                     len(requests),
		ActiveRequestTitleEnabled: s.activeRequestTitleEnabled,
	})
}

// HandleRuntimeMetrics 返回当前 ccLoad 进程的运行状态。
func (s *Server) HandleRuntimeMetrics(c *gin.Context) {
	stats := responsesExecutionSessionStoreStats{}
	if s.responsesExecutionSessions != nil {
		stats = s.responsesExecutionSessions.stats()
	}
	if s.responsesWebsocketConnections != nil {
		connections := s.responsesWebsocketConnections.stats()
		stats.DownstreamConnections = connections.Active
		stats.RejectedDownstreamConnections = connections.Rejected
		stats.MaxDownstreamConnections = connections.Max
		stats.MaxDownstreamConnectionsPerToken = connections.MaxPerSubject
	}
	data := gin.H{
		"process":             s.processRuntimeMetrics(time.Now()),
		"http_proxy":          s.httpRuntime.stats(),
		"responses_websocket": stats,
	}
	if provider, ok := s.store.(storage.HybridRuntimeMetricsProvider); ok {
		data["storage"] = provider.RuntimeMetrics()
	}
	if s.logService != nil {
		data["logs"] = s.logService.runtimeMetrics()
	}
	RespondJSON(c, http.StatusOK, data)
}

// HandleAbortActiveRequest 中断运行中请求当前的上游尝试。
// POST /admin/active-requests/:request_id/abort
//
// 中断按「上游连接被重置」处理，因此后续行为完全由既有的网络故障链路决定：
// 上游尚未提交响应时切换到下一个渠道，已经在向客户端输出时按流中断收尾。
func (s *Server) HandleAbortActiveRequest(c *gin.Context) {
	requestID, err := strconv.ParseInt(c.Param("request_id"), 10, 64)
	if err != nil || requestID <= 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request_id")
		return
	}

	if s.activeRequests == nil || !s.activeRequests.Abort(requestID) {
		RespondErrorMsg(c, http.StatusNotFound, "active request not found or not abortable")
		return
	}

	RespondJSON(c, http.StatusOK, gin.H{"aborted": true})
}

// HandleGetActiveRequestDebugLog 返回运行中请求的调试日志快照。
// GET /admin/active-requests/:request_id/debug-log
func (s *Server) HandleGetActiveRequestDebugLog(c *gin.Context) {
	requestIDStr := c.Param("request_id")
	requestID, err := strconv.ParseInt(requestIDStr, 10, 64)
	if err != nil || requestID <= 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request_id")
		return
	}

	if s.activeRequests == nil {
		RespondErrorWithData(c, http.StatusNotFound, "debug log unavailable", s.buildDebugLogUnavailableInfo(c.Request.Context()))
		return
	}

	entry, ok := s.activeRequests.GetDebugLogSnapshot(requestID)
	if !ok || entry == nil {
		RespondErrorWithData(c, http.StatusNotFound, "debug log unavailable", s.buildDebugLogUnavailableInfo(c.Request.Context()))
		return
	}

	RespondJSON(c, http.StatusOK, debugLogResponse(entry))
}
