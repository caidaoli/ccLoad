package util

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HTTP状态码错误分类器
// 设计原则：区分Key级错误和渠道级错误，避免误判导致多Key功能失效

// ErrorLevel 错误级别枚举
// ErrUpstreamFirstByteTimeout 是上游首字节超时的统一错误标识，避免依赖具体报错文案
var ErrUpstreamFirstByteTimeout = errors.New("upstream first byte timeout")

// resetTime1308Regex 匹配1308错误 message 中的重置时间（不依赖具体语言文案）
// 格式示例: 2025-12-09 18:08:11
var resetTime1308Regex = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)

// HTTP 状态码常量（统一定义，避免魔法数字）
const (
	// StatusClientClosedRequest 客户端取消请求（Nginx扩展状态码）
	// 来源：(1) context.Canceled → 不重试  (2) 上游返回499 → 重试其他渠道
	StatusClientClosedRequest = 499

	// StatusFirstByteTimeout 上游首字节超时（自定义状态码，触发渠道级冷却）
	StatusFirstByteTimeout = 598

	// StatusStreamIncomplete 流式响应不完整（自定义状态码）
	// 触发条件：流正常结束但没有usage数据，或流传输中断
	StatusStreamIncomplete = 599

	// ErrCodeNetworkRetryable 可重试的网络错误（内部标识符，非HTTP状态码）
	// 使用负值避免与HTTP状态码混淆
	ErrCodeNetworkRetryable = -1
)

// Rate Limit 相关常量
const (
	// RetryAfterThresholdSeconds Retry-After超过此值视为渠道级限流
	RetryAfterThresholdSeconds = 60
	// RateLimitScope 常量
	RateLimitScopeGlobal  = "global"
	RateLimitScopeIP      = "ip"
	RateLimitScopeAccount = "account"
)

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

// statusCodeClassification 状态码→错误级别映射表
// 设计原则：表驱动替代巨型switch，提高可维护性
var statusCodeClassification = map[int]ErrorLevel{
	// 499: 上游返回的客户端关闭请求，应切换渠道重试
	// 注意：context.Canceled 在 ClassifyError 中单独处理
	499: ErrorLevelChannel,

	// Key级错误：API Key相关问题
	400: ErrorLevelKey, // Bad Request - API Key格式错误
	401: ErrorLevelKey, // Unauthorized - 默认Key级，需结合响应体分析
	402: ErrorLevelKey, // Payment Required - 配额或余额不足
	403: ErrorLevelKey, // Forbidden - 默认Key级，需结合响应体分析
	429: ErrorLevelKey, // Too Many Requests - Key限流

	// 渠道级错误：服务器端问题
	500: ErrorLevelChannel, // Internal Server Error
	502: ErrorLevelChannel, // Bad Gateway
	503: ErrorLevelChannel, // Service Unavailable
	504: ErrorLevelChannel, // Gateway Timeout
	520: ErrorLevelChannel, // Cloudflare: Unknown Error
	521: ErrorLevelChannel, // Cloudflare: Web Server Is Down
	524: ErrorLevelChannel, // Cloudflare: A Timeout Occurred

	// 自定义内部状态码
	StatusFirstByteTimeout: ErrorLevelChannel, // 598 上游首字节超时
	StatusStreamIncomplete: ErrorLevelChannel, // 599 流式响应不完整

	// 客户端错误：不冷却，直接返回
	404: ErrorLevelClient, // Not Found
	405: ErrorLevelClient, // Method Not Allowed
	406: ErrorLevelClient, // Not Acceptable
	408: ErrorLevelClient, // Request Timeout
	410: ErrorLevelClient, // Gone
	413: ErrorLevelClient, // Payload Too Large
	414: ErrorLevelClient, // URI Too Long
	415: ErrorLevelClient, // Unsupported Media Type
	416: ErrorLevelClient, // Range Not Satisfiable
	417: ErrorLevelClient, // Expectation Failed
}

func ClassifyHTTPStatus(statusCode int) ErrorLevel {
	// 2xx 成功
	if statusCode >= 200 && statusCode < 300 {
		return ErrorLevelNone
	}

	// 表驱动：状态码 → 错误级别映射
	if level, ok := statusCodeClassification[statusCode]; ok {
		return level
	}

	// 默认规则：4xx → Client, 5xx → Channel
	if statusCode >= 400 && statusCode < 500 {
		return ErrorLevelClient
	}
	if statusCode >= 500 {
		return ErrorLevelChannel
	}
	return ErrorLevelClient
}

// classifyHTTPStatusWithBody 基于状态码和响应体智能分类错误级别
// 针对401/403错误进行语义分析，区分Key级错误和渠道级错误
//
// 设计原则（遵循用户的ultrathink）：
// - 401/403错误**默认为Key级**，让handleProxyError根据渠道Key数量决定是否升级
// - 只有明确的"账户级不可逆错误"才分类为Channel级（如账户暂停、服务禁用）
// - "额度用尽"可能是单个Key的问题，应该先尝试其他Key
func ClassifyHTTPStatusWithBody(statusCode int, responseBody []byte) ErrorLevel {
	// [INFO] 特殊处理：检测1308错误（可能以SSE error事件形式出现，HTTP状态码是200）
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
			// [INFO] 如果Retry-After > 阈值,可能是账户级或IP级限流
			// 这种长时间限流通常影响整个渠道
			if seconds > RetryAfterThresholdSeconds {
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
		if scope == RateLimitScopeGlobal || scope == RateLimitScopeIP || scope == RateLimitScopeAccount {
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
// [FIX] 使用正则匹配时间格式，不再依赖中文文案（如"将在"/"重置"）
// 这样即使上游修改错误消息措辞或切换语言，只要包含 YYYY-MM-DD HH:MM:SS 格式的时间就能正确解析
//
// 参数:
//   - responseBody: JSON格式的错误响应体
//
// 返回:
//   - time.Time: 解析出的重置时间（如果成功）
//   - bool: 是否成功解析（true表示是1308错误且成功提取时间）
func ParseResetTimeFrom1308Error(responseBody []byte) (time.Time, bool) {
	// 1. 解析JSON结构
	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(responseBody, &errResp); err != nil {
		return time.Time{}, false
	}

	// 2. 检查是否为1308错误
	if errResp.Error.Type != "1308" {
		return time.Time{}, false
	}

	// 3. 使用正则从message中提取时间字符串（不依赖具体语言文案）
	// 匹配格式: YYYY-MM-DD HH:MM:SS
	timeStr := resetTime1308Regex.FindString(errResp.Error.Message)
	if timeStr == "" {
		return time.Time{}, false
	}

	// 4. 解析时间字符串
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

	// [INFO] 空响应检测：上游返回200但Content-Length=0
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
