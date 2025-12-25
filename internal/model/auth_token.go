package model

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// AuthToken 表示一个API访问令牌
// 用于代理API (/v1/*) 的认证授权
type AuthToken struct {
	ID          int64     `json:"id"`
	Token       string    `json:"token"`                  // SHA256哈希值(存储时)或明文(创建时返回)
	Description string    `json:"description"`            // 令牌用途描述
	CreatedAt   time.Time `json:"created_at"`             // 创建时间
	ExpiresAt   *int64    `json:"expires_at,omitempty"`   // 过期时间(Unix毫秒时间戳)，nil表示永不过期
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

	// RPM统计（2025-12新增，用于tokens.html显示）
	PeakRPM   float64 `json:"peak_rpm,omitempty"`   // 峰值RPM
	AvgRPM    float64 `json:"avg_rpm,omitempty"`    // 平均RPM
	RecentRPM float64 `json:"recent_rpm,omitempty"` // 最近一分钟RPM
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
	if t.ExpiresAt == nil {
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
	t.LastUsedAt = &now
}
