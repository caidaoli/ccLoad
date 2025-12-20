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

	// 成功率缓存 map[channelID]successRate
	successRates sync.Map

	// 控制
	stopCh chan struct{}
	wg     *sync.WaitGroup

	// shutdown标志
	isShuttingDown *atomic.Bool
}

// NewHealthCache 创建健康度缓存
func NewHealthCache(store storage.Store, config model.HealthScoreConfig, shutdownCh chan struct{}, isShuttingDown *atomic.Bool, wg *sync.WaitGroup) *HealthCache {
	return &HealthCache{
		store:          store,
		config:         config,
		stopCh:         shutdownCh,
		wg:             wg,
		isShuttingDown: isShuttingDown,
	}
}

// Start 启动后台更新协程
func (h *HealthCache) Start() {
	if !h.config.Enabled {
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

	// 更新缓存
	for channelID, rate := range rates {
		h.successRates.Store(channelID, rate)
	}
}

// GetSuccessRate 获取渠道成功率，不存在返回1.0（新渠道不惩罚）
func (h *HealthCache) GetSuccessRate(channelID int64) float64 {
	if v, ok := h.successRates.Load(channelID); ok {
		return v.(float64)
	}
	return 1.0 // 新渠道默认成功率100%
}

// GetAllSuccessRates 获取所有渠道成功率
func (h *HealthCache) GetAllSuccessRates() map[int64]float64 {
	result := make(map[int64]float64)
	h.successRates.Range(func(key, value any) bool {
		result[key.(int64)] = value.(float64)
		return true
	})
	return result
}

// Config 返回健康度配置
func (h *HealthCache) Config() model.HealthScoreConfig {
	return h.config
}
