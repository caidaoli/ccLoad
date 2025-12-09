package sql

import (
	"context"
	"database/sql"
	"time"

	"ccLoad/internal/model"
)

// GetAuthTokenStatsInRange 查询指定时间范围内每个token的统计数据（从logs表聚合）
// 用于tokens.html页面按时间范围筛选显示（2025-12新增）
func (s *SQLStore) GetAuthTokenStatsInRange(ctx context.Context, startTime, endTime time.Time) (map[int64]*model.AuthTokenRangeStats, error) {
	sinceMs := startTime.UnixMilli()
	untilMs := endTime.UnixMilli()

	query := `
		SELECT
			auth_token_id,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success_count,
			SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END) AS failure_count,
			SUM(input_tokens) AS prompt_tokens,
			SUM(output_tokens) AS completion_tokens,
			SUM(cache_read_input_tokens) AS cache_read_tokens,
			SUM(cache_creation_input_tokens) AS cache_creation_tokens,
			SUM(cost) AS total_cost,
			AVG(CASE WHEN is_streaming = 1 THEN first_byte_time ELSE NULL END) AS stream_avg_ttfb,
			AVG(CASE WHEN is_streaming = 0 THEN duration ELSE NULL END) AS non_stream_avg_rt,
			SUM(CASE WHEN is_streaming = 1 THEN 1 ELSE 0 END) AS stream_count,
			SUM(CASE WHEN is_streaming = 0 THEN 1 ELSE 0 END) AS non_stream_count
		FROM logs
		WHERE time >= ? AND time <= ? AND auth_token_id > 0
		GROUP BY auth_token_id
	`

	rows, err := s.db.QueryContext(ctx, query, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
