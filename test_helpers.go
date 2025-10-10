package main

import (
	"context"
	"time"
)

// 测试辅助函数：封装新的批量查询方法为单个查询接口
// 这些函数仅用于测试，保持测试代码简洁可读

// getChannelCooldownUntil 获取单个渠道的冷却截止时间
func getChannelCooldownUntil(ctx context.Context, store Store, channelID int64) (time.Time, bool) {
	cooldowns, err := store.GetAllChannelCooldowns(ctx)
	if err != nil {
		return time.Time{}, false
	}
	until, exists := cooldowns[channelID]
	return until, exists
}

// getKeyCooldownUntil 获取单个Key的冷却截止时间
func getKeyCooldownUntil(ctx context.Context, store Store, channelID int64, keyIndex int) (time.Time, bool) {
	cooldowns, err := store.GetAllKeyCooldowns(ctx)
	if err != nil {
		return time.Time{}, false
	}
	channelCooldowns, exists := cooldowns[channelID]
	if !exists {
		return time.Time{}, false
	}
	until, exists := channelCooldowns[keyIndex]
	return until, exists
}
