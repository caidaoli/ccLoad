package app

import (
	"context"
	"errors"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/url"
	"slices"
	"sync"
	"time"
)

const (
	defaultURLSelectorCleanupInterval = time.Hour
	defaultURLSelectorLatencyMaxAge   = 24 * time.Hour
	defaultURLSelectorProbeTimeout    = 5 * time.Second
)

// urlKey 标识渠道+URL的组合
type urlKey struct {
	channelID int64
	url       string
}

// ewmaValue 指数加权移动平均值
type ewmaValue struct {
	value    float64 // 当前EWMA值（毫秒）
	lastSeen time.Time
}

// urlCooldownState URL冷却状态
type urlCooldownState struct {
	until            time.Time
	consecutiveFails int
}

// urlRequestCount URL调用计数（内存）
type urlRequestCount struct {
	success int64
	failure int64
}

// URLSelector 基于EWMA延迟和冷却状态选择最优URL
type URLSelector struct {
	mu           sync.RWMutex
	latencies    map[urlKey]*ewmaValue
	cooldowns    map[urlKey]urlCooldownState
	requests     map[urlKey]*urlRequestCount
	probing      map[urlKey]time.Time
	disabled     map[urlKey]bool // 手动禁用的URL（纯内存，重启恢复）
	alpha        float64         // EWMA权重因子
	cooldownBase time.Duration   // 基础冷却时间
	cooldownMax  time.Duration   // 最大冷却时间
	probeTimeout time.Duration
	probeDial    func(ctx context.Context, network, address string) (net.Conn, error)
	// 低频清理调度，避免 map 长期只增不减。
	cleanupInterval time.Duration
	latencyMaxAge   time.Duration
	nextCleanup     time.Time
}

func normalizeLatencyMS(ttfb time.Duration) float64 {
	ms := float64(ttfb) / float64(time.Millisecond)
	if ms <= 0 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return 0.1
	}
	return ms
}

func (s *URLSelector) upsertLatencyLocked(key urlKey, ms float64, now time.Time) {
	if e, ok := s.latencies[key]; ok {
		e.value = s.alpha*ms + (1-s.alpha)*e.value
		e.lastSeen = now
		return
	}
	s.latencies[key] = &ewmaValue{value: ms, lastSeen: now}
}

// NewURLSelector 创建URL选择器
func NewURLSelector() *URLSelector {
	now := time.Now()
	return &URLSelector{
		latencies:       make(map[urlKey]*ewmaValue),
		cooldowns:       make(map[urlKey]urlCooldownState),
		requests:        make(map[urlKey]*urlRequestCount),
		probing:         make(map[urlKey]time.Time),
		disabled:        make(map[urlKey]bool),
		alpha:           0.3,
		cooldownBase:    2 * time.Minute,
		cooldownMax:     30 * time.Minute,
		probeTimeout:    defaultURLSelectorProbeTimeout,
		probeDial:       (&net.Dialer{}).DialContext,
		cleanupInterval: defaultURLSelectorCleanupInterval,
		latencyMaxAge:   defaultURLSelectorLatencyMaxAge,
		nextCleanup:     now.Add(defaultURLSelectorCleanupInterval),
	}
}

func (s *URLSelector) gcLocked(now time.Time, maxAge time.Duration) {
	if maxAge <= 0 {
		maxAge = s.latencyMaxAge
	}
	if maxAge > 0 {
		cutoff := now.Add(-maxAge)
		for key, ewma := range s.latencies {
			if ewma == nil || ewma.lastSeen.IsZero() || ewma.lastSeen.Before(cutoff) {
				delete(s.latencies, key)
				delete(s.requests, key)
			}
		}
	}

	for key, cooldown := range s.cooldowns {
		if !now.Before(cooldown.until) {
			delete(s.cooldowns, key)
		}
	}

	// probing 条目正常生命周期极短（<= probeTimeout）。
	// 若因 goroutine 异常未清理而滞留，这里兜底回收，避免该 URL 永远无法被再次探测。
	probeCutoff := now.Add(-2 * s.probeTimeout)
	for key, started := range s.probing {
		if started.Before(probeCutoff) {
			delete(s.probing, key)
		}
	}
}

func (s *URLSelector) maybeCleanupLocked(now time.Time) {
	if s.cleanupInterval <= 0 {
		return
	}
	if !s.nextCleanup.IsZero() && now.Before(s.nextCleanup) {
		return
	}
	s.gcLocked(now, s.latencyMaxAge)
	s.nextCleanup = now.Add(s.cleanupInterval)
}

