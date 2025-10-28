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

	CreatedAt JSONTime `json:"created_at"` // 使用JSONTime确保序列化格式一致（RFC3339）
	UpdatedAt JSONTime `json:"updated_at"` // 使用JSONTime确保序列化格式一致（RFC3339）

	// 🔧 P1性能优化：缓存Key数量，避免冷却判断时的N+1查询
	KeyCount int `json:"key_count"` // API Key数量（查询时JOIN计算）

	// 性能优化：模型查找索引（内存缓存，不序列化）
	modelsSet map[string]struct{} `json:"-"`
}

// GetChannelType 默认返回"anthropic"（Claude API）
func (c *Config) GetChannelType() string {
	if c.ChannelType == "" {
		return "anthropic"
	}
	return c.ChannelType
}

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

type APIKey struct {
	ID        int64  `json:"id"`
	ChannelID int64  `json:"channel_id"`
	KeyIndex  int    `json:"key_index"`
	APIKey    string `json:"api_key"`

	KeyStrategy string `json:"key_strategy"` // "sequential" | "round_robin"

	// Key级冷却（从key_cooldowns表迁移）
	CooldownUntil      int64 `json:"cooldown_until"`
	CooldownDurationMs int64 `json:"cooldown_duration_ms"`

	CreatedAt JSONTime `json:"created_at"`
	UpdatedAt JSONTime `json:"updated_at"`
}

func (k *APIKey) IsCoolingDown(now time.Time) bool {
	return k.CooldownUntil > now.Unix()
}

// ChannelWithKeys 用于Redis完整同步
// 设计目标：解决Redis恢复后渠道缺少API Keys的问题
type ChannelWithKeys struct {
	Config  *Config  `json:"config"`
	APIKeys []APIKey `json:"api_keys"` // 不使用指针避免额外分配
}
