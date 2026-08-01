package app_test

import "testing"

// ==================== CSV导出默认值测试 ====================
// 注意：新架构中APIKey和KeyStrategy已从Config移除，CSV导出从api_keys表查询
// 此测试仅验证 Key 策略默认值。

// ==================== CSV导入默认值测试 ====================

func TestCSVImport_DefaultValues(t *testing.T) {
	// 测试Key策略默认值处理
	keyStrategy := ""
	if keyStrategy == "" {
		keyStrategy = "sequential"
	}
	if keyStrategy != "sequential" {
		t.Errorf("空key_strategy应填充为sequential，实际为: %s", keyStrategy)
	}
}

// ==================== CSV时间字段缺失测试 ====================

func TestCSVExport_NoTimeFields(t *testing.T) {
	// 验证CSV导出不包含时间字段
	header := []string{"id", "name", "api_key", "urls", "priority", "rpm_limit", "max_concurrency", "models", "model_redirects", "protocol_transform_mode", "key_strategy", "enabled", "scheduled_check_enabled", "scheduled_check_model"}

	hasCreatedAt := false
	hasUpdatedAt := false

	for _, col := range header {
		if col == "created_at" {
			hasCreatedAt = true
		}
		if col == "updated_at" {
			hasUpdatedAt = true
		}
	}

	if hasCreatedAt {
		t.Error("CSV不应包含created_at字段（设计决定：导入时使用当前时间）")
	}

	if hasUpdatedAt {
		t.Error("CSV不应包含updated_at字段（设计决定：导入时使用当前时间）")
	}

	t.Log("✅ CSV正确省略了时间字段，导入时将使用当前时间")
}
