package util

import "strings"

// NormalizeProtocol 规范化协议名。空值保持为空，调用方必须显式决定默认行为。
func NormalizeProtocol(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// 支持的协议名（string 形式，供不便引入 protocol.Protocol 类型的调用点使用；
// 权威枚举是 protocol.AllProtocols）。
const (
	ProtocolAnthropic = "anthropic"
	ProtocolCodex     = "codex"
	ProtocolOpenAI    = "openai"
	ProtocolGemini    = "gemini"
)
