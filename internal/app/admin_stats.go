package app

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"ccLoad/internal/version"

	"github.com/gin-gonic/gin"
)

// ==================== 统计和监控 ====================
// 从admin.go拆分统计监控,遵循SRP原则

// HandleErrors 获取日志列表
// GET /admin/logs?range=today&limit=100&offset=0
func (s *Server) HandleErrors(c *gin.Context) {
	params := ParsePaginationParams(c)
	lf := BuildLogFilter(c)
	since, until := params.GetTimeRange()

	// 并行查询日志列表和总数（优化性能）
	logs, err := s.store.ListLogsRange(c.Request.Context(), since, until, params.Limit, params.Offset, &lf)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	total, err := s.store.CountLogsRange(c.Request.Context(), since, until, &lf)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 返回包含总数的响应（支持前端精确分页）
	RespondJSONWithCount(c, http.StatusOK, logs, total)
}

// HandleMetrics 获取聚合指标数据
// GET /admin/metrics?range=today&bucket_min=5&channel_type=anthropic&model=claude-3-5-sonnet-20241022&channel_id=1&channel_name_like=xxx
func (s *Server) HandleMetrics(c *gin.Context) {
	params := ParsePaginationParams(c)
	bucketMin, _ := strconv.Atoi(c.DefaultQuery("bucket_min", "5"))
	if bucketMin <= 0 {
		bucketMin = 5
	}

	// 使用统一的筛选参数构建器（支持 channel_type、channel_id、channel_name_like、model、auth_token_id）
	lf := BuildLogFilter(c)

	since, until := params.GetTimeRange()
	pts, err := s.store.AggregateRangeWithFilter(c.Request.Context(), since, until, time.Duration(bucketMin)*time.Minute, &lf)

	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	RespondJSON(c, http.StatusOK, pts)
}

// HandleStats 获取渠道和模型统计
// GET /admin/stats?range=today&channel_name_like=xxx&model_like=xxx
func (s *Server) HandleStats(c *gin.Context) {
	params := ParsePaginationParams(c)
	lf := BuildLogFilter(c)

	startTime, endTime := params.GetTimeRange()

	// 判断是否为本日（本日才计算最近一分钟）
	isToday := params.Range == "today" || params.Range == ""

	stats, err := s.statsCache.GetStats(c.Request.Context(), startTime, endTime, &lf, isToday)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 计算时间跨度（秒），用于前端计算RPM和QPS
	durationSeconds := endTime.Sub(startTime).Seconds()
	if durationSeconds < 1 {
		durationSeconds = 1 // 防止除零
	}

	// 获取RPM统计（峰值、平均、最近一分钟）
	rpmStats, err := s.statsCache.GetRPMStats(c.Request.Context(), startTime, endTime, &lf, isToday)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 计算健康时间线（固定48个时间点，当日显示最近4小时）
	s.fillHealthTimeline(c.Request.Context(), stats, startTime, endTime, &lf, isToday)

	RespondJSON(c, http.StatusOK, gin.H{
		"stats":            stats,
		"duration_seconds": durationSeconds,
		"rpm_stats":        rpmStats,
		"is_today":         isToday,
	})
}

