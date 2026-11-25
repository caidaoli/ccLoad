package app

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"
)

// KeySelector 负责从渠道的多个API Key中选择可用的Key
// 移除store依赖，避免重复查询数据库
//
// 说明：使用 RWMutex + map 取代 sync.Map，原因是读多写少且保持类型安全。
type KeySelector struct {
	// 轮询计数器按渠道和候选 Key 集合隔离，避免不同模型组互相推进游标。
	// 渠道删除时需要清理对应计数器，避免rrCounters无界增长。
	rrCounters map[rrCounterScope]*rrCounter
	rrMutex    sync.RWMutex
}

type rrCounterScope struct {
	channelID int64
	keySetID  string
}

// rrCounter 轮询计数器（简化版）
type rrCounter struct {
	counter    atomic.Uint32
	lastAccess atomic.Int64 // UnixNano: 最后一次访问时间，用于后台清理

	weightMu       sync.Mutex  ///< 保护 currentWeights
	currentWeights map[int]int ///< 平滑加权轮询的 KeyIndex 当前权重，仅档内存在显式 Key 权重时使用
}

// SelectModelRow advances the cursor only after a usable row has been chosen.
// available has one position per configured row, including disabled/cooling rows.
func (ks *KeySelector) SelectModelRow(channelID int64, group string, available []bool) (int, bool) {
	if ks == nil || len(available) == 0 {
		return 0, false
	}
	counter := ks.getOrCreateCounter(rrCounterScope{channelID: channelID, keySetID: "model:" + group})
	counter.lastAccess.Store(time.Now().UnixNano())
	for {
		previous := counter.counter.Load()
		start := 0
		if previous > 0 && previous <= uint32(len(available)) { //nolint:gosec // row count is bounded by channel config size
			start = int(previous) % len(available)
		}
		for step := range available {
			index := (start + step) % len(available)
			if !available[index] {
				continue
			}
			if counter.counter.CompareAndSwap(previous, uint32(index+1)) { //nolint:gosec // index is within slice bounds
				return index, true
			}
			break
		}
		if !slices.Contains(available, true) {
			return 0, false
		}
	}
}

// NewKeySelector 创建Key选择器
func NewKeySelector() *KeySelector {
	return &KeySelector{
		rrCounters: make(map[rrCounterScope]*rrCounter),
	}
}

// SelectAvailableKey 返回 (keyIndex, apiKey, error)
// 优先选择最高可用优先级，同优先级轮询；key_strategy 仅保留为历史配置字段。
// excludeKeys: 避免同一请求内重复尝试
// 移除store依赖，apiKeys由调用方传入，避免重复查询
func (ks *KeySelector) SelectAvailableKey(channelID int64, apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	if len(apiKeys) == 0 {
		return -1, "", fmt.Errorf("no API keys configured for channel %d", channelID)
	}

	// 先确定最高可用档位；计数器仍包含该档的全部候选 Key，
	// 不因临时冷却或单次请求的排除集合改变轮询作用域。
	now := time.Now()
	var best *model.APIKey
	for _, key := range apiKeys {
		if key == nil || key.Disabled || excludeKeys[key.KeyIndex] || key.IsCoolingDown(now) {
			continue
		}
		if best == nil || key.Priority > best.Priority {
			best = key
		}
	}
	if best == nil {
		if len(apiKeys) == 1 && apiKeys[0] != nil {
			return -1, "", fmt.Errorf("single key (index=%d) is disabled, in cooldown or already tried", apiKeys[0].KeyIndex)
		}
		return -1, "", fmt.Errorf("all API keys are in cooldown or already tried")
	}
	if len(apiKeys) == 1 {
		return best.KeyIndex, best.APIKey, nil
	}
	peers := make([]*model.APIKey, 0, len(apiKeys))
	weighted := false
	for _, key := range apiKeys {
		if key != nil && key.Priority == best.Priority {
			peers = append(peers, key)
			weighted = weighted || key.Weight > 0
		}
	}
	if weighted {
		keyIndex, apiKey := ks.selectWeighted(channelID, peers, excludeKeys, now)
		return keyIndex, apiKey, nil
	}
	return ks.selectRoundRobin(channelID, peers, excludeKeys)
}

