package app

import (
	"context"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
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

	logs, total, err := s.store.ListLogsRangeWithCount(c.Request.Context(), since, until, params.Limit, params.Offset, &lf)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	if isAPITokenWebRequest(c) {
		channels, err := s.tokenLogChannels(c.Request.Context(), logs)
		if err != nil {
			log.Printf("[ERROR] 加载 API Token 日志脱敏元数据失败: %v", err)
			RespondErrorMsg(c, http.StatusInternalServerError, "读取日志脱敏元数据失败")
			return
		}
		projected := projectTokenLogs(logs, channels, s.logModelPrices(c.Request.Context(), logs))
		if hideTokenChannels(c) {
			for i := range projected {
				projected[i].ChannelID = 0
				projected[i].ChannelName = ""
				// Hide both model sources used by the UI, including names in error summaries.
				for _, name := range []string{projected[i].ActualModel, projected[i].ResponseModel} {
					if name != "" && name != projected[i].Model {
						projected[i].Message = strings.ReplaceAll(projected[i].Message, name, "[redacted]")
					}
				}
				projected[i].ActualModel = ""
				projected[i].ResponseModel = ""
			}
		}
		RespondJSONWithCount(c, http.StatusOK, projected, total)
		return
	}
	RespondJSONWithCount(c, http.StatusOK, projectDashboardLogs(logs, s.logModelPrices(c.Request.Context(), logs)), total)
}

func (s *Server) tokenLogChannels(ctx context.Context, logs []*model.LogEntry) (map[int64]tokenLogChannelMetadata, error) {
	needed := make(map[int64]struct{})
	for _, entry := range logs {
		if entry != nil && entry.ChannelID > 0 {
			needed[entry.ChannelID] = struct{}{}
		}
	}
	channels := make(map[int64]tokenLogChannelMetadata, len(needed))
	if len(needed) == 0 {
		return channels, nil
	}
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}
	apiKeysByChannel, err := s.store.GetAllAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, cfg := range configs {
		if _, ok := needed[cfg.ID]; !ok {
			continue
		}
		metadata := tokenLogChannelMetadata{
			APIKeyHashes: make(map[string]struct{}, len(apiKeysByChannel[cfg.ID])),
		}
		for _, apiKey := range apiKeysByChannel[cfg.ID] {
			if apiKey != nil && apiKey.APIKey != "" {
				metadata.APIKeys = append(metadata.APIKeys, apiKey.APIKey)
				metadata.APIKeyHashes[util.HashAPIKey(apiKey.APIKey)] = struct{}{}
			}
		}
		channels[cfg.ID] = metadata
	}
	return channels, nil
}

// HandleMetrics 获取聚合指标数据；by_channel=1 时附带逐渠道拆分
// GET /admin/metrics?range=today&bucket_min=5&by_channel=1&upstream_protocol=anthropic&model=claude-3-5-sonnet-20241022&channel_id=1&channel_name_like=xxx
func (s *Server) HandleMetrics(c *gin.Context) {
	params := ParsePaginationParams(c)
	bucketMin, _ := strconv.Atoi(c.DefaultQuery("bucket_min", "5"))
	if bucketMin <= 0 {
		bucketMin = 5
	}

	// 使用统一的筛选参数构建器。
	lf := BuildLogFilter(c)
	lf.LogSource = model.LogSourceProxy

	since, until := params.GetTimeRange()
	pts, err := s.statsCache.AggregateRangeWithFilter(c.Request.Context(), since, until, time.Duration(bucketMin)*time.Minute, &lf)

	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	// 逐渠道拆分只有趋势页的渠道筛选使用，其他调用方只取总量。
	// 缓存结果在请求间共享，就地修改前先复制。
	if c.Query("by_channel") != "1" || hideTokenChannels(c) {
		pts = slices.Clone(pts)
		for i := range pts {
			pts[i].Channels = nil
		}
	}
	RespondJSON(c, http.StatusOK, pts)
}

