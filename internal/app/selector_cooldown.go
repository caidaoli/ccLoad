package app

import (
	"cmp"
	"context"
	"log"
	"slices"
	"time"

	modelpkg "ccLoad/internal/model"
)

// filterCooldownChannels 过滤冷却中的渠道
//
// [IMPORTANT] 冷却状态优先级：**最高优先级**，必须在健康度排序前执行
// 即使健康度缓存显示渠道可用，冷却状态具有最高优先级。
//
// 执行顺序保证：
// 1. 先执行冷却过滤（本函数）
// 2. 再执行健康度排序（sortChannelsByHealth）
// 3. 确保不会选中已冷却的渠道，避免雪崩效应
//
// 行为说明：
// - 冷却语义：渠道级冷却、或“所有Key均在冷却”的渠道会被过滤
// - 健康度排序：仅对“已通过冷却过滤”的渠道进行排序/负载均衡
func (s *Server) filterCooldownChannels(ctx context.Context, channels []*modelpkg.Config) ([]*modelpkg.Config, error) {
	if len(channels) == 0 {
		return channels, nil
	}

	now := time.Now()

	// === 成本限额过滤（在冷却过滤之前）===
	channels = s.filterCostLimitExceededChannels(channels)
	if len(channels) == 0 {
		log.Print("[INFO] All channels exceeded daily cost limit")
		return nil, nil
	}

	// 批量查询冷却状态（优先走缓存层）
	channelCooldowns, err := s.getAllChannelCooldowns(ctx)
	if err != nil {
		// 降级策略：无法获取冷却数据时，跳过冷却过滤；仍保留后续健康度/负载均衡逻辑，避免直接返回未排序列表。
		log.Printf("[ERROR] Failed to get channel cooldowns, skipping cooldown filtering (degraded mode): %v", err)
		channelCooldowns = make(map[int64]time.Time)
	}

	keyCooldowns, err := s.getAllKeyCooldowns(ctx)
	if err != nil {
		// 降级策略：同上。
		log.Printf("[ERROR] Failed to get key cooldowns, skipping cooldown filtering (degraded mode): %v", err)
		keyCooldowns = make(map[int64]map[int]time.Time)
	}

	// 先执行冷却过滤，保证冷却语义不被绕开（正确性优先）
	filtered := s.filterCooledChannels(channels, channelCooldowns, keyCooldowns, now)
	if len(filtered) == 0 {
		// 全冷却兜底：开关控制（false=禁用，true=启用）
		// 启用时：直接返回"最早恢复"的渠道，让上层继续走正常流程（不要再搞阈值这类花活）。
		fallbackEnabled := true
		if s.configService != nil {
			fallbackEnabled = s.configService.GetBool("cooldown_fallback_enabled", true)
		}
		if !fallbackEnabled {
			log.Printf("[INFO] All channels cooled, fallback disabled (cooldown_fallback_enabled=false)")
			return nil, nil
		}

		best, readyIn := s.pickBestChannelWhenAllCooled(channels, channelCooldowns, keyCooldowns, now)
		if best != nil {
			log.Printf("[INFO] All channels cooled, fallback to channel %d (ready in %.1fs)", best.ID, readyIn.Seconds())
			return []*modelpkg.Config{best}, nil
		}
		return nil, nil
	}

	// 启用健康度排序：对"已通过冷却过滤"的渠道按健康度排序
	if s.healthCache != nil && s.healthCache.Config().Enabled {
		return s.sortChannelsByHealth(filtered, keyCooldowns, now), nil
	}

	// healthCache 关闭时：按优先级分组，使用平滑加权轮询
	return s.balanceSamePriorityChannels(filtered, keyCooldowns, now), nil
}

// pickBestChannelWhenAllCooled 全冷却时选择最佳渠道。
// 返回最佳渠道和距离恢复的剩余时间。
// 选择规则：最早恢复 > 有效优先级高 > 基础优先级高
func (s *Server) pickBestChannelWhenAllCooled(
	channels []*modelpkg.Config,
	channelCooldowns map[int64]time.Time,
	keyCooldowns map[int64]map[int]time.Time,
	now time.Time,
) (*modelpkg.Config, time.Duration) {
	if len(channels) == 0 {
		return nil, 0
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
				var earliest time.Time
				hasAvailableKey := false
				for _, until := range keyMap {
					if !until.After(now) {
						hasAvailableKey = true
						break
					}
					if earliest.IsZero() || until.Before(earliest) {
						earliest = until
					}
				}
				// 当“所有Key都在冷却”时：渠道真正可用时间 = max(渠道冷却, 最早Key解禁)
				if !hasAvailableKey && !earliest.IsZero() && earliest.After(readyAt) {
					readyAt = earliest
				}
			}
		}
		return readyAt
	}

	// 计算有效优先级
	getEffPriority := func(ch *modelpkg.Config) float64 {
		if healthEnabled {
			return s.calculateEffectivePriority(ch, s.healthCache.GetHealthStats(ch.ID), healthCfg)
		}
		return float64(ch.Priority)
	}

	// 过滤nil并找最优
	valid := slices.DeleteFunc(slices.Clone(channels), func(ch *modelpkg.Config) bool { return ch == nil })
	if len(valid) == 0 {
		return nil, 0
	}

	best := slices.MinFunc(valid, func(a, b *modelpkg.Config) int {
		// 1. 最早恢复优先（时间小的排前面）
		if getReadyAt(a) != getReadyAt(b) {
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

	readyAt := getReadyAt(best)
	readyIn := readyAt.Sub(now)
	if readyIn < 0 {
		readyIn = 0
	}

	return best, readyIn
}

// filterCooledChannels 过滤冷却中的渠道
// 渠道级冷却或所有Key都在冷却时，该渠道被过滤
func (s *Server) filterCooledChannels(
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

// filterCostLimitExceededChannels 过滤超过每日成本限额的渠道
func (s *Server) filterCostLimitExceededChannels(channels []*modelpkg.Config) []*modelpkg.Config {
	if s.costCache == nil {
		return channels
	}

	costs := s.costCache.GetAll()
	filtered := make([]*modelpkg.Config, 0, len(channels))
	for _, ch := range channels {
		// DailyCostLimit <= 0 表示无限制
		if ch.DailyCostLimit <= 0 {
			filtered = append(filtered, ch)
			continue
		}

		usedCost := costs[ch.ID]
		if usedCost < ch.DailyCostLimit {
			filtered = append(filtered, ch)
			log.Printf("[DEBUG] Channel %d (%s) cost check passed: $%.4f/$%.2f",
				ch.ID, ch.Name, usedCost, ch.DailyCostLimit)
		} else {
			log.Printf("[INFO] Channel %d (%s) exceeded daily cost limit: $%.4f/$%.2f",
				ch.ID, ch.Name, usedCost, ch.DailyCostLimit)
		}
	}
	return filtered
}
