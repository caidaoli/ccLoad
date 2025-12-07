package sqlite

import (
	"strings"
	"testing"
)

// TestBuildMainDBDSN_FileMode 验证文件模式DSN包含WAL
func TestBuildMainDBDSN_FileMode(t *testing.T) {
	dsn := buildMainDBDSN("/tmp/test.db")

	// 验证DSN包含journal_mode=WAL
	if !strings.Contains(dsn, "journal_mode=WAL") {
		t.Error("File mode DSN should contain journal_mode=WAL for performance")
	}
}
