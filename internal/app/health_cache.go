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

	// 统计与全局首字基准一起发布，列表筛选和路由候选不能改变渠道分数。
	snapshot atomic.Pointer[channelHealthSnapshot]

	// 控制
	stopCh chan struct{}
	wg     *sync.WaitGroup

	// shutdown标志
	isShuttingDown *atomic.Bool
}

type channelHealthSnapshot struct {
	stats      map[int64]model.ChannelHealthStats
	medianTTFB float64
}

func newChannelHealthSnapshot(stats map[int64]model.ChannelHealthStats) *channelHealthSnapshot {
	samples := make([]float64, 0, len(stats))
	for _, st := range stats {
		if st.FirstByteSampleCount > 0 && st.AvgFirstByteSeconds > 0 {
			samples = append(samples, st.AvgFirstByteSeconds)
		}
	}
	return &channelHealthSnapshot{stats: stats, medianTTFB: medianFloat64(samples)}
}

func (snapshot *channelHealthSnapshot) healthStats(channelID int64) model.ChannelHealthStats {
	if snapshot != nil {
		if stats, ok := snapshot.stats[channelID]; ok {
			return stats
		}
	}
	return model.ChannelHealthStats{SuccessRate: 1.0} // 新渠道默认成功率100%，不惩罚
}

func (snapshot *channelHealthSnapshot) effectivePriority(ch *model.Config, cfg model.HealthScoreConfig) float64 {
	medianTTFB := 0.0
	if snapshot != nil {
		medianTTFB = snapshot.medianTTFB
	}
	return calculateEffectivePriority(ch, snapshot.healthStats(ch.ID), cfg, medianTTFB)
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
	stats, err := h.store.GetChannelSuccessRates(ctx, since)
	if err != nil {
		log.Printf("[WARN] 更新渠道成功率缓存失败: %v", err)
		return
	}

	// 原子替换：用新快照整体替换旧数据，避免遍历删除的并发问题
	h.snapshot.Store(newChannelHealthSnapshot(stats))
}

// GetHealthStats 获取渠道健康统计，不存在返回默认值（新渠道不惩罚）
func (h *HealthCache) GetHealthStats(channelID int64) model.ChannelHealthStats {
	return h.snapshot.Load().healthStats(channelID)
}

// Config 返回健康度配置
func (h *HealthCache) Config() model.HealthScoreConfig {
	return h.config
}
