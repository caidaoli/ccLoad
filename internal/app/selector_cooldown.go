package app

import (
	"cmp"
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"time"

	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"
)

type channelRestrictionTokenContextKey struct{}

var errNoAvailableModelRow = errors.New("no available model row for channel")

type channelRestrictionState struct {
	tokenHash string
	denied    bool
}

func withChannelRestrictionToken(ctx context.Context, tokenHash string) context.Context {
	if ctx == nil || tokenHash == "" {
		return ctx
	}
	return context.WithValue(ctx, channelRestrictionTokenContextKey{}, &channelRestrictionState{tokenHash: tokenHash})
}

func channelRestrictionTokenFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	state, _ := ctx.Value(channelRestrictionTokenContextKey{}).(*channelRestrictionState)
	if state == nil {
		return ""
	}
	return state.tokenHash
}

func channelRestrictionDeniedFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	state, _ := ctx.Value(channelRestrictionTokenContextKey{}).(*channelRestrictionState)
	return state != nil && state.denied
}

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
// - 冷却语义：渠道级冷却、当前模型冷却，或“所有Key均在冷却”的渠道会被过滤
// - 健康度排序：仅对“已通过冷却过滤”的渠道进行排序/负载均衡
func (s *Server) filterCooldownChannels(ctx context.Context, channels []*modelpkg.Config, requestModel, requestProtocol string) ([]*modelpkg.Config, error) {
	return s.filterCooldownChannelsInternal(ctx, channels, requestModel, requestProtocol, true)
}

// filterCooldownChannelsStrict 与 filterCooldownChannels 类似，但不会触发“全冷却兜底”选择。
// 用于需要在“候选为空”时继续做下一步回退（例如模型模糊匹配）的场景。
func (s *Server) filterCooldownChannelsStrict(ctx context.Context, channels []*modelpkg.Config, requestModel, requestProtocol string) ([]*modelpkg.Config, error) {
	return s.filterCooldownChannelsInternal(ctx, channels, requestModel, requestProtocol, false)
}

