package util

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

// HTTP状态码错误分类器
// 设计原则：区分Key级错误和渠道级错误，避免误判导致多Key功能失效

// ErrorLevel 错误级别枚举
// ErrUpstreamFirstByteTimeout 是上游首字节超时的统一错误标识，避免依赖具体报错文案
var ErrUpstreamFirstByteTimeout = errors.New("upstream first byte timeout")

// StatusFirstByteTimeout 是上游首字节超时时返回的自定义状态码（配合冷却策略使用）
const StatusFirstByteTimeout = 598

type ErrorLevel int

const (
	// ErrorLevelNone 无错误（2xx成功）
	ErrorLevelNone ErrorLevel = iota
	// ErrorLevelKey Key级错误：应该冷却当前Key，重试其他Key
	ErrorLevelKey
	// ErrorLevelChannel 渠道级错误：应该冷却整个渠道，切换到其他渠道
	ErrorLevelChannel
	// ErrorLevelClient 客户端错误：不应该冷却，直接返回给客户端
	ErrorLevelClient
)

// classifyHTTPStatus 分类HTTP状态码，返回错误级别
// 遵循SRP原则：单一职责 - 仅负责状态码分类
//
// 注意：401/403错误需要结合响应体内容进一步判断（通过ClassifyHTTPStatusWithBody）
func ClassifyHTTPStatus(statusCode int) ErrorLevel {
	// 2xx 成功
	if statusCode >= 200 && statusCode < 300 {
		return ErrorLevelNone
	}

	// 特殊状态码处理
	switch statusCode {
	case 499: // Client Closed Request
		// 499错误需要区分来源
		// 注意：此函数处理的是HTTP响应中的499状态码
		// 当499来自HTTP响应时，说明是上游API返回的状态码（不是下游客户端取消）
		// 可能原因：API服务器过载、限流、或内部错误
		// 应该切换到其他渠道重试
		//
		// context.Canceled（下游客户端取消）在ClassifyError中单独处理（line 156-158）
		// 那里返回的是499 + ErrorLevelClient（正确，不应重试）
		return ErrorLevelChannel

	// Key级错误：API Key相关问题（4xx客户端错误）
	case 400: // Bad Request - 通常是API Key格式错误或无效
		return ErrorLevelKey
	case 402: // Payment Required / 配额或余额不足等，需要切换Key
		return ErrorLevelKey
	case 401: // Unauthorized - 需要进一步分析（默认Key级）
		return ErrorLevelKey
	case 403: // Forbidden - 需要进一步分析（默认Key级）
		return ErrorLevelKey
	case 429: // Too Many Requests - Key限流（注意：也可能是IP限流，但优先假设Key级）
		return ErrorLevelKey

	// 渠道级错误：服务器端问题（5xx服务器错误）
	case 500: // Internal Server Error
		return ErrorLevelChannel
	case 502: // Bad Gateway
		return ErrorLevelChannel
	case 503: // Service Unavailable
		return ErrorLevelChannel
	case 504: // Gateway Timeout
		return ErrorLevelChannel
	case 520: // Web Server Returned an Unknown Error (Cloudflare) - 源服务器返回未知错误
		return ErrorLevelChannel
	case 521: // Web Server Is Down (Cloudflare) - 源服务器关闭
		return ErrorLevelChannel
	case 524: // A Timeout Occurred (Cloudflare) - 连接超时
		return ErrorLevelChannel

	// 其他4xx错误：默认为客户端错误（不冷却）
	// 例如：404 Not Found（模型不存在）、405 Method Not Allowed等
	case 404, 405, 406, 408, 410, 413, 414, 415, 416, 417:
		return ErrorLevelClient

	default:
		// 未知状态码：保守策略
		// 4xx范围 → 客户端错误（不冷却，避免误判）
		// 5xx范围 → 渠道级错误（冷却渠道）
		if statusCode >= 400 && statusCode < 500 {
			return ErrorLevelClient
		}
		if statusCode >= 500 {
			return ErrorLevelChannel
		}
		return ErrorLevelClient // 极端情况，默认不冷却
	}
}

