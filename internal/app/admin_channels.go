package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"ccLoad/internal/xaiauth"
	"ccLoad/internal/zaiauth"
	"ccLoad/internal/zedauth"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// ==================== 渠道CRUD管理 ====================
// 从admin.go拆分渠道CRUD,遵循SRP原则

// HandleChannels 处理渠道列表请求
func (s *Server) HandleChannels(c *gin.Context) {
	switch c.Request.Method {
	case "GET":
		s.handleListChannels(c)
	case "POST":
		s.handleCreateChannel(c)
	default:
		RespondErrorMsg(c, 405, "method not allowed")
	}
}

func channelKeyStrategy(apiKeys []*model.APIKey) string {
	if len(apiKeys) > 0 && apiKeys[0].KeyStrategy != "" {
		return apiKeys[0].KeyStrategy
	}
	return model.KeyStrategySequential
}

// 获取渠道列表
// 使用批量查询优化N+1问题
// filterConfigs 用谓词筛选 *model.Config 切片，消除 handleListChannels 中重复的
// "make/for/append/cfgs=filtered" 五行片段。空容量预分配避免短切片再次扩容。
func filterConfigs(cfgs []*model.Config, keep func(*model.Config) bool) []*model.Config {
	out := make([]*model.Config, 0, len(cfgs))
	for _, cfg := range cfgs {
		if keep(cfg) {
			out = append(out, cfg)
		}
	}
	return out
}

func configHasURLProtocol(cfg *model.Config, configuredProtocol string) bool {
	configuredProtocol = strings.ToLower(strings.TrimSpace(configuredProtocol))
	if cfg == nil || configuredProtocol == "" {
		return false
	}
	for _, entry := range cfg.URLs {
		if configuredProtocol == "auto" {
			if entry.UsesAutomaticProtocolDetection() {
				return true
			}
			continue
		}
		if !entry.UsesAutomaticProtocolDetection() && entry.SupportsProtocol(configuredProtocol) {
			return true
		}
	}
	return false
}

type channelCooldownSnapshot struct {
	channels map[int64]time.Time
	keys     map[int64]map[int]time.Time
	models   map[int64]map[string]time.Time
}

func (s *Server) loadChannelCooldownSnapshot(ctx context.Context) channelCooldownSnapshot {
	snapshot := channelCooldownSnapshot{}
	var err error

	snapshot.channels, err = s.getAllChannelCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] 批量查询渠道冷却状态失败: %v", err)
		snapshot.channels = make(map[int64]time.Time)
	}
	snapshot.keys, err = s.getAllKeyCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] 批量查询Key冷却状态失败: %v", err)
		snapshot.keys = make(map[int64]map[int]time.Time)
	}
	snapshot.models, err = s.getAllModelCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] 批量查询模型冷却状态失败: %v", err)
		snapshot.models = make(map[int64]map[string]time.Time)
	}
	return snapshot
}

func (snapshot channelCooldownSnapshot) hasActiveCooldown(channelID int64, now time.Time) bool {
	if until, ok := snapshot.channels[channelID]; ok && until.After(now) {
		return true
	}
	for _, until := range snapshot.keys[channelID] {
		if until.After(now) {
			return true
		}
	}
	for _, until := range snapshot.models[channelID] {
		if until.After(now) {
			return true
		}
	}
	return false
}

func (s *Server) handleListChannels(c *gin.Context) {
	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	now := time.Now()

	// 三类冷却必须使用同一份快照，否则筛选结果和列表状态会互相矛盾。
	cooldowns := s.loadChannelCooldownSnapshot(c.Request.Context())

	// 应用所有列表过滤（type / channel_name|search / status / model|model_like）
	// 注意：筛选下拉的全集走独立接口 /admin/channels/filter-options，
	// 这里只负责按所有筛选条件返回当前页，避免列表数据与下拉选项耦合。
	cfgs = applyChannelListFilters(cfgs, c, cooldowns, now)

	hasPagination := c.Query("limit") != "" || c.Query("offset") != ""

	// 批量查询所有API Keys（一次查询替代 N 次）
	allAPIKeys, err := s.store.GetAllAPIKeys(c.Request.Context())
	if err != nil {
		log.Printf("[WARN] 批量查询API Keys失败: %v", err)
		allAPIKeys = make(map[int64][]*model.APIKey) // 降级：使用空map
	}

	// 健康度模式检查
	healthEnabled := s.healthCache != nil && s.healthCache.Config().Enabled

	// 排序：健康度开启按 effective_priority 降序；关闭按 priority DESC, name ASC，
	// 与前端 filterChannels 的排序键对齐，保证分页跨页顺序稳定。
	priorityMap, successRateMap := s.sortChannelsByEffectivePriority(cfgs, healthEnabled)

	totalCount := len(cfgs)

	if hasPagination {
		cfgs = paginateChannels(cfgs, c)
	}

	ectx := &channelEnrichmentContext{
		now:                  now,
		healthEnabled:        healthEnabled,
		priorityMap:          priorityMap,
		successRateMap:       successRateMap,
		channelCooldownsMap:  cooldowns.channels,
		keyCooldownsMap:      cooldowns.keys,
		modelCooldownsMap:    cooldowns.models,
		protocolProbeRetries: s.protocolCapabilities.unsupportedRetrySummaries(now),
		apiKeysMap:           allAPIKeys,
	}
	out := make([]ChannelWithCooldown, 0, len(cfgs))
	for _, cfg := range cfgs {
		channel := ectx.enrichChannel(cfg)
		channel.ManagementAccount = s.managementAccountView(cfg)
		out = append(out, channel)
	}

	// 填充空的重定向模型为请求模型（方便前端编辑时显示）
	for i := range out {
		for j := range out[i].ModelEntries {
			if out[i].Config.ModelEntries[j].RedirectModel == "" {
				out[i].Config.ModelEntries[j].RedirectModel = out[i].Config.ModelEntries[j].Model
			}
		}
	}

	if hasPagination {
		RespondPaginated(c, http.StatusOK, out, totalCount)
		return
	}
	RespondJSON(c, http.StatusOK, out)
}

// applyChannelListFilters 串联应用所有列表过滤条件：
//   - protocol: URL 显式声明的协议，或 auto（存在未声明协议的 URL）
//   - auth_type: api_key / codex_oauth（认证机制，不复用历史 channel_type）
//   - channel_name | search: 名称精确/模糊（互斥，channel_name 优先）
//   - status: enabled / disabled / cooldown（cooldown 包含渠道、Key、模型任一有效冷却）
//   - model | model_like: 模型精确/模糊（互斥，model 优先）
//
// 空字符串或 "all" 视为不过滤。
func applyChannelListFilters(cfgs []*model.Config, c *gin.Context, cooldowns channelCooldownSnapshot, now time.Time) []*model.Config {
	if configuredProtocol := strings.TrimSpace(c.Query("protocol")); configuredProtocol != "" && configuredProtocol != "all" {
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			return configHasURLProtocol(cfg, configuredProtocol)
		})
	}

	if authType := strings.TrimSpace(c.Query("auth_type")); authType != "" && authType != "all" {
		normalizedAuthType := model.NormalizeAuthType(authType)
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			return normalizedAuthType != "" && cfg.GetAuthType() == normalizedAuthType
		})
	}

	// channel_name | search（互斥）
	if name := strings.TrimSpace(c.Query("channel_name")); name != "" {
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			return strings.TrimSpace(cfg.Name) == name
		})
	} else if search := strings.TrimSpace(c.Query("search")); search != "" {
		searchLower := strings.ToLower(search)
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			return strings.Contains(strings.ToLower(strings.TrimSpace(cfg.Name)), searchLower)
		})
	}

	// status
	if status := strings.TrimSpace(c.Query("status")); status != "" && status != "all" {
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			switch status {
			case "enabled":
				return cfg.Enabled
			case "disabled":
				return !cfg.Enabled
			case "cooldown":
				return cooldowns.hasActiveCooldown(cfg.ID, now)
			}
			return false
		})
	}

	// model | model_like（互斥）
	if modelName := strings.TrimSpace(c.Query("model")); modelName != "" && modelName != "all" {
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			for _, entry := range cfg.ModelEntries {
				if entry.Model == modelName {
					return true
				}
			}
			return false
		})
	} else if modelLike := strings.TrimSpace(c.Query("model_like")); modelLike != "" && modelLike != "all" {
		modelLikeLower := strings.ToLower(modelLike)
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			for _, entry := range cfg.ModelEntries {
				if strings.Contains(strings.ToLower(strings.TrimSpace(entry.Model)), modelLikeLower) {
					return true
				}
			}
			return false
		})
	}

	return cfgs
}

