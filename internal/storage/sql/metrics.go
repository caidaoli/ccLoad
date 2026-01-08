package sql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"ccLoad/internal/model"
)

// GetStats 实现统计功能，按渠道和模型统计成功/失败次数
// 消除 N+1：渠道过滤/名称解析用一次批量查询完成
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
	defer func() { _ = rows.Close() }()

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

// GetStatsLite 轻量版统计查询，跳过RPM计算和渠道名称填充
// 适用于 /public/summary 等只需要基础聚合数据的场景
func (s *SQLStore) GetStatsLite(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter) ([]model.StatsEntry, error) {
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

	startMs := startTime.UnixMilli()
	endMs := endTime.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("time >= ?", startMs).
		Where("time <= ?", endMs).
		Where("channel_id > 0")

	_, isEmpty, err := s.applyChannelFilter(ctx, qb, filter)
	if err != nil {
		return nil, err
	}
	if isEmpty {
		return []model.StatsEntry{}, nil
	}

	qb.ApplyFilter(filter)

	suffix := "GROUP BY channel_id, model ORDER BY channel_id ASC, model ASC"
	query, args := qb.BuildWithSuffix(suffix)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	stats := make([]model.StatsEntry, 0)

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

		stats = append(stats, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return stats, nil
}

// GetRPMStats 获取RPM/QPS统计数据（峰值、平均、最近一分钟）
// isToday参数控制是否计算最近一分钟数据（仅本日有意义）
// [FIX] 2025-12: 排除499（客户端取消）避免污染RPM统计
func (s *SQLStore) GetRPMStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) (*model.RPMStats, error) {
	stats := &model.RPMStats{}

	startBucket := startTime.UnixMilli() / minuteMs
	endBucket := endTime.UnixMilli() / minuteMs

	// 合并峰值RPM和总数查询为单次数据库往返
	// 子查询按分钟桶分组统计，外层查询同时计算峰值和总数
	// 排除499：客户端取消不应计入RPM
	combinedBaseQuery := `
		SELECT COALESCE(MAX(cnt), 0) as peak_rpm, COALESCE(SUM(cnt), 0) as total_count FROM (
			SELECT COUNT(*) as cnt
			FROM logs`

	combinedQB := NewQueryBuilder(combinedBaseQuery).
		Where("minute_bucket >= ?", startBucket).
		Where("minute_bucket <= ?", endBucket).
		Where("channel_id > 0").
		Where("status_code != 499")

	// 应用渠道类型或名称过滤
	_, isEmpty, err := s.applyChannelFilter(ctx, combinedQB, filter)
	if err != nil {
		return nil, fmt.Errorf("apply channel filter: %w", err)
	}
	if isEmpty {
		return stats, nil
	}

	// 应用其余过滤器（模型/状态码等）
	combinedQB.ApplyFilter(filter)

	combinedQuery, combinedArgs := combinedQB.BuildWithSuffix("GROUP BY minute_bucket) t")

	var peakRPM float64
	var totalCount int64
	if err := s.db.QueryRowContext(ctx, combinedQuery, combinedArgs...).Scan(&peakRPM, &totalCount); err != nil {
		return nil, fmt.Errorf("query peak RPM and total: %w", err)
	}
	stats.PeakRPM = peakRPM
	stats.PeakQPS = peakRPM / 60

	// 计算平均RPM/QPS
	durationSeconds := endTime.Sub(startTime).Seconds()
	if durationSeconds < 1 {
		durationSeconds = 1
	}
	stats.AvgRPM = float64(totalCount) * 60 / durationSeconds
	stats.AvgQPS = float64(totalCount) / durationSeconds

	// 计算最近一分钟（仅本日有意义）
	if isToday {
		now := time.Now()
		recentStartBucket := now.Add(-60*time.Second).UnixMilli() / minuteMs
		recentEndBucket := now.UnixMilli() / minuteMs

		recentBaseQuery := `SELECT COUNT(*) FROM logs`
		recentQB := NewQueryBuilder(recentBaseQuery).
			Where("minute_bucket >= ?", recentStartBucket).
			Where("minute_bucket <= ?", recentEndBucket).
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
// [FIX] 2025-12: 排除499（客户端取消）避免污染RPM统计
func (s *SQLStore) fillStatsRPM(ctx context.Context, stats []model.StatsEntry, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) error {
	startBucket := startTime.UnixMilli() / minuteMs
	endBucket := endTime.UnixMilli() / minuteMs

	// 计算时间跨度（秒）用于平均RPM
	durationSeconds := endTime.Sub(startTime).Seconds()
	if durationSeconds < 1 {
		durationSeconds = 1
	}

	type statsKey struct {
		channelID int
		model     string
	}
	peakRPMMap := make(map[statsKey]float64)

	// 1) 峰值RPM（分钟桶内最大请求数）
	peakBaseQuery := `
		SELECT channel_id, COALESCE(model, '') AS model, MAX(cnt) AS peak_rpm
		FROM (
			SELECT channel_id, COALESCE(model, '') AS model, COUNT(*) AS cnt
			FROM logs`

	peakQB := NewQueryBuilder(peakBaseQuery).
		Where("minute_bucket >= ?", startBucket).
		Where("minute_bucket <= ?", endBucket).
		Where("channel_id > 0").
		Where("status_code != 499")

	_, isEmpty, err := s.applyChannelFilter(ctx, peakQB, filter)
	if err != nil {
		return fmt.Errorf("apply channel filter for peak: %w", err)
	}

	// 仅当渠道过滤非空时才执行查询
	if !isEmpty {
		peakQB.ApplyFilter(filter)
		peakQuery, peakArgs := peakQB.BuildWithSuffix("GROUP BY channel_id, model, minute_bucket) t GROUP BY channel_id, model")

		peakRows, err := s.db.QueryContext(ctx, peakQuery, peakArgs...)
		if err != nil {
			return fmt.Errorf("query peak RPM: %w", err)
		}
		defer func() { _ = peakRows.Close() }()

		for peakRows.Next() {
			var channelID int
			var model string
			var peakRPM float64
			if err := peakRows.Scan(&channelID, &model, &peakRPM); err != nil {
				return fmt.Errorf("scan peak RPM: %w", err)
			}
			peakRPMMap[statsKey{channelID, model}] = peakRPM
		}
		if err := peakRows.Err(); err != nil {
			return fmt.Errorf("iterate peak RPM rows: %w", err)
		}
	}

	// 2) 最近一分钟RPM（仅本日有效）
	recentRPMMap := make(map[statsKey]float64)
	if isToday {
		now := time.Now()
		recentStartBucket := now.Add(-60*time.Second).UnixMilli() / minuteMs
		recentEndBucket := now.UnixMilli() / minuteMs

		recentBaseQuery := `
			SELECT channel_id, COALESCE(model, '') AS model, COUNT(*) AS cnt
			FROM logs`
		recentQB := NewQueryBuilder(recentBaseQuery).
			Where("minute_bucket >= ?", recentStartBucket).
			Where("minute_bucket <= ?", recentEndBucket).
			Where("channel_id > 0").
			Where("status_code != 499")

		_, isEmpty, err := s.applyChannelFilter(ctx, recentQB, filter)
		if err != nil {
			return fmt.Errorf("apply channel filter for recent: %w", err)
		}

		// 仅当渠道过滤非空时才执行查询
		if !isEmpty {
			recentQB.ApplyFilter(filter)
			recentQuery, recentArgs := recentQB.BuildWithSuffix("GROUP BY channel_id, model")
			recentRows, err := s.db.QueryContext(ctx, recentQuery, recentArgs...)
			if err != nil {
				return fmt.Errorf("query recent RPM: %w", err)
			}
			defer func() { _ = recentRows.Close() }()

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
	}

	// 3) 填充到stats中
	for i := range stats {
		entry := &stats[i]
		if entry.ChannelID == nil {
			continue
		}

		key := statsKey{*entry.ChannelID, entry.Model}

		if peakRPM, ok := peakRPMMap[key]; ok && peakRPM > 0 {
			entry.PeakRPM = &peakRPM
		}

		if entry.Total > 0 {
			avgRPM := float64(entry.Total) * 60 / durationSeconds
			entry.AvgRPM = &avgRPM
		}

		if isToday {
			if recentRPM, ok := recentRPMMap[key]; ok && recentRPM > 0 {
				entry.RecentRPM = &recentRPM
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
	sinceBucket := since.UnixMilli() / minuteMs
	untilBucket := time.Now().UnixMilli() / minuteMs

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

	// 使用 minute_bucket 索引优化查询
	//nolint:gosec // G202: eligible 为内部定义的常量SQL片段，安全可控
	query := `
		SELECT
			channel_id,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN ` + eligible + ` THEN 1 ELSE 0 END) AS total
		FROM logs
		WHERE minute_bucket >= ? AND minute_bucket <= ? AND channel_id > 0
		GROUP BY channel_id`

	rows, err := s.db.QueryContext(ctx, query, sinceBucket, untilBucket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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

// GetTodayChannelCosts 获取今日各渠道成本（启动时加载缓存用）
func (s *SQLStore) GetTodayChannelCosts(ctx context.Context, todayStart time.Time) (map[int64]float64, error) {
	todayStartMs := todayStart.UnixMilli()

	query := `
		SELECT channel_id, COALESCE(SUM(cost), 0) as total_cost
		FROM logs
		WHERE time >= ? AND channel_id > 0
		GROUP BY channel_id`

	rows, err := s.db.QueryContext(ctx, query, todayStartMs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]float64)
	for rows.Next() {
		var channelID int64
		var totalCost float64
		if err := rows.Scan(&channelID, &totalCost); err != nil {
			return nil, err
		}
		result[channelID] = totalCost
	}

	return result, rows.Err()
}
