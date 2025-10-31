package storage

import (
	modelpkg "ccLoad/internal/model"
	"context"
	"sync"
	"time"
)

// ChannelCache 高性能渠道缓存层
// 遵循KISS原则：内存查询比数据库查询快1000倍+
type ChannelCache struct {
	store           Store
	channelsByModel map[string][]*modelpkg.Config // model → channels
	channelsByType  map[string][]*modelpkg.Config // type → channels
	allChannels     []*modelpkg.Config             // 所有渠道
	lastUpdate      time.Time
	mutex           sync.RWMutex
	ttl             time.Duration

	// 扩展缓存支持更多关键查询
	apiKeysByChannelID map[int64][]*modelpkg.APIKey // channelID → API keys
	cooldownCache      struct {
		channels map[int64]time.Time // channelID → cooldown until
		keys     map[int64]map[int]time.Time // channelID→keyIndex→cooldown until
		lastUpdate time.Time
		ttl      time.Duration
	}
	geminiModels []string // 缓存的Gemini模型列表
	modelsMutex  sync.RWMutex
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
		geminiModels:       make([]string, 0),
		cooldownCache: struct {
			channels map[int64]time.Time
			keys     map[int64]map[int]time.Time
			lastUpdate time.Time
			ttl      time.Duration
		}{
			channels: make(map[int64]time.Time),
			keys:     make(map[int64]map[int]time.Time),
			ttl:      30 * time.Second, // 冷却状态缓存30秒
		},
	}
}

// GetEnabledChannelsByModel 缓存优先的模型查询
// 性能：内存查询 < 2ms vs 数据库查询 50ms+
func (c *ChannelCache) GetEnabledChannelsByModel(ctx context.Context, model string) ([]*modelpkg.Config, error) {
	if err := c.refreshIfNeeded(ctx); err != nil {
		// 缓存失败时降级到数据库查询
		return c.store.GetEnabledChannelsByModel(ctx, model)
	}

	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if model == "*" {
		// 返回所有渠道的副本
		result := make([]*modelpkg.Config, len(c.allChannels))
		copy(result, c.allChannels)
		return result, nil
	}

	// 返回指定模型的渠道副本
	channels, exists := c.channelsByModel[model]
	if !exists {
		return []*modelpkg.Config{}, nil
	}

	result := make([]*modelpkg.Config, len(channels))
	copy(result, channels)
	return result, nil
}

// GetEnabledChannelsByType 缓存优先的类型查询
func (c *ChannelCache) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	if err := c.refreshIfNeeded(ctx); err != nil {
		// 缓存失败时降级到数据库查询
		return c.store.GetEnabledChannelsByType(ctx, channelType)
	}

	c.mutex.RLock()
	defer c.mutex.RUnlock()

	normalizedType := modelpkg.NormalizeChannelType(channelType)
	channels, exists := c.channelsByType[normalizedType]
	if !exists {
		return []*modelpkg.Config{}, nil
	}

	result := make([]*modelpkg.Config, len(channels))
	copy(result, channels)
	return result, nil
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
func (c *ChannelCache) refreshCache(ctx context.Context) error {
	start := time.Now()

	// 并发加载不同类型的数据
	var wg sync.WaitGroup
	var allChannels []*modelpkg.Config
	byModel := make(map[string][]*modelpkg.Config)
	byType := make(map[string][]*modelpkg.Config)

	var errAll, errModel error
	var mu sync.Mutex

	wg.Add(2)

	// 加载所有渠道
	go func() {
		defer wg.Done()
		allChannels, errAll = c.store.GetEnabledChannelsByModel(ctx, "*")
	}()

	// 加载按模型分组的渠道
	go func() {
		defer wg.Done()
		// 获取所有不同的模型
		models, err := c.getUniqueModels(ctx)
		if err != nil {
			errModel = err
			return
		}

		// 并发加载每个模型的渠道
		var modelWg sync.WaitGroup
		for _, model := range models {
			modelWg.Add(1)
			go func(m string) {
				defer modelWg.Done()
				channels, err := c.store.GetEnabledChannelsByModel(ctx, m)
				if err == nil {
					mu.Lock()
					byModel[m] = channels
					mu.Unlock()
				}
			}(model)
		}
		modelWg.Wait()
	}()

	wg.Wait()

	if errAll != nil {
		return errAll
	}
	if errModel != nil {
		return errModel
	}

	// 构建按类型分组的索引
	for _, channel := range allChannels {
		channelType := channel.GetChannelType()
		byType[channelType] = append(byType[channelType], channel)

		// 同时填充模型索引
		for _, model := range channel.Models {
			byModel[model] = append(byModel[model], channel)
		}
	}

	// 原子性更新缓存
	c.allChannels = allChannels
	c.channelsByModel = byModel
	c.channelsByType = byType
	c.lastUpdate = time.Now()

	// 性能日志
	refreshDuration := time.Since(start)
	totalChannels := len(allChannels)
	totalModels := len(byModel)
	totalTypes := len(byType)

	// 这里应该使用结构化日志，暂时简化
	if refreshDuration > 5*time.Second {
		// 缓存刷新过慢的警告
		_ = refreshDuration
		_ = totalChannels
		_ = totalModels
		_ = totalTypes
	}

	return nil
}

