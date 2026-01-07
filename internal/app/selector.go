package app

import (
	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/util"

	"cmp"
	"context"
	"log"
	"slices"
	"sort"
	"strconv"
	"strings"
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
	return s.filterCooldownChannels(ctx, channels)
}

// selectCandidatesByModelAndType 根据模型和渠道类型筛选候选渠道
// 遵循SRP：数据库负责返回满足模型的渠道，本函数仅负责类型过滤
func (s *Server) selectCandidatesByModelAndType(ctx context.Context, model string, channelType string) ([]*modelpkg.Config, error) {
	// 预计算 normalizedType（闭包捕获）
	normalizedType := util.NormalizeChannelType(channelType)

	// 类型过滤辅助函数
	filterByType := func(channels []*modelpkg.Config) []*modelpkg.Config {
		if channelType == "" {
			return channels
		}
		filtered := make([]*modelpkg.Config, 0, len(channels))
		for _, cfg := range channels {
			if cfg.GetChannelType() == normalizedType {
				filtered = append(filtered, cfg)
			}
		}
		return filtered
	}

	fastPath := func() ([]*modelpkg.Config, error) {
		channels, err := s.GetEnabledChannelsByModel(ctx, model)
		if err != nil {
			return nil, err
		}
		// [FIX] 在判断是否回退前，先应用 channelType 过滤
		// 否则精确匹配到一个 openai 渠道会阻止回退到 anthropic 渠道
		filtered := filterByType(channels)
		if len(filtered) > 0 || !s.modelLookupStripDateSuffix || model == "*" {
			return filtered, nil
		}
		stripped, ok := stripTrailingYYYYMMDD(model)
		if !ok || stripped == model {
			return filtered, nil
		}
		channels, err = s.GetEnabledChannelsByModel(ctx, stripped)
		if err != nil {
			return nil, err
		}
		return filterByType(channels), nil
	}

	// matcher 也需要考虑 channelType
	matcher := func(cfg *modelpkg.Config) bool {
		if channelType != "" && cfg.GetChannelType() != normalizedType {
			return false
		}
		return s.configSupportsModelWithDateFallback(cfg, model)
	}

	channels, err := s.getEnabledChannelsWithFallback(ctx, fastPath, matcher)
	if err != nil {
		return nil, err
	}

	return s.filterCooldownChannels(ctx, channels)
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
	return cfg.SupportsModel(model)
}

func (s *Server) configSupportsModelWithDateFallback(cfg *modelpkg.Config, model string) bool {
	if s.configSupportsModel(cfg, model) {
		return true
	}
	if model == "*" {
		return false
	}

	// 日期后缀回退
	if s.modelLookupStripDateSuffix {
		// 请求带日期：claude-3-5-sonnet-20241022 -> claude-3-5-sonnet
		if stripped, ok := stripTrailingYYYYMMDD(model); ok && stripped != model {
			if cfg.SupportsModel(stripped) {
				return true
			}
		}

		// 请求无日期：claude-sonnet-4-5 -> claude-sonnet-4-5-20250929
		for _, entry := range cfg.ModelEntries {
			if entry.Model == "" {
				continue
			}
			if stripped, ok := stripTrailingYYYYMMDD(entry.Model); ok && stripped == model {
				return true
			}
		}
	}

	// 模糊匹配：sonnet -> claude-sonnet-4-5-20250929
	if s.modelFuzzyMatch {
		if _, ok := cfg.FuzzyMatchModel(model); ok {
			return true
		}
	}

	return false
}

func stripTrailingYYYYMMDD(model string) (string, bool) {
	dash := strings.LastIndexByte(model, '-')
	if dash < 0 {
		return model, false
	}
	suffix := model[dash+1:]
	if len(suffix) != 8 {
		return model, false
	}
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return model, false
		}
	}
	year, _ := strconv.Atoi(suffix[:4])
	month, _ := strconv.Atoi(suffix[4:6])
	day, _ := strconv.Atoi(suffix[6:8])
	if year < 2000 || year > 2100 {
		return model, false
	}
	if month < 1 || month > 12 {
		return model, false
	}
	lastDay := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day < 1 || day > lastDay {
		return model, false
	}
	return model[:dash], true
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
	// 精度：*10 取整，可区分 0.1 差异（如 5.0 vs 5.1）
	// 设计考虑：优先级通常是整数（5, 10），成功率惩罚基于统计（精度有限），0.1 精度已足够
	result := make([]*modelpkg.Config, len(scored))
	groupStart := 0
	for i := 1; i <= len(scored); i++ {
		if i == len(scored) || int(scored[i].effPriority*10) != int(scored[groupStart].effPriority*10) {
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