func (s *Server) filterCooldownChannelsInternal(ctx context.Context, channels []*modelpkg.Config, requestModel, requestProtocol string, allowAllCooledFallback bool) ([]*modelpkg.Config, error) {
	if len(channels) == 0 {
		return channels, nil
	}
	if tokenHash := channelRestrictionTokenFromContext(ctx); tokenHash != "" && s.authService != nil {
		filtered, restricted := s.authService.FilterAllowedChannels(tokenHash, channels)
		if restricted {
			// 空集合必须保持为空，不能让越权渠道重新参与冷却兜底。
			channels = filtered
			if state, _ := ctx.Value(channelRestrictionTokenContextKey{}).(*channelRestrictionState); state != nil {
				state.denied = len(channels) == 0
			}
			if len(channels) == 0 {
				return nil, nil
			}
		}
	}

	now := time.Now()

	// === 成本限额过滤（在冷却过滤之前）===
	channels = s.filterCostLimitExceededChannels(channels)
	if len(channels) == 0 {
		log.Print("[INFO] 所有渠道均已达每日成本上限")
		return nil, nil
	}

	// 渠道可用时段是硬路由约束，必须在冷却兜底前过滤；不在时段内的渠道
	// 不能因为“所有渠道冷却”而被强行选中。
	channels = filterAvailableTimeChannels(channels, now)
	if len(channels) == 0 {
		log.Print("[INFO] 当前时间没有可用时段内的渠道")
		return nil, nil
	}

	// 批量查询冷却状态（优先走缓存层）
	paid := slices.ContainsFunc(channels, func(cfg *modelpkg.Config) bool { return cfg.AntigravityCredits })
	channelCooldowns, err := s.getAllChannelCooldowns(ctx)
	if err != nil {
		if paid {
			return nil, err
		}
		// 降级策略：无法获取冷却数据时，跳过冷却过滤；仍保留后续健康度/负载均衡逻辑，避免直接返回未排序列表。
		log.Printf("[ERROR] 获取渠道冷却状态失败，跳过冷却过滤（降级模式）: %v", err)
		channelCooldowns = make(map[int64]time.Time)
	}

	keyCooldowns, err := s.getAllKeyCooldowns(ctx)
	if err != nil {
		if paid {
			return nil, err
		}
		// 降级策略：同上。
		log.Printf("[ERROR] 获取 Key 冷却状态失败，跳过冷却过滤（降级模式）: %v", err)
		keyCooldowns = make(map[int64]map[int]time.Time)
	}

	modelCooldowns := make(map[int64]map[string]time.Time)
	if requestModel != "" && requestModel != "*" {
		modelCooldowns, err = s.getAllModelCooldowns(ctx)
		if err != nil {
			if paid {
				return nil, err
			}
			log.Printf("[ERROR] 获取模型冷却状态失败，跳过模型冷却过滤（降级模式）: %v", err)
			modelCooldowns = make(map[int64]map[string]time.Time)
		}
	}

	// Compute actual row/Key combinations once for both filtering and fallback.
	rowReadiness := make(map[int64]time.Time, len(channels))
	if requestModel != "" && requestModel != "*" {
		eligible := channels[:0]
		for _, cfg := range channels {
			var keys []*modelpkg.APIKey
			if !cfg.UsesOAuth() {
				keys, err = s.getAPIKeys(ctx, cfg.ID)
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					log.Printf("[ERROR] 获取渠道 %d 的 Key 失败，跳过该渠道: %v", cfg.ID, err)
					continue
				}
				// Channels without credentials retain model-only candidate
				// selection; the forwarding boundary reports the missing Key.
				if len(keys) == 0 {
					keys = nil
				}
			}
			found := false
			var earliest time.Time
			for _, row := range s.enumerateModelRows(cfg, requestModel) {
				if row.entry.Disabled || (cfg.AntigravityCredits && !s.modelRowApplicable(cfg, row, now)) {
					continue
				}
				readyAt, authorized := s.modelRowWithKeysReadyAt(cfg, row, requestProtocol, modelCooldowns[cfg.ID], keys, now)
				if !authorized {
					continue
				}
				if !found || readyAt.Before(earliest) {
					earliest = readyAt
				}
				found = true
			}
			if found {
				rowReadiness[cfg.ID] = earliest
				eligible = append(eligible, cfg)
			}
		}
		channels = eligible
	}

	// 先执行冷却过滤，保证冷却语义不被绕开（正确性优先）
	filtered := s.filterCooledChannels(channels, requestModel, channelCooldowns, keyCooldowns, rowReadiness, now)
	if len(filtered) == 0 {
		if !allowAllCooledFallback {
			return nil, nil
		}
		// 全冷却兜底：开关控制（false=禁用，true=启用）
		// 启用时：直接返回"最早恢复"的渠道，让上层继续走正常流程（不要再搞阈值这类花活）。
		fallbackEnabled := true
		if s.configService != nil {
			fallbackEnabled = s.configService.GetBool("cooldown_fallback_enabled", true)
		}
		if !fallbackEnabled {
			log.Printf("[INFO] 所有渠道冷却中，兜底已禁用（cooldown_fallback_enabled=false）")
			return nil, nil
		}

		best, readyIn := s.pickBestChannelWhenAllCooled(channels, channelCooldowns, keyCooldowns, rowReadiness, now)
		if best != nil {
			log.Printf("[INFO] 所有渠道冷却中，兜底使用渠道 %d（%.1fs 后就绪）", best.ID, readyIn.Seconds())
			return []*modelpkg.Config{cooldownFallbackCandidate(best)}, nil
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

func filterAvailableTimeChannels(channels []*modelpkg.Config, now time.Time) []*modelpkg.Config {
	filtered := make([]*modelpkg.Config, 0, len(channels))
	for _, cfg := range channels {
		if cfg != nil && cfg.IsAvailableAt(now) {
			filtered = append(filtered, cfg)
		}
	}
	return filtered
}

func cooldownFallbackCandidate(cfg *modelpkg.Config) *modelpkg.Config {
	clone := cfg.Clone()
	if clone == nil {
		return nil
	}
	clone.CooldownFallback = true
	return clone
}

// pickBestChannelWhenAllCooled 全冷却时选择最佳渠道。
// 返回最佳渠道和距离恢复的剩余时间。
// 选择规则：最早恢复 > 有效优先级高 > 基础优先级高
func (s *Server) pickBestChannelWhenAllCooled(
	channels []*modelpkg.Config,
	channelCooldowns map[int64]time.Time,
	keyCooldowns map[int64]map[int]time.Time,
	rowReadiness map[int64]time.Time,
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
		if until, ok := rowReadiness[ch.ID]; ok {
			if until.After(readyAt) {
				readyAt = until
			}
			return readyAt
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

	// 过滤nil并找最优
	valid := slices.DeleteFunc(slices.Clone(channels), func(ch *modelpkg.Config) bool { return ch == nil })
	if len(valid) == 0 {
		return nil, 0
	}

	// 计算有效优先级（含候选集首字中位）
	medianTTFB := 0.0
	if healthEnabled {
		samples := make([]float64, 0, len(valid))
		for _, ch := range valid {
			st := s.healthCache.GetHealthStats(ch.ID)
			if st.FirstByteSampleCount > 0 && st.AvgFirstByteSeconds > 0 {
				samples = append(samples, st.AvgFirstByteSeconds)
			}
		}
		medianTTFB = medianFloat64(samples)
	}
	getEffPriority := func(ch *modelpkg.Config) float64 {
		if healthEnabled {
			return s.calculateEffectivePriority(ch, s.healthCache.GetHealthStats(ch.ID), healthCfg, medianTTFB)
		}
		return float64(ch.Priority)
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
//
// 原地压缩 channels：写入下标恒不大于读取下标，所以不会覆盖尚未读到的元素。
// 调用方必须传入请求私有的 slice，且只能使用返回值；唯一例外是"返回空"——
// 此时一次写入都没发生，入参仍然完整，全冷却兜底才能安全复用它。
func (s *Server) filterCooledChannels(
	channels []*modelpkg.Config,
	requestModel string,
	channelCooldowns map[int64]time.Time,
	keyCooldowns map[int64]map[int]time.Time,
	rowReadiness map[int64]time.Time,
	now time.Time,
) []*modelpkg.Config {
	filtered := channels[:0]
	for _, cfg := range channels {
		if requestModel != "*" && requestModel != "" && len(s.applicableModelRows(cfg, requestModel, now)) == 0 {
			continue
		}
		// 1. 检查渠道级冷却
		if cooldownUntil, exists := channelCooldowns[cfg.ID]; exists {
			if cooldownUntil.After(now) {
				continue
			}
		}

		// 2. 检查当前请求是否存在模型、协议与 Key 均可用的组合
		if until, exists := rowReadiness[cfg.ID]; exists {
			if !until.After(now) {
				filtered = append(filtered, cfg)
			}
			continue
		}

		// 3. 检查是否所有Key都在冷却
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

func (s *Server) possibleActualModels(cfg *modelpkg.Config, requestModel, requestProtocol string) []string {
	client := protocol.Protocol(util.NormalizeProtocol(requestProtocol))
	protocols := possibleUpstreamProtocols(cfg, client)

	seen := make(map[string]struct{}, len(protocols))
	models := make([]string, 0, len(protocols))
	for _, selected := range s.applicableModelRows(cfg, requestModel, time.Now()) {
		for _, upstreamProtocol := range protocols {
			actualModel := s.resolveFinalUpstreamModel(cfg, selected, string(upstreamProtocol))
			if actualModel == "" {
				continue
			}
			if _, ok := seen[actualModel]; ok {
				continue
			}
			seen[actualModel] = struct{}{}
			models = append(models, actualModel)
		}
	}
	return models
}

func (s *Server) applicableModelRows(cfg *modelpkg.Config, requested string, now time.Time) []modelRoutingSelection {
	rows := s.enumerateModelRows(cfg, requested)
	result := rows[:0]
	for _, selected := range rows {
		if !s.modelRowApplicable(cfg, selected, now) {
			continue
		}
		result = append(result, selected)
	}
	return result
}

func (s *Server) modelRowApplicable(cfg *modelpkg.Config, selected modelRoutingSelection, now time.Time) bool {
	if selected.entry.Disabled {
		return false
	}
	geminiModel := s.resolveFinalUpstreamModel(cfg, selected, string(protocol.Gemini))
	quotaUntil := s.antigravityCredentials.standardQuotaUntil(cfg, geminiModel)
	if cfg.AntigravityCredits {
		return antigravityClaudeModel(geminiModel) && quotaUntil.After(now)
	}
	return cfg.CooldownFallback || !quotaUntil.After(now)
}

// modelRowWithKeysReadyAt evaluates complete protocol/Key combinations. A
// zero time means ready now; false means no authorized combination exists.
func (s *Server) modelRowWithKeysReadyAt(cfg *modelpkg.Config, selected modelRoutingSelection, requestProtocol string, cooldowns map[string]time.Time, keys []*modelpkg.APIKey, now time.Time) (time.Time, bool) {
	protocols := possibleUpstreamProtocols(cfg, protocol.Protocol(util.NormalizeProtocol(requestProtocol)))
	var earliest time.Time
	found := false
	for _, upstream := range protocols {
		actual := s.resolveFinalUpstreamModel(cfg, selected, string(upstream))
		modelUntil := cooldowns[actual]
		if !modelUntil.After(now) {
			modelUntil = time.Time{}
		}
		if keys == nil {
			if !found || modelUntil.Before(earliest) {
				earliest = modelUntil
			}
			found = true
			continue
		}
		for _, key := range keys {
			if key == nil || key.Disabled || !key.AllowsModel(selected.logicalModel) || !key.AllowsUpstreamModel(actual) {
				continue
			}
			readyAt := modelUntil
			if key.IsCoolingDown(now) {
				if until := time.Unix(key.CooldownUntil, 0); until.After(readyAt) {
					readyAt = until
				}
			}
			if !found || readyAt.Before(earliest) {
				earliest = readyAt
			}
			found = true
		}
	}
	if found && !cfg.AntigravityCredits && !cfg.CooldownFallback {
		actual := s.resolveFinalUpstreamModel(cfg, selected, string(protocol.Gemini))
		if until := s.antigravityCredentials.standardQuotaUntil(cfg, actual); until.After(now) && until.After(earliest) {
			earliest = until
		}
	}
	return earliest, found
}

func (s *Server) keyAllowsModelRow(cfg *modelpkg.Config, key *modelpkg.APIKey, selected modelRoutingSelection, requestProtocol string) bool {
	if key == nil || key.Disabled {
		return false
	}
	return s.keyModelScopeAllowsRow(cfg, key, selected, possibleUpstreamProtocols(cfg, protocol.Protocol(util.NormalizeProtocol(requestProtocol))))
}

func (s *Server) keyModelScopeAllowsRow(cfg *modelpkg.Config, key *modelpkg.APIKey, selected modelRoutingSelection, protocols []protocol.Protocol) bool {
	if key == nil || !key.AllowsModel(selected.logicalModel) {
		return false
	}
	for _, upstream := range protocols {
		if key.AllowsUpstreamModel(s.resolveFinalUpstreamModel(cfg, selected, string(upstream))) {
			return true
		}
	}
	return false
}

func (s *Server) selectChannelModelRow(ctx context.Context, cfg *modelpkg.Config, requested, requestProtocol string, session *responsesExecutionSession, apiKeys []*modelpkg.APIKey) (modelRoutingSelection, error) {
	if requested == "" || requested == "*" {
		return modelRoutingSelection{logicalModel: requested, entry: modelpkg.ModelEntry{Model: requested}, wildcard: true}, nil
	}
	rows := s.configuredModelRows(cfg, requested)
	if len(rows) == 0 {
		return modelRoutingSelection{}, errNoAvailableModelRow
	}
	allCooldowns, err := s.getAllModelCooldowns(ctx)
	if err != nil {
		if cfg.AntigravityCredits {
			return modelRoutingSelection{}, err
		}
		log.Printf("[ERROR] 获取模型冷却状态失败，跳过选行冷却过滤: %v", err)
		allCooldowns = nil
	}
	cooldowns := allCooldowns[cfg.ID]
	now := time.Now()
	available := make([]bool, len(rows))
	hasAvailable := false
	hasAuthorized := false
	bestFallback := -1
	var earliest time.Time
	for i, selected := range rows {
		readyAt, authorized := s.modelRowWithKeysReadyAt(cfg, selected, requestProtocol, cooldowns, apiKeys, now)
		if !authorized {
			continue
		}
		hasAuthorized = true
		if !s.modelRowApplicable(cfg, selected, now) {
			continue
		}
		available[i] = readyAt.IsZero()
		hasAvailable = hasAvailable || available[i]
		if cfg.CooldownFallback {
			if bestFallback < 0 || readyAt.Before(earliest) {
				bestFallback, earliest = i, readyAt
			}
			continue
		}
	}
	if apiKeys != nil && !hasAuthorized {
		return modelRoutingSelection{}, ErrNoAPIKeyForModel
	}
	if session != nil {
		if bound, ok := session.boundModelRow(cfg.ID, rows[0].logicalModel); ok {
			for i, selected := range rows {
				if available[i] && selected.entry.Model == bound.entry.Model &&
					strings.EqualFold(selected.entry.RedirectModel, bound.entry.RedirectModel) {
					return selected, nil
				}
			}
			session.forgetModelRow(cfg.ID, rows[0].logicalModel)
		}
	}
	remember := func(selected modelRoutingSelection) (modelRoutingSelection, error) {
		if session != nil {
			session.rememberModelRow(cfg.ID, selected)
		}
		return selected, nil
	}
	if cfg.CooldownFallback && !hasAvailable && bestFallback >= 0 {
		return remember(rows[bestFallback])
	}
	if len(rows) == 1 && available[0] {
		return remember(rows[0])
	}
	if s.keySelector != nil {
		if index, ok := s.keySelector.SelectModelRow(cfg.ID, rows[0].logicalModel, available); ok {
			return remember(rows[index])
		}
	} else {
		for i, ok := range available {
			if ok {
				return remember(rows[i])
			}
		}
	}
	return modelRoutingSelection{}, errNoAvailableModelRow
}

func possibleUpstreamProtocols(cfg *modelpkg.Config, client protocol.Protocol) []protocol.Protocol {
	if cfg == nil {
		return nil
	}
	seen := make(map[protocol.Protocol]struct{})
	result := make([]protocol.Protocol, 0, len(localFallbackProtocolOrder))
	appendProtocol := func(candidate protocol.Protocol) {
		if !configCanUseUpstreamProtocol(cfg, candidate) {
			return
		}
		if _, exists := seen[candidate]; exists {
			return
		}
		seen[candidate] = struct{}{}
		result = append(result, candidate)
	}

	switch cfg.GetProtocolTransformMode() {
	case modelpkg.ProtocolTransformModeUpstream:
		appendProtocol(client)
	case modelpkg.ProtocolTransformModeLocal:
		for _, candidate := range localUpstreamProtocolOrder(cfg.URLs) {
			appendProtocol(candidate)
		}
	default:
		appendProtocol(client)
		for _, candidate := range automaticFallbackProtocolOrder {
			appendProtocol(candidate)
		}
	}
	return result
}

// filterCostLimitExceededChannels 过滤超过每日成本限额的渠道
func (s *Server) filterCostLimitExceededChannels(channels []*modelpkg.Config) []*modelpkg.Config {
	if s.costCache == nil {
		return channels
	}
	hasLimit := false
	for _, ch := range channels {
		if ch.DailyCostLimit > 0 {
			hasLimit = true
			break
		}
	}
	if !hasLimit {
		return channels
	}

	// 这里必须新建 slice：调用方在本函数返回后仍可能按原长度复用入参
	// （selector.go 的模糊匹配回退会对同一 allCandidates 过滤两次），
	// 原地压缩会让尾部元素重复。hasLimit 预检已保证这条分支很少走到。
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
		}
	}
	return filtered
}
