package sql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ccLoad/internal/model"
)

// AggregateRangeWithFilter 聚合指定时间范围的指标数据，支持多种筛选条件
// filter 为 nil 时返回所有数据
// [FIX] 2025-12: 排除499（客户端取消）避免污染趋势图统计
func (s *SQLStore) AggregateRangeWithFilter(ctx context.Context, since, until time.Time, bucket time.Duration, filter *model.LogFilter) ([]model.MetricPoint, error) {
	bucketMs := int64(bucket / time.Millisecond)
	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()

	// 构建查询:使用IN子句过滤渠道
	// 使用FLOOR确保bucket_ts是整数,避免浮点数导致map查找失败
	// 排除499：客户端取消不应计入成功/失败/RPM统计
	// 优化：直接使用毫秒时间戳匹配索引，避免运行时除法阻止索引使用
	query := `
		SELECT
			FLOOR(logs.time / ?) * ? / 1000 AS bucket_ts,
			logs.channel_id,
			SUM(CASE WHEN logs.status_code >= 200 AND logs.status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN (logs.status_code < 200 OR logs.status_code >= 300) AND logs.status_code != 499 THEN 1 ELSE 0 END) AS error,
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
		WHERE logs.time >= ? AND logs.time <= ? AND logs.status_code != 499
	`

	args := []any{bucketMs, bucketMs, sinceMs, untilMs}

	// 应用渠道筛选（channel_type、channel_id、channel_name、channel_name_like）
	if filter != nil {
		channelIDs, isEmpty, err := s.resolveChannelFilter(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("resolve channel filter: %w", err)
		}
		if isEmpty {
			return buildEmptyMetricPoints(since, until, bucket), nil
		}
		if len(channelIDs) > 0 {
			placeholders := make([]string, len(channelIDs))
			for i := range channelIDs {
				placeholders[i] = "?"
				args = append(args, channelIDs[i])
			}
			query += fmt.Sprintf(" AND logs.channel_id IN (%s)", strings.Join(placeholders, ","))
		}

		// 添加模型过滤
		if filter.Model != "" {
			query += " AND logs.model = ?"
			args = append(args, filter.Model)
		}

		// 添加 auth_token_id 过滤
		if filter.AuthTokenID != nil && *filter.AuthTokenID > 0 {
			query += " AND logs.auth_token_id = ?"
			args = append(args, *filter.AuthTokenID)
		}
	}

	query += `
		GROUP BY bucket_ts, logs.channel_id
		ORDER BY bucket_ts ASC
	`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	mapp, helperMap, channelIDsToFetch, err := scanAggregatedMetricsRows(rows)
	if err != nil {
		return nil, err
	}

	return s.finalizeMetricPoints(ctx, mapp, helperMap, channelIDsToFetch, since, until, bucket), nil
}

// resolveChannelFilter 解析渠道筛选条件，返回符合条件的渠道ID列表
// 返回值：channelIDs（空切片表示不限制）、isEmpty（true表示无匹配结果）、error
func (s *SQLStore) resolveChannelFilter(ctx context.Context, filter *model.LogFilter) ([]int64, bool, error) {
	if filter == nil {
		return nil, false, nil
	}

	// 精确匹配渠道ID优先级最高
	if filter.ChannelID != nil && *filter.ChannelID > 0 {
		return []int64{*filter.ChannelID}, false, nil
	}

	var candidateIDs []int64
	hasTypeFilter := filter.ChannelType != ""
	hasNameFilter := filter.ChannelName != "" || filter.ChannelNameLike != ""

	// 按渠道类型过滤
	if hasTypeFilter {
		ids, err := s.fetchChannelIDsByType(ctx, filter.ChannelType)
		if err != nil {
			return nil, false, err
		}
		if len(ids) == 0 {
			return nil, true, nil // 无匹配结果
		}
		candidateIDs = ids
	}

	// 按渠道名称过滤
	if hasNameFilter {
		ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
		if err != nil {
			return nil, false, err
		}
		if len(ids) == 0 {
			return nil, true, nil // 无匹配结果
		}

		if hasTypeFilter {
			// 取交集
			candidateIDs = intersectIDs(candidateIDs, ids)
			if len(candidateIDs) == 0 {
				return nil, true, nil
			}
		} else {
			candidateIDs = ids
		}
	}

	return candidateIDs, false, nil
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
	// 优化：直接使用毫秒时间戳匹配索引，避免运行时除法阻止索引使用
	query := `
		SELECT DISTINCT model
		FROM logs
		WHERE time >= ? AND time <= ? AND model != ''
		ORDER BY model
	`

	rows, err := s.db.QueryContext(ctx, query, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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
