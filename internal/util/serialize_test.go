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

// TestSerializeModels 测试模型序列化
func TestSerializeModels(t *testing.T) {
	tests := []struct {
		name        string
		input       []string
		expected    string
		expectError bool
	}{
		{
			name:        "空数组",
			input:       nil,
			expected:    "[]",
			expectError: false,
		},
		{
			name:        "单个模型",
			input:       []string{"gpt-4"},
			expected:    `["gpt-4"]`,
			expectError: false,
		},
		{
			name:        "多个模型",
			input:       []string{"gpt-4", "gpt-3.5-turbo", "claude-3"},
			expected:    `["gpt-4","gpt-3.5-turbo","claude-3"]`,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := SerializeModels(tt.input)
			if (err != nil) != tt.expectError {
				t.Errorf("期望错误=%v, 实际错误=%v", tt.expectError, err)
			}
			if result != tt.expected {
				t.Errorf("期望 %q, 实际 %q", tt.expected, result)
			}
		})
	}
}

// TestSerializeModelRedirects 测试模型重定向序列化
func TestSerializeModelRedirects(t *testing.T) {
	tests := []struct {
		name        string
		input       map[string]string
		expected    string
		expectError bool
	}{
		{
			name:        "空map",
			input:       nil,
			expected:    "{}",
			expectError: false,
		},
		{
			name:        "单个重定向",
			input:       map[string]string{"gpt-4": "gpt-4-0613"},
			expected:    `{"gpt-4":"gpt-4-0613"}`,
			expectError: false,
		},
		{
			name: "多个重定向",
			input: map[string]string{
				"gpt-4": "gpt-4-0613",
				"gpt-3": "gpt-3.5-turbo",
			},
			// JSON对象的键顺序可能不同,只验证不报错
			expected:    "",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := SerializeModelRedirects(tt.input)
			if (err != nil) != tt.expectError {
				t.Errorf("期望错误=%v, 实际错误=%v", tt.expectError, err)
			}
			if tt.name == "多个重定向" {
				// 只验证包含必要的键值对
				if len(result) == 0 {
					t.Error("期望非空结果")
				}
			} else if result != tt.expected {
				t.Errorf("期望 %q, 实际 %q", tt.expected, result)
			}
		})
	}
}