// HandleStats 获取渠道和模型统计；health_timeline=1 时附带健康时间线
// GET /admin/stats?range=today&channel_name_like=xxx&model_like=xxx&health_timeline=1
func (s *Server) HandleStats(c *gin.Context) {
	params := ParsePaginationParams(c)
	lf := BuildLogFilter(c)
	lf.LogSource = model.LogSourceProxy

	startTime, endTime := params.GetTimeRange()

	// 判断是否为本日（本日才计算最近一分钟）
	isToday := params.Range == "today" || params.Range == ""

	// 统计、RPM（峰值、平均、最近一分钟）与健康时间线互不依赖，大范围时都是秒级，并行查询。
	// 健康时间线需要额外的分桶查询，只有统计页的健康条使用。
	ctx := c.Request.Context()
	withTimeline := c.Query("health_timeline") == "1"
	var (
		stats          []model.StatsEntry
		rpmStats       *model.RPMStats
		timeline       []model.HealthTimelineRow
		timelineParams model.HealthTimelineParams
		statsErr       error
		rpmErr         error
		timelineErr    error
		wg             sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		stats, statsErr = s.statsCache.GetStats(ctx, startTime, endTime, &lf, isToday)
	}()
	go func() {
		defer wg.Done()
		rpmStats, rpmErr = s.statsCache.GetRPMStats(ctx, startTime, endTime, &lf, isToday)
	}()
	if withTimeline {
		timelineParams = healthTimelineParams(startTime, endTime, &lf, isToday)
		wg.Add(1)
		go func() {
			defer wg.Done()
			timeline, timelineErr = s.statsCache.GetHealthTimeline(ctx, timelineParams)
		}()
	}
	wg.Wait()

	if statsErr != nil {
		RespondError(c, http.StatusInternalServerError, statsErr)
		return
	}
	if rpmErr != nil {
		RespondError(c, http.StatusInternalServerError, rpmErr)
		return
	}
	// 缓存结果在请求间共享，就地修改前先复制
	if isAPITokenWebRequest(c) {
		stats = projectTokenStats(stats)
	} else if withTimeline {
		stats = slices.Clone(stats)
	}

	// 计算时间跨度（秒），用于前端计算RPM和QPS
	durationSeconds := endTime.Sub(startTime).Seconds()
	if durationSeconds < 1 {
		durationSeconds = 1 // 防止除零
	}

	// 时间线查询失败时静默跳过，不影响主流程
	if withTimeline && timelineErr == nil {
		fillHealthTimeline(stats, timelineParams, timeline)
	}
	if hideTokenChannels(c) {
		for i := range stats {
			stats[i].ChannelID = nil
			stats[i].ChannelName = ""
		}
	}

	RespondJSON(c, http.StatusOK, gin.H{
		"stats":            stats,
		"duration_seconds": durationSeconds,
		"rpm_stats":        rpmStats,
		"is_today":         isToday,
	})
}

func projectTokenStats(stats []model.StatsEntry) []model.StatsEntry {
	projected := make([]model.StatsEntry, len(stats))
	copy(projected, stats)
	for i := range projected {
		projected[i].LastRequestMessage = ""
		projected[i].ChannelPriority = nil
		projected[i].CostMultiplierMin = nil
		projected[i].CostMultiplierMax = nil
	}
	return projected
}

// HandlePublicSummary 获取基础统计摘要(公开端点,无需认证)
// GET /public/summary?range=today
// 按客户端入口协议和渠道认证类型分组统计，附带 RPM 与时间跨度。
//
// [SECURITY NOTE] 该端点故意设计为公开访问。
// 认证仪表盘使用 /dashboard/summary，并由 Web 身份强制作用域。
func (s *Server) HandlePublicSummary(c *gin.Context) {
	s.respondSummary(c, &model.LogFilter{LogSource: model.LogSourceProxy}, true)
}

// HandleDashboardSummary 返回首页概览所需的协议与认证类型汇总。
// GET /dashboard/summary?range=today
// 首页不展示 RPM，跳过 RPM 查询；统计范围由 Web 身份强制作用域。
func (s *Server) HandleDashboardSummary(c *gin.Context) {
	filter := BuildLogFilter(c)
	filter.LogSource = model.LogSourceProxy
	s.respondSummary(c, &filter, false)
}

