package app

import (
	"context"
	"log"
	"strings"
	"time"

	"ccLoad/internal/model"
)

func selectScheduledCheckModel(cfg *model.Config) (string, string) {
	if cfg == nil || len(cfg.ModelEntries) == 0 {
		return "", "未配置模型"
	}
	if cfg.ScheduledCheckModel == "" {
		return cfg.ModelEntries[0].Model, ""
	}
	if cfg.SupportsModel(cfg.ScheduledCheckModel) {
		return cfg.ScheduledCheckModel, ""
	}
	return "", "scheduled_check_model 不在渠道模型列表中"
}

func detectionLogFromResult(cfg *model.Config, logSource, requestModel, actualModel, apiKeyUsed, clientIP string, authTokenID int64, result map[string]any) *model.LogEntry {
	entry := &model.LogEntry{
		Time:          model.JSONTime{Time: time.Now()},
		LogSource:     logSource,
		Model:         requestModel,
		ClientIP:      clientIP,
		APIKeyUsed:    apiKeyUsed,
		AuthTokenID:   authTokenID,
		BaseURL:       getResultString(result, "base_url"),
		StatusCode:    getResultIntOrDefault(result, "status_code", 0),
		Duration:      float64(getResultInt64OrDefault(result, "duration_ms", 0)) / 1000,
		FirstByteTime: float64(getResultInt64OrDefault(result, "first_byte_duration_ms", 0)) / 1000,
		Cost:          getResultFloat64OrDefault(result, "cost_usd", 0),
	}
	if cfg != nil {
		entry.ChannelID = cfg.ID
	}
	if actualModel != "" && actualModel != requestModel {
		entry.ActualModel = actualModel
	}
	populateDetectionUsage(entry, result)
	entry.Message = detectionMessage(result)
	return entry
}

func detectionSkipLog(cfg *model.Config, logSource, modelName, reason string) *model.LogEntry {
	return &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		LogSource:  logSource,
		ChannelID:  cfg.ID,
		Model:      modelName,
		StatusCode: 0,
		Message:    strings.TrimSpace(reason),
	}
}

func (s *Server) persistDetectionLog(ctx context.Context, entry *model.LogEntry) {
	if s == nil || s.store == nil || entry == nil {
		return
	}
	if err := s.store.AddLog(ctx, entry); err != nil {
		log.Printf("[WARN] detection log write failed: %v", err)
	}
}

func populateDetectionUsage(entry *model.LogEntry, result map[string]any) {
	usage, ok := getNestedMap(result, "api_response", "usage")
	if !ok {
		return
	}
	entry.InputTokens = getMapIntOrDefault(usage, "input_tokens", 0)
	entry.OutputTokens = getMapIntOrDefault(usage, "output_tokens", 0)
	entry.CacheReadInputTokens = getMapIntOrDefault(usage, "cache_read_input_tokens", 0)
	entry.Cache5mInputTokens = getMapIntOrDefault(usage, "cache_5m_input_tokens", 0)
	entry.Cache1hInputTokens = getMapIntOrDefault(usage, "cache_1h_input_tokens", 0)
	entry.CacheCreationInputTokens = getMapIntOrDefault(usage, "cache_creation_input_tokens", entry.Cache5mInputTokens+entry.Cache1hInputTokens)
	if entry.CacheCreationInputTokens == 0 {
		entry.CacheCreationInputTokens = entry.Cache5mInputTokens + entry.Cache1hInputTokens
	}
}

func detectionMessage(result map[string]any) string {
	if msg := getResultString(result, "message"); strings.TrimSpace(msg) != "" {
		return msg
	}
	if msg := getResultString(result, "error"); strings.TrimSpace(msg) != "" {
		return msg
	}
	return "unknown"
}

func getResultString(result map[string]any, key string) string {
	if result == nil {
		return ""
	}
	if value, ok := result[key].(string); ok {
		return value
	}
	return ""
}

func getResultIntOrDefault(result map[string]any, key string, fallback int) int {
	if result == nil {
		return fallback
	}
	if value, ok := getResultInt(result[key]); ok {
		return value
	}
	return fallback
}

func getResultInt64OrDefault(result map[string]any, key string, fallback int64) int64 {
	if result == nil {
		return fallback
	}
	if value, ok := getResultInt64(result[key]); ok {
		return value
	}
	return fallback
}

func getResultFloat64OrDefault(result map[string]any, key string, fallback float64) float64 {
	if result == nil {
		return fallback
	}
	switch value := result[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	default:
		return fallback
	}
}

func getNestedMap(result map[string]any, outerKey, innerKey string) (map[string]any, bool) {
	outer, ok := result[outerKey].(map[string]any)
	if !ok {
		return nil, false
	}
	inner, ok := outer[innerKey].(map[string]any)
	return inner, ok
}

func getMapIntOrDefault(m map[string]any, key string, fallback int) int {
	if value, ok := getResultInt(m[key]); ok {
		return value
	}
	return fallback
}
