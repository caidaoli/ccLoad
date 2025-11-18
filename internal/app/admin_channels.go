package app

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
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

	// 附带冷却状态
	now := time.Now()

	// 使用缓存层查询（<1ms vs 数据库查询5-10ms）
	// 性能优化：批量获取冷却状态，减少管理API的数据库查询
	allChannelCooldowns, err := s.getAllChannelCooldowns(c.Request.Context())
	if err != nil {
		// 渠道冷却查询失败不影响主流程，仅记录错误
		log.Printf("⚠️  警告: 批量查询渠道冷却状态失败: %v", err)
		allChannelCooldowns = make(map[int64]time.Time)
	}

	// 性能优化：批量查询所有Key冷却状态（一次查询替代 N*M 次）
	// 使用缓存层查询（<1ms vs 数据库查询5-10ms）
	// 性能优化：批量获取Key冷却状态，减少管理API的数据库查询
	allKeyCooldowns, err := s.getAllKeyCooldowns(c.Request.Context())
	if err != nil {
		// Key冷却查询失败不影响主流程，仅记录错误
		log.Printf("⚠️  警告: 批量查询Key冷却状态失败: %v", err)
		allKeyCooldowns = make(map[int64]map[int]time.Time)
	}

	// 批量查询所有API Keys（一次查询替代 N 次）
	var allAPIKeys map[int64][]*model.APIKey
	if sqliteStore, ok := s.store.(*sqlite.SQLiteStore); ok {
		allAPIKeys, err = sqliteStore.GetAllAPIKeys(c.Request.Context())
		if err != nil {
			log.Printf("⚠️  警告: 批量查询API Keys失败: %v", err)
			allAPIKeys = make(map[int64][]*model.APIKey) // 降级：使用空map
		}
	} else {
		// 兼容其他Store实现
		allAPIKeys = make(map[int64][]*model.APIKey)
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

		// 从预加载的map中获取API Keys（O(1)查找）
		apiKeys := allAPIKeys[cfg.ID]

		// ✅ 修复 (2025-10-11): 填充key_strategy字段（从第一个Key获取，所有Key的策略应该相同）
		if len(apiKeys) > 0 && apiKeys[0].KeyStrategy != "" {
			oc.KeyStrategy = apiKeys[0].KeyStrategy
		} else {
			oc.KeyStrategy = "sequential" // 默认值
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
		keyStrategy = "sequential" // 默认策略
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
			log.Printf("⚠️  警告: 创建API Key失败 (channel=%d, index=%d): %v", created.ID, i, err)
		}
	}

	// 新增、删除或更新渠道后，失效缓存保持一致性
	s.InvalidateChannelListCache()
	s.InvalidateAPIKeysCache(created.ID)
	s.invalidateCooldownCache()

	RespondJSON(c, http.StatusCreated, created)
}

func (s *Server) HandleChannelByID(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	// ✅ Linus风格：直接switch，删除不必要的抽象
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

	// ✅ 修复 (2025-10-11): 附带key_strategy信息
	// 使用缓存层查询（<1ms vs 数据库查询10-20ms）
	// 性能优化：管理API查询也使用缓存，减少延迟
	apiKeys, err := s.getAPIKeys(c.Request.Context(), id)
	if err != nil {
		log.Printf("⚠️  警告: 查询渠道 %d 的API Keys失败: %v", id, err)
	}

	// 构建响应（动态添加key_strategy字段）
	response := gin.H{
		"id":              cfg.ID,
		"name":            cfg.Name,
		"channel_type":    cfg.ChannelType,
		"url":             cfg.URL,
		"priority":        cfg.Priority,
		"models":          cfg.Models,
		"model_redirects": cfg.ModelRedirects,
		"enabled":         cfg.Enabled,
		"created_at":      cfg.CreatedAt,
		"updated_at":      cfg.UpdatedAt,
	}

	// 添加key_strategy（从第一个Key获取，所有Key的策略应该相同）
	if len(apiKeys) > 0 {
		response["key_strategy"] = apiKeys[0].KeyStrategy
		// 同时返回API Keys（逗号分隔）
		apiKeyStrs := make([]string, 0, len(apiKeys))
		for _, key := range apiKeys {
			apiKeyStrs = append(apiKeyStrs, key.APIKey)
		}
		response["api_key"] = strings.Join(apiKeyStrs, ",")
	} else {
		response["key_strategy"] = "sequential" // 默认值
		response["api_key"] = ""
	}

	RespondJSON(c, http.StatusOK, response)
}

// ✅ 修复:获取渠道的所有 API Keys(2025-10 新架构支持)
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
		log.Printf("⚠️  警告: 查询旧API Keys失败: %v", err)
		oldKeys = []*model.APIKey{}
	}

	newKeys := util.ParseAPIKeys(req.APIKey)
	keyStrategy := strings.TrimSpace(req.KeyStrategy)
	if keyStrategy == "" {
		keyStrategy = "sequential"
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

	// ✅ 修复 (2025-10-11): 检测策略变化
	strategyChanged := false
	if !keyChanged && len(oldKeys) > 0 && len(newKeys) > 0 {
		// Key内容未变化时，检查策略是否变化
		oldStrategy := oldKeys[0].KeyStrategy
		if oldStrategy == "" {
			oldStrategy = "sequential"
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
				log.Printf("⚠️  警告: 创建API Key失败 (channel=%d, index=%d): %v", id, i, err)
			}
		}
	} else if strategyChanged {
		// 仅策略变化：高效更新所有Key的策略字段（无需删除重建）
		now := time.Now()
		for _, oldKey := range oldKeys {
			oldKey.KeyStrategy = keyStrategy
			oldKey.UpdatedAt = model.JSONTime{Time: now}
			if err := s.store.UpdateAPIKey(c.Request.Context(), oldKey); err != nil {
				log.Printf("⚠️  警告: 更新API Key策略失败 (channel=%d, index=%d): %v", id, oldKey.KeyIndex, err)
			}
		}
	}

	// 渠道更新后刷新缓存，避免返回陈旧数据
	s.InvalidateChannelListCache()
	s.InvalidateAPIKeysCache(id)
	s.invalidateCooldownCache()

	RespondJSON(c, http.StatusOK, upd)
}

// 删除渠道
func (s *Server) handleDeleteChannel(c *gin.Context, id int64) {
	if err := s.store.DeleteConfig(c.Request.Context(), id); err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}
	// 删除渠道后刷新缓存
	s.InvalidateChannelListCache()
	s.InvalidateAPIKeysCache(id)
	s.invalidateCooldownCache()
	// 数据库级联删除会自动清理冷却数据（无需手动清理缓存）
	c.Status(http.StatusNoContent)
}
