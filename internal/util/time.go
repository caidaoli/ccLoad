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
	TimeoutErrorCooldown = 5 * time.Minute

	// ServerErrorInitialCooldown 服务器错误（500/502/503/504）的初始冷却时间
	// 设计目标：指数退避策略，起始1秒（1s → 2s → 4s → ... → 2min上限）
	ServerErrorInitialCooldown = 1 * time.Second

	// OtherErrorInitialCooldown 其他错误（429等）的初始冷却时间
	OtherErrorInitialCooldown = 1 * time.Second

	// MaxCooldownDuration 最大冷却时长（指数退避上限）
	MaxCooldownDuration = 30 * time.Minute

	// MinCooldownDuration 最小冷却时长（指数退避下限）
	MinCooldownDuration = 1 * time.Second
)

// scanUnixTimestamp 统一的Unix时间戳扫描器
// 消除代码中8+处重复的时间戳转换逻辑
// 使用场景: GetCooldownUntil, GetKeyCooldownUntil, BumpCooldownOnError等
type scannable interface {
	Scan(dest ...any) error
}

// scanUnixTimestamp 从数据库扫描Unix时间戳并转换为time.Time
func scanUnixTimestamp(scanner scannable) (time.Time, bool) {
	var unixTime int64
	if err := scanner.Scan(&unixTime); err != nil {
		return time.Time{}, false
	}
	if unixTime == 0 {
		return time.Time{}, false
	}
	return time.Unix(unixTime, 0), true
}

// calculateBackoffDuration 计算指数退避冷却时间
// 统一冷却策略:
//   - 认证错误(401/402/403): 起始5分钟，后续翻倍，上限30分钟
//   - 服务器错误(500/502/503/504): 起始1秒，后续翻倍，上限30分钟
//   - 其他错误(429等): 起始1秒，后续翻倍，上限30分钟
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
	// 特殊处理：超时错误（状态码598）直接冷却5分钟，不使用指数退避
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
			// 服务器错误（500/502/503/504）：1秒冷却，指数退避（1s → 2s → 4s → ...）
			if statusCode != nil && (*statusCode == 500 || *statusCode == 502 || *statusCode == 503 || *statusCode == 504) {
				return ServerErrorInitialCooldown
			}
			// 认证错误（401/402/403）：5分钟冷却，减少无效重试
			if statusCode != nil && (*statusCode == 401 || *statusCode == 402 || *statusCode == 403) {
				return AuthErrorInitialCooldown
			}
			// 其他错误（429等）：1秒冷却，允许快速恢复
			return OtherErrorInitialCooldown
		}
	}

	// 后续错误：指数退避翻倍
	next := prev * 2

	// 边界限制（使用常量）
	if next < MinCooldownDuration {
		next = MinCooldownDuration
	}
	if next > MaxCooldownDuration {
		next = MaxCooldownDuration
	}

	return next
}

// toUnixTimestamp 安全转换time.Time到Unix时间戳
// 处理零值时间
func toUnixTimestamp(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// calculateCooldownDuration 计算冷却持续时间（毫秒）
// 用于存储到数据库的duration_ms字段
// CalculateCooldownDuration 计算冷却持续时间（毫秒）
func CalculateCooldownDuration(until time.Time, now time.Time) int64 {
	if until.IsZero() || !until.After(now) {
		return 0
	}
	return int64(until.Sub(now) / time.Millisecond)
}