// HandlePublicSummary 获取基础统计摘要(公开端点,无需认证)
// GET /public/summary?range=today
// 按渠道类型分组统计，Claude和Codex类型包含Token和成本信息
//
// [SECURITY NOTE] 该端点故意设计为公开访问，用于首页仪表盘展示。
// 如需隐藏运营数据，可在 server.go:SetupRoutes 中添加 RequireTokenAuth 中间件。
func (s *Server) HandlePublicSummary(c *gin.Context) {
	params := ParsePaginationParams(c)
	startTime, endTime := params.GetTimeRange()

	// 判断是否为本日（本日才计算最近一分钟）
	isToday := params.Range == "today" || params.Range == ""
	ctx := c.Request.Context()

	// [OPT] P1: 并行执行三个独立查询
	var (
		stats        []model.StatsEntry
		rpmStats     *model.RPMStats
		channelTypes map[int64]string
		statsErr     error
		rpmErr       error
		typesErr     error
		wg           sync.WaitGroup
	)

	wg.Add(3)

	// 查询1: 基础统计（使用 Lite 版本跳过 fillStatsRPM）
	go func() {
		defer wg.Done()
		stats, statsErr = s.statsCache.GetStatsLite(ctx, startTime, endTime, nil)
	}()

	// 查询2: RPM统计
	go func() {
		defer wg.Done()
		rpmStats, rpmErr = s.statsCache.GetRPMStats(ctx, startTime, endTime, nil, isToday)
	}()

	// 查询3: 渠道类型映射（带缓存）
	go func() {
		defer wg.Done()
		channelTypes, typesErr = s.getChannelTypesMapCached(ctx)
	}()

	wg.Wait()

	// 错误处理
	if statsErr != nil {
		RespondError(c, http.StatusInternalServerError, statsErr)
		return
	}
	if rpmErr != nil {
		RespondError(c, http.StatusInternalServerError, rpmErr)
		return
	}
	if typesErr != nil {
		RespondError(c, http.StatusInternalServerError, typesErr)
		return
	}

	// 计算时间跨度（秒），用于前端计算RPM和QPS
	durationSeconds := endTime.Sub(startTime).Seconds()
	if durationSeconds < 1 {
		durationSeconds = 1 // 防止除零
	}

	// 按渠道类型分组统计
	typeStats := make(map[string]*TypeSummary)
	totalSuccess := 0
	totalError := 0

	for _, stat := range stats {
		// 获取渠道类型，跳过无法确定类型的记录（已删除的渠道）
		var channelType string
		if stat.ChannelID != nil {
			if ct, ok := channelTypes[int64(*stat.ChannelID)]; ok {
				channelType = ct
			}
		}
		if channelType == "" {
			// 渠道已删除或类型未知，不计入按类型统计（与 /admin/stats 保持一致）
			continue
		}

		totalSuccess += stat.Success
		totalError += stat.Error

		// 初始化类型统计
		if _, exists := typeStats[channelType]; !exists {
			typeStats[channelType] = &TypeSummary{
				ChannelType:     channelType,
				TotalRequests:   0,
				SuccessRequests: 0,
				ErrorRequests:   0,
			}
		}

		ts := typeStats[channelType]
		ts.TotalRequests += stat.Success + stat.Error
		ts.SuccessRequests += stat.Success
		ts.ErrorRequests += stat.Error

		// 所有渠道类型都统计Token和成本
		if stat.TotalInputTokens != nil {
			ts.TotalInputTokens += *stat.TotalInputTokens
		}
		if stat.TotalOutputTokens != nil {
			ts.TotalOutputTokens += *stat.TotalOutputTokens
		}
		if stat.TotalCost != nil {
			ts.TotalCost += *stat.TotalCost
		}

		// Claude和Codex类型额外统计缓存（其他类型不支持prompt caching）
		if channelType == "anthropic" || channelType == "codex" {
			if stat.TotalCacheReadInputTokens != nil {
				ts.TotalCacheReadTokens += *stat.TotalCacheReadInputTokens
			}
			if stat.TotalCacheCreationInputTokens != nil {
				ts.TotalCacheCreationTokens += *stat.TotalCacheCreationInputTokens
			}
		}
	}

	response := gin.H{
		"total_requests":   totalSuccess + totalError,
		"success_requests": totalSuccess,
		"error_requests":   totalError,
		"range":            params.Range,
		"duration_seconds": durationSeconds,
		"rpm_stats":        rpmStats,
		"is_today":         isToday,
		"by_type":          typeStats, // 按渠道类型分组的统计
	}

	RespondJSON(c, http.StatusOK, response)
}

// TypeSummary 按渠道类型的统计摘要
type TypeSummary struct {
	ChannelType              string  `json:"channel_type"`
	TotalRequests            int     `json:"total_requests"`
	SuccessRequests          int     `json:"success_requests"`
	ErrorRequests            int     `json:"error_requests"`
	TotalInputTokens         int64   `json:"total_input_tokens,omitempty"`          // 所有类型
	TotalOutputTokens        int64   `json:"total_output_tokens,omitempty"`         // 所有类型
	TotalCacheReadTokens     int64   `json:"total_cache_read_tokens,omitempty"`     // Claude/Codex专用（prompt caching）
	TotalCacheCreationTokens int64   `json:"total_cache_creation_tokens,omitempty"` // Claude/Codex专用（prompt caching）
	TotalCost                float64 `json:"total_cost,omitempty"`                  // 所有类型
}

