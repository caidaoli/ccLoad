// Package model 定义核心业务模型和数据结构
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

const microUSDScale = 1_000_000

// AuthToken 表示一个API访问令牌
// 用于代理API (/v1/*) 的认证授权
type AuthToken struct {
	ID          int64     `json:"id"`
	Token       string    `json:"token"`                  // SHA256哈希值(存储时)或明文(创建时返回)
	Description string    `json:"description"`            // 令牌用途描述
	CreatedAt   time.Time `json:"created_at"`             // 创建时间
	ExpiresAt   *int64    `json:"expires_at,omitempty"`   // 过期时间(Unix毫秒时间戳)，nil/0 表示永不过期
	LastUsedAt  *int64    `json:"last_used_at,omitempty"` // 最后使用时间(Unix毫秒时间戳)
	IsActive    bool      `json:"is_active"`              // 是否启用

	// 统计字段（2025-11新增）
	SuccessCount   int64   `json:"success_count"`     // 成功调用次数
	FailureCount   int64   `json:"failure_count"`     // 失败调用次数
	StreamAvgTTFB  float64 `json:"stream_avg_ttfb"`   // 流式请求平均首字节时间(秒)
	NonStreamAvgRT float64 `json:"non_stream_avg_rt"` // 非流式请求平均响应时间(秒)
	StreamCount    int64   `json:"stream_count"`      // 流式请求计数(用于计算平均值)
	NonStreamCount int64   `json:"non_stream_count"`  // 非流式请求计数(用于计算平均值)

	// Token成本统计（2025-12新增）
	PromptTokensTotal        int64   `json:"prompt_tokens_total"`         // 累计输入Token数
	CompletionTokensTotal    int64   `json:"completion_tokens_total"`     // 累计输出Token数
	CacheReadTokensTotal     int64   `json:"cache_read_tokens_total"`     // 累计缓存读Token数
	CacheCreationTokensTotal int64   `json:"cache_creation_tokens_total"` // 累计缓存写Token数
	TotalCostUSD             float64 `json:"total_cost_usd"`              // 累计成本(美元)

	// 费用限额（2026-01新增）
	// 使用微美元整数存储，避免浮点误差。JSON序列化时自动转换为USD浮点数。
	// 1 USD = 1,000,000 微美元
	CostUsedMicroUSD  int64 `json:"-"` // 已消耗费用（微美元）
	CostLimitMicroUSD int64 `json:"-"` // 费用上限（微美元；0=无限制）

	// RPM统计（2025-12新增，用于tokens.html显示）
	PeakRPM   float64 `json:"peak_rpm,omitempty"`   // 峰值RPM
	AvgRPM    float64 `json:"avg_rpm,omitempty"`    // 平均RPM
	RecentRPM float64 `json:"recent_rpm,omitempty"` // 最近一分钟RPM

	// 模型限制（2026-01新增）
	AllowedModels []string `json:"allowed_models,omitempty"` // 允许的模型列表，空表示无限制
}

// AuthTokenRangeStats 某个时间范围内的token统计（从logs表聚合，2025-12新增）
type AuthTokenRangeStats struct {
	SuccessCount        int64   `json:"success_count"`         // 成功次数
	FailureCount        int64   `json:"failure_count"`         // 失败次数
	PromptTokens        int64   `json:"prompt_tokens"`         // 输入Token总数
	CompletionTokens    int64   `json:"completion_tokens"`     // 输出Token总数
	CacheReadTokens     int64   `json:"cache_read_tokens"`     // 缓存读Token总数
	CacheCreationTokens int64   `json:"cache_creation_tokens"` // 缓存写Token总数
	TotalCost           float64 `json:"total_cost"`            // 总费用(美元)
	StreamAvgTTFB       float64 `json:"stream_avg_ttfb"`       // 流式请求平均首字节时间
	NonStreamAvgRT      float64 `json:"non_stream_avg_rt"`     // 非流式请求平均响应时间
	StreamCount         int64   `json:"stream_count"`          // 流式请求计数
	NonStreamCount      int64   `json:"non_stream_count"`      // 非流式请求计数
	// RPM统计（2025-12新增）
	PeakRPM   float64 `json:"peak_rpm"`   // 峰值RPM（每分钟最大请求数）
	AvgRPM    float64 `json:"avg_rpm"`    // 平均RPM
	RecentRPM float64 `json:"recent_rpm"` // 最近一分钟RPM（仅本日有效）
}

// HashToken 计算令牌的SHA256哈希值
// 用于安全存储令牌到数据库
func HashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

// IsExpired 检查令牌是否已过期
func (t *AuthToken) IsExpired() bool {
	if t.ExpiresAt == nil || *t.ExpiresAt == 0 {
		return false
	}
	return time.Now().UnixMilli() > *t.ExpiresAt
}

