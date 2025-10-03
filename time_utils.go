package main

import (
	"time"
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
// 统一冷却策略: 起始1秒，错误翻倍，上限30分钟
//
// 参数:
//   - prevMs: 上次冷却持续时间（毫秒）
//   - until: 上次冷却截止时间
//   - now: 当前时间
//
// 返回: 新的冷却持续时间
func calculateBackoffDuration(prevMs int64, until time.Time, now time.Time) time.Duration {
	// 转换上次冷却持续时间
	prev := time.Duration(prevMs) * time.Millisecond

	// 如果没有历史记录，检查until字段
	if prev <= 0 {
		if !until.IsZero() && until.After(now) {
			prev = until.Sub(now)
		} else {
			// 首次错误，从1秒开始
			prev = time.Second
		}
	}

	// 指数退避：错误一次翻倍
	next := prev * 2

	// 边界限制
	if next < time.Second {
		next = time.Second
	}
	if next > 30*time.Minute {
		next = 30 * time.Minute
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
func calculateCooldownDuration(until time.Time, now time.Time) int64 {
	if until.IsZero() || !until.After(now) {
		return 0
	}
	return int64(until.Sub(now) / time.Millisecond)
}
