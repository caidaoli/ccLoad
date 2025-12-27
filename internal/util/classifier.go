package util

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
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

	// StatusQuotaExceeded 1308配额超限（自定义状态码）
	// 即使HTTP状态码为200，但响应体为1308错误。需从成功率计算中排除
	StatusQuotaExceeded = 596

	// StatusSSEError SSE流中检测到error事件（自定义状态码）
	// HTTP状态码200但流中包含错误，如其他类型的API错误
	StatusSSEError = 597

	// StatusFirstByteTimeout 上游首字节超时（自定义状态码，触发渠道级冷却）
	StatusFirstByteTimeout = 598

	// StatusStreamIncomplete 流式响应不完整（自定义状态码）
	// 触发条件：流正常结束但没有usage数据，或流传输中断
	StatusStreamIncomplete = 599
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

// StatusCodeMeta 状态码元数据（统一定义错误级别）
// 设计原则：单一数据源，消除 proxy_handler.go / classifier.go 分散的状态码分类逻辑。
//
// 注意：对外状态码映射不应该掺进这个表里，否则很快就会变成另一份“半套规则”。
type StatusCodeMeta struct {
	Level ErrorLevel // 错误级别（Key/Channel/Client）
}

type HTTPResponseClassification struct {
	Level            ErrorLevel
	ResetTime1308    time.Time
	HasResetTime1308 bool
}

// sseErrorResponse SSE error事件的JSON结构（Anthropic API / 88code API）
// [FIX] 提取为公共结构体，消除 classifySSEError 和 ParseResetTimeFrom1308Error 的重复定义
type sseErrorResponse struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"` // Anthropic使用
		Code    string `json:"code"` // 其他渠道使用
		Message string `json:"message"`
	} `json:"error"`
}

// ErrorType 返回错误类型（优先使用type字段，如果为空则使用code字段）
// [FIX] 消除重复的errorType判断逻辑
func (r *sseErrorResponse) ErrorType() string {
	if r.Error.Type != "" {
		return r.Error.Type
	}
	return r.Error.Code
}

// statusCodeMetaMap 状态码元数据映射表
// 设计原则：表驱动替代分散的 switch/map，提高可维护性
var statusCodeMetaMap = map[int]StatusCodeMeta{
	// === 客户端取消 ===
	// 499: 上游返回的客户端关闭请求，应切换渠道重试
	// 注意：context.Canceled 在 ClassifyError 中单独处理
	499: {ErrorLevelChannel},

	// === Key级错误：API Key相关问题 ===
	// 这些错误在本系统中属于"后端Key/渠道配置问题"，不应甩锅给客户端
	401: {ErrorLevelKey}, // Unauthorized - Key invalid
	402: {ErrorLevelKey}, // Payment Required - quota/balance
	403: {ErrorLevelKey}, // Forbidden - Key permission
	429: {ErrorLevelKey}, // Too Many Requests - rate limited

	// === 渠道级错误：服务器端问题 ===
	500: {ErrorLevelChannel}, // Internal Server Error
	502: {ErrorLevelChannel}, // Bad Gateway
	503: {ErrorLevelChannel}, // Service Unavailable
	504: {ErrorLevelChannel}, // Gateway Timeout
	520: {ErrorLevelChannel}, // Cloudflare: Unknown Error
	521: {ErrorLevelChannel}, // Cloudflare: Web Server Is Down
	524: {ErrorLevelChannel}, // Cloudflare: A Timeout Occurred

	// === 自定义内部状态码 ===
	StatusQuotaExceeded:    {ErrorLevelKey},     // 1308 quota exceeded
	StatusSSEError:         {ErrorLevelKey},     // SSE error event
	StatusFirstByteTimeout: {ErrorLevelChannel}, // First byte timeout
	StatusStreamIncomplete: {ErrorLevelChannel}, // Stream incomplete

	// === 客户端错误：不冷却，直接返回 ===
	// 408 Request Timeout: RFC 7231 定义为"服务器等待客户端发送完整请求超时"（客户端慢）
	408: {ErrorLevelClient}, // Request Timeout - client slow
	// 405 Method Not Allowed: 在代理场景下，这更可能意味着上游 endpoint/路由配置错误（方法不被支持）
	// 作为渠道级故障处理：触发渠道冷却。
	405: {ErrorLevelChannel}, // Method Not Allowed
	406: {ErrorLevelClient},  // Not Acceptable
	410: {ErrorLevelClient},  // Gone
	413: {ErrorLevelClient},  // Payload Too Large
	414: {ErrorLevelClient},  // URI Too Long
	415: {ErrorLevelClient},  // Unsupported Media Type
	416: {ErrorLevelClient},  // Range Not Satisfiable
	417: {ErrorLevelClient},  // Expectation Failed
}

