package config

import "time"

// HTTP服务器配置常量
const (
	// DefaultPort HTTP服务默认端口
	DefaultPort = "8080"

	// DefaultMaxConcurrency 默认最大并发请求数
	DefaultMaxConcurrency = 1000

	// DefaultMaxKeyRetries 单个渠道内最大Key重试次数
    DefaultMaxKeyRetries = 3

    // DefaultMaxBodyBytes 默认最大请求体字节数（用于代理入口的解析）
    DefaultMaxBodyBytes = 2 * 1024 * 1024 // 2MB
)

// HTTP客户端配置常量
const (
	// HTTPDialTimeout DNS解析+TCP连接建立超时（秒）
	HTTPDialTimeout = 30

	// HTTPKeepAliveInterval TCP keepalive间隔（秒）
	HTTPKeepAliveInterval = 30

	// HTTPTLSHandshakeTimeout TLS握手超时（秒）
	HTTPTLSHandshakeTimeout = 30

	// HTTPMaxIdleConns 全局空闲连接池大小
	HTTPMaxIdleConns = 100

	// HTTPMaxIdleConnsPerHost 单host空闲连接数
	HTTPMaxIdleConnsPerHost = 5

	// HTTPIdleConnTimeout 空闲连接超时（秒）
	HTTPIdleConnTimeout = 30

	// HTTPMaxConnsPerHost 单host最大连接数
	HTTPMaxConnsPerHost = 50

	// HTTPWriteBufferSize HTTP写缓冲区大小（字节）
	HTTPWriteBufferSize = 64 * 1024 // 64KB

	// HTTPReadBufferSize HTTP读缓冲区大小（字节）
	HTTPReadBufferSize = 64 * 1024 // 64KB

	// TLSSessionCacheSize TLS会话缓存大小
	TLSSessionCacheSize = 1024
)

// 日志系统配置常量
const (
	// DefaultLogBufferSize 默认日志缓冲区大小（条数）
	DefaultLogBufferSize = 1000

	// DefaultLogWorkers 默认日志Worker协程数
	DefaultLogWorkers = 3

	// LogBatchSize 批量写入日志的大小（条数）
	LogBatchSize = 100

    // LogBatchTimeout 批量写入超时时间（秒）
    LogBatchTimeout = 1

    // LogFlushTimeoutMs 单次日志刷盘的超时时间（毫秒）
    // 关停期间需要尽快完成，避免测试和生产关停卡顿
    LogFlushTimeoutMs = 300

	// LogDropAlertThreshold 日志丢弃告警阈值（条数）
	LogDropAlertThreshold = 1000

	// LogMaxMessageLength 单条日志最大长度（字符）
	LogMaxMessageLength = 2000

	// LogErrorTruncateLength 错误信息截断长度（字符）
	LogErrorTruncateLength = 512
)

// Token认证配置常量
const (
	// TokenRandomBytes Token随机字节数（生成64字符十六进制）
	TokenRandomBytes = 32

	// TokenExpiryHours Token有效期（小时）
	TokenExpiryHours = 24

	// TokenCleanupIntervalHours Token清理间隔（小时）
	TokenCleanupIntervalHours = 1
)

// SQLite连接池配置常量
// ✅ P1修复（2025-10-13）：优化连接池配置，提升高并发性能
const (
	// SQLiteMaxOpenConnsMemory 内存模式最大连接数
	SQLiteMaxOpenConnsMemory = 10

	// SQLiteMaxIdleConnsMemory 内存模式最大空闲连接数
	SQLiteMaxIdleConnsMemory = 5

	// SQLiteMaxOpenConnsFile 文件模式最大连接数（WAL写并发瓶颈）
	// 保持5：1写 + 4读 = 充分利用WAL模式并发能力
	SQLiteMaxOpenConnsFile = 5

	// SQLiteMaxIdleConnsFile 文件模式最大空闲连接数
	// ✅ 从2提升到5：避免高并发时频繁创建/销毁连接
	// 设计原则：空闲连接数 = 最大连接数，减少连接重建开销
	SQLiteMaxIdleConnsFile = 5

	// SQLiteConnMaxLifetimeMinutes 连接最大生命周期（分钟）
	// ✅ 从1分钟提升到5分钟：降低连接过期频率
	// 权衡：更长的生命周期 vs 更低的连接重建开销
	SQLiteConnMaxLifetimeMinutes = 5
)

// 性能优化配置常量
const (
	// CacheWarmupChannelCount 启动时预热的高优先级渠道数量
	CacheWarmupChannelCount = 5

	// LogCleanupIntervalHours 日志清理间隔（小时）
	LogCleanupIntervalHours = 1

	// LogRetentionDays 日志保留天数
	LogRetentionDays = 3
)

// Redis同步配置常量
const (
	// RedisSyncShutdownTimeoutMs 优雅关闭等待时间（毫秒）
	RedisSyncShutdownTimeoutMs = 100
)

// 工具函数：转换秒到time.Duration
func SecondsToDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

// 工具函数：转换分钟到time.Duration
func MinutesToDuration(minutes int) time.Duration {
	return time.Duration(minutes) * time.Minute
}

// 工具函数：转换小时到time.Duration
func HoursToDuration(hours int) time.Duration {
	return time.Duration(hours) * time.Hour
}