// sortChannelsByEffectivePriority 原地排序 cfgs。
// 健康度开启时：用 healthCache 计算 effectivePriority 与 successRate（仅 SampleCount>0），
// 按 effective 降序；关闭时按 priority DESC, name ASC（与前端 filterChannels 排序键对齐）。
// 返回的两个 map 供 enrichChannel 复用，避免重复计算。
func (s *Server) sortChannelsByEffectivePriority(cfgs []*model.Config, healthEnabled bool) (priorityMap, successRateMap map[int64]float64) {
	priorityMap = make(map[int64]float64, len(cfgs))
	successRateMap = make(map[int64]float64, len(cfgs))
	if healthEnabled {
		hcfg := s.healthCache.Config()
		samples := make([]float64, 0, len(cfgs))
		statsByID := make(map[int64]model.ChannelHealthStats, len(cfgs))
		for _, cfg := range cfgs {
			stats := s.healthCache.GetHealthStats(cfg.ID)
			statsByID[cfg.ID] = stats
			if stats.FirstByteSampleCount > 0 && stats.AvgFirstByteSeconds > 0 {
				samples = append(samples, stats.AvgFirstByteSeconds)
			}
		}
		medianTTFB := medianFloat64(samples)
		for _, cfg := range cfgs {
			stats := statsByID[cfg.ID]
			priorityMap[cfg.ID] = s.calculateEffectivePriority(cfg, stats, hcfg, medianTTFB)
			if stats.SampleCount > 0 {
				successRateMap[cfg.ID] = stats.SuccessRate
			}
		}
		sort.Slice(cfgs, func(i, j int) bool {
			return priorityMap[cfgs[i].ID] > priorityMap[cfgs[j].ID]
		})
	} else {
		sort.Slice(cfgs, func(i, j int) bool {
			if cfgs[i].Priority != cfgs[j].Priority {
				return cfgs[i].Priority > cfgs[j].Priority
			}
			return cfgs[i].Name < cfgs[j].Name
		})
	}
	return priorityMap, successRateMap
}

// paginateChannels 按 query 中的 limit/offset 截取 cfgs。
// limit: [1, 1000]，默认 20；offset: [0, +∞)，默认 0。offset 越界返回空切片。
func paginateChannels(cfgs []*model.Config, c *gin.Context) []*model.Config {
	limit := 20
	offset := 0
	if v, err := strconv.Atoi(strings.TrimSpace(c.DefaultQuery("limit", "20"))); err == nil && v > 0 {
		limit = min(v, 1000)
	}
	if v, err := strconv.Atoi(strings.TrimSpace(c.DefaultQuery("offset", "0"))); err == nil && v >= 0 {
		offset = v
	}
	totalCount := len(cfgs)
	if offset >= totalCount {
		return []*model.Config{}
	}
	end := min(offset+limit, totalCount)
	return cfgs[offset:end]
}

// channelEnrichmentContext 聚合 enrichChannel 所需的批量预计算数据，避免长参数列表。
type channelEnrichmentContext struct {
	now                  time.Time
	healthEnabled        bool
	priorityMap          map[int64]float64
	successRateMap       map[int64]float64
	channelCooldownsMap  map[int64]time.Time
	keyCooldownsMap      map[int64]map[int]time.Time
	modelCooldownsMap    map[int64]map[string]time.Time
	protocolProbeRetries map[int64]protocolProbeRetrySummary
	apiKeysMap           map[int64][]*model.APIKey
}

// enrichChannel 把单个 cfg 拼装为 ChannelWithCooldown：
// 渠道冷却剩余时间、健康度模式下的有效优先级与成功率、Key 策略、Key 与模型冷却详情。
// channelCostMultiplierRange 返回渠道成本倍率区间。
// api_key 渠道取未禁用 Key 的 min/max（无启用 Key 时回退渠道列）；OAuth 渠道即渠道列。
func channelCostMultiplierRange(cfg *model.Config, apiKeys []*model.APIKey) (float64, float64) {
	if cfg.AuthType == model.AuthTypeAPIKey {
		var minValue, maxValue float64
		hasEnabled := false
		for _, key := range apiKeys {
			if key.Disabled {
				continue
			}
			v := key.CostMultiplier
			if v < 0 {
				v = 1
			}
			if !hasEnabled {
				minValue, maxValue = v, v
				hasEnabled = true
				continue
			}
			if v < minValue {
				minValue = v
			}
			if v > maxValue {
				maxValue = v
			}
		}
		if hasEnabled {
			return minValue, maxValue
		}
	}
	m := cfg.CostMultiplier
	if m < 0 {
		m = 1
	}
	return m, m
}

func (ectx *channelEnrichmentContext) enrichChannel(cfg *model.Config) ChannelWithCooldown {
	metadata := channelOAuthMetadataFromCredential(cfg)
	oc := ChannelWithCooldown{
		Config:                       cfg,
		CodexPlanType:                metadata.planType,
		CodexSubscriptionActiveUntil: metadata.subscriptionActiveUntil,
		AnthropicPlanType:            metadata.anthropicPlanType,
		OAuthUsage:                   metadata.oauthUsage,
		AntigravityPaidTier:          metadata.antigravityPaidTier,
		XAIEmail:                     metadata.xaiEmail,
		XAISubscriptionTier:          metadata.xaiSubscriptionTier,
		XAIEntitlementStatus:         metadata.xaiEntitlementStatus,
	}

	// 渠道级别冷却：使用批量查询结果（性能提升：N -> 1 次查询）
	if until, cooled := ectx.channelCooldownsMap[cfg.ID]; cooled && until.After(ectx.now) {
		oc.CooldownUntil = &until
		oc.CooldownRemainingMS = int64(until.Sub(ectx.now) / time.Millisecond)
	}

	// 健康度模式：使用预计算的有效优先级和成功率
	if ectx.healthEnabled {
		if rate, ok := ectx.successRateMap[cfg.ID]; ok {
			oc.SuccessRate = &rate
		}
		effPriority := ectx.priorityMap[cfg.ID]
		oc.EffectivePriority = &effPriority
	}

	// 从预加载的map中获取API Keys（O(1)查找）
	apiKeys := ectx.apiKeysMap[cfg.ID]

	// Key 策略属于渠道行为，详情和列表都必须返回同一语义。
	oc.KeyStrategy = channelKeyStrategy(apiKeys)

	// 成本倍率区间角标：api_key 渠道按启用 Key 计算，OAuth 渠道即渠道倍率。
	multiplierMin, multiplierMax := channelCostMultiplierRange(cfg, apiKeys)
	if multiplierMin != 1 || multiplierMax != 1 {
		minValue, maxValue := multiplierMin, multiplierMax
		oc.CostMultiplierMin = &minValue
		oc.CostMultiplierMax = &maxValue
	}

	keyCooldowns := make([]KeyCooldownInfo, 0, len(apiKeys))
	channelKeyCooldowns := ectx.keyCooldownsMap[cfg.ID]
	for _, apiKey := range apiKeys {
		keyInfo := KeyCooldownInfo{KeyIndex: apiKey.KeyIndex}
		if until, cooled := channelKeyCooldowns[apiKey.KeyIndex]; cooled && until.After(ectx.now) {
			u := until
			keyInfo.CooldownUntil = &u
			keyInfo.CooldownRemainingMS = int64(until.Sub(ectx.now) / time.Millisecond)
		}
		keyCooldowns = append(keyCooldowns, keyInfo)
	}
	oc.KeyCooldowns = keyCooldowns
	oc.ModelCooldowns = activeModelCooldownInfos(ectx.modelCooldownsMap[cfg.ID], ectx.now)
	applyProtocolProbeRetrySummary(&oc, ectx.protocolProbeRetries[cfg.ID], ectx.now)
	return oc
}

func applyProtocolProbeRetrySummary(channel *ChannelWithCooldown, summary protocolProbeRetrySummary, now time.Time) {
	if channel == nil || summary.count <= 0 || !summary.retryAt.After(now) {
		return
	}
	retryAt := summary.retryAt
	channel.ProtocolProbeRetryCount = summary.count
	channel.ProtocolProbeRetryAt = &retryAt
	channel.ProtocolProbeRetryRemainingMS = max(1, retryAt.Sub(now).Milliseconds())
}

type channelOAuthMetadata struct {
	planType                string
	subscriptionActiveUntil *time.Time
	anthropicPlanType       string
	oauthUsage              *oauthUsageSummary
	antigravityPaidTier     string
	xaiEmail                string
	xaiSubscriptionTier     string
	xaiEntitlementStatus    string
}

