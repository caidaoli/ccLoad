// Package validator 提供渠道验证功能
//
// [REFACTOR] 2025-12: 移除 Manager 和接口层，简化为单个 88code 验证函数
// 理由：只有一个验证器，Manager + 接口 + 责任链模式是过度设计
package validator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
)

var (
	// 全局验证器状态（启动时初始化）
	enabled88CodeValidator bool
	httpClient             *http.Client
	cache88Code            sync.Map
	cacheTTL               = 60 * time.Second
	apiTimeout             = 3 * time.Second
)

// cacheEntry 缓存条目
type cacheEntry struct {
	available bool      // 渠道是否可用
	reason    string    // 不可用时的原因
	expiry    time.Time // 过期时间
}

// usage88CodeResponse 88code API响应结构
type usage88CodeResponse struct {
	Data struct {
		SubscriptionName string `json:"subscriptionName"`
	} `json:"data"`
}

// Init88CodeValidator 初始化88code验证器
// enabled: 是否启用验证（从配置读取，修改后重启生效）
func Init88CodeValidator(enabled bool) {
	enabled88CodeValidator = enabled
	if enabled {
		httpClient = &http.Client{
			Timeout: apiTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     30 * time.Second,
			},
		}
		log.Print("[INFO] 88code subscription validator enabled (non-FREE plans will be cooled down)")
	}
}

// Validate88CodeSubscription 验证88code渠道的套餐类型
//
// 功能:
// - 检查88code渠道是否在免费套餐(FREE)
// - 非免费套餐的渠道返回 available=false
//
// 性能优化:
// - 60秒TTL缓存，减少外部API调用
// - 3秒超时控制，避免阻塞主流程
//
// 容错策略:
// - API调用失败时默认允许通过（防御性设计）
//
// 返回:
//
//	available - true表示可用(FREE套餐或验证失败),false表示不可用(非FREE套餐)
//	reason - 不可用时的原因描述
func Validate88CodeSubscription(ctx context.Context, cfg *model.Config, apiKey string) (bool, string) {
	// 1. 验证器未启用，直接通过
	if !enabled88CodeValidator {
		return true, ""
	}

	// 2. 非88code渠道，跳过验证
	if !strings.HasPrefix(strings.ToLower(cfg.Name), "88code") {
		return true, ""
	}

	// 3. 检查缓存 (key级缓存：channelID:apiKey)
	cacheKey := fmt.Sprintf("%d:%s", cfg.ID, apiKey)
	if entry, ok := cache88Code.Load(cacheKey); ok {
		cached := entry.(cacheEntry)
		if time.Now().Before(cached.expiry) {
			return cached.available, cached.reason
		}
		cache88Code.Delete(cacheKey) // 缓存过期
	}

	// 4. 调用88code API
	subscription, err := fetch88CodeSubscription(ctx, apiKey)
	if err != nil {
		// API调用失败时的防御性策略：默认允许通过
		log.Printf("[WARN] 88code validator error for channel %s: %v (defaulting to available)", cfg.Name, err)
		return true, ""
	}

	// 5. 判断套餐类型
	isFree := strings.EqualFold(subscription, "FREE")
	reason := ""
	if !isFree {
		reason = fmt.Sprintf("subscription=%s (not FREE)", subscription)
	}

	// 6. 更新缓存
	cache88Code.Store(cacheKey, cacheEntry{
		available: isFree,
		reason:    reason,
		expiry:    time.Now().Add(cacheTTL),
	})

	return isFree, reason
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
//	{"data":{"subscriptionName": "FREE"}}
//	{"data":{"subscriptionName": "PRO"}}
func fetch88CodeSubscription(ctx context.Context, apiKey string) (string, error) {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxWithTimeout, http.MethodPost, "https://www.88code.org/api/usage", nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	var usageResp usage88CodeResponse
	if err := json.Unmarshal(body, &usageResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return usageResp.Data.SubscriptionName, nil
}
