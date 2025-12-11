package app

import (
	"ccLoad/internal/model"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// KeySelector 负责从渠道的多个API Key中选择可用的Key
// 移除store依赖，避免重复查询数据库
type KeySelector struct {
	cooldownGauge *atomic.Int64 // 监控指标：当前活跃的Key级冷却数量

	// 轮询计数器：channelID -> *rrCounter
	// 注意：渠道删除后计数器不会自动清理，但泄漏量有限（≈渠道数量，每个24字节）
	// 设计选择：YAGNI原则，除非有上万个渠道频繁增删，否则可忽略
	rrCounters map[int64]*rrCounter
	rrMutex    sync.RWMutex
}

// rrCounter 轮询计数器（简化版）
type rrCounter struct {
	counter atomic.Uint32
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
// 移除store依赖，apiKeys由调用方传入，避免重复查询
func (ks *KeySelector) SelectAvailableKey(channelID int64, apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	if len(apiKeys) == 0 {
		return -1, "", fmt.Errorf("no API keys configured for channel %d", channelID)
	}

	// 单Key场景:检查排除和冷却状态
	if len(apiKeys) == 1 {
		if excludeKeys != nil && excludeKeys[0] {
			return -1, "", fmt.Errorf("single key already tried in this request")
		}
		// ✅ 修复(2025-12-09): 检查冷却状态,防止单Key渠道冷却后仍被请求
		// 原逻辑"不使用Key级别冷却(YAGNI原则)"是错误的,会导致冷却Key持续触发上游错误
		if apiKeys[0].IsCoolingDown(time.Now()) {
			return -1, "", fmt.Errorf("single key (index=%d) is in cooldown until %s",
				apiKeys[0].KeyIndex,
				time.Unix(apiKeys[0].CooldownUntil, 0).Format("2006-01-02 15:04:05"))
		}
		return apiKeys[0].KeyIndex, apiKeys[0].APIKey, nil
	}

	// 多Key场景:根据策略选择
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

// getOrCreateCounter 获取或创建渠道的轮询计数器（双重检查锁定）
func (ks *KeySelector) getOrCreateCounter(channelID int64) *rrCounter {
	ks.rrMutex.RLock()
	counter, ok := ks.rrCounters[channelID]
	ks.rrMutex.RUnlock()

	if ok {
		return counter
	}

	ks.rrMutex.Lock()
	defer ks.rrMutex.Unlock()

	// 再次检查，避免多个goroutine同时创建
	if counter, ok = ks.rrCounters[channelID]; !ok {
		counter = &rrCounter{}
		ks.rrCounters[channelID] = counter
	}
	return counter
}

// findKeyByIndex 在apiKeys中查找指定索引的Key
func findKeyByIndex(apiKeys []*model.APIKey, idx int) *model.APIKey {
	for _, apiKey := range apiKeys {
		if apiKey.KeyIndex == idx {
			return apiKey
		}
	}
	return nil
}

// selectRoundRobin 轮询选择可用Key
func (ks *KeySelector) selectRoundRobin(channelID int64, apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	keyCount := len(apiKeys)
	now := time.Now()

	counter := ks.getOrCreateCounter(channelID)
	startIdx := int(counter.counter.Add(1) % uint32(keyCount))

	// 从startIdx开始轮询，最多尝试keyCount次
	for i := range keyCount {
		idx := (startIdx + i) % keyCount

		selectedKey := findKeyByIndex(apiKeys, idx)
		if selectedKey == nil {
			continue
		}

		if excludeKeys != nil && excludeKeys[idx] {
			continue
		}

		if selectedKey.IsCoolingDown(now) {
			continue
		}

		return idx, selectedKey.APIKey, nil
	}

	return -1, "", fmt.Errorf("all API keys are in cooldown or already tried")
}

// KeySelector 专注于Key选择逻辑，冷却管理已移至 cooldownManager
// 移除的方法: MarkKeyError, MarkKeySuccess, GetKeyCooldownInfo
// 原因: 违反SRP原则，冷却管理应由专门的 cooldownManager 负责
