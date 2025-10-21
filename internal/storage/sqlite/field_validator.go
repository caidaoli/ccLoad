package sqlite

import (
	"fmt"
	"strings"
)

// allowedFields SQL 查询允许的字段名白名单
// 安全原则：显式枚举所有合法字段，拒绝其他输入
var allowedFields = map[string]bool{
	// channels 表
	"id":                   true,
	"name":                 true,
	"url":                  true,
	"priority":             true,
	"models":               true,
	"model_redirects":      true,
	"channel_type":         true,
	"enabled":              true,
	"cooldown_until":       true,
	"cooldown_duration_ms": true,
	"created_at":           true,
	"updated_at":           true,

	// logs 表
	"time":            true,
	"model":           true,
	"channel_id":      true,
	"status_code":     true,
	"message":         true,
	"duration":        true,
	"is_streaming":    true,
	"first_byte_time": true,
	"api_key_used":    true,

	// api_keys 表
	"key_index":    true,
	"api_key":      true,
	"key_strategy": true,

	// 带表前缀的字段（用于 JOIN 查询）
	"c.id":                   true,
	"c.name":                 true,
	"c.priority":             true,
	"c.models":               true,
	"c.model_redirects":      true,
	"c.channel_type":         true,
	"c.enabled":              true,
	"c.cooldown_until":       true,
	"c.cooldown_duration_ms": true,
	"c.created_at":           true,
	"c.updated_at":           true,
}

// ValidateFieldName 验证字段名是否在白名单中
// 返回 error 如果字段名非法
func ValidateFieldName(field string) error {
	// 标准化字段名（去除空格）
	field = strings.TrimSpace(field)

	// 检查白名单
	if !allowedFields[field] {
		return fmt.Errorf("invalid field name: %s (not in whitelist)", field)
	}

	return nil
}

// ValidateMultipleFields 批量验证字段名
func ValidateMultipleFields(fields ...string) error {
	for _, field := range fields {
		if err := ValidateFieldName(field); err != nil {
			return err
		}
	}
	return nil
}

// SanitizeOrderBy 验证并消毒 ORDER BY 子句
// 仅允许：字段名 [ASC|DESC]
func SanitizeOrderBy(orderBy string) (string, error) {
	if orderBy == "" {
		return "", nil
	}

	// 分割多个排序字段
	parts := strings.Split(orderBy, ",")
	sanitized := make([]string, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)

		// 分割字段名和排序方向
		tokens := strings.Fields(part)
		if len(tokens) == 0 || len(tokens) > 2 {
			return "", fmt.Errorf("invalid ORDER BY clause: %s", part)
		}

		// 验证字段名
		field := tokens[0]
		if err := ValidateFieldName(field); err != nil {
			return "", fmt.Errorf("invalid field in ORDER BY: %w", err)
		}

		// 验证排序方向（可选）
		if len(tokens) == 2 {
			direction := strings.ToUpper(tokens[1])
			if direction != "ASC" && direction != "DESC" {
				return "", fmt.Errorf("invalid ORDER BY direction: %s (must be ASC or DESC)", tokens[1])
			}
			sanitized = append(sanitized, field+" "+direction)
		} else {
			sanitized = append(sanitized, field)
		}
	}

	return strings.Join(sanitized, ", "), nil
}