func channelOAuthMetadataFromCredential(cfg *model.Config) channelOAuthMetadata {
	if cfg == nil || cfg.OAuthCredential == "" {
		return channelOAuthMetadata{}
	}
	if cfg.UsesAntigravityOAuth() {
		credential, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return channelOAuthMetadata{}
		}
		usage, _, _ := persistedOAuthUsage(credential.OAuthUsage, antigravityauth.ChannelType)
		usage = attachOAuthQuotaCostUsage(usage, credential.QuotaCostUsage)
		return channelOAuthMetadata{
			antigravityPaidTier: credential.PaidTier.DisplayName(),
			oauthUsage:          usage,
		}
	}
	if cfg.UsesXAIOAuth() {
		credential, err := xaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return channelOAuthMetadata{}
		}
		usage, _, _ := persistedOAuthUsage(credential.OAuthUsage, xaiauth.ChannelType)
		usage = attachOAuthQuotaCostUsage(usage, credential.QuotaCostUsage)
		return channelOAuthMetadata{
			xaiEmail:             credential.Identity().Email,
			xaiSubscriptionTier:  strings.TrimSpace(credential.SubscriptionTier),
			xaiEntitlementStatus: strings.TrimSpace(credential.EntitlementStatus),
			oauthUsage:           usage,
		}
	}
	if cfg.UsesAnthropicOAuth() {
		credential, err := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return channelOAuthMetadata{}
		}
		active, activeSampledAt, _ := persistedOAuthUsage(credential.OAuthUsage, anthropicauth.ChannelType)
		passiveSampledAt := ""
		if credential.PassiveUsage != nil {
			passiveSampledAt = credential.PassiveUsage.SampledAt
		}
		usage := latestOAuthUsage(
			active, activeSampledAt, anthropicPassiveUsageSummary(credential), passiveSampledAt,
		)
		return channelOAuthMetadata{
			anthropicPlanType: strings.TrimSpace(credential.PlanType),
			oauthUsage:        attachOAuthQuotaCostUsage(usage, credential.QuotaCostUsage),
		}
	}
	if cfg.UsesZAIOAuth() {
		credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return channelOAuthMetadata{}
		}
		usage, _, _ := persistedOAuthUsage(credential.OAuthUsage, zaiauth.ChannelType)
		return channelOAuthMetadata{oauthUsage: usage}
	}
	if cfg.UsesCursorOAuth() {
		credential, err := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return channelOAuthMetadata{}
		}
		usage, _, _ := persistedOAuthUsage(credential.OAuthUsage, cursorauth.ChannelType)
		return channelOAuthMetadata{oauthUsage: usage}
	}
	if cfg.UsesZedOAuth() {
		credential, err := zedauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return channelOAuthMetadata{}
		}
		usage, _, _ := persistedOAuthUsage(credential.OAuthUsage, zedauth.ChannelType)
		return channelOAuthMetadata{oauthUsage: usage}
	}
	if !cfg.UsesCodexOAuth() {
		return channelOAuthMetadata{}
	}
	credential, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return channelOAuthMetadata{}
	}
	active, activeSampledAt, _ := persistedOAuthUsage(credential.OAuthUsage, codexauth.ChannelType)
	passiveSampledAt := ""
	if credential.PassiveUsage != nil {
		passiveSampledAt = credential.PassiveUsage.SampledAt
	}
	usage := latestOAuthUsage(
		active, activeSampledAt, codexPassiveUsageSummary(credential), passiveSampledAt,
	)
	metadata := channelOAuthMetadata{
		planType:   credential.PlanType,
		oauthUsage: attachOAuthQuotaCostUsage(usage, credential.QuotaCostUsage),
	}
	if until, ok := credential.SubscriptionActiveUntil(); ok {
		metadata.subscriptionActiveUntil = &until
	}
	return metadata
}

func activeModelCooldownInfos(cooldowns map[string]time.Time, now time.Time) []ModelCooldownInfo {
	infos := make([]ModelCooldownInfo, 0, len(cooldowns))
	for modelName, until := range cooldowns {
		if !until.After(now) {
			continue
		}
		u := until
		infos = append(infos, ModelCooldownInfo{
			Model:               modelName,
			CooldownUntil:       &u,
			CooldownRemainingMS: int64(until.Sub(now) / time.Millisecond),
		})
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Model < infos[j].Model
	})
	return infos
}

// HandleChannelsFilterOptions 返回渠道筛选下拉的全集（渠道名/模型），
// 仅按 type/status 联动，与列表分页/搜索/模型筛选解耦。
// GET /admin/channels/filter-options?type=&status=
func (s *Server) HandleChannelsFilterOptions(c *gin.Context) {
	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	status := strings.TrimSpace(c.Query("status"))
	var cooldowns channelCooldownSnapshot
	if status == "cooldown" {
		cooldowns = s.loadChannelCooldownSnapshot(c.Request.Context())
	}
	cfgs = filterChannelOptionConfigs(
		cfgs,
		strings.TrimSpace(c.Query("protocol")),
		status,
		cooldowns,
		time.Now(),
	)
	RespondJSON(c, http.StatusOK, buildChannelFilterOptions(cfgs))
}

// HandleCheckDuplicateChannel 检测渠道是否与已有渠道重复
// POST /admin/channels/check-duplicate
// 判断条件：任意规范化 URL 与已有渠道相交。
func (s *Server) HandleCheckDuplicateChannel(c *gin.Context) {
	var req CheckDuplicateRequest
	if err := BindAndValidate(c, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	// 构建新渠道 URL 集合（去除空行）
	newURLSet := make(map[string]struct{}, len(req.URLs))
	for _, entry := range req.URLs {
		newURLSet[entry.RuntimeURL()] = struct{}{}
	}

	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	var duplicates []DuplicateChannelInfo
	for _, cfg := range cfgs {
		// 遍历已有渠道的 URL 行，检查是否与新渠道 URL 有交集
		for _, line := range cfg.GetURLs() {
			if _, ok := newURLSet[line]; ok {
				duplicates = append(duplicates, DuplicateChannelInfo{
					ID:   cfg.ID,
					Name: cfg.Name,
					URLs: cfg.URLs.Clone(),
				})
				break // 同一渠道只报告一次
			}
		}
	}

	if duplicates == nil {
		duplicates = []DuplicateChannelInfo{}
	}
	RespondJSON(c, http.StatusOK, CheckDuplicateResponse{Duplicates: duplicates})
}

// 创建新渠道
func (s *Server) handleCreateChannel(c *gin.Context) {
	var req ChannelRequest
	if err := BindAndValidate(c, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if req.forbiddenCredentialFields {
		RespondErrorMsg(c, http.StatusConflict, "credential fields must be submitted through management_account")
		return
	}
	if req.managementAccountSet && req.AuthType != model.AuthTypeAPIKey {
		RespondErrorMsg(c, http.StatusConflict, "OAuth channels cannot use management_account")
		return
	}
	if req.AuthType != model.AuthTypeAPIKey {
		RespondErrorMsg(c, http.StatusBadRequest, "OAuth channels must be created by login or credential import")
		return
	}
	// 创建渠道（不包含API Key）
	created, err := s.store.CreateConfig(c.Request.Context(), req.ToConfig())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if req.managementAccountSet {
		if _, err := s.channelManagement.SaveSettings(c.Request.Context(), created, req.ManagementAccount); err != nil {
			s.rollbackCreatedChannel(c.Request.Context(), created.ID)
			respondChannelManagementError(c, err)
			return
		}
	}

	keyStrategy := strings.TrimSpace(req.KeyStrategy)
	if keyStrategy == "" {
		keyStrategy = model.KeyStrategySequential // 默认策略
	}

	now := time.Now()
	apiKeyEntries := req.normalizeAPIKeys()
	keysToCreate := make([]*model.APIKey, 0, len(apiKeyEntries))
	for i, entry := range apiKeyEntries {
		keysToCreate = append(keysToCreate, &model.APIKey{
			ChannelID:       created.ID,
			KeyIndex:        i,
			APIKey:          entry.APIKey,
			Note:            entry.Note,
			AllowedModels:   append([]string(nil), entry.AllowedModels...),
			ModelScopeEmpty: entry.ModelScopeEmpty,
			KeyStrategy:     keyStrategy,
			Disabled:        entry.ModelScopeEmpty,
			CostMultiplier:  apiKeyCostMultiplier(entry),
			CreatedAt:       model.JSONTime{Time: now},
			UpdatedAt:       model.JSONTime{Time: now},
		})
	}
	if len(keysToCreate) > 0 {
		if err := s.store.CreateAPIKeysBatch(c.Request.Context(), keysToCreate); err != nil {
			log.Printf("[WARN] 批量创建API Key失败 (channel=%d): %v", created.ID, err)
		}
	}

	// 新增渠道后，失效渠道列表缓存使选择器立即可见
	s.InvalidateChannelListCache()

	RespondJSON(c, http.StatusCreated, created)
}

// HandleChannelByID 处理单个渠道的CRUD操作
func (s *Server) HandleChannelByID(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	// [INFO] Linus风格：直接switch，删除不必要的抽象
	switch c.Request.Method {
	case "GET":
		s.handleGetChannel(c, id)
	case "PUT":
		s.handleUpdateChannel(c, id)
	case "DELETE":
		s.handleDeleteChannel(c, id)
	default:
		RespondErrorMsg(c, 405, "method not allowed")
	}
}

// 获取单个渠道（包含key_strategy信息）
func (s *Server) handleGetChannel(c *gin.Context, id int64) {
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}
	detail, _, err := s.buildChannelDetail(c.Request.Context(), id, cfg)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	RespondJSON(c, http.StatusOK, detail)
}

func (s *Server) buildChannelDetail(ctx context.Context, id int64, cfg *model.Config) (ChannelWithCooldown, []*model.APIKey, error) {
	// 填充空的重定向模型为请求模型（方便前端编辑时显示）
	for i := range cfg.ModelEntries {
		if cfg.ModelEntries[i].RedirectModel == "" {
			cfg.ModelEntries[i].RedirectModel = cfg.ModelEntries[i].Model
		}
	}

	apiKeys, err := s.getAPIKeys(ctx, id)
	if err != nil {
		return ChannelWithCooldown{}, nil, err
	}
	if apiKeys == nil {
		apiKeys = make([]*model.APIKey, 0)
	}
	allModelCooldowns, err := s.getAllModelCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] 查询渠道模型冷却状态失败 (channel=%d): %v", id, err)
		allModelCooldowns = make(map[int64]map[string]time.Time)
	}

	now := time.Now()
	metadata := channelOAuthMetadataFromCredential(cfg)
	detail := ChannelWithCooldown{
		Config:                       cfg,
		CodexPlanType:                metadata.planType,
		CodexSubscriptionActiveUntil: metadata.subscriptionActiveUntil,
		AnthropicPlanType:            metadata.anthropicPlanType,
		OAuthUsage:                   metadata.oauthUsage,
		AntigravityPaidTier:          metadata.antigravityPaidTier,
		XAIEmail:                     metadata.xaiEmail,
		XAISubscriptionTier:          metadata.xaiSubscriptionTier,
		ManagementAccount:            s.managementAccountView(cfg),
		XAIEntitlementStatus:         metadata.xaiEntitlementStatus,
		KeyStrategy:                  channelKeyStrategy(apiKeys),
		ModelCooldowns:               activeModelCooldownInfos(allModelCooldowns[id], now),
	}
	applyProtocolProbeRetrySummary(&detail, s.protocolCapabilities.unsupportedRetrySummaries(now)[id], now)
	return detail, apiKeys, nil
}

