package model

import (
	"slices"
	"strings"
	"time"
)

// Config 渠道配置
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

	// Key轮询指针（从key_rr表迁移）
	RRKeyIndex int `json:"rr_key_index"` // 当前轮询的Key索引（0-based）

	CreatedAt JSONTime `json:"created_at"` // 使用JSONTime确保序列化格式一致（RFC3339）
	UpdatedAt JSONTime `json:"updated_at"` // 使用JSONTime确保序列化格式一致（RFC3339）

	// 性能优化：模型查找索引（内存缓存，不序列化）
	modelsSet map[string]struct{} `json:"-"`
}

// GetChannelType 返回渠道类型（默认anthropic）
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

// NormalizeChannelType 规范化渠道类型命名
// 空值返回默认类型 anthropic，其他值原样返回（保持灵活性，支持未来扩展）
func NormalizeChannelType(t string) string {
	lower := strings.ToLower(strings.TrimSpace(t))
	if lower == "" {
		return "anthropic"
	}
	return lower
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
	Config  *Config  `json:"config"`   // 渠道基本配置
	APIKeys []APIKey `json:"api_keys"` // 关联的所有API Keys（不使用指针避免额外分配）
}
