package main

import (
    "bytes"
    "context"
    "encoding/csv"
    "fmt"
    "log"
    "io"
    "net/http"
    "strconv"
    "strings"
    "time"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// ChannelRequest 渠道创建/更新请求结构
type ChannelRequest struct {
	Name           string            `json:"name" binding:"required"`
	APIKey         string            `json:"api_key" binding:"required"`
	ChannelType    string            `json:"channel_type,omitempty"` // 渠道类型：anthropic, codex, gemini
	KeyStrategy    string            `json:"key_strategy,omitempty"` // Key使用策略：sequential, round_robin
	URL            string            `json:"url" binding:"required,url"`
	Priority       int               `json:"priority"`
	Models         []string          `json:"models" binding:"required,min=1"`
	ModelRedirects map[string]string `json:"model_redirects,omitempty"` // 可选的模型重定向映射
	Enabled        bool              `json:"enabled"`
}

// Validate 实现RequestValidator接口
func (cr *ChannelRequest) Validate() error {
	if strings.TrimSpace(cr.Name) == "" {
		return fmt.Errorf("name cannot be empty")
	}
	if strings.TrimSpace(cr.APIKey) == "" {
		return fmt.Errorf("api_key cannot be empty")
	}
	if len(cr.Models) == 0 {
		return fmt.Errorf("models cannot be empty")
	}
	return nil
}

// ToConfig 转换为Config结构
func (cr *ChannelRequest) ToConfig() *Config {
	return &Config{
		Name:           strings.TrimSpace(cr.Name),
		APIKey:         strings.TrimSpace(cr.APIKey),
		ChannelType:    strings.TrimSpace(cr.ChannelType), // 传递渠道类型
		KeyStrategy:    strings.TrimSpace(cr.KeyStrategy), // 传递Key使用策略
		URL:            strings.TrimSpace(cr.URL),
		Priority:       cr.Priority,
		Models:         cr.Models,
		ModelRedirects: cr.ModelRedirects,
		Enabled:        cr.Enabled,
	}
}

// ChannelWithCooldown 带冷却状态的渠道响应结构
// KeyCooldownInfo Key级别冷却信息
type KeyCooldownInfo struct {
	KeyIndex            int        `json:"key_index"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	CooldownRemainingMS int64      `json:"cooldown_remaining_ms,omitempty"`
}

type ChannelWithCooldown struct {
	*Config
	CooldownUntil       *time.Time        `json:"cooldown_until,omitempty"`
	CooldownRemainingMS int64             `json:"cooldown_remaining_ms,omitempty"`
	KeyCooldowns        []KeyCooldownInfo `json:"key_cooldowns,omitempty"`
}

// ChannelImportSummary 导入结果统计
type ChannelImportSummary struct {
	Created   int      `json:"created"`
	Updated   int      `json:"updated"`
	Skipped   int      `json:"skipped"`
	Processed int      `json:"processed"`
	Errors    []string `json:"errors,omitempty"`
	// Redis同步相关字段 (OCP: 开放扩展)
	RedisSyncEnabled    bool   `json:"redis_sync_enabled"`              // Redis同步是否启用
	RedisSyncSuccess    bool   `json:"redis_sync_success,omitempty"`    // Redis同步是否成功
	RedisSyncError      string `json:"redis_sync_error,omitempty"`      // Redis同步错误信息
	RedisSyncedChannels int    `json:"redis_synced_channels,omitempty"` // 成功同步到Redis的渠道数量
}

// Admin: /admin/channels (GET, POST) - 重构版本
func (s *Server) handleChannels(c *gin.Context) {
	router := NewMethodRouter().
		GET(s.handleListChannels).
		POST(s.handleCreateChannel)

	router.Handle(c)
}

// 获取渠道列表
// P1修复 (2025-10-05): 使用批量查询优化N+1问题
func (s *Server) handleListChannels(c *gin.Context) {
	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 附带冷却状态
	now := time.Now()

	// P0性能优化：批量查询所有渠道冷却状态（一次查询替代 N 次）
    allChannelCooldowns, err := s.store.GetAllChannelCooldowns(c.Request.Context())
    if err != nil {
        // 渠道冷却查询失败不影响主流程，仅记录错误
        log.Printf("⚠️  警告: 批量查询渠道冷却状态失败: %v", err)
        allChannelCooldowns = make(map[int64]time.Time)
    }

	// 性能优化：批量查询所有Key冷却状态（一次查询替代 N*M 次）
    allKeyCooldowns, err := s.store.GetAllKeyCooldowns(c.Request.Context())
    if err != nil {
        // Key冷却查询失败不影响主流程，仅记录错误
        log.Printf("⚠️  警告: 批量查询Key冷却状态失败: %v", err)
        allKeyCooldowns = make(map[int64]map[int]time.Time)
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

		// Key级别冷却：使用批量查询结果（性能提升：N*M -> 1 次查询）
		keys := cfg.GetAPIKeys()
		keyCooldowns := make([]KeyCooldownInfo, 0, len(keys))

		// 从批量查询结果中获取该渠道的所有Key冷却状态
		channelKeyCooldowns := allKeyCooldowns[cfg.ID]

		for i := range keys {
			keyInfo := KeyCooldownInfo{KeyIndex: i}

			// 检查是否在冷却中
			if until, cooled := channelKeyCooldowns[i]; cooled && until.After(now) {
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

	created, err := s.store.CreateConfig(c.Request.Context(), req.ToConfig())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	RespondJSON(c, http.StatusCreated, created)
}

// 导出渠道为CSV
func (s *Server) handleExportChannelsCSV(c *gin.Context) {
	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	buf := &bytes.Buffer{}
	// 添加 UTF-8 BOM，兼容 Excel 等工具
	buf.WriteString("\ufeff")

	writer := csv.NewWriter(buf)
	defer writer.Flush()

	header := []string{"id", "name", "api_key", "url", "priority", "models", "model_redirects", "channel_type", "key_strategy", "enabled"}
	if err := writer.Write(header); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	for _, cfg := range cfgs {
		// 序列化模型重定向为JSON字符串
		modelRedirectsJSON := "{}"
		if len(cfg.ModelRedirects) > 0 {
			if jsonBytes, err := sonic.Marshal(cfg.ModelRedirects); err == nil {
				modelRedirectsJSON = string(jsonBytes)
			}
		}

		record := []string{
			strconv.FormatInt(cfg.ID, 10),
			cfg.Name,
			cfg.APIKey,
			cfg.URL,
			strconv.Itoa(cfg.Priority),
			strings.Join(cfg.Models, ","),
			modelRedirectsJSON,
			cfg.GetChannelType(), // 使用GetChannelType确保默认值
			cfg.GetKeyStrategy(), // 使用GetKeyStrategy确保默认值
			strconv.FormatBool(cfg.Enabled),
		}
		if err := writer.Write(record); err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	filename := fmt.Sprintf("channels-%s.csv", time.Now().Format("20060102-150405"))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	c.Header("Cache-Control", "no-cache")
	c.String(http.StatusOK, buf.String())
}

// 导入渠道CSV
func (s *Server) handleImportChannelsCSV(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "缺少上传文件")
		return
	}

	src, err := fileHeader.Open()
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	defer src.Close()

	reader := csv.NewReader(src)
	reader.TrimLeadingSpace = true

	headerRow, err := reader.Read()
	if err == io.EOF {
		RespondErrorMsg(c, http.StatusBadRequest, "CSV内容为空")
		return
	}
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	columnIndex := buildCSVColumnIndex(headerRow)
	required := []string{"name", "api_key", "url", "models"}
	for _, key := range required {
		if _, ok := columnIndex[key]; !ok {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("缺少必需列: %s", key))
			return
		}
	}

	summary := ChannelImportSummary{}
	lineNo := 1

	// 预加载现有渠道名称，O(n) 替代 O(n^2)（KISS/DRY/性能优化）
	existingConfigs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	existingNames := make(map[string]struct{}, len(existingConfigs))
	for _, ec := range existingConfigs {
		existingNames[ec.Name] = struct{}{}
	}

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		lineNo++

		if err != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行读取失败: %v", lineNo, err))
			summary.Skipped++
			continue
		}

		if isCSVRecordEmpty(record) {
			summary.Skipped++
			continue
		}

		fetch := func(key string) string {
			idx, ok := columnIndex[key]
			if !ok || idx >= len(record) {
				return ""
			}
			return strings.TrimSpace(record[idx])
		}

		name := fetch("name")
		apiKey := fetch("api_key")
		url := fetch("url")
		modelsRaw := fetch("models")
		modelRedirectsRaw := fetch("model_redirects")
		channelType := fetch("channel_type")
		keyStrategy := fetch("key_strategy")

		if name == "" || apiKey == "" || url == "" || modelsRaw == "" {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行缺少必填字段", lineNo))
			summary.Skipped++
			continue
		}

		// 渠道类型规范化与校验（openai → codex，空值 → anthropic）
		channelType = normalizeChannelType(channelType)
		if !IsValidChannelType(channelType) {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行渠道类型无效: %s（仅支持anthropic/codex/gemini）", lineNo, channelType))
			summary.Skipped++
			continue
		}

		// 验证Key使用策略（可选字段，默认sequential）
		if keyStrategy == "" {
			keyStrategy = "sequential" // 默认值
		} else if keyStrategy != "sequential" && keyStrategy != "round_robin" {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行Key使用策略无效: %s（仅支持sequential/round_robin）", lineNo, keyStrategy))
			summary.Skipped++
			continue
		}

		models := parseImportModels(modelsRaw)
		if len(models) == 0 {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行模型格式无效", lineNo))
			summary.Skipped++
			continue
		}

		// 解析模型重定向（可选字段）
		var modelRedirects map[string]string
		if modelRedirectsRaw != "" && modelRedirectsRaw != "{}" {
			if err := sonic.Unmarshal([]byte(modelRedirectsRaw), &modelRedirects); err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行模型重定向格式错误: %v", lineNo, err))
				summary.Skipped++
				continue
			}
		}

		priority := 0
		if pRaw := fetch("priority"); pRaw != "" {
			p, err := strconv.Atoi(pRaw)
			if err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行优先级格式错误: %v", lineNo, err))
				summary.Skipped++
				continue
			}
			priority = p
		}

		enabled := true
		if eRaw := fetch("enabled"); eRaw != "" {
			if val, ok := parseImportEnabled(eRaw); ok {
				enabled = val
			} else {
				summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行启用状态格式错误: %s", lineNo, eRaw))
				summary.Skipped++
				continue
			}
		}

		cfg := &Config{
			Name:           name,
			APIKey:         apiKey,
			URL:            url,
			Priority:       priority,
			Models:         models,
			ModelRedirects: modelRedirects,
			ChannelType:    channelType,
			KeyStrategy:    keyStrategy,
			Enabled:        enabled,
		}

		// CSV导入特殊处理：确保APIKeys字段被正确设置
		// 因为CSV只有api_key列，需要调用normalizeAPIKeys转换
		// 这样ReplaceConfig才能序列化正确的JSON数组
		normalizeAPIKeys(cfg)

		// 检查渠道是否已存在（基于名称）- 使用预加载集合
		_, isUpdate := existingNames[name]

		// 使用ReplaceConfig进行插入或更新
		if _, err := s.store.ReplaceConfig(c.Request.Context(), cfg); err != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行处理失败: %v", lineNo, err))
			summary.Skipped++
			continue
		}

		if isUpdate {
			summary.Updated++
		} else {
			summary.Created++
			// 新创建的渠道加入已存在集合，避免后续重复计算
			existingNames[name] = struct{}{}
		}
	}

	summary.Processed = summary.Created + summary.Updated + summary.Skipped

	// 导入完成后，批量同步所有渠道到Redis (DRY: 避免逐个同步的重复操作)
	summary.RedisSyncEnabled = false
	if sqliteStore, ok := s.store.(*SQLiteStore); ok && sqliteStore.redisSync.IsEnabled() {
		summary.RedisSyncEnabled = true

		// 批量同步所有渠道到Redis
		if err := sqliteStore.SyncAllChannelsToRedis(c.Request.Context()); err != nil {
			summary.RedisSyncSuccess = false
			summary.RedisSyncError = fmt.Sprintf("Redis同步失败: %v", err)
			// Redis同步失败不影响导入结果，仅记录错误
		} else {
			summary.RedisSyncSuccess = true
			// 获取当前渠道总数作为同步数量
			if configs, err := s.store.ListConfigs(c.Request.Context()); err == nil {
				summary.RedisSyncedChannels = len(configs)
			}
		}
	}

	RespondJSON(c, http.StatusOK, summary)
}

// Admin: /admin/channels/{id} (GET, PUT, DELETE) - 重构版本
func (s *Server) handleChannelByID(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	router := NewMethodRouter().
		GET(func(c *gin.Context) { s.handleGetChannel(c, id) }).
		PUT(func(c *gin.Context) { s.handleUpdateChannel(c, id) }).
		DELETE(func(c *gin.Context) { s.handleDeleteChannel(c, id) })

	router.Handle(c)
}

// 获取单个渠道
func (s *Server) handleGetChannel(c *gin.Context, id int64) {
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}
	RespondJSON(c, http.StatusOK, cfg)
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

	// 检测api_key是否变化（防止Key索引错位）
	keyChanged := existing.APIKey != req.APIKey

	upd, err := s.store.UpdateConfig(c.Request.Context(), id, req.ToConfig())
	if err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}

	// Key变化时清理所有key冷却数据（避免索引错位）
	if keyChanged {
        if err := s.store.ClearAllKeyCooldowns(c.Request.Context(), id); err != nil {
            // 清理失败仅记录警告，不影响更新流程
            log.Printf("warning: failed to clear key cooldowns for channel %d: %v", id, err)
        }
	}

	RespondJSON(c, http.StatusOK, upd)
}

// 删除渠道
func (s *Server) handleDeleteChannel(c *gin.Context, id int64) {
	if err := s.store.DeleteConfig(c.Request.Context(), id); err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}
	// 数据库级联删除会自动清理冷却数据（无需手动清理缓存）
	c.Status(http.StatusNoContent)
}

// Admin: /admin/errors?hours=24&limit=100&offset=0 - 重构版本
func (s *Server) handleErrors(c *gin.Context) {
	params := ParsePaginationParams(c)
	lf := BuildLogFilter(c)

	since := params.GetSinceTime()
	logs, err := s.store.ListLogs(c.Request.Context(), since, params.Limit, params.Offset, &lf)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	RespondJSON(c, http.StatusOK, logs)
}

// Admin: /admin/metrics?hours=24&bucket_min=5 - 重构版本
func (s *Server) handleMetrics(c *gin.Context) {
	params := ParsePaginationParams(c)
	bucketMin, _ := strconv.Atoi(c.DefaultQuery("bucket_min", "5"))
	if bucketMin <= 0 {
		bucketMin = 5
	}

	since := params.GetSinceTime()
	pts, err := s.store.Aggregate(c.Request.Context(), since, time.Duration(bucketMin)*time.Minute)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 添加调试信息
	totalReqs := 0
	for _, pt := range pts {
		totalReqs += pt.Success + pt.Error
	}

	c.Header("X-Debug-Since", since.Format(time.RFC3339))
	c.Header("X-Debug-Points", fmt.Sprintf("%d", len(pts)))
	c.Header("X-Debug-Total", fmt.Sprintf("%d", totalReqs))

	RespondJSON(c, http.StatusOK, pts)
}

// Admin: /admin/stats?hours=24&channel_name_like=xxx&model_like=xxx - 重构版本
func (s *Server) handleStats(c *gin.Context) {
	params := ParsePaginationParams(c)
	lf := BuildLogFilter(c)

	since := params.GetSinceTime()
	stats, err := s.store.GetStats(c.Request.Context(), since, &lf)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	RespondJSON(c, http.StatusOK, gin.H{"stats": stats})
}

// Public: /public/summary 基础请求统计（不需要身份验证）- 重构版本
func (s *Server) handlePublicSummary(c *gin.Context) {
	params := ParsePaginationParams(c)
	since := params.GetSinceTime()
	stats, err := s.store.GetStats(c.Request.Context(), since, nil) // 不使用过滤条件
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 计算总体统计
	totalSuccess := 0
	totalError := 0
	totalChannels := make(map[string]bool)
	totalModels := make(map[string]bool)

	for _, stat := range stats {
		totalSuccess += stat.Success
		totalError += stat.Error
		totalChannels[stat.ChannelName] = true
		totalModels[stat.Model] = true
	}

	response := gin.H{
		"total_requests":   totalSuccess + totalError,
		"success_requests": totalSuccess,
		"error_requests":   totalError,
		"active_channels":  len(totalChannels),
		"active_models":    len(totalModels),
		"hours":            params.Hours,
	}

	RespondJSON(c, http.StatusOK, response)
}

// handleCooldownStats 获取当前冷却状态监控指标（P2优化）
// GET /admin/cooldown/stats
// 返回：当前活跃的渠道级和Key级冷却数量
func (s *Server) handleCooldownStats(c *gin.Context) {
	response := gin.H{
		"channel_cooldowns": s.channelCooldownGauge.Load(),
		"key_cooldowns":     s.keyCooldownGauge.Load(),
	}
	RespondJSON(c, http.StatusOK, response)
}

// TestChannelRequest 渠道测试请求结构
type TestChannelRequest struct {
	Model       string            `json:"model" binding:"required"`
	MaxTokens   int               `json:"max_tokens,omitempty"`   // 可选，默认512
	Stream      bool              `json:"stream,omitempty"`       // 可选，流式响应
	Content     string            `json:"content,omitempty"`      // 可选，测试内容，默认"test"
	Headers     map[string]string `json:"headers,omitempty"`      // 可选，自定义请求头
	ChannelType string            `json:"channel_type,omitempty"` // 可选，渠道类型：anthropic(默认)、codex、gemini
	KeyIndex    int               `json:"key_index,omitempty"`    // 可选，指定测试的Key索引，默认0（第一个）
}

// Validate 实现RequestValidator接口
func (tcr *TestChannelRequest) Validate() error {
	if strings.TrimSpace(tcr.Model) == "" {
		return fmt.Errorf("model cannot be empty")
	}
	return nil
}

// Admin: /admin/channels/{id}/test (POST) - 重构版本
func (s *Server) handleChannelTest(c *gin.Context) {
	// 解析渠道ID
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	// 解析请求体
	var testReq TestChannelRequest
	if err := BindAndValidate(c, &testReq); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// 获取渠道配置
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}

	// 解析 API Keys（支持多 Key）
	keys := ParseAPIKeys(cfg.APIKey)
	if len(keys) == 0 {
		RespondJSON(c, http.StatusOK, gin.H{
			"success": false,
			"error":   "渠道未配置有效的 API Key",
		})
		return
	}

	// 验证并选择 Key 索引
	keyIndex := testReq.KeyIndex
	if keyIndex < 0 || keyIndex >= len(keys) {
		keyIndex = 0 // 默认使用第一个 Key
	}

	// 创建测试用的配置副本，使用选定的 Key
	testCfg := *cfg
	testCfg.APIKey = keys[keyIndex]

	// 检查模型是否支持
	modelSupported := false
	for _, model := range testCfg.Models {
		if model == testReq.Model {
			modelSupported = true
			break
		}
	}
	if !modelSupported {
		RespondJSON(c, http.StatusOK, gin.H{
			"success":          false,
			"error":            "模型 " + testReq.Model + " 不在此渠道的支持列表中",
			"model":            testReq.Model,
			"supported_models": testCfg.Models,
		})
		return
	}

	// 执行测试
	testResult := s.testChannelAPI(&testCfg, &testReq)
	// 添加测试的 Key 索引信息到结果中
	testResult["tested_key_index"] = keyIndex
	testResult["total_keys"] = len(keys)

	// ✅ 修复：测试成功时清除该Key的冷却状态
	if success, ok := testResult["success"].(bool); ok && success {
		if err := s.store.ResetKeyCooldown(c.Request.Context(), id, keyIndex); err != nil {
			log.Printf("⚠️  警告: 清除Key #%d冷却状态失败: %v", keyIndex, err)
		}

		// ✨ 优化：同时清除渠道级冷却（因为至少有一个Key可用）
		// 设计理念：测试成功证明渠道恢复正常，应立即解除渠道级冷却，避免选择器过滤该渠道
		_ = s.store.ResetCooldown(c.Request.Context(), id)
	}

	RespondJSON(c, http.StatusOK, testResult)
}

// 测试渠道API连通性
func (s *Server) testChannelAPI(cfg *Config, testReq *TestChannelRequest) map[string]any {
	// 选择并规范化渠道类型
	channelType := normalizeChannelType(testReq.ChannelType)
	var tester ChannelTester
	switch channelType {
	case "codex":
		tester = &CodexTester{}
	case "openai":
		tester = &OpenAITester{}
	case "gemini":
		tester = &GeminiTester{}
	case "anthropic":
		tester = &AnthropicTester{}
	default:
		tester = &AnthropicTester{}
	}

	// 构建请求
	fullURL, baseHeaders, body, err := tester.Build(cfg, testReq)
	if err != nil {
		return map[string]any{"success": false, "error": "构造测试请求失败: " + err.Error()}
	}

	// 创建HTTP请求
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(body))
	if err != nil {
		return map[string]any{"success": false, "error": "创建HTTP请求失败: " + err.Error()}
	}

	// 设置基础请求头
	for k, vs := range baseHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// 添加/覆盖自定义请求头
	for key, value := range testReq.Headers {
		req.Header.Set(key, value)
	}

	// 发送请求
	start := time.Now()
	resp, err := s.client.Do(req)
	duration := time.Since(start)
	if err != nil {
		return map[string]any{"success": false, "error": "网络请求失败: " + err.Error(), "duration_ms": duration.Milliseconds()}
	}
	defer resp.Body.Close()

	// 读取响应
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return map[string]any{"success": false, "error": "读取响应失败: " + err.Error(), "duration_ms": duration.Milliseconds(), "status_code": resp.StatusCode}
	}

	// 通用结果
	result := map[string]any{
		"success":     resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code": resp.StatusCode,
		"duration_ms": duration.Milliseconds(),
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 成功：委托给 tester 解析
		parsed := tester.Parse(resp.StatusCode, respBody)
		for k, v := range parsed {
			result[k] = v
		}
		result["message"] = "API测试成功"
	} else {
		// 错误：统一解析
		var errorMsg string
		var apiError map[string]any
		if err := sonic.Unmarshal(respBody, &apiError); err == nil {
			if errInfo, ok := apiError["error"].(map[string]any); ok {
				if msg, ok := errInfo["message"].(string); ok {
					errorMsg = msg
				} else if typeStr, ok := errInfo["type"].(string); ok {
					errorMsg = typeStr
				}
			}
			result["api_error"] = apiError
		} else {
			result["raw_response"] = string(respBody)
		}
		if errorMsg == "" {
			errorMsg = "API返回错误状态: " + resp.Status
		}
		result["error"] = errorMsg
	}

	return result
}

func buildCSVColumnIndex(header []string) map[string]int {
	index := make(map[string]int, len(header))
	for i, col := range header {
		norm := normalizeCSVHeader(col)
		if norm == "" {
			continue
		}
		index[norm] = i
	}
	return index
}

func normalizeCSVHeader(name string) string {
	trimmed := strings.TrimSpace(name)
	trimmed = strings.TrimPrefix(trimmed, "\ufeff")
	lower := strings.ToLower(trimmed)
	switch lower {
	case "apikey", "api-key", "api key":
		return "api_key"
	case "model", "model_list", "model(s)":
		return "models"
	case "model_redirect", "model-redirects", "modelredirects", "redirects":
		return "model_redirects"
	case "key_strategy", "key-strategy", "keystrategy", "策略", "使用策略":
		return "key_strategy"
	case "status":
		return "enabled"
	default:
		return lower
	}
}

func isCSVRecordEmpty(record []string) bool {
	for _, cell := range record {
		if strings.TrimSpace(cell) != "" {
			return false
		}
	}
	return true
}

func parseImportModels(raw string) []string {
	if raw == "" {
		return nil
	}
	splitter := func(r rune) bool {
		switch r {
		case ',', ';', '|', '\n', '\r', '\t':
			return true
		default:
			return false
		}
	}
	parts := strings.FieldsFunc(raw, splitter)
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		clean := strings.TrimSpace(p)
		if clean == "" {
			continue
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func parseImportEnabled(raw string) (bool, bool) {
	val := strings.TrimSpace(strings.ToLower(raw))
	switch val {
	case "1", "true", "yes", "y", "启用", "enabled", "on":
		return true, true
	case "0", "false", "no", "n", "禁用", "disabled", "off":
		return false, true
	default:
		return false, false
	}
}

// handleGetChannelTypes 获取渠道类型配置（公开端点，前端动态加载）
func (s *Server) handleGetChannelTypes(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"data": ChannelTypes,
	})
}

// CooldownRequest 冷却设置请求结构
type CooldownRequest struct {
	DurationMs int64 `json:"duration_ms" binding:"required,min=1000"` // 最少1秒
}

// handleSetChannelCooldown 设置渠道级别冷却
func (s *Server) handleSetChannelCooldown(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "invalid channel ID"})
		return
	}

	var req CooldownRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}

	until := time.Now().Add(time.Duration(req.DurationMs) * time.Millisecond)
	err = s.store.SetCooldown(c.Request.Context(), id, until)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("渠道已冷却 %d 毫秒", req.DurationMs),
	})
}

// handleSetKeyCooldown 设置Key级别冷却
func (s *Server) handleSetKeyCooldown(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "invalid channel ID"})
		return
	}

	keyIndexStr := c.Param("keyIndex")
	keyIndex, err := strconv.Atoi(keyIndexStr)
	if err != nil || keyIndex < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "invalid key index"})
		return
	}

	var req CooldownRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}

	until := time.Now().Add(time.Duration(req.DurationMs) * time.Millisecond)
	err = s.store.SetKeyCooldown(c.Request.Context(), id, keyIndex, until)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("Key #%d 已冷却 %d 毫秒", keyIndex+1, req.DurationMs),
	})
}