// handleGetChannelKeys 获取渠道的所有 API Keys
// GET /admin/channels/{id}/keys
func (s *Server) handleGetChannelKeys(c *gin.Context, id int64) {
	ctx := c.Request.Context()
	cfg, err := s.store.GetConfig(ctx, id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}
	apiKeys, err := s.getAPIKeys(ctx, id)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	apiKeys, err = channelKeysForAdmin(cfg, apiKeys)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, apiKeys)
}

func channelKeysForAdmin(cfg *model.Config, storedKeys []*model.APIKey) ([]*model.APIKey, error) {
	if cfg == nil || !cfg.UsesOAuth() {
		if storedKeys == nil {
			return make([]*model.APIKey, 0), nil
		}
		return storedKeys, nil
	}

	accessToken, note, err := oauthSyntheticKeyFields(cfg)
	if err != nil {
		return nil, err
	}

	return []*model.APIKey{{
		ChannelID:      cfg.ID,
		KeyIndex:       0,
		APIKey:         util.MaskAPIKey(accessToken),
		Note:           note,
		KeyStrategy:    model.KeyStrategySequential,
		CostMultiplier: cfg.CostMultiplier,
	}}, nil
}

// oauthSyntheticKeyFields 解析 OAuth 渠道凭证，返回合成 Key 行所需的原始凭证值与备注。
func oauthSyntheticKeyFields(cfg *model.Config) (accessToken, note string, err error) {
	switch {
	case cfg.UsesCodexOAuth():
		credential, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return "", "", err
		}
		return credential.AccessToken, codexCredentialKeyNote(credential), nil
	case cfg.UsesAntigravityOAuth():
		credential, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return "", "", err
		}
		return credential.AccessToken, "Antigravity OAuth AT", nil
	case cfg.UsesXAIOAuth():
		credential, err := xaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return "", "", err
		}
		return credential.AccessToken, "xAI OAuth AT", nil
	case cfg.UsesAnthropicOAuth():
		credential, err := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return "", "", err
		}
		return credential.AccessToken, "Anthropic OAuth AT", nil
	case cfg.UsesZAIOAuth():
		credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return "", "", err
		}
		return credential.APIKey, "Z.ai Coding Plan Key", nil
	case cfg.UsesCursorOAuth():
		credential, err := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return "", "", err
		}
		return credential.AccessToken, "Cursor session", nil
	case cfg.UsesZedOAuth():
		credential, err := zedauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return "", "", err
		}
		return credential.AccessToken, "Zed LLM JWT", nil
	}
	return "", "", nil
}

func codexCredentialKeyNote(credential *codexauth.Credential) string {
	if credential != nil && credential.IsPersonalAccessToken() {
		return "Codex Personal Access Token"
	}
	return "Codex OAuth AT"
}

// HandleChannelModelStats 返回渠道当天的按模型轻量统计。
// GET /admin/channels/:id/model-stats
func (s *Server) HandleChannelModelStats(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	if _, err := s.store.GetConfig(c.Request.Context(), id); err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}

	result, err := s.getChannelModelStats(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, result)
}

func (s *Server) getChannelModelStats(ctx context.Context, id int64) ([]ChannelModelStats, error) {
	params := &PaginationParams{Range: "today"}
	startTime, endTime := params.GetTimeRange()
	filter := &model.LogFilter{
		ChannelID: &id,
		LogSource: model.LogSourceProxy,
	}
	stats, err := s.statsCache.GetStatsLite(ctx, startTime, endTime, filter)
	if err != nil {
		return nil, err
	}
	result := make([]ChannelModelStats, 0, len(stats))
	for _, entry := range stats {
		result = append(result, ChannelModelStats{
			Model:                   entry.Model,
			Success:                 entry.Success,
			Error:                   entry.Error,
			Total:                   entry.Total,
			AvgFirstByteTimeSeconds: entry.AvgFirstByteTimeSeconds,
			AvgDurationSeconds:      entry.AvgDurationSeconds,
		})
	}
	return result, nil
}

// HandleChannelURLStats 返回渠道各URL的实时状态（延迟、冷却）
// GET /admin/channels/:id/url-stats
func (s *Server) HandleChannelURLStats(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}

	urls := cfg.GetURLs()
	if len(urls) == 0 || s.urlSelector == nil {
		RespondJSON(c, http.StatusOK, []URLStat{})
		return
	}

	stats := s.urlSelector.GetURLStats(id, urls)
	RespondJSON(c, http.StatusOK, stats)
}

// HandleURLDisable 手动禁用渠道的指定URL
// POST /admin/channels/:id/url-disable
func (s *Server) HandleURLDisable(c *gin.Context) {
	s.handleURLToggle(c, true)
}

// HandleURLEnable 重新启用渠道的指定URL
// POST /admin/channels/:id/url-enable
func (s *Server) HandleURLEnable(c *gin.Context) {
	s.handleURLToggle(c, false)
}

func (s *Server) handleURLToggle(c *gin.Context, disable bool) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	var req struct {
		URL string `json:"url" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "url is required")
		return
	}

	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}

	// 验证URL属于该渠道
	urls := cfg.GetURLs()
	if !slices.Contains(urls, req.URL) {
		RespondErrorMsg(c, http.StatusBadRequest, "url not found in channel")
		return
	}

	if s.urlSelector == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "url selector not available")
		return
	}

	s.disabledURLSyncMu.Lock()
	defer s.disabledURLSyncMu.Unlock()
	if err := s.store.SetURLDisabled(c.Request.Context(), id, req.URL, disable); err != nil {
		RespondErrorMsg(c, http.StatusInternalServerError, "persist url state failed")
		return
	}

	if disable {
		s.urlSelector.DisableURL(id, req.URL)
	} else {
		s.urlSelector.EnableURL(id, req.URL)
	}

	RespondJSON(c, http.StatusOK, gin.H{"ok": true})
}

// HandleAPIKeyDisable 手动禁用渠道的指定 API Key
// POST /admin/channels/:id/key-disable
func (s *Server) HandleAPIKeyDisable(c *gin.Context) {
	s.handleAPIKeyToggle(c, true)
}

// HandleAPIKeyEnable 重新启用渠道的指定 API Key
// POST /admin/channels/:id/key-enable
func (s *Server) HandleAPIKeyEnable(c *gin.Context) {
	s.handleAPIKeyToggle(c, false)
}

func (s *Server) handleAPIKeyToggle(c *gin.Context, disable bool) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	var req struct {
		KeyIndex *int `json:"key_index"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "key_index is required")
		return
	}
	if req.KeyIndex == nil || *req.KeyIndex < 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid key_index")
		return
	}
	keyIndex := *req.KeyIndex
	if !s.requireMutableAPIKeys(c, id) {
		return
	}

	key, err := s.store.GetAPIKey(c.Request.Context(), id, keyIndex)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "api key not found")
		return
	}
	if !disable && key.ModelScopeEmpty {
		RespondErrorMsg(c, http.StatusConflict, "configure at least one model or allow all models before enabling this key")
		return
	}

	if err := s.store.SetAPIKeyDisabled(c.Request.Context(), id, keyIndex, disable); err != nil {
		RespondErrorMsg(c, http.StatusInternalServerError, "persist key disabled state failed")
		return
	}

	s.InvalidateAPIKeysCache(id)
	s.invalidateCooldownCache()
	s.InvalidateChannelListCache()

	RespondJSON(c, http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleUpdateChannelModelDisabled(c *gin.Context, id int64, modelName string, disabled bool) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "model cannot be empty")
		return
	}

	ctx := c.Request.Context()
	cfg, err := s.store.GetConfig(ctx, id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}

	found := -1
	for i := range cfg.ModelEntries {
		if strings.EqualFold(cfg.ModelEntries[i].Model, modelName) {
			found = i
			break
		}
	}
	if found < 0 {
		RespondErrorMsg(c, http.StatusNotFound, "model not found")
		return
	}

	if cfg.ModelEntries[found].Disabled == disabled {
		RespondJSON(c, http.StatusOK, cfg)
		return
	}

	cfg.ModelEntries[found].Disabled = disabled
	upd, err := s.store.UpdateConfig(ctx, id, cfg)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	s.InvalidateAPIKeysCache(id)
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, upd)
}

