package app

import (
	"cmp"
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
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

/**
 * @brief 按令牌范围、成本、时段和模型及凭据冷却过滤候选，并应用真实选路排序
 * @param ctx 请求上下文；取消时停止加载 Key
 * @param channels 请求私有的候选切片
 * @param requestModel 已归一化的路由模型；空值或星号跳过模型行过滤
 * @param requestProtocol 客户端协议
 * @param allowAllCooledFallback 是否允许从全冷却候选中选择最早恢复渠道
 * @return 过滤排序后的候选；没有可用渠道时为空
 * @return 取消或 Credits 冷却状态加载失败时返回错误
 * @note 可原地压缩入参并推进同档渠道轮询；调用方仅使用返回切片
 */
func (s *Server) filterCooldownChannelsInternal(ctx context.Context, channels []*modelpkg.Config, requestModel, requestProtocol string, allowAllCooledFallback bool) ([]*modelpkg.Config, error) {
	state, err := s.loadChannelCooldownState(ctx, channels, requestModel, requestProtocol)
	if err != nil {
		return nil, err
	}
	channels = state.channels
	if len(channels) == 0 {
		return channels, nil
	}
	channelCooldowns, keyCooldowns := state.channelCooldowns, state.keyCooldowns
	rowReadiness, now := state.rowReadiness, state.now

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

/**
 * @brief 共享真实选路的候选硬约束与渠道、模型和凭据冷却快照
 */
type channelCooldownState struct {
	channels         []*modelpkg.Config          ///< 已通过令牌、成本、时段和模型授权的候选
	channelCooldowns map[int64]time.Time         ///< 渠道冷却截止时间
	keyCooldowns     map[int64]map[int]time.Time ///< 凭据冷却截止时间
	rowReadiness     map[int64]time.Time         ///< 授权模型行与凭据组合的最早恢复时间
	now              time.Time                   ///< 本次计算使用的同一时刻
	loaded           bool                        ///< 所有冷却及凭据状态均已加载，未使用选路的读取失败降级
}

/**
 * @brief 加载选路与最终限流响应共用的完整合格候选和冷却状态
 * @param ctx 请求上下文，含调用方渠道授权范围
 * @param channels 请求私有的原始候选切片
 * @param requestModel 路由模型；空值或星号跳过模型行过滤
 * @param requestProtocol 客户端协议
 * @return 硬约束过滤后的候选及其冷却状态；无合格候选时为空
 * @return 取消或 Credits 冷却状态加载失败时返回错误
 * @note 可原地压缩候选切片，不执行排序、负载均衡或全冷却兜底
 */
func (s *Server) loadChannelCooldownState(ctx context.Context, channels []*modelpkg.Config, requestModel, requestProtocol string) (channelCooldownState, error) {
	if len(channels) == 0 {
		return channelCooldownState{channels: channels, loaded: true}, nil
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
				return channelCooldownState{loaded: true}, nil
			}
		}
	}

	now := time.Now()
	loaded := true

	// === 成本限额过滤（在冷却过滤之前）===
	channels = s.filterCostLimitExceededChannels(channels)
	if len(channels) == 0 {
		log.Print("[INFO] 所有渠道均已达每日成本上限")
		return channelCooldownState{loaded: true}, nil
	}

	// 渠道可用时段是硬路由约束，必须在冷却兜底前过滤；不在时段内的渠道
	// 不能因为“所有渠道冷却”而被强行选中。
	channels = filterAvailableTimeChannels(channels, now)
	if len(channels) == 0 {
		log.Print("[INFO] 当前时间没有可用时段内的渠道")
		return channelCooldownState{loaded: true}, nil
	}

	// 批量查询冷却状态（优先走缓存层）
	paid := slices.ContainsFunc(channels, func(cfg *modelpkg.Config) bool { return cfg.AntigravityCredits })
	channelCooldowns, err := s.getAllChannelCooldowns(ctx)
	if err != nil {
		if paid {
			return channelCooldownState{}, err
		}
		// 降级策略：无法获取冷却数据时，跳过冷却过滤；仍保留后续健康度/负载均衡逻辑，避免直接返回未排序列表。
		log.Printf("[ERROR] 获取渠道冷却状态失败，跳过冷却过滤（降级模式）: %v", err)
		channelCooldowns = make(map[int64]time.Time)
		loaded = false
	}

	keyCooldowns, err := s.getAllKeyCooldowns(ctx)
	if err != nil {
		if paid {
			return channelCooldownState{}, err
		}
		// 降级策略：同上。
		log.Printf("[ERROR] 获取 Key 冷却状态失败，跳过冷却过滤（降级模式）: %v", err)
		keyCooldowns = make(map[int64]map[int]time.Time)
		loaded = false
	}

	modelCooldowns := make(map[int64]map[string]time.Time)
	if requestModel != "" && requestModel != "*" {
		modelCooldowns, err = s.getAllModelCooldowns(ctx)
		if err != nil {
			if paid {
				return channelCooldownState{}, err
			}
			log.Printf("[ERROR] 获取模型冷却状态失败，跳过模型冷却过滤（降级模式）: %v", err)
			modelCooldowns = make(map[int64]map[string]time.Time)
			loaded = false
		}
	}

	// Compute actual row/Key combinations once for both filtering and fallback.
	rowReadiness := make(map[int64]time.Time, len(channels))
	if requestModel != "" && requestModel != "*" {
		eligible := channels[:0]
		for _, cfg := range channels {
			var keys []*modelpkg.APIKey
			if !cfg.UsesOAuth() {
				keys, err = s.getAPIKeysSnapshot(ctx, cfg.ID)
				if err != nil {
					if ctx.Err() != nil {
						return channelCooldownState{}, ctx.Err()
					}
					log.Printf("[ERROR] 获取渠道 %d 的 Key 失败，跳过该渠道: %v", cfg.ID, err)
					loaded = false
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
			rows := s.enumerateModelRows(cfg, requestModel)
			for _, row := range rows {
				if row.entry.Disabled || (cfg.AntigravityCredits && !s.modelRowApplicable(cfg, row, now)) {
					continue
				}
				readyAt, authorized := s.modelRowWithKeysReadyAt(cfg, row, requestProtocol, modelCooldowns[cfg.ID], keys, now)
				if !authorized {
					continue
				}
				if readyAt.IsZero() {
					earliest = readyAt
					found = true
					break
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

	return channelCooldownState{
		channels: channels, channelCooldowns: channelCooldowns, keyCooldowns: keyCooldowns,
		rowReadiness: rowReadiness, now: now, loaded: loaded,
	}, nil
}

/**
 * @brief 为耗尽且完全冷却的候选池附加真实最早恢复时间，保留上游错误正文
 * @param ctx 当前请求上下文
 * @param reqCtx 已完成尝试的请求身份及渠道约束
 * @param result 最终上游失败结果；nil 或非 429 时不处理
 * @note 仅修改结果的 Retry-After；存在可用候选或无法读取候选状态时保留原响应
 */
func (s *Server) annotatePoolCooldownRetryAfter(ctx context.Context, reqCtx *proxyRequestContext, result *proxyResult) {
	if result == nil || result.status != http.StatusTooManyRequests || result.header == nil || result.zaiLocalPlanKind != "" {
		return
	}
	var channels []*modelpkg.Config
	var err error
	switch {
	case reqCtx.requestMethod == http.MethodGet && reqCtx.clientProtocol == protocol.Gemini:
		channels, err = s.getEnabledChannelsSnapshotByModel(ctx, "*")
	case reqCtx.clientProtocol == protocol.SystemOne:
		channels, err = s.systemOneCandidatePool(ctx, reqCtx.originalModel)
	case protocol.DetectRequestFamily(reqCtx.requestPath) == protocol.RequestFamilyAlphaSearch:
		channels, err = s.alphaSearchCandidatePool(ctx, reqCtx.originalModel)
	default:
		channels, err = s.fuzzyModelCandidates(ctx, reqCtx.originalModel)
	}
	if err != nil {
		return
	}
	ctx = withChannelRestrictionToken(ctx, reqCtx.tokenHash)
	// 普通与 Credits 候选可共享渠道 ID，但模型行的额度约束不同，分别计算避免覆盖就绪状态。
	pools := [][]*modelpkg.Config{channels}
	if reqCtx.requestMethod == http.MethodPost && protocol.DetectRequestFamily(reqCtx.requestPath) != protocol.RequestFamilyAlphaSearch {
		paid, poolErr := s.antigravityCreditsCandidatePool(ctx, reqCtx.originalModel, reqCtx.body)
		if poolErr != nil {
			return
		}
		for _, cfg := range paid {
			cfg.AntigravityCredits = true
			cfg.CooldownFallback = false
		}
		pools = append(pools, paid)
	}
	var earliest time.Time
	channelFilter := extractRequestedChannelFilter(&http.Request{Header: reqCtx.header})
	for _, pool := range pools {
		pool, _ = filterByRequestedChannel(pool, channelFilter)
		state, loadErr := s.loadChannelCooldownState(ctx, pool, reqCtx.originalModel, string(reqCtx.clientProtocol))
		if loadErr != nil || !state.loaded {
			return
		}
		if len(state.channels) == 0 {
			continue
		}
		if len(s.filterCooledChannels(state.channels, reqCtx.originalModel, state.channelCooldowns, state.keyCooldowns, state.rowReadiness, state.now)) > 0 {
			return
		}
		_, readyIn := s.pickBestChannelWhenAllCooled(state.channels, state.channelCooldowns, state.keyCooldowns, state.rowReadiness, state.now)
		deadline := state.now.Add(readyIn)
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	readyIn := time.Until(earliest)
	if readyIn > 0 {
		result.header.Set("Retry-After", strconv.FormatInt(int64((readyIn-1)/time.Second)+1, 10))
	}
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
	var healthSnapshot *channelHealthSnapshot
	if healthEnabled {
		healthCfg = s.healthCache.Config()
		healthSnapshot = s.healthCache.snapshot.Load()
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

	getEffPriority := func(ch *modelpkg.Config) float64 {
		if healthEnabled {
			return healthSnapshot.effectivePriority(ch, healthCfg)
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
	readyIn := max(readyAt.Sub(now), 0)

	return best, readyIn
}

/**
 * @brief 过滤渠道、模型行或全部 Key 仍处于冷却中的候选
 * @param channels 请求私有的候选切片
 * @param requestModel 路由模型；空值或星号跳过模型行适用性检查
 * @param channelCooldowns 渠道冷却截止时间
 * @param keyCooldowns 各渠道 Key 的冷却截止时间
 * @param rowReadiness 已授权模型行与 Key 组合的最早恢复时间；零值表示已就绪
 * @param now 本轮选路的当前时间
 * @return 原地压缩后的可用候选
 * @note 调用方仅使用返回切片；返回空时入参完整，可供全冷却兜底复用
 */
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

/**
 * @brief 计算模型行在所有可用协议及授权 Key 组合中的最早恢复时间
 * @param cfg 渠道配置
 * @param selected 已解析的模型行
 * @param requestProtocol 客户端协议
 * @param cooldowns 上游模型冷却截止时间
 * @param keys 渠道凭据；nil 表示仅检查模型冷却
 * @param now 本轮选路的当前时间
 * @return 最早恢复时间；零值表示已就绪
 * @return 是否存在获得模型授权的协议与凭据组合
 */
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
			// OAuth 渠道 (AntigravityCredits) 不走 Key 循环，CooldownFallback 需尾部
			// standardQuotaUntil 合并；此守卫只对普通 API Key 渠道生效。
			if readyAt.IsZero() && !cfg.AntigravityCredits && !cfg.CooldownFallback {
				return readyAt, true
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
	if cfg.IsSystemOne() {
		return []protocol.Protocol{protocol.SystemOne}
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