// GC 手动触发状态清理（用于测试或运维兜底）。
// maxAge 控制 latency 条目的保留时长，cooldown 条目始终按 until 过期清理。
func (s *URLSelector) GC(maxAge time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now(), maxAge)
}

// PruneChannel 清理指定渠道中不再存在的 URL 状态。
// keepURLs 为空时会移除该渠道全部状态。
func (s *URLSelector) PruneChannel(channelID int64, keepURLs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	keep := make(map[string]struct{}, len(keepURLs))
	for _, u := range keepURLs {
		keep[u] = struct{}{}
	}

	for key := range s.latencies {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.latencies, key)
		}
	}
	for key := range s.cooldowns {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.cooldowns, key)
		}
	}
	for key := range s.requests {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.requests, key)
		}
	}
}

// RemoveChannel 移除指定渠道的全部 URL 状态。
func (s *URLSelector) RemoveChannel(channelID int64) {
	s.PruneChannel(channelID, nil)
}

// SelectURL 从候选URL中选择最优的
// 返回选中的URL和在原列表中的索引
func (s *URLSelector) SelectURL(channelID int64, urls []string) (string, int) {
	if len(urls) == 0 {
		return "", -1
	}
	if len(urls) == 1 {
		return urls[0], 0
	}

	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	type candidate struct {
		url     string
		idx     int
		latency float64 // -1 表示无数据
		cooled  bool
	}

	candidates := make([]candidate, 0, len(urls))
	for i, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		// 跳过手动禁用的URL
		if s.disabled[key] {
			continue
		}
		c := candidate{url: u, idx: i, latency: -1}

		if e, ok := s.latencies[key]; ok {
			c.latency = e.value
		}
		if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
			c.cooled = true
		}
		candidates = append(candidates, c)
	}

	// 所有URL都被禁用：退化到原始列表（兜底，避免死锁）
	if len(candidates) == 0 {
		return urls[0], 0
	}

	// 分离可用和冷却中的候选
	var available, cooled []candidate
	for _, c := range candidates {
		if c.cooled {
			cooled = append(cooled, c)
		} else {
			available = append(available, c)
		}
	}

	// 如果所有URL都冷却了，退化到全部候选（兜底）
	if len(available) == 0 {
		available = cooled
	}

	// 未探索URL优先：随机选一个未探索的
	var unknown, known []candidate
	for _, c := range available {
		if c.latency < 0 {
			unknown = append(unknown, c)
		} else {
			known = append(known, c)
		}
	}
	if len(unknown) > 0 {
		pick := unknown[rand.IntN(len(unknown))]
		return pick.url, pick.idx
	}

	// 所有URL已探索：加权随机（权重=1/latency），延迟越低概率越高
	totalWeight := 0.0
	weights := make([]float64, len(known))
	for i, c := range known {
		latency := c.latency
		if latency <= 0 || math.IsNaN(latency) || math.IsInf(latency, 0) {
			latency = 0.1
		}
		weights[i] = 1.0 / latency
		totalWeight += weights[i]
	}
	if totalWeight <= 0 || math.IsNaN(totalWeight) || math.IsInf(totalWeight, 0) {
		pick := known[rand.IntN(len(known))]
		return pick.url, pick.idx
	}
	r := rand.Float64() * totalWeight
	for i, w := range weights {
		r -= w
		if r <= 0 {
			return known[i].url, known[i].idx
		}
	}
	return known[len(known)-1].url, known[len(known)-1].idx
}

// RecordLatency 记录URL的首字节时间，更新EWMA
func (s *URLSelector) RecordLatency(channelID int64, url string, ttfb time.Duration) {
	key := urlKey{channelID: channelID, url: url}
	ms := normalizeLatencyMS(ttfb)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.maybeCleanupLocked(now)

	s.upsertLatencyLocked(key, ms, now)

	// 成功请求：清除冷却状态，立即恢复可用
	delete(s.cooldowns, key)

	// 递增成功计数
	if rc := s.requests[key]; rc != nil {
		rc.success++
	} else {
		s.requests[key] = &urlRequestCount{success: 1}
	}
}

// CooldownURL 对URL施加指数退避冷却
func (s *URLSelector) CooldownURL(channelID int64, url string) {
	key := urlKey{channelID: channelID, url: url}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.maybeCleanupLocked(now)

	cd := s.cooldowns[key]
	cd.consecutiveFails++

	// 指数退避: base * 2^(fails-1), 上限 max
	multiplier := math.Pow(2, float64(cd.consecutiveFails-1))
	duration := time.Duration(float64(s.cooldownBase) * multiplier)
	if duration > s.cooldownMax {
		duration = s.cooldownMax
	}

	cd.until = now.Add(duration)
	s.cooldowns[key] = cd

	// 递增失败计数
	if rc := s.requests[key]; rc != nil {
		rc.failure++
	} else {
		s.requests[key] = &urlRequestCount{failure: 1}
	}
}

