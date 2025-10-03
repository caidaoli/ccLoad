package main

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
func classifyHTTPStatus(statusCode int) ErrorLevel {
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
	case 401: // Unauthorized - API Key未授权或过期
		return ErrorLevelKey
	case 403: // Forbidden - API Key没有权限
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
