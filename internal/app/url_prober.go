package app

import (
	"context"
	"log"
	"net/http"
	"time"

	"ccLoad/internal/storage"
)

// URLProber 定期探测低流量URL的网络延迟
type URLProber struct {
	selector     *URLSelector
	channelCache *storage.ChannelCache
	client       *http.Client
	interval     time.Duration
	idleThresh   time.Duration
}

// NewURLProber 创建URL探测器
func NewURLProber(selector *URLSelector, channelCache *storage.ChannelCache, client *http.Client) *URLProber {
	return &URLProber{
		selector:     selector,
		channelCache: channelCache,
		client:       client,
		interval:     60 * time.Second,
		idleThresh:   5 * time.Minute,
	}
}

// shouldProbe 判断URL是否需要探测（最近无流量）
func (p *URLProber) shouldProbe(channelID int64, url string) bool {
	lastSeen := p.selector.LastSeen(channelID, url)
	if lastSeen.IsZero() {
		return true // 从未使用过
	}
	return time.Since(lastSeen) > p.idleThresh
}

// Start 启动后台探测循环
func (p *URLProber) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.probeAll(ctx)
			}
		}
	}()
}

// probeAll 探测所有多URL渠道中的空闲URL
func (p *URLProber) probeAll(ctx context.Context) {
	channels, err := p.channelCache.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		return
	}
	for _, ch := range channels {
		urls := ch.GetURLs()
		if len(urls) <= 1 {
			continue // 单URL渠道不需要探测
		}

		for _, url := range urls {
			if !p.shouldProbe(ch.ID, url) {
				continue
			}
			go p.probeOne(ctx, ch.ID, url)
		}
	}
}

// probeOne 对单个URL发送轻量级探测
func (p *URLProber) probeOne(ctx context.Context, channelID int64, baseURL string) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, "GET", baseURL, nil)
	if err != nil {
		return
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	ttfb := time.Since(start)

	if err != nil {
		// 连接失败：记录高延迟，让排序自然降权
		p.selector.RecordLatency(channelID, baseURL, 30*time.Second)
		return
	}
	_ = resp.Body.Close()

	// 不管状态码，只要TCP连通就记录TTFB
	p.selector.RecordLatency(channelID, baseURL, ttfb)
	log.Printf("[PROBE] channel=%d url=%s ttfb=%v status=%d", channelID, baseURL, ttfb, resp.StatusCode)
}
