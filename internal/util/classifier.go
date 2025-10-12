package util

import "strings"

// HTTP状态码错误分类器
// 设计原则：区分Key级错误和渠道级错误，避免误判导致多Key功能失效

// ErrorLevel 错误级别枚举
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
		return ErrorLevelClient

	// Key级错误：API Key相关问题（4xx客户端错误）
	case 400: // Bad Request - 通常是API Key格式错误或无效
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
	// 仅分析401和403错误，其他状态码使用标准分类器
	if statusCode != 401 && statusCode != 403 {
		return ClassifyHTTPStatus(statusCode)
	}

	// 401/403错误：分析响应体内容
	if len(responseBody) == 0 {
		return ErrorLevelKey // 无响应体，默认Key级错误
	}

	bodyLower := strings.ToLower(string(responseBody))

	// 渠道级错误特征：**仅限账户级不可逆错误**
	// 设计原则：保守策略，只有明确是渠道级错误时才返回ErrorLevelChannel
	channelErrorPatterns := []string{
		// 账户状态（不可逆）
		"account suspended", // 账户暂停
		"account disabled",  // 账户禁用
		"account banned",    // 账户封禁
		"service disabled",  // 服务禁用

		// 注意：以下错误已移除（改为Key级，让系统先尝试其他Key）：
		// - "额度已用尽", "quota_exceeded" → 可能只是单个Key额度用尽
		// - "余额不足", "balance" → 可能只是单个Key余额不足
		// - "limit reached" → 可能只是单个Key限额到达
	}

	for _, pattern := range channelErrorPatterns {
		if strings.Contains(bodyLower, pattern) {
			return ErrorLevelChannel // 明确的渠道级错误
		}
	}

	// 默认：Key级错误
	// 包括：认证失败、权限不足、额度用尽、余额不足等
	// 让handleProxyError根据渠道Key数量决定是否升级为渠道级
	return ErrorLevelKey
}
