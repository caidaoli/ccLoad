package main

import (
	"bytes"
	"context"
	"github.com/bytedance/sonic"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Admin: /admin/channels (GET, POST)
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfgs, err := s.store.ListConfigs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 附带冷却状态（until与剩余毫秒）
		type outCh struct {
			*Config
			CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
			CooldownRemainingMS int64      `json:"cooldown_remaining_ms,omitempty"`
		}
		now := time.Now()
		out := make([]outCh, 0, len(cfgs))
		for _, c := range cfgs {
			oc := outCh{Config: c}
			if until, ok := s.store.GetCooldownUntil(r.Context(), c.ID); ok && until.After(now) {
				u := until // capture
				oc.CooldownUntil = &u
				oc.CooldownRemainingMS = int64(until.Sub(now) / time.Millisecond)
			}
			out = append(out, oc)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in Config
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		if err := sonic.Unmarshal(body, &in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if in.Name == "" || in.APIKey == "" || in.URL == "" || len(in.Models) == 0 {
			http.Error(w, "missing fields name/api_key/url/models", http.StatusBadRequest)
			return
		}
		created, err := s.store.CreateConfig(r.Context(), &in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Admin: /admin/channels/{id} (GET, PUT, DELETE) 和 /admin/channels/{id}/test (POST)
func (s *Server) handleChannelByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/channels/")
	
	// 检查是否是测试路径
	if strings.Contains(rest, "/test") {
		s.handleChannelTest(w, r)
		return
	}
	
	id, err := parseInt64Param(rest)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.store.GetConfig(r.Context(), id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPut:
		var in Config
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		if err := sonic.Unmarshal(body, &in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		upd, err := s.store.UpdateConfig(r.Context(), id, &in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, upd)
	case http.MethodDelete:
		if err := s.store.DeleteConfig(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Admin: /admin/errors?hours=24&limit=100&offset=0
func (s *Server) handleErrors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hours, _ := strconv.Atoi(q.Get("hours"))
	if hours <= 0 {
		hours = 24
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	// 过滤：按渠道ID或渠道名
	var lf LogFilter
	if cidStr := strings.TrimSpace(q.Get("channel_id")); cidStr != "" {
		if id, err := strconv.ParseInt(cidStr, 10, 64); err == nil && id > 0 {
			lf.ChannelID = &id
		}
	}
	if cn := strings.TrimSpace(q.Get("channel_name")); cn != "" {
		lf.ChannelName = cn
	}
	if cnl := strings.TrimSpace(q.Get("channel_name_like")); cnl != "" {
		lf.ChannelNameLike = cnl
	}
	if m := strings.TrimSpace(q.Get("model")); m != "" {
		lf.Model = m
	}
	if ml := strings.TrimSpace(q.Get("model_like")); ml != "" {
		lf.ModelLike = ml
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	logs, err := s.store.ListLogs(r.Context(), since, limit, offset, &lf)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

// Admin: /admin/metrics?hours=24&bucket_min=5
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hours, _ := strconv.Atoi(q.Get("hours"))
	if hours <= 0 {
		hours = 24
	}
	bucketMin, _ := strconv.Atoi(q.Get("bucket_min"))
	if bucketMin <= 0 {
		bucketMin = 5
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	pts, err := s.store.Aggregate(r.Context(), since, time.Duration(bucketMin)*time.Minute)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, pts)
}

// Admin: /admin/stats?hours=24&channel_name_like=xxx&model_like=xxx
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hours, _ := strconv.Atoi(q.Get("hours"))
	if hours <= 0 {
		hours = 24
	}

	// 构建过滤条件（复用errors API的逻辑）
	var lf LogFilter
	if cidStr := strings.TrimSpace(q.Get("channel_id")); cidStr != "" {
		if id, err := strconv.ParseInt(cidStr, 10, 64); err == nil && id > 0 {
			lf.ChannelID = &id
		}
	}
	if cn := strings.TrimSpace(q.Get("channel_name")); cn != "" {
		lf.ChannelName = cn
	}
	if cnl := strings.TrimSpace(q.Get("channel_name_like")); cnl != "" {
		lf.ChannelNameLike = cnl
	}
	if m := strings.TrimSpace(q.Get("model")); m != "" {
		lf.Model = m
	}
	if ml := strings.TrimSpace(q.Get("model_like")); ml != "" {
		lf.ModelLike = ml
	}

	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	stats, err := s.store.GetStats(r.Context(), since, &lf)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 包装成简单响应格式
	response := map[string]interface{}{
		"stats": stats,
	}
	writeJSON(w, http.StatusOK, response)
}

// Public: /public/summary 基础请求统计（不需要身份验证）
func (s *Server) handlePublicSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hours, _ := strconv.Atoi(q.Get("hours"))
	if hours <= 0 {
		hours = 24 // 默认24小时
	}

	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	stats, err := s.store.GetStats(r.Context(), since, nil) // 不使用过滤条件
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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

	response := map[string]interface{}{
		"total_requests":   totalSuccess + totalError,
		"success_requests": totalSuccess,
		"error_requests":   totalError,
		"active_channels":  len(totalChannels),
		"active_models":    len(totalModels),
		"hours":            hours,
	}

	writeJSON(w, http.StatusOK, response)
}

// Admin: /admin/channels/{id}/test (POST)
func (s *Server) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 解析渠道ID
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/channels/"), "/")
	if len(pathParts) < 2 || pathParts[1] != "test" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	
	id, err := parseInt64Param(pathParts[0])
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	// 解析请求体
	var testReq struct {
		Model string `json:"model"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	if err := sonic.Unmarshal(body, &testReq); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if testReq.Model == "" {
		http.Error(w, "missing model field", http.StatusBadRequest)
		return
	}

	// 获取渠道配置
	cfg, err := s.store.GetConfig(r.Context(), id)
	if err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
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
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
			"error":   "模型 " + testReq.Model + " 不在此渠道的支持列表中",
		})
		return
	}

	// 执行测试
	testResult := s.testChannelAPI(cfg, testReq.Model)
	writeJSON(w, http.StatusOK, testResult)
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
