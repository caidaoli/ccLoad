// Package storage 提供数据持久化和缓存层的实现。
// 包括 SQLite/MySQL 存储和内存缓存功能。
package storage

import (
	"context"
	"log"
	"maps"
	"strings"
	"sync"
	"time"

	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/util"
)

// ChannelCache 高性能渠道缓存层
// 内存查询比数据库查询快 1000 倍+
type ChannelCache struct {
	store                      Store
	channelsByModel            map[string][]*modelpkg.Config            // model → channels
	channelsByModelAndProtocol map[string]map[string][]*modelpkg.Config // model → protocol → channels
	channelsByType             map[string][]*modelpkg.Config            // type → channels
	channelsByExposedProtocol  map[string][]*modelpkg.Config            // protocol → channels
	allChannels                []*modelpkg.Config                       // 所有渠道
	lastUpdate                 time.Time
	mutex                      sync.RWMutex
	refreshMutex               sync.Mutex // 串行化刷新动作，避免数据库 IO 在 mutex 锁内阻塞读者
	ttl                        time.Duration

	// 扩展缓存支持更多关键查询
	apiKeysByChannelID map[int64][]*modelpkg.APIKey // channelID → API keys
	cooldownCache      struct {
		channels          map[int64]time.Time         // channelID → cooldown until
		keys              map[int64]map[int]time.Time // channelID→keyIndex→cooldown until
		channelLastUpdate time.Time
		keyLastUpdate     time.Time
		ttl               time.Duration
	}
}

// NewChannelCache 创建渠道缓存实例
func NewChannelCache(store Store, ttl time.Duration) *ChannelCache {
	return &ChannelCache{
		store:                      store,
		channelsByModel:            make(map[string][]*modelpkg.Config),
		channelsByModelAndProtocol: make(map[string]map[string][]*modelpkg.Config),
		channelsByType:             make(map[string][]*modelpkg.Config),
		channelsByExposedProtocol:  make(map[string][]*modelpkg.Config),
		allChannels:                make([]*modelpkg.Config, 0),
		ttl:                        ttl,

		// 初始化扩展缓存
		apiKeysByChannelID: make(map[int64][]*modelpkg.APIKey),
		cooldownCache: struct {
			channels          map[int64]time.Time
			keys              map[int64]map[int]time.Time
			channelLastUpdate time.Time
			keyLastUpdate     time.Time
			ttl               time.Duration
		}{
			channels: make(map[int64]time.Time),
			keys:     make(map[int64]map[int]time.Time),
			ttl:      30 * time.Second, // 冷却状态缓存30秒
		},
	}
}