// IsValid 检查令牌是否有效(启用且未过期)
func (t *AuthToken) IsValid() bool {
	return t.IsActive && !t.IsExpired()
}

// MaskToken 脱敏显示令牌(仅显示前4后4字符)
// 例如: "sk-ant-1234567890abcdef" -> "sk-a****cdef"
func MaskToken(token string) string {
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "****" + token[len(token)-4:]
}

// UpdateLastUsed 更新最后使用时间为当前时间
func (t *AuthToken) UpdateLastUsed() {
	now := time.Now().UnixMilli()
	if t.LastUsedAt != nil && now <= *t.LastUsedAt {
		now = *t.LastUsedAt + 1
	}
	t.LastUsedAt = &now
}

// IsModelAllowed 检查模型是否被令牌允许访问
// 如果 AllowedModels 为空，表示无限制，允许所有模型
func (t *AuthToken) IsModelAllowed(model string) bool {
	if len(t.AllowedModels) == 0 {
		return true
	}
	for _, m := range t.AllowedModels {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return false
}

// CostUsedUSD 返回已消耗费用（美元）
func (t *AuthToken) CostUsedUSD() float64 {
	return float64(t.CostUsedMicroUSD) / microUSDScale
}

// CostLimitUSD 返回费用上限（美元）
func (t *AuthToken) CostLimitUSD() float64 {
	return float64(t.CostLimitMicroUSD) / microUSDScale
}

// SetCostLimitUSD 设置费用上限（从美元转换为微美元）
func (t *AuthToken) SetCostLimitUSD(usd float64) {
	if usd <= 0 {
		t.CostLimitMicroUSD = 0
		return
	}
	t.CostLimitMicroUSD = int64(usd * microUSDScale)
}

// authTokenJSON 是用于JSON序列化的内部结构
type authTokenJSON struct {
	ID                       int64     `json:"id"`
	Token                    string    `json:"token"`
	Description              string    `json:"description"`
	CreatedAt                time.Time `json:"created_at"`
	ExpiresAt                *int64    `json:"expires_at,omitempty"`
	LastUsedAt               *int64    `json:"last_used_at,omitempty"`
	IsActive                 bool      `json:"is_active"`
	SuccessCount             int64     `json:"success_count"`
	FailureCount             int64     `json:"failure_count"`
	StreamAvgTTFB            float64   `json:"stream_avg_ttfb"`
	NonStreamAvgRT           float64   `json:"non_stream_avg_rt"`
	StreamCount              int64     `json:"stream_count"`
	NonStreamCount           int64     `json:"non_stream_count"`
	PromptTokensTotal        int64     `json:"prompt_tokens_total"`
	CompletionTokensTotal    int64     `json:"completion_tokens_total"`
	CacheReadTokensTotal     int64     `json:"cache_read_tokens_total"`
	CacheCreationTokensTotal int64     `json:"cache_creation_tokens_total"`
	TotalCostUSD             float64   `json:"total_cost_usd"`
	CostUsedUSD              float64   `json:"cost_used_usd"`
	CostLimitUSD             float64   `json:"cost_limit_usd"`
	PeakRPM                  float64   `json:"peak_rpm,omitempty"`
	AvgRPM                   float64   `json:"avg_rpm,omitempty"`
	RecentRPM                float64   `json:"recent_rpm,omitempty"`
	AllowedModels            []string  `json:"allowed_models,omitempty"`
}

// MarshalJSON 自定义JSON序列化，将MicroUSD转换为USD浮点数
func (t AuthToken) MarshalJSON() ([]byte, error) {
	return json.Marshal(authTokenJSON{
		ID:                       t.ID,
		Token:                    t.Token,
		Description:              t.Description,
		CreatedAt:                t.CreatedAt,
		ExpiresAt:                t.ExpiresAt,
		LastUsedAt:               t.LastUsedAt,
		IsActive:                 t.IsActive,
		SuccessCount:             t.SuccessCount,
		FailureCount:             t.FailureCount,
		StreamAvgTTFB:            t.StreamAvgTTFB,
		NonStreamAvgRT:           t.NonStreamAvgRT,
		StreamCount:              t.StreamCount,
		NonStreamCount:           t.NonStreamCount,
		PromptTokensTotal:        t.PromptTokensTotal,
		CompletionTokensTotal:    t.CompletionTokensTotal,
		CacheReadTokensTotal:     t.CacheReadTokensTotal,
		CacheCreationTokensTotal: t.CacheCreationTokensTotal,
		TotalCostUSD:             t.TotalCostUSD,
		CostUsedUSD:              t.CostUsedUSD(),
		CostLimitUSD:             t.CostLimitUSD(),
		PeakRPM:                  t.PeakRPM,
		AvgRPM:                   t.AvgRPM,
		RecentRPM:                t.RecentRPM,
		AllowedModels:            t.AllowedModels,
	})
}
