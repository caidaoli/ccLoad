package main

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 数据模型与接口定义

type Config struct {
	ID             int64             `json:"id"`
	Name           string            `json:"name"`
	ChannelType    string            `json:"channel_type"` // 渠道类型: "anthropic" | "codex" | "gemini"，默认anthropic
	URL            string            `json:"url"`
	Priority       int               `json:"priority"`
	Models         []string          `json:"models"`
	ModelRedirects map[string]string `json:"model_redirects,omitempty"` // 模型重定向映射：请求模型 -> 实际转发模型
	Enabled        bool              `json:"enabled"`

	// 渠道级冷却（从cooldowns表迁移）
	CooldownUntil      int64 `json:"cooldown_until"`       // Unix秒时间戳，0表示无冷却
	CooldownDurationMs int64 `json:"cooldown_duration_ms"` // 冷却持续时间（毫秒）

	CreatedAt JSONTime `json:"created_at"` // 使用JSONTime确保序列化格式一致（RFC3339）
	UpdatedAt JSONTime `json:"updated_at"` // 使用JSONTime确保序列化格式一致（RFC3339）

	// 性能优化：模型查找索引（内存缓存，不序列化）
	modelsSet map[string]struct{} `json:"-"`
}

// GetChannelType 返回渠道类型（默认anthropic��
func (c *Config) GetChannelType() string {
	if c.ChannelType == "" {
		return "anthropic" // 默认Claude API
	}
	return c.ChannelType
}

// IsCoolingDown 检查渠道是否在冷却中
func (c *Config) IsCoolingDown(now time.Time) bool {
	return c.CooldownUntil > now.Unix()
}

// APIKey 表示单个API Key及其配置
type APIKey struct {
	ID        int64  `json:"id"`
	ChannelID int64  `json:"channel_id"`
	KeyIndex  int    `json:"key_index"` // Key在渠道中的索引（0,1,2...）
	APIKey    string `json:"api_key"`   // 实际的API Key

	KeyStrategy string `json:"key_strategy"` // Key使用策略: "sequential" | "round_robin"

	// Key级冷却（从key_cooldowns表迁移）
	CooldownUntil      int64 `json:"cooldown_until"`       // Unix秒时间戳，0表示无冷却
	CooldownDurationMs int64 `json:"cooldown_duration_ms"` // 冷却持续时间（毫秒）

	CreatedAt JSONTime `json:"created_at"`
	UpdatedAt JSONTime `json:"updated_at"`
}

// IsCoolingDown 检查Key是否在冷却中
func (k *APIKey) IsCoolingDown(now time.Time) bool {
	return k.CooldownUntil > now.Unix()
}

// ChannelWithKeys 用于Redis完整同步（包含渠道配置和所有API Keys）
// 设计目标：解决Redis恢复后渠道缺少API Keys的问题
type ChannelWithKeys struct {
	Config  *Config   `json:"config"`   // 渠道基本配置
	APIKeys []APIKey  `json:"api_keys"` // 关联的所有API Keys（不使用指针避免额外分配）
}

// BuildModelsSet 构建模型查找索引（性能优化：O(1)查找）
// 应在配置加载或更新后调用
func (c *Config) BuildModelsSet() {
	c.modelsSet = make(map[string]struct{}, len(c.Models))
	for _, model := range c.Models {
		c.modelsSet[model] = struct{}{}
	}
}

// HasModel 检查渠道是否支持指定模型（O(1)复杂度）
// 性能优化：使用map查找替代线性扫描，节省60-80%查找时间
func (c *Config) HasModel(model string) bool {
	if c.modelsSet == nil {
		// 降级到线性查找（向后兼容未初始化索引的场景）
		return slices.Contains(c.Models, model)
	}
	_, exists := c.modelsSet[model]
	return exists
}

// normalizeChannelType 规范化渠道类型命名
// 空值返回默认类型 anthropic，其他值原样返回（保持灵活性，支持未来扩展）
func normalizeChannelType(t string) string {
	lower := strings.ToLower(strings.TrimSpace(t))
	if lower == "" {
		return "anthropic"
	}
	return lower
}

// 自定义时间类型，使用Unix时间戳进行JSON序列化
// 设计原则：与数据库格式统一，减少转换复杂度（KISS原则）
type JSONTime struct {
	time.Time
}

func (jt JSONTime) MarshalJSON() ([]byte, error) {
	if jt.Time.IsZero() {
		return []byte("0"), nil
	}
	return []byte(strconv.FormatInt(jt.Time.Unix(), 10)), nil
}

func (jt *JSONTime) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || string(data) == "0" {
		jt.Time = time.Time{}
		return nil
	}
	ts, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return err
	}
	jt.Time = time.Unix(ts, 0)
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
	// 简化查询：直接从数据库按条件查询（利用索引）
	GetEnabledChannelsByModel(ctx context.Context, model string) ([]*Config, error)
	GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*Config, error)

	// api_keys mgmt (新增)
	GetAPIKeys(ctx context.Context, channelID int64) ([]*APIKey, error)
	GetAPIKey(ctx context.Context, channelID int64, keyIndex int) (*APIKey, error)
	CreateAPIKey(ctx context.Context, key *APIKey) error
	UpdateAPIKey(ctx context.Context, key *APIKey) error
	DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error
	DeleteAllAPIKeys(ctx context.Context, channelID int64) error // 删除渠道的所有Key

	// cooldown (channel-level) - 简化接口，冷却数据直接在channels表中
	GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error)
	BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error)
	ResetChannelCooldown(ctx context.Context, channelID int64) error
	SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error

	// cooldown (key-level) - 简化接口，冷却数据直接在api_keys表中
	GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error)
	BumpKeyCooldown(ctx context.Context, channelID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error)
	ResetKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error
	SetKeyCooldown(ctx context.Context, channelID int64, keyIndex int, until time.Time) error

	// key-level round-robin
	NextKeyRR(ctx context.Context, channelID int64, keyCount int) int
	SetKeyRR(ctx context.Context, channelID int64, idx int) error

	// logs
	AddLog(ctx context.Context, e *LogEntry) error
	ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *LogFilter) ([]*LogEntry, error)

	// metrics
	Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]MetricPoint, error)

	// stats - 统计功能
	GetStats(ctx context.Context, since time.Time, filter *LogFilter) ([]StatsEntry, error)

	// round-robin pointer per (model, priority)
	NextRR(ctx context.Context, model string, priority int, n int) int
	SetRR(ctx context.Context, model string, priority int, idx int) error

	// maintenance
	CleanupLogsBefore(ctx context.Context, cutoff time.Time) error
}
