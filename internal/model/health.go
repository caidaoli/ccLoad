package model

// HealthScoreConfig 健康度排序配置
type HealthScoreConfig struct {
	Enabled                  bool    // 是否启用健康度排序
	SuccessRatePenaltyWeight float64 // 成功率惩罚权重(乘以失败率)
	WindowMinutes            int     // 成功率统计时间窗口(分钟)
	UpdateIntervalSeconds    int     // 成功率缓存更新间隔(秒)
}

// DefaultHealthScoreConfig 返回默认健康度配置
func DefaultHealthScoreConfig() HealthScoreConfig {
	return HealthScoreConfig{
		Enabled:                  false,
		SuccessRatePenaltyWeight: 100,
		WindowMinutes:            5,
		UpdateIntervalSeconds:    30,
	}
}
