package app

import (
	"fmt"
	neturl "net/url"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

// ==================== 共享数据结构 ====================
// 从admin.go提取共享类型,遵循SRP原则

// ChannelRequest 渠道创建/更新请求结构
type ChannelRequest struct {
	Name        string             `json:"name" binding:"required"`
	APIKey      string             `json:"api_key" binding:"required"`
	ChannelType string             `json:"channel_type,omitempty"` // 渠道类型:anthropic, codex, gemini
	KeyStrategy string             `json:"key_strategy,omitempty"` // Key使用策略:sequential, round_robin
	URL         string             `json:"url" binding:"required,url"`
	Priority    int                `json:"priority"`
	Models      []model.ModelEntry `json:"models" binding:"required,min=1"` // 模型配置（包含重定向）
	Enabled     bool               `json:"enabled"`
}

func validateChannelBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url cannot be empty")
	}

	u, err := neturl.Parse(raw)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid url: %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid url scheme: %q (allowed: http, https)", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("url must not contain user info")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("url must not contain query or fragment")
	}

	// [FIX] 只禁止包含 /v1 的 path（防止误填 API endpoint 如 /v1/messages）
	// 允许其他 path（如 /api, /openai 等用于反向代理或 API gateway）
	if strings.Contains(u.Path, "/v1") {
		return "", fmt.Errorf("url should not contain API endpoint path like /v1 (current path: %q)", u.Path)
	}

	// 强制返回标准化格式（scheme://host+path，移除 trailing slash）
	// 例如: "https://example.com/api/" → "https://example.com/api"
	normalizedPath := strings.TrimSuffix(u.Path, "/")
	return u.Scheme + "://" + u.Host + normalizedPath, nil
}

// Validate 实现RequestValidator接口
// [FIX] P0-1: 添加白名单校验和标准化（Fail-Fast + 边界防御）
func (cr *ChannelRequest) Validate() error {
	// 必填字段校验（现有逻辑保留）
	if strings.TrimSpace(cr.Name) == "" {
		return fmt.Errorf("name cannot be empty")
	}
	if strings.TrimSpace(cr.APIKey) == "" {
		return fmt.Errorf("api_key cannot be empty")
	}
	if len(cr.Models) == 0 {
		return fmt.Errorf("models cannot be empty")
	}
	// 验证模型条目（DRY: 使用 ModelEntry.Validate()）
	for i := range cr.Models {
		if err := cr.Models[i].Validate(); err != nil {
			return fmt.Errorf("models[%d]: %w", i, err)
		}
	}

	// URL 验证规则（Fail-Fast 边界防御）：
	// - 必须包含 scheme+host（http/https）
	// - 禁止 userinfo、query、fragment
	// - 禁止包含 /v1 的 path（防止误填 endpoint 如 /v1/messages）
	// - 允许其他 path（如 /api, /openai 等用于反向代理或 API gateway）
	normalizedURL, err := validateChannelBaseURL(cr.URL)
	if err != nil {
		return err
	}
	cr.URL = normalizedURL

	// [FIX] channel_type 白名单校验 + 标准化
	// 设计：空值允许（使用默认值anthropic），非空值必须合法
	cr.ChannelType = strings.TrimSpace(cr.ChannelType)
	if cr.ChannelType != "" {
		// 先标准化（小写化）
		normalized := util.NormalizeChannelType(cr.ChannelType)
		// 再白名单校验
		if !util.IsValidChannelType(normalized) {
			return fmt.Errorf("invalid channel_type: %q (allowed: anthropic, openai, gemini, codex)", cr.ChannelType)
		}
		cr.ChannelType = normalized // 应用标准化结果
	}

	// [FIX] key_strategy 白名单校验 + 标准化
	// 设计：空值允许（使用默认值sequential），非空值必须合法
	cr.KeyStrategy = strings.TrimSpace(cr.KeyStrategy)
	if cr.KeyStrategy != "" {
		// 先标准化（小写化）
		normalized := strings.ToLower(cr.KeyStrategy)
		// 再白名单校验
		if !model.IsValidKeyStrategy(normalized) {
			return fmt.Errorf("invalid key_strategy: %q (allowed: sequential, round_robin)", cr.KeyStrategy)
		}
		cr.KeyStrategy = normalized // 应用标准化结果
	}

	return nil
}

// ToConfig 转换为Config结构(不包含API Key,API Key单独处理)
func (cr *ChannelRequest) ToConfig() *model.Config {
	return &model.Config{
		Name:         strings.TrimSpace(cr.Name),
		ChannelType:  strings.TrimSpace(cr.ChannelType), // 传递渠道类型
		URL:          strings.TrimSpace(cr.URL),
		Priority:     cr.Priority,
		ModelEntries: cr.Models,
		Enabled:      cr.Enabled,
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
	KeyStrategy          string            `json:"key_strategy,omitempty"`           // [INFO] 修复 (2025-10-11): 添加key_strategy字段
	CooldownUntil        *time.Time        `json:"cooldown_until,omitempty"`
	CooldownRemainingMS  int64             `json:"cooldown_remaining_ms,omitempty"`
	KeyCooldowns         []KeyCooldownInfo `json:"key_cooldowns,omitempty"`
	EffectivePriority    *float64          `json:"effective_priority,omitempty"`     // 健康度模式下的有效优先级
	SuccessRate          *float64          `json:"success_rate,omitempty"`           // 成功率(0-1)
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

// SettingUpdateRequest 系统配置更新请求
type SettingUpdateRequest struct {
	Value string `json:"value" binding:"required"`
}
