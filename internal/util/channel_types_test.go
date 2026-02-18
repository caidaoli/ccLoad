package util

import "testing"

func TestDetectChannelTypeFromPath(t *testing.T) {
	testCases := []struct {
		name     string
		path     string
		expected string
	}{
		// Anthropic/Claude paths
		{"Claude Messages", "/v1/messages", ChannelTypeAnthropic},
		{"Claude Count Tokens", "/v1/messages/count_tokens", ChannelTypeAnthropic},

		// Codex paths
		{"Codex Responses", "/v1/responses", ChannelTypeCodex},

		// OpenAI paths
		{"OpenAI Chat", "/v1/chat/completions", ChannelTypeOpenAI},
		{"OpenAI Completions", "/v1/completions", ChannelTypeOpenAI},
		{"OpenAI Embeddings", "/v1/embeddings", ChannelTypeOpenAI},

		// OpenAI Images paths
		{"OpenAI Images Generations", "/v1/images/generations", ChannelTypeOpenAI},
		{"OpenAI Images Edits", "/v1/images/edits", ChannelTypeOpenAI},
		{"OpenAI Images Variations", "/v1/images/variations", ChannelTypeOpenAI},

		// Gemini paths
		{"Gemini Stream", "/v1beta/models/gemini-pro:streamGenerateContent", ChannelTypeGemini},
		{"Gemini Generate", "/v1beta/models/gemini-2.5-flash:generateContent", ChannelTypeGemini},

		// Unknown paths
		{"Unknown Path", "/unknown/path", ""},
		{"Empty Path", "", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := DetectChannelTypeFromPath(tc.path)
			if result != tc.expected {
				t.Errorf("DetectChannelTypeFromPath(%q) = %q, want %q", tc.path, result, tc.expected)
			}
		})
	}
}

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

func TestMatchPath(t *testing.T) {
	testCases := []struct {
		name      string
		path      string
		patterns  []string
		matchType string
		expected  bool
	}{
		// Prefix matching
		{"Prefix exact match", "/v1/messages", []string{"/v1/messages"}, MatchTypePrefix, true},
		{"Prefix with suffix", "/v1/messages/count", []string{"/v1/messages"}, MatchTypePrefix, true},
		{"Prefix no match", "/v2/messages", []string{"/v1/messages"}, MatchTypePrefix, false},
		{"Prefix multi-pattern match", "/v1/completions", []string{"/v1/chat", "/v1/completions"}, MatchTypePrefix, true},
		{"Prefix multi-pattern no match", "/v1/embeddings", []string{"/v1/chat", "/v1/completions"}, MatchTypePrefix, false},

		// Contains matching
		{"Contains match", "/v1beta/models/gemini", []string{"/v1beta/"}, MatchTypeContains, true},
		{"Contains no match", "/v1/models/gemini", []string{"/v1beta/"}, MatchTypeContains, false},
		{"Contains anywhere", "prefix/v1beta/suffix", []string{"/v1beta/"}, MatchTypeContains, true},

		// Edge cases
		{"Empty pattern", "/v1/messages", []string{}, MatchTypePrefix, false},
		{"Empty path", "", []string{"/v1/"}, MatchTypePrefix, false},
		{"Invalid match type", "/v1/messages", []string{"/v1/messages"}, "invalid", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := matchPath(tc.path, tc.patterns, tc.matchType)
			if result != tc.expected {
				t.Errorf("matchPath(%q, %v, %q) = %v, want %v",
					tc.path, tc.patterns, tc.matchType, result, tc.expected)
			}
		})
	}
}