// IsCooledDown 检查URL是否在冷却中
func (s *URLSelector) IsCooledDown(channelID int64, url string) bool {
	key := urlKey{channelID: channelID, url: url}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cd, ok := s.cooldowns[key]
	return ok && time.Now().Before(cd.until)
}

// URLStat 单个URL的运行时状态快照
type URLStat struct {
	URL              string  `json:"url"`
	LatencyMs        float64 `json:"latency_ms"`         // EWMA延迟（毫秒），-1表示无数据
	CooledDown       bool    `json:"cooled_down"`        // 是否在冷却中
	CooldownRemainMs int64   `json:"cooldown_remain_ms"` // 剩余冷却时间（毫秒）
	Requests         int64   `json:"requests"`           // 成功调用次数
	Failures         int64   `json:"failures"`           // 失败调用次数
	Disabled         bool    `json:"disabled"`           // 是否被手动禁用
}

// GetURLStats 返回指定渠道各URL的运行时状态（延迟、冷却）
func (s *URLSelector) GetURLStats(channelID int64, urls []string) []URLStat {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := make([]URLStat, len(urls))
	for i, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		st := URLStat{URL: u, LatencyMs: -1}

		if s.disabled[key] {
			st.Disabled = true
		}
		if e, ok := s.latencies[key]; ok {
			st.LatencyMs = e.value
		}
		if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
			st.CooledDown = true
			st.CooldownRemainMs = cd.until.Sub(now).Milliseconds()
		}
		if rc, ok := s.requests[key]; ok {
			st.Requests = rc.success
			st.Failures = rc.failure
		}
		stats[i] = st
	}
	return stats
}

// sortedURL 排序后的URL条目
type sortedURL struct {
	url string
	idx int
}

// SortURLs 返回按EWMA延迟排序的全部URL列表（非冷却URL优先，用于故障切换遍历）
func (s *URLSelector) SortURLs(channelID int64, urls []string) []sortedURL {
	if len(urls) == 0 {
		return nil
	}
	if len(urls) == 1 {
		return []sortedURL{{url: urls[0], idx: 0}}
	}

	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	type candidate struct {
		url     string
		idx     int
		latency float64
		cooled  bool
	}

	candidates := make([]candidate, 0, len(urls))
	for i, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		// 跳过手动禁用的URL
		if s.disabled[key] {
			continue
		}
		c := candidate{url: u, idx: i, latency: -1}
		if e, ok := s.latencies[key]; ok {
			c.latency = e.value
		}
		if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
			c.cooled = true
		}
		candidates = append(candidates, c)
	}

	// 所有URL都被禁用：退化到原始列表
	if len(candidates) == 0 {
		result := make([]sortedURL, len(urls))
		for i, u := range urls {
			result[i] = sortedURL{url: u, idx: i}
		}
		return result
	}

	// 先随机打乱，再稳定排序
	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	// 排序优先级：非冷却 > 冷却，同组内未探索 > 已知，已知按EWMA升序
	slices.SortStableFunc(candidates, func(ci, cj candidate) int {
		if ci.cooled != cj.cooled {
			if !ci.cooled {
				return -1 // 非冷却优先
			}
			return 1
		}
		iKnown, jKnown := ci.latency >= 0, cj.latency >= 0
		if iKnown != jKnown {
			if !iKnown {
				return -1 // 未探索的优先
			}
			return 1
		}
		if !iKnown {
			return 0 // 都未探索：保持随机顺序
		}
		if ci.latency < cj.latency {
			return -1
		}
		if ci.latency > cj.latency {
			return 1
		}
		return 0
	})

	result := make([]sortedURL, len(candidates))
	for i, c := range candidates {
		result[i] = sortedURL{url: c.url, idx: c.idx}
	}
	return result
}

// DisableURL 手动禁用指定URL，使其不再被选择
func (s *URLSelector) DisableURL(channelID int64, url string) {
	key := urlKey{channelID: channelID, url: url}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disabled[key] = true
}

// EnableURL 重新启用手动禁用的URL
func (s *URLSelector) EnableURL(channelID int64, url string) {
	key := urlKey{channelID: channelID, url: url}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.disabled, key)
}

// IsDisabled 检查URL是否被手动禁用
func (s *URLSelector) IsDisabled(channelID int64, url string) bool {
	key := urlKey{channelID: channelID, url: url}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.disabled[key]
}