// fetchChannelTypesMap 查询所有渠道的类型映射
func (s *Server) fetchChannelTypesMap(ctx context.Context) (map[int64]string, error) {
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}

	channelTypes := make(map[int64]string, len(configs))
	for _, cfg := range configs {
		channelTypes[cfg.ID] = cfg.ChannelType
	}
	return channelTypes, nil
}

// getChannelTypesMapCached 带 TTL 缓存的渠道类型映射查询
// [OPT] P3: 渠道类型变化频率极低，使用 60 秒缓存减少数据库查询
const channelTypesCacheTTL = 60 * time.Second

func (s *Server) getChannelTypesMapCached(ctx context.Context) (map[int64]string, error) {
	// 读锁检查缓存
	s.channelTypesCacheMu.RLock()
	if s.channelTypesCache != nil && time.Since(s.channelTypesCacheTime) < channelTypesCacheTTL {
		result := s.channelTypesCache
		s.channelTypesCacheMu.RUnlock()
		return result, nil
	}
	s.channelTypesCacheMu.RUnlock()

	// 写锁更新缓存
	s.channelTypesCacheMu.Lock()
	defer s.channelTypesCacheMu.Unlock()

	// 双重检查：可能其他 goroutine 已更新
	if s.channelTypesCache != nil && time.Since(s.channelTypesCacheTime) < channelTypesCacheTTL {
		return s.channelTypesCache, nil
	}

	channelTypes, err := s.fetchChannelTypesMap(ctx)
	if err != nil {
		return nil, err
	}

	s.channelTypesCache = channelTypes
	s.channelTypesCacheTime = time.Now()
	return channelTypes, nil
}

// HandleCooldownStats 获取当前冷却状态监控指标
// GET /admin/cooldown/stats
func (s *Server) HandleCooldownStats(c *gin.Context) {
	// 优先走缓存层，缓存不可用时自动降级到数据库查询
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

// HandleGetChannelTypes 获取渠道类型配置(公开端点,前端动态加载)
// GET /public/channel-types
func (s *Server) HandleGetChannelTypes(c *gin.Context) {
	RespondJSON(c, http.StatusOK, util.ChannelTypes)
}

// HandlePublicVersion 获取当前版本信息(公开端点,前端显示版本)
// GET /public/version
func (s *Server) HandlePublicVersion(c *gin.Context) {
	hasUpdate, latestVersion, releaseURL := version.GetUpdateInfo()
	RespondJSON(c, http.StatusOK, gin.H{
		"version":        version.Version,
		"has_update":     hasUpdate,
		"latest_version": latestVersion,
		"release_url":    releaseURL,
	})
}

// HandleGetModels 获取数据库中存在的所有模型列表（去重）
// GET /admin/models
// 支持参数：range（时间范围）、channel_type（渠道类型筛选）
func (s *Server) HandleGetModels(c *gin.Context) {
	// 获取时间范围（默认最近30天）
	rangeParam := c.DefaultQuery("range", "this_month")
	params := ParsePaginationParams(c)
	params.Range = rangeParam
	since, until := params.GetTimeRange()

	// 获取渠道类型筛选（可选）
	channelType := c.Query("channel_type")

	// 查询模型列表
	models, err := s.store.GetDistinctModels(c.Request.Context(), since, until, channelType)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	RespondJSON(c, http.StatusOK, models)
}

// HandleHealth 健康检查端点(公开访问,无需认证)
// GET /health
// 仅检查数据库连接是否活跃（适用于K8s liveness/readiness probe）
func (s *Server) HandleHealth(c *gin.Context) {
	// 设置100ms超时，避免慢查询阻塞healthcheck
	ctx, cancel := context.WithTimeout(c.Request.Context(), 100*time.Millisecond)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		RespondError(c, http.StatusServiceUnavailable, err)
		return
	}

	RespondJSON(c, http.StatusOK, gin.H{"status": "ok"})
}

