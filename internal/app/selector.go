package app

import (
	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/util"

	"cmp"
	"context"
	"log"
	"math/rand/v2"
	"slices"
	"sort"
	"time"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
// 性能优化：使用缓存层，内存查询 < 2ms vs 数据库查询 50ms+
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	normalizedType := util.NormalizeChannelType(channelType)
	matcher := func(cfg *modelpkg.Config) bool {
		return cfg.GetChannelType() == normalizedType
	}
	channels, err := s.getEnabledChannelsWithFallback(ctx,
		func() ([]*modelpkg.Config, error) { return s.GetEnabledChannelsByType(ctx, channelType) },
		matcher,
	)
	if err != nil {
		return nil, err
	}
	return s.filterCooldownChannels(ctx, shuffleSamePriorityChannels(channels))
}

// selectCandidates 选择支持指定模型的候选渠道
// 性能优化：使用缓存层，消除JSON查询和聚合操作的性能杀手
func (s *Server) selectCandidates(ctx context.Context, model string) ([]*modelpkg.Config, error) {
	channels, err := s.getEnabledChannelsWithFallback(ctx,
		func() ([]*modelpkg.Config, error) { return s.GetEnabledChannelsByModel(ctx, model) },
		func(cfg *modelpkg.Config) bool { return s.configSupportsModel(cfg, model) },
	)
	if err != nil {
		return nil, err
	}
	return s.filterCooldownChannels(ctx, shuffleSamePriorityChannels(channels))
}

// selectCandidatesByModelAndType 根据模型和渠道类型筛选候选渠道
// 遵循SRP：数据库负责返回满足模型的渠道，本函数仅负责类型过滤
func (s *Server) selectCandidatesByModelAndType(ctx context.Context, model string, channelType string) ([]*modelpkg.Config, error) {
	channels, err := s.getEnabledChannelsWithFallback(ctx,
		func() ([]*modelpkg.Config, error) { return s.GetEnabledChannelsByModel(ctx, model) },
		func(cfg *modelpkg.Config) bool { return s.configSupportsModel(cfg, model) },
	)
	if err != nil {
		return nil, err
	}

	if channelType == "" {
		return s.filterCooldownChannels(ctx, shuffleSamePriorityChannels(channels))
	}

	normalizedType := util.NormalizeChannelType(channelType)
	filtered := make([]*modelpkg.Config, 0, len(channels))
	for _, cfg := range channels {
		if cfg.GetChannelType() == normalizedType {
			filtered = append(filtered, cfg)
		}
	}

	return s.filterCooldownChannels(ctx, shuffleSamePriorityChannels(filtered))
}

// getEnabledChannelsWithFallback 统一的降级查询逻辑（DRY）
// 快路径：优先走缓存/索引；若结果为空，降级到全量扫描（用于"全冷却兜底"场景）
func (s *Server) getEnabledChannelsWithFallback(
	ctx context.Context,
	fastPath func() ([]*modelpkg.Config, error),
	matcher func(*modelpkg.Config) bool,
) ([]*modelpkg.Config, error) {
	candidates, err := fastPath()
	if err != nil {
		return nil, err
	}
	if len(candidates) > 0 {
		return candidates, nil
	}

	// 降级：全量查询，手动过滤（用于"全冷却兜底"场景）
	all, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}

	enabled := make([]*modelpkg.Config, 0, len(all))
	for _, cfg := range all {
		if cfg == nil || !cfg.Enabled {
			continue
		}
		if matcher(cfg) {
			enabled = append(enabled, cfg)
		}
	}
	return enabled, nil
}

// configSupportsModel 检查渠道是否支持指定模型
func (s *Server) configSupportsModel(cfg *modelpkg.Config, model string) bool {
	if model == "*" {
		return true
	}
	if cfg.ModelRedirects != nil {
		if _, ok := cfg.ModelRedirects[model]; ok {
			return true
		}
	}
	for _, m := range cfg.Models {
		if m == model {
			return true
		}
	}
	return false
}

