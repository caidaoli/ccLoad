package util

import (
	"os"
	"strconv"
	"time"
)

// 冷却时间变量（支持环境变量覆盖，启动时读取一次）
var (
	// AuthErrorInitialCooldown 认证错误（401/402/403）的初始冷却时间
	AuthErrorInitialCooldown = 5 * time.Minute

	// TimeoutErrorCooldown 超时错误(597/598)的冷却时间
	TimeoutErrorCooldown = time.Minute

	// ServerErrorInitialCooldown 服务器错误（5xx）的初始冷却时间
	ServerErrorInitialCooldown = 2 * time.Minute

	// RateLimitErrorCooldown 限流错误（429）的初始冷却时间
	RateLimitErrorCooldown = time.Minute

	// MaxCooldownDuration 最大冷却时长（指数退避上限）
	MaxCooldownDuration = 30 * time.Minute

	// MinCooldownDuration 最小冷却时长（指数退避下限）
	MinCooldownDuration = 10 * time.Second
)

func init() {
	// 环境变量覆盖（启动时读取一次，重启生效）
	if v := envSeconds("CCLOAD_COOLDOWN_AUTH_SEC"); v > 0 {
		AuthErrorInitialCooldown = v
	}
	if v := envSeconds("CCLOAD_COOLDOWN_TIMEOUT_SEC"); v > 0 {
		TimeoutErrorCooldown = v
	}
	if v := envSeconds("CCLOAD_COOLDOWN_SERVER_SEC"); v > 0 {
		ServerErrorInitialCooldown = v
	}
	if v := envSeconds("CCLOAD_COOLDOWN_RATE_LIMIT_SEC"); v > 0 {
		RateLimitErrorCooldown = v
	}
	if v := envSeconds("CCLOAD_COOLDOWN_MAX_SEC"); v > 0 {
		MaxCooldownDuration = v
	}
	if v := envSeconds("CCLOAD_COOLDOWN_MIN_SEC"); v > 0 {
		MinCooldownDuration = v
	}
}

func envSeconds(key string) time.Duration {
	s := os.Getenv(key)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return time.Duration(v) * time.Second
}

// CalculateBackoffDuration 计算指数退避冷却时间
func CalculateBackoffDuration(prevMs int64, until time.Time, now time.Time, statusCode *int) time.Duration {
	prev := time.Duration(prevMs) * time.Millisecond

	// 如果没有历史记录，检查until字段
	if prev <= 0 {
		if !until.IsZero() && until.After(now) {
			prev = until.Sub(now)
		} else {
			// 首次错误：根据状态码确定初始冷却时间
			return getInitialCooldown(statusCode)
		}
	}

	// 后续错误：指数退避翻倍
	next := min(max(prev*2, MinCooldownDuration), MaxCooldownDuration)
	return next
}

// getInitialCooldown 根据状态码返回初始冷却时间
func getInitialCooldown(statusCode *int) time.Duration {
	if statusCode == nil {
		return RateLimitErrorCooldown
	}
	code := *statusCode
	switch {
	case code == 401 || code == 402 || code == 403:
		return AuthErrorInitialCooldown
	case code == StatusFirstByteTimeout || code == StatusSSEError:
		return TimeoutErrorCooldown
	case code >= 500:
		return ServerErrorInitialCooldown
	default:
		return RateLimitErrorCooldown
	}
}

// CalculateCooldownDuration 计算冷却持续时间（毫秒）
func CalculateCooldownDuration(until time.Time, now time.Time) int64 {
	if until.IsZero() || !until.After(now) {
		return 0
	}
	return int64(until.Sub(now) / time.Millisecond)
}