// deepCopyConfigs 批量深拷贝 Config 对象
// 缓存边界隔离，避免共享指针污染
func deepCopyConfigs(src []*modelpkg.Config) []*modelpkg.Config {
	if src == nil {
		return nil
	}

	result := make([]*modelpkg.Config, len(src))
	for i, cfg := range src {
		result[i] = cfg.Clone()
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

// GetEnabledChannelsByExposedProtocol 缓存优先的暴露协议查询
func (c *ChannelCache) GetEnabledChannelsByExposedProtocol(ctx context.Context, protocol string) ([]*modelpkg.Config, error) {
	protocol = normalizeProtocol(protocol)
	if protocol == "" {
		return []*modelpkg.Config{}, nil
	}
	if err := c.refreshIfNeeded(ctx); err != nil {
		return c.store.GetEnabledChannelsByExposedProtocol(ctx, protocol)
	}

	c.mutex.RLock()
	defer c.mutex.RUnlock()

	channels, exists := c.channelsByExposedProtocol[protocol]
	if !exists {
		return []*modelpkg.Config{}, nil
	}

	return deepCopyConfigs(channels), nil
}

// GetEnabledChannelsByModelAndProtocol 缓存优先的“模型 + 暴露协议”联合查询。
func (c *ChannelCache) GetEnabledChannelsByModelAndProtocol(ctx context.Context, modelName string, protocol string) ([]*modelpkg.Config, error) {
	protocol = normalizeProtocol(protocol)
	if protocol == "" {
		return c.GetEnabledChannelsByModel(ctx, modelName)
	}
	if err := c.refreshIfNeeded(ctx); err != nil {
		channels, err := c.store.GetEnabledChannelsByModelAndProtocol(ctx, modelName, protocol)
		if err != nil {
			return nil, err
		}
		return deepCopyConfigs(channels), nil
	}

	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if modelName == "*" {
		channels, exists := c.channelsByExposedProtocol[protocol]
		if !exists {
			return []*modelpkg.Config{}, nil
		}
		return deepCopyConfigs(channels), nil
	}

	byProtocol, exists := c.channelsByModelAndProtocol[modelName]
	if !exists {
		return []*modelpkg.Config{}, nil
	}

	channels, exists := byProtocol[protocol]
	if !exists {
		return []*modelpkg.Config{}, nil
	}
	return deepCopyConfigs(channels), nil
}

func normalizeProtocol(protocol string) string {
	return strings.ToLower(strings.TrimSpace(protocol))
}

// GetConfig 获取指定ID的渠道配置
// 直接查询数据库，保证数据永远是最新的
func (c *ChannelCache) GetConfig(ctx context.Context, channelID int64) (*modelpkg.Config, error) {
	return c.store.GetConfig(ctx, channelID)
}

// refreshIfNeeded 智能缓存刷新
// 锁策略：refreshMutex 串行化刷新动作，c.mutex 仅在指针互换瞬间持有写锁，
// DB IO 与索引构建均发生在锁外，读者可继续访问旧数据。
func (c *ChannelCache) refreshIfNeeded(ctx context.Context) error {
	c.mutex.RLock()
	needsRefresh := time.Since(c.lastUpdate) > c.ttl
	c.mutex.RUnlock()

	if !needsRefresh {
		return nil
	}

	// 串行化刷新（避免重复 DB 查询），但不阻塞读者
	c.refreshMutex.Lock()
	defer c.refreshMutex.Unlock()

	// 双重检查：可能已被并发刷新者完成
	c.mutex.RLock()
	stale := time.Since(c.lastUpdate) > c.ttl
	c.mutex.RUnlock()
	if !stale {
		return nil
	}

	return c.refreshCache(ctx)
}

// refreshCache 刷新缓存数据
// 说明：DB 加载与索引构建在 c.mutex 之外完成，仅在指针互换瞬间持写锁。
// 缓存内部索引共享指针；对外统一返回深拷贝，避免调用方污染缓存。
// 调用方必须已持有 refreshMutex 以串行化刷新动作。
func (c *ChannelCache) refreshCache(ctx context.Context) error {
	start := time.Now()

	allChannels, err := c.store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		return err
	}

	// 构建按类型分组的索引（内部共享指针，对外深拷贝隔离）
	byModel := make(map[string][]*modelpkg.Config)
	byModelAndProtocol := make(map[string]map[string][]*modelpkg.Config)
	byType := make(map[string][]*modelpkg.Config)
	byExposedProtocol := make(map[string][]*modelpkg.Config)

	for _, channel := range allChannels {
		channelType := channel.GetChannelType()
		byType[channelType] = append(byType[channelType], channel) // 内部共享
		protocols := channel.SupportedProtocols()
		for _, protocol := range protocols {
			byExposedProtocol[protocol] = append(byExposedProtocol[protocol], channel)
		}

		// 同时填充模型索引（使用 GetModels() 辅助方法）
		for _, model := range channel.GetModels() {
			byModel[model] = append(byModel[model], channel) // 内部共享
			if _, exists := byModelAndProtocol[model]; !exists {
				byModelAndProtocol[model] = make(map[string][]*modelpkg.Config)
			}
			for _, protocol := range protocols {
				byModelAndProtocol[model][protocol] = append(byModelAndProtocol[model][protocol], channel)
			}
		}
	}

	// 原子性更新缓存（整体替换指针，临界区只覆盖赋值瞬间）
	c.mutex.Lock()
	c.allChannels = allChannels
	c.channelsByModel = byModel
	c.channelsByModelAndProtocol = byModelAndProtocol
	c.channelsByType = byType
	c.channelsByExposedProtocol = byExposedProtocol
	c.lastUpdate = time.Now()
	c.mutex.Unlock()

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
	if time.Since(c.cooldownCache.channelLastUpdate) <= c.cooldownCache.ttl {
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
	c.cooldownCache.channelLastUpdate = time.Now()
	c.mutex.Unlock()

	result := make(map[int64]time.Time, len(cooldowns))
	maps.Copy(result, cooldowns)
	return result, nil
}

// GetAllKeyCooldowns 缓存优先的Key冷却查询
func (c *ChannelCache) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	// 检查冷却缓存是否有效
	c.mutex.RLock()
	if time.Since(c.cooldownCache.keyLastUpdate) <= c.cooldownCache.ttl {
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
	c.cooldownCache.keyLastUpdate = time.Now()
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
	c.cooldownCache.channelLastUpdate = time.Time{}
	c.cooldownCache.keyLastUpdate = time.Time{}
}
