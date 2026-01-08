package app

import (
	"sync"
	"time"
)

// CostCache 渠道每日成本缓存
// 启动时从数据库加载当日成本，请求完成后累加，跨天自动重置
type CostCache struct {
	mu       sync.RWMutex
	costs    map[int64]float64 // channelID -> 今日已消耗成本
	dayStart time.Time         // 当前统计周期的0点时间
}

// NewCostCache 创建成本缓存
func NewCostCache() *CostCache {
	now := time.Now()
	return &CostCache{
		costs:    make(map[int64]float64),
		dayStart: todayStart(now),
	}
}

// todayStart 返回给定时间当天0点
func todayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// checkAndResetIfNewDay 检查是否跨天，如果是则重置缓存
// 调用方必须持有写锁
func (c *CostCache) checkAndResetIfNewDay(now time.Time) {
	today := todayStart(now)
	if !today.Equal(c.dayStart) {
		// 跨天，重置缓存
		c.costs = make(map[int64]float64)
		c.dayStart = today
	}
}

// Add 累加成本（请求完成后调用）
func (c *CostCache) Add(channelID int64, cost float64) {
	if cost <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.checkAndResetIfNewDay(time.Now())
	c.costs[channelID] += cost
}

// Get 获取渠道今日成本
func (c *CostCache) Get(channelID int64) float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// 读锁下检查跨天（只读检查，不重置）
	today := todayStart(time.Now())
	if !today.Equal(c.dayStart) {
		return 0 // 跨天了，返回0，下次Add时会重置
	}

	return c.costs[channelID]
}

// GetAll 批量获取所有渠道今日成本（供过滤器使用）
func (c *CostCache) GetAll() map[int64]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// 读锁下检查跨天
	today := todayStart(time.Now())
	if !today.Equal(c.dayStart) {
		return make(map[int64]float64) // 跨天了，返回空map
	}

	// 返回副本，避免并发问题
	result := make(map[int64]float64, len(c.costs))
	for k, v := range c.costs {
		result[k] = v
	}
	return result
}

// Load 加载初始数据（启动时调用）
func (c *CostCache) Load(costs map[int64]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	c.dayStart = todayStart(now)
	c.costs = make(map[int64]float64, len(costs))
	for k, v := range costs {
		c.costs[k] = v
	}
}

// DayStart 返回当前统计周期的0点时间（用于查询数据库）
func (c *CostCache) DayStart() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dayStart
}
