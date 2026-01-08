// Package storage 提供数据持久化和缓存层的实现。
// 包括 SQLite/MySQL 存储、内存缓存和 Redis 同步功能。
package storage

import (
	"context"
	"log"
	"maps"
	"sync"
	"time"

	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/util"
)

// ChannelCache 高性能渠道缓存层
// 内存查询比数据库查询快 1000 倍+
type ChannelCache struct {
	store           Store
	channelsByModel map[string][]*modelpkg.Config // model → channels
	channelsByType  map[string][]*modelpkg.Config // type → channels
	allChannels     []*modelpkg.Config            // 所有渠道
	lastUpdate      time.Time
	mutex           sync.RWMutex
	ttl             time.Duration

	// 扩展缓存支持更多关键查询
	apiKeysByChannelID map[int64][]*modelpkg.APIKey // channelID → API keys
	cooldownCache      struct {
		channels   map[int64]time.Time         // channelID → cooldown until
		keys       map[int64]map[int]time.Time // channelID→keyIndex→cooldown until
		lastUpdate time.Time
		ttl        time.Duration
	}
}

// NewChannelCache 创建渠道缓存实例
func NewChannelCache(store Store, ttl time.Duration) *ChannelCache {
	return &ChannelCache{
		store:           store,
		channelsByModel: make(map[string][]*modelpkg.Config),
		channelsByType:  make(map[string][]*modelpkg.Config),
		allChannels:     make([]*modelpkg.Config, 0),
		ttl:             ttl,

		// 初始化扩展缓存
		apiKeysByChannelID: make(map[int64][]*modelpkg.APIKey),
		cooldownCache: struct {
			channels   map[int64]time.Time
			keys       map[int64]map[int]time.Time
			lastUpdate time.Time
			ttl        time.Duration
		}{
			channels: make(map[int64]time.Time),
			keys:     make(map[int64]map[int]time.Time),
			ttl:      30 * time.Second, // 冷却状态缓存30秒
		},
	}
}

// deepCopyConfig 深拷贝 Config 对象（包括 slice）
// 防止调用方修改污染缓存
// 设计：拷贝所有可变字段（ModelEntries），重置索引缓存（modelIndex + indexOnce）
// [FIX] P0: 重置索引缓存，避免复制 sync.Once 和指向旧 slice 的 map
func deepCopyConfig(src *modelpkg.Config) *modelpkg.Config {
	if src == nil {
		return nil
	}

	dst := &modelpkg.Config{
		ID:                 src.ID,
		Name:               src.Name,
		ChannelType:        src.ChannelType,
		URL:                src.URL,
		Priority:           src.Priority,
		Enabled:            src.Enabled,
		CooldownUntil:      src.CooldownUntil,
		CooldownDurationMs: src.CooldownDurationMs,
		CreatedAt:          src.CreatedAt,
		UpdatedAt:          src.UpdatedAt,
		KeyCount:           src.KeyCount,
	}

	// 深拷贝 ModelEntries slice
	if src.ModelEntries != nil {
		dst.ModelEntries = make([]modelpkg.ModelEntry, len(src.ModelEntries))
		copy(dst.ModelEntries, src.ModelEntries)
	}

	return dst
}

// deepCopyConfigs 批量深拷贝 Config 对象
// 缓存边界隔离，避免共享指针污染
func deepCopyConfigs(src []*modelpkg.Config) []*modelpkg.Config {
	if src == nil {
		return nil
	}

	result := make([]*modelpkg.Config, len(src))
	for i, cfg := range src {
		result[i] = deepCopyConfig(cfg)
	}
	return result
}