// extractHostPort 从URL字符串提取 host:port，用于TCP连接测试。
// 如果URL中没有端口，根据scheme自动补全（https→443, http→80）。
func extractHostPort(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	host := parsed.Hostname()
	if host == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return ""
		}
	}
	return net.JoinHostPort(host, port)
}

// ProbeURLs 对无延迟数据的URL做并行TCP连接探测，记录连接耗时作为初始EWMA。
// 设计目标：多URL渠道首次被选中时，避免随机选到网络延迟高的URL。
//
// TCP连接时间反映纯网络延迟（DNS+TCP握手），与模型推理时间无关，
// 因此不会误杀推理模型的长首字节等待。
//
// 探测结果仅作为初始EWMA种子，后续真实请求的TTFB会纳入EWMA并逐步校准。
func (s *URLSelector) ProbeURLs(parentCtx context.Context, channelID int64, urls []string) {
	if len(urls) <= 1 {
		return
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	// 原子筛选+占位，避免并发请求重复探测同一URL。
	s.mu.Lock()
	now := time.Now()
	s.maybeCleanupLocked(now)
	unknowns := make([]string, 0, len(urls))
	for _, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		if _, known := s.latencies[key]; known {
			continue
		}
		if _, inFlight := s.probing[key]; inFlight {
			continue
		}
		s.probing[key] = now
		unknowns = append(unknowns, u)
	}
	s.mu.Unlock()

	if len(unknowns) == 0 {
		return // 所有URL已有数据
	}

	probeTimeout := s.probeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultURLSelectorProbeTimeout
	}

	// 并行TCP连接探测（默认总超时5s，可被调用方context更早打断）
	ctx, cancel := context.WithTimeout(parentCtx, probeTimeout)
	defer cancel()

	type probeResult struct {
		url     string
		latency time.Duration
		err     error
	}

	results := make(chan probeResult, len(unknowns))
	pending := make(map[string]struct{}, len(unknowns))
	clearProbing := func(probedURL string) {
		key := urlKey{channelID: channelID, url: probedURL}
		s.mu.Lock()
		delete(s.probing, key)
		s.mu.Unlock()
	}
	for _, u := range unknowns {
		pending[u] = struct{}{}
		go func(rawURL string) {
			host := extractHostPort(rawURL)
			if host == "" {
				results <- probeResult{url: rawURL, err: net.UnknownNetworkError("invalid URL")}
				return
			}

			start := time.Now()
			conn, err := s.probeDial(ctx, "tcp", host)
			if err != nil {
				results <- probeResult{url: rawURL, err: err}
				return
			}
			_ = conn.Close()
			results <- probeResult{url: rawURL, latency: time.Since(start)}
		}(u)
	}

	// 收集结果
	probed := 0
	failed := 0
	handleResult := func(r probeResult) {
		if _, ok := pending[r.url]; !ok {
			return
		}
		delete(pending, r.url)
		defer clearProbing(r.url)
		if r.err != nil {
			// 请求取消/服务关闭导致的探测中断不应污染URL冷却状态。
			if errors.Is(r.err, context.Canceled) {
				return
			}
			s.CooldownURL(channelID, r.url)
			failed++
			return
		}
		latency := r.latency
		if latency <= 0 {
			latency = time.Millisecond
		}
		key := urlKey{channelID: channelID, url: r.url}
		s.mu.Lock()
		now := time.Now()
		s.maybeCleanupLocked(now)
		s.upsertLatencyLocked(key, normalizeLatencyMS(latency), now)
		s.mu.Unlock()
		probed++
	}

	for range len(unknowns) {
		select {
		case r := <-results:
			handleResult(r)
		case <-ctx.Done():
			// 超时/取消：先吸收已完成结果，再把剩余未完成URL标记为冷却，避免继续以unknown优先被选中。
			ctxErr := ctx.Err()
			shouldCooldownPending := errors.Is(ctxErr, context.DeadlineExceeded)
			for {
				select {
				case r := <-results:
					handleResult(r)
				default:
					for pendingURL := range pending {
						clearProbing(pendingURL)
						if shouldCooldownPending {
							s.CooldownURL(channelID, pendingURL)
							failed++
						}
					}
					log.Printf("[PROBE] TCP探测提前结束(%v)，已完成=%d/%d", ctxErr, probed+failed, len(unknowns))
					if probed > 0 || failed > 0 {
						log.Printf("[PROBE] 渠道ID=%d TCP探测完成: 成功=%d 失败=%d", channelID, probed, failed)
					}
					return
				}
			}
		}
	}

	if probed > 0 || failed > 0 {
		log.Printf("[PROBE] 渠道ID=%d TCP探测完成: 成功=%d 失败=%d", channelID, probed, failed)
	}
}
