package app

import (
	"fmt"
	"strings"
	"time"

	"ccLoad/internal/model"
)

// ==================== 共享数据结构 ====================
// 从admin.go提取共享类型,遵循SRP原则

// ChannelRequest 渠道创建/更新请求结构
type ChannelRequest struct {
	Name           string            `json:"name" binding:"required"`
	APIKey         string            `json:"api_key" binding:"required"`
	ChannelType    string            `json:"channel_type,omitempty"` // 渠道类型:anthropic, codex, gemini
	KeyStrategy    string            `json:"key_strategy,omitempty"` // Key使用策略:sequential, round_robin
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

// ToConfig 转换为Config结构(不包含API Key,API Key单独处理)
func (cr *ChannelRequest) ToConfig() *model.Config {
	return &model.Config{
		Name:           strings.TrimSpace(cr.Name),
		ChannelType:    strings.TrimSpace(cr.ChannelType), // 传递渠道类型
		URL:            strings.TrimSpace(cr.URL),
		Priority:       cr.Priority,
		Models:         cr.Models,
		ModelRedirects: cr.ModelRedirects,
		Enabled:        cr.Enabled,
	}
}

// KeyCooldownInfo Key级别冷却信息
type KeyCooldownInfo struct {
	KeyIndex            int        `json:"key_index"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	CooldownRemainingMS int64      `json:"cooldown_remaining_ms,omitempty"`
}

// ChannelWithCooldown 带冷却状态的渠道响应结构
type ChannelWithCooldown struct {
	*model.Config
	KeyStrategy         string            `json:"key_strategy,omitempty"` // ✅ 修复 (2025-10-11): 添加key_strategy字段
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

// CooldownRequest 冷却设置请求
type CooldownRequest struct {
	DurationMs int64 `json:"duration_ms" binding:"required,min=1000"` // 最少1秒
}
