// Package util 提供通用工具函数
package util

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ParseAPIKeys 解析 API Key 字符串（支持逗号分隔的多个 Key）
// 设计原则（DRY）：统一的Key解析逻辑，供多个模块复用
func ParseAPIKeys(apiKey string) []string {
	if apiKey == "" {
		return []string{}
	}
	parts := strings.Split(apiKey, ",")
	keys := make([]string, 0, len(parts))
	for _, k := range parts {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// MaskAPIKey 将API Key脱敏为 "abcd...klmn" 格式（前4位 + ... + 后4位）
func MaskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// HashAPIKey 计算API Key的SHA256哈希（十六进制字符串）
// 用于日志中稳定标识 key，不存储明文。
func HashAPIKey(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