// 更新渠道
func (s *Server) handleUpdateChannel(c *gin.Context, id int64) {
	// 解析请求为通用map以支持部分更新
	var rawReq map[string]any
	if err := c.ShouldBindJSON(&rawReq); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request format")
		return
	}

	// 检查是否为简单的enabled字段更新
	if len(rawReq) == 1 {
		if enabled, ok := rawReq["enabled"].(bool); ok {
			upd, err := s.store.UpdateChannelEnabled(c.Request.Context(), id, enabled)
			if err != nil {
				if strings.Contains(err.Error(), "not found") {
					RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
				} else {
					RespondError(c, http.StatusInternalServerError, err)
				}
				return
			}
			if enabled {
				s.clearAllChannelCooldowns(c.Request.Context(), id)
			}
			if upd.UsesXAIOAuth() && s.xaiCredentials != nil {
				s.xaiCredentials.invalidate(id)
			}
			// enabled 状态变更影响渠道选择，必须立即失效缓存
			s.InvalidateChannelListCache()
			RespondJSON(c, http.StatusOK, upd)
			return
		}
	}

	if len(rawReq) == 2 {
		modelName, hasModel := rawReq["model"].(string)
		disabled, hasDisabled := rawReq["disabled"].(bool)
		if hasModel && hasDisabled {
			s.handleUpdateChannelModelDisabled(c, id, modelName, disabled)
			return
		}
	}

	existing, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}

	// 处理完整更新：重新序列化为ChannelRequest
	reqBytes, err := sonic.Marshal(rawReq)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request format")
		return
	}

	var req ChannelRequest
	if err := sonic.Unmarshal(reqBytes, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request format")
		return
	}
	rawAuthType := strings.TrimSpace(req.AuthType)
	if rawAuthType == "" {
		req.AuthType = existing.GetAuthType()
	} else {
		req.AuthType = model.NormalizeAuthType(rawAuthType)
		if req.AuthType == "" {
			RespondErrorMsg(c, http.StatusBadRequest, "invalid auth_type")
			return
		}
	}
	if req.forbiddenCredentialFields {
		RespondErrorMsg(c, http.StatusConflict, "credential fields must be submitted through management_account")
		return
	}
	if req.managementAccountSet && req.AuthType != model.AuthTypeAPIKey {
		RespondErrorMsg(c, http.StatusConflict, "OAuth channels cannot use management_account")
		return
	}
	if req.managementAccountSet {
		if req.ManagementAccount == nil {
			RespondErrorMsg(c, http.StatusBadRequest, "invalid management account")
			return
		}
	}
	if existing.UsesOAuth() {
		if req.AuthType != existing.GetAuthType() {
			RespondErrorMsg(c, http.StatusConflict, "OAuth channel auth_type is read-only")
			return
		}
		// 合成 Key 行最多一条，只用于回传倍率，永不落库；
		// 其 api_key 必须是当前凭证掩码值，防止借合成行改写凭证，其余 Key 变更一律 409。
		submittedKeys := req.normalizeAPIKeys()
		if len(submittedKeys) > 1 {
			RespondErrorMsg(c, http.StatusConflict, "OAuth channel accepts at most one synthetic API key row")
			return
		}
		if len(submittedKeys) == 1 {
			accessToken, _, parseErr := oauthSyntheticKeyFields(existing)
			if parseErr != nil {
				RespondError(c, http.StatusInternalServerError, parseErr)
				return
			}
			if submittedKeys[0].APIKey != util.MaskAPIKey(accessToken) {
				RespondErrorMsg(c, http.StatusConflict, "OAuth channel API keys are read-only")
				return
			}
		}
		if _, submitted := rawReq["key_strategy"]; submitted {
			RespondErrorMsg(c, http.StatusConflict, "OAuth channel key strategy is read-only")
			return
		}
	}
	if existing.UsesXAIOAuth() {
		for _, field := range []string{"oauth_credential", "credential", "access_token", "refresh_token", "id_token"} {
			if _, submitted := rawReq[field]; submitted {
				RespondErrorMsg(c, http.StatusConflict, "xAI OAuth credential is read-only")
				return
			}
		}
	}
	if existing.UsesCodexOAuth() {
		credential, parseErr := codexauth.ParseCredential([]byte(existing.OAuthCredential))
		if parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		req.Models = filterCodexOAuthModelEntries(req.Models, credential.PlanType)
	}
	var oldKeys []*model.APIKey
	if !existing.UsesOAuth() {
		oldKeys, err = s.getAPIKeys(c.Request.Context(), id)
		if err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		submittedKeys := req.normalizeAPIKeys()
		preserveOmittedAPIKeyAllowedModels(submittedKeys, oldKeys)
		req.APIKeys = submittedKeys
		req.APIKey = strings.Join(apiKeyStrings(submittedKeys), ",")
	}
	// Normalize submitted scopes before validation so a stale editor cannot
	// resurrect models that were removed from the channel in the meantime.
	req.APIKeys = req.normalizeAPIKeys()
	normalizeAPIKeyScopesForModels(req.APIKeys, req.Models)
	req.APIKey = strings.Join(apiKeyStrings(req.APIKeys), ",")

	if err := req.Validate(); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.managementAccountSet {
		// Sub2API login creates a real upstream session. Run it only after every
		// local channel field has passed validation, so rejected edits have no
		// remote side effects.
		candidate := req.ToConfig()
		candidate.ID = existing.ID
		candidate.OAuthCredential = existing.OAuthCredential
		resolvedManagement, resolveErr := s.channelManagement.resolveChannelManagementInput(
			c.Request.Context(), candidate, req.ManagementAccount,
		)
		if resolveErr != nil {
			respondChannelManagementError(c, resolveErr)
			return
		}
		if _, _, mergeErr := mergeChannelManagementSettings(existing.OAuthCredential, resolvedManagement); mergeErr != nil {
			RespondErrorMsg(c, http.StatusBadRequest, "invalid management account")
			return
		}
		req.ManagementAccount = resolvedManagement
	}

	// 检测api_key是否变化（需要重建API Keys）
	if existing.UsesOAuth() {
		oldKeys = nil
	}

	newKeys := req.normalizeAPIKeys()
	normalizeAPIKeyScopesForModels(newKeys, req.Models)
	keyStrategy := strings.TrimSpace(req.KeyStrategy)
	if keyStrategy == "" {
		keyStrategy = model.KeyStrategySequential
	}

	// 比较Key数量和内容是否变化
	keyChanged := len(oldKeys) != len(newKeys)
	if !keyChanged {
		for i, oldKey := range oldKeys {
			if i >= len(newKeys) || oldKey.APIKey != newKeys[i].APIKey {
				keyChanged = true
				break
			}
		}
	}

	notesByIndex := make(map[int]string)
	scopesByIndex := make(map[int]model.APIKeyModelScope)
	multipliersByIndex := make(map[int]float64)
	if !keyChanged {
		for i, oldKey := range oldKeys {
			if oldKey.Note != newKeys[i].Note {
				notesByIndex[oldKey.KeyIndex] = newKeys[i].Note
			}
			if newKeys[i].CostMultiplier != nil && *newKeys[i].CostMultiplier != oldKey.CostMultiplier {
				multipliersByIndex[oldKey.KeyIndex] = *newKeys[i].CostMultiplier
			}
			if !slices.Equal(oldKey.AllowedModels, newKeys[i].AllowedModels) || oldKey.ModelScopeEmpty != newKeys[i].ModelScopeEmpty {
				scopesByIndex[oldKey.KeyIndex] = model.APIKeyModelScope{
					AllowedModels:   append([]string(nil), newKeys[i].AllowedModels...),
					ModelScopeEmpty: newKeys[i].ModelScopeEmpty,
					// Only preserve a manually disabled key. A key disabled because
					// its previous scope became empty is re-enabled by an explicit
					// scope edit.
					Disabled: (oldKey.Disabled && !oldKey.ModelScopeEmpty) || newKeys[i].ModelScopeEmpty,
				}
			}
		}
	}
	noteChanged := len(notesByIndex) > 0
	modelsChanged := len(scopesByIndex) > 0
	multiplierChanged := len(multipliersByIndex) > 0

	// [INFO] 修复 (2025-10-11): 检测策略变化
	strategyChanged := false
	if !keyChanged && len(oldKeys) > 0 && len(newKeys) > 0 {
		// Key内容未变化时，检查策略是否变化
		oldStrategy := oldKeys[0].KeyStrategy
		if oldStrategy == "" {
			oldStrategy = model.KeyStrategySequential
		}
		strategyChanged = oldStrategy != keyStrategy
	}

	// OAuth 渠道倍率经合成 Key 行回传；未提交时保留现值，避免 UpdateConfig 无条件覆盖成 0。
	if existing.UsesOAuth() {
		submitted := req.normalizeAPIKeys()
		if len(submitted) == 0 || submitted[0].CostMultiplier == nil {
			multiplier := existing.CostMultiplier
			if len(submitted) == 0 {
				req.APIKeys = []ChannelAPIKeyRequest{{CostMultiplier: &multiplier}}
			} else {
				submitted[0].CostMultiplier = &multiplier
				req.APIKeys = submitted
			}
		}
	}

	upd, err := s.store.UpdateConfig(c.Request.Context(), id, req.ToConfig())
	if err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}
	if existing.UsesXAIOAuth() && s.xaiCredentials != nil {
		s.xaiCredentials.invalidate(id)
	}

	// Key或策略变化时更新API Keys
	if existing.UsesOAuth() {
		// OAuth 凭证只由登录、导入和刷新链路维护。
	} else if keyChanged {
		existingByValue := make(map[string][]*model.APIKey, len(oldKeys))
		for _, oldKey := range oldKeys {
			if oldKey != nil {
				existingByValue[oldKey.APIKey] = append(existingByValue[oldKey.APIKey], oldKey)
			}
		}
		nextOccurrence := make(map[string]int, len(existingByValue))

		// Key内容/数量变化：删除旧Key并重建
		if err := s.store.DeleteAllAPIKeys(c.Request.Context(), id); err != nil {
			RespondError(c, http.StatusInternalServerError, fmt.Errorf("delete old API keys before rebuild: %w", err))
			return
		}

		// 批量创建新的API Keys（优化：单次事务插入替代循环单条插入）
		now := time.Now()
		apiKeys := make([]*model.APIKey, 0, len(newKeys))
		for i, key := range newKeys {
			var wasDisabled bool
			occurrence := nextOccurrence[key.APIKey]
			if matches := existingByValue[key.APIKey]; occurrence < len(matches) {
				wasDisabled = matches[occurrence].Disabled && !matches[occurrence].ModelScopeEmpty
				nextOccurrence[key.APIKey] = occurrence + 1
			}
			apiKeys = append(apiKeys, &model.APIKey{
				ChannelID:       id,
				KeyIndex:        i,
				APIKey:          key.APIKey,
				Note:            key.Note,
				AllowedModels:   append([]string(nil), key.AllowedModels...),
				ModelScopeEmpty: key.ModelScopeEmpty,
				KeyStrategy:     keyStrategy,
				Disabled:        wasDisabled || key.ModelScopeEmpty,
				CostMultiplier:  apiKeyCostMultiplier(key),
				CreatedAt:       model.JSONTime{Time: now},
				UpdatedAt:       model.JSONTime{Time: now},
			})
		}
		if err := s.store.CreateAPIKeysBatch(c.Request.Context(), apiKeys); err != nil {
			RespondError(c, http.StatusInternalServerError, fmt.Errorf("rebuild API keys: %w", err))
			return
		}
	} else {
		// Key内容未变化：策略和备注都是独立元数据，不能重建 Key 导致禁用状态丢失。
		if strategyChanged {
			if err := s.store.UpdateAPIKeysStrategy(c.Request.Context(), id, keyStrategy); err != nil {
				log.Printf("[WARN] 批量更新API Key策略失败 (channel=%d): %v", id, err)
			}
		}
		if noteChanged {
			if err := s.store.UpdateAPIKeyNotes(c.Request.Context(), id, notesByIndex); err != nil {
				log.Printf("[WARN] 批量更新API Key备注失败 (channel=%d): %v", id, err)
			}
		}
		if multiplierChanged {
			if err := s.store.UpdateAPIKeyCostMultipliers(c.Request.Context(), id, multipliersByIndex); err != nil {
				RespondError(c, http.StatusInternalServerError, fmt.Errorf("update API key cost multipliers: %w", err))
				return
			}
		}
		if modelsChanged {
			if err := s.store.UpdateAPIKeyModelScopes(c.Request.Context(), id, scopesByIndex); err != nil {
				RespondError(c, http.StatusInternalServerError, fmt.Errorf("update API key model scopes: %w", err))
				return
			}
		}
	}
	if req.managementAccountSet {
		if _, err := s.channelManagement.SaveSettings(c.Request.Context(), upd, req.ManagementAccount); err != nil {
			respondChannelManagementError(c, err)
			return
		}
	}

	// 编辑保存后重置渠道、Key 和模型冷却状态。
	s.clearAllChannelCooldowns(c.Request.Context(), id)

	// 渠道更新后刷新缓存，确保选择器立即生效
	s.InvalidateChannelListCache()

	// URL 更新后立即清理失效的 URL 状态（内存+数据库同步）
	if s.urlSelector != nil {
		s.urlSelector.PruneChannel(id, upd.GetURLs())
	}
	// 同步清理数据库中已移除URL的禁用状态记录
	s.cleanupOrphanedURLStates(c.Request.Context(), id, upd.GetURLs())

	RespondJSON(c, http.StatusOK, upd)
}