// GetStatusCodeMeta 获取状态码元数据（统一入口）
func GetStatusCodeMeta(status int) StatusCodeMeta {
	if meta, ok := statusCodeMetaMap[status]; ok {
		return meta
	}
	// 默认行为
	if status >= 500 {
		return StatusCodeMeta{ErrorLevelChannel}
	}
	if status >= 400 {
		return StatusCodeMeta{ErrorLevelClient}
	}
	return StatusCodeMeta{ErrorLevelClient}
}

// ClientStatusFor 将 status 映射为对外暴露的状态码。
//
// 设计目标：
// - 对外语义一致：不把后端 Key/渠道故障伪装成“客户端错误”
// - 单一映射入口：避免在 app 层再堆一份 if/switch（那就是第二套规则）
func ClientStatusFor(status int) int {
	if status <= 0 {
		return http.StatusBadGateway
	}

	// 内部状态码：无条件映射为标准 HTTP 语义值
	switch status {
	case StatusQuotaExceeded:
		return http.StatusTooManyRequests
	case StatusSSEError:
		return http.StatusBadGateway
	case StatusFirstByteTimeout:
		return http.StatusGatewayTimeout
	case StatusStreamIncomplete:
		return http.StatusBadGateway
	}

	// 透明代理原则：透传所有上游状态码，不篡改HTTP语义
	return status
}

// ClassifyHTTPStatus 分类HTTP状态码，返回错误级别
// 注意：401/403/429 需要结合响应体/headers进一步判断（通过ClassifyHTTPResponse）
func ClassifyHTTPStatus(statusCode int) ErrorLevel {
	if statusCode >= 200 && statusCode < 300 {
		return ErrorLevelNone
	}
	return GetStatusCodeMeta(statusCode).Level
}

// ClassifyHTTPResponseWithMeta 基于状态码 + headers + 响应体智能分类错误级别
// 返回 HTTPResponseClassification，包含错误级别和1308重置时间（如果存在）
//
// 分类策略：
//   - 401/403 做语义分析：默认 Key 级，只在明确账户级不可逆错误时升级为 Channel 级
//   - 429 做限流范围分析：默认 Key 级，只有明确长时间/全局限流特征才升级为 Channel 级
//   - 1308 错误优先：无论 HTTP 状态码，检测到就按 Key 级处理（用于精确冷却时间）
//   - 其他状态码：走表驱动分类（statusCodeMetaMap）
func ClassifyHTTPResponseWithMeta(statusCode int, headers map[string][]string, responseBody []byte) HTTPResponseClassification {
	// [INFO] 特殊处理：检测1308错误（可能以SSE error事件形式出现，HTTP状态码是200）
	// 1308错误表示达到使用上限，应该触发Key级冷却
	if resetTime, has1308 := ParseResetTimeFrom1308Error(responseBody); has1308 {
		return HTTPResponseClassification{
			Level:            ErrorLevelKey,
			ResetTime1308:    resetTime,
			HasResetTime1308: true,
		}
	}

	// [INFO] 597 SSE error事件：解析实际错误类型动态判断级别
	// SSE error JSON格式: {"type":"error","error":{"type":"api_error","message":"上游API返回错误: 500"}}
	// 根据error.type判断：api_error/overloaded_error → 渠道级，其他 → Key级
	if statusCode == StatusSSEError {
		return HTTPResponseClassification{Level: classifySSEError(responseBody)}
	}

	// 429错误：需要结合 headers 判断限流范围
	if statusCode == 429 {
		if headers != nil {
			return HTTPResponseClassification{Level: classifyRateLimitError(headers, responseBody)}
		}
		return HTTPResponseClassification{Level: ErrorLevelKey}
	}

	// 400错误：根据响应体智能分类
	if statusCode == 400 {
		return HTTPResponseClassification{Level: classify400Error(responseBody)}
	}

	// 404错误：根据响应体智能分类
	if statusCode == 404 {
		return HTTPResponseClassification{Level: classify404Error(responseBody)}
	}

	// 仅分析401和403错误,其他状态码使用标准分类器
	if statusCode != 401 && statusCode != 403 {
		return HTTPResponseClassification{Level: ClassifyHTTPStatus(statusCode)}
	}

	// 401/403错误:分析响应体内容
	if len(responseBody) == 0 {
		return HTTPResponseClassification{Level: ErrorLevelKey} // 无响应体,默认Key级错误
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
			return HTTPResponseClassification{Level: ErrorLevelChannel} // 明确的渠道级错误
		}
	}

	// 默认:Key级错误
	// 包括:认证失败、权限不足、额度用尽、余额不足等
	// 让handleProxyError根据渠道Key数量决定是否升级为渠道级
	return HTTPResponseClassification{Level: ErrorLevelKey}
}

