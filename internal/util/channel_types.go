package util

import "strings"

// ChannelTypeConfig 渠道类型配置（元数据定义）
type ChannelTypeConfig struct {
	Value        string   `json:"value"`         // 内部值（数据库存储）
	DisplayName  string   `json:"display_name"`  // 显示名称（前端展示）
	Description  string   `json:"description"`   // 描述信息
	PathPatterns []string `json:"path_patterns"` // 路径匹配模式列表
	MatchType    string   `json:"match_type"`    // 匹配类型: "prefix"(前缀) 或 "contains"(包含)
}

// ChannelTypes 全局渠道类型配置（单一数据源 - Single Source of Truth）
var ChannelTypes = []ChannelTypeConfig{
	{
		Value:        ChannelTypeAnthropic,
		DisplayName:  "Claude Code",
		Description:  "Claude Code兼容API",
		PathPatterns: []string{"/v1/messages"},
		MatchType:    MatchTypePrefix,
	},
	{
		Value:        ChannelTypeCodex,
		DisplayName:  "Codex",
		Description:  "Codex兼容API",
		PathPatterns: []string{"/v1/responses"},
		MatchType:    MatchTypePrefix,
	},
	{
		Value:        ChannelTypeOpenAI,
		DisplayName:  "OpenAI",
		Description:  "OpenAI API (GPT系列)",
		PathPatterns: []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/images/"},
		MatchType:    MatchTypePrefix,
	},
	{
		Value:        ChannelTypeGemini,
		DisplayName:  "Google Gemini",
		Description:  "Google Gemini API",
		PathPatterns: []string{"/v1beta/"},
		MatchType:    MatchTypeContains,
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

// 匹配类型常量（路径匹配方式）
const (
	MatchTypePrefix   = "prefix"   // 前缀匹配（strings.HasPrefix）
	MatchTypeContains = "contains" // 包含匹配（strings.Contains）
)

// DetectChannelTypeFromPath 根据请求路径自动检测渠道类型
// 使用 ChannelTypes 配置进行统一检测，遵循DRY原则
func DetectChannelTypeFromPath(path string) string {
	for _, ct := range ChannelTypes {
		if matchPath(path, ct.PathPatterns, ct.MatchType) {
			return ct.Value
		}
	}
	return "" // 未匹配到任何类型
}

// matchPath 辅助函数：根据匹配类型检查路径是否匹配模式列表
func matchPath(path string, patterns []string, matchType string) bool {
	for _, pattern := range patterns {
		switch matchType {
		case MatchTypePrefix:
			if strings.HasPrefix(path, pattern) {
				return true
			}
		case MatchTypeContains:
			if strings.Contains(path, pattern) {
				return true
			}
		}
	}
	return false
}
