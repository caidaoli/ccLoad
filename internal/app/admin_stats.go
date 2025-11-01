package app

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// ==================== 统计和监控 ====================
// 从admin.go拆分统计监控,遵循SRP原则

// handleErrors 获取错误日志列表
// GET /admin/errors?hours=24&limit=100&offset=0
func (s *Server) handleErrors(c *gin.Context) {
	params := ParsePaginationParams(c)
	lf := BuildLogFilter(c)

	since := params.GetSinceTime()

	// 并行查询日志列表和总数（优化性能）
	logs, err := s.store.ListLogs(c.Request.Context(), since, params.Limit, params.Offset, &lf)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	total, err := s.store.CountLogs(c.Request.Context(), since, &lf)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 返回包含总数的响应（支持前端精确分页）
	RespondJSON(c, http.StatusOK, map[string]any{
		"data":  logs,
		"total": total,
	})
}

// handleMetrics 获取聚合指标数据
// GET /admin/metrics?hours=24&bucket_min=5
func (s *Server) handleMetrics(c *gin.Context) {
	params := ParsePaginationParams(c)
	bucketMin, _ := strconv.Atoi(c.DefaultQuery("bucket_min", "5"))
	if bucketMin <= 0 {
		bucketMin = 5
	}

	since := params.GetSinceTime()
	pts, err := s.store.Aggregate(c.Request.Context(), since, time.Duration(bucketMin)*time.Minute)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 添加调试信息
	totalReqs := 0
	for _, pt := range pts {
		totalReqs += pt.Success + pt.Error
	}

	c.Header("X-Debug-Since", since.Format(time.RFC3339))
	c.Header("X-Debug-Points", fmt.Sprintf("%d", len(pts)))
	c.Header("X-Debug-Total", fmt.Sprintf("%d", totalReqs))

	RespondJSON(c, http.StatusOK, pts)
}

// handleStats 获取渠道和模型统计
// GET /admin/stats?hours=24&channel_name_like=xxx&model_like=xxx
func (s *Server) handleStats(c *gin.Context) {
	params := ParsePaginationParams(c)
	lf := BuildLogFilter(c)

	since := params.GetSinceTime()
	stats, err := s.store.GetStats(c.Request.Context(), since, &lf)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	RespondJSON(c, http.StatusOK, gin.H{"stats": stats})
}

// handlePublicSummary 获取基础统计摘要(公开端点,无需认证)
// GET /public/summary?hours=24
func (s *Server) handlePublicSummary(c *gin.Context) {
	params := ParsePaginationParams(c)
	since := params.GetSinceTime()
	stats, err := s.store.GetStats(c.Request.Context(), since, nil) // 不使用过滤条件
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 计算总体统计
	totalSuccess := 0
	totalError := 0
	totalChannels := make(map[string]bool)
	totalModels := make(map[string]bool)

	for _, stat := range stats {
		totalSuccess += stat.Success
		totalError += stat.Error
		totalChannels[stat.ChannelName] = true
		totalModels[stat.Model] = true
	}

	response := gin.H{
		"total_requests":   totalSuccess + totalError,
		"success_requests": totalSuccess,
		"error_requests":   totalError,
		"active_channels":  len(totalChannels),
		"active_models":    len(totalModels),
		"hours":            params.Hours,
	}

	RespondJSON(c, http.StatusOK, response)
}

// handleCooldownStats 获取当前冷却状态监控指标
// GET /admin/cooldown/stats
// ✅ Linus风格:按需查询,简单直接
func (s *Server) handleCooldownStats(c *gin.Context) {
	// 使用缓存层查询（<1ms vs 数据库查询5-10ms），若缓存不可用自动退化
	channelCooldowns, _ := s.getAllChannelCooldowns(c.Request.Context())
	keyCooldowns, _ := s.getAllKeyCooldowns(c.Request.Context())

	var keyCount int
	for _, m := range keyCooldowns {
		keyCount += len(m)
	}

	response := gin.H{
		"channel_cooldowns": len(channelCooldowns),
		"key_cooldowns":     keyCount,
	}
	RespondJSON(c, http.StatusOK, response)
}

// handleCacheStats 暴露缓存命中率等指标，方便监控采集
// GET /admin/cache/stats
func (s *Server) handleCacheStats(c *gin.Context) {
	cache := s.getChannelCache()
	if cache == nil {
		RespondJSON(c, http.StatusOK, gin.H{
			"cache_enabled": false,
			"stats":         gin.H{},
		})
		return
	}

	stats := cache.GetCacheStats()
	RespondJSON(c, http.StatusOK, gin.H{
		"cache_enabled": true,
		"stats":         stats,
	})
}

// handleGetChannelTypes 获取渠道类型配置(公开端点,前端动态加载)
// GET /public/channel-types
func (s *Server) handleGetChannelTypes(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"data": util.ChannelTypes,
	})
}
