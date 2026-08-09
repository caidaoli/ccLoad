package util

import "testing"

// TestNormalizeProtocol 测试协议规范化
func TestNormalizeProtocol(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"正常小写", "anthropic", "anthropic"},
		{"大写转小写", "ANTHROPIC", "anthropic"},
		{"混合大小写", "AnThRoPiC", "anthropic"},
		{"带空格", " anthropic ", "anthropic"},
		{"空字符串保持为空", "", ""},
		{"仅空格规范为空", "   ", ""},
		{"codex类型", "CODEX", "codex"},
		{"gemini类型", "Gemini", "gemini"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeProtocol(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeProtocol(%q) = %q, 期望 %q", tt.input, result, tt.expected)
			}
		})
	}
}