// SelectCooldownFallbackKey 在“全冷却兜底”路径中选择最早恢复的冷却Key。
// 只给兜底候选使用；普通请求仍必须走 SelectAvailableKey 的严格冷却过滤。
func (ks *KeySelector) SelectCooldownFallbackKey(channelID int64, apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	if len(apiKeys) == 0 {
		return -1, "", fmt.Errorf("no API keys configured for channel %d", channelID)
	}

	now := time.Now()
	var best *model.APIKey
	for _, apiKey := range apiKeys {
		if apiKey == nil {
			continue
		}
		if apiKey.Disabled {
			continue
		}
		keyIndex := apiKey.KeyIndex
		if excludeKeys != nil && excludeKeys[keyIndex] {
			continue
		}
		if !apiKey.IsCoolingDown(now) {
			return ks.SelectAvailableKey(channelID, apiKeys, excludeKeys)
		}
		if best == nil ||
			apiKey.CooldownUntil < best.CooldownUntil ||
			(apiKey.CooldownUntil == best.CooldownUntil &&
				(apiKey.Priority > best.Priority || (apiKey.Priority == best.Priority && keyIndex < best.KeyIndex))) {
			best = apiKey
		}
	}

	if best != nil {
		return best.KeyIndex, best.APIKey, nil
	}
	return -1, "", fmt.Errorf("all API keys are already tried")
}

func newRRCounterScope(channelID int64, apiKeys []*model.APIKey) rrCounterScope {
	keyIndices := make([]int, 0, len(apiKeys))
	for _, apiKey := range apiKeys {
		if apiKey != nil {
			keyIndices = append(keyIndices, apiKey.KeyIndex)
		}
	}
	sort.Ints(keyIndices)
	var keySet strings.Builder
	previous := 0
	hasPrevious := false
	for _, keyIndex := range keyIndices {
		if hasPrevious && keyIndex == previous {
			continue
		}
		keySet.WriteString(strconv.Itoa(keyIndex))
		keySet.WriteByte(',')
		previous = keyIndex
		hasPrevious = true
	}
	return rrCounterScope{channelID: channelID, keySetID: keySet.String()}
}

// getOrCreateCounter 获取或创建候选 Key 集合的轮询计数器（双重检查锁定）。
func (ks *KeySelector) getOrCreateCounter(scope rrCounterScope) *rrCounter {
	ks.rrMutex.RLock()
	counter, ok := ks.rrCounters[scope]
	ks.rrMutex.RUnlock()

	if ok {
		return counter
	}

	ks.rrMutex.Lock()
	defer ks.rrMutex.Unlock()

	// 再次检查，避免多个goroutine同时创建
	if counter, ok = ks.rrCounters[scope]; !ok {
		counter = &rrCounter{}
		counter.lastAccess.Store(time.Now().UnixNano())
		ks.rrCounters[scope] = counter
	}
	return counter
}

// RemoveChannelCounter 删除指定渠道的轮询计数器。
// 在渠道被删除时调用，避免rrCounters长期积累。
func (ks *KeySelector) RemoveChannelCounter(channelID int64) {
	ks.rrMutex.Lock()
	for scope := range ks.rrCounters {
		if scope.channelID == channelID {
			delete(ks.rrCounters, scope)
		}
	}
	ks.rrMutex.Unlock()
}