func (s *Server) respondSummary(c *gin.Context, logFilter *model.LogFilter, includeRPM bool) {
	params := ParsePaginationParams(c)
	startTime, endTime := params.GetTimeRange()

	// 判断是否为本日（本日才计算最近一分钟）
	isToday := params.Range == "today" || params.Range == ""
	ctx := c.Request.Context()

	// 协议摘要、认证类型摘要与 RPM 相互独立，并行查询。
	var (
		stats        []model.ClientProtocolStats
		authStats    []model.AuthTypeStats
		rpmStats     *model.RPMStats
		statsErr     error
		authStatsErr error
		rpmErr       error
		wg           sync.WaitGroup
	)

	wg.Add(2)

	go func() {
		defer wg.Done()
		stats, statsErr = s.statsCache.GetClientProtocolStats(ctx, startTime, endTime, logFilter)
	}()

	go func() {
		defer wg.Done()
		authStats, authStatsErr = s.statsCache.GetAuthTypeStats(ctx, startTime, endTime, logFilter)
	}()

	if includeRPM {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rpmStats, rpmErr = s.statsCache.GetRPMStats(ctx, startTime, endTime, logFilter, isToday)
		}()
	}

	wg.Wait()

	if statsErr != nil {
		RespondError(c, http.StatusInternalServerError, statsErr)
		return
	}
	if authStatsErr != nil {
		RespondError(c, http.StatusInternalServerError, authStatsErr)
		return
	}
	if rpmErr != nil {
		RespondError(c, http.StatusInternalServerError, rpmErr)
		return
	}

	byClientProtocol := make(map[string]model.ClientProtocolStats)
	totalSuccess := 0
	totalError := 0

	for _, stat := range stats {
		totalSuccess += stat.SuccessRequests
		totalError += stat.ErrorRequests

		if protocol.IsValid(protocol.Protocol(stat.ClientProtocol)) {
			byClientProtocol[stat.ClientProtocol] = stat
		}
	}

	byAuthType := make(map[string]model.AuthTypeStats, len(authStats))
	for _, stat := range authStats {
		byAuthType[stat.AuthType] = stat
	}

	response := gin.H{
		"total_requests":     totalSuccess + totalError,
		"success_requests":   totalSuccess,
		"error_requests":     totalError,
		"by_client_protocol": byClientProtocol,
		"by_auth_type":       byAuthType,
	}
	if includeRPM {
		// 计算时间跨度（秒），用于前端计算RPM和QPS
		durationSeconds := endTime.Sub(startTime).Seconds()
		if durationSeconds < 1 {
			durationSeconds = 1 // 防止除零
		}
		response["range"] = params.Range
		response["duration_seconds"] = durationSeconds
		response["rpm_stats"] = rpmStats
		response["is_today"] = isToday
	}

	if hideTokenChannels(c) {
		delete(response, "by_auth_type")
	}
	RespondJSON(c, http.StatusOK, response)
}

// HandleGetProtocols 获取支持的协议列表(公开端点,前端动态加载)
// GET /public/protocols
// 编译时常量，浏览器缓存24小时减少HF Spaces等高延迟环境的网络往返
func (s *Server) HandleGetProtocols(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=86400")
	RespondJSON(c, http.StatusOK, protocol.AllProtocols())
}

// HandlePublicVersion 获取当前版本信息(公开端点,前端显示版本)
// GET /public/version
// 版本信息变化频率极低，缓存5分钟。
func (s *Server) HandlePublicVersion(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=300")
	state := version.UpdateState{}
	if s.updateManager != nil {
		state = s.updateManager.State()
	}
	RespondJSON(c, http.StatusOK, gin.H{
		"version":        version.Version,
		"has_update":     state.HasUpdate,
		"latest_version": state.LatestVersion,
		"release_url":    state.ReleaseURL,
	})
}

// ModelsChannelsResponse 日志筛选所需的模型、渠道和状态码列表响应。
type ModelsChannelsResponse struct {
	Models      []string              `json:"models"`
	Channels    []model.ChannelNameID `json:"channels"`
	StatusCodes []int                 `json:"status_codes"`
}

// HandleGetModels 获取数据库中有日志的模型、渠道和状态码列表（去重）。
// GET /admin/models
// 支持参数：range（时间范围）、upstream_protocol（实际上游协议筛选）
func (s *Server) HandleGetModels(c *gin.Context) {
	rangeParam := c.DefaultQuery("range", "this_month")
	params := ParsePaginationParams(c)
	params.Range = rangeParam
	since, until := params.GetTimeRange()

	logFilter := BuildLogFilter(c)
	logFilter.LogSource = model.LogSourceProxy

	var (
		models                              []string
		channels                            []model.ChannelNameID
		statusCodes                         []int
		wg                                  sync.WaitGroup
		modelsErr, channelsErr, statusesErr error
	)

	wg.Go(func() {
		models, modelsErr = s.store.GetDistinctModels(c.Request.Context(), since, until, &logFilter)
	})
	wg.Go(func() {
		channels, channelsErr = s.store.GetDistinctChannels(c.Request.Context(), since, until, &logFilter)
	})
	wg.Go(func() {
		statusCodes, statusesErr = s.store.GetDistinctStatusCodes(c.Request.Context(), since, until, &logFilter)
	})
	wg.Wait()

	if modelsErr != nil {
		RespondError(c, http.StatusInternalServerError, modelsErr)
		return
	}
	if channelsErr != nil {
		RespondError(c, http.StatusInternalServerError, channelsErr)
		return
	}
	if statusesErr != nil {
		RespondError(c, http.StatusInternalServerError, statusesErr)
		return
	}
	if models == nil {
		models = make([]string, 0)
	}
	if channels == nil {
		channels = make([]model.ChannelNameID, 0)
	}
	if statusCodes == nil {
		statusCodes = make([]int, 0)
	}

	if hideTokenChannels(c) {
		channels = make([]model.ChannelNameID, 0)
	}
	RespondJSON(c, http.StatusOK, ModelsChannelsResponse{Models: models, Channels: channels, StatusCodes: statusCodes})
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

// healthTimelineBuckets 健康时间线固定的时间桶数量
const healthTimelineBuckets = 48

// healthTimelineParams 计算健康时间线的查询窗口与桶大小
// isToday=true: 显示最近4小时，每5分钟一个状态（48个）
// isToday=false: 按总时间跨度/48计算时间桶
func healthTimelineParams(startTime, endTime time.Time, filter *model.LogFilter, isToday bool) model.HealthTimelineParams {
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
		bucketSeconds = int64(duration.Seconds() / healthTimelineBuckets)
		if bucketSeconds < 1 {
			bucketSeconds = 1
		}
		healthStart = startTime
	}

	// 转换为毫秒，直接与 logs.time 比较，避免索引失效
	return model.HealthTimelineParams{
		SinceMs:  healthStart.UnixMilli(),
		UntilMs:  endTime.UnixMilli(),
		BucketMs: bucketSeconds * 1000,
		Filter:   filter,
	}
}

