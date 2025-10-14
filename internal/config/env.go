package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnvConfig 统一环境变量配置结构
// 遵循 SOLID 原则：单一职责 + 配置验证
type EnvConfig struct {
	// 服务配置
	Port              string
	GinMode           string
	Password          string
	AuthTokens        []string

	// 数据库配置
	SQLitePath        string
	RedisURL          string
	UseMemoryDB       bool
	JournalMode       string

	// 性能配置
	MaxConcurrency    int
	MaxKeyRetries     int
	FirstByteTimeout  int
	EnableTrace       bool
	EnableWarmup      bool
	SkipTLSVerify     bool

	// 日志配置
	LogBufferSize     int
	LogWorkers        int
}

// LoadFromEnv 从环境变量加载配置并验证
func LoadFromEnv() (*EnvConfig, error) {
	cfg := &EnvConfig{}

	// 服务配置
	cfg.Port = getEnvOrDefault("PORT", DefaultPort)
	cfg.GinMode = os.Getenv("GIN_MODE")
	cfg.Password = os.Getenv("CCLOAD_PASS")

	// ✅ 强制安全检查
	if cfg.Password == "" {
		return nil, fmt.Errorf("❌ CCLOAD_PASS 环境变量未设置（生产环境必须配置强密码）")
	}

	// 解析认证令牌
	if authEnv := os.Getenv("CCLOAD_AUTH"); authEnv != "" {
		tokens := strings.Split(authEnv, ",")
		for _, token := range tokens {
			if trimmed := strings.TrimSpace(token); trimmed != "" {
				cfg.AuthTokens = append(cfg.AuthTokens, trimmed)
			}
		}
	}

	// 数据库配置
	cfg.SQLitePath = getEnvOrDefault("SQLITE_PATH", "data/ccload.db")
	cfg.RedisURL = os.Getenv("REDIS_URL")
	cfg.UseMemoryDB = getBoolEnv("CCLOAD_USE_MEMORY_DB", false)
	cfg.JournalMode = getEnvOrDefault("SQLITE_JOURNAL_MODE", "WAL")

	// ✅ 内存模式强制检查 Redis
	if cfg.UseMemoryDB && cfg.RedisURL == "" {
		return nil, fmt.Errorf("❌ 内存模式（CCLOAD_USE_MEMORY_DB=true）必须配置 REDIS_URL")
	}

	// 性能配置
	cfg.MaxConcurrency = getIntEnv("CCLOAD_MAX_CONCURRENCY", DefaultMaxConcurrency)
	cfg.MaxKeyRetries = getIntEnv("CCLOAD_MAX_KEY_RETRIES", DefaultMaxKeyRetries)
	cfg.FirstByteTimeout = getIntEnv("CCLOAD_FIRST_BYTE_TIMEOUT", DefaultFirstByteTimeout)
	cfg.EnableTrace = getBoolEnv("CCLOAD_ENABLE_TRACE", false)
	cfg.EnableWarmup = getBoolEnv("CCLOAD_ENABLE_WARMUP", false)
	cfg.SkipTLSVerify = getBoolEnv("CCLOAD_SKIP_TLS_VERIFY", false)

	// 日志配置
	cfg.LogBufferSize = getIntEnv("CCLOAD_LOG_BUFFER", DefaultLogBufferSize)
	cfg.LogWorkers = getIntEnv("CCLOAD_LOG_WORKERS", DefaultLogWorkers)

	// ✅ 配置验证
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置验证失败: %w", err)
	}

	return cfg, nil
}

// Validate 验证配置合法性
func (c *EnvConfig) Validate() error {
	// 端口范围验证
	if c.Port != "" && strings.HasPrefix(c.Port, ":") {
		portNum, err := strconv.Atoi(c.Port[1:])
		if err != nil || portNum < 1 || portNum > 65535 {
			return fmt.Errorf("无效端口号: %s", c.Port)
		}
	}

	// 并发数验证
	if c.MaxConcurrency < 1 || c.MaxConcurrency > 10000 {
		return fmt.Errorf("MaxConcurrency 超出合理范围 [1, 10000]: %d", c.MaxConcurrency)
	}

	// 超时时间验证
	if c.FirstByteTimeout < 10 || c.FirstByteTimeout > 600 {
		return fmt.Errorf("FirstByteTimeout 超出合理范围 [10s, 600s]: %d", c.FirstByteTimeout)
	}

	return nil
}

// 辅助函数：获取环境变量或默认值
func getEnvOrDefault(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

// 辅助函数：获取整数环境变量
func getIntEnv(key string, defaultValue int) int {
	if val := os.Getenv(key); val != "" {
		if intVal, err := strconv.Atoi(val); err == nil && intVal > 0 {
			return intVal
		}
	}
	return defaultValue
}

// 辅助函数：获取布尔环境变量
func getBoolEnv(key string, defaultValue bool) bool {
	val := os.Getenv(key)
	if val == "1" || strings.EqualFold(val, "true") {
		return true
	}
	if val == "0" || strings.EqualFold(val, "false") {
		return false
	}
	return defaultValue
}
