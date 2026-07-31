package util

import "testing"

func TestProtocolConstants(t *testing.T) {
	// 验证常量值正确
	tests := []struct {
		constant string
		expected string
	}{
		{ProtocolAnthropic, "anthropic"},
		{ProtocolCodex, "codex"},
		{ProtocolOpenAI, "openai"},
		{ProtocolGemini, "gemini"},
	}

	for _, tt := range tests {
		if tt.constant != tt.expected {
			t.Errorf("Constant mismatch: got %q, want %q", tt.constant, tt.expected)
		}
	}
}

func TestProtocolsConfiguration(t *testing.T) {
	if len(Protocols) != 4 {
		t.Errorf("Expected 4 protocols, got %d", len(Protocols))
	}

	expectedValues := map[string]bool{
		ProtocolAnthropic: true,
		ProtocolCodex:     true,
		ProtocolOpenAI:    true,
		ProtocolGemini:    true,
	}

	for _, protocol := range Protocols {
		if !expectedValues[protocol.Value] {
			t.Errorf("Unexpected protocol value: %q", protocol.Value)
		}
		if protocol.DisplayName == "" {
			t.Errorf("Protocol %q has empty DisplayName", protocol.Value)
		}
		if protocol.Description == "" {
			t.Errorf("Protocol %q has empty Description", protocol.Value)
		}
		delete(expectedValues, protocol.Value)
	}

	for value := range expectedValues {
		t.Errorf("Missing protocol value: %q", value)
	}
}

// TestIsValidProtocol 测试协议验证
func TestIsValidProtocol(t *testing.T) {
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
			result := IsValidProtocol(tt.input)
			if result != tt.expected {
				t.Errorf("IsValidProtocol(%q) = %v, 期望 %v", tt.input, result, tt.expected)
			}
		})
	}
}

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
