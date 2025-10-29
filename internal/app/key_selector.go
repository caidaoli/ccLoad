package app

import (
	"ccLoad/internal/model"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// KeySelector 负责从渠道的多个API Key中选择可用的Key
// 🔧 P0修复：移除store依赖，避免重复查询数据库
type KeySelector struct {
	cooldownGauge *atomic.Int64 // 监控指标：当前活跃的Key级冷却数量

	// 轮询计数器：channelID -> *rrCounter（带TTL）
	// ✅ P0修复(2025-10-29): 添加lastAccess跟踪，支持TTL清理，防止内存泄漏
	rrCounters map[int64]*rrCounter
	rrMutex    sync.RWMutex
}

// rrCounter 轮询计数器（带最后访问时间）
// ✅ P0修复(2025-10-29): 新增结构，支持TTL清理
type rrCounter struct {
	counter    atomic.Uint32
	lastAccess atomic.Int64 // Unix时间戳（秒）
}

// NewKeySelector 创建Key选择器
func NewKeySelector(gauge *atomic.Int64) *KeySelector {
	return &KeySelector{
		cooldownGauge: gauge,
		rrCounters:    make(map[int64]*rrCounter),
	}
}

// SelectAvailableKey 返回 (keyIndex, apiKey, error)
// 策略: sequential顺序尝试 | round_robin轮询选择
// excludeKeys: 避免同一请求内重复尝试
// ✅ P0重构: 移除store依赖，apiKeys由调用方传入，避免重复查询
func (ks *KeySelector) SelectAvailableKey(channelID int64, apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	if len(apiKeys) == 0 {
		return -1, "", fmt.Errorf("no API keys configured for channel %d", channelID)
	}

	// 单Key场景：直接返回，不使用Key级别冷却（YAGNI原则）
	if len(apiKeys) == 1 {
		if excludeKeys != nil && excludeKeys[0] {
			return -1, "", fmt.Errorf("single key already tried in this request")
		}
		return apiKeys[0].KeyIndex, apiKeys[0].APIKey, nil
	}

	// 多Key场景：根据策略选择
	strategy := apiKeys[0].KeyStrategy
	if strategy == "" {
		strategy = "sequential"
	}

	switch strategy {
	case "round_robin":
		return ks.selectRoundRobin(channelID, apiKeys, excludeKeys)
	case "sequential":
		return ks.selectSequential(apiKeys, excludeKeys)
	default:
		return ks.selectSequential(apiKeys, excludeKeys)
	}
}

func (ks *KeySelector) selectSequential(apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	now := time.Now()

	for _, apiKey := range apiKeys {
		keyIndex := apiKey.KeyIndex

		if excludeKeys != nil && excludeKeys[keyIndex] {
			continue
		}

		if apiKey.IsCoolingDown(now) {
			continue
		}

		return keyIndex, apiKey.APIKey, nil
	}

	return -1, "", fmt.Errorf("all API keys are in cooldown or already tried")
}

// selectRoundRobin 使用双重检查锁定确保并发安全
// ✅ P0修复(2025-10-29): 添加lastAccess更新，支持TTL清理
func (ks *KeySelector) selectRoundRobin(channelID int64, apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	keyCount := len(apiKeys)
	now := time.Now()

	// 🔧 双重检查锁定：确保每个channelID只创建一次counter
	ks.rrMutex.RLock()
	counter, ok := ks.rrCounters[channelID]
	ks.rrMutex.RUnlock()

	if !ok {
		ks.rrMutex.Lock()
		// 再次检查，避免多个goroutine同时创建
		if counter, ok = ks.rrCounters[channelID]; !ok {
			counter = &rrCounter{}
			counter.lastAccess.Store(now.Unix())
			ks.rrCounters[channelID] = counter
		}
		ks.rrMutex.Unlock()
	}

	// ✅ P0修复(2025-10-29): 更新最后访问时间
	counter.lastAccess.Store(now.Unix())
	startIdx := int(counter.counter.Add(1) % uint32(keyCount))

	// 从startIdx开始轮询，最多尝试keyCount次
	for i := 0; i < keyCount; i++ {
		idx := (startIdx + i) % keyCount

		// 在apiKeys中查找对应key_index的Key
		var selectedKey *model.APIKey
		for _, apiKey := range apiKeys {
			if apiKey.KeyIndex == idx {
				selectedKey = apiKey
				break
			}
		}

		if selectedKey == nil {
			continue // Key不存在，跳过（理论上不应该发生）
		}

		// 跳过本次请求已尝试过的Key
		if excludeKeys != nil && excludeKeys[idx] {
			continue
		}

		// 检查Key内联的冷却状态（优化：优先使用内存数据）
		if selectedKey.IsCoolingDown(now) {
			continue // Key冷却中，跳过
		}

		return idx, selectedKey.APIKey, nil
	}

	return -1, "", fmt.Errorf("all API keys are in cooldown or already tried")
}

// ✅ P0重构完成：KeySelector 专注于Key选择逻辑，冷却管理已移至 cooldownManager
// 移除的方法: MarkKeyError, MarkKeySuccess, GetKeyCooldownInfo
// 原因: 违反SRP原则，冷却管理应由专门的 cooldownManager 负责

// CleanupStaleCounters 清理长时间未使用的轮询计数器
// ✅ P0修复(2025-10-29): 新增清理方法，防止rrCounters内存泄漏
// TTL: 1小时未访问的计数器将被移除
func (ks *KeySelector) CleanupStaleCounters(ttlSeconds int64) int {
	if ttlSeconds <= 0 {
		ttlSeconds = 3600 // 默认1小时
	}

	now := time.Now().Unix()
	threshold := now - ttlSeconds

	ks.rrMutex.Lock()
	defer ks.rrMutex.Unlock()

	removed := 0
	for channelID, counter := range ks.rrCounters {
		if counter.lastAccess.Load() < threshold {
			delete(ks.rrCounters, channelID)
			removed++
		}
	}

	return removed
}
