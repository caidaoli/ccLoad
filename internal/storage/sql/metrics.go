package sql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"ccLoad/internal/model"
)

func (s *SQLStore) Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]model.MetricPoint, error) {
	// 性能优化：使用SQL GROUP BY进行数据库层聚合，避免内存聚合
	// 原方案：加载所有日志到内存聚合（10万条日志需2-5秒，占用100-200MB内存）
	// 新方案：数据库聚合（查询时间-80%，内存占用-90%）
	// 批量查询渠道名称消除N+1问题（100渠道场景提升50-100倍）

	bucketMs := int64(bucket / time.Millisecond)
	sinceMs := since.UnixMilli()

	// SQL聚合查询：使用毫秒时间戳实现时间桶分组
	// 优化：直接使用毫秒时间戳匹配索引，避免运行时除法阻止索引使用
	// bucket_ts = FLOOR(time_ms / bucket_ms) * bucket_ms / 1000 (返回秒级时间戳)
	// 使用FLOOR确保bucket_ts是整数，避免浮点数导致map查找失败
	query := `
		SELECT
			FLOOR(time / ?) * ? / 1000 AS bucket_ts,
			channel_id,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END) AS error,
			ROUND(
				AVG(CASE WHEN is_streaming = 1 AND first_byte_time > 0 AND status_code >= 200 AND status_code < 300 THEN first_byte_time ELSE NULL END),
				3
			) as avg_first_byte_time,
			ROUND(
				AVG(CASE WHEN duration > 0 AND status_code >= 200 AND status_code < 300 THEN duration ELSE NULL END),
				3
			) as avg_duration,
			SUM(CASE WHEN is_streaming = 1 AND first_byte_time > 0 AND status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) as stream_success_first_byte_count,
			SUM(CASE WHEN duration > 0 AND status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) as duration_success_count,
			SUM(COALESCE(cost, 0.0)) as total_cost,
			SUM(COALESCE(input_tokens, 0)) as input_tokens,
			SUM(COALESCE(output_tokens, 0)) as output_tokens,
			SUM(COALESCE(cache_read_input_tokens, 0)) as cache_read_tokens,
			SUM(COALESCE(cache_creation_input_tokens, 0)) as cache_creation_tokens
		FROM logs
		WHERE time >= ?
		GROUP BY bucket_ts, channel_id
		ORDER BY bucket_ts ASC
	`

	rows, err := s.db.QueryContext(ctx, query, bucketMs, bucketMs, sinceMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mapp, helperMap, channelIDsToFetch, err := scanAggregatedMetricsRows(rows)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	return s.finalizeMetricPoints(ctx, mapp, helperMap, channelIDsToFetch, since, now, bucket), nil
}

// AggregateRange 聚合指定时间范围内的指标数据（支持精确日期范围如"昨日"）
func (s *SQLStore) AggregateRange(ctx context.Context, since, until time.Time, bucket time.Duration) ([]model.MetricPoint, error) {
	bucketMs := int64(bucket / time.Millisecond)
	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()

	// 优化：直接使用毫秒时间戳匹配索引，避免运行时除法阻止索引使用
	query := `
		SELECT
			FLOOR(time / ?) * ? / 1000 AS bucket_ts,
			channel_id,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END) AS error,
			ROUND(
				AVG(CASE WHEN is_streaming = 1 AND first_byte_time > 0 AND status_code >= 200 AND status_code < 300 THEN first_byte_time ELSE NULL END),
				3
			) as avg_first_byte_time,
			ROUND(
				AVG(CASE WHEN duration > 0 AND status_code >= 200 AND status_code < 300 THEN duration ELSE NULL END),
				3
			) as avg_duration,
			SUM(CASE WHEN is_streaming = 1 AND first_byte_time > 0 AND status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) as stream_success_first_byte_count,
			SUM(CASE WHEN duration > 0 AND status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) as duration_success_count,
			SUM(COALESCE(cost, 0.0)) as total_cost,
			SUM(COALESCE(input_tokens, 0)) as input_tokens,
			SUM(COALESCE(output_tokens, 0)) as output_tokens,
			SUM(COALESCE(cache_read_input_tokens, 0)) as cache_read_tokens,
			SUM(COALESCE(cache_creation_input_tokens, 0)) as cache_creation_tokens
		FROM logs
		WHERE time >= ? AND time <= ?
		GROUP BY bucket_ts, channel_id
		ORDER BY bucket_ts ASC
	`

	rows, err := s.db.QueryContext(ctx, query, bucketMs, bucketMs, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mapp, helperMap, channelIDsToFetch, err := scanAggregatedMetricsRows(rows)
	if err != nil {
		return nil, err
	}

	return s.finalizeMetricPoints(ctx, mapp, helperMap, channelIDsToFetch, since, until, bucket), nil
}

// GetStats 实现统计功能，按渠道和模型统计成功/失败次数
// 性能优化：批量查询渠道名称消除N+1问题（100渠道场景提升50-100倍）
// [FIX] 2025-12: 排除499（客户端取消）避免污染成功率和调用次数统计
func (s *SQLStore) GetStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) ([]model.StatsEntry, error) {
	// 使用查询构建器构建统计查询
	// 排除499：客户端取消不应计入成功/失败统计
	baseQuery := `
		SELECT
			channel_id,
			COALESCE(model, '') AS model,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN (status_code < 200 OR status_code >= 300) AND status_code != 499 THEN 1 ELSE 0 END) AS error,
			SUM(CASE WHEN status_code != 499 THEN 1 ELSE 0 END) AS total,
			ROUND(
				AVG(CASE WHEN is_streaming = 1 AND first_byte_time > 0 AND status_code >= 200 AND status_code < 300 THEN first_byte_time ELSE NULL END),
				3
			) as avg_first_byte_time,
			ROUND(
				AVG(CASE WHEN duration > 0 THEN duration ELSE NULL END),
				3
			) as avg_duration,
			SUM(COALESCE(input_tokens, 0)) as total_input_tokens,
			SUM(COALESCE(output_tokens, 0)) as total_output_tokens,
			SUM(COALESCE(cache_read_input_tokens, 0)) as total_cache_read_input_tokens,
			SUM(COALESCE(cache_creation_input_tokens, 0)) as total_cache_creation_input_tokens,
			SUM(COALESCE(cost, 0.0)) as total_cost
		FROM logs`

	// time字段现在是BIGINT毫秒时间戳
	startMs := startTime.UnixMilli()
	endMs := endTime.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("time >= ?", startMs).
		Where("time <= ?", endMs).
		Where("channel_id > 0") // [TARGET] 核心修改:排除channel_id=0的无效记录

	// 应用渠道类型或名称过滤
	_, isEmpty, err := s.applyChannelFilter(ctx, qb, filter)
	if err != nil {
		return nil, err
	}
	if isEmpty {
		return []model.StatsEntry{}, nil
	}

	// 应用其余过滤器（模型/状态码等）
	qb.ApplyFilter(filter)

	suffix := "GROUP BY channel_id, model ORDER BY channel_id ASC, model ASC"
	query, args := qb.BuildWithSuffix(suffix)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := make([]model.StatsEntry, 0)
	channelIDsToFetch := make(map[int64]bool)

	for rows.Next() {
		var entry model.StatsEntry
		var avgFirstByteTime, avgDuration sql.NullFloat64
		var totalInputTokens, totalOutputTokens, totalCacheReadTokens, totalCacheCreationTokens sql.NullInt64
		var totalCost sql.NullFloat64

		err := rows.Scan(&entry.ChannelID, &entry.Model,
			&entry.Success, &entry.Error, &entry.Total, &avgFirstByteTime, &avgDuration,
			&totalInputTokens, &totalOutputTokens, &totalCacheReadTokens, &totalCacheCreationTokens, &totalCost)
		if err != nil {
			return nil, err
		}

		if avgFirstByteTime.Valid {
			entry.AvgFirstByteTimeSeconds = &avgFirstByteTime.Float64
		}
		if avgDuration.Valid {
			entry.AvgDurationSeconds = &avgDuration.Float64
		}

		// 填充token统计字段（仅当有值时）
		if totalInputTokens.Valid && totalInputTokens.Int64 > 0 {
			entry.TotalInputTokens = &totalInputTokens.Int64
		}
		if totalOutputTokens.Valid && totalOutputTokens.Int64 > 0 {
			entry.TotalOutputTokens = &totalOutputTokens.Int64
		}
		if totalCacheReadTokens.Valid && totalCacheReadTokens.Int64 > 0 {
			entry.TotalCacheReadInputTokens = &totalCacheReadTokens.Int64
		}
		if totalCacheCreationTokens.Valid && totalCacheCreationTokens.Int64 > 0 {
			entry.TotalCacheCreationInputTokens = &totalCacheCreationTokens.Int64
		}
		if totalCost.Valid && totalCost.Float64 > 0 {
			entry.TotalCost = &totalCost.Float64
		}

		if entry.ChannelID != nil {
			channelIDsToFetch[int64(*entry.ChannelID)] = true
		}
		stats = append(stats, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(channelIDsToFetch) > 0 {
		channelInfos, err := s.fetchChannelInfoBatch(ctx, channelIDsToFetch)
		if err != nil {
			// 降级处理:查询失败不影响统计返回,仅记录错误
			log.Printf("[WARN]  批量查询渠道信息失败: %v", err)
			channelInfos = make(map[int64]ChannelInfo)
		}

		// 填充渠道名称和优先级
		for i := range stats {
			if stats[i].ChannelID != nil {
				if info, ok := channelInfos[int64(*stats[i].ChannelID)]; ok {
					stats[i].ChannelName = info.Name
					stats[i].ChannelPriority = &info.Priority
				} else {
					// 如果查询不到渠道信息,使用默认值
					stats[i].ChannelName = "未知渠道"
				}
			}
		}
	}

	// 计算每个channel_id+model的RPM统计
	if len(stats) > 0 {
		if err := s.fillStatsRPM(ctx, stats, startTime, endTime, filter, isToday); err != nil {
			// 降级处理：RPM计算失败不影响主要统计数据
			log.Printf("[WARN] 计算RPM统计失败: %v", err)
		}
	}

	return stats, nil
}

// GetRPMStats 获取RPM/QPS统计数据（峰值、平均、最近一分钟）
// isToday参数控制是否计算最近一分钟数据（仅本日有意义）
// [FIX] 2025-12: 排除499（客户端取消）避免污染RPM统计
func (s *SQLStore) GetRPMStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) (*model.RPMStats, error) {
	stats := &model.RPMStats{}

	startMs := startTime.UnixMilli()
	endMs := endTime.UnixMilli()

	// 1. 计算峰值RPM（每分钟请求数的最大值）
	// 使用 FLOOR 确保 MySQL 返回整数（MySQL 的 / 返回浮点数，会导致分组错误）
	// 排除499：客户端取消不应计入RPM
	peakBaseQuery := `
		SELECT COALESCE(MAX(cnt), 0) as peak_rpm FROM (
			SELECT COUNT(*) as cnt
			FROM logs`

	peakQB := NewQueryBuilder(peakBaseQuery).
		Where("time >= ?", startMs).
		Where("time <= ?", endMs).
		Where("channel_id > 0").
		Where("status_code != 499")

	// 应用渠道类型或名称过滤
	_, isEmpty, err := s.applyChannelFilter(ctx, peakQB, filter)
	if err != nil {
		return nil, fmt.Errorf("apply channel filter: %w", err)
	}
	if isEmpty {
		return stats, nil
	}

	// 应用其余过滤器（模型/状态码等）
	peakQB.ApplyFilter(filter)

	peakQuery, peakArgs := peakQB.BuildWithSuffix("GROUP BY FLOOR(time / 60000)) t")

	var peakRPM float64
	if err := s.db.QueryRowContext(ctx, peakQuery, peakArgs...).Scan(&peakRPM); err != nil {
		return nil, fmt.Errorf("query peak RPM: %w", err)
	}
	stats.PeakRPM = peakRPM
	stats.PeakQPS = peakRPM / 60

	// 2. 计算平均RPM/QPS
	durationSeconds := endTime.Sub(startTime).Seconds()
	if durationSeconds < 1 {
		durationSeconds = 1
	}

	totalBaseQuery := `SELECT COUNT(*) FROM logs`
	totalQB := NewQueryBuilder(totalBaseQuery).
		Where("time >= ?", startMs).
		Where("time <= ?", endMs).
		Where("channel_id > 0").
		Where("status_code != 499")

	// 应用渠道过滤
	_, isEmpty, err = s.applyChannelFilter(ctx, totalQB, filter)
	if err != nil {
		return nil, fmt.Errorf("apply channel filter for total: %w", err)
	}
	if isEmpty {
		return stats, nil
	}

	// 应用其余过滤器
	totalQB.ApplyFilter(filter)

	totalQuery, totalArgs := totalQB.Build()
	var totalCount int64
	if err := s.db.QueryRowContext(ctx, totalQuery, totalArgs...).Scan(&totalCount); err != nil {
		return nil, fmt.Errorf("query total count: %w", err)
	}

	stats.AvgRPM = float64(totalCount) * 60 / durationSeconds
	stats.AvgQPS = float64(totalCount) / durationSeconds

	// 3. 计算最近一分钟（仅本日有意义）
	if isToday {
		now := time.Now()
		recentStartMs := now.Add(-60 * time.Second).UnixMilli()
		recentEndMs := now.UnixMilli()

		recentBaseQuery := `SELECT COUNT(*) FROM logs`
		recentQB := NewQueryBuilder(recentBaseQuery).
			Where("time >= ?", recentStartMs).
			Where("time <= ?", recentEndMs).
			Where("channel_id > 0").
			Where("status_code != 499")

		// 应用渠道过滤
		_, isEmpty, err = s.applyChannelFilter(ctx, recentQB, filter)
		if err != nil {
			return nil, fmt.Errorf("apply channel filter for recent: %w", err)
		}
		if !isEmpty {
			// 应用其余过滤器
			recentQB.ApplyFilter(filter)

			recentQuery, recentArgs := recentQB.Build()
			var recentCount int64
			if err := s.db.QueryRowContext(ctx, recentQuery, recentArgs...).Scan(&recentCount); err != nil {
				return nil, fmt.Errorf("query recent count: %w", err)
			}

			stats.RecentRPM = float64(recentCount)
			stats.RecentQPS = float64(recentCount) / 60

			// 峰值必须 >= 最近值（滑动窗口可能比固定分钟桶更高）
			if stats.RecentRPM > stats.PeakRPM {
				stats.PeakRPM = stats.RecentRPM
				stats.PeakQPS = stats.RecentQPS
			}
		}
	}

	return stats, nil
}

// fillStatsRPM 计算每个channel_id+model组合的RPM统计数据
// 使用分钟级分组计算峰值RPM，避免因时间跨度大导致的值过小问题
// [FIX] 2025-12: 排除499（客户端取消）避免污染RPM统计
func (s *SQLStore) fillStatsRPM(ctx context.Context, stats []model.StatsEntry, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) error {
	startMs := startTime.UnixMilli()
	endMs := endTime.UnixMilli()

	// 计算时间跨度（秒）用于平均RPM
	durationSeconds := endTime.Sub(startTime).Seconds()
	if durationSeconds < 1 {
		durationSeconds = 1
	}

	// 查询每个channel_id+model的峰值RPM（每分钟请求数的最大值）
	// 使用 FLOOR 确保 MySQL 返回整数（MySQL 的 / 返回浮点数，会导致分组错误）
	// 排除499：客户端取消不应计入RPM
	peakQuery := `
		SELECT channel_id, COALESCE(model, '') AS model, MAX(cnt) AS peak_rpm
		FROM (
			SELECT channel_id, model, COUNT(*) AS cnt
			FROM logs
			WHERE time >= ? AND time <= ? AND channel_id > 0 AND status_code != 499
			GROUP BY channel_id, model, FLOOR(time / 60000)
		) t
		GROUP BY channel_id, model`

	rows, err := s.db.QueryContext(ctx, peakQuery, startMs, endMs)
	if err != nil {
		return fmt.Errorf("query peak RPM: %w", err)
	}
	defer rows.Close()

	// 构建 (channel_id, model) -> peak_rpm 映射
	type statsKey struct {
		channelID int
		model     string
	}
	peakRPMMap := make(map[statsKey]float64)

	for rows.Next() {
		var channelID int
		var model string
		var peakRPM float64
		if err := rows.Scan(&channelID, &model, &peakRPM); err != nil {
			return fmt.Errorf("scan peak RPM: %w", err)
		}
		peakRPMMap[statsKey{channelID, model}] = peakRPM
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate peak RPM rows: %w", err)
	}

	// 如果是本日，查询每个channel_id+model的最近一分钟RPM
	// 排除499：客户端取消不应计入RPM
	recentRPMMap := make(map[statsKey]float64)
	if isToday {
		now := time.Now()
		recentStartMs := now.Add(-60 * time.Second).UnixMilli()
		recentEndMs := now.UnixMilli()

		recentQuery := `
			SELECT channel_id, COALESCE(model, '') AS model, COUNT(*) AS cnt
			FROM logs
			WHERE time >= ? AND time <= ? AND channel_id > 0 AND status_code != 499
			GROUP BY channel_id, model`

		recentRows, err := s.db.QueryContext(ctx, recentQuery, recentStartMs, recentEndMs)
		if err != nil {
			return fmt.Errorf("query recent RPM: %w", err)
		}
		defer recentRows.Close()

		for recentRows.Next() {
			var channelID int
			var model string
			var cnt float64
			if err := recentRows.Scan(&channelID, &model, &cnt); err != nil {
				return fmt.Errorf("scan recent RPM: %w", err)
			}
			recentRPMMap[statsKey{channelID, model}] = cnt
		}

		if err := recentRows.Err(); err != nil {
			return fmt.Errorf("iterate recent RPM rows: %w", err)
		}
	}

	// 填充到stats中
	for i := range stats {
		entry := &stats[i]
		if entry.ChannelID == nil {
			continue
		}

		key := statsKey{*entry.ChannelID, entry.Model}

		// 峰值RPM
		if peakRPM, ok := peakRPMMap[key]; ok && peakRPM > 0 {
			entry.PeakRPM = &peakRPM
		}

		// 平均RPM = total * 60 / durationSeconds
		if entry.Total > 0 {
			avgRPM := float64(entry.Total) * 60 / durationSeconds
			entry.AvgRPM = &avgRPM
		}

		// 最近一分钟RPM（仅本日）
		if isToday {
			if recentRPM, ok := recentRPMMap[key]; ok && recentRPM > 0 {
				entry.RecentRPM = &recentRPM
				// 峰值必须 >= 最近值（滑动窗口可能比固定分钟桶更高）
				if entry.PeakRPM == nil || *entry.PeakRPM < recentRPM {
					entry.PeakRPM = &recentRPM
				}
			}
		}
	}

	return nil
}

// GetChannelSuccessRates 获取指定时间窗口内各渠道的成功率和样本量
// 返回 map[channelID]ChannelHealthStats
func (s *SQLStore) GetChannelSuccessRates(ctx context.Context, since time.Time) (map[int64]model.ChannelHealthStats, error) {
	sinceMs := since.UnixMilli()

	// 成功率统计口径：
	// - 只统计能反映渠道/Key质量的结果（2xx成功 + 可重试/可冷却错误）
	// - 排除客户端误用造成的4xx（404/415等）和客户端取消(499)，避免"坏客户端把好渠道打残"
	//
	// 纳入统计的状态码：
	//   2xx: 成功响应
	//   401/402/403: Key认证/付费/权限错误（Key级）
	//   429: 限流（Key级或渠道级）
	//   500/502/503/504: 服务器错误（渠道级）
	//   520/521/524: Cloudflare错误（渠道级）- 520未知错误/521服务器宕机/524超时
	//   597: SSE流错误（Key级，自定义状态码）
	//   注：596(1308配额超限)不纳入统计，因为它不反映渠道质量
	//   598: 上游首字节超时（渠道级，自定义状态码）
	//   599: 流式响应不完整（渠道级，自定义状态码）
	//   注：408已改为客户端错误，不计入健康度
	eligible := `
				(status_code >= 200 AND status_code < 300)
				OR status_code IN (401, 402, 403, 405, 429, 500, 502, 503, 504, 520, 521, 524, 597, 598, 599)
			`

	// 优化：调整条件顺序，time条件在前以利用 idx_logs_time_channel_model 索引的最左前缀
	// channel_id > 0 过滤无效记录（channel_id=0 表示未路由成功的请求）
	query := `
		SELECT
			channel_id,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN ` + eligible + ` THEN 1 ELSE 0 END) AS total
		FROM logs
		WHERE time >= ? AND channel_id > 0
		GROUP BY channel_id`

	rows, err := s.db.QueryContext(ctx, query, sinceMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]model.ChannelHealthStats)
	for rows.Next() {
		var channelID int64
		var success, total int64
		if err := rows.Scan(&channelID, &success, &total); err != nil {
			return nil, err
		}
		if total > 0 {
			result[channelID] = model.ChannelHealthStats{
				SuccessRate: float64(success) / float64(total),
				SampleCount: total,
			}
		}
	}

	return result, rows.Err()
}
