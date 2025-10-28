package model

import "time"

// MetricPoint 指标数据点（用于趋势图）
type MetricPoint struct {
	Ts       time.Time                `json:"ts"`
	Success  int                      `json:"success"`
	Error    int                      `json:"error"`
	Channels map[string]ChannelMetric `json:"channels,omitempty"`
}

// ChannelMetric 单个渠道的指标
type ChannelMetric struct {
	Success int `json:"success"`
	Error   int `json:"error"`
}

// StatsEntry 统计数据条目
type StatsEntry struct {
	ChannelID               *int     `json:"channel_id,omitempty"`
	ChannelName             string   `json:"channel_name"`
	Model                   string   `json:"model"`
	Success                 int      `json:"success"`
	Error                   int      `json:"error"`
	Total                   int      `json:"total"`
	AvgFirstByteTimeSeconds *float64 `json:"avg_first_byte_time_seconds,omitempty"` // 流式请求平均首字响应时间(秒)
}
