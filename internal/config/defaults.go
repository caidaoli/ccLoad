package config

import "time"

// HTTP服务器配置常量
const (
	// DefaultMaxConcurrency 默认最大并发请求数
	DefaultMaxConcurrency = 1000

	// DefaultMaxKeyRetries 单个渠道内最大Key重试次数
	DefaultMaxKeyRetries = 3

	// DefaultMaxBodyBytes 默认最大请求体字节数（用于代理入口的解析）
	DefaultMaxBodyBytes = 2 * 1024 * 1024 // 2MB
)

// HTTP客户端配置常量
const (
	// HTTPDialTimeout DNS解析+TCP连接建立超时
	HTTPDialTimeout = 30 * time.Second

	// HTTPKeepAliveInterval TCP keepalive间隔
	// 15秒：快速检测僵死连接（上游进程崩溃、网络中断）
	// 配合Linux默认重试(9次×3s)，总检测时间42秒
	HTTPKeepAliveInterval = 15 * time.Second

	// HTTPTLSHandshakeTimeout TLS握手超时
	HTTPTLSHandshakeTimeout = 30 * time.Second

	// HTTPMaxIdleConns 全局空闲连接池大小
	HTTPMaxIdleConns = 100

	// HTTPMaxIdleConnsPerHost 单host空闲连接数
	HTTPMaxIdleConnsPerHost = 5

	// HTTPMaxConnsPerHost 单host最大连接数
	HTTPMaxConnsPerHost = 50

	// TLSSessionCacheSize TLS会话缓存大小
	TLSSessionCacheSize = 1024
)

// 日志系统配置常量
const (
	// DefaultLogBufferSize 默认日志缓冲区大小（条数）
	DefaultLogBufferSize = 1000

	// DefaultLogWorkers 默认日志Worker协程数
	// 改为1以保证日志写入顺序(FIFO)
	// 多worker会导致竞争消费logChan,打乱日志顺序
	// 性能影响: 单worker仍支持批量写入,性能足够(1000条/秒+)
	DefaultLogWorkers = 1

	// LogBatchSize 批量写入日志的大小（条数）
	LogBatchSize = 100

	// LogBatchTimeout 批量写入超时时间
	LogBatchTimeout = 1 * time.Second

	// LogFlushTimeoutMs 单次日志刷盘的超时时间（毫秒）
	// 关停期间需要尽快完成，避免测试和生产关停卡顿
	LogFlushTimeoutMs = 300
)

// Token认证配置常量
const (
	// TokenRandomBytes Token随机字节数（生成64字符十六进制）
	TokenRandomBytes = 32

	// TokenExpiry Token有效期
	TokenExpiry = 24 * time.Hour

	// TokenCleanupInterval Token清理间隔
	TokenCleanupInterval = 1 * time.Hour
)

// SQLite连接池配置常量
const (
	// SQLiteMaxOpenConnsFile 文件模式最大连接数（WAL写并发瓶颈）
	// 保持5：1写 + 4读 = 充分利用WAL模式并发能力
	SQLiteMaxOpenConnsFile = 5

	// SQLiteMaxIdleConnsFile 文件模式最大空闲连接数
	// [INFO] 从2提升到5：避免高并发时频繁创建/销毁连接
	// 设计原则：空闲连接数 = 最大连接数，减少连接重建开销
	SQLiteMaxIdleConnsFile = 5

	// SQLiteConnMaxLifetime 连接最大生命周期
	// [INFO] 从1分钟提升到5分钟：降低连接过期频率
	// 权衡：更长的生命周期 vs 更低的连接重建开销
	SQLiteConnMaxLifetime = 5 * time.Minute
)

// 性能优化配置常量
const (
	// LogCleanupInterval 日志清理间隔
	LogCleanupInterval = 1 * time.Hour
)

// Redis同步配置常量
const (
	// RedisSyncShutdownTimeoutMs 优雅关闭等待时间（毫秒）
	RedisSyncShutdownTimeoutMs = 100
)
