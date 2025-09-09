package main

import (
	"bytes"
	"context"
	"fmt"
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
	Name     string   `json:"name" binding:"required"`
	APIKey   string   `json:"api_key" binding:"required"`
	URL      string   `json:"url" binding:"required,url"`
	Priority int      `json:"priority"`
	Models   []string `json:"models" binding:"required,min=1"`
	Enabled  bool     `json:"enabled"`
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
		Name:     strings.TrimSpace(cr.Name),
		APIKey:   strings.TrimSpace(cr.APIKey),
		URL:      strings.TrimSpace(cr.URL),
		Priority: cr.Priority,
		Models:   cr.Models,
		Enabled:  cr.Enabled,
	}
}

// ChannelWithCooldown 带冷却状态的渠道响应结构
type ChannelWithCooldown struct {
	*Config
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	CooldownRemainingMS int64      `json:"cooldown_remaining_ms,omitempty"`
}

// Admin: /admin/channels (GET, POST) - 重构版本
func (s *Server) handleChannels(c *gin.Context) {
	router := NewMethodRouter().
		GET(s.handleListChannels).
		POST(s.handleCreateChannel)
	
	router.Handle(c)
}

// 获取渠道列表
func (s *Server) handleListChannels(c *gin.Context) {
	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	
	// 附带冷却状态
	now := time.Now()
	out := make([]ChannelWithCooldown, 0, len(cfgs))
	for _, cfg := range cfgs {
		oc := ChannelWithCooldown{Config: cfg}
		if until, ok := s.store.GetCooldownUntil(c.Request.Context(), cfg.ID); ok && until.After(now) {
			u := until
			oc.CooldownUntil = &u
			oc.CooldownRemainingMS = int64(until.Sub(now) / time.Millisecond)
		}
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
	var rawReq map[string]interface{}
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
	
	upd, err := s.store.UpdateConfig(c.Request.Context(), id, req.ToConfig())
	if err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}
	RespondJSON(c, http.StatusOK, upd)
}

// 删除渠道
func (s *Server) handleDeleteChannel(c *gin.Context, id int64) {
	if err := s.store.DeleteConfig(c.Request.Context(), id); err != nil {
		RespondError(c, http.StatusNotFound, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Admin: /admin/errors?hours=24&limit=100&offset=0 - 重构版本
func (s *Server) handleErrors(c *gin.Context) {
	params := ParsePaginationParams(c)
	
	// 过滤：按渠道ID或渠道名
	var lf LogFilter
	if cidStr := strings.TrimSpace(c.Query("channel_id")); cidStr != "" {
		if id, err := strconv.ParseInt(cidStr, 10, 64); err == nil && id > 0 {
			lf.ChannelID = &id
		}
	}
	if cn := strings.TrimSpace(c.Query("channel_name")); cn != "" {
		lf.ChannelName = cn
	}
	if cnl := strings.TrimSpace(c.Query("channel_name_like")); cnl != "" {
		lf.ChannelNameLike = cnl
	}
	if m := strings.TrimSpace(c.Query("model")); m != "" {
		lf.Model = m
	}
	if ml := strings.TrimSpace(c.Query("model_like")); ml != "" {
		lf.ModelLike = ml
	}
	
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
	
	// 构建过滤条件（复用errors API的逻辑）
	var lf LogFilter
	if cidStr := strings.TrimSpace(c.Query("channel_id")); cidStr != "" {
		if id, err := strconv.ParseInt(cidStr, 10, 64); err == nil && id > 0 {
			lf.ChannelID = &id
		}
	}
	if cn := strings.TrimSpace(c.Query("channel_name")); cn != "" {
		lf.ChannelName = cn
	}
	if cnl := strings.TrimSpace(c.Query("channel_name_like")); cnl != "" {
		lf.ChannelNameLike = cnl
	}
	if m := strings.TrimSpace(c.Query("model")); m != "" {
		lf.Model = m
	}
	if ml := strings.TrimSpace(c.Query("model_like")); ml != "" {
		lf.ModelLike = ml
	}
	
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

// TestChannelRequest 渠道测试请求结构
type TestChannelRequest struct {
	Model string `json:"model" binding:"required"`
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
	
	// 检查模型是否支持
	modelSupported := false
	for _, model := range cfg.Models {
		if model == testReq.Model {
			modelSupported = true
			break
		}
	}
	if !modelSupported {
		RespondJSON(c, http.StatusOK, gin.H{
			"success": false,
			"error":   "模型 " + testReq.Model + " 不在此渠道的支持列表中",
		})
		return
	}
	
	// 执行测试
	testResult := s.testChannelAPI(cfg, testReq.Model)
	RespondJSON(c, http.StatusOK, testResult)
}

// 测试渠道API连通性
func (s *Server) testChannelAPI(cfg *Config, model string) map[string]interface{} {
	// 创建测试请求
	testMessage := map[string]interface{}{
		"model":      model,
		"max_tokens": 10,
		"messages": []map[string]interface{}{
			{
				"role":    "user",
				"content": "test",
			},
		},
		"system": "You are Claude Code, Anthropic's official CLI for Claude.",
	}

	reqBody, err := sonic.Marshal(testMessage)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"error":   "构造测试请求失败: " + err.Error(),
		}
	}

	// 创建HTTP请求
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 构建完整的API URL (与proxy.go保持一致)
	fullURL := strings.TrimRight(cfg.URL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(reqBody))
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"error":   "创建HTTP请求失败: " + err.Error(),
		}
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", "ccLoad/1.0")

	// 发送请求
	start := time.Now()
	resp, err := s.client.Do(req)
	duration := time.Since(start)

	if err != nil {
		return map[string]interface{}{
			"success":     false,
			"error":       "网络请求失败: " + err.Error(),
			"duration_ms": duration.Milliseconds(),
		}
	}
	defer resp.Body.Close()

	// 读取响应
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return map[string]interface{}{
			"success":     false,
			"error":       "读取响应失败: " + err.Error(),
			"duration_ms": duration.Milliseconds(),
			"status_code": resp.StatusCode,
		}
	}

	// 根据状态码判断成功或失败
	result := map[string]interface{}{
		"success":     resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code": resp.StatusCode,
		"duration_ms": duration.Milliseconds(),
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 成功响应
		var apiResp map[string]interface{}
		if err := sonic.Unmarshal(respBody, &apiResp); err == nil {
			// 提取响应文本
			if content, ok := apiResp["content"].([]interface{}); ok && len(content) > 0 {
				if textBlock, ok := content[0].(map[string]interface{}); ok {
					if text, ok := textBlock["text"].(string); ok {
						result["response_text"] = text
					}
				}
			}
			// 添加完整的API响应
			result["api_response"] = apiResp
		} else {
			// JSON解析失败，返回原始响应
			result["raw_response"] = string(respBody)
		}
		result["message"] = "API测试成功"
	} else {
		// 错误响应
		var errorMsg string
		var apiError map[string]interface{}
		if err := sonic.Unmarshal(respBody, &apiError); err == nil {
			if errInfo, ok := apiError["error"].(map[string]interface{}); ok {
				if msg, ok := errInfo["message"].(string); ok {
					errorMsg = msg
				} else if typeStr, ok := errInfo["type"].(string); ok {
					errorMsg = typeStr
				}
			}
			// 添加完整的错误响应
			result["api_error"] = apiError
		} else {
			// JSON解析失败，返回原始响应
			result["raw_response"] = string(respBody)
		}
		if errorMsg == "" {
			errorMsg = "API返回错误状态: " + resp.Status
		}
		result["error"] = errorMsg
	}

	return result
}