func preserveOmittedAPIKeyAllowedModels(submitted []ChannelAPIKeyRequest, existing []*model.APIKey) {
	byValue := make(map[string]*model.APIKey, len(existing))
	for _, key := range existing {
		if key != nil {
			if _, seen := byValue[key.APIKey]; !seen {
				byValue[key.APIKey] = key
			}
		}
	}
	for i := range submitted {
		if submitted[i].allowedModelsSet {
			continue
		}
		oldKey := byValue[submitted[i].APIKey]
		if i < len(existing) && existing[i] != nil && existing[i].APIKey == submitted[i].APIKey {
			oldKey = existing[i]
		}
		if oldKey != nil {
			submitted[i].AllowedModels = append([]string(nil), oldKey.AllowedModels...)
			submitted[i].ModelScopeEmpty = oldKey.ModelScopeEmpty
		}
	}
}

// normalizeAPIKeyScopesForModels removes scopes that no longer exist in the
// submitted model table. This is the backend safety net for stale editors: a
// key rebuild must not reintroduce a model deleted from the channel.
func normalizeAPIKeyScopesForModels(keys []ChannelAPIKeyRequest, entries []model.ModelEntry) {
	configured := make(map[string]struct{}, len(entries))
	wildcard := false
	for _, entry := range entries {
		name := strings.ToLower(strings.TrimSpace(model.RoutingModelName(entry.Model)))
		if name == "*" {
			wildcard = true
			continue
		}
		if name != "" {
			configured[name] = struct{}{}
		}
	}
	if wildcard {
		return
	}
	for i := range keys {
		if keys[i].ModelScopeEmpty || len(keys[i].AllowedModels) == 0 {
			continue
		}
		kept := keys[i].AllowedModels[:0]
		for _, allowed := range keys[i].AllowedModels {
			name := strings.ToLower(strings.TrimSpace(model.RoutingModelName(allowed)))
			if _, ok := configured[name]; ok {
				kept = append(kept, allowed)
			}
		}
		keys[i].AllowedModels = kept
		if len(kept) == 0 {
			keys[i].ModelScopeEmpty = true
		}
	}
}

