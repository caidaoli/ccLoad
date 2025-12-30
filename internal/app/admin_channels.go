package app

import (
	"fmt"
	"log"
	"net/http"
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

// 获取渠道列表
// 使用批量查询优化N+1问题
func (s *Server) handleListChannels(c *gin.Context) {
	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 支持按渠道类型过滤（性能优化：减少后续批量查询的数据量）
	// [FIX] P2-7: 标准化类型比较，避免"同一概念多种写法"
	channelType := c.Query("type")
	if channelType != "" && channelType != "all" {
		// 标准化查询参数（统一转小写）
		normalizedQueryType := util.NormalizeChannelType(channelType)

		filtered := make([]*model.Config, 0, len(cfgs))
		for _, cfg := range cfgs {
			// 标准化 Config 中的类型（统一转小写）
			normalizedCfgType := util.NormalizeChannelType(cfg.ChannelType)

			if normalizedCfgType == normalizedQueryType {
				filtered = append(filtered, cfg)
			}
		}
		cfgs = filtered
	}

	// 附带冷却状态
	now := time.Now()

	// 使用缓存层查询（<1ms vs 数据库查询5-10ms）
	// 性能优化：批量获取冷却状态，减少管理API的数据库查询
	allChannelCooldowns, err := s.getAllChannelCooldowns(c.Request.Context())
	if err != nil {
		// 渠道冷却查询失败不影响主流程，仅记录错误
		log.Printf("[WARN] 批量查询渠道冷却状态失败: %v", err)
		allChannelCooldowns = make(map[int64]time.Time)
	}

	// 性能优化：批量查询所有Key冷却状态（一次查询替代 N*M 次）
	// 使用缓存层查询（<1ms vs 数据库查询5-10ms）
	// 性能优化：批量获取Key冷却状态，减少管理API的数据库查询
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

	// 健康度模式：获取成功率数据
	var successRates map[int64]float64
	healthEnabled := s.healthCache != nil && s.healthCache.Config().Enabled
	if healthEnabled {
		successRates = s.healthCache.GetAllSuccessRates()
	}

	out := make([]ChannelWithCooldown, 0, len(cfgs))
	for _, cfg := range cfgs {
		oc := ChannelWithCooldown{Config: cfg}

		// 渠道级别冷却：使用批量查询结果（性能提升：N -> 1 次查询）
		if until, cooled := allChannelCooldowns[cfg.ID]; cooled && until.After(now) {
			oc.CooldownUntil = &until
			cooldownRemainingMS := int64(until.Sub(now) / time.Millisecond)
			oc.CooldownRemainingMS = cooldownRemainingMS
		}

		// 健康度模式：计算有效优先级和成功率
		if healthEnabled {
			rate := 1.0
			if successRates != nil {
				if v, exists := successRates[cfg.ID]; exists {
					rate = v
					oc.SuccessRate = &v
				}
			}
			effPriority := s.calculateEffectivePriority(cfg, rate, s.healthCache.Config())
			oc.EffectivePriority = &effPriority
		}

		// 从预加载的map中获取API Keys（O(1)查找）
		apiKeys := allAPIKeys[cfg.ID]

		// [INFO] 修复 (2025-10-11): 填充key_strategy字段（从第一个Key获取，所有Key的策略应该相同）
		if len(apiKeys) > 0 && apiKeys[0].KeyStrategy != "" {
			oc.KeyStrategy = apiKeys[0].KeyStrategy
		} else {
			oc.KeyStrategy = model.KeyStrategySequential // 默认值
		}

		keyCooldowns := make([]KeyCooldownInfo, 0, len(apiKeys))

		// 从批量查询结果中获取该渠道的所有Key冷却状态
		channelKeyCooldowns := allKeyCooldowns[cfg.ID]

		for _, apiKey := range apiKeys {
			keyInfo := KeyCooldownInfo{KeyIndex: apiKey.KeyIndex}

			// 检查是否在冷却中
			if until, cooled := channelKeyCooldowns[apiKey.KeyIndex]; cooled && until.After(now) {
				u := until
				keyInfo.CooldownUntil = &u
				keyInfo.CooldownRemainingMS = int64(until.Sub(now) / time.Millisecond)
			}

			keyCooldowns = append(keyCooldowns, keyInfo)
		}
		oc.KeyCooldowns = keyCooldowns

		out = append(out, oc)
	}

	// 健康度模式：按有效优先级降序排序（与请求路由一致）
	if healthEnabled {
		sort.Slice(out, func(i, j int) bool {
			pi, pj := float64(0), float64(0)
			if out[i].EffectivePriority != nil {
				pi = *out[i].EffectivePriority
			}
			if out[j].EffectivePriority != nil {
				pj = *out[j].EffectivePriority
			}
			return pi > pj
		})
	}

	// 填充空的重定向模型为请求模型（方便前端编辑时显示）
	for i := range out {
		for j := range out[i].Config.ModelEntries {
			if out[i].Config.ModelEntries[j].RedirectModel == "" {
				out[i].Config.ModelEntries[j].RedirectModel = out[i].Config.ModelEntries[j].Model
			}
		}
	}

	RespondJSON(c, http.StatusOK, out)
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
	for i, key := range apiKeys {
		apiKey := &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    i,
			APIKey:      key,
			KeyStrategy: keyStrategy,
			CreatedAt:   model.JSONTime{Time: now},
			UpdatedAt:   model.JSONTime{Time: now},
		}
		if err := s.store.CreateAPIKey(c.Request.Context(), apiKey); err != nil {
			log.Printf("[WARN] 创建API Key失败 (channel=%d, index=%d): %v", created.ID, i, err)
		}
	}

	// 新增、删除或更新渠道后，失效缓存保持一致性
	s.invalidateChannelRelatedCache(created.ID)

	RespondJSON(c, http.StatusCreated, created)
}

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
	// 渠道详情仅返回配置本身；API Keys 通过 /admin/channels/:id/keys 单独获取（避免无意泄漏明文Key）。
	RespondJSON(c, http.StatusOK, cfg)
}