// CleanupInactiveCounters 清理长时间未使用的轮询计数器
// [FIX] P1: 自动清理过期计数器，防止内存泄漏（渠道删除后未手动调用RemoveChannelCounter）
// maxIdleTime: 最大空闲时间，超过此时间未使用的计数器将被清理
func (ks *KeySelector) CleanupInactiveCounters(maxIdleTime time.Duration) {
	if maxIdleTime <= 0 {
		return
	}

	cutoff := time.Now().Add(-maxIdleTime).UnixNano()

	ks.rrMutex.Lock()
	for scope, counter := range ks.rrCounters {
		if counter == nil {
			delete(ks.rrCounters, scope)
			continue
		}
		if counter.lastAccess.Load() < cutoff {
			delete(ks.rrCounters, scope)
		}
	}
	ks.rrMutex.Unlock()
}

// selectRoundRobin 轮询选择可用Key
// [FIX] 按 slice 索引轮询，返回真实 KeyIndex，不再假设 KeyIndex 连续
func (ks *KeySelector) selectRoundRobin(channelID int64, apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	keyCount := len(apiKeys)
	now := time.Now()

	counter := ks.getOrCreateCounter(newRRCounterScope(channelID, apiKeys))
	counter.lastAccess.Store(now.UnixNano())
	startIdx := int(counter.counter.Add(1) % uint32(keyCount)) //nolint:gosec // G115: keyCount 来自 API Keys 切片长度，不可能溢出

	// 从startIdx开始轮询，最多尝试keyCount次
	for i := range keyCount {
		sliceIdx := (startIdx + i) % keyCount
		selectedKey := apiKeys[sliceIdx]
		if selectedKey == nil {
			continue
		}

		if selectedKey.Disabled {
			continue
		}

		keyIndex := selectedKey.KeyIndex // 真实 KeyIndex，可能不连续

		// 检查排除集合（使用真实 KeyIndex）
		if excludeKeys != nil && excludeKeys[keyIndex] {
			continue
		}

		if selectedKey.IsCoolingDown(now) {
			continue
		}

		// 返回真实 KeyIndex，而非 slice 索引
		return keyIndex, selectedKey.APIKey, nil
	}

	return -1, "", fmt.Errorf("all API keys are in cooldown or already tried")
}

/**
 * @brief 在同优先级档内按 Key 权重做平滑加权轮询
 * @param channelID 渠道 ID
 * @param peers 同档全部候选 Key（非 nil），决定轮询状态作用域
 * @param excludeKeys 本次请求已尝试的 KeyIndex
 * @param now 判断冷却的参考时刻
 * @return 选中 Key 的 KeyIndex
 * @return 选中 Key 的密钥
 * @details 自动权重（Weight=0）按 1 计；只有可用 Key 累加当前权重，
 *          因此冷却、禁用或已尝试的 Key 不占份额。
 * @note 调用方保证 peers 至少含一个可用 Key；并发安全，同作用域串行。
 */
func (ks *KeySelector) selectWeighted(channelID int64, peers []*model.APIKey, excludeKeys map[int]bool, now time.Time) (int, string) {
	counter := ks.getOrCreateCounter(newRRCounterScope(channelID, peers))
	counter.lastAccess.Store(now.UnixNano())
	counter.weightMu.Lock()
	defer counter.weightMu.Unlock()
	if counter.currentWeights == nil {
		counter.currentWeights = make(map[int]int, len(peers))
	}
	total := 0
	var selected *model.APIKey
	for _, key := range peers {
		if key.Disabled || excludeKeys[key.KeyIndex] || key.IsCoolingDown(now) {
			continue
		}
		weight := max(key.Weight, 1)
		total += weight
		counter.currentWeights[key.KeyIndex] += weight
		if selected == nil || counter.currentWeights[key.KeyIndex] > counter.currentWeights[selected.KeyIndex] {
			selected = key
		}
	}
	counter.currentWeights[selected.KeyIndex] -= total
	return selected.KeyIndex, selected.APIKey
}

// KeySelector 专注于Key选择逻辑，冷却管理已移至 cooldownManager
// 移除的方法: MarkKeyError, MarkKeySuccess, GetKeyCooldownInfo
// 原因: 违反SRP原则，冷却管理应由专门的 cooldownManager 负责
