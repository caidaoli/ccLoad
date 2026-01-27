package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// StatsCache 统计结果缓存层
//
// 核心职责：
// - 缓存统计查询结果，减少重复聚合计算
// - 智能 TTL：越近的数据 TTL 越短
// - filter 哈希：支持复杂过滤器的缓存键生成
// - 定期清理：后台 goroutine 清理过期条目，防止内存泄漏
// - 容量限制：最多 1000 个条目，超过时强制清理
//
// 设计原则：
// - KISS：简单的 sync.Map，避免过度工程
// - 透明降级：缓存失效不影响业务
type StatsCache struct {
	store      storage.Store
	cache      sync.Map     // key: cacheKey, value: *cachedStats
	entryCount atomic.Int64 // 当前缓存条目数（原子计数，避免锁）
	stopCh     chan struct{}
	stopWg     sync.WaitGroup
}

const maxCacheEntries = 1000 // 最大缓存条目数

// cachedStats 缓存的统计数据
type cachedStats struct {
	data   any       // 实际数据（[]model.StatsEntry 或 *model.RPMStats）
	expiry time.Time // 过期时间
}

// NewStatsCache 创建统计缓存实例
func NewStatsCache(store storage.Store) *StatsCache {
	sc := &StatsCache{
		store:  store,
		stopCh: make(chan struct{}),
	}

	// 启动后台清理 goroutine
	sc.stopWg.Add(1)
	go sc.cleanupWorker()

	return sc
}

// cleanupWorker 后台清理过期缓存条目
func (sc *StatsCache) cleanupWorker() {
	defer sc.stopWg.Done()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-sc.stopCh:
			return
		case <-ticker.C:
			sc.cleanupExpired()
		}
	}
}

// cleanupExpired 清理所有过期条目
func (sc *StatsCache) cleanupExpired() {
	now := time.Now()
	sc.cache.Range(func(key, value any) bool {
		cs := value.(*cachedStats)
		if now.After(cs.expiry) {
			if _, ok := sc.cache.LoadAndDelete(key); ok {
				sc.entryCount.Add(-1)
			}
		}
		return true
	})
}

// storeCache 存储缓存条目（带容量检查）
//
// 使用 LoadOrStore 保证原子性：要么是新插入（计数+1），要么是更新（计数不变）
func (sc *StatsCache) storeCache(key string, value *cachedStats) {
	if _, loaded := sc.cache.LoadOrStore(key, value); loaded {
		// key 已存在，LoadOrStore 不会插入，手动更新值
		sc.cache.Store(key, value)
		return
	}
	// 新插入成功，增加计数
	if sc.entryCount.Add(1) > maxCacheEntries {
		sc.cleanupExpired()
	}
}

// Close 关闭缓存（停止清理 goroutine）
func (sc *StatsCache) Close() {
	close(sc.stopCh)
	sc.stopWg.Wait()
}

// GetStats 获取统计数据（带缓存）
func (sc *StatsCache) GetStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) ([]model.StatsEntry, error) {
	key := buildCacheKey("stats", startTime, endTime, filter)

	// 尝试缓存
	if cached, ok := sc.cache.Load(key); ok {
		cs := cached.(*cachedStats)
		if time.Now().Before(cs.expiry) {
			return cs.data.([]model.StatsEntry), nil
		}
	}

	// 缓存未命中，查询数据库
	result, err := sc.store.GetStats(ctx, startTime, endTime, filter, isToday)
	if err != nil {
		return nil, err
	}

	// 写入缓存
	ttl := calculateTTL(endTime)
	sc.storeCache(key, &cachedStats{
		data:   result,
		expiry: time.Now().Add(ttl),
	})

	return result, nil
}

// GetStatsLite 获取轻量统计数据（带缓存）
func (sc *StatsCache) GetStatsLite(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter) ([]model.StatsEntry, error) {
	key := buildCacheKey("stats_lite", startTime, endTime, filter)

	// 尝试缓存
	if cached, ok := sc.cache.Load(key); ok {
		cs := cached.(*cachedStats)
		if time.Now().Before(cs.expiry) {
			return cs.data.([]model.StatsEntry), nil
		}
	}

	// 缓存未命中，查询数据库
	result, err := sc.store.GetStatsLite(ctx, startTime, endTime, filter)
	if err != nil {
		return nil, err
	}

	// 写入缓存
	ttl := calculateTTL(endTime)
	sc.storeCache(key, &cachedStats{
		data:   result,
		expiry: time.Now().Add(ttl),
	})

	return result, nil
}

// GetRPMStats 获取 RPM 统计（带缓存）
func (sc *StatsCache) GetRPMStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) (*model.RPMStats, error) {
	key := buildCacheKey("rpm", startTime, endTime, filter)

	// 尝试缓存
	if cached, ok := sc.cache.Load(key); ok {
		cs := cached.(*cachedStats)
		if time.Now().Before(cs.expiry) {
			return cs.data.(*model.RPMStats), nil
		}
	}

	// 缓存未命中，查询数据库
	result, err := sc.store.GetRPMStats(ctx, startTime, endTime, filter, isToday)
	if err != nil {
		return nil, err
	}

	// 写入缓存
	ttl := calculateTTL(endTime)
	sc.storeCache(key, &cachedStats{
		data:   result,
		expiry: time.Now().Add(ttl),
	})

	return result, nil
}

// buildCacheKey 生成缓存键
func buildCacheKey(typ string, startTime, endTime time.Time, filter *model.LogFilter) string {
	// 使用时间戳（秒）+ filter 哈希作为键
	filterHash := hashFilter(filter)
	return fmt.Sprintf("%s:%d:%d:%s", typ, startTime.Unix(), endTime.Unix(), filterHash)
}

// hashFilter 对 filter 进行哈希
func hashFilter(filter *model.LogFilter) string {
	if filter == nil {
		return "nil"
	}

	// 构建 filter 的字符串表示
	var parts []string
	if filter.ChannelID != nil {
		parts = append(parts, fmt.Sprintf("ch:%d", *filter.ChannelID))
	}
	if filter.ChannelType != "" {
		parts = append(parts, fmt.Sprintf("type:%s", filter.ChannelType))
	}
	if filter.Model != "" {
		parts = append(parts, fmt.Sprintf("model:%s", filter.Model))
	}
	if filter.ChannelNameLike != "" {
		parts = append(parts, fmt.Sprintf("name:%s", filter.ChannelNameLike))
	}
	if filter.ModelLike != "" {
		parts = append(parts, fmt.Sprintf("model_like:%s", filter.ModelLike))
	}
	if filter.AuthTokenID != nil {
		parts = append(parts, fmt.Sprintf("auth:%d", *filter.AuthTokenID))
	}

	// 排序确保顺序一致性
	sort.Strings(parts)

	// 计算 SHA256 哈希
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))[:16] // 取前16字符即可
}

// calculateTTL 根据时间范围计算 TTL
//
// TTL 策略：越近的数据 TTL 越短
//   - 最近 1 小时：30 秒
//   - 今天：5 分钟
//   - 最近 7 天：30 分钟
//   - 历史数据：2 小时
func calculateTTL(endTime time.Time) time.Duration {
	now := time.Now()

	// 最近 1 小时
	if endTime.After(now.Add(-1 * time.Hour)) {
		return 30 * time.Second
	}

	// 今天
	if endTime.After(now.Add(-24 * time.Hour)) {
		return 5 * time.Minute
	}

	// 最近 7 天
	if endTime.After(now.Add(-7 * 24 * time.Hour)) {
		return 30 * time.Minute
	}

	// 历史数据
	return 2 * time.Hour
}
