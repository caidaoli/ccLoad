package app

import (
	"ccLoad/internal/model"

	"ccLoad/internal/storage"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ✅ 轮询指针内存化：使用 sync.Map 存储每个渠道的轮询计数器
var rrCounters sync.Map // channelID(int64) -> *atomic.Uint32

// KeySelector 负责从渠道的多个API Key中选择可用的Key
// 重构：移除内存缓存，直接查询数据库
type KeySelector struct {
	store         storage.Store
	cooldownGauge *atomic.Int64 // 监控指标：当前活跃的Key级冷却数量（P2优化）
}

// NewKeySelector 创建Key选择器
func NewKeySelector(store storage.Store, gauge *atomic.Int64) *KeySelector {
	return &KeySelector{
		store:         store,
		cooldownGauge: gauge,
	}
}

// SelectAvailableKey 为渠道选择一个可用的API Key
// 返回：(keyIndex, apiKey, error)
// 策略：
// - sequential: 顺序尝试，跳过冷却中的Key和已尝试的Key
// - round_robin: 轮询选择，跳过冷却中的Key和已尝试的Key
// excludeKeys: 本次请求中已尝试过的Key索引集合（避免同一请求内重复尝试）
func (ks *KeySelector) SelectAvailableKey(ctx context.Context, cfg *model.Config, excludeKeys map[int]bool) (int, string, error) {
	// 从数据库查询渠道的所有API Keys
	apiKeys, err := ks.store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		return -1, "", fmt.Errorf("failed to get API keys for channel %d: %w", cfg.ID, err)
	}
	if len(apiKeys) == 0 {
		return -1, "", fmt.Errorf("no API keys configured for channel %d", cfg.ID)
	}

	// 单Key场景：直接返回，不使用Key级别冷却（YAGNI原则）
	if len(apiKeys) == 1 {
		if excludeKeys != nil && excludeKeys[0] {
			return -1, "", fmt.Errorf("single key already tried in this request")
		}
		return apiKeys[0].KeyIndex, apiKeys[0].APIKey, nil
	}

	// 多Key场景：根据策略选择（从第一个Key读取策略，所有Key共享策略）
	strategy := apiKeys[0].KeyStrategy
	if strategy == "" {
		strategy = "sequential" // 默认顺序策略
	}

	switch strategy {
	case "round_robin":
		return ks.selectRoundRobin(ctx, cfg, apiKeys, excludeKeys)
	case "sequential":
		return ks.selectSequential(apiKeys, excludeKeys)
	default:
		// 默认使用顺序策略
		return ks.selectSequential(apiKeys, excludeKeys)
	}
}

// selectSequential 顺序选择：从第一个开始，跳过冷却中的Key和已尝试的Key
func (ks *KeySelector) selectSequential(apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	now := time.Now()

	for _, apiKey := range apiKeys {
		keyIndex := apiKey.KeyIndex

		// 跳过本次请求已尝试过的Key
		if excludeKeys != nil && excludeKeys[keyIndex] {
			continue
		}

		// 检查Key内联的冷却状态（优化：优先使用内存数据）
		if apiKey.IsCoolingDown(now) {
			continue // Key冷却中，跳过
		}

		return keyIndex, apiKey.APIKey, nil
	}

	return -1, "", fmt.Errorf("all API keys are in cooldown or already tried")
}

// selectRoundRobin 轮询选择：使用内存atomic计数器，跳过冷却中的Key和已尝试的Key
// ✅ Linus风格优化：删除数据库持久化，重启后丢失也无所谓（轮询位置本就是临时状态）
func (ks *KeySelector) selectRoundRobin(ctx context.Context, cfg *model.Config, apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error) {
	keyCount := len(apiKeys)
	now := time.Now()

	// ✅ Linus风格：使用内存atomic计数器，删除数据库持久化
	counter, _ := rrCounters.LoadOrStore(cfg.ID, new(atomic.Uint32))
	startIdx := int(counter.(*atomic.Uint32).Add(1) % uint32(keyCount))

	// 从startIdx开始轮���，最多尝试keyCount次
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

// MarkKeyError 标记Key错误，触发指数退避冷却
func (ks *KeySelector) MarkKeyError(ctx context.Context, channelID int64, keyIndex int, statusCode int) error {
	now := time.Now()
	_, err := ks.store.BumpKeyCooldown(ctx, channelID, keyIndex, now, statusCode)
	if err != nil {
		return err
	}

	return nil
}

// MarkKeySuccess 标记Key成功，重置冷却状态
func (ks *KeySelector) MarkKeySuccess(ctx context.Context, channelID int64, keyIndex int) error {
	// 直接清除数据库冷却记录
	return ks.store.ResetKeyCooldown(ctx, channelID, keyIndex)
}

// GetKeyCooldownInfo 获取Key冷却信息（用于调试和监控）
func (ks *KeySelector) GetKeyCooldownInfo(ctx context.Context, channelID int64, keyIndex int) (until time.Time, cooled bool) {
	now := time.Now()

	// 查询API Key对象（包含内联冷却数据）
	apiKey, err := ks.store.GetAPIKey(ctx, channelID, keyIndex)
	if err != nil || apiKey == nil {
		return time.Time{}, false
	}

	// 检查冷却状态
	if apiKey.IsCoolingDown(now) {
		return time.Unix(apiKey.CooldownUntil, 0), true
	}

	return time.Time{}, false
}

// CleanupExpiredKeyCooldowns 已废弃：SQLite查询时自动过滤过期数据（WHERE until > NOW()）
// 该函数已被移除以消除goroutine泄漏风险
// 历史原因：重构后移除了内存缓存，此函数不再需要
// 修复日期：2025-10-05 (代码审查发现的P0问题)
