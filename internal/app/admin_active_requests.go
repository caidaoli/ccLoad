package app

import (
	"net/http"
	"strconv"

	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

// HandleActiveRequests 返回当前进行中的请求列表（内存状态，不持久化）
func (s *Server) HandleActiveRequests(c *gin.Context) {
	var requests []*ActiveRequest
	if s.activeRequests != nil {
		requests = s.activeRequests.List()
	}
	RespondJSONWithCount(c, http.StatusOK, requests, len(requests))
}

// HandleRuntimeMetrics 返回进程内长连接资源。
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
	data := gin.H{"responses_websocket": stats}
	if provider, ok := s.store.(storage.HybridRuntimeMetricsProvider); ok {
		data["storage"] = provider.RuntimeMetrics()
	}
	if s.logService != nil {
		data["logs"] = s.logService.runtimeMetrics()
	}
	RespondJSON(c, http.StatusOK, data)
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
