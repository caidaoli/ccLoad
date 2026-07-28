package util

import (
	"time"
)

// 内置冷却时间。这些值是不可变默认值；运行时覆盖保存在具体存储实例的 CooldownPolicy 中。
const (
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

// CooldownSettings 冷却时长配置（单位：秒）。非正值表示保留内置默认值。
type CooldownSettings struct {
	AuthSec      int
	TimeoutSec   int
	ServerSec    int
	RateLimitSec int
	MaxSec       int
	MinSec       int
}

// CooldownPolicy 是单个存储实例的不可变指数退避策略。
type CooldownPolicy struct {
	authInitial   time.Duration
	timeout       time.Duration
	serverInitial time.Duration
	rateLimit     time.Duration
	maxDuration   time.Duration
	minDuration   time.Duration
}

// NewCooldownPolicy 根据系统设置构造冷却策略；非正值使用内置默认值。
func NewCooldownPolicy(s CooldownSettings) *CooldownPolicy {
	policy := &CooldownPolicy{
		authInitial:   AuthErrorInitialCooldown,
		timeout:       TimeoutErrorCooldown,
		serverInitial: ServerErrorInitialCooldown,
		rateLimit:     RateLimitErrorCooldown,
		maxDuration:   MaxCooldownDuration,
		minDuration:   MinCooldownDuration,
	}
	assign := func(target *time.Duration, seconds int) {
		if seconds > 0 {
			*target = time.Duration(seconds) * time.Second
		}
	}
	assign(&policy.authInitial, s.AuthSec)
	assign(&policy.timeout, s.TimeoutSec)
	assign(&policy.serverInitial, s.ServerSec)
	assign(&policy.rateLimit, s.RateLimitSec)
	assign(&policy.maxDuration, s.MaxSec)
	assign(&policy.minDuration, s.MinSec)
	return policy
}

var defaultCooldownPolicy = NewCooldownPolicy(CooldownSettings{})

// CalculateBackoffDuration 计算指数退避冷却时间
func CalculateBackoffDuration(prevMs int64, until time.Time, now time.Time, statusCode *int) time.Duration {
	return defaultCooldownPolicy.CalculateBackoffDuration(prevMs, until, now, statusCode)
}

// CalculateBackoffDuration 使用当前策略计算指数退避冷却时间。
func (p *CooldownPolicy) CalculateBackoffDuration(prevMs int64, until time.Time, now time.Time, statusCode *int) time.Duration {
	prev := time.Duration(prevMs) * time.Millisecond

	// 如果没有历史记录，检查until字段
	if prev <= 0 {
		if !until.IsZero() && until.After(now) {
			prev = until.Sub(now)
		} else {
			// 首次错误：根据状态码确定初始冷却时间
			return p.initialCooldown(statusCode)
		}
	}

	// 后续错误：指数退避翻倍
	next := min(max(prev*2, p.minDuration), p.maxDuration)
	return next
}

// initialCooldown 根据状态码返回初始冷却时间。
func (p *CooldownPolicy) initialCooldown(statusCode *int) time.Duration {
	if statusCode == nil {
		return p.rateLimit
	}
	code := *statusCode
	switch {
	case code == 401 || code == 402 || code == 403:
		return p.authInitial
	case code == StatusFirstByteTimeout || code == StatusSSEError:
		return p.timeout
	case code >= 500:
		return p.serverInitial
	default:
		return p.rateLimit
	}
}

// CalculateCooldownDuration 计算冷却持续时间（毫秒）
func CalculateCooldownDuration(until time.Time, now time.Time) int64 {
	if until.IsZero() || !until.After(now) {
		return 0
	}
	return int64(until.Sub(now) / time.Millisecond)
}
