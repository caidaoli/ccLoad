package model

import "time"

// MetricPoint 指标数据点（用于趋势图）
type MetricPoint struct {
	Ts                      time.Time                `json:"ts"`
	Success                 int                      `json:"success"`
	Error                   int                      `json:"error"`
	AvgFirstByteTimeSeconds *float64                 `json:"avg_first_byte_time_seconds,omitempty"` // 平均首字响应时间(秒)
	AvgDurationSeconds      *float64                 `json:"avg_duration_seconds,omitempty"`        // 平均总耗时(秒)
	TotalCost               *float64                 `json:"total_cost,omitempty"`                  // 总费用（美元）
	FirstByteSampleCount    int                      `json:"first_byte_count,omitempty"`            // 首字响应样本数（流式成功且有首字时间）
	DurationSampleCount     int                      `json:"duration_count,omitempty"`              // 总耗时样本数（成功且有耗时）
	InputTokens             int64                    `json:"input_tokens,omitempty"`                // 输入Token
	OutputTokens            int64                    `json:"output_tokens,omitempty"`               // 输出Token
	CacheReadTokens         int64                    `json:"cache_read_tokens,omitempty"`           // 缓存读取Token
	CacheCreationTokens     int64                    `json:"cache_creation_tokens,omitempty"`       // 缓存创建Token
	Channels                map[string]ChannelMetric `json:"channels,omitempty"`
}

// ChannelMetric 单个渠道的指标
type ChannelMetric struct {
	Success                 int      `json:"success"`
	Error                   int      `json:"error"`
	AvgFirstByteTimeSeconds *float64 `json:"avg_first_byte_time_seconds,omitempty"` // 平均首字响应时间(秒)
	AvgDurationSeconds      *float64 `json:"avg_duration_seconds,omitempty"`        // 平均总耗时(秒)
	TotalCost               *float64 `json:"total_cost,omitempty"`                  // 总费用（美元）
	InputTokens             int64    `json:"input_tokens,omitempty"`                // 输入Token
	OutputTokens            int64    `json:"output_tokens,omitempty"`               // 输出Token
	CacheReadTokens         int64    `json:"cache_read_tokens,omitempty"`           // 缓存读取Token
	CacheCreationTokens     int64    `json:"cache_creation_tokens,omitempty"`       // 缓存创建Token
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
	AvgDurationSeconds      *float64 `json:"avg_duration_seconds,omitempty"`        // 平均总耗时(秒)

	// Token统计（2025-11新增）
	TotalInputTokens              *int64   `json:"total_input_tokens,omitempty"`                // 总输入Token
	TotalOutputTokens             *int64   `json:"total_output_tokens,omitempty"`               // 总输出Token
	TotalCacheReadInputTokens     *int64   `json:"total_cache_read_input_tokens,omitempty"`     // 总缓存读取Token
	TotalCacheCreationInputTokens *int64   `json:"total_cache_creation_input_tokens,omitempty"` // 总缓存创建Token
	TotalCost                     *float64 `json:"total_cost,omitempty"`                        // 总成本（美元）
}
