package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// KeySelector 负责从渠道的多个API Key中选择可用的Key
// 遵循SRP原则：职责单一，仅负责Key选择逻辑
type KeySelector struct {
	store          Store
	keyCooldown    sync.Map // 内存缓存：key=fmt.Sprintf("%d_%d", channelID, keyIndex) -> time.Time
	keyRRCache     sync.Map // 内存缓存：key=channelID -> keyIndex
	keyCooldownTTL time.Duration
}

// NewKeySelector 创建Key选择器
func NewKeySelector(store Store) *KeySelector {
	return &KeySelector{
		store:          store,
		keyCooldownTTL: 5 * time.Second, // 冷却状态缓存5秒
	}
}

// SelectAvailableKey 为渠道选择一个可用的API Key
// 返回：(keyIndex, apiKey, error)
// 策略：
// - sequential: 顺序尝试，跳过冷却中的Key和已尝试的Key
// - round_robin: 轮询选择，跳过冷却中的Key和已尝试的Key
// excludeKeys: 本次请求中已尝试过的Key索引集合（避免同一请求内重复尝试）
func (ks *KeySelector) SelectAvailableKey(ctx context.Context, cfg *Config, excludeKeys map[int]bool) (int, string, error) {
	keys := cfg.GetAPIKeys()
	if len(keys) == 0 {
		return -1, "", fmt.Errorf("no API keys configured for channel %d", cfg.ID)
	}

	// 单Key场景：直接返回，不使用Key级别冷却（YAGNI原则）
	// 原因：单Key渠道应完全依赖渠道级别冷却（在selector.go中已实现）
	// 如果渠道被选中，说明渠道不在冷却中，直接返回唯一的Key
	if len(keys) == 1 {
		// 检查是否已尝试过（单Key场景下第二次调用应该返回错误）
		if excludeKeys != nil && excludeKeys[0] {
			return -1, "", fmt.Errorf("single key already tried in this request")
		}
		return 0, keys[0], nil
	}

	// 多Key场景：根据策略选择
	strategy := cfg.GetKeyStrategy()
	now := time.Now()

	switch strategy {
	case "round_robin":
		return ks.selectRoundRobin(ctx, cfg.ID, keys, now, excludeKeys)
	case "sequential":
		return ks.selectSequential(ctx, cfg.ID, keys, now, excludeKeys)
	default:
		// 默认使用顺序策略
		return ks.selectSequential(ctx, cfg.ID, keys, now, excludeKeys)
	}
}

// selectSequential 顺序选择：从第一个开始，跳过冷却中的Key和已尝试的Key
func (ks *KeySelector) selectSequential(_ context.Context, channelID int64, keys []string, _ time.Time, excludeKeys map[int]bool) (int, string, error) {
	for i, key := range keys {
		// 跳过本次请求已尝试过的Key
		if excludeKeys != nil && excludeKeys[i] {
			continue
		}
		// 跳过冷却中的Key
		if !ks.isKeyCooledDown(channelID, i) {
			return i, key, nil
		}
	}
	return -1, "", fmt.Errorf("all API keys are in cooldown or already tried")
}

// selectRoundRobin 轮询选择：使用轮询指针，跳过冷却中的Key和已尝试的Key
func (ks *KeySelector) selectRoundRobin(ctx context.Context, channelID int64, keys []string, _ time.Time, excludeKeys map[int]bool) (int, string, error) {
	keyCount := len(keys)

	// 从内存缓存获取轮询指针
	var startIdx int
	if val, ok := ks.keyRRCache.Load(channelID); ok {
		startIdx = val.(int) % keyCount
	} else {
		// 从数据库加载持久化的轮询指针
		startIdx = ks.store.NextKeyRR(ctx, channelID, keyCount)
		ks.keyRRCache.Store(channelID, startIdx)
	}

	// 从startIdx开始轮询，最多尝试keyCount次
	for i := 0; i < keyCount; i++ {
		idx := (startIdx + i) % keyCount
		// 跳过本次请求已尝试过的Key
		if excludeKeys != nil && excludeKeys[idx] {
			continue
		}
		// 跳过冷却中的Key
		if !ks.isKeyCooledDown(channelID, idx) {
			// 更新轮询指针到下一个位置
			nextIdx := (idx + 1) % keyCount
			ks.keyRRCache.Store(channelID, nextIdx)
			_ = ks.store.SetKeyRR(ctx, channelID, nextIdx)
			return idx, keys[idx], nil
		}
	}

	return -1, "", fmt.Errorf("all API keys are in cooldown or already tried")
}

// isKeyCooledDown 检查指定Key是否在冷却中（优先内存缓存，fallback数据库）
func (ks *KeySelector) isKeyCooledDown(channelID int64, keyIndex int) bool {
	cacheKey := fmt.Sprintf("%d_%d", channelID, keyIndex)
	now := time.Now()

	// 检查内存缓存
	if val, ok := ks.keyCooldown.Load(cacheKey); ok {
		expireTime := val.(time.Time)
		if expireTime.After(now) {
			return true // 仍在冷却中
		}
		// 缓存过期，删除
		ks.keyCooldown.Delete(cacheKey)
		return false
	}

	// 内存缓存未命中，查询数据库（修复：避免服务重启后丢失冷却状态）
	until, ok := ks.store.GetKeyCooldownUntil(context.Background(), channelID, keyIndex)
	if ok && until.After(now) {
		// 回填内存缓存，避免重复数据库查询
		ks.keyCooldown.Store(cacheKey, until)
		return true
	}

	return false
}

// MarkKeyError 标记Key错误，触发指数退避冷却
func (ks *KeySelector) MarkKeyError(ctx context.Context, channelID int64, keyIndex int) error {
	now := time.Now()
	cooldownDur, err := ks.store.BumpKeyCooldownOnError(ctx, channelID, keyIndex, now)
	if err != nil {
		return err
	}

	// 更新内存缓存
	cacheKey := fmt.Sprintf("%d_%d", channelID, keyIndex)
	cooldownUntil := now.Add(cooldownDur)
	ks.keyCooldown.Store(cacheKey, cooldownUntil)

	return nil
}

// MarkKeySuccess 标记Key成功，重置冷却状态
func (ks *KeySelector) MarkKeySuccess(ctx context.Context, channelID int64, keyIndex int) error {
	// 清除数据库冷却记录
	if err := ks.store.ResetKeyCooldown(ctx, channelID, keyIndex); err != nil {
		return err
	}

	// 清除内存缓存
	cacheKey := fmt.Sprintf("%d_%d", channelID, keyIndex)
	ks.keyCooldown.Delete(cacheKey)

	return nil
}

// GetKeyCooldownInfo 获取Key冷却信息（用于调试和监控）
func (ks *KeySelector) GetKeyCooldownInfo(ctx context.Context, channelID int64, keyIndex int) (until time.Time, cooled bool) {
	cacheKey := fmt.Sprintf("%d_%d", channelID, keyIndex)
	now := time.Now()

	// 优先从内存缓存获取
	if val, ok := ks.keyCooldown.Load(cacheKey); ok {
		expireTime := val.(time.Time)
		if expireTime.After(now) {
			return expireTime, true
		}
	}

	// 从数据库查询
	until, ok := ks.store.GetKeyCooldownUntil(ctx, channelID, keyIndex)
	if ok && until.After(now) {
		// 更新内存缓存
		ks.keyCooldown.Store(cacheKey, until)
		return until, true
	}

	return time.Time{}, false
}
