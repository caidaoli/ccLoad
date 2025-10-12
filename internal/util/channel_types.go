package util

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
		Description: "Claude Code兼容API",
	},
	{
		Value:       "codex",
		DisplayName: "Codex",
		Description: "Codex兼容API",
	},
	{
		Value:       "openai",
		DisplayName: "OpenAI",
		Description: "OpenAI API (GPT系列)",
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

// NormalizeChannelType 规范化渠道类型（兼容性处理）
// - 去除首尾空格
// - 转小写
// - 空值 → "anthropic" (默认值)
func NormalizeChannelType(value string) string {
	// 去除首尾空格
	value = trimSpace(value)

	// 空值返回默认值
	if value == "" {
		return "anthropic"
	}

	// 转小写
	return toLowerCase(value)
}

// trimSpace 去除字符串首尾空格（手动实现，避免引入strings包）
func trimSpace(s string) string {
	start := 0
	end := len(s)

	// 找到第一个非空格字符
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}

	// 找到最后一个非空格字符
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}

	return s[start:end]
}

// toLowerCase 将字符串转为小写（手动实现，避免引入strings包）
func toLowerCase(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			result[i] = c + ('a' - 'A')
		} else {
			result[i] = c
		}
	}
	return string(result)
}