// filterCooldownChannels 过滤或降权冷却中的渠道
// 当启用健康度排序时：冷却渠道降权而非过滤，按有效优先级排序
// 当禁用健康度排序时：保持原有行为，完全过滤冷却渠道
func (s *Server) filterCooldownChannels(ctx context.Context, channels []*modelpkg.Config) ([]*modelpkg.Config, error) {
	if len(channels) == 0 {
		return channels, nil
	}

	now := time.Now()

	// 批量查询冷却状态（使用缓存层，性能优化）
	channelCooldowns, err := s.getAllChannelCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] Failed to get channel cooldowns (degraded mode): %v", err)
		return channels, nil
	}

	keyCooldowns, err := s.getAllKeyCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] Failed to get key cooldowns (degraded mode): %v", err)
		return channels, nil
	}

	// 先执行冷却过滤，保证冷却语义不被绕开（正确性优先）
	filtered := s.filterCooldownChannelsLegacy(channels, channelCooldowns, keyCooldowns, now)
	if len(filtered) == 0 {
		// 全冷却兜底：仍需返回一个最可能最快恢复的渠道，避免直接无路可走
		// 原则：尽量少破坏冷却机制，只在“没有任何可用渠道”时选择 1 个候选
		return s.pickBestChannelWhenAllCooled(channels, channelCooldowns, keyCooldowns, now), nil
	}

	// 启用健康度排序：对“已通过冷却过滤”的渠道按健康度排序
	if s.healthCache != nil && s.healthCache.Config().Enabled {
		return s.sortChannelsByHealth(filtered), nil
	}

	return filtered, nil
}

// pickBestChannelWhenAllCooled 全冷却兜底选择。
// 选择规则：最早恢复 > 有效优先级高 > 基础优先级高
func (s *Server) pickBestChannelWhenAllCooled(
	channels []*modelpkg.Config,
	channelCooldowns map[int64]time.Time,
	keyCooldowns map[int64]map[int]time.Time,
	now time.Time,
) []*modelpkg.Config {
	if len(channels) == 0 {
		return channels
	}

	healthEnabled := s.healthCache != nil && s.healthCache.Config().Enabled
	healthCfg := modelpkg.HealthScoreConfig{}
	if healthEnabled {
		healthCfg = s.healthCache.Config()
	}

	// 计算渠道的恢复时间
	getReadyAt := func(ch *modelpkg.Config) time.Time {
		readyAt := now
		if until, ok := channelCooldowns[ch.ID]; ok && until.After(readyAt) {
			readyAt = until
		}
		// Key全冷却时，取最早解禁时间
		if ch.KeyCount > 0 {
			if keyMap := keyCooldowns[ch.ID]; keyMap != nil && len(keyMap) >= ch.KeyCount {
				for _, until := range keyMap {
					if until.After(now) && (readyAt.Equal(now) || until.Before(readyAt)) {
						readyAt = until
					}
				}
			}
		}
		return readyAt
	}

	// 计算有效优先级
	getEffPriority := func(ch *modelpkg.Config) float64 {
		if healthEnabled {
			return s.calculateEffectivePriority(ch, s.healthCache.GetSuccessRate(ch.ID), healthCfg)
		}
		return float64(ch.Priority)
	}

	// 过滤nil并找最优
	valid := slices.DeleteFunc(slices.Clone(channels), func(ch *modelpkg.Config) bool { return ch == nil })
	if len(valid) == 0 {
		return nil
	}

	best := slices.MinFunc(valid, func(a, b *modelpkg.Config) int {
		// 1. 最早恢复优先（时间小的排前面）
		if c := a.ID - b.ID; getReadyAt(a) != getReadyAt(b) {
			_ = c // 避免unused
			if getReadyAt(a).Before(getReadyAt(b)) {
				return -1
			}
			return 1
		}
		// 2. 有效优先级高优先（值大的排前面，所以反过来比较）
		if c := cmp.Compare(getEffPriority(b), getEffPriority(a)); c != 0 {
			return c
		}
		// 3. 基础优先级高优先
		return cmp.Compare(b.Priority, a.Priority)
	})

	return []*modelpkg.Config{best}
}