// fillHealthTimeline 为每个统计条目填充健康时间线
// isToday=true: 显示最近4小时，每5分钟一个状态（48个）
// isToday=false: 按总时间跨度/48计算时间桶
func (s *Server) fillHealthTimeline(ctx context.Context, stats []model.StatsEntry, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) {
	if len(stats) == 0 {
		return
	}

	const numBuckets = 48

	// 计算健康指示器的时间范围和桶大小
	var healthStart time.Time
	var bucketSeconds int64

	if isToday {
		// 当日：最近4小时，每5分钟一个桶
		bucketSeconds = 5 * 60 // 5分钟
		healthStart = endTime.Add(-4 * time.Hour)
		// 确保不早于查询开始时间
		if healthStart.Before(startTime) {
			healthStart = startTime
		}
	} else {
		// 其他时间范围：按总时长/48计算
		duration := endTime.Sub(startTime)
		bucketSeconds = int64(duration.Seconds() / numBuckets)
		if bucketSeconds < 1 {
			bucketSeconds = 1
		}
		healthStart = startTime
	}

	// 转换为毫秒，直接与 logs.time 比较，避免索引失效
	sinceMs := healthStart.UnixMilli()
	untilMs := endTime.UnixMilli()
	bucketMs := bucketSeconds * 1000

	// 构建结构化查询参数（SQL 构建已下沉到存储层）
	params := model.HealthTimelineParams{
		SinceMs:  sinceMs,
		UntilMs:  untilMs,
		BucketMs: bucketMs,
	}
	if filter != nil {
		if filter.ChannelID != nil && *filter.ChannelID > 0 {
			params.ChannelID = filter.ChannelID
		}
		if filter.Model != "" {
			params.Model = filter.Model
		}
	}

	rows, err := s.store.GetHealthTimeline(ctx, params)
	if err != nil {
		// 静默失败，不影响主流程
		return
	}

	// 构建映射：(channel_id, model) -> StatsEntry索引
	type channelModelKey struct {
		channelID int
		model     string
	}
	statsMap := make(map[channelModelKey]int)
	for i := range stats {
		if stats[i].ChannelID != nil {
			key := channelModelKey{
				channelID: *stats[i].ChannelID,
				model:     stats[i].Model,
			}
			statsMap[key] = i
		}
	}

	// 解析查询结果 - 按时间桶索引位置填充
	timeline := make(map[channelModelKey][]model.HealthPoint)

	sinceUnix := healthStart.Unix()

	// 为每个渠道初始化48个空时间点
	for key := range statsMap {
		points := make([]model.HealthPoint, numBuckets)
		for i := 0; i < numBuckets; i++ {
			points[i] = model.HealthPoint{
				Ts:          time.Unix(sinceUnix+int64(i)*bucketSeconds, 0),
				SuccessRate: -1, // -1 表示无数据
			}
		}
		timeline[key] = points
	}

	for _, row := range rows {
		key := channelModelKey{channelID: row.ChannelID, model: row.Model}

		// 只处理 stats 中存在的组合
		if _, exists := statsMap[key]; !exists {
			continue
		}

		// 计算该时间桶对应的索引位置（BucketTs 是毫秒，需转换为秒再计算）
		bucketIndex := int((row.BucketTs/1000 - sinceUnix) / bucketSeconds)
		if bucketIndex < 0 || bucketIndex >= numBuckets {
			continue
		}

		total := row.Success + row.ErrorCount
		successRate := 0.0
		if total > 0 {
			successRate = float64(row.Success) / float64(total)
		}

		// duration/first_byte_time 在日志中以"秒"存储，这里直接透传
		timeline[key][bucketIndex] = model.HealthPoint{
			Ts:                       time.Unix(row.BucketTs/1000, 0),
			SuccessRate:              successRate,
			SuccessCount:             row.Success,
			ErrorCount:               row.ErrorCount,
			AvgFirstByteTime:         row.AvgFirstByteTime,
			AvgDuration:              row.AvgDuration,
			TotalInputTokens:         row.InputTokens,
			TotalOutputTokens:        row.OutputTokens,
			TotalCacheReadTokens:     row.CacheReadTokens,
			TotalCacheCreationTokens: row.CacheCreationTokens,
			TotalCost:                row.TotalCost,
		}
	}

	// 填充到 stats 中
	for key, idx := range statsMap {
		if points, exists := timeline[key]; exists {
			stats[idx].HealthTimeline = points
		}
	}
}
