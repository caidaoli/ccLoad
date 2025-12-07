package mysql

import (
	"fmt"
	"strings"
)

// allowedFields SQL 查询允许的字段名白名单
var allowedFields = map[string]bool{
	// channels 表
	"id": true, "name": true, "url": true, "priority": true,
	"models": true, "model_redirects": true, "channel_type": true,
	"enabled": true, "cooldown_until": true, "cooldown_duration_ms": true,
	"created_at": true, "updated_at": true,

	// logs 表
	"time": true, "model": true, "channel_id": true, "status_code": true,
	"message": true, "duration": true, "is_streaming": true,
	"first_byte_time": true, "api_key_used": true,

	// api_keys 表
	"key_index": true, "api_key": true, "key_strategy": true,

	// 带表前缀的字段
	"c.id": true, "c.name": true, "c.priority": true, "c.models": true,
	"c.model_redirects": true, "c.channel_type": true, "c.enabled": true,
	"c.cooldown_until": true, "c.cooldown_duration_ms": true,
	"c.created_at": true, "c.updated_at": true,
}

func ValidateFieldName(field string) error {
	field = strings.TrimSpace(field)
	if !allowedFields[field] {
		return fmt.Errorf("invalid field name: %s (not in whitelist)", field)
	}
	return nil
}
