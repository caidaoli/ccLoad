package mysql

import (
	"ccLoad/internal/model"
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// AggregateRangeWithFilter 聚合指定时间范围、渠道类型和模型的指标数据
// channelType 为空字符串时返回所有渠道类型的数据
// modelFilter 为空字符串时返回所有模型的数据
func (s *MySQLStore) AggregateRangeWithFilter(ctx context.Context, since, until time.Time, bucket time.Duration, channelType string, modelFilter string) ([]model.MetricPoint, error) {
	bucketSeconds := int64(bucket.Seconds())
	sinceUnix := since.Unix()
	untilUnix := until.Unix()

	// 🎯 修复跨数据库JOIN：先从主库查询符合类型的渠道ID列表
	var channelIDs []int64
	if channelType != "" {
		var err error
		channelIDs, err = s.fetchChannelIDsByType(ctx, channelType)
		if err != nil {
			return nil, fmt.Errorf("fetch channel ids by type: %w", err)
		}
		// 如果没有符合条件的渠道，直接返回空结果
		if len(channelIDs) == 0 {
			return buildEmptyMetricPoints(since, until, bucket), nil
		}
	}

	// 构建查询：不再JOIN channels表，使用IN子句过滤
	query := `
		SELECT
			((logs.time / 1000) / ?) * ? AS bucket_ts,
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
			SUM(COALESCE(logs.cost, 0.0)) as total_cost
		FROM logs
		WHERE (logs.time / 1000) >= ? AND (logs.time / 1000) <= ?
	`

	args := []any{bucketSeconds, bucketSeconds, sinceUnix, untilUnix}

	// 添加 channel_type 过滤（使用IN子句）
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

	query += `
		GROUP BY bucket_ts, logs.channel_id
		ORDER BY bucket_ts ASC
	`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mapp := make(map[int64]*model.MetricPoint)
	channelIDsToFetch := make(map[int64]bool)
	type aggregationHelper struct {
		totalFirstByteTime float64
		firstByteCount     int
		totalDuration      float64
		durationCount      int
	}
	helperMap := make(map[int64]*aggregationHelper)

	for rows.Next() {
		var bucketTs int64
		var channelID sql.NullInt64
		var success, errorCount int
		var avgFirstByteTime sql.NullFloat64
		var avgDuration sql.NullFloat64
		var streamSuccessFirstByteCount int
		var durationSuccessCount int
		var totalCost float64

		if err := rows.Scan(&bucketTs, &channelID, &success, &errorCount, &avgFirstByteTime, &avgDuration, &streamSuccessFirstByteCount, &durationSuccessCount, &totalCost); err != nil {
			return nil, err
		}

		mp, ok := mapp[bucketTs]
		if !ok {
			mp = &model.MetricPoint{
				Ts:       time.Unix(bucketTs, 0),
				Channels: make(map[string]model.ChannelMetric),
			}
			mapp[bucketTs] = mp
		}

		helper, ok := helperMap[bucketTs]
		if !ok {
			helper = &aggregationHelper{}
			helperMap[bucketTs] = helper
		}

		mp.Success += success
		mp.Error += errorCount

		if mp.TotalCost == nil {
			mp.TotalCost = new(float64)
		}
		*mp.TotalCost += totalCost

		if avgFirstByteTime.Valid {
			helper.totalFirstByteTime += avgFirstByteTime.Float64 * float64(streamSuccessFirstByteCount)
			helper.firstByteCount += streamSuccessFirstByteCount
		}

		if avgDuration.Valid {
			helper.totalDuration += avgDuration.Float64 * float64(durationSuccessCount)
			helper.durationCount += durationSuccessCount
		}

		channelKey := "未知渠道"
		if channelID.Valid {
			channelKey = fmt.Sprintf("ch_%d", channelID.Int64)
			channelIDsToFetch[channelID.Int64] = true
		}

		var avgFBT *float64
		if avgFirstByteTime.Valid {
			avgFBT = new(float64)
			*avgFBT = avgFirstByteTime.Float64
		}
		var avgDur *float64
		if avgDuration.Valid {
			avgDur = new(float64)
			*avgDur = avgDuration.Float64
		}
		var chCost *float64
		if totalCost > 0 {
			chCost = new(float64)
			*chCost = totalCost
		}

		mp.Channels[channelKey] = model.ChannelMetric{
			Success:                 success,
			Error:                   errorCount,
			AvgFirstByteTimeSeconds: avgFBT,
			AvgDurationSeconds:      avgDur,
			TotalCost:               chCost,
		}
	}

	channelNames := make(map[int64]string)
	if len(channelIDsToFetch) > 0 {
		var err error
		channelNames, err = s.fetchChannelNamesBatch(ctx, channelIDsToFetch)
		if err != nil {
			log.Printf("⚠️  批量查询渠道名称失败: %v", err)
			channelNames = make(map[int64]string)
		}
	}

	for bucketTs, mp := range mapp {
		newChannels := make(map[string]model.ChannelMetric)
		for key, metric := range mp.Channels {
			if key == "未知渠道" {
				newChannels[key] = metric
			} else {
				var channelID int64
				fmt.Sscanf(key, "ch_%d", &channelID)
				if name, ok := channelNames[channelID]; ok {
					newChannels[name] = metric
				} else {
					newChannels["未知渠道"] = metric
				}
			}
		}
		mp.Channels = newChannels

		if helper, ok := helperMap[bucketTs]; ok && helper.firstByteCount > 0 {
			avgFBT := helper.totalFirstByteTime / float64(helper.firstByteCount)
			mp.AvgFirstByteTimeSeconds = new(float64)
			*mp.AvgFirstByteTimeSeconds = avgFBT
			mp.FirstByteSampleCount = helper.firstByteCount
		}

		if helper, ok := helperMap[bucketTs]; ok && helper.durationCount > 0 {
			avgDur := helper.totalDuration / float64(helper.durationCount)
			mp.AvgDurationSeconds = new(float64)
			*mp.AvgDurationSeconds = avgDur
			mp.DurationSampleCount = helper.durationCount
		}
	}

	out := []model.MetricPoint{}
	endTime := until.Truncate(bucket).Add(bucket)
	startTime := since.Truncate(bucket)

	for t := startTime; t.Before(endTime); t = t.Add(bucket) {
		ts := t.Unix()
		if mp, ok := mapp[ts]; ok {
			out = append(out, *mp)
		} else {
			out = append(out, model.MetricPoint{
				Ts:       t,
				Channels: make(map[string]model.ChannelMetric),
			})
		}
	}

	return out, nil
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
func (s *MySQLStore) GetDistinctModels(ctx context.Context, since, until time.Time) ([]string, error) {
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
			continue
		}
		models = append(models, model)
	}

	return models, nil
}