// GetEnabledChannelsByModel 缓存优先的模型查询
// [FIX] P0-2: 返回深拷贝，防止调用方污染缓存
func (c *ChannelCache) GetEnabledChannelsByModel(ctx context.Context, model string) ([]*modelpkg.Config, error) {
	if err := c.refreshIfNeeded(ctx); err != nil {
		// 缓存失败时降级到数据库查询
		return c.store.GetEnabledChannelsByModel(ctx, model)
	}

	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if model == "*" {
		// 返回所有渠道的深拷贝（隔离可变字段：ModelEntries）
		return deepCopyConfigs(c.allChannels), nil
	}

	// 返回指定模型的渠道深拷贝
	channels, exists := c.channelsByModel[model]
	if !exists {
		return []*modelpkg.Config{}, nil
	}

	return deepCopyConfigs(channels), nil
}

// GetEnabledChannelsByType 缓存优先的类型查询
// [FIX] P0-2: 返回深拷贝，防止调用方污染缓存
func (c *ChannelCache) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	normalizedType := util.NormalizeChannelType(channelType)
	if err := c.refreshIfNeeded(ctx); err != nil {
		// 缓存失败时降级到数据库查询
		return c.store.GetEnabledChannelsByType(ctx, normalizedType)
	}

	c.mutex.RLock()
	defer c.mutex.RUnlock()

	channels, exists := c.channelsByType[normalizedType]
	if !exists {
		return []*modelpkg.Config{}, nil
	}

	// 返回深拷贝（隔离可变字段：ModelEntries）
	return deepCopyConfigs(channels), nil
}

// GetConfig 获取指定ID的渠道配置
// 直接查询数据库，保证数据永远是最新的
func (c *ChannelCache) GetConfig(ctx context.Context, channelID int64) (*modelpkg.Config, error) {
	return c.store.GetConfig(ctx, channelID)
}

// refreshIfNeeded 智能缓存刷新
func (c *ChannelCache) refreshIfNeeded(ctx context.Context) error {
	c.mutex.RLock()
	needsRefresh := time.Since(c.lastUpdate) > c.ttl
	c.mutex.RUnlock()

	if !needsRefresh {
		return nil
	}

	// 使用写锁保护刷新操作
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// 双重检查，防止并发刷新
	if time.Since(c.lastUpdate) <= c.ttl {
		return nil
	}

	return c.refreshCache(ctx)
}

// refreshCache 刷新缓存数据
// 说明：缓存内部索引共享指针；对外统一返回深拷贝，避免调用方污染缓存。
func (c *ChannelCache) refreshCache(ctx context.Context) error {
	start := time.Now()

	allChannels, err := c.store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		return err
	}

	// 构建按类型分组的索引（内部共享指针，对外深拷贝隔离）
	byModel := make(map[string][]*modelpkg.Config)
	byType := make(map[string][]*modelpkg.Config)

	for _, channel := range allChannels {
		channelType := channel.GetChannelType()
		byType[channelType] = append(byType[channelType], channel) // 内部共享

		// 同时填充模型索引（使用 GetModels() 辅助方法）
		for _, model := range channel.GetModels() {
			byModel[model] = append(byModel[model], channel) // 内部共享
		}
	}

	// 原子性更新缓存（整体替换，不修改单个对象）
	c.allChannels = allChannels
	c.channelsByModel = byModel
	c.channelsByType = byType
	c.lastUpdate = time.Now()

	refreshDuration := time.Since(start)
	if refreshDuration > 5*time.Second {
		log.Printf("[WARN]  缓存刷新过慢: %v, 渠道数: %d, 模型数: %d, 类型数: %d",
			refreshDuration, len(allChannels), len(byModel), len(byType))
	}

	return nil
}

// InvalidateCache 手动失效缓存
func (c *ChannelCache) InvalidateCache() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.lastUpdate = time.Time{} // 重置为0时间，强制刷新
}

