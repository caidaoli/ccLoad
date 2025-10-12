package sqlite

import (
	"os"
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

// TestBuildMainDBDSN_MemoryMode 验证内存模式DSN不包含WAL
func TestBuildMainDBDSN_MemoryMode(t *testing.T) {
	// 设置内存模式
	os.Setenv("CCLOAD_USE_MEMORY_DB", "true")
	defer os.Unsetenv("CCLOAD_USE_MEMORY_DB")

	dsn := buildMainDBDSN("/tmp/test.db")

	// 验证DSN不包含journal_mode=WAL
	if strings.Contains(dsn, "journal_mode=WAL") {
		t.Error("Memory mode DSN should not contain journal_mode=WAL")
	}

	// 验证DSN包含命名内存数据库标识
	if !strings.Contains(dsn, "file:ccload_mem_db") {
		t.Errorf("Memory mode DSN should contain 'file:ccload_mem_db', got: %s", dsn)
	}

	// 验证DSN包含cache=shared
	if !strings.Contains(dsn, "cache=shared") {
		t.Errorf("Memory mode DSN should contain 'cache=shared', got: %s", dsn)
	}

	// 验证文件模式仍然使用WAL
	os.Setenv("CCLOAD_USE_MEMORY_DB", "false")
	fileDSN := buildMainDBDSN("/tmp/test.db")
	if !strings.Contains(fileDSN, "journal_mode=WAL") {
		t.Error("File mode DSN should contain journal_mode=WAL for performance")
	}
}
