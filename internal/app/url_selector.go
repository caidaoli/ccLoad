package app

import (
	"math"
	"math/rand/v2"
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
	epsilon      float64       // 探索概率 (epsilon-greedy)
	cooldownBase time.Duration // 基础冷却时间
	cooldownMax  time.Duration // 最大冷却时间
}

// NewURLSelector 创建URL选择器
func NewURLSelector() *URLSelector {
	return &URLSelector{
		latencies:    make(map[urlKey]*ewmaValue),
		cooldowns:    make(map[urlKey]urlCooldownState),
		alpha:        0.3,
		epsilon:      0.1, // 10%概率随机探索
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

	// 先随机打乱（同组内随机），再稳定排序
	rand.Shuffle(len(available), func(i, j int) {
		available[i], available[j] = available[j], available[i]
	})
	// 探索优先：未探索URL排在已知URL前面，已知URL按EWMA升序
	sort.SliceStable(available, func(i, j int) bool {
		iKnown, jKnown := available[i].latency >= 0, available[j].latency >= 0
		if iKnown != jKnown {
			return !iKnown // 未探索的优先
		}
		if !iKnown {
			return false // 都未探索：保持随机顺序
		}
		return available[i].latency < available[j].latency
	})

	// Epsilon-greedy：所有URL都已探索且有多个可选时，以epsilon概率随机选
	if len(available) > 1 && available[0].latency >= 0 && rand.Float64() < s.epsilon {
		pick := rand.IntN(len(available)-1) + 1 // 排除[0]，从剩余中随机选
		return available[pick].url, available[pick].idx
	}

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

	// 先随机打乱，再稳定排序
	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	// 排序优先级：非冷却 > 冷却，同组内未探索 > 已知，已知按EWMA升序
	sort.SliceStable(candidates, func(i, j int) bool {
		ci, cj := candidates[i], candidates[j]
		if ci.cooled != cj.cooled {
			return !ci.cooled // 非冷却优先
		}
		iKnown, jKnown := ci.latency >= 0, cj.latency >= 0
		if iKnown != jKnown {
			return !iKnown // 未探索的优先
		}
		if !iKnown {
			return false // 都未探索：保持随机顺序
		}
		return ci.latency < cj.latency
	})

	result := make([]sortedURL, len(candidates))
	for i, c := range candidates {
		result[i] = sortedURL{url: c.url, idx: c.idx}
	}
	return result
}
