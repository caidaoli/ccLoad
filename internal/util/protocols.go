package util

import "strings"

// ProtocolConfig 描述前端可选择的线协议。
type ProtocolConfig struct {
	Value       string `json:"value"`
	DisplayName string `json:"display_name"` // 显示名称（前端展示）
	Description string `json:"description"`  // 描述信息
}

// Protocols 是网关支持的四种线协议。
var Protocols = []ProtocolConfig{
	{
		Value:       ProtocolAnthropic,
		DisplayName: "Claude Code",
		Description: "Claude Code兼容API",
	},
	{
		Value:       ProtocolCodex,
		DisplayName: "Codex",
		Description: "Codex兼容API",
	},
	{
		Value:       ProtocolOpenAI,
		DisplayName: "OpenAI",
		Description: "OpenAI API (GPT系列)",
	},
	{
		Value:       ProtocolGemini,
		DisplayName: "Google Gemini",
		Description: "Google Gemini API",
	},
}

// IsValidProtocol 验证协议名是否受支持。
func IsValidProtocol(value string) bool {
	for _, protocol := range Protocols {
		if protocol.Value == value {
			return true
		}
	}
	return false
}

// NormalizeProtocol 规范化协议名。空值保持为空，调用方必须显式决定默认行为。
func NormalizeProtocol(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// 支持的协议名。
const (
	ProtocolAnthropic = "anthropic"
	ProtocolCodex     = "codex"
	ProtocolOpenAI    = "openai"
	ProtocolGemini    = "gemini"
)
