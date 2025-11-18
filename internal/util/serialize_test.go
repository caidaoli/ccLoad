package util

import (
	"testing"
)

// TestSerializeJSON 测试JSON序列化
func TestSerializeJSON(t *testing.T) {
	tests := []struct {
		name         string
		input        any
		defaultValue string
		expected     string
		expectError  bool
	}{
		{
			name:         "空数组",
			input:        []string{},
			defaultValue: "[]",
			expected:     "[]",
			expectError:  false,
		},
		{
			name:         "单个元素",
			input:        []string{"test"},
			defaultValue: "[]",
			expected:     `["test"]`,
			expectError:  false,
		},
		{
			name:         "多个元素",
			input:        []string{"a", "b", "c"},
			defaultValue: "[]",
			expected:     `["a","b","c"]`,
			expectError:  false,
		},
		{
			name:         "nil值返回默认值",
			input:        nil,
			defaultValue: "default",
			expected:     "default",
			expectError:  false,
		},
		{
			name:         "map对象",
			input:        map[string]string{"key": "value"},
			defaultValue: "{}",
			expected:     `{"key":"value"}`,
			expectError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := SerializeJSON(tt.input, tt.defaultValue)
			if (err != nil) != tt.expectError {
				t.Errorf("期望错误=%v, 实际错误=%v", tt.expectError, err)
			}
			if result != tt.expected {
				t.Errorf("期望 %q, 实际 %q", tt.expected, result)
			}
		})
	}
}
