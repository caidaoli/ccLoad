package validator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
)

// SubscriptionValidator 88code套餐验证器
//
// 功能:
// - 检查88code渠道是否在免费套餐(FREE)
// - 非免费套餐的渠道将被标记为不可用
//
// 性能优化:
// - 60秒TTL缓存,减少外部API调用
// - 5秒超时控制,避免阻塞主流程
//
// 容错策略:
// - API调用失败时默认允许通过(防御性设计)
type SubscriptionValidator struct {
	httpClient *http.Client
	apiURL     string
	enabled    bool

	// 缓存: channelID → cacheEntry
	cache      sync.Map
	cacheTTL   time.Duration
	apiTimeout time.Duration
}

// cacheEntry 缓存条目
type cacheEntry struct {
	available bool      // 渠道是否可用
	reason    string    // 不可用时的原因
	expiry    time.Time // 过期时间
}

// usage88CodeResponse 88code API响应结构
type usage88CodeResponse struct {
	SubscriptionName string `json:"subscriptionName"`
	// 其他字段按需添加
}

// NewSubscriptionValidator 创建88code套餐验证器
//
// 参数:
//
//	enabled - 是否启用验证(从环境变量CCLOAD_88CODE_FREE_ONLY读取)
//
// 返回:
//
//	验证器实例
func NewSubscriptionValidator(enabled bool) *SubscriptionValidator {
	return &SubscriptionValidator{
		httpClient: &http.Client{
			Timeout: 30 * time.Second, // 5秒超时
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		apiURL:     "https://www.88code.org/api/usage",
		enabled:    enabled,
		cacheTTL:   60 * time.Second, // 60秒缓存
		apiTimeout: 30 * time.Second, // API调用超时
	}
}

// ShouldValidate 判断是否需要验证此渠道
//
// 验证条件:
// - 验证器已启用(CCLOAD_88CODE_FREE_ONLY=true)
// - 渠道名称以"88code"开头(不区分大小写)
func (v *SubscriptionValidator) ShouldValidate(cfg *model.Config) bool {
	if !v.enabled {
		return false
	}

	// 不区分大小写的前缀匹配
	// 例如: "88code-test", "88Code-prod", "88CODE_backup"都会被验证
	return strings.HasPrefix(strings.ToLower(cfg.Name), "88code")
}

// Validate 验证88code渠道的套餐类型
//
// 验证流程:
// 1. 检查缓存,如果有效直接返回
// 2. 调用88code API获取套餐信息
// 3. 判断subscriptionName是否为"FREE"
// 4. 更新缓存
//
// 返回:
//
//	available - true表示可用(FREE套餐),false表示不可用(非FREE套餐)
//	reason - 不可用时的原因描述
//	err - API调用错误(网络故障、超时等)
func (v *SubscriptionValidator) Validate(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
	// 1. 检查缓存
	if entry, ok := v.cache.Load(cfg.ID); ok {
		cached := entry.(cacheEntry)
		if time.Now().Before(cached.expiry) {
			// 缓存命中
			return cached.available, cached.reason, nil
		}
		// 缓存过期,删除旧条目
		v.cache.Delete(cfg.ID)
	}

	// 2. 调用88code API
	subscription, err := v.fetch88CodeSubscription(ctx, apiKey)
	if err != nil {
		// API调用失败时的防御性策略:
		// - 返回error,由Manager决定降级行为(默认允许通过)
		// - 不写入缓存,下次请求重试
		return false, "", fmt.Errorf("failed to fetch 88code subscription: %w", err)
	}

	// 3. 判断套餐类型
	isFree := strings.EqualFold(subscription, "FREE")
	reason := ""
	if !isFree {
		reason = fmt.Sprintf("subscription=%s (not FREE)", subscription)
	}

	// 4. 更新缓存
	v.cache.Store(cfg.ID, cacheEntry{
		available: isFree,
		reason:    reason,
		expiry:    time.Now().Add(v.cacheTTL),
	})

	return isFree, reason, nil
}

// fetch88CodeSubscription 调用88code API获取套餐信息
//
// API规范:
//
//	curl -X POST 'https://www.88code.org/api/usage' \
//	  --header 'Authorization: Bearer ${API_KEY}'
//
// 响应示例:
//
//	{"subscriptionName": "FREE", ...}
//	{"subscriptionName": "PRO", ...}
func (v *SubscriptionValidator) fetch88CodeSubscription(ctx context.Context, apiKey string) (string, error) {
	// 创建带超时的context
	ctxWithTimeout, cancel := context.WithTimeout(ctx, v.apiTimeout)
	defer cancel()

	// 构建HTTP请求(使用POST方法)
	req, err := http.NewRequestWithContext(ctxWithTimeout, http.MethodPost, v.apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// 设置Authorization header
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))
	req.Header.Set("Accept", "application/json")

	// 发送请求
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// 检查HTTP状态码
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)) // 限制读取1KB
		return "", fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	// 解析响应
	var usageResp usage88CodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&usageResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return usageResp.SubscriptionName, nil
}

// ClearCache 清空缓存(用于测试和手动刷新)
func (v *SubscriptionValidator) ClearCache() {
	v.cache = sync.Map{}
}

// SetCacheTTL 设置缓存TTL(用于测试)
func (v *SubscriptionValidator) SetCacheTTL(ttl time.Duration) {
	v.cacheTTL = ttl
}

// SetAPITimeout 设置API超时(用于测试)
func (v *SubscriptionValidator) SetAPITimeout(timeout time.Duration) {
	v.apiTimeout = timeout
}
