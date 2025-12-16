package sql

import (
	"ccLoad/internal/model"
	"database/sql"
	"fmt"
	"time"
)

func scanAggregatedMetricsRows(rows *sql.Rows) (map[int64]*model.MetricPoint, map[int64]*metricAggregationHelper, map[int64]bool, error) {
	mapp := make(map[int64]*model.MetricPoint)
	channelIDsToFetch := make(map[int64]bool)
	helperMap := make(map[int64]*metricAggregationHelper)

	for rows.Next() {
		var bucketTsFloat float64
		var channelID sql.NullInt64
		var success, errorCount int
		var avgFirstByteTime sql.NullFloat64
		var avgDuration sql.NullFloat64
		var streamSuccessFirstByteCount int
		var durationSuccessCount int
		var totalCost float64
		var inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int64

		if err := rows.Scan(&bucketTsFloat, &channelID, &success, &errorCount, &avgFirstByteTime, &avgDuration, &streamSuccessFirstByteCount, &durationSuccessCount, &totalCost, &inputTokens, &outputTokens, &cacheReadTokens, &cacheCreationTokens); err != nil {
			return nil, nil, nil, err
		}
		bucketTs := int64(bucketTsFloat)

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
			helper = &metricAggregationHelper{}
			helperMap[bucketTs] = helper
		}

		mp.Success += success
		mp.Error += errorCount

		if mp.TotalCost == nil {
			mp.TotalCost = new(float64)
		}
		*mp.TotalCost += totalCost

		// 累加 token 统计
		mp.InputTokens += inputTokens
		mp.OutputTokens += outputTokens
		mp.CacheReadTokens += cacheReadTokens
		mp.CacheCreationTokens += cacheCreationTokens

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
			InputTokens:             inputTokens,
			OutputTokens:            outputTokens,
			CacheReadTokens:         cacheReadTokens,
			CacheCreationTokens:     cacheCreationTokens,
		}
	}

	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	return mapp, helperMap, channelIDsToFetch, nil
}