// classifyHTTPStatusWithBody 基于状态码和响应体智能分类错误级别
// 针对401/403错误进行语义分析，区分Key级错误和渠道级错误
//
// 设计原则（遵循用户的ultrathink）：
// - 401/403错误**默认为Key级**，让handleProxyError根据渠道Key数量决定是否升级
// - 只有明确的"账户级不可逆错误"才分类为Channel级（如账户暂停、服务禁用）
// - "额度用尽"可能是单个Key的问题，应该先尝试其他Key
func ClassifyHTTPStatusWithBody(statusCode int, responseBody []byte) ErrorLevel {
	// ✅ 特殊处理：检测1308错误（可能以SSE error事件形式出现，HTTP状态码是200）
	// 1308错误表示达到使用上限，应该触发Key级冷却
	if _, has1308 := ParseResetTimeFrom1308Error(responseBody); has1308 {
		return ErrorLevelKey // 1308错误视为Key级错误，触发冷却
	}

	// 增加429错误的特殊处理
	if statusCode == 429 {
		// 429错误需要分析headers,但此函数没有headers参数
		// 为了向后兼容,这里默认返回Key级,由调用方使用ClassifyRateLimitError
		return ErrorLevelKey
	}

	// 仅分析401和403错误,其他状态码使用标准分类器
	if statusCode != 401 && statusCode != 403 {
		return ClassifyHTTPStatus(statusCode)
	}

	// 401/403错误:分析响应体内容
	if len(responseBody) == 0 {
		return ErrorLevelKey // 无响应体,默认Key级错误
	}

	bodyLower := strings.ToLower(string(responseBody))

	// 渠道级错误特征:**仅限账户级不可逆错误**
	// 设计原则:保守策略,只有明确是渠道级错误时才返回ErrorLevelChannel
	channelErrorPatterns := []string{
		// 账户状态(不可逆)
		"account suspended", // 账户暂停
		"account disabled",  // 账户禁用
		"account banned",    // 账户封禁
		"service disabled",  // 服务禁用

		// 注意:以下错误已移除(改为Key级,让系统先尝试其他Key):
		// - "额度已用尽", "quota_exceeded" → 可能只是单个Key额度用尽
		// - "余额不足", "balance" → 可能只是单个Key余额不足
		// - "limit reached" → 可能只是单个Key限额到达
	}

	for _, pattern := range channelErrorPatterns {
		if strings.Contains(bodyLower, pattern) {
			return ErrorLevelChannel // 明确的渠道级错误
		}
	}

	// 默认:Key级错误
	// 包括:认证失败、权限不足、额度用尽、余额不足等
	// 让handleProxyError根据渠道Key数量决定是否升级为渠道级
	return ErrorLevelKey
}

// ClassifyRateLimitError 分析429 Rate Limit错误的具体类型
// 增强429错误处理,区分Key级和渠道级限流
//
// 判断逻辑:
//  1. 检查Retry-After头: 如果>60秒,可能是IP/账户级限流 → 渠道级
//  2. 检查X-RateLimit-Scope: 如果是"global"或"ip" → 渠道级
//  3. 检查响应体中的错误描述
//  4. 默认: Key级(保守策略)
//
// 参数:
//   - headers: HTTP响应头
//   - responseBody: 响应体内容
//
// 返回:
//   - ErrorLevel: Key级或渠道级
func ClassifyRateLimitError(headers map[string][]string, responseBody []byte) ErrorLevel {
	// 1. 解析Retry-After头
	if retryAfterValues, ok := headers["Retry-After"]; ok && len(retryAfterValues) > 0 {
		retryAfter := retryAfterValues[0]

		// Retry-After可能是秒数或HTTP日期
		// 尝试解析为秒数
		if seconds, err := strconv.Atoi(retryAfter); err == nil {
			// ✅ 如果Retry-After > 60秒,可能是账户级或IP级限流
			// 这种长时间限流通常影响整个渠道
			if seconds > 60 {
				return ErrorLevelChannel
			}
		}
		// 如果是HTTP日期格式,通常表示长时间限流,也视为渠道级
		if _, err := time.Parse(time.RFC1123, retryAfter); err == nil {
			return ErrorLevelChannel
		}
	}

	// 2. 检查X-RateLimit-Scope头(某些API使用)
	if scopeValues, ok := headers["X-Ratelimit-Scope"]; ok && len(scopeValues) > 0 {
		scope := strings.ToLower(scopeValues[0])
		// global/ip级别的限流影响整个渠道
		if scope == "global" || scope == "ip" || scope == "account" {
			return ErrorLevelChannel
		}
	}

	// 3. 分析响应体中的错误描述
	if len(responseBody) > 0 {
		bodyLower := strings.ToLower(string(responseBody))

		// 渠道级限流特征
		channelPatterns := []string{
			"ip rate limit",      // IP级别限流
			"account rate limit", // 账户级别限流
			"global rate limit",  // 全局限流
			"organization limit", // 组织级别限流
		}

		for _, pattern := range channelPatterns {
			if strings.Contains(bodyLower, pattern) {
				return ErrorLevelChannel
			}
		}
	}

	// 4. 默认: Key级别限流(保守策略)
	// 让系统先尝试其他Key,如果所有Key都限流了,会自动升级为渠道级
	return ErrorLevelKey
}

