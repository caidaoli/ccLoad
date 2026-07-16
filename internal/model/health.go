package model

// ChannelHealthStats 渠道健康统计数据
type ChannelHealthStats struct {
	SuccessRate          float64 // 成功率 0-1
	SampleCount          int64   // 样本量（健康度口径请求数）
	AvgFirstByteSeconds  float64 // 成功请求平均首字节时间（秒），无样本时为 0
	FirstByteSampleCount int64   // 有效首字节样本数
}

// HealthScoreConfig 健康度排序配置
type HealthScoreConfig struct {
	Enabled                  bool    // 是否启用健康度排序
	SuccessRatePenaltyWeight int     // 成功率惩罚权重(乘以失败率)
	WindowMinutes            int     // 成功率统计时间窗口(分钟)
	UpdateIntervalSeconds    int     // 成功率缓存更新间隔(秒)
	MinConfidentSample       int     // 置信样本量阈值（样本量达到此值时惩罚全额生效）
	EnableTTFBScore          bool    // 是否启用首字相对惩罚
	TTFBPenaltyWeight        float64 // 首字惩罚权重 W_ttfb
	TTFBMaxSlowRatio         float64 // (s-1) 上限 S_max
	TTFBMinConfidentSample   int     // 首字置信样本量阈值
}

// DefaultHealthScoreConfig 返回默认健康度配置
func DefaultHealthScoreConfig() HealthScoreConfig {
	return HealthScoreConfig{
		Enabled:                  false,
		SuccessRatePenaltyWeight: 100,
		WindowMinutes:            5,
		UpdateIntervalSeconds:    30,
		MinConfidentSample:       20, // 默认20次请求才全额惩罚
		EnableTTFBScore:          false,
		TTFBPenaltyWeight:        20,
		TTFBMaxSlowRatio:         2.0,
		TTFBMinConfidentSample:   10,
	}
}