// filterCooldownChannelsLegacy 原有过滤逻辑（健康度排序禁用时使用）
func (s *Server) filterCooldownChannelsLegacy(
	channels []*modelpkg.Config,
	channelCooldowns map[int64]time.Time,
	keyCooldowns map[int64]map[int]time.Time,
	now time.Time,
) []*modelpkg.Config {
	filtered := make([]*modelpkg.Config, 0, len(channels))
	for _, cfg := range channels {
		// 1. 检查渠道级冷却
		if cooldownUntil, exists := channelCooldowns[cfg.ID]; exists {
			if cooldownUntil.After(now) {
				continue
			}
		}

		// 2. 检查是否所有Key都在冷却
		keyMap, hasCooldownKeys := keyCooldowns[cfg.ID]
		if hasCooldownKeys && cfg.KeyCount > 0 {
			if len(keyMap) >= cfg.KeyCount {
				hasAvailableKey := false
				for _, cooldownUntil := range keyMap {
					if !cooldownUntil.After(now) {
						hasAvailableKey = true
						break
					}
				}
				if !hasAvailableKey {
					continue
				}
			}
		}

		filtered = append(filtered, cfg)
	}
	return filtered
}

// channelWithScore 带有效优先级的渠道
type channelWithScore struct {
	config      *modelpkg.Config
	effPriority float64
}

// sortChannelsByHealth 按健康度排序渠道（仅排序，不改变冷却过滤语义）
func (s *Server) sortChannelsByHealth(
	channels []*modelpkg.Config,
) []*modelpkg.Config {
	if len(channels) == 0 {
		return channels
	}

	cfg := s.healthCache.Config()

	scored := make([]channelWithScore, len(channels))
	for i, ch := range channels {
		successRate := s.healthCache.GetSuccessRate(ch.ID)
		scored[i] = channelWithScore{
			config:      ch,
			effPriority: s.calculateEffectivePriority(ch, successRate, cfg),
		}
	}

	// 按有效优先级排序（越大越优先，与原有逻辑一致）
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].effPriority > scored[j].effPriority
	})

	// 同有效优先级内随机打散（负载均衡）
	result := make([]*modelpkg.Config, len(scored))
	groupStart := 0
	for i := 1; i <= len(scored); i++ {
		// 检测优先级边界（使用整数部分比较，避免浮点精度问题）
		if i == len(scored) || int(scored[i].effPriority) != int(scored[groupStart].effPriority) {
			if i-groupStart > 1 {
				rand.Shuffle(i-groupStart, func(a, b int) {
					scored[groupStart+a], scored[groupStart+b] = scored[groupStart+b], scored[groupStart+a]
				})
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
// 有效优先级 = 基础优先级 - 成功率惩罚（越大越优先）
func (s *Server) calculateEffectivePriority(
	ch *modelpkg.Config,
	successRate float64,
	cfg modelpkg.HealthScoreConfig,
) float64 {
	basePriority := float64(ch.Priority)

	// 成功率惩罚（减少优先级）
	if successRate < 0 {
		successRate = 0
	} else if successRate > 1 {
		successRate = 1
	}
	failureRate := 1.0 - successRate
	successRatePenalty := failureRate * cfg.SuccessRatePenaltyWeight

	return basePriority - successRatePenalty
}

// shuffleSamePriorityChannels 随机打乱相同优先级的渠道，实现负载均衡
// 设计原则：KISS、无状态、保持优先级排序
func shuffleSamePriorityChannels(channels []*modelpkg.Config) []*modelpkg.Config {
	n := len(channels)
	if n <= 1 {
		return channels
	}

	result := make([]*modelpkg.Config, n)
	copy(result, channels)

	// 单次遍历：识别优先级边界并就地打乱
	groupStart := 0
	for i := 1; i <= n; i++ {
		// 检测优先级边界（包括末尾）
		if i == n || result[i].Priority != result[groupStart].Priority {
			// 打乱 [groupStart, i) 区间
			if i-groupStart > 1 {
				rand.Shuffle(i-groupStart, func(a, b int) {
					result[groupStart+a], result[groupStart+b] = result[groupStart+b], result[groupStart+a]
				})
			}
			groupStart = i
		}
	}

	return result
}
