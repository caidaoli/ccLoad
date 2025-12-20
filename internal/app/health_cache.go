package app

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// HealthCache 渠道健康度缓存
type HealthCache struct {
	store  storage.Store
	config model.HealthScoreConfig

	// 成功率缓存：使用原子指针实现无锁快照替换
	// 读取时直接Load，更新时用新map整体替换，避免遍历删除的并发问题
	successRates atomic.Pointer[map[int64]float64]

	// 控制
	stopCh chan struct{}
	wg     *sync.WaitGroup

	// shutdown标志
	isShuttingDown *atomic.Bool
}

// NewHealthCache 创建健康度缓存
func NewHealthCache(store storage.Store, config model.HealthScoreConfig, shutdownCh chan struct{}, isShuttingDown *atomic.Bool, wg *sync.WaitGroup) *HealthCache {
	h := &HealthCache{
		store:          store,
		config:         config,
		stopCh:         shutdownCh,
		wg:             wg,
		isShuttingDown: isShuttingDown,
	}
	// 初始化空map
	emptyMap := make(map[int64]float64)
	h.successRates.Store(&emptyMap)
	return h
}

// Start 启动后台更新协程
func (h *HealthCache) Start() {
	if !h.config.Enabled {
		return
	}
	if h.config.UpdateIntervalSeconds <= 0 || h.config.WindowMinutes <= 0 {
		log.Printf("[WARN] 健康度缓存未启动：无效配置 update_interval=%d window_minutes=%d", h.config.UpdateIntervalSeconds, h.config.WindowMinutes)
		return
	}

	h.wg.Add(1)
	go h.updateLoop()
}

// updateLoop 定期更新成功率缓存
func (h *HealthCache) updateLoop() {
	defer h.wg.Done()

	// 立即执行一次
	h.update()

	interval := time.Duration(h.config.UpdateIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			if h.isShuttingDown.Load() {
				return
			}
			h.update()
		}
	}
}

// update 更新成功率缓存
func (h *HealthCache) update() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	since := time.Now().Add(-time.Duration(h.config.WindowMinutes) * time.Minute)
	rates, err := h.store.GetChannelSuccessRates(ctx, since)
	if err != nil {
		log.Printf("[WARN] 更新渠道成功率缓存失败: %v", err)
		return
	}

	// 原子替换：用新快照整体替换旧数据，避免遍历删除的并发问题
	h.successRates.Store(&rates)
}

// GetSuccessRate 获取渠道成功率，不存在返回1.0（新渠道不惩罚）
func (h *HealthCache) GetSuccessRate(channelID int64) float64 {
	rates := h.successRates.Load()
	if rates == nil {
		return 1.0
	}
	if v, ok := (*rates)[channelID]; ok {
		return v
	}
	return 1.0 // 新渠道默认成功率100%
}

// GetAllSuccessRates 获取所有渠道成功率（返回快照副本）
func (h *HealthCache) GetAllSuccessRates() map[int64]float64 {
	rates := h.successRates.Load()
	if rates == nil {
		return make(map[int64]float64)
	}
	// 返回副本，避免调用方修改影响缓存
	result := make(map[int64]float64, len(*rates))
	for k, v := range *rates {
		result[k] = v
	}
	return result
}

// Config 返回健康度配置
func (h *HealthCache) Config() model.HealthScoreConfig {
	return h.config
}
