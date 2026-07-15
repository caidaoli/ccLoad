package app

import (
	"math"
	"sort"
	"time"

	modelpkg "ccLoad/internal/model"
)

const (
	// effPriorityPrecision 有效优先级分组精度（*10可区分0.1差异，如5.0 vs 5.1）
	// 设计考虑：优先级通常是整数（5, 10），成功率惩罚基于统计（精度有限），0.1精度已足够
	effPriorityPrecision = 10
)

func effPriorityBucket(p float64) int64 {
	scaled := p * float64(effPriorityPrecision)
	// 浮点误差修正：避免 5.1*10 得到 50.999999... 被截断到 50
	if scaled >= 0 {
		scaled += 1e-9
	} else {
		scaled -= 1e-9
	}
	return int64(math.Trunc(scaled))
}

// channelWithScore 带有效优先级的渠道
type channelWithScore struct {
	config      *modelpkg.Config
	effPriority float64
}

// sortChannelsByHealth 按健康度排序渠道（仅排序，不改变冷却过滤语义）
// keyCooldowns: Key级冷却状态，用于计算有效Key数量（排除冷却中的Key）
// now: 当前时间，用于判断Key是否处于冷却中
func (s *Server) sortChannelsByHealth(
	channels []*modelpkg.Config,
	keyCooldowns map[int64]map[int]time.Time,
	now time.Time,
) []*modelpkg.Config {
	if len(channels) == 0 {
		return channels
	}

	cfg := s.healthCache.Config()

	// Preload stats and compute candidate median TTFB for relative penalty.
	statsByID := make(map[int64]modelpkg.ChannelHealthStats, len(channels))
	ttfbSamples := make([]float64, 0, len(channels))
	for _, ch := range channels {
		st := s.healthCache.GetHealthStats(ch.ID)
		statsByID[ch.ID] = st
		if st.FirstByteSampleCount > 0 && st.AvgFirstByteSeconds > 0 {
			ttfbSamples = append(ttfbSamples, st.AvgFirstByteSeconds)
		}
	}
	medianTTFB := medianFloat64(ttfbSamples)

	scored := make([]channelWithScore, len(channels))
	for i, ch := range channels {
		stats := statsByID[ch.ID]
		scored[i] = channelWithScore{
			config:      ch,
			effPriority: s.calculateEffectivePriority(ch, stats, cfg, medianTTFB),
		}
	}

	// 按有效优先级排序（越大越优先，与原有逻辑一致）
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].effPriority > scored[j].effPriority
	})

	// 同有效优先级内按 KeyCount 平滑加权轮询（负载均衡）
	// 说明：healthCache 开启后仍需按 Key 数量分流。
	// 这里仅把“本轮选中的渠道”移动到组首，确保首选渠道按权重分布；其余顺序保持稳定，便于失败回退时可预测。
	result := make([]*modelpkg.Config, len(scored))
	groupStart := 0
	for i := 1; i <= len(scored); i++ {
		if i == len(scored) || effPriorityBucket(scored[i].effPriority) != effPriorityBucket(scored[groupStart].effPriority) {
			if i-groupStart > 1 {
				s.balanceScoredChannelsInPlace(scored[groupStart:i], keyCooldowns, now)
			}
			groupStart = i
		}
	}

	for i, item := range scored {
		result[i] = item.config
	}
	return result
}

