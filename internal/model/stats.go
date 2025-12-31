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

// HealthPoint 健康状态数据点（用于健康状态指示器）
type HealthPoint struct {
	Ts                       time.Time `json:"ts"`                    // 时间点
	SuccessRate              float64   `json:"rate"`                  // 成功率 (0-1), -1表示无数据
	SuccessCount             int       `json:"success"`               // 成功次数
	ErrorCount               int       `json:"error"`                 // 失败次数
	AvgFirstByteTime         float64   `json:"avg_first_byte_time"`   // 平均首字时间(秒)
	AvgDuration              float64   `json:"avg_duration"`          // 平均耗时(秒)
	TotalInputTokens         int64     `json:"input_tokens"`          // 输入Token
	TotalOutputTokens        int64     `json:"output_tokens"`         // 输出Token
	TotalCacheReadTokens     int64     `json:"cache_read_tokens"`     // 缓存读取Token
	TotalCacheCreationTokens int64     `json:"cache_creation_tokens"` // 缓存创建Token
	TotalCost                float64   `json:"cost"`                  // 成本(美元)
}

// StatsEntry 统计数据条目
type StatsEntry struct {
	ChannelID               *int     `json:"channel_id,omitempty"`
	ChannelName             string   `json:"channel_name"`
	ChannelPriority         *int     `json:"channel_priority,omitempty"` // 渠道优先级（用于前端排序）
	Model                   string   `json:"model"`
	Success                 int      `json:"success"`
	Error                   int      `json:"error"`
	Total                   int      `json:"total"`
	AvgFirstByteTimeSeconds *float64 `json:"avg_first_byte_time_seconds,omitempty"` // 流式请求平均首字响应时间(秒)
	AvgDurationSeconds      *float64 `json:"avg_duration_seconds,omitempty"`        // 平均总耗时(秒)

	// RPM/QPS统计（基于分钟级数据）
	PeakRPM   *float64 `json:"peak_rpm,omitempty"`   // 峰值RPM（该渠道+模型的最大每分钟请求数）
	AvgRPM    *float64 `json:"avg_rpm,omitempty"`    // 平均RPM
	RecentRPM *float64 `json:"recent_rpm,omitempty"` // 最近一分钟RPM（仅本日有效）

	// Token统计（2025-11新增）
	TotalInputTokens              *int64   `json:"total_input_tokens,omitempty"`                // 总输入Token
	TotalOutputTokens             *int64   `json:"total_output_tokens,omitempty"`               // 总输出Token
	TotalCacheReadInputTokens     *int64   `json:"total_cache_read_input_tokens,omitempty"`     // 总缓存读取Token
	TotalCacheCreationInputTokens *int64   `json:"total_cache_creation_input_tokens,omitempty"` // 总缓存创建Token
	TotalCost                     *float64 `json:"total_cost,omitempty"`                        // 总成本（美元）

	// 健康状态时间线（2025-12新增）
	HealthTimeline []HealthPoint `json:"health_timeline,omitempty"` // 固定24个时间点的健康状态
}

// RPMStats 包含RPM/QPS相关的统计数据
type RPMStats struct {
	PeakRPM   float64 `json:"peak_rpm"`   // 峰值RPM（每分钟最大请求数）
	PeakQPS   float64 `json:"peak_qps"`   // 峰值QPS（每秒最大请求数）
	AvgRPM    float64 `json:"avg_rpm"`    // 平均RPM
	AvgQPS    float64 `json:"avg_qps"`    // 平均QPS
	RecentRPM float64 `json:"recent_rpm"` // 最近一分钟RPM（仅本日有效）
	RecentQPS float64 `json:"recent_qps"` // 最近一分钟QPS（仅本日有效）
}
