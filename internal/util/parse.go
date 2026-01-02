package util

import "strings"

// ParseBool 解析常见的布尔字符串表示
// 返回 (value, ok)：ok 表示是否为有效的布尔值
func ParseBool(raw string) (bool, bool) {
	val := strings.TrimSpace(strings.ToLower(raw))
	switch val {
	case "1", "true", "yes", "y", "启用", "enabled", "on":
		return true, true
	case "0", "false", "no", "n", "禁用", "disabled", "off":
		return false, true
	default:
		return false, false
	}
}

// ParseBoolDefault 解析布尔字符串，无效值时返回默认值
func ParseBoolDefault(raw string, defaultVal bool) bool {
	if val, ok := ParseBool(raw); ok {
		return val
	}
	return defaultVal
}
