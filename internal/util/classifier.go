package util

import (
	"context"
	"errors"
	"net"
	"strings"
)

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
		// ✅ P3修复（2025-10-28）：499错误需要区分来源
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

// ClassifyError 统一错误分类器（网络错误+HTTP错误）
// ✅ P0重构：将proxy_util.go中的classifyError和classifyErrorByString整合到此处
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

	// ✅ 快速路径1：优先检查最常见的错误类型（避免字符串操作）
	// Context canceled - 客户端主动取消，不应重试（最常见）
	if errors.Is(err, context.Canceled) {
		return 499, ErrorLevelClient, false // StatusClientClosedRequest
	}

	// ⚠️ Context deadline exceeded 需要区分两种情况：
	// 1. 客户端超时（来自客户端设置的超时）- 不应重试
	// 2. 上游服务器响应慢导致的超时 - 应该重试其他渠道
	// ✅ P0修复 (2025-10-13): 默认将DeadlineExceeded视为上游超时（可重试）
	// 设计原则：
	// - 客户端主动取消通常是context.Canceled，而不是DeadlineExceeded
	// - 保守策略：宁可多重试（提升可用性），也不要漏掉上游超时（导致可用性下降）
	// - 兼容性：不依赖特定的错误消息格式，适配Go不同版本和HTTP客户端实现
	if errors.Is(err, context.DeadlineExceeded) {
		// 所有DeadlineExceeded错误默认为上游超时，应该重试其他渠道
		return 504, ErrorLevelChannel, true // ✅ Gateway Timeout，触发渠道切换
	}

	// ✅ 快速路径2：检查系统级错误（使用类型断言替代字符串匹配）
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return 504, ErrorLevelChannel, true // Gateway Timeout，可重试
		}
	}

	// ✅ 慢速路径：字符串匹配（<1%的错误会到达这里）
	return classifyErrorByString(err.Error())
}

// classifyErrorByString 通过字符串匹配分类网络错误
// ✅ P0重构：从proxy_util.go迁移，作为ClassifyError的私有辅助函数
func classifyErrorByString(errStr string) (int, ErrorLevel, bool) {
	errLower := strings.ToLower(errStr)

	// Connection reset by peer - 不应重试
	if strings.Contains(errLower, "connection reset by peer") ||
		strings.Contains(errLower, "broken pipe") {
		return 499, ErrorLevelClient, false // StatusConnectionReset
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

	// ✅ P2-3 修复：使用负值错误码，避免与HTTP状态码混淆
	// 其他网络错误 - 可以重试
	return -1, ErrorLevelChannel, true // ErrCodeNetworkRetryable
}