// GetAPIKeys 缓存优先的API Keys查询
func (c *ChannelCache) GetAPIKeys(ctx context.Context, channelID int64) ([]*modelpkg.APIKey, error) {
	// 检查缓存
	c.mutex.RLock()
	if keys, exists := c.apiKeysByChannelID[channelID]; exists {
		c.mutex.RUnlock()
		// 深拷贝: 防止调用方修改污染缓存
		result := make([]*modelpkg.APIKey, len(keys))
		for i, key := range keys {
			keyCopy := *key // 拷贝对象本身
			result[i] = &keyCopy
		}
		return result, nil
	}
	c.mutex.RUnlock()

	// 缓存未命中，从数据库加载
	keys, err := c.store.GetAPIKeys(ctx, channelID)
	if err != nil {
		return nil, err
	}

	// 存储到缓存（只存 slice 本身；对外总是返回深拷贝，避免污染缓存）
	c.mutex.Lock()
	c.apiKeysByChannelID[channelID] = keys
	c.mutex.Unlock()

	result := make([]*modelpkg.APIKey, len(keys))
	for i, key := range keys {
		keyCopy := *key // 拷贝对象本身
		result[i] = &keyCopy
	}
	return result, nil
}

// GetAllChannelCooldowns 缓存优先的渠道冷却查询
func (c *ChannelCache) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	// 检查冷却缓存是否有效
	c.mutex.RLock()
	if time.Since(c.cooldownCache.lastUpdate) <= c.cooldownCache.ttl {
		// 有效缓存，返回副本
		result := make(map[int64]time.Time, len(c.cooldownCache.channels))
		maps.Copy(result, c.cooldownCache.channels)
		c.mutex.RUnlock()
		return result, nil
	}
	c.mutex.RUnlock()

	// 缓存过期，从数据库加载
	cooldowns, err := c.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		return nil, err
	}

	// 存到缓存；对外总是返回副本，避免调用方修改污染缓存。
	c.mutex.Lock()
	c.cooldownCache.channels = cooldowns
	c.cooldownCache.lastUpdate = time.Now()
	c.mutex.Unlock()

	result := make(map[int64]time.Time, len(cooldowns))
	maps.Copy(result, cooldowns)
	return result, nil
}

// GetAllKeyCooldowns 缓存优先的Key冷却查询
func (c *ChannelCache) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	// 检查冷却缓存是否有效
	c.mutex.RLock()
	if time.Since(c.cooldownCache.lastUpdate) <= c.cooldownCache.ttl {
		// 有效缓存，返回副本
		result := make(map[int64]map[int]time.Time)
		for k, v := range c.cooldownCache.keys {
			keyMap := make(map[int]time.Time)
			maps.Copy(keyMap, v)
			result[k] = keyMap
		}
		c.mutex.RUnlock()
		return result, nil
	}
	c.mutex.RUnlock()

	// 缓存过期，从数据库加载
	cooldowns, err := c.store.GetAllKeyCooldowns(ctx)
	if err != nil {
		return nil, err
	}

	// 存到缓存；对外总是返回深拷贝，避免调用方修改污染缓存。
	c.mutex.Lock()
	c.cooldownCache.keys = cooldowns
	c.cooldownCache.lastUpdate = time.Now()
	c.mutex.Unlock()

	result := make(map[int64]map[int]time.Time, len(cooldowns))
	for k, v := range cooldowns {
		keyMap := make(map[int]time.Time, len(v))
		maps.Copy(keyMap, v)
		result[k] = keyMap
	}
	return result, nil
}

// InvalidateAPIKeysCache 手动失效API Keys缓存
func (c *ChannelCache) InvalidateAPIKeysCache(channelID int64) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.apiKeysByChannelID, channelID)
}

// InvalidateAllAPIKeysCache 清空所有API Key缓存（批量操作后使用）
func (c *ChannelCache) InvalidateAllAPIKeysCache() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.apiKeysByChannelID = make(map[int64][]*modelpkg.APIKey)
}

// InvalidateCooldownCache 手动失效冷却缓存
func (c *ChannelCache) InvalidateCooldownCache() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.cooldownCache.lastUpdate = time.Time{}
}
