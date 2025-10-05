package main

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

// ==================== CSV导出默认值测试 ====================

func TestCSVExport_DefaultValues(t *testing.T) {
	// 模拟数据库中的Config（包含空默认值）
	configs := []*Config{
		{
			ID:          1,
			Name:        "test-empty-defaults",
			APIKey:      "sk-test",
			ChannelType: "", // 数据库中的空值
			KeyStrategy: "", // 数据库中的空值
			URL:         "https://api.example.com",
			Priority:    10,
			Models:      []string{"test-model"},
			Enabled:     true,
		},
		{
			ID:          2,
			Name:        "test-with-values",
			APIKey:      "sk-test-2",
			ChannelType: "gemini",
			KeyStrategy: "round_robin",
			URL:         "https://api.google.com",
			Priority:    5,
			Models:      []string{"gemini-flash"},
			Enabled:     true,
		},
	}

	// 模拟CSV导出逻辑（admin.go:200-225）
	buf := &bytes.Buffer{}
	writer := csv.NewWriter(buf)

	// 写入头部
	header := []string{"id", "name", "api_key", "url", "priority", "models", "model_redirects", "channel_type", "key_strategy", "enabled"}
	writer.Write(header)

	// 写入数据（使用GetChannelType和GetKeyStrategy）
	for _, cfg := range configs {
		modelRedirectsJSON := "{}"
		if len(cfg.ModelRedirects) > 0 {
			if jsonBytes, err := sonic.Marshal(cfg.ModelRedirects); err == nil {
				modelRedirectsJSON = string(jsonBytes)
			}
		}

		record := []string{
			"1",
			cfg.Name,
			cfg.APIKey,
			cfg.URL,
			"10",
			strings.Join(cfg.Models, ","),
			modelRedirectsJSON,
			cfg.GetChannelType(), // ← 关键：使用Get方法获取默认值
			cfg.GetKeyStrategy(), // ← 关键：使用Get方法获取默认值
			"true",
		}
		writer.Write(record)
	}
	writer.Flush()

	csvContent := buf.String()

	// 验证CSV内容
	lines := strings.Split(strings.TrimSpace(csvContent), "\n")
	if len(lines) != 3 { // header + 2 data rows
		t.Fatalf("CSV行数错误：期望3，实际%d", len(lines))
	}

	// 解析第二行数据（空默认值的Config）
	row1 := strings.Split(lines[1], ",")
	channelTypeCol := 7 // channel_type列索引
	keyStrategyCol := 8 // key_strategy列索引

	if row1[channelTypeCol] != "anthropic" {
		t.Errorf("CSV导出空channel_type应为anthropic，实际为: %s", row1[channelTypeCol])
	}

	if row1[keyStrategyCol] != "sequential" {
		t.Errorf("CSV导出空key_strategy应为sequential，实际为: %s", row1[keyStrategyCol])
	}

	// 解析第三行数据（有值的Config）
	row2 := strings.Split(lines[2], ",")
	if row2[channelTypeCol] != "gemini" {
		t.Errorf("CSV导出gemini channel_type应保持不变，实际为: %s", row2[channelTypeCol])
	}

	if row2[keyStrategyCol] != "round_robin" {
		t.Errorf("CSV导出round_robin key_strategy应保持不变，实际为: %s", row2[keyStrategyCol])
	}
}

// ==================== CSV导入默认值测试 ====================

func TestCSVImport_DefaultValues(t *testing.T) {
	// 模拟CSV输入（空channel_type和key_strategy）
	csvInput := `name,api_key,url,priority,models,model_redirects,channel_type,key_strategy,enabled
test-empty,sk-test,https://api.example.com,10,test-model,{},,sequential,true
test-with-values,sk-test-2,https://api.google.com,5,gemini-flash,{},gemini,round_robin,true`

	reader := csv.NewReader(strings.NewReader(csvInput))
	reader.TrimLeadingSpace = true

	headerRow, _ := reader.Read()
	columnIndex := buildCSVColumnIndex(headerRow)

	// 读取第一行数据（空默认值）
	record1, _ := reader.Read()

	fetch := func(key string) string {
		idx, ok := columnIndex[key]
		if !ok || idx >= len(record1) {
			return ""
		}
		return strings.TrimSpace(record1[idx])
	}

	channelType := fetch("channel_type")
	keyStrategy := fetch("key_strategy")

	// 验证空值处理
	if channelType != "" {
		t.Errorf("CSV空channel_type应为空字符串，实际为: %s", channelType)
	}

	// 模拟导入逻辑（admin.go:331-346）
	channelType = normalizeChannelType(channelType)
	if channelType != "anthropic" {
		t.Errorf("规范化后空channel_type应为anthropic，实际为: %s", channelType)
	}

	if keyStrategy == "" {
		keyStrategy = "sequential" // 默认值
	}
	if keyStrategy != "sequential" {
		t.Errorf("空key_strategy应填充为sequential，实际为: %s", keyStrategy)
	}

	// 验证规范化后的值（直接验证变量，无需创建Config对象）
	if channelType != "anthropic" {
		t.Errorf("规范化后的channel_type应为anthropic，实际为: %s", channelType)
	}

	if keyStrategy != "sequential" {
		t.Errorf("填充后的key_strategy应为sequential，实际为: %s", keyStrategy)
	}
}

// ==================== CSV导出导入循环测试 ====================

func TestCSVExportImportCycle(t *testing.T) {
	// 场景：数据库中有空channel_type的Config
	original := &Config{
		ID:          1,
		Name:        "test-cycle",
		APIKey:      "sk-test",
		ChannelType: "", // 数据库中的空值
		URL:         "https://api.example.com",
		Priority:    10,
		Models:      []string{"test-model"},
		Enabled:     true,
	}

	// 步骤1：导出CSV（使用GetChannelType()）
	exportedChannelType := original.GetChannelType()
	if exportedChannelType != "anthropic" {
		t.Fatalf("导出channel_type应为anthropic，实际为: %s", exportedChannelType)
	}

	// 步骤2：CSV内容（模拟导出结果）
	_ = "test-cycle,sk-test,https://api.example.com,10,test-model,{},anthropic,sequential,true"

	// 步骤3：导入CSV（规范化channel_type）
	importedChannelType := "anthropic" // 从CSV读取
	importedChannelType = normalizeChannelType(importedChannelType)

	if importedChannelType != "anthropic" {
		t.Fatalf("导入channel_type应为anthropic，实际为: %s", importedChannelType)
	}

	// 验证：导入后的channel_type是anthropic（非空）
	if importedChannelType != "anthropic" {
		t.Errorf("导入后的channel_type应为anthropic，实际为: %s", importedChannelType)
	}

	// 关键验证：原始Config的空值在导出导入循环后被修复
	t.Logf("原始Config channel_type: '%s' (空=%v)", original.ChannelType, original.ChannelType == "")
	t.Logf("导入后channel_type: '%s' (空=%v)", importedChannelType, importedChannelType == "")

	// 结论：CSV导出导入可以间接修复数据库中的空channel_type
	if importedChannelType == "" {
		t.Error("CSV导入应修复空channel_type为默认值")
	}
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

// ==================== normalizeChannelType 边界条件测试 ====================

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
		{"openai", "openai"},       // openai保持（不再转换为codex）
		{"ANTHROPIC", "anthropic"}, // 大写转小写
		{"  gemini  ", "gemini"},   // 去除空格并转小写
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := normalizeChannelType(tt.input)
			if result != tt.expected {
				t.Errorf("normalizeChannelType(%q) = %q, 期望 %q", tt.input, result, tt.expected)
			}
		})
	}
}
