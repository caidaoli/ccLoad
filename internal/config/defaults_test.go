package config

import (
	"testing"
)

// TestConfigRelationships 测试配置项之间的关系
func TestConfigRelationships(t *testing.T) {
	// SQLite连接池配置: MaxOpenConns >= MaxIdleConns
	if SQLiteMaxOpenConnsFile < SQLiteMaxIdleConnsFile {
		t.Errorf("文件模式: MaxOpenConns(%d) < MaxIdleConns(%d)",
			SQLiteMaxOpenConnsFile, SQLiteMaxIdleConnsFile)
	}

	// HTTP连接池配置: MaxIdleConns >= MaxIdleConnsPerHost
	if HTTPMaxIdleConns < HTTPMaxIdleConnsPerHost {
		t.Errorf("HTTP: MaxIdleConns(%d) < MaxIdleConnsPerHost(%d)",
			HTTPMaxIdleConns, HTTPMaxIdleConnsPerHost)
	}

	// 日志配置: BufferSize >= BatchSize
	if DefaultLogBufferSize < LogBatchSize {
		t.Errorf("日志: BufferSize(%d) < BatchSize(%d)",
			DefaultLogBufferSize, LogBatchSize)
	}

	// 日志清理: CleanupInterval < 最小保留天数(1天)
	// log_retention_days 最小值为1天(24h), 清理间隔必须小于它
	cleanupHours := int(LogCleanupInterval.Hours())
	minRetentionHours := 24 // 最小保留1天
	if cleanupHours >= minRetentionHours {
		t.Errorf("日志清理: CleanupInterval(%dh) >= MinRetention(%dh)",
			cleanupHours, minRetentionHours)
	}
}
