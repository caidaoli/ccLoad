package util

import "strings"

// ChannelTypeConfig 渠道类型配置（元数据定义）
type ChannelTypeConfig struct {
	Value       string `json:"value"`        // 内部值（数据库存储）
	DisplayName string `json:"display_name"` // 显示名称（前端展示）
	Description string `json:"description"`  // 描述信息
}

// ChannelTypes 全局渠道类型配置（单一数据源 - Single Source of Truth）
var ChannelTypes = []ChannelTypeConfig{
	{
		Value:       ChannelTypeAnthropic,
		DisplayName: "Claude Code",
		Description: "Claude Code兼容API",
	},
	{
		Value:       ChannelTypeCodex,
		DisplayName: "Codex",
		Description: "Codex兼容API",
	},
	{
		Value:       ChannelTypeOpenAI,
		DisplayName: "OpenAI",
		Description: "OpenAI API (GPT系列)",
	},
	{
		Value:       ChannelTypeGemini,
		DisplayName: "Google Gemini",
		Description: "Google Gemini API",
	},
}

// IsValidChannelType 验证渠道类型是否有效（替代models.go中的硬编码）
func IsValidChannelType(value string) bool {
	for _, ct := range ChannelTypes {
		if ct.Value == value {
			return true
		}
	}
	return false
}

// NormalizeChannelType 规范化渠道类型（兼容性处理）
// - 去除首尾空格
// - 转小写
// - 空值 → "anthropic" (默认值)
func NormalizeChannelType(value string) string {
	// 去除首尾空格
	value = strings.TrimSpace(value)

	// 空值返回默认值
	if value == "" {
		return "anthropic"
	}

	// 转小写
	return strings.ToLower(value)
}

// 渠道类型常量（导出供其他包使用，遵循DRY原则）
const (
	ChannelTypeAnthropic = "anthropic"
	ChannelTypeCodex     = "codex"
	ChannelTypeOpenAI    = "openai"
	ChannelTypeGemini    = "gemini"
)