// resetAllChannelCooldowns 清除渠道、Key、模型和 URL 冷却，并立即失效相关缓存。
func (s *Server) resetAllChannelCooldowns(ctx context.Context, channelID int64) error {
	if s.cooldownManager == nil {
		s.invalidateChannelRelatedCache(channelID)
		return fmt.Errorf("cooldown manager is not initialized")
	}
	err := s.cooldownManager.ClearAllCooldowns(ctx, channelID)
	if err == nil && s.urlSelector != nil {
		s.urlSelector.ClearCooldowns(channelID)
	}
	s.invalidateChannelRelatedCache(channelID)
	return err
}

// clearAllChannelCooldowns 用于配置更新后的尽力清理；失败不回滚已提交的配置。
func (s *Server) clearAllChannelCooldowns(ctx context.Context, channelID int64) {
	if s.cooldownManager == nil {
		s.invalidateChannelRelatedCache(channelID)
		return
	}
	if err := s.resetAllChannelCooldowns(ctx, channelID); err != nil {
		log.Printf("[WARN] 清除渠道全部冷却状态失败 (channel=%d): %v", channelID, err)
	}
}

// 删除渠道
func (s *Server) handleDeleteChannel(c *gin.Context, id int64) {
	deleted, err := s.deleteChannelByID(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if !deleted {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}

	s.InvalidateChannelListCache()
	// 删除渠道后必须同步失效该渠道的 API Keys 缓存，
	// 否则若后续以同 ID 重新创建渠道（显式主键路径，例如混合存储恢复），可能读到旧 keys。
	s.InvalidateAPIKeysCache(id)
	if err := s.reloadAuthTokensAfterChannelDeletion(); err != nil {
		log.Printf("渠道 %d 已删除，但 API 令牌限制热更新失败: %v", id, err)
		RespondError(c, http.StatusServiceUnavailable, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"id": id})
}

// cleanupOrphanedURLStates 清理数据库中已移除URL的禁用状态记录，失败仅警告不影响主流程
func (s *Server) cleanupOrphanedURLStates(ctx context.Context, channelID int64, keepURLs []string) {
	if s.store == nil {
		return
	}

	if err := s.store.CleanupOrphanedURLStates(ctx, channelID, keepURLs); err != nil {
		log.Printf("[WARN] 清理孤立URL状态失败 (channel=%d, urls=%d): %v", channelID, len(keepURLs), err)
	}
}

func (s *Server) requireMutableAPIKeys(c *gin.Context, channelID int64) bool {
	cfg, err := s.store.GetConfig(c.Request.Context(), channelID)
	if err != nil {
		RespondError(c, http.StatusNotFound, err)
		return false
	}
	if cfg.UsesOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "OAuth channel API keys are read-only")
		return false
	}
	return true
}

// HandleDeleteAPIKey 删除渠道下的单个Key，并保持key_index连续
func (s *Server) HandleDeleteAPIKey(c *gin.Context) {
	// 解析渠道ID
	channelID, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	// 解析Key索引
	keyIndexStr := c.Param("keyIndex")
	keyIndex, err := strconv.Atoi(keyIndexStr)
	if err != nil || keyIndex < 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid key index")
		return
	}

	ctx := c.Request.Context()
	if !s.requireMutableAPIKeys(c, channelID) {
		return
	}

	// 获取当前Keys，确认目标存在并计算剩余数量
	apiKeys, err := s.store.GetAPIKeys(ctx, channelID)
	if err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}
	if len(apiKeys) == 0 {
		RespondErrorMsg(c, http.StatusNotFound, "channel has no keys")
		return
	}

	found := false
	for _, k := range apiKeys {
		if k.KeyIndex == keyIndex {
			found = true
			break
		}
	}
	if !found {
		RespondErrorMsg(c, http.StatusNotFound, "key not found")
		return
	}

	// 删除目标Key
	if err := s.store.DeleteAPIKey(ctx, channelID, keyIndex); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 紧凑索引，确保key_index连续
	if err := s.store.CompactKeyIndices(ctx, channelID, keyIndex); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	remaining := len(apiKeys) - 1

	// 失效缓存
	s.InvalidateAPIKeysCache(channelID)
	s.invalidateCooldownCache()

	RespondJSON(c, http.StatusOK, gin.H{
		"remaining_keys": remaining,
	})
}