// fillHealthTimeline 把分桶查询结果按 (channel_id, model) 填充到统计条目
func fillHealthTimeline(stats []model.StatsEntry, params model.HealthTimelineParams, rows []model.HealthTimelineRow) {
	if len(stats) == 0 {
		return
	}
	bucketSeconds := params.BucketMs / 1000

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

	sinceUnix := time.UnixMilli(params.SinceMs).Unix()

	// 为每个渠道初始化48个空时间点
	for key := range statsMap {
		points := make([]model.HealthPoint, healthTimelineBuckets)
		for i := 0; i < healthTimelineBuckets; i++ {
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
		if bucketIndex < 0 || bucketIndex >= healthTimelineBuckets {
			continue
		}

		total := row.Success + row.ErrorCount
		successRate := 0.0
		if total > 0 {
			successRate = float64(row.Success) / float64(total)
		}

		// 更新数据字段，保留初始化时的 Ts（Go 计算的桶起始时间）
		// 不能用 SQL 的 FLOOR 桶边界覆盖 Ts，否则同一桶索引在不同模型间
		// 产生不同时间戳，导致前端按 ts 合并时出现幽灵条目
		p := &timeline[key][bucketIndex]
		p.SuccessRate = successRate
		p.SuccessCount = row.Success
		p.ErrorCount = row.ErrorCount
		p.RateLimitedCount = row.RateLimitedCount
		p.AvgFirstByteTime = row.AvgFirstByteTime
		p.AvgDuration = row.AvgDuration
		p.TotalInputTokens = row.InputTokens
		p.TotalOutputTokens = row.OutputTokens
		p.TotalCacheReadTokens = row.CacheReadTokens
		p.TotalCacheCreationTokens = row.CacheCreationTokens
		p.TotalCost = row.TotalCost
		p.EffectiveCost = row.EffectiveCost
	}

	// 填充到 stats 中（per-model，供 stats 页面使用）
	for key, idx := range statsMap {
		if points, exists := timeline[key]; exists {
			stats[idx].HealthTimeline = points
		}
	}
}

// HandleStatsFilterOptions 返回统计页筛选下拉的全集（渠道名/模型），
// 从指定时间范围内的日志记录中提取，与表格数据解耦。
// GET /admin/stats/filter-options?range=today&upstream_protocol=
func (s *Server) HandleStatsFilterOptions(c *gin.Context) {
	params := ParsePaginationParams(c)
	startTime, endTime := params.GetTimeRange()

	lf := BuildLogFilter(c)
	lf.LogSource = model.LogSourceProxy

	// 两个去重查询互不依赖，并行执行
	ctx := c.Request.Context()
	var (
		channels    []model.ChannelNameID
		models      []string
		channelsErr error
		modelsErr   error
		wg          sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		channels, channelsErr = s.statsCache.GetDistinctChannels(ctx, startTime, endTime, &lf)
	}()
	go func() {
		defer wg.Done()
		models, modelsErr = s.statsCache.GetDistinctModels(ctx, startTime, endTime, &lf)
	}()
	wg.Wait()
	if channelsErr != nil {
		RespondError(c, http.StatusInternalServerError, channelsErr)
		return
	}
	if modelsErr != nil {
		RespondError(c, http.StatusInternalServerError, modelsErr)
		return
	}

	if hideTokenChannels(c) {
		channels = nil
	}
	channelNames := make([]string, 0, len(channels))
	for _, ch := range channels {
		if ch.Name != "" {
			channelNames = append(channelNames, ch.Name)
		}
	}

	RespondJSON(c, http.StatusOK, gin.H{
		"channel_names": channelNames,
		"models":        models,
	})
}
