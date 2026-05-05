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

	"ccLoad/internal/model"
	"ccLoad/internal/util"

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

func (s *Server) handleListChannels(c *gin.Context) {
	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	now := time.Now()

	// 批量获取冷却状态（缓存优先）
	allChannelCooldowns, err := s.getAllChannelCooldowns(c.Request.Context())
	if err != nil {
		// 渠道冷却查询失败不影响主流程，仅记录错误
		log.Printf("[WARN] 批量查询渠道冷却状态失败: %v", err)
		allChannelCooldowns = make(map[int64]time.Time)
	}

	// 应用所有列表过滤（type / channel_name|search / status / model|model_like）
	// 注意：筛选下拉的全集走独立接口 /admin/channels/filter-options，
	// 这里只负责按所有筛选条件返回当前页，避免列表数据与下拉选项耦合。
	cfgs = applyChannelListFilters(cfgs, c, allChannelCooldowns, now)

	hasPagination := c.Query("limit") != "" || c.Query("offset") != ""

	// 批量查询所有Key冷却状态（缓存优先）
	allKeyCooldowns, err := s.getAllKeyCooldowns(c.Request.Context())
	if err != nil {
		// Key冷却查询失败不影响主流程，仅记录错误
		log.Printf("[WARN] 批量查询Key冷却状态失败: %v", err)
		allKeyCooldowns = make(map[int64]map[int]time.Time)
	}

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
		now:                 now,
		healthEnabled:       healthEnabled,
		priorityMap:         priorityMap,
		successRateMap:      successRateMap,
		channelCooldownsMap: allChannelCooldowns,
		keyCooldownsMap:     allKeyCooldowns,
		apiKeysMap:          allAPIKeys,
	}
	out := make([]ChannelWithCooldown, 0, len(cfgs))
	for _, cfg := range cfgs {
		out = append(out, ectx.enrichChannel(cfg))
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
//   - type: 渠道类型（标准化比较）
//   - channel_name | search: 名称精确/模糊（互斥，channel_name 优先）
//   - status: enabled / disabled / cooldown（cooldown 依赖 channelCooldownsMap）
//   - model | model_like: 模型精确/模糊（互斥，model 优先）
//
// 空字符串或 "all" 视为不过滤。
func applyChannelListFilters(cfgs []*model.Config, c *gin.Context, channelCooldownsMap map[int64]time.Time, now time.Time) []*model.Config {
	// type
	if t := c.Query("type"); t != "" && t != "all" {
		normalized := util.NormalizeChannelType(t)
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			return util.NormalizeChannelType(cfg.ChannelType) == normalized
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
				until, cooled := channelCooldownsMap[cfg.ID]
				return cooled && until.After(now)
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
		for _, cfg := range cfgs {
			stats := s.healthCache.GetHealthStats(cfg.ID)
			priorityMap[cfg.ID] = s.calculateEffectivePriority(cfg, stats, hcfg)
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
	now                 time.Time
	healthEnabled       bool
	priorityMap         map[int64]float64
	successRateMap      map[int64]float64
	channelCooldownsMap map[int64]time.Time
	keyCooldownsMap     map[int64]map[int]time.Time
	apiKeysMap          map[int64][]*model.APIKey
}

// enrichChannel 把单个 cfg 拼装为 ChannelWithCooldown：
// 渠道冷却剩余时间、健康度模式下的有效优先级与成功率、Key 策略与各 Key 冷却详情。
func (ectx *channelEnrichmentContext) enrichChannel(cfg *model.Config) ChannelWithCooldown {
	oc := ChannelWithCooldown{Config: cfg}

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
	return oc
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

	if t := c.Query("type"); t != "" && t != "all" {
		normalizedQueryType := util.NormalizeChannelType(t)
		filtered := make([]*model.Config, 0, len(cfgs))
		for _, cfg := range cfgs {
			if util.NormalizeChannelType(cfg.ChannelType) == normalizedQueryType {
				filtered = append(filtered, cfg)
			}
		}
		cfgs = filtered
	}

	if status := strings.TrimSpace(c.Query("status")); status != "" && status != "all" {
		now := time.Now()
		allChannelCooldowns, err := s.getAllChannelCooldowns(c.Request.Context())
		if err != nil {
			log.Printf("[WARN] 批量查询渠道冷却状态失败: %v", err)
			allChannelCooldowns = make(map[int64]time.Time)
		}
		filtered := make([]*model.Config, 0, len(cfgs))
		for _, cfg := range cfgs {
			switch status {
			case "enabled":
				if cfg.Enabled {
					filtered = append(filtered, cfg)
				}
			case "disabled":
				if !cfg.Enabled {
					filtered = append(filtered, cfg)
				}
			case "cooldown":
				if until, cooled := allChannelCooldowns[cfg.ID]; cooled && until.After(now) {
					filtered = append(filtered, cfg)
				}
			}
		}
		cfgs = filtered
	}

	nameSet := make(map[string]struct{}, len(cfgs))
	modelSet := make(map[string]struct{})
	for _, cfg := range cfgs {
		if name := strings.TrimSpace(cfg.Name); name != "" {
			nameSet[name] = struct{}{}
		}
		for _, entry := range cfg.ModelEntries {
			if entry.Model != "" {
				modelSet[entry.Model] = struct{}{}
			}
		}
	}

	channelNames := make([]string, 0, len(nameSet))
	for n := range nameSet {
		channelNames = append(channelNames, n)
	}
	sort.Strings(channelNames)

	models := make([]string, 0, len(modelSet))
	for m := range modelSet {
		models = append(models, m)
	}
	sort.Strings(models)

	RespondJSON(c, http.StatusOK, gin.H{
		"channel_names": channelNames,
		"models":        models,
	})
}

// HandleCheckDuplicateChannel 检测渠道是否与已有渠道重复
// POST /admin/channels/check-duplicate
// 判断条件：channel_type 相同 且 任意 URL 行与已有渠道任意 URL 行相交
func (s *Server) HandleCheckDuplicateChannel(c *gin.Context) {
	var req CheckDuplicateRequest
	if err := BindAndValidate(c, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	normalizedType := util.NormalizeChannelType(req.ChannelType)

	// 构建新渠道 URL 集合（去除空行）
	newURLSet := make(map[string]struct{}, len(req.URLs))
	for _, u := range req.URLs {
		u = strings.TrimSpace(u)
		if u != "" {
			newURLSet[u] = struct{}{}
		}
	}

	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	var duplicates []DuplicateChannelInfo
	for _, cfg := range cfgs {
		if util.NormalizeChannelType(cfg.ChannelType) != normalizedType {
			continue
		}
		// 遍历已有渠道的 URL 行，检查是否与新渠道 URL 有交集
		for line := range strings.SplitSeq(cfg.URL, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if _, ok := newURLSet[line]; ok {
				duplicates = append(duplicates, DuplicateChannelInfo{
					ID:          cfg.ID,
					Name:        cfg.Name,
					ChannelType: cfg.ChannelType,
					URL:         cfg.URL,
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

	// 创建渠道（不包含API Key）
	created, err := s.store.CreateConfig(c.Request.Context(), req.ToConfig())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 解析并创建API Keys
	apiKeys := util.ParseAPIKeys(req.APIKey)
	keyStrategy := strings.TrimSpace(req.KeyStrategy)
	if keyStrategy == "" {
		keyStrategy = model.KeyStrategySequential // 默认策略
	}

	now := time.Now()
	keysToCreate := make([]*model.APIKey, 0, len(apiKeys))
	for i, key := range apiKeys {
		keysToCreate = append(keysToCreate, &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    i,
			APIKey:      key,
			KeyStrategy: keyStrategy,
			CreatedAt:   model.JSONTime{Time: now},
			UpdatedAt:   model.JSONTime{Time: now},
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
	// 填充空的重定向模型为请求模型（方便前端编辑时显示）
	for i := range cfg.ModelEntries {
		if cfg.ModelEntries[i].RedirectModel == "" {
			cfg.ModelEntries[i].RedirectModel = cfg.ModelEntries[i].Model
		}
	}

	apiKeys, err := s.getAPIKeys(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 渠道详情返回配置和策略，但仍不返回明文 Key；API Keys 继续走 /keys 端点。
	RespondJSON(c, http.StatusOK, ChannelWithCooldown{
		Config:      cfg,
		KeyStrategy: channelKeyStrategy(apiKeys),
	})
}

// handleGetChannelKeys 获取渠道的所有 API Keys
// GET /admin/channels/{id}/keys
func (s *Server) handleGetChannelKeys(c *gin.Context, id int64) {
	apiKeys, err := s.getAPIKeys(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if apiKeys == nil {
		apiKeys = make([]*model.APIKey, 0)
	}
	RespondJSON(c, http.StatusOK, apiKeys)
}

// HandleChannelURLStats 返回多URL渠道各URL的实时状态（延迟、冷却）
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
	if len(urls) <= 1 || s.urlSelector == nil {
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
			// enabled 状态变更影响渠道选择，必须立即失效缓存
			s.InvalidateChannelListCache()
			RespondJSON(c, http.StatusOK, upd)
			return
		}
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

	if err := req.Validate(); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}

	// 检测api_key是否变化（需要重建API Keys）
	oldKeys, err := s.getAPIKeys(c.Request.Context(), id)
	if err != nil {
		log.Printf("[WARN] 查询旧API Keys失败: %v", err)
		oldKeys = []*model.APIKey{}
	}

	newKeys := util.ParseAPIKeys(req.APIKey)
	keyStrategy := strings.TrimSpace(req.KeyStrategy)
	if keyStrategy == "" {
		keyStrategy = model.KeyStrategySequential
	}

	// 比较Key数量和内容是否变化
	keyChanged := len(oldKeys) != len(newKeys)
	if !keyChanged {
		for i, oldKey := range oldKeys {
			if i >= len(newKeys) || oldKey.APIKey != newKeys[i] {
				keyChanged = true
				break
			}
		}
	}

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

	upd, err := s.store.UpdateConfig(c.Request.Context(), id, req.ToConfig())
	if err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}

	// Key或策略变化时更新API Keys
	if keyChanged {
		// Key内容/数量变化：删除旧Key并重建
		_ = s.store.DeleteAllAPIKeys(c.Request.Context(), id)

		// 批量创建新的API Keys（优化：单次事务插入替代循环单条插入）
		now := time.Now()
		apiKeys := make([]*model.APIKey, 0, len(newKeys))
		for i, key := range newKeys {
			apiKeys = append(apiKeys, &model.APIKey{
				ChannelID:   id,
				KeyIndex:    i,
				APIKey:      key,
				KeyStrategy: keyStrategy,
				CreatedAt:   model.JSONTime{Time: now},
				UpdatedAt:   model.JSONTime{Time: now},
			})
		}
		if err := s.store.CreateAPIKeysBatch(c.Request.Context(), apiKeys); err != nil {
			log.Printf("[WARN] 批量创建API Keys失败 (channel=%d, count=%d): %v", id, len(apiKeys), err)
		}
	} else if strategyChanged {
		// 仅策略变化：单条SQL批量更新所有Key的策略字段
		if err := s.store.UpdateAPIKeysStrategy(c.Request.Context(), id, keyStrategy); err != nil {
			log.Printf("[WARN] 批量更新API Key策略失败 (channel=%d): %v", id, err)
		}
	}

	// 清除渠道的冷却状态（编辑保存后重置冷却）
	// 设计原则: 清除失败不应影响渠道更新成功，但需要记录用于监控
	if s.cooldownManager != nil {
		if err := s.cooldownManager.ClearChannelCooldown(c.Request.Context(), id); err != nil {
			log.Printf("[WARN] 清除渠道冷却状态失败 (channel=%d): %v", id, err)
		}
	}
	// 冷却状态可能被更新，必须失效冷却缓存，避免前端立即刷新仍读到旧冷却状态
	s.invalidateCooldownCache()

	// 渠道更新后刷新缓存，确保选择器立即生效
	s.InvalidateChannelListCache()

	// Key变更时必须失效API Keys缓存，否则再次编辑会读到旧缓存
	if keyChanged || strategyChanged {
		s.InvalidateAPIKeysCache(id)
	}

	// URL 更新后立即清理失效的 URL 状态（内存+数据库同步）
	if s.urlSelector != nil {
		s.urlSelector.PruneChannel(id, upd.GetURLs())
	}
	// 同步清理数据库中已移除URL的禁用状态记录
	s.cleanupOrphanedURLStates(c.Request.Context(), id, upd.GetURLs())

	RespondJSON(c, http.StatusOK, upd)
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
		log.Printf("batch-priority: failed: %v", err)
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
			continue
		}

		cfg.Enabled = *req.Enabled
		if _, err := s.store.UpdateChannelEnabled(ctx, channelID, *req.Enabled); err != nil {
			log.Printf("batch-enabled: update channel %d failed: %v", channelID, err)
			RespondError(c, http.StatusInternalServerError, err)
			return
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
			log.Printf("batch-delete: delete channel %d failed: %v", channelID, err)
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		if !wasDeleted {
			notFound = append(notFound, channelID)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		s.InvalidateChannelListCache()
		// 同步失效所有 API Keys 缓存：批量删除涉及多个渠道，
		// 全量清空比逐个 InvalidateAPIKeysCache(id) 更便宜，且不会造成残留。
		s.InvalidateAllAPIKeysCache()
	}

	RespondJSON(c, http.StatusOK, gin.H{
		"total":           len(channelIDs),
		"deleted":         deleted,
		"not_found":       notFound,
		"not_found_count": len(notFound),
	})
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

func (s *Server) deleteChannelByID(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, nil
	}

	if _, err := s.store.GetConfig(ctx, id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}

	if err := s.store.DeleteConfig(ctx, id); err != nil {
		return false, err
	}
	if s.keySelector != nil {
		s.keySelector.RemoveChannelCounter(id)
	}
	if s.urlSelector != nil {
		s.urlSelector.RemoveChannel(id)
	}
	return true, nil
}
