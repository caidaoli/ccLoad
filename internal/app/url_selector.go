package app

import (
	"math"
	"sort"
	"sync"
	"time"
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

// URLSelector 基于EWMA延迟和冷却状态选择最优URL
type URLSelector struct {
	mu           sync.RWMutex
	latencies    map[urlKey]*ewmaValue
	cooldowns    map[urlKey]urlCooldownState
	alpha        float64       // EWMA权重因子
	cooldownBase time.Duration // 基础冷却时间
	cooldownMax  time.Duration // 最大冷却时间
}

// NewURLSelector 创建URL选择器
func NewURLSelector() *URLSelector {
	return &URLSelector{
		latencies:    make(map[urlKey]*ewmaValue),
		cooldowns:    make(map[urlKey]urlCooldownState),
		alpha:        0.3,
		cooldownBase: 2 * time.Minute,
		cooldownMax:  30 * time.Minute,
	}
}

// SelectURL 从候选URL中选择最优的
// 返回选中的URL和在原列表中的索引
func (s *URLSelector) SelectURL(channelID int64, urls []string) (string, int) {
	if len(urls) <= 1 {
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

	candidates := make([]candidate, len(urls))
	for i, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		c := candidate{url: u, idx: i, latency: -1}

		if e, ok := s.latencies[key]; ok {
			c.latency = e.value
		}
		if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
			c.cooled = true
		}
		candidates[i] = c
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

	// 排序：有延迟数据的按EWMA升序排前面，无数据的保持原始顺序排后面
	sort.SliceStable(available, func(i, j int) bool {
		li, lj := available[i].latency, available[j].latency
		if li >= 0 && lj >= 0 {
			return li < lj // 都有数据：按延迟排序
		}
		if li >= 0 {
			return true // i有数据j没有，i优先
		}
		return false // j有数据或都没有，保持原序
	})

	return available[0].url, available[0].idx
}

// RecordLatency 记录URL的首字节时间，更新EWMA
func (s *URLSelector) RecordLatency(channelID int64, url string, ttfb time.Duration) {
	key := urlKey{channelID: channelID, url: url}
	ms := float64(ttfb.Milliseconds())

	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.latencies[key]; ok {
		e.value = s.alpha*ms + (1-s.alpha)*e.value
		e.lastSeen = time.Now()
	} else {
		s.latencies[key] = &ewmaValue{value: ms, lastSeen: time.Now()}
	}

	// 成功请求：重置冷却连续失败计数
	if cd, ok := s.cooldowns[key]; ok {
		cd.consecutiveFails = 0
		s.cooldowns[key] = cd
	}
}

// CooldownURL 对URL施加指数退避冷却
func (s *URLSelector) CooldownURL(channelID int64, url string) {
	key := urlKey{channelID: channelID, url: url}

	s.mu.Lock()
	defer s.mu.Unlock()

	cd := s.cooldowns[key]
	cd.consecutiveFails++

	// 指数退避: base * 2^(fails-1), 上限 max
	multiplier := math.Pow(2, float64(cd.consecutiveFails-1))
	duration := time.Duration(float64(s.cooldownBase) * multiplier)
	if duration > s.cooldownMax {
		duration = s.cooldownMax
	}

	cd.until = time.Now().Add(duration)
	s.cooldowns[key] = cd
}

// IsCooledDown 检查URL是否在冷却中
func (s *URLSelector) IsCooledDown(channelID int64, url string) bool {
	key := urlKey{channelID: channelID, url: url}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cd, ok := s.cooldowns[key]
	return ok && time.Now().Before(cd.until)
}

// LastSeen 返回URL最后一次被使用的时间（用于探测器判断是否需要主动探测）
func (s *URLSelector) LastSeen(channelID int64, url string) time.Time {
	key := urlKey{channelID: channelID, url: url}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.latencies[key]; ok {
		return e.lastSeen
	}
	return time.Time{} // 零值表示从未被使用
}

// sortedURL 排序后的URL条目
type sortedURL struct {
	url string
	idx int
}

// SortURLs 返回按EWMA延迟排序的全部URL列表（非冷却URL优先，用于故障切换遍历）
func (s *URLSelector) SortURLs(channelID int64, urls []string) []sortedURL {
	if len(urls) <= 1 {
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

	candidates := make([]candidate, len(urls))
	for i, u := range urls {
		key := urlKey{channelID: channelID, url: u}
		c := candidate{url: u, idx: i, latency: -1}
		if e, ok := s.latencies[key]; ok {
			c.latency = e.value
		}
		if cd, ok := s.cooldowns[key]; ok && now.Before(cd.until) {
			c.cooled = true
		}
		candidates[i] = c
	}

	// 排序：非冷却URL优先，同组内按EWMA升序，无数据的保持原序
	sort.SliceStable(candidates, func(i, j int) bool {
		ci, cj := candidates[i], candidates[j]
		if ci.cooled != cj.cooled {
			return !ci.cooled // 非冷却优先
		}
		li, lj := ci.latency, cj.latency
		if li >= 0 && lj >= 0 {
			return li < lj
		}
		if li >= 0 {
			return true
		}
		return false
	})

	result := make([]sortedURL, len(candidates))
	for i, c := range candidates {
		result[i] = sortedURL{url: c.url, idx: c.idx}
	}
	return result
}
