package app

import (
	"sort"
	"time"

	modelpkg "ccLoad/internal/model"
)

const (
	// effPriorityPrecision 有效优先级分组精度（*10可区分0.1差异，如5.0 vs 5.1）
	// 设计考虑：优先级通常是整数（5, 10），成功率惩罚基于统计（精度有限），0.1精度已足够
	effPriorityPrecision = 10
)

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

	scored := make([]channelWithScore, len(channels))
	for i, ch := range channels {
		stats := s.healthCache.GetHealthStats(ch.ID)
		scored[i] = channelWithScore{
			config:      ch,
			effPriority: s.calculateEffectivePriority(ch, stats, cfg),
		}
	}

	// 按有效优先级排序（越大越优先，与原有逻辑一致）
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].effPriority > scored[j].effPriority
	})

	// 同有效优先级内按 KeyCount 平滑加权轮询（负载均衡）
	// 说明：healthCache 开启后仍需按 Key 数量分流，使用确定性轮询替代随机
	result := make([]*modelpkg.Config, len(scored))
	groupStart := 0
	for i := 1; i <= len(scored); i++ {
		if i == len(scored) || int(scored[i].effPriority*effPriorityPrecision) != int(scored[groupStart].effPriority*effPriorityPrecision) {
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
// 有效优先级 = 基础优先级 - 成功率惩罚 × 置信度（越大越优先）
// 置信度 = min(1.0, 样本量 / 置信阈值)，样本量越小惩罚越轻
func (s *Server) calculateEffectivePriority(
	ch *modelpkg.Config,
	stats modelpkg.ChannelHealthStats,
	cfg modelpkg.HealthScoreConfig,
) float64 {
	basePriority := float64(ch.Priority)

	successRate := stats.SuccessRate
	if successRate < 0 {
		successRate = 0
	} else if successRate > 1 {
		successRate = 1
	}
	failureRate := 1.0 - successRate

	// 置信度：样本量越小，惩罚打折越多
	confidence := 1.0
	if cfg.MinConfidentSample > 0 {
		confidence = min(1.0, float64(stats.SampleCount)/float64(cfg.MinConfidentSample))
	}

	// 惩罚 = 失败率 × 权重 × 置信度
	penalty := failureRate * cfg.SuccessRatePenaltyWeight * confidence

	return basePriority - penalty
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

	result := make([]*modelpkg.Config, n)
	copy(result, channels)

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
// 用于 healthCache 开启时的同有效优先级组内负载均衡
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

// calcEffectiveKeyCount 计算渠道的有效Key数量（排除冷却中的Key）
func calcEffectiveKeyCount(cfg *modelpkg.Config, keyCooldowns map[int64]map[int]time.Time, now time.Time) int {
	total := cfg.KeyCount
	if total <= 0 {
		return 1 // 最小为1
	}

	keyMap, ok := keyCooldowns[cfg.ID]
	if !ok || len(keyMap) == 0 {
		return total // 无冷却信息，使用全部Key数量
	}

	// 统计冷却中的Key数量
	cooledCount := 0
	for _, cooldownUntil := range keyMap {
		if cooldownUntil.After(now) {
			cooledCount++
		}
	}

	effective := total - cooledCount
	if effective <= 0 {
		return 1 // 最小为1
	}
	return effective
}
