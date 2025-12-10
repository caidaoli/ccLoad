package config

import (
	"testing"
	"time"
)

// TestDefaultConstants 测试默认常量值的合理性
func TestDefaultConstants(t *testing.T) {
	tests := []struct {
		name  string
		value int
		min   int
		max   int
	}{
		// HTTP配置
		{"DefaultMaxConcurrency", DefaultMaxConcurrency, 1, 10000},
		{"DefaultMaxKeyRetries", DefaultMaxKeyRetries, 1, 10},
		{"HTTPMaxIdleConns", HTTPMaxIdleConns, 1, 1000},
		{"HTTPMaxIdleConnsPerHost", HTTPMaxIdleConnsPerHost, 1, 1000},
		{"HTTPMaxConnsPerHost", HTTPMaxConnsPerHost, 0, 1000},

		// 日志配置
		{"DefaultLogBufferSize", DefaultLogBufferSize, 100, 100000},
		{"DefaultLogWorkers", DefaultLogWorkers, 1, 10},
		{"LogBatchSize", LogBatchSize, 1, 1000},

		// Token配置
		{"TokenRandomBytes", TokenRandomBytes, 16, 64},

		// SQLite配置
		{"SQLiteMaxOpenConnsFile", SQLiteMaxOpenConnsFile, 1, 100},
		{"SQLiteMaxIdleConnsFile", SQLiteMaxIdleConnsFile, 1, 100},

		// 日志超时配置
		{"LogFlushTimeoutMs", LogFlushTimeoutMs, 100, 60000}, // 毫秒
		{"RedisSyncShutdownTimeoutMs", RedisSyncShutdownTimeoutMs, 100, 10000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value < tt.min || tt.value > tt.max {
				t.Errorf("%s=%d 超出合理范围 [%d, %d]", tt.name, tt.value, tt.min, tt.max)
			}
		})
	}
}

// TestBufferSizeConstants 测试缓冲区大小常量
func TestBufferSizeConstants(t *testing.T) {
	tests := []struct {
		name  string
		value int
		min   int
		max   int
	}{
		{"TLSSessionCacheSize", TLSSessionCacheSize, 0, 10000},
		{"DefaultMaxBodyBytes", DefaultMaxBodyBytes, 1024, 100 * 1024 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value < tt.min || tt.value > tt.max {
				t.Errorf("%s=%d 超出合理范围 [%d, %d]", tt.name, tt.value, tt.min, tt.max)
			}
		})
	}
}

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

// TestRedisSyncShutdownTimeout 测试Redis同步关闭超时
func TestRedisSyncShutdownTimeout(t *testing.T) {
	// 关闭超时应该在合理范围内 (100ms - 10s)
	if RedisSyncShutdownTimeoutMs < 100 {
		t.Errorf("RedisSyncShutdownTimeout=%dms 太短", RedisSyncShutdownTimeoutMs)
	}
	if RedisSyncShutdownTimeoutMs > 10000 {
		t.Errorf("RedisSyncShutdownTimeout=%dms 太长", RedisSyncShutdownTimeoutMs)
	}
}

// TestHTTPTimeoutValues 测试HTTP超时值的合理性
func TestHTTPTimeoutValues(t *testing.T) {
	// 所有HTTP超时应该大于0
	timeouts := map[string]time.Duration{
		"HTTPDialTimeout":         HTTPDialTimeout,
		"HTTPKeepAliveInterval":   HTTPKeepAliveInterval,
		"HTTPTLSHandshakeTimeout": HTTPTLSHandshakeTimeout,
	}

	for name, value := range timeouts {
		if value <= 0 {
			t.Errorf("%s=%v 应该大于0", name, value)
		}
	}
}

// TestLogConfigValues 测试日志配置值的合理性
func TestLogConfigValues(t *testing.T) {
	// 日志Worker数量应该合理
	if DefaultLogWorkers < 1 {
		t.Error("DefaultLogWorkers应该至少为1")
	}
	if DefaultLogWorkers > 10 {
		t.Logf("DefaultLogWorkers=%d 可能过多", DefaultLogWorkers)
	}

	// 日志批次大小应该小于缓冲区大小
	if LogBatchSize > DefaultLogBufferSize {
		t.Errorf("LogBatchSize(%d) > DefaultLogBufferSize(%d)",
			LogBatchSize, DefaultLogBufferSize)
	}
}