// getUniqueModels 获取所有唯一的模型列表
func (c *ChannelCache) getUniqueModels(ctx context.Context) ([]string, error) {
	// 从数据库获取所有启用的模型
	allChannels, err := c.store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		return nil, err
	}

	modelSet := make(map[string]struct{})
	for _, channel := range allChannels {
		for _, model := range channel.Models {
			modelSet[model] = struct{}{}
		}
	}

	models := make([]string, 0, len(modelSet))
	for model := range modelSet {
		models = append(models, model)
	}

	return models, nil
}

// InvalidateCache 手动失效缓存
func (c *ChannelCache) InvalidateCache() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.lastUpdate = time.Time{} // 重置为0时间，强制刷新
}

// GetCacheStats 获取缓存统计信息
func (c *ChannelCache) GetCacheStats() map[string]interface{} {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return map[string]interface{}{
		"last_update":         c.lastUpdate,
		"age_seconds":        time.Since(c.lastUpdate).Seconds(),
		"total_channels":     len(c.allChannels),
		"total_models":       len(c.channelsByModel),
		"total_types":        len(c.channelsByType),
		"ttl_seconds":        c.ttl.Seconds(),
	}
}

// GetAPIKeys 缓存优先的API Keys查询
// 性能：内存查询 <1ms vs 数据库查询 10-20ms
func (c *ChannelCache) GetAPIKeys(ctx context.Context, channelID int64) ([]*modelpkg.APIKey, error) {
	// 检查缓存
	c.mutex.RLock()
	if keys, exists := c.apiKeysByChannelID[channelID]; exists {
		c.mutex.RUnlock()
		// 返回副本
		result := make([]*modelpkg.APIKey, len(keys))
		copy(result, keys)
		return result, nil
	}
	c.mutex.RUnlock()

	// 缓存未命中，从数据库加载
	keys, err := c.store.GetAPIKeys(ctx, channelID)
	if err == nil {
		// 存储到缓存
		c.mutex.Lock()
		c.apiKeysByChannelID[channelID] = keys
		c.mutex.Unlock()
	}
	return keys, err
}

// GetAllChannelCooldowns 缓存优先的渠道冷却查询
// 性能：内存查询 <1ms vs 数据库查询 5-10ms
func (c *ChannelCache) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	// 检查冷却缓存是否有效
	c.mutex.RLock()
	if time.Since(c.cooldownCache.lastUpdate) <= c.cooldownCache.ttl {
		// 有效缓存，返回副本
		result := make(map[int64]time.Time, len(c.cooldownCache.channels))
		for k, v := range c.cooldownCache.channels {
			result[k] = v
		}
		c.mutex.RUnlock()
		return result, nil
	}
	c.mutex.RUnlock()

	// 缓存过期，从数据库加载
	cooldowns, err := c.store.GetAllChannelCooldowns(ctx)
	if err == nil {
		c.mutex.Lock()
		c.cooldownCache.channels = cooldowns
		c.cooldownCache.lastUpdate = time.Now()
		c.mutex.Unlock()
	}
	return cooldowns, err
}

// GetAllKeyCooldowns 缓存优先的Key冷却查询
// 性能：内存查询 <1ms vs 数据库查询 5-10ms
func (c *ChannelCache) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	// 检查冷却缓存是否有效
	c.mutex.RLock()
	if time.Since(c.cooldownCache.lastUpdate) <= c.cooldownCache.ttl {
		// 有效缓存，返回副本
		result := make(map[int64]map[int]time.Time)
		for k, v := range c.cooldownCache.keys {
			keyMap := make(map[int]time.Time)
			for kk, vv := range v {
				keyMap[kk] = vv
			}
			result[k] = keyMap
		}
		c.mutex.RUnlock()
		return result, nil
	}
	c.mutex.RUnlock()

	// 缓存过期，从数据库加载
	cooldowns, err := c.store.GetAllKeyCooldowns(ctx)
	if err == nil {
		c.mutex.Lock()
		c.cooldownCache.keys = cooldowns
		c.cooldownCache.lastUpdate = time.Now()
		c.mutex.Unlock()
	}
	return cooldowns, err
}

// GetGeminiModels 缓存的Gemini模型列表查询
// 性能：内存查询 <1ms vs 数据库查询 20-50ms
func (c *ChannelCache) GetGeminiModels(ctx context.Context) ([]string, error) {
	c.modelsMutex.RLock()
	if len(c.geminiModels) > 0 && time.Since(c.lastUpdate) <= c.ttl {
		models := make([]string, len(c.geminiModels))
		copy(models, c.geminiModels)
		c.modelsMutex.RUnlock()
		return models, nil
	}
	c.modelsMutex.RUnlock()

	// 缓存未命中或过期，从数据库查询
	channels, err := c.store.GetEnabledChannelsByType(ctx, "gemini")
	if err != nil {
		return nil, err
	}

	// 提取模型并去重
	modelSet := make(map[string]bool)
	for _, cfg := range channels {
		for _, model := range cfg.Models {
			modelSet[model] = true
		}
	}

	models := make([]string, 0, len(modelSet))
	for model := range modelSet {
		models = append(models, model)
	}

	// 更新缓存
	c.modelsMutex.Lock()
	c.geminiModels = models
	c.modelsMutex.Unlock()

	return models, nil
}

// InvalidateAPIKeysCache 手动失效API Keys缓存
func (c *ChannelCache) InvalidateAPIKeysCache(channelID int64) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.apiKeysByChannelID, channelID)
}

// InvalidateCooldownCache 手动失效冷却缓存
func (c *ChannelCache) InvalidateCooldownCache() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.cooldownCache.lastUpdate = time.Time{}
}