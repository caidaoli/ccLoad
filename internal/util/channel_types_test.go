package util

import "testing"

func TestChannelTypeConstants(t *testing.T) {
	// 验证常量值正确
	tests := []struct {
		constant string
		expected string
	}{
		{ChannelTypeAnthropic, "anthropic"},
		{ChannelTypeCodex, "codex"},
		{ChannelTypeOpenAI, "openai"},
		{ChannelTypeGemini, "gemini"},
	}

	for _, tt := range tests {
		if tt.constant != tt.expected {
			t.Errorf("Constant mismatch: got %q, want %q", tt.constant, tt.expected)
		}
	}
}

func TestMatchTypeConstants(t *testing.T) {
	// 验证匹配类型常量值正确
	tests := []struct {
		constant string
		expected string
	}{
		{MatchTypePrefix, "prefix"},
		{MatchTypeContains, "contains"},
	}

	for _, tt := range tests {
		if tt.constant != tt.expected {
			t.Errorf("MatchType constant mismatch: got %q, want %q", tt.constant, tt.expected)
		}
	}
}

func TestChannelTypesConfiguration(t *testing.T) {
	// 验证 ChannelTypes 配置使用了正确的常量
	if len(ChannelTypes) != 4 {
		t.Errorf("Expected 4 channel types, got %d", len(ChannelTypes))
	}

	// 验证每个配置的 Value 和 MatchType 使用了常量
	expectedValues := map[string]bool{
		ChannelTypeAnthropic: true,
		ChannelTypeCodex:     true,
		ChannelTypeOpenAI:    true,
		ChannelTypeGemini:    true,
	}

	for _, ct := range ChannelTypes {
		if !expectedValues[ct.Value] {
			t.Errorf("Unexpected channel type value: %q", ct.Value)
		}

		// 验证 MatchType 是已知的常量
		if ct.MatchType != MatchTypePrefix && ct.MatchType != MatchTypeContains {
			t.Errorf("Channel %q has invalid MatchType: %q", ct.Value, ct.MatchType)
		}

		// 验证 PathPatterns 不为空
		if len(ct.PathPatterns) == 0 {
			t.Errorf("Channel %q has no PathPatterns", ct.Value)
		}
	}
}

// TestIsValidChannelType 测试渠道类型验证
func TestIsValidChannelType(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"anthropic类型", "anthropic", true},
		{"codex类型", "codex", true},
		{"openai类型", "openai", true},
		{"gemini类型", "gemini", true},
		{"无效类型", "invalid", false},
		{"空字符串", "", false},
		{"大写类型", "ANTHROPIC", false}, // 严格匹配
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidChannelType(tt.input)
			if result != tt.expected {
				t.Errorf("IsValidChannelType(%q) = %v, 期望 %v", tt.input, result, tt.expected)
			}
		})
	}
}

// TestNormalizeChannelType 测试渠道类型规范化
func TestNormalizeChannelType(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"正常小写", "anthropic", "anthropic"},
		{"大写转小写", "ANTHROPIC", "anthropic"},
		{"混合大小写", "AnThRoPiC", "anthropic"},
		{"带空格", " anthropic ", "anthropic"},
		{"空字符串返回默认值", "", "anthropic"},
		{"仅空格返回默认值", "   ", "anthropic"},
		{"codex类型", "CODEX", "codex"},
		{"gemini类型", "Gemini", "gemini"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeChannelType(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeChannelType(%q) = %q, 期望 %q", tt.input, result, tt.expected)
			}
		})
	}
}
