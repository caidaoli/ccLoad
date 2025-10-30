package util

import (
	"testing"
)

// TestParseAPIKeys 测试API Key解析
func TestParseAPIKeys(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "单个Key",
			input:    "sk-test-key",
			expected: []string{"sk-test-key"},
		},
		{
			name:     "多个Key (逗号分隔)",
			input:    "sk-key1,sk-key2,sk-key3",
			expected: []string{"sk-key1", "sk-key2", "sk-key3"},
		},
		{
			name:     "带空格的Key",
			input:    " sk-key1 , sk-key2 , sk-key3 ",
			expected: []string{"sk-key1", "sk-key2", "sk-key3"},
		},
		{
			name:     "空字符串",
			input:    "",
			expected: []string{},
		},
		{
			name:     "仅空格",
			input:    "   ",
			expected: []string{},
		},
		{
			name:     "包含空项",
			input:    "sk-key1,,sk-key3",
			expected: []string{"sk-key1", "sk-key3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseAPIKeys(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("期望 %d 个key, 实际 %d 个", len(tt.expected), len(result))
				return
			}
			for i, key := range result {
				if key != tt.expected[i] {
					t.Errorf("索引 %d: 期望 %q, 实际 %q", i, tt.expected[i], key)
				}
			}
		})
	}
}
