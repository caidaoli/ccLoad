package main

import (
	"encoding/json"
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
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
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

// Admin: /admin/channels/{id} (GET, PUT, DELETE)
func (s *Server) handleChannelByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/channels/")
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
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
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
