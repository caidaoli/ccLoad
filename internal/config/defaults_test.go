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
		{"LogDropAlertThreshold", LogDropAlertThreshold, 1, 10000},
		{"LogMaxMessageLength", LogMaxMessageLength, 100, 100000},
		{"LogErrorTruncateLength", LogErrorTruncateLength, 50, 10000},

		// Token配置
		{"TokenRandomBytes", TokenRandomBytes, 16, 64},
		{"TokenExpiryHours", TokenExpiryHours, 1, 8760},
		{"TokenCleanupIntervalHours", TokenCleanupIntervalHours, 1, 168},

		// SQLite配置
		{"SQLiteMaxOpenConnsMemory", SQLiteMaxOpenConnsMemory, 1, 100},
		{"SQLiteMaxIdleConnsMemory", SQLiteMaxIdleConnsMemory, 1, 100},
		{"SQLiteMaxOpenConnsFile", SQLiteMaxOpenConnsFile, 1, 100},
		{"SQLiteMaxIdleConnsFile", SQLiteMaxIdleConnsFile, 1, 100},
		{"SQLiteConnMaxLifetimeMinutes", SQLiteConnMaxLifetimeMinutes, 1, 1440},

		// 缓存配置
		{"CacheWarmupChannelCount", CacheWarmupChannelCount, 1, 1000},

		// 日志清理配置
		{"LogCleanupIntervalHours", LogCleanupIntervalHours, 1, 168},
		{"LogRetentionDays", LogRetentionDays, 1, 365},

		// HTTP超时配置(秒)
		{"HTTPDialTimeout", HTTPDialTimeout, 1, 120},
		{"HTTPKeepAliveInterval", HTTPKeepAliveInterval, 1, 300},
		{"HTTPTLSHandshakeTimeout", HTTPTLSHandshakeTimeout, 1, 120},
		{"HTTPIdleConnTimeout", HTTPIdleConnTimeout, 10, 600},

		// 日志超时配置
		{"LogBatchTimeout", LogBatchTimeout, 1, 60},          // 秒
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
		{"HTTPWriteBufferSize", HTTPWriteBufferSize, 1024, 1024 * 1024},
		{"HTTPReadBufferSize", HTTPReadBufferSize, 1024, 1024 * 1024},
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

// TestSecondsToDuration 测试秒转时间间隔
func TestSecondsToDuration(t *testing.T) {
	tests := []struct {
		name     string
		seconds  int
		expected time.Duration
	}{
		{"0秒", 0, 0},
		{"1秒", 1, 1 * time.Second},
		{"60秒", 60, 60 * time.Second},
		{"3600秒", 3600, 3600 * time.Second},
		{"负数", -1, -1 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SecondsToDuration(tt.seconds)
			if result != tt.expected {
				t.Errorf("期望 %v, 实际 %v", tt.expected, result)
			}
		})
	}
}

// TestMinutesToDuration 测试分钟转时间间隔
func TestMinutesToDuration(t *testing.T) {
	tests := []struct {
		name     string
		minutes  int
		expected time.Duration
	}{
		{"0分钟", 0, 0},
		{"1分钟", 1, 1 * time.Minute},
		{"60分钟", 60, 60 * time.Minute},
		{"1440分钟", 1440, 1440 * time.Minute},
		{"负数", -1, -1 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MinutesToDuration(tt.minutes)
			if result != tt.expected {
				t.Errorf("期望 %v, 实际 %v", tt.expected, result)
			}
		})
	}
}

// TestHoursToDuration 测试小时转时间间隔
func TestHoursToDuration(t *testing.T) {
	tests := []struct {
		name     string
		hours    int
		expected time.Duration
	}{
		{"0小时", 0, 0},
		{"1小时", 1, 1 * time.Hour},
		{"24小时", 24, 24 * time.Hour},
		{"168小时", 168, 168 * time.Hour},
		{"负数", -1, -1 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HoursToDuration(tt.hours)
			if result != tt.expected {
				t.Errorf("期望 %v, 实际 %v", tt.expected, result)
			}
		})
	}
}

// TestDurationConversionConsistency 测试时间转换的一致性
func TestDurationConversionConsistency(t *testing.T) {
	// 验证转换关系: 1小时 = 60分钟 = 3600秒
	oneHour := HoursToDuration(1)
	sixtyMinutes := MinutesToDuration(60)
	threeThousandSixHundredSeconds := SecondsToDuration(3600)

	if oneHour != sixtyMinutes {
		t.Errorf("1小时 != 60分钟: %v != %v", oneHour, sixtyMinutes)
	}
	if oneHour != threeThousandSixHundredSeconds {
		t.Errorf("1小时 != 3600秒: %v != %v", oneHour, threeThousandSixHundredSeconds)
	}
}

// TestConfigRelationships 测试配置项之间的关系
func TestConfigRelationships(t *testing.T) {
	// SQLite连接池配置: MaxOpenConns >= MaxIdleConns
	if SQLiteMaxOpenConnsMemory < SQLiteMaxIdleConnsMemory {
		t.Errorf("内存模式: MaxOpenConns(%d) < MaxIdleConns(%d)",
			SQLiteMaxOpenConnsMemory, SQLiteMaxIdleConnsMemory)
	}
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

	// 日志清理: CleanupInterval < Retention
	cleanupHours := LogCleanupIntervalHours
	retentionHours := LogRetentionDays * 24
	if cleanupHours >= retentionHours {
		t.Errorf("日志清理: CleanupInterval(%dh) >= Retention(%dh)",
			cleanupHours, retentionHours)
	}
}

// TestDefaultPort 测试默认端口
func TestDefaultPort(t *testing.T) {
	// DefaultPort是字符串类型
	if DefaultPort == "" {
		t.Error("DefaultPort不应为空")
	}
	// 验证是有效的端口号字符串
	if DefaultPort != "8080" {
		t.Logf("DefaultPort=%s (非标准8080)", DefaultPort)
	}
}

// TestLogFlushTimeout 测试日志刷新超时
func TestLogFlushTimeout(t *testing.T) {
	// LogFlushTimeoutMs应该大于LogBatchTimeout
	flushMs := LogFlushTimeoutMs
	batchMs := LogBatchTimeout

	if flushMs <= batchMs {
		t.Errorf("LogFlushTimeout(%dms) <= LogBatchTimeout(%dms)",
			flushMs, batchMs)
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
	timeouts := map[string]int{
		"HTTPDialTimeout":         HTTPDialTimeout,
		"HTTPKeepAliveInterval":   HTTPKeepAliveInterval,
		"HTTPTLSHandshakeTimeout": HTTPTLSHandshakeTimeout,
		"HTTPIdleConnTimeout":     HTTPIdleConnTimeout,
	}

	for name, value := range timeouts {
		if value <= 0 {
			t.Errorf("%s=%d 应该大于0", name, value)
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
