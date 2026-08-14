package app

import (
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"ccLoad/internal/model"
)

const (
	defaultURLSelectorCleanupInterval = time.Hour
	defaultURLSelectorLatencyMaxAge   = 24 * time.Hour
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
	disabled     map[urlKey]bool // 手动禁用的URL（启动时从 channel_url_states 回填）
	alpha        float64         // EWMA权重因子
	cooldownBase time.Duration   // 基础冷却时间
	cooldownMax  time.Duration   // 最大冷却时间
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
		disabled:        make(map[urlKey]bool),
		alpha:           0.3,
		cooldownBase:    2 * time.Minute,
		cooldownMax:     30 * time.Minute,
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
	for key := range s.disabled {
		if key.channelID != channelID {
			continue
		}
		if _, ok := keep[key.url]; !ok {
			delete(s.disabled, key)
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
}

// RecordRequestResult 按日志聚合口径记录一次 URL 调用结果。
// 计数与延迟、冷却解耦：健康探测和状态变更都不应伪造 API 调用次数。
func (s *URLSelector) RecordRequestResult(channelID int64, url string, statusCode int) {
	if channelID <= 0 || url == "" || statusCode == StatusClientClosedRequest {
		return
	}

	key := urlKey{channelID: channelID, url: url}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.maybeCleanupLocked(time.Now())

	rc := s.requests[key]
	if rc == nil {
		rc = &urlRequestCount{}
		s.requests[key] = rc
	}
	if statusCode >= 200 && statusCode < 300 {
		rc.success++
		return
	}
	rc.failure++
}

// LoadPersistedStats 将启动时聚合出的 URL 日志快照灌入内存态。
// 仅回填成功/失败计数和延迟；冷却与禁用仍保持纯运行时语义。
func (s *URLSelector) LoadPersistedStats(stats []model.ChannelURLLogStat) {
	if len(stats) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.maybeCleanupLocked(now)

	for _, stat := range stats {
		if stat.ChannelID <= 0 || stat.BaseURL == "" {
			continue
		}

		key := urlKey{channelID: stat.ChannelID, url: stat.BaseURL}

		if stat.LatencyMs >= 0 {
			lastSeen := stat.LastSeen
			if lastSeen.IsZero() {
				lastSeen = now
			}
			s.latencies[key] = &ewmaValue{value: stat.LatencyMs, lastSeen: lastSeen}
		}

		if stat.Requests > 0 || stat.Failures > 0 {
			s.requests[key] = &urlRequestCount{
				success: stat.Requests,
				failure: stat.Failures,
			}
		}
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
}

// ClearCooldowns 清除指定渠道的全部 URL 冷却，不影响延迟统计和手动禁用状态。
func (s *URLSelector) ClearCooldowns(channelID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range s.cooldowns {
		if key.channelID == channelID {
			delete(s.cooldowns, key)
		}
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
	// 排序优先级：非冷却 > 冷却，同组内未探索 > 已知。
	slices.SortStableFunc(candidates, func(ci, cj candidate) int {
		if ci.cooled != cj.cooled {
			if !ci.cooled {
				return -1 // 非冷却优先
			}
			return 1
		}
		rank := func(c candidate) int {
			if c.latency >= 0 {
				return 1
			}
			return 0
		}
		iRank, jRank := rank(ci), rank(cj)
		if iRank != jRank {
			return iRank - jRank
		}
		if iRank != 1 {
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

// LoadDisabled 用持久化快照替换手动禁用URL集合。
func (s *URLSelector) LoadDisabled(disabled map[int64][]string) {
	next := make(map[urlKey]bool)
	for channelID, urls := range disabled {
		for _, u := range urls {
			next[urlKey{channelID: channelID, url: u}] = true
		}
	}
	s.mu.Lock()
	s.disabled = next
	s.mu.Unlock()
}

// IsDisabled 检查URL是否被手动禁用
func (s *URLSelector) IsDisabled(channelID int64, url string) bool {
	key := urlKey{channelID: channelID, url: url}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.disabled[key]
}