// calculateEffectivePriority 计算渠道的有效优先级
// P_eff = Priority - Penalty_fail - Penalty_ttfb
// 越大越优先。medianTTFB 为当前候选集有效首字中位数（秒），<=0 表示不启用相对首字惩罚。
func (s *Server) calculateEffectivePriority(
	ch *modelpkg.Config,
	stats modelpkg.ChannelHealthStats,
	cfg modelpkg.HealthScoreConfig,
	medianTTFB float64,
) float64 {
	basePriority := float64(ch.Priority)

	successRate := stats.SuccessRate
	if successRate < 0 {
		successRate = 0
	} else if successRate > 1 {
		successRate = 1
	}
	failureRate := 1.0 - successRate

	// 失败惩罚置信度：样本量越小，惩罚打折越多
	failConfidence := 1.0
	if cfg.MinConfidentSample > 0 {
		failConfidence = min(1.0, float64(stats.SampleCount)/float64(cfg.MinConfidentSample))
	}
	penaltyFail := failureRate * float64(cfg.SuccessRatePenaltyWeight) * failConfidence

	penaltyTTFB := 0.0
	if cfg.EnableTTFBScore && medianTTFB > 0 && stats.FirstByteSampleCount > 0 && stats.AvgFirstByteSeconds > 0 && cfg.TTFBPenaltyWeight > 0 {
		sRatio := stats.AvgFirstByteSeconds / medianTTFB
		slow := sRatio - 1.0
		if slow < 0 {
			slow = 0
		}
		maxSlow := cfg.TTFBMaxSlowRatio
		if maxSlow < 0 {
			maxSlow = 0
		}
		if slow > maxSlow {
			slow = maxSlow
		}
		ttfbConfidence := 1.0
		if cfg.TTFBMinConfidentSample > 0 {
			ttfbConfidence = min(1.0, float64(stats.FirstByteSampleCount)/float64(cfg.TTFBMinConfidentSample))
		}
		penaltyTTFB = slow * cfg.TTFBPenaltyWeight * ttfbConfidence
	}

	return basePriority - penaltyFail - penaltyTTFB
}

// medianFloat64 returns the median of values. Empty input returns 0.
// Requires at least 2 values for relative scoring callers; this helper itself
// still returns a median for a single value.
func medianFloat64(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	// Need >=2 peers for relative median comparison in caller.
	if n < 2 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	mid := n / 2
	if n%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

// balanceSamePriorityChannels 按优先级分组，组内使用平滑加权轮询
// 用于 healthCache 关闭时的场景，确保确定性分流
func (s *Server) balanceSamePriorityChannels(
	channels []*modelpkg.Config,
	keyCooldowns map[int64]map[int]time.Time,
	now time.Time,
) []*modelpkg.Config {
	n := len(channels)
	if n <= 1 {
		return channels
	}

	// channelBalancer 在 Init() 中无条件初始化，nil 表示初始化错误
	if s.channelBalancer == nil {
		panic("channelBalancer is nil: server not properly initialized")
	}

	// 按优先级降序排序（优先级大的排前面），确保相同优先级渠道连续
	result := make([]*modelpkg.Config, n)
	copy(result, channels)
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].Priority > result[j].Priority
	})

	// 按优先级分组，组内使用平滑加权轮询
	groupStart := 0
	for i := 1; i <= n; i++ {
		if i == n || result[i].Priority != result[groupStart].Priority {
			if i-groupStart > 1 {
				group := result[groupStart:i]
				balanced := s.channelBalancer.SelectWithCooldown(group, keyCooldowns, now)
				copy(result[groupStart:i], balanced)
			}
			groupStart = i
		}
	}

	return result
}

// balanceScoredChannelsInPlace 对带分数的渠道列表进行平滑加权轮询
// 用于 healthCache 开启时的同有效优先级组内负载均衡（仅决定组内“首选”渠道）
func (s *Server) balanceScoredChannelsInPlace(
	items []channelWithScore,
	keyCooldowns map[int64]map[int]time.Time,
	now time.Time,
) {
	n := len(items)
	if n <= 1 {
		return
	}

	// channelBalancer 在 Init() 中无条件初始化，nil 表示初始化错误
	if s.channelBalancer == nil {
		panic("channelBalancer is nil: server not properly initialized")
	}

	// 提取 Config 列表用于轮询选择
	configs := make([]*modelpkg.Config, n)
	for i, item := range items {
		configs[i] = item.config
	}

	// 使用平滑加权轮询获取排序后的结果
	balanced := s.channelBalancer.SelectWithCooldown(configs, keyCooldowns, now)

	// 按轮询结果重排 items（O(n) 交换）
	// balanced[0] 是选中的渠道，需要把它移到 items[0]
	selectedID := balanced[0].ID
	for i, item := range items {
		if item.config.ID == selectedID && i != 0 {
			items[0], items[i] = items[i], items[0]
			break
		}
	}
}
