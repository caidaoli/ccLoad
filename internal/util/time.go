package util

import (
	"time"
)

// 冷却时间常量定义
const (
	// AuthErrorInitialCooldown 认证错误（401/403）的初始冷却时间
	// 设计目标：减少认证失败的无效重试，避免API配额浪费
	AuthErrorInitialCooldown = 5 * time.Minute

	// TimeoutErrorCooldown 超时错误的固定冷却时间
	// 设计目标：上游服务响应超时或完全无响应时，直接冷却避免资源浪费和级联故障
	// 适用场景：网络超时、上游服务无响应等（状态码598）
	TimeoutErrorCooldown = time.Minute

	// ServerErrorInitialCooldown 服务器错误（500/502/503/504）的初始冷却时间
	// 设计目标：指数退避策略，起始2分钟（2min → 4min → 8min → 16min → 30min上限）
	ServerErrorInitialCooldown = 2 * time.Minute

	// OtherErrorInitialCooldown 其他错误（429等）的初始冷却时间
	OtherErrorInitialCooldown = 10 * time.Second

	// MaxCooldownDuration 最大冷却时长（指数退避上限）
	MaxCooldownDuration = 30 * time.Minute

	// MinCooldownDuration 最小冷却时长（指数退避下限）
	MinCooldownDuration = 10 * time.Second
)

// calculateBackoffDuration 计算指数退避冷却时间
// 统一冷却策略:
//   - 认证错误(401/402/403): 起始5分钟，后续翻倍，上限30分钟
//   - 服务器错误(500/502/503/504): 起始2分钟，后续翻倍，上限30分钟
//   - 其他错误(429等): 起始10秒，后续翻倍，上限30分钟
//
// 参数:
//   - prevMs: 上次冷却持续时间（毫秒）
//   - until: 上次冷却截止时间
//   - now: 当前时间
//   - statusCode: HTTP状态码（可选，用于首次错误时确定初始冷却时间）
//
// 返回: 新的冷却持续时间
// CalculateBackoffDuration 计算指数退避冷却时间
func CalculateBackoffDuration(prevMs int64, until time.Time, now time.Time, statusCode *int) time.Duration {
	// 特殊处理：超时错误（状态码598）直接冷却1分钟，不使用指数退避
	// 设计原则：上游服务响应超时或完全无响应时，应立即停止请求避免资源浪费和级联故障
	if statusCode != nil && *statusCode == 598 {
		return TimeoutErrorCooldown
	}

	// 转换上次冷却持续时间
	prev := time.Duration(prevMs) * time.Millisecond

	// 如果没有历史记录，检查until字段
	if prev <= 0 {
		if !until.IsZero() && until.After(now) {
			prev = until.Sub(now)
		} else {
			// 首次错误：根据状态码确定初始冷却时间（直接返回，不翻倍）
			// 服务器错误（500/502/503/504/520/521/524/599）：2分钟冷却，指数退避（2min → 4min → 8min → ...）
			// 599：流式响应不完整，归类为服务器错误（上游服务问题）
			if statusCode != nil && (*statusCode == 500 || *statusCode == 502 || *statusCode == 503 || *statusCode == 504 || *statusCode == 520 || *statusCode == 521 || *statusCode == 524 || *statusCode == 599) {
				return ServerErrorInitialCooldown
			}
			// 认证错误（401/402/403）：5分钟冷却，减少无效重试
			if statusCode != nil && (*statusCode == 401 || *statusCode == 402 || *statusCode == 403) {
				return AuthErrorInitialCooldown
			}
			// 其他错误（429等）：10秒冷却，允许快速恢复
			return OtherErrorInitialCooldown
		}
	}

	// 后续错误：指数退避翻倍	// 边界限制（使用常量）

	next := min(max(prev*2, MinCooldownDuration), MaxCooldownDuration)

	return next
}

// CalculateCooldownDuration 计算冷却持续时间（毫秒）
func CalculateCooldownDuration(until time.Time, now time.Time) int64 {
	if until.IsZero() || !until.After(now) {
		return 0
	}
	return int64(until.Sub(now) / time.Millisecond)
}
