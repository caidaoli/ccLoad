package app

import (
	"fmt"
	"runtime"
)

// Metrics 系统监控指标
type Metrics struct {
	// Goroutine 指标
	NumGoroutines int64 `json:"num_goroutines"`

	// 日志系统指标
	LogChannelSize int64 `json:"log_channel_size"` // 当前队列中的日志数
	LogDropCount   int64 `json:"log_drop_count"`   // 累计丢弃的日志数

	// 冷却指标
	ChannelCooldowns int64 `json:"channel_cooldowns"` // 当前冷却的渠道数
	KeyCooldowns     int64 `json:"key_cooldowns"`     // 当前冷却的Key数

	// 并发指标
	ActiveRequests int64 `json:"active_requests"` // 当前活跃请求数
	MaxConcurrency int64 `json:"max_concurrency"` // 最大并发数
}

// GetMetrics 获取当前系统指标（用于监控和诊断）
func (s *Server) GetMetrics() *Metrics {
	return &Metrics{
		NumGoroutines:    int64(runtime.NumGoroutine()),
		LogChannelSize:   int64(len(s.logChan)),
		LogDropCount:     s.logDropCount.Load(),
		ChannelCooldowns: s.channelCooldownGauge.Load(),
		KeyCooldowns:     s.keyCooldownGauge.Load(),
		ActiveRequests:   int64(len(s.concurrencySem)),
		MaxConcurrency:   int64(s.maxConcurrency),
	}
}

// CheckHealth 健康检查（检测潜在的资源泄漏）
func (s *Server) CheckHealth() *HealthStatus {
	metrics := s.GetMetrics()

	status := &HealthStatus{
		Healthy: true,
		Metrics: metrics,
		Warnings: make([]string, 0),
	}

	// 检查1：Goroutine数量异常
	if metrics.NumGoroutines > 1000 {
		status.Warnings = append(status.Warnings, 
			fmt.Sprintf("⚠️ Goroutine数量异常: %d (正常<1000)", metrics.NumGoroutines))
		if metrics.NumGoroutines > 5000 {
			status.Healthy = false
		}
	}

	// 检查2：日志队列积压
	logQueueUsage := float64(metrics.LogChannelSize) / float64(cap(s.logChan)) * 100
	if logQueueUsage > 80 {
		status.Warnings = append(status.Warnings,
			fmt.Sprintf("⚠️ 日志队列积压: %.1f%% (阈值80%%)", logQueueUsage))
	}

	// 检查3：日志丢弃严重
	if metrics.LogDropCount > 10000 {
		status.Warnings = append(status.Warnings,
			fmt.Sprintf("⚠️ 日志丢弃严重: %d 条 (阈值10000)", metrics.LogDropCount))
		if metrics.LogDropCount > 100000 {
			status.Healthy = false
		}
	}

	// 检查4：并发槽位耗尽
	concurrencyUsage := float64(metrics.ActiveRequests) / float64(metrics.MaxConcurrency) * 100
	if concurrencyUsage > 90 {
		status.Warnings = append(status.Warnings,
			fmt.Sprintf("⚠️ 并发槽位告急: %.1f%% (阈值90%%)", concurrencyUsage))
	}

	return status
}

// HealthStatus 健康状态
type HealthStatus struct {
	Healthy  bool      `json:"healthy"`
	Metrics  *Metrics  `json:"metrics"`
	Warnings []string  `json:"warnings,omitempty"`
}