// classifyRateLimitError 分析429 Rate Limit错误的具体类型
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
func classifyRateLimitError(headers map[string][]string, responseBody []byte) ErrorLevel {
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

// classifySSEError 分析SSE error事件的具体类型
// SSE error JSON格式: {"type":"error","error":{"type":"api_error","message":"上游API返回错误: 500"}}
//
// 判断逻辑:
//   - api_error: 上游服务错误（通常是5xx）→ 渠道级
//   - overloaded_error: 上游过载 → 渠道级
//   - rate_limit_error: 限流错误 → Key级（可能只是单个Key限流）
//   - authentication_error: 认证错误 → Key级
//   - invalid_request_error: 请求错误 → Key级
//   - 其他/解析失败: 默认Key级（保守策略）
func classifySSEError(responseBody []byte) ErrorLevel {
	if len(responseBody) == 0 {
		return ErrorLevelKey
	}

	// 解析SSE error JSON
	// [FIX] 支持两种格式：
	//   1. Anthropic格式: {"type":"error", "error":{"type":"1308", ...}}
	//   2. 其他渠道格式: {"error":{"code":"1308", ...}}
	var errResp sseErrorResponse

	if err := json.Unmarshal(responseBody, &errResp); err != nil {
		return ErrorLevelKey // 解析失败，保守处理
	}

	// 根据error.type/code判断错误级别
	switch errResp.ErrorType() {
	case "api_error", "overloaded_error":
		// 上游服务错误或过载 → 渠道级冷却
		return ErrorLevelChannel
	case "rate_limit_error", "authentication_error", "invalid_request_error", "1308":
		// 限流/认证/请求错误 → Key级冷却
		return ErrorLevelKey
	default:
		// 未知错误类型，保守处理为Key级
		return ErrorLevelKey
	}
}

// classify400Error 根据响应体内容智能分类 400 错误
// 设计原则：90%+ 的 400 错误是客户端请求参数错误，只有极少数是 Key 级错误（如 invalid_api_key）
func classify400Error(responseBody []byte) ErrorLevel {
	if len(responseBody) == 0 {
		return ErrorLevelClient // 空响应体 = 客户端错误
	}
	bodyLower := strings.ToLower(string(responseBody))

	// Key 级特征（罕见）
	if strings.Contains(bodyLower, "invalid_api_key") ||
		strings.Contains(bodyLower, "api key") {
		return ErrorLevelKey
	}

	// 默认：客户端级（参数错误、格式错误等）
	return ErrorLevelClient
}

// classify404Error 根据响应体内容智能分类 404 错误
// 设计原则：需要区分两种情况
//   - BaseURL 配置错误（渠道级）：上游返回 HTML 错误页面
//   - 模型不存在（客户端级）：上游返回 JSON 错误消息
func classify404Error(responseBody []byte) ErrorLevel {
	if len(responseBody) == 0 {
		return ErrorLevelChannel // 空响应 = 路径错误，渠道配置问题
	}
	bodyLower := strings.ToLower(string(responseBody))

	// 客户端级特征
	if strings.Contains(bodyLower, "model_not_found") ||
		strings.Contains(bodyLower, "does not exist") {
		return ErrorLevelClient
	}

	// 渠道级特征（HTML 错误页面 = BaseURL 配错）
	if strings.Contains(bodyLower, "<!doctype html") ||
		strings.Contains(bodyLower, "<html") {
		return ErrorLevelChannel
	}

	// 默认：客户端级（保守策略）
	return ErrorLevelClient
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
	// [FIX] 支持两种格式：
	//   1. Anthropic格式: {"type":"error", "error":{"type":"1308", ...}}
	//   2. 其他渠道格式: {"error":{"code":"1308", ...}}
	var errResp sseErrorResponse

	if err := json.Unmarshal(responseBody, &errResp); err != nil {
		return time.Time{}, false
	}

	// 2. 检查是否为1308错误（优先使用type，如果为空则使用code）
	if errResp.ErrorType() != "1308" {
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

	// broken pipe - 客户端主动断开连接，完全不重试
	if strings.Contains(errLower, "broken pipe") {
		return 499, ErrorLevelClient, false
	}

	// connection reset by peer - 通常是对端（上游）突然断开连接
	// 这不是“客户端取消”的语义，内部统一按 502 处理以进入健康度统计，并允许切换渠道重试。
	if strings.Contains(errLower, "connection reset by peer") {
		return 502, ErrorLevelChannel, true
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

	// HTTP/2 流级错误 - 上游服务器主动关闭流或内部错误
	// 常见原因：上游负载过高、服务崩溃、网络中间件超时、CDN断开
	// 应触发渠道级重试（切换到其他渠道）
	if strings.Contains(errLower, "http2: response body closed") ||
		strings.Contains(errLower, "stream error:") {
		return 502, ErrorLevelChannel, true // Bad Gateway - 上游服务异常
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
	// 对外/日志统一使用标准HTTP语义：502 Bad Gateway
	return 502, ErrorLevelChannel, true
}
