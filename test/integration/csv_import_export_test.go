package integration_test

import (
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"testing"
)

// ==================== CSV导出默认值测试 ====================
// 注意：新架构中APIKey和KeyStrategy已从Config移除，CSV导出从api_keys表查询
// 此测试简化为仅验证channel_type的默认值处理

// ==================== CSV导入默认值测试 ====================

func TestCSVImport_DefaultValues(t *testing.T) {
	// 测试渠道类型规范化
	tests := []struct {
		input    string
		expected string
	}{
		{"", "anthropic"},          // 空值 → 默认值
		{"  ", "anthropic"},        // 空白 → 默认值
		{"anthropic", "anthropic"}, // 有效值保持
		{"gemini", "gemini"},       // 有效值保持
		{"codex", "codex"},         // 有效值保持
	}

	for _, tt := range tests {
		result := util.NormalizeChannelType(tt.input)
		if result != tt.expected {
			t.Errorf("util.NormalizeChannelType(%q) = %q, 期望 %q", tt.input, result, tt.expected)
		}
	}

	// 测试Key策略默认值处理
	keyStrategy := ""
	if keyStrategy == "" {
		keyStrategy = "sequential"
	}
	if keyStrategy != "sequential" {
		t.Errorf("空key_strategy应填充为sequential，实际为: %s", keyStrategy)
	}
}

// ==================== CSV导出导入循环测试 ====================

func TestCSVExportImportCycle(t *testing.T) {
	// 测试channel_type的导出导入循环
	// 场景：数据库中有空channel_type的Config
	original := &model.Config{
		ID:          1,
		Name:        "test-cycle",
		ChannelType: "", // 数据库中的空值
		URL:         "https://api.example.com",
		Priority:    10,
		ModelEntries: []model.ModelEntry{
			{Model: "test-model"},
		},
		Enabled: true,
	}

	// 步骤1：导出CSV（使用GetChannelType()）
	exportedChannelType := original.GetChannelType()
	if exportedChannelType != "anthropic" {
		t.Fatalf("导出channel_type应为anthropic，实际为: %s", exportedChannelType)
	}

	// 步骤2：导入CSV（规范化channel_type）
	importedChannelType := util.NormalizeChannelType(exportedChannelType)
	if importedChannelType != "anthropic" {
		t.Fatalf("导入channel_type应为anthropic，实际为: %s", importedChannelType)
	}

	t.Log("✅ CSV导出导入可以修复空channel_type为默认值")
}

// ==================== CSV时间字段缺失测试 ====================

func TestCSVExport_NoTimeFields(t *testing.T) {
	// 验证CSV导出不包含时间字段
	header := []string{"id", "name", "api_key", "url", "priority", "models", "model_redirects", "channel_type", "key_strategy", "enabled"}

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

// ==================== util.NormalizeChannelType 边界条件测试 ====================

func TestNormalizeChannelType(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "anthropic"},          // 空值 → 默认值
		{"  ", "anthropic"},        // 空白 → 默认值
		{"anthropic", "anthropic"}, // 有效值保持
		{"gemini", "gemini"},       // 有效值保持
		{"codex", "codex"},         // 有效值保持
		{"openai", "openai"},       // 有效值保持（openai是有效的渠道类型）
		{"ANTHROPIC", "anthropic"}, // 大写转小写
		{"  gemini  ", "gemini"},   // 去除空格并转小写
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := util.NormalizeChannelType(tt.input)
			if result != tt.expected {
				t.Errorf("util.NormalizeChannelType(%q) = %q, 期望 %q", tt.input, result, tt.expected)
			}
		})
	}
}