// [INFO] 修复:获取渠道的所有 API Keys(2025-10 新架构支持)
// 使用缓存层查询（<1ms vs 数据库查询10-20ms）
// GET /admin/channels/{id}/keys
func (s *Server) handleGetChannelKeys(c *gin.Context, id int64) {
	apiKeys, err := s.getAPIKeys(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, apiKeys)
}

// 更新渠道
func (s *Server) handleUpdateChannel(c *gin.Context, id int64) {
	// 先获取现有配置
	existing, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}

	// 解析请求为通用map以支持部分更新
	var rawReq map[string]any
	if err := c.ShouldBindJSON(&rawReq); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request format")
		return
	}

	// 检查是否为简单的enabled字段更新
	if len(rawReq) == 1 {
		if enabled, ok := rawReq["enabled"].(bool); ok {
			existing.Enabled = enabled
			upd, err := s.store.UpdateConfig(c.Request.Context(), id, existing)
			if err != nil {
				RespondError(c, http.StatusInternalServerError, err)
				return
			}
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
	// 使用缓存层查询（<1ms vs 数据库查询10-20ms）
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

		// 创建新的API Keys
		now := time.Now()
		for i, key := range newKeys {
			apiKey := &model.APIKey{
				ChannelID:   id,
				KeyIndex:    i,
				APIKey:      key,
				KeyStrategy: keyStrategy,
				CreatedAt:   model.JSONTime{Time: now},
				UpdatedAt:   model.JSONTime{Time: now},
			}
			if err := s.store.CreateAPIKey(c.Request.Context(), apiKey); err != nil {
				log.Printf("[WARN] 创建API Key失败 (channel=%d, index=%d): %v", id, i, err)
			}
		}
	} else if strategyChanged {
		// 仅策略变化：高效更新所有Key的策略字段（无需删除重建）
		now := time.Now()
		for _, oldKey := range oldKeys {
			oldKey.KeyStrategy = keyStrategy
			oldKey.UpdatedAt = model.JSONTime{Time: now}
			if err := s.store.UpdateAPIKey(c.Request.Context(), oldKey); err != nil {
				log.Printf("[WARN] 更新API Key策略失败 (channel=%d, index=%d): %v", id, oldKey.KeyIndex, err)
			}
		}
	}

	// 清除渠道的冷却状态（编辑保存后重置冷却）
	// 设计原则: 清除失败不应影响渠道更新成功，但需要记录用于监控
	if s.cooldownManager != nil {
		if err := s.cooldownManager.ClearChannelCooldown(c.Request.Context(), id); err != nil {
			log.Printf("[WARN] 清除渠道冷却状态失败 (channel=%d): %v", id, err)
		}
	}

	// 渠道更新后刷新缓存，避免返回陈旧数据
	s.invalidateChannelRelatedCache(id)

	RespondJSON(c, http.StatusOK, upd)
}

// 删除渠道
func (s *Server) handleDeleteChannel(c *gin.Context, id int64) {
	if err := s.store.DeleteConfig(c.Request.Context(), id); err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}
	// 删除渠道对应的轮询计数器，避免KeySelector内部状态泄漏
	if s.keySelector != nil {
		s.keySelector.RemoveChannelCounter(id)
	}
	// 删除渠道后刷新缓存
	s.invalidateChannelRelatedCache(id)
	// 数据库级联删除会自动清理冷却数据（无需手动清理缓存）
	RespondJSON(c, http.StatusOK, gin.H{"id": id})
}

// 删除渠道下的单个Key，并保持key_index连续
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

	// 去重合并（使用规范化后的值）
	existing := make(map[string]bool)
	for _, e := range cfg.ModelEntries {
		existing[e.Model] = true
	}
	for _, e := range req.Models {
		if !existing[e.Model] {
			cfg.ModelEntries = append(cfg.ModelEntries, e)
			existing[e.Model] = true
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

	// 过滤掉要删除的模型
	toDelete := make(map[string]bool)
	for _, m := range req.Models {
		toDelete[m] = true
	}
	remaining := make([]model.ModelEntry, 0, len(cfg.ModelEntries))
	for _, e := range cfg.ModelEntries {
		if !toDelete[e.Model] {
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

// 批量更新渠道优先级
// POST /admin/channels/batch-priority
// 性能优化：使用单条批量UPDATE语句替代N次独立查询（90次→1次）
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
