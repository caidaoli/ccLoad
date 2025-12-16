package sql

import (
	"ccLoad/internal/model"
	"context"
	"fmt"
	"log"
	"time"
)

type metricAggregationHelper struct {
	totalFirstByteTime float64
	firstByteCount     int
	totalDuration      float64
	durationCount      int
}

func (s *SQLStore) finalizeMetricPoints(ctx context.Context, mapp map[int64]*model.MetricPoint, helperMap map[int64]*metricAggregationHelper, channelIDsToFetch map[int64]bool, since, until time.Time, bucket time.Duration) []model.MetricPoint {
	channelNames, err := s.fetchChannelNamesBatch(ctx, channelIDsToFetch)
	if err != nil {
		log.Printf("[WARN]  批量查询渠道名称失败: %v", err)
		channelNames = make(map[int64]string)
	}

	for bucketTs, mp := range mapp {
		newChannels := make(map[string]model.ChannelMetric)
		for key, metric := range mp.Channels {
			if key == "未知渠道" {
				newChannels[key] = metric
				continue
			}
			var channelID int64
			fmt.Sscanf(key, "ch_%d", &channelID)
			if name, ok := channelNames[channelID]; ok {
				newChannels[name] = metric
			} else {
				newChannels["未知渠道"] = metric
			}
		}
		mp.Channels = newChannels

		if helper, ok := helperMap[bucketTs]; ok {
			if helper.firstByteCount > 0 {
				avgFBT := helper.totalFirstByteTime / float64(helper.firstByteCount)
				mp.AvgFirstByteTimeSeconds = new(float64)
				*mp.AvgFirstByteTimeSeconds = avgFBT
				mp.FirstByteSampleCount = helper.firstByteCount
			}
			if helper.durationCount > 0 {
				avgDur := helper.totalDuration / float64(helper.durationCount)
				mp.AvgDurationSeconds = new(float64)
				*mp.AvgDurationSeconds = avgDur
				mp.DurationSampleCount = helper.durationCount
			}
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

	return out
}