// HandleAddModels 添加模型到渠道（去重）
// POST /admin/channels/:id/models
func (s *Server) HandleAddModels(c *gin.Context) {
	channelID, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	var req struct {
		Models []model.ModelEntry `json:"models" binding:"required,min=1"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request")
		return
	}

	ctx := c.Request.Context()
	cfg, err := s.store.GetConfig(ctx, channelID)
	if err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}

	// 验证模型条目（DRY: 使用 ModelEntry.Validate()）
	for i := range req.Models {
		if err := req.Models[i].Validate(); err != nil {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("models[%d]: %s", i, err.Error()))
			return
		}
	}

	// 去重合并（大小写不敏感，兼容 MySQL utf8mb4_general_ci 排序规则）
	existing := make(map[string]bool)
	for _, e := range cfg.ModelEntries {
		existing[strings.ToLower(e.Model)] = true
	}
	for _, e := range req.Models {
		key := strings.ToLower(e.Model)
		if !existing[key] {
			cfg.ModelEntries = append(cfg.ModelEntries, e)
			existing[key] = true
		}
	}

	if _, err := s.store.UpdateConfig(ctx, channelID, cfg); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	s.InvalidateAPIKeysCache(channelID)
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"total": len(cfg.ModelEntries)})
}

// HandleDeleteModels 删除渠道中的指定模型
// DELETE /admin/channels/:id/models
func (s *Server) HandleDeleteModels(c *gin.Context) {
	channelID, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	var req struct {
		Models []string `json:"models" binding:"required,min=1"` // 只需要模型名称列表
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request")
		return
	}

	ctx := c.Request.Context()
	cfg, err := s.store.GetConfig(ctx, channelID)
	if err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}

	// 过滤掉要删除的模型（大小写不敏感，兼容 MySQL utf8mb4_general_ci）
	toDelete := make(map[string]bool)
	for _, m := range req.Models {
		toDelete[strings.ToLower(m)] = true
	}
	remaining := make([]model.ModelEntry, 0, len(cfg.ModelEntries))
	for _, e := range cfg.ModelEntries {
		if !toDelete[strings.ToLower(e.Model)] {
			remaining = append(remaining, e)
		}
	}

	cfg.ModelEntries = remaining
	if _, err := s.store.UpdateConfig(ctx, channelID, cfg); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	s.InvalidateAPIKeysCache(channelID)
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"remaining": len(remaining)})
}

// HandleBatchUpdatePriority 批量更新渠道优先级
// POST /admin/channels/batch-priority
// 使用单条批量 UPDATE 语句更新多个渠道优先级
func (s *Server) HandleBatchUpdatePriority(c *gin.Context) {
	var req struct {
		Updates []struct {
			ID       int64 `json:"id"`
			Priority int   `json:"priority"`
		} `json:"updates"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	if len(req.Updates) == 0 {
		RespondError(c, http.StatusBadRequest, fmt.Errorf("updates cannot be empty"))
		return
	}

	ctx := c.Request.Context()

	// 转换为storage层的类型
	updates := make([]struct {
		ID       int64
		Priority int
	}, len(req.Updates))
	for i, u := range req.Updates {
		updates[i] = struct {
			ID       int64
			Priority int
		}{ID: u.ID, Priority: u.Priority}
	}

	// 调用storage层批量更新方法
	rowsAffected, err := s.store.BatchUpdatePriority(ctx, updates)
	if err != nil {
		log.Printf("批量优先级更新失败: %v", err)
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 清除缓存
	s.InvalidateChannelListCache()

	RespondJSON(c, http.StatusOK, gin.H{
		"updated": rowsAffected,
		"total":   len(req.Updates),
	})
}

// HandleBatchSetEnabled 批量启用/禁用渠道
// POST /admin/channels/batch-enabled
func (s *Server) HandleBatchSetEnabled(c *gin.Context) {
	var req struct {
		ChannelIDs []int64 `json:"channel_ids"`
		Enabled    *bool   `json:"enabled"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if req.Enabled == nil {
		RespondError(c, http.StatusBadRequest, fmt.Errorf("enabled is required"))
		return
	}

	channelIDs := normalizeBatchChannelIDs(req.ChannelIDs)
	if len(channelIDs) == 0 {
		RespondError(c, http.StatusBadRequest, fmt.Errorf("channel_ids cannot be empty"))
		return
	}

	ctx := c.Request.Context()
	updated := 0
	unchanged := 0
	notFound := make([]int64, 0)

	for _, channelID := range channelIDs {
		cfg, err := s.store.GetConfig(ctx, channelID)
		if err != nil {
			notFound = append(notFound, channelID)
			continue
		}

		if cfg.Enabled == *req.Enabled {
			unchanged++
			if *req.Enabled {
				s.clearAllChannelCooldowns(ctx, channelID)
			}
			continue
		}

		cfg.Enabled = *req.Enabled
		if _, err := s.store.UpdateChannelEnabled(ctx, channelID, *req.Enabled); err != nil {
			log.Printf("批量启用更新渠道 %d 失败: %v", channelID, err)
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		if *req.Enabled {
			s.clearAllChannelCooldowns(ctx, channelID)
		}
		updated++
	}

	if updated > 0 {
		s.InvalidateChannelListCache()
	}

	RespondJSON(c, http.StatusOK, gin.H{
		"enabled":         *req.Enabled,
		"total":           len(channelIDs),
		"updated":         updated,
		"unchanged":       unchanged,
		"not_found":       notFound,
		"not_found_count": len(notFound),
	})
}

// HandleBatchClearCooldowns 清除所选渠道的渠道、Key、模型和 URL 冷却状态。
// POST /admin/channels/batch-clear-cooldowns
func (s *Server) HandleBatchClearCooldowns(c *gin.Context) {
	var req struct {
		ChannelIDs []int64 `json:"channel_ids"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	channelIDs := normalizeBatchChannelIDs(req.ChannelIDs)
	if len(channelIDs) == 0 {
		RespondError(c, http.StatusBadRequest, fmt.Errorf("channel_ids cannot be empty"))
		return
	}

	ctx := c.Request.Context()
	cleared := 0
	notFound := make([]int64, 0)
	for _, channelID := range channelIDs {
		if _, err := s.store.GetConfig(ctx, channelID); err != nil {
			notFound = append(notFound, channelID)
			continue
		}
		if err := s.resetAllChannelCooldowns(ctx, channelID); err != nil {
			log.Printf("批量清除渠道 %d 冷却状态失败: %v", channelID, err)
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		cleared++
	}

	RespondJSON(c, http.StatusOK, gin.H{
		"total":           len(channelIDs),
		"cleared":         cleared,
		"not_found":       notFound,
		"not_found_count": len(notFound),
	})
}

// HandleBatchPatchChannels atomically applies advanced settings to selected channels.
// POST /admin/channels/batch-advanced
func (s *Server) HandleBatchPatchChannels(c *gin.Context) {
	var req struct {
		ChannelIDs            []int64            `json:"channel_ids"`
		Priority              *int               `json:"priority"`
		CostMultiplier        *float64           `json:"cost_multiplier"`
		DailyCostLimit        *float64           `json:"daily_cost_limit"`
		RPMLimit              *int               `json:"rpm_limit"`
		MaxConcurrency        *int               `json:"max_concurrency"`
		ProtocolTransformMode *string            `json:"protocol_transform_mode"`
		Models                []model.ModelEntry `json:"models"`
		ModelImportMode       string             `json:"model_import_mode"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	channelIDs := normalizeBatchChannelIDs(req.ChannelIDs)
	if len(channelIDs) == 0 {
		RespondError(c, http.StatusBadRequest, fmt.Errorf("channel_ids cannot be empty"))
		return
	}

	patch, err := (model.BatchConfigPatch{
		Priority:              req.Priority,
		CostMultiplier:        req.CostMultiplier,
		DailyCostLimit:        req.DailyCostLimit,
		RPMLimit:              req.RPMLimit,
		MaxConcurrency:        req.MaxConcurrency,
		ProtocolTransformMode: req.ProtocolTransformMode,
		ModelEntries:          req.Models,
		ModelImportMode:       req.ModelImportMode,
	}).Normalize()
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	result, err := s.store.BatchPatchConfigs(c.Request.Context(), channelIDs, patch)
	if err != nil {
		log.Printf("批量更新渠道高级配置失败: %v", err)
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if result.Updated > 0 {
		if patch.ModelImportMode != "" || patch.CostMultiplier != nil {
			for _, channelID := range channelIDs {
				s.InvalidateAPIKeysCache(channelID)
			}
		}
		s.InvalidateChannelListCache()
	}

	RespondJSON(c, http.StatusOK, gin.H{
		"total":           len(channelIDs),
		"updated":         result.Updated,
		"unchanged":       result.Unchanged,
		"not_found":       result.NotFound,
		"not_found_count": len(result.NotFound),
	})
}

// HandleBatchDeleteChannels 批量删除渠道
func (s *Server) HandleBatchDeleteChannels(c *gin.Context) {
	var req struct {
		ChannelIDs []int64 `json:"channel_ids"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	channelIDs := normalizeBatchChannelIDs(req.ChannelIDs)
	if len(channelIDs) == 0 {
		RespondError(c, http.StatusBadRequest, fmt.Errorf("channel_ids cannot be empty"))
		return
	}

	ctx := c.Request.Context()
	deleted := 0
	notFound := make([]int64, 0)

	for _, channelID := range channelIDs {
		wasDeleted, err := s.deleteChannelByID(ctx, channelID)
		if err != nil {
			log.Printf("批量删除渠道 %d 失败: %v", channelID, err)
			if finalizeErr := s.finalizeDeletedChannels(deleted); finalizeErr != nil {
				log.Printf("部分渠道已删除，但删除后状态同步失败: %v", finalizeErr)
				RespondError(c, http.StatusServiceUnavailable, finalizeErr)
				return
			}
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		if !wasDeleted {
			notFound = append(notFound, channelID)
			continue
		}
		deleted++
	}

	if err := s.finalizeDeletedChannels(deleted); err != nil {
		log.Printf("批量渠道已删除，但删除后状态同步失败: %v", err)
		RespondError(c, http.StatusServiceUnavailable, err)
		return
	}

	RespondJSON(c, http.StatusOK, gin.H{
		"total":           len(channelIDs),
		"deleted":         deleted,
		"not_found":       notFound,
		"not_found_count": len(notFound),
	})
}

func (s *Server) finalizeDeletedChannels(deleted int) error {
	if deleted == 0 {
		return nil
	}
	s.InvalidateChannelListCache()
	s.InvalidateAllAPIKeysCache()
	return s.reloadAuthTokensAfterChannelDeletion()
}

func normalizeBatchChannelIDs(rawIDs []int64) []int64 {
	if len(rawIDs) == 0 {
		return nil
	}

	seen := make(map[int64]struct{}, len(rawIDs))
	ids := make([]int64, 0, len(rawIDs))
	for _, id := range rawIDs {
		if id <= 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func (s *Server) reloadAuthTokensAfterChannelDeletion() error {
	if s.authService == nil {
		return nil
	}
	if err := s.authService.ReloadAuthTokens(); err != nil {
		return fmt.Errorf("reload API tokens after channel deletion: %w", err)
	}
	return nil
}

func (s *Server) deleteChannelByID(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, nil
	}

	cfg, err := s.store.GetConfig(ctx, id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}

	if err := s.store.DeleteConfig(ctx, id); err != nil {
		return false, err
	}
	s.removeDeletedChannelRuntimeState(cfg)
	return true, nil
}

func (s *Server) deleteChannelIfOAuthCredentialMatches(
	ctx context.Context,
	cfg *model.Config,
) (bool, error) {
	if cfg == nil || cfg.ID <= 0 || !cfg.UsesOAuth() || strings.TrimSpace(cfg.OAuthCredential) == "" {
		return false, nil
	}
	deleted, err := s.store.DeleteConfigIfOAuthSnapshotMatches(ctx, cfg)
	if err != nil || !deleted {
		return deleted, err
	}
	s.removeDeletedChannelRuntimeState(cfg)
	return true, nil
}

func (s *Server) disableChannelIfOAuthSnapshotMatches(
	ctx context.Context,
	cfg *model.Config,
) (bool, error) {
	if cfg == nil || cfg.ID <= 0 || !cfg.UsesOAuth() || strings.TrimSpace(cfg.OAuthCredential) == "" {
		return false, nil
	}
	disabled, err := s.store.DisableConfigIfOAuthSnapshotMatches(ctx, cfg)
	if err != nil || !disabled {
		return disabled, err
	}
	s.invalidateChannelRelatedCache(cfg.ID)
	s.InvalidateChannelListCache()
	return true, nil
}

func (s *Server) removeDeletedChannelRuntimeState(cfg *model.Config) {
	id := cfg.ID
	if s.keySelector != nil {
		s.keySelector.RemoveChannelCounter(id)
	}
	if s.urlSelector != nil {
		s.urlSelector.RemoveChannel(id)
	}
	if s.channelRPMLimiter != nil {
		s.channelRPMLimiter.RemoveChannel(id)
	}
	if s.codexCredentials != nil {
		s.codexCredentials.invalidate(id)
	}
	if s.antigravityCredentials != nil {
		s.antigravityCredentials.invalidate(id)
	}
	if cfg.UsesXAIOAuth() && s.xaiCredentials != nil {
		s.xaiCredentials.invalidate(id)
	}
	if s.anthropicCredentials != nil {
		s.anthropicCredentials.invalidate(id)
	}
	if s.zedCredentials != nil {
		s.zedCredentials.invalidate(id)
	}
}
