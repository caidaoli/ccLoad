package main

// ChannelTypeConfig 渠道类型配置（元数据定义）
type ChannelTypeConfig struct {
	Value       string `json:"value"`        // 内部值（数据库存储）
	DisplayName string `json:"display_name"` // 显示名称（前端展示）
	Description string `json:"description"`  // 描述信息
}

// ChannelTypes 全局渠道类型配置（单一数据源 - Single Source of Truth）
var ChannelTypes = []ChannelTypeConfig{
	{
		Value:       "anthropic",
		DisplayName: "Claude Code",
		Description: "Anthropic Claude API（Code风格）",
	},
	{
		Value:       "codex",
		DisplayName: "OpenAI",
		Description: "OpenAI兼容API",
	},
	{
		Value:       "gemini",
		DisplayName: "Google Gemini",
		Description: "Google Gemini API",
	},
}

// GetChannelTypeDisplayName 根据内部值获取显示名称
func GetChannelTypeDisplayName(value string) string {
	for _, ct := range ChannelTypes {
		if ct.Value == value {
			return ct.DisplayName
		}
	}
	return value // 回退到原始值
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

// GetDefaultChannelType 获取默认渠道类型
func GetDefaultChannelType() string {
	if len(ChannelTypes) > 0 {
		return ChannelTypes[0].Value
	}
	return "anthropic" // 最终回退
}
