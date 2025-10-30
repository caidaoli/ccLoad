package util

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitizeLogMessage(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "正常消息",
			input:    "Hello, World!",
			expected: "Hello, World!",
		},
		{
			name:     "包含换行符",
			input:    "Line 1\nLine 2\rLine 3",
			expected: "Line 1\\nLine 2\\rLine 3",
		},
		{
			name:     "包含制表符",
			input:    "Column1\tColumn2\tColumn3",
			expected: "Column1\\tColumn2\\tColumn3",
		},
		{
			name:     "包含控制字符",
			input:    "Hello\x00\x01World",
			expected: "Hello\\x00\\x01World",
		},
		{
			name:     "超长消息",
			input:    strings.Repeat("A", 2500),
			expected: strings.Repeat("A", 2000) + "...[truncated]",
		},
		{
			name:     "空字符串",
			input:    "",
			expected: "",
		},
		{
			name:     "Unicode字符",
			input:    "你好，世界！",
			expected: "你好，世界！",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeLogMessage(tt.input)
			if result != tt.expected {
				t.Errorf("SanitizeLogMessage() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestSanitizeError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "正常错误",
			err:      errors.New("normal error"),
			expected: "normal error",
		},
		{
			name:     "包含换行的错误",
			err:      errors.New("error\nwith\nnewlines"),
			expected: "error\\nwith\\nnewlines",
		},
		{
			name:     "nil错误",
			err:      nil,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeError(tt.err)
			if result != tt.expected {
				t.Errorf("SanitizeError() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func BenchmarkSanitizeLogMessage(b *testing.B) {
	msg := "Error: connection failed\nRetrying...\r\nAttempt 1"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SanitizeLogMessage(msg)
	}
}
