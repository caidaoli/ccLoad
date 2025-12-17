package sql

import (
	"ccLoad/internal/model"
	"context"
	"fmt"
	"strings"
	"time"
)

// AggregateRangeWithFilter 聚合指定时间范围、渠道类型和模型的指标数据
// channelType 为空字符串时返回所有渠道类型的数据
// modelFilter 为空字符串时返回所有模型的数据
func (s *SQLStore) AggregateRangeWithFilter(ctx context.Context, since, until time.Time, bucket time.Duration, channelType string, modelFilter string, authTokenID int64) ([]model.MetricPoint, error) {
	bucketSeconds := int64(bucket.Seconds())
	sinceUnix := since.Unix()
	untilUnix := until.Unix()

	// [TARGET] 修复跨数据库JOIN:先从主库查询符合类型的渠道ID列表
	var channelIDs []int64
	if channelType != "" {
		var err error
		channelIDs, err = s.fetchChannelIDsByType(ctx, channelType)
		if err != nil {
			return nil, fmt.Errorf("fetch channel ids by type: %w", err)
		}
		// 如果没有符合条件的渠道,直接返回空结果
		if len(channelIDs) == 0 {
			return buildEmptyMetricPoints(since, until, bucket), nil
		}
	}

	// 构建查询:不再JOIN channels表,使用IN子句过滤
	// 使用FLOOR确保bucket_ts是整数,避免浮点数导致map查找失败
	query := `
		SELECT
			FLOOR((logs.time / 1000) / ?) * ? AS bucket_ts,
			logs.channel_id,
			SUM(CASE WHEN logs.status_code >= 200 AND logs.status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN logs.status_code < 200 OR logs.status_code >= 300 THEN 1 ELSE 0 END) AS error,
			ROUND(
				AVG(CASE WHEN logs.is_streaming = 1 AND logs.first_byte_time > 0 AND logs.status_code >= 200 AND logs.status_code < 300 THEN logs.first_byte_time ELSE NULL END),
				3
			) as avg_first_byte_time,
			ROUND(
				AVG(CASE WHEN logs.duration > 0 AND logs.status_code >= 200 AND logs.status_code < 300 THEN logs.duration ELSE NULL END),
				3
			) as avg_duration,
			SUM(CASE WHEN logs.is_streaming = 1 AND logs.first_byte_time > 0 AND logs.status_code >= 200 AND logs.status_code < 300 THEN 1 ELSE 0 END) as stream_success_first_byte_count,
			SUM(CASE WHEN logs.duration > 0 AND logs.status_code >= 200 AND logs.status_code < 300 THEN 1 ELSE 0 END) as duration_success_count,
			SUM(COALESCE(logs.cost, 0.0)) as total_cost,
			SUM(COALESCE(logs.input_tokens, 0)) as input_tokens,
			SUM(COALESCE(logs.output_tokens, 0)) as output_tokens,
			SUM(COALESCE(logs.cache_read_input_tokens, 0)) as cache_read_tokens,
			SUM(COALESCE(logs.cache_creation_input_tokens, 0)) as cache_creation_tokens
		FROM logs
		WHERE (logs.time / 1000) >= ? AND (logs.time / 1000) <= ?
	`

	args := []any{bucketSeconds, bucketSeconds, sinceUnix, untilUnix}

	// 添加 channel_type 过滤(使用IN子句)
	if len(channelIDs) > 0 {
		placeholders := make([]string, len(channelIDs))
		for i := range channelIDs {
			placeholders[i] = "?"
			args = append(args, channelIDs[i])
		}
		query += fmt.Sprintf(" AND logs.channel_id IN (%s)", strings.Join(placeholders, ","))
	}

	// 添加模型过滤
	if modelFilter != "" {
		query += " AND logs.model = ?"
		args = append(args, modelFilter)
	}

	// 添加 auth_token_id 过滤
	if authTokenID > 0 {
		query += " AND logs.auth_token_id = ?"
		args = append(args, authTokenID)
	}

	query += `
		GROUP BY bucket_ts, logs.channel_id
		ORDER BY bucket_ts ASC
	`

	rows, err := s.db.QueryContext(ctx, query, args...)
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

// buildEmptyMetricPoints 构建空的时间序列数据点（用于无数据场景）
func buildEmptyMetricPoints(since, until time.Time, bucket time.Duration) []model.MetricPoint {
	var out []model.MetricPoint
	endTime := until.Truncate(bucket).Add(bucket)
	startTime := since.Truncate(bucket)

	for t := startTime; t.Before(endTime); t = t.Add(bucket) {
		out = append(out, model.MetricPoint{
			Ts:       t,
			Channels: make(map[string]model.ChannelMetric),
		})
	}
	return out
}

// GetDistinctModels 获取指定时间范围内的去重模型列表
func (s *SQLStore) GetDistinctModels(ctx context.Context, since, until time.Time) ([]string, error) {
	query := `
		SELECT DISTINCT model
		FROM logs
		WHERE (time / 1000) >= ? AND (time / 1000) <= ? AND model != ''
		ORDER BY model
	`

	rows, err := s.db.QueryContext(ctx, query, since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		models = append(models, model)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	if models == nil {
		models = make([]string, 0)
	}
	return models, nil
}
