package sqlite

import (
	"strings"
	"testing"
)

// TestGenerateLogDBPath 测试日志数据库路径生成
func TestGenerateLogDBPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "标准路径添加-log后缀",
			input:    "/data/ccload.db",
			expected: "/data/ccload-log.db",
		},
		{
			name:     "已有-log后缀不重复添加",
			input:    "/data/ccload-log.db",
			expected: "/data/ccload-log.db",
		},
		{
			name:     "内存数据库不添加后缀",
			input:    ":memory:",
			expected: ":memory:",
		},
		{
			name:     "相对路径处理",
			input:    "./data/db.db",
			expected: "./data/db-log.db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := generateLogDBPath(tt.input)
			if result != tt.expected {
				t.Errorf("generateLogDBPath(%q) = %q, expected %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestBuildMainDBDSN_FileMode 验证文件模式DSN包含WAL
func TestBuildMainDBDSN_FileMode(t *testing.T) {
	dsn := buildMainDBDSN("/tmp/test.db")

	// 验证DSN包含journal_mode=WAL
	if !strings.Contains(dsn, "journal_mode=WAL") {
		t.Error("File mode DSN should contain journal_mode=WAL for performance")
	}
}
