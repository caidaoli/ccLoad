package sql

import (
	"context"
	"database/sql"
	"time"

	"ccLoad/internal/model"
)

// GetAuthTokenStatsInRange 查询指定时间范围内每个token的统计数据（从logs表聚合）
// 用于tokens.html页面按时间范围筛选显示（2025-12新增）
// [FIX] 2025-12: 排除499（客户端取消）避免污染成功率统计
func (s *SQLStore) GetAuthTokenStatsInRange(ctx context.Context, startTime, endTime time.Time) (map[int64]*model.AuthTokenRangeStats, error) {
	sinceMs := startTime.UnixMilli()
	untilMs := endTime.UnixMilli()

	// 排除499：客户端取消不应计入成功/失败统计
	query := `
		SELECT
			auth_token_id,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success_count,
			SUM(CASE WHEN (status_code < 200 OR status_code >= 300) AND status_code != 499 THEN 1 ELSE 0 END) AS failure_count,
			SUM(input_tokens) AS prompt_tokens,
			SUM(output_tokens) AS completion_tokens,
			SUM(cache_read_input_tokens) AS cache_read_tokens,
			SUM(cache_creation_input_tokens) AS cache_creation_tokens,
			SUM(cost) AS total_cost,
			AVG(CASE WHEN is_streaming = 1 THEN first_byte_time ELSE NULL END) AS stream_avg_ttfb,
			AVG(CASE WHEN is_streaming = 0 THEN duration ELSE NULL END) AS non_stream_avg_rt,
			SUM(CASE WHEN is_streaming = 1 AND status_code != 499 THEN 1 ELSE 0 END) AS stream_count,
			SUM(CASE WHEN is_streaming = 0 AND status_code != 499 THEN 1 ELSE 0 END) AS non_stream_count
		FROM logs
		WHERE time >= ? AND time <= ? AND auth_token_id > 0
		GROUP BY auth_token_id
	`

	rows, err := s.db.QueryContext(ctx, query, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	stats := make(map[int64]*model.AuthTokenRangeStats)
	for rows.Next() {
		var tokenID int64
		var stat model.AuthTokenRangeStats
		var streamAvgTTFB, nonStreamAvgRT sql.NullFloat64

		if err := rows.Scan(&tokenID, &stat.SuccessCount, &stat.FailureCount,
			&stat.PromptTokens, &stat.CompletionTokens,
			&stat.CacheReadTokens, &stat.CacheCreationTokens,
			&stat.TotalCost,
			&streamAvgTTFB, &nonStreamAvgRT,
			&stat.StreamCount, &stat.NonStreamCount); err != nil {
			return nil, err
		}

		// 处理NULL值（当没有该类型请求时AVG返回NULL）
		if streamAvgTTFB.Valid {
			stat.StreamAvgTTFB = streamAvgTTFB.Float64
		}
		if nonStreamAvgRT.Valid {
			stat.NonStreamAvgRT = nonStreamAvgRT.Float64
		}

		stats[tokenID] = &stat
	}

	return stats, rows.Err()
}

// FillAuthTokenRPMStats 计算每个token的RPM统计（峰值、平均、最近）
// 直接修改传入的stats map中的RPM字段
// [FIX] 2025-12: 排除499（客户端取消）避免污染RPM统计
func (s *SQLStore) FillAuthTokenRPMStats(ctx context.Context, stats map[int64]*model.AuthTokenRangeStats, startTime, endTime time.Time, isToday bool) error {
	if len(stats) == 0 {
		return nil
	}

	sinceBucket := startTime.UnixMilli() / minuteMs
	untilBucket := endTime.UnixMilli() / minuteMs

	// 计算时间跨度（秒）
	durationSeconds := endTime.Sub(startTime).Seconds()
	if durationSeconds < 1 {
		durationSeconds = 1
	}

	// 1. 计算平均RPM = 总请求数 × 60 / 时间范围秒数
	for _, stat := range stats {
		totalCount := stat.SuccessCount + stat.FailureCount
		stat.AvgRPM = float64(totalCount) * 60 / durationSeconds
	}

	// 2. 计算峰值RPM（每分钟请求数的最大值）
	// 排除499：客户端取消不应计入RPM
	peakQuery := `
		SELECT auth_token_id, MAX(cnt) AS peak_rpm
		FROM (
			SELECT auth_token_id, COUNT(*) AS cnt
			FROM logs
			WHERE minute_bucket >= ? AND minute_bucket <= ? AND auth_token_id > 0 AND status_code != 499
			GROUP BY auth_token_id, minute_bucket
		) t
		GROUP BY auth_token_id
	`
	peakRows, err := s.db.QueryContext(ctx, peakQuery, sinceBucket, untilBucket)
	if err != nil {
		return err
	}
	defer func() { _ = peakRows.Close() }()

	for peakRows.Next() {
		var tokenID int64
		var peakRPM float64
		if err := peakRows.Scan(&tokenID, &peakRPM); err != nil {
			return err
		}
		if stat, ok := stats[tokenID]; ok {
			stat.PeakRPM = peakRPM
		}
	}

	// 3. 计算最近一分钟RPM（仅本日有效）
	// 排除499：客户端取消不应计入RPM
	if isToday {
		now := time.Now()
		recentStartBucket := now.Add(-60*time.Second).UnixMilli() / minuteMs
		recentEndBucket := now.UnixMilli() / minuteMs

		recentQuery := `
			SELECT auth_token_id, COUNT(*) AS cnt
			FROM logs
			WHERE minute_bucket >= ? AND minute_bucket <= ? AND auth_token_id > 0 AND status_code != 499
			GROUP BY auth_token_id
		`
		recentRows, err := s.db.QueryContext(ctx, recentQuery, recentStartBucket, recentEndBucket)
		if err != nil {
			return err
		}
		defer func() { _ = recentRows.Close() }()

		for recentRows.Next() {
			var tokenID int64
			var recentRPM float64
			if err := recentRows.Scan(&tokenID, &recentRPM); err != nil {
				return err
			}
			if stat, ok := stats[tokenID]; ok {
				stat.RecentRPM = recentRPM
				// 峰值必须 >= 最近值
				if stat.PeakRPM < recentRPM {
					stat.PeakRPM = recentRPM
				}
			}
		}
	}

	return nil
}
