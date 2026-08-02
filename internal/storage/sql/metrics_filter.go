package sql

import (
	"context"
	"fmt"
	"time"

	"ccLoad/internal/model"
)

// AggregateRangeWithFilter 聚合指定时间范围的指标数据，支持多种筛选条件
// filter 为 nil 时返回所有数据
// [FIX] 2025-12: 排除499（客户端取消）避免污染趋势图统计
func (s *SQLStore) AggregateRangeWithFilter(ctx context.Context, since, until time.Time, bucket time.Duration, filter *model.LogFilter) ([]model.MetricPoint, error) {
	bucketMinutes := int64(bucket / time.Minute)
	if bucketMinutes < 1 {
		bucketMinutes = 1
	}
	sinceBucket := since.UnixMilli() / minuteMs
	untilBucket := until.UnixMilli() / minuteMs

	// 使用 minute_bucket 索引优化
	// 排除499：客户端取消不应计入成功/失败/RPM统计
	qb := NewQueryBuilder(`
		SELECT
			FLOOR(logs.minute_bucket / ?) * ? * 60 AS bucket_ts,
			logs.channel_id,
			SUM(CASE WHEN logs.status_code >= 200 AND logs.status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN (logs.status_code < 200 OR logs.status_code >= 300) AND logs.status_code != 499 THEN 1 ELSE 0 END) AS error,
			AVG(CASE WHEN logs.is_streaming = 1 AND logs.first_byte_time > 0 AND logs.status_code >= 200 AND logs.status_code < 300 THEN logs.first_byte_time ELSE NULL END) as avg_first_byte_time,
			AVG(CASE WHEN logs.duration > 0 AND logs.status_code >= 200 AND logs.status_code < 300 THEN logs.duration ELSE NULL END) as avg_duration,
			SUM(CASE WHEN logs.is_streaming = 1 AND logs.first_byte_time > 0 AND logs.status_code >= 200 AND logs.status_code < 300 THEN 1 ELSE 0 END) as stream_success_first_byte_count,
			SUM(CASE WHEN logs.duration > 0 AND logs.status_code >= 200 AND logs.status_code < 300 THEN 1 ELSE 0 END) as duration_success_count,
			SUM(COALESCE(logs.cost, 0.0)) as total_cost,
			SUM(COALESCE(logs.cost, 0.0) * COALESCE(logs.cost_multiplier, 1)) as effective_cost,
			SUM(COALESCE(logs.input_tokens, 0)) as input_tokens,
			SUM(COALESCE(logs.output_tokens, 0)) as output_tokens,
			SUM(COALESCE(logs.cache_read_input_tokens, 0)) as cache_read_tokens,
			SUM(COALESCE(logs.cache_creation_input_tokens, 0)) as cache_creation_tokens
		FROM logs
	`).
		Where("logs.minute_bucket >= ?", sinceBucket).
		Where("logs.minute_bucket <= ?", untilBucket).
		Where("logs.status_code != 499").
		Where("logs.channel_id > 0")

	// 渠道名称先解析为 ID；其余条件统一交给 LogFilter，避免不同统计端点口径漂移。
	if filter != nil {
		channelIDs, isEmpty, err := s.resolveChannelFilter(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("resolve channel filter: %w", err)
		}
		if isEmpty {
			return buildEmptyMetricPoints(since, until, bucket), nil
		}
		if len(channelIDs) > 0 {
			values := make([]any, len(channelIDs))
			for i, channelID := range channelIDs {
				values[i] = channelID
			}
			qb.WhereIn("logs.channel_id", values)
		}
	}

	if filter != nil {
		qb.ApplyFilter(filter)
	}
	query, args := qb.BuildWithSuffix(`
		GROUP BY bucket_ts, logs.channel_id
		ORDER BY bucket_ts ASC
	`)
	args = append([]any{bucketMinutes, bucketMinutes}, args...)

	rows, err := s.QueryContext(ctx, query, args...)
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
	hasNameFilter := filter.ChannelName != "" || filter.ChannelNameLike != ""

	// 按渠道名称过滤
	if hasNameFilter {
		ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
		if err != nil {
			return nil, false, err
		}
		if len(ids) == 0 {
			return nil, true, nil // 无匹配结果
		}

		candidateIDs = ids
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
func (s *SQLStore) GetDistinctModels(ctx context.Context, since, until time.Time, filter *model.LogFilter) ([]string, error) {
	args := []any{since.UnixMilli(), until.UnixMilli()}

	query := `
		SELECT DISTINCT logs.model
		FROM logs
		WHERE logs.time >= ? AND logs.time <= ? AND logs.model != '' AND logs.channel_id > 0
	`

	wb := NewWhereBuilder()
	wb.ApplyLogFilter(filter)
	whereClause, whereArgs := wb.Build()
	if whereClause != "" {
		query += " AND " + whereClause
		args = append(args, whereArgs...)
	}

	query += " ORDER BY logs.model"

	rows, err := s.QueryContext(ctx, query, args...)
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

// GetDistinctStatusCodes 获取指定时间范围内的去重状态码列表。
func (s *SQLStore) GetDistinctStatusCodes(ctx context.Context, since, until time.Time, filter *model.LogFilter) ([]int, error) {
	args := []any{since.UnixMilli(), until.UnixMilli()}
	query := `
		SELECT DISTINCT logs.status_code
		FROM logs
		WHERE logs.time >= ? AND logs.time <= ?
			AND logs.status_code BETWEEN 100 AND 999
	`

	wb := NewWhereBuilder()
	wb.ApplyLogFilter(filter)
	whereClause, whereArgs := wb.Build()
	if whereClause != "" {
		query += " AND " + whereClause
		args = append(args, whereArgs...)
	}

	query += " ORDER BY logs.status_code"

	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	statusCodes := make([]int, 0)
	for rows.Next() {
		var statusCode int
		if err := rows.Scan(&statusCode); err != nil {
			return nil, err
		}
		statusCodes = append(statusCodes, statusCode)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return statusCodes, nil
}

// GetDistinctChannels 获取指定时间范围内有日志数据的渠道列表（ID+名称）
func (s *SQLStore) GetDistinctChannels(ctx context.Context, since, until time.Time, filter *model.LogFilter) ([]model.ChannelNameID, error) {
	args := []any{since.UnixMilli(), until.UnixMilli()}

	query := `
		SELECT DISTINCT l.channel_id, c.name
		FROM logs l JOIN channels c ON l.channel_id = c.id
		WHERE l.time >= ? AND l.time <= ? AND l.channel_id > 0
	`

	wb := NewWhereBuilder()
	wb.ApplyLogFilter(filter)
	whereClause, whereArgs := wb.Build()
	if whereClause != "" {
		query += " AND " + whereClause
		args = append(args, whereArgs...)
	}

	query += " ORDER BY c.name"

	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var channels []model.ChannelNameID
	for rows.Next() {
		var ch model.ChannelNameID
		if err := rows.Scan(&ch.ID, &ch.Name); err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	if channels == nil {
		channels = make([]model.ChannelNameID, 0)
	}
	return channels, nil
}