// ParseResetTimeFrom1308Error 从1308错误响应中提取重置时间
// 错误格式: {"type":"error","error":{"type":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2025-12-09 18:08:11 重置。"},"request_id":"..."}
//
// 参数:
//   - responseBody: JSON格式的错误响应体
//
// 返回:
//   - time.Time: 解析出的重置时间（如果成功）
//   - bool: 是否成功解析（true表示是1308错误且成功提取时间）
func ParseResetTimeFrom1308Error(responseBody []byte) (time.Time, bool) {
	// 1. 使用sonic解析JSON（项目使用go_json tag指定了sonic）
	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	// 使用string类型避免sonic的未导出字段问题
	if err := json.Unmarshal(responseBody, &errResp); err != nil {
		return time.Time{}, false
	}

	// 2. 检查是否为1308错误
	if errResp.Error.Type != "1308" {
		return time.Time{}, false
	}

	// 3. 从message中提取时间字符串
	// 格式: "您的限额将在 2025-12-09 18:08:11 重置"
	msg := errResp.Error.Message
	
	// 查找"将在"和"重置"之间的内容
	startIdx := strings.Index(msg, "将在 ")
	endIdx := strings.Index(msg, " 重置")
	
	if startIdx == -1 || endIdx == -1 || startIdx >= endIdx {
		return time.Time{}, false
	}
	
	// 提取时间字符串（跳过"将在 "，包含到" 重置"之前）
	timeStr := strings.TrimSpace(msg[startIdx+len("将在 "):endIdx])
	
	// 4. 解析时间字符串（格式: 2025-12-09 18:08:11）
	resetTime, err := time.ParseInLocation("2006-01-02 15:04:05", timeStr, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	
	return resetTime, true
}

// ClassifyError 统一错误分类器（网络错误+HTTP错误）
// 将proxy_util.go中的classifyError和classifyErrorByString整合到此处
//
// 参数:
//   - err: 错误对象（可能是context错误、网络错误、或其他错误）
//
// 返回:
//   - statusCode: HTTP状态码（或内部错误码）
//   - errorLevel: 错误级别（Key级/渠道级/客户端级）
//   - shouldRetry: 是否应该重试
//
// 设计原则（DRY+SRP）:
//   - 统一入口处理所有错误分类
//   - 消除proxy_util.go中的重复逻辑
//   - 分层设计：快速路径（context错误）→ 网络错误 → 字符串匹配
func ClassifyError(err error) (statusCode int, errorLevel ErrorLevel, shouldRetry bool) {
	if err == nil {
		return 200, ErrorLevelNone, false
	}

	// 快速路径1：专门识别上游首字节超时，优先切换渠道
	if errors.Is(err, ErrUpstreamFirstByteTimeout) {
		return StatusFirstByteTimeout, ErrorLevelChannel, true
	}

	// 快速路径2：处理客户端主动取消
	if errors.Is(err, context.Canceled) {
		return 499, ErrorLevelClient, false // StatusClientClosedRequest
	}

	// 快速路径3：统一处理其它 DeadlineExceeded，默认视为上游超时
	if errors.Is(err, context.DeadlineExceeded) {
		return 504, ErrorLevelChannel, true // Gateway Timeout，触发渠道切换
	}

	// 快速路径4：检测net.Error的超时场景
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return 504, ErrorLevelChannel, true // Gateway Timeout，可重试
		}
	}

	// 慢速路径：回退到字符串匹配
	return classifyErrorByString(err.Error())
}

// classifyErrorByString 通过字符串匹配分类网络错误
// 从proxy_util.go迁移，作为ClassifyError的私有辅助函数
func classifyErrorByString(errStr string) (int, ErrorLevel, bool) {
	errLower := strings.ToLower(errStr)

	// Connection reset by peer - 不应重试
	if strings.Contains(errLower, "connection reset by peer") ||
		strings.Contains(errLower, "broken pipe") {
		return 499, ErrorLevelClient, false // StatusConnectionReset
	}

	// ✅ 空响应检测：上游返回200但Content-Length=0
	// 常见于CDN/代理错误、认证失败等异常场景，应触发渠道级重试
	if strings.Contains(errLower, "empty response") &&
		strings.Contains(errLower, "content-length: 0") {
		return 502, ErrorLevelChannel, true // 归类为Bad Gateway(上游异常)
	}

	// Connection refused - 应该重试其他渠道
	if strings.Contains(errLower, "connection refused") {
		return 502, ErrorLevelChannel, true
	}

	// 其他常见的网络连接错误也应该重试
	if strings.Contains(errLower, "no such host") ||
		strings.Contains(errLower, "host unreachable") ||
		strings.Contains(errLower, "network unreachable") ||
		strings.Contains(errLower, "connection timeout") ||
		strings.Contains(errLower, "no route to host") {
		return 502, ErrorLevelChannel, true
	}

	// 使用负值错误码，避免与HTTP状态码混淆
	// 其他网络错误 - 可以重试
	return -1, ErrorLevelChannel, true // ErrCodeNetworkRetryable
}
