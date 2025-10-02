package main

import (
	"context"
	"strings"
	"time"
)

// 数据模型与接口定义

type Config struct {
	ID             int64             `json:"id"`
	Name           string            `json:"name"`
	APIKey         string            `json:"api_key"`          // 向后兼容：单Key场景
	APIKeys        []string          `json:"api_keys"`         // 多Key支持：逗号分割的Key数组
	KeyStrategy    string            `json:"key_strategy"`     // Key使用策略: "sequential"（顺序） | "round_robin"（轮询），默认顺序
	ChannelType    string            `json:"channel_type"`     // 渠道类型: "anthropic" | "openai" | "gemini"，默认anthropic
	URL            string            `json:"url"`
	Priority       int               `json:"priority"`
	Models         []string          `json:"models"`
	ModelRedirects map[string]string `json:"model_redirects,omitempty"` // 模型重定向映射：请求模型 -> 实际转发模型
	Enabled        bool              `json:"enabled"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// GetAPIKeys 返回标准化的API Key列表
// 规则：
// 1. 优先使用APIKeys数组
// 2. 如果APIKeys为空且APIKey不为空，将APIKey拆分（兼容旧数据）
// 3. 支持逗号分割的多个Key（去除空格）
func (c *Config) GetAPIKeys() []string {
	// 优先使用新字段APIKeys
	if len(c.APIKeys) > 0 {
		return c.APIKeys
	}

	// 向后兼容：从旧字段APIKey解析
	if c.APIKey == "" {
		return []string{}
	}

	// 支持逗号分割的多Key
	keys := strings.Split(c.APIKey, ",")
	result := make([]string, 0, len(keys))
	for _, k := range keys {
		trimmed := strings.TrimSpace(k)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// GetKeyStrategy 返回Key使用策略（默认顺序）
func (c *Config) GetKeyStrategy() string {
	if c.KeyStrategy == "" {
		return "sequential" // 默认顺序访问
	}
	return c.KeyStrategy
}

// GetChannelType 返回渠道类型（默认anthropic）
func (c *Config) GetChannelType() string {
	if c.ChannelType == "" {
		return "anthropic" // 默认Claude API
	}
	return c.ChannelType
}

// IsValidChannelType 验证渠道类型是否合法
func IsValidChannelType(t string) bool {
	switch t {
	case "anthropic", "openai", "gemini":
		return true
	default:
		return false
	}
}

// 自定义时间类型，强制使用RFC3339格式进行JSON序列化
type JSONTime struct {
	time.Time
}

func (jt JSONTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + jt.Time.Format(time.RFC3339) + `"`), nil
}

func (jt *JSONTime) UnmarshalJSON(data []byte) error {
	str := string(data)
	if str == "null" {
		return nil
	}
	str = str[1 : len(str)-1] // 去掉引号
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return err
	}
	jt.Time = t
	return nil
}

type LogEntry struct {
	ID            int64    `json:"id"`
	Time          JSONTime `json:"time"`
	Model         string   `json:"model"`
	ChannelID     *int64   `json:"channel_id,omitempty"`
	ChannelName   string   `json:"channel_name,omitempty"`
	StatusCode    int      `json:"status_code"`
	Message       string   `json:"message"`
	Duration      float64  `json:"duration"`                  // 总耗时（秒）
	IsStreaming   bool     `json:"is_streaming"`              // 是否为流式请求
	FirstByteTime *float64 `json:"first_byte_time,omitempty"` // 首字节响应时间（秒）
	APIKeyUsed    string   `json:"api_key_used,omitempty"`    // 使用的API Key（查询时自动脱敏为 abcd...klmn 格式）
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
	Ts       time.Time                `json:"ts"`
	Success  int                      `json:"success"`
	Error    int                      `json:"error"`
	Channels map[string]ChannelMetric `json:"channels,omitempty"`
}

type ChannelMetric struct {
	Success int `json:"success"`
	Error   int `json:"error"`
}

// 统计数据结构
type StatsEntry struct {
	ChannelID   *int   `json:"channel_id,omitempty"`
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
	ReplaceConfig(ctx context.Context, c *Config) (*Config, error)

	// cooldown (channel-level)
	GetCooldownUntil(ctx context.Context, configID int64) (time.Time, bool)
	SetCooldown(ctx context.Context, configID int64, until time.Time) error
	// 指数退避：错误时翻倍，成功时清零
	BumpCooldownOnError(ctx context.Context, configID int64, now time.Time) (time.Duration, error)
	ResetCooldown(ctx context.Context, configID int64) error

	// key-level cooldown (新增)
	GetKeyCooldownUntil(ctx context.Context, configID int64, keyIndex int) (time.Time, bool)
	BumpKeyCooldownOnError(ctx context.Context, configID int64, keyIndex int, now time.Time) (time.Duration, error)
	ResetKeyCooldown(ctx context.Context, configID int64, keyIndex int) error

	// key-level round-robin (新增)
	NextKeyRR(ctx context.Context, configID int64, keyCount int) int
	SetKeyRR(ctx context.Context, configID int64, idx int) error

	// logs
	AddLog(ctx context.Context, e *LogEntry) error
	ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *LogFilter) ([]*LogEntry, error)

	// metrics
	Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]MetricPoint, error)

	// stats - 新增统计功能
	GetStats(ctx context.Context, since time.Time, filter *LogFilter) ([]StatsEntry, error)

	// round-robin pointer per (model, priority)
	NextRR(ctx context.Context, model string, priority int, n int) int
	SetRR(ctx context.Context, model string, priority int, idx int) error
}
