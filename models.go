package main

import (
	"context"
	"time"
)

// 数据模型与接口定义

type Config struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	APIKey    string    `json:"api_key"`
	URL       string    `json:"url"`
	Priority  int       `json:"priority"`
	Models    []string  `json:"models"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type LogEntry struct {
	ID          int64     `json:"id"`
	Time        time.Time `json:"time"`
	Model       string    `json:"model"`
	ChannelID   *int64    `json:"channel_id,omitempty"`
	ChannelName string    `json:"channel_name,omitempty"`
	StatusCode  int       `json:"status_code"`
	Message     string    `json:"message"`
	Duration    float64   `json:"duration"` // 耗时（秒）
}

// 日志查询过滤条件
type LogFilter struct {
	ChannelID       *int64
	ChannelName     string
	ChannelNameLike string
	Model           string
	ModelLike       string
}

type MetricPoint struct {
	Ts      time.Time `json:"ts"`
	Success int       `json:"success"`
	Error   int       `json:"error"`
}

// 统计数据结构
type StatsEntry struct {
	ChannelName string `json:"channel_name"`
	Model       string `json:"model"`
	Success     int    `json:"success"`
	Error       int    `json:"error"`
	Total       int    `json:"total"`
}

// Store 接口
type Store interface {
	// config mgmt
	ListConfigs(ctx context.Context) ([]*Config, error)
	GetConfig(ctx context.Context, id int64) (*Config, error)
	CreateConfig(ctx context.Context, c *Config) (*Config, error)
	UpdateConfig(ctx context.Context, id int64, upd *Config) (*Config, error)
	DeleteConfig(ctx context.Context, id int64) error

	// cooldown
	GetCooldownUntil(ctx context.Context, configID int64) (time.Time, bool)
	SetCooldown(ctx context.Context, configID int64, until time.Time) error
	// 指数退避：错误时翻倍，成功时清零
	BumpCooldownOnError(ctx context.Context, configID int64, now time.Time) (time.Duration, error)
	ResetCooldown(ctx context.Context, configID int64) error

	// logs
	AddLog(ctx context.Context, e *LogEntry) error
	ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *LogFilter) ([]*LogEntry, error)

	// metrics
	Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]MetricPoint, error)

	// stats - 新增统计功能
	GetStats(ctx context.Context, since time.Time, filter *LogFilter) ([]StatsEntry, error)

	// round-robin pointer per (model, priority)
	NextRR(ctx context.Context, model string, priority int, n int) int
}
