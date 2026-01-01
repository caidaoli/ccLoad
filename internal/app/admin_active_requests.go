package app

import (
	"net/http"

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
