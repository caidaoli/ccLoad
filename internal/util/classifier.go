package util

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/protocol"
)

// HTTP状态码错误分类器
// 设计原则：区分Key级错误和渠道级错误，避免误判导致多Key功能失效

// ErrUpstreamFirstByteTimeout 是上游首字节超时的统一错误标识，避免依赖具体报错文案。
var ErrUpstreamFirstByteTimeout = errors.New("upstream first byte timeout")

// ErrUpstreamStreamTimeout 是上游流式请求总超时的统一错误标识。
var ErrUpstreamStreamTimeout = errors.New("upstream stream timeout")

// ErrUpstreamEmptyResponse 是上游 200 但无响应体的统一错误标识。
var ErrUpstreamEmptyResponse = errors.New("upstream returned empty response")

// ErrUpstreamInvalidResponse 是上游未返回可用 API 响应的统一错误标识：2xx 正文不是 API 响应，或重定向到其他源。
var ErrUpstreamInvalidResponse = errors.New("upstream returned invalid response")

// ErrUpstreamPromptBlocked 表示上游安全策略拦截了提示词本身（Gemini 不带 candidates 的
// promptFeedback.blockReason）。结果由请求内容决定，换 Key 或渠道都一样。
var ErrUpstreamPromptBlocked = errors.New("upstream blocked the prompt")

// resetTime1308Regex 匹配1308错误 message 中的重置时间（不依赖具体语言文案）
// 格式示例: 2025-12-09 18:08:11
var resetTime1308Regex = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)

// beijingTomorrowResetRegex 匹配类似“明天凌晨3点13分（北京时间）恢复”的相对重置时间。
var beijingTomorrowResetRegex = regexp.MustCompile(`明天\s*(?:凌晨|早上|上午)?\s*(\d{1,2})\s*点\s*(?:(\d{1,2})\s*分)?`)

// retryInDurationRegex 匹配 Gemini RESOURCE_EXHAUSTED 中的 “Please retry in 59.409754061s”。
var retryInDurationRegex = regexp.MustCompile(`(?i)\bretry\s+in\s+([0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h)(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h))*)`)

// tryAgainInHMRegex 匹配 “Try again in 2h 18m” / “1.5h” / “2 hours 5 minutes”。
// 末尾词边界避免把 5ms 读成 5m。
const tryAgainInPart = `\d+(?:\.\d+)?\s*(?:hours?|minutes?|seconds?|h|m|s)`

var tryAgainInHMRegex = regexp.MustCompile(`(?i)\bagain\s+in\s+(` + tryAgainInPart + `(?:\s*(?:and\s+)?` + tryAgainInPart + `)*)\b`)
var tryAgainInPartRegex = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(hours?|minutes?|seconds?|h|m|s)`)

// retryAfterSecondsRegex 匹配 Codex rolling spend limit 文案中的 “Please retry after 2196 seconds”。
var retryAfterSecondsRegex = regexp.MustCompile(`(?i)\bretry\s+after\s+([0-9]+)\s*seconds?\b`)

// rollingFreeAllowanceResetRegex 匹配 Token Harbor 免费额度的下一个滚动周期起点。
var rollingFreeAllowanceResetRegex = regexp.MustCompile(`(?i)\bnext\s+rolling\s+7-day\s+period\s+starts\s+at\s+(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))\b`)

var xaiFreeUsageModelRegex = regexp.MustCompile(`(?i)\bfor\s+model\s+["\x60']?([a-z0-9][a-z0-9._/-]*)`)

// globalFixedWindowRetryClockRegex 匹配“请在 今天 12:00 后再试”这类全站固定窗口限额文案。
var globalFixedWindowRetryClockRegex = regexp.MustCompile(`(今天|明天)\s*(\d{1,2})\s*[:：]\s*(\d{1,2})`)

// codexUsageFrequencyLimitResetRegex 匹配 Codex 频率限制错误中的绝对重置时间。
// 文案可能是英文或中文，但时间格式固定为 YYYY-MM-DD HH:MM:SS UTC+8。
var codexUsageFrequencyLimitResetRegex = regexp.MustCompile(`(?i)(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})\s+UTC\s*([+-])\s*(\d{1,2})(?:\s*:\s*(\d{2}))?`)

// HTTP 状态码常量（统一定义，避免魔法数字）
const (
	// StatusClientClosedRequest 客户端取消请求（Nginx扩展状态码）
	// 来源：(1) context.Canceled → 不重试  (2) 上游返回499 → 冷却当前模型并切换渠道
	StatusClientClosedRequest = 499

	// StatusQuotaExceeded 1308配额超限（自定义状态码）
	// 即使HTTP状态码为200，但响应体为1308错误。需从成功率计算中排除
	StatusQuotaExceeded = 596

	// StatusSSEError SSE流中检测到error事件（自定义状态码）
	// HTTP状态码200但流中包含错误，如其他类型的API错误
	StatusSSEError = 597

	// StatusFirstByteTimeout 上游首字节超时（自定义状态码，触发模型级冷却）
	StatusFirstByteTimeout = 598

	// StatusStreamIncomplete 流式响应不完整（自定义状态码）
	// 触发条件：流正常结束但没有usage数据，或流传输中断；触发模型级冷却
	StatusStreamIncomplete = 599
)

// Rate Limit 相关常量
const (
	// RetryAfterThresholdSeconds Retry-After超过此值视为渠道级限流
	RetryAfterThresholdSeconds     = 60
	CodexUsageFrequencyLimitReason = "CODEX_USAGE_FREQUENCY_LIMIT" ///< 6004 模型额度错误给出了有效重置时间。
	// anthropicRateLimitUnifiedResetHeader 是 Anthropic 当前被拒绝配额窗口的 Unix 秒重置时间。
	anthropicRateLimitUnifiedResetHeader = "Anthropic-Ratelimit-Unified-Reset"
	// anthropicMaxRateLimitWindow 是 Anthropic 最长限流窗口（7d）加 1h 时钟偏差余量；
	// 更晚的时间戳是计费周期边界，不是可恢复的限流。
	anthropicMaxRateLimitWindow = 7*24*time.Hour + time.Hour
	// WebsocketConnectionLimitCooldown 是上游 WebSocket 连接寿命到期、同渠道重连仍未恢复时的渠道冷却时长。
	// 换新连接即可继续，冷却只需覆盖“切走再回来”的窗口，绝不能走指数退避。
	WebsocketConnectionLimitCooldown = 5 * time.Second
	// xaiFreeUsageExhaustedCooldown 是 xAI 免费额度声明的滚动窗口上限。
	xaiFreeUsageExhaustedCooldown = 24 * time.Hour
	// RateLimitScope 常量
	RateLimitScopeGlobal  = "global"
	RateLimitScopeIP      = "ip"
	RateLimitScopeAccount = "account"
)

// ErrorLevel 表示错误的严重级别。
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
	ErrorLevelCapability ///< 当前渠道无法满足本次请求的能力要求，其他请求仍可使用它。
)

// StatusCodeMeta 状态码元数据（统一定义错误级别）
// 设计原则：单一数据源，消除 proxy_handler.go / classifier.go 分散的状态码分类逻辑。
//
// 注意：对外状态码映射不应该掺进这个表里，否则很快就会变成另一份“半套规则”。
type StatusCodeMeta struct {
	Level ErrorLevel // 错误级别（Key/Channel/Client）
}

// HTTPResponseClassification 包含 HTTP 响应分类的结果。
type HTTPResponseClassification struct {
	Level                 ErrorLevel
	Model                 string
	ModelScoped           bool
	PreventKeyFallback    bool
	ModelCooldownUntil    time.Time
	HasModelCooldownUntil bool
	ModelCooldownReason   string
	KeyCooldownUntil      time.Time
	HasKeyCooldownUntil   bool
	KeyCooldownReason     string
	// CredentialScoped 表示故障属于整个上游凭证（如 Anthropic 5h/7d 窗口被拒）：
	// 有独立 Key 时冷却该 Key，OAuth 渠道（无独立 Key）冷却整个渠道，而不是收窄到模型。
	CredentialScoped        bool
	ChannelCooldownUntil    time.Time
	HasChannelCooldownUntil bool
	ChannelCooldownReason   string
}

// sseErrorResponse SSE error事件的通用JSON结构（兼容 error.type / error.code）
// [FIX] 提取为公共结构体，消除 classifySSEError 和 ParseResetTimeFrom1308Error 的重复定义
type sseErrorResponse struct {
	Type     string         `json:"type"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Error    sseErrorDetail `json:"error"`
	Response struct {
		Error sseErrorDetail `json:"error"`
	} `json:"response"`
}

type sseErrorDetail struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

/**
 * @brief 上游配额错误的顶层字段和 Responses 事件的错误容器。
 */
type structuredQuotaErrorResponse struct {
	Code            any                          `json:"code"`
	Message         string                       `json:"message"`
	Msg             string                       `json:"msg"`
	Model           string                       `json:"model"`
	ResetSeconds    int64                        `json:"reset_seconds"`
	ResetsInSeconds int64                        `json:"resets_in_seconds"` // 部分上游使用复数形式
	ResetsAt        int64                        `json:"resets_at"`         // unix 时间戳
	ResetTime       string                       `json:"reset_time"`
	Status          string                       `json:"status"`
	Details         []structuredQuotaErrorDetail `json:"details"`
	Error           json.RawMessage              `json:"error"`
	Response        json.RawMessage              `json:"response"` ///< response.failed 事件中的 response.error 容器
}

type structuredQuotaErrorObject struct {
	Type            any                          `json:"type"`
	Code            any                          `json:"code"`
	Message         string                       `json:"message"`
	Msg             string                       `json:"msg"`
	Model           string                       `json:"model"`
	ResetSeconds    int64                        `json:"reset_seconds"`
	ResetsInSeconds int64                        `json:"resets_in_seconds"` // 部分上游使用复数形式
	ResetsAt        int64                        `json:"resets_at"`         // unix 时间戳
	ResetTime       string                       `json:"reset_time"`
	Status          string                       `json:"status"`
	Details         []structuredQuotaErrorDetail `json:"details"`
}

type structuredQuotaErrorDetail struct {
	Reason     string `json:"reason"`
	RetryDelay string `json:"retryDelay"` ///< google.rpc.RetryInfo 的恢复时长，如 "37s"
	Metadata   struct {
		Model               string `json:"model"`
		QuotaResetDelay     string `json:"quotaResetDelay"`
		QuotaResetTimeStamp string `json:"quotaResetTimeStamp"`
	} `json:"metadata"`
}

type structuredQuotaError struct {
	code                string
	message             string
	model               string
	resetSeconds        int64
	resetsAt            int64 // unix 时间戳（秒）
	resetTime           string
	status              string
	quotaResetDelay     string
	quotaResetTimeStamp string
	retryDelay          string ///< google.rpc.RetryInfo.retryDelay，上游明确给出的恢复时长
}

// ErrorType 返回错误类型（优先使用type字段，如果为空则使用code字段）
// [FIX] 消除重复的errorType判断逻辑
func (r *sseErrorResponse) ErrorType() string {
	if r.Error.Type != "" {
		return r.Error.Type
	}
	return r.Error.Code
}

/**
 * @brief Identifies an upstream context overflow in native Anthropic, OpenAI, or Google errors.
 * @param responseBody HTTP error body or SSE error payload.
 * @return True if the error identifies an exceeded input context window.
 */
func IsContextLengthExceededError(responseBody []byte) bool {
	if len(responseBody) == 0 {
		return false
	}

	var payload sseErrorResponse
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		// Google RPC encodes error.code as an HTTP status number.
		return GoogleInputTokenLimitMessage(responseBody) != ""
	}

	details := [...]sseErrorDetail{
		payload.Error,
		payload.Response.Error,
		{Type: payload.Type, Code: payload.Code, Message: payload.Message},
	}
	for _, detail := range details {
		code := strings.ToLower(strings.TrimSpace(detail.Code))
		if isContextLengthErrorCode(code) {
			return true
		}
		if code != "" && code != "invalid_request_error" && code != "bad_request_error" {
			continue
		}

		errorType := strings.ToLower(strings.TrimSpace(detail.Type))
		if errorType != "" && errorType != "error" && errorType != "invalid_request_error" && errorType != "bad_request_error" {
			continue
		}
		message := strings.ToLower(strings.TrimSpace(detail.Message))
		// Anthropic 只用消息报告输入超限："prompt is too long: N tokens > M maximum"。
		if strings.Contains(message, "context window") ||
			strings.Contains(message, "context length") ||
			strings.HasPrefix(message, "prompt is too long") ||
			strings.Contains(message, "maximum context") ||
			strings.Contains(message, "too many tokens") {
			return true
		}
	}
	return false
}

/**
 * @brief Matches the start of Google's input token limit error, including the optional reported count.
 */
var googleInputTokenLimit = regexp.MustCompile(`(?i)^(?:prompt is too long:\s*)?(?:the )?input token count(?:\s*\([0-9,]+\))?\s+exceeds the maximum\b`)

/**
 * @brief Recognizes the Google RPC context overflow envelope.
 * @param body HTTP error body or SSE error payload.
 * @return Upstream message, or an empty string for any other error.
 */
func GoogleInputTokenLimitMessage(body []byte) string {
	var payload struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil ||
		payload.Error.Code != http.StatusBadRequest || payload.Error.Status != "INVALID_ARGUMENT" {
		return ""
	}
	message := strings.TrimSpace(payload.Error.Message)
	if googleInputTokenLimit.MatchString(message) {
		return message
	}
	return ""
}

/**
 * @brief 判断上游明确报告的是单次请求的模型上下文超限。
 * @param code 上游错误码；没有结构化错误码时传空字符串。
 * @param message 上游错误说明。
 * @return 明确的上下文错误码或超限文案返回 true，模糊的 token 数和吞吐限流返回 false。
 */
func IsExplicitContextLengthExceeded(code, message string) bool {
	if isContextLengthErrorCode(strings.ToLower(strings.TrimSpace(code))) {
		return true
	}
	message = strings.ToLower(strings.TrimSpace(message))
	for _, throttle := range [...]string{"rate limit", "rate-limit", "throttl", "quota", "usage limit", "per minute", "per second", "tokens/minute", "tokens/second", "too many requests"} {
		if strings.Contains(message, throttle) {
			return false
		}
	}
	marker, _, _ := strings.Cut(message, ":")
	if isContextLengthErrorCode(strings.TrimSpace(marker)) ||
		strings.HasPrefix(message, "prompt is too long") || strings.HasPrefix(message, "prompt too long") ||
		strings.HasPrefix(message, "your prompt is too long") {
		return true
	}
	if strings.Contains(message, "context window") || strings.Contains(message, "context length") {
		return strings.Contains(message, "exceed") || strings.Contains(message, "too long") ||
			strings.Contains(message, "too large") || strings.Contains(message, "over the") ||
			(strings.Contains(message, "maximum context length is ") &&
				(strings.Contains(message, "you requested") || strings.Contains(message, "your messages resulted in")))
	}
	return false
}

/**
 * @brief 识别上游协议使用的上下文超限错误码。
 * @param code 已规范为小写且无首尾空白的上游错误码。
 * @return 出现已知错误码时为 true。
 */
func isContextLengthErrorCode(code string) bool {
	return code == "context_length_exceeded" || code == "context_too_large"
}

/**
 * @brief 提供上游状态码的默认错误级别。
 */
var statusCodeMetaMap = map[int]StatusCodeMeta{
	// === 客户端取消 ===
	// 499: 上游返回的客户端关闭请求；基础级别为 Channel，HTTP 分类层收窄到模型作用域
	// 注意：context.Canceled 在 ClassifyError 中单独处理
	499: {ErrorLevelChannel},

	// === Key级错误：API Key相关问题 ===
	// 这些错误在本系统中属于"后端Key/渠道配置问题"，不应甩锅给客户端
	401: {ErrorLevelKey}, // Unauthorized - Key invalid
	402: {ErrorLevelKey}, // Payment Required - quota/balance
	403: {ErrorLevelKey}, // Forbidden - Key permission
	429: {ErrorLevelKey}, // Too Many Requests - rate limited

	// === 渠道级错误：服务器端问题 ===
	408: {ErrorLevelChannel}, // Request Timeout - 上游等待网关发送请求超时
	444: {ErrorLevelChannel}, // nginx: No Response (服务器主动关闭连接)
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
	// 405 Method Not Allowed: 在代理场景下，这更可能意味着上游 endpoint/路由配置错误（方法不被支持）
	// 作为渠道级故障处理：触发渠道冷却。
	405: {ErrorLevelChannel}, // Method Not Allowed
	406: {ErrorLevelClient},  // Not Acceptable
	410: {ErrorLevelClient},  // Gone（模型退役由响应语义收窄为模型级故障）
	413: {ErrorLevelKey},     // Payload Too Large：按模型冷却并换渠，见 ClassifyHTTPResponseWithMeta
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
	// 默认行为（兜底策略）
	if status >= 500 {
		return StatusCodeMeta{ErrorLevelChannel}
	}
	if status >= 400 {
		// [FIX] 未知 4xx 状态码默认 Key 级冷却（保守策略）
		// 设计理念：未知错误应保守处理，避免持续请求故障 Key
		// 如果所有 Key 都冷却了，会自动升级为渠道级冷却
		return StatusCodeMeta{ErrorLevelKey}
	}
	return StatusCodeMeta{ErrorLevelClient}
}

/**
 * @brief 判断上游 HTTP 响应是否默认冷却当前模型。
 * @param status 上游状态码。
 * @return 上游 408 及服务端故障返回 true。596–599 由各自的内部状态契约处理。
 */
func IsModelScopedHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout || status >= 500 && status < StatusQuotaExceeded
}

// IsModelScopedStreamFailure 判断内部流故障是否只应冷却当前实际模型。
func IsModelScopedStreamFailure(status int) bool {
	return status == StatusFirstByteTimeout || status == StatusStreamIncomplete
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

/**
 * @brief 根据上游响应判断错误影响范围和冷却截止时间。
 * @param statusCode 本次尝试的有效状态码。
 * @param headers 上游响应头。
 * @param responseBody 上游错误正文。
 * @return 错误级别及已识别的冷却作用域与截止时间。
 * @details 1308 优先识别；401/403 在明确账户终态时影响整个渠道。400/413 默认只影响模型，
 * 上下文超限与图片无法处理原样直返。429 优先识别具体配额，普通限流按实际模型退避。
 */
func ClassifyHTTPResponseWithMeta(statusCode int, headers map[string][]string, responseBody []byte) HTTPResponseClassification {
	return classifyHTTPResponseWithMetaAt(statusCode, headers, responseBody, time.Now())
}

/**
 * @brief 基于本次分类的起算时间解释上游错误信号。
 * @param statusCode 本次尝试的有效状态码。
 * @param headers 上游响应头。
 * @param responseBody 上游错误正文。
 * @param now 冷却延迟的起算时间。
 * @return 本次错误的分类和提供商冷却信息。
 */
func classifyHTTPResponseWithMetaAt(statusCode int, headers map[string][]string, responseBody []byte, now time.Time) HTTPResponseClassification {
	// 上游 HTTP 499 与本地 context.Canceled 不同：切换渠道，但只冷却当前实际模型。
	if statusCode == StatusClientClosedRequest {
		return HTTPResponseClassification{
			Level:       ErrorLevelChannel,
			ModelScoped: true,
		}
	}

	// [INFO] 特殊处理：检测1308错误（可能以SSE error事件形式出现，HTTP状态码是200）
	// 1308错误表示达到使用上限，应该触发Key级冷却
	if resetTime, has1308 := ParseResetTimeFrom1308Error(responseBody); has1308 {
		return HTTPResponseClassification{
			Level:               ErrorLevelKey,
			KeyCooldownUntil:    resetTime,
			HasKeyCooldownUntil: true,
			KeyCooldownReason:   "1308",
		}
	}

	// 上游 WebSocket 连接寿命到期：原生 WS 已在同渠道换新连接重放过，仍到达这里才切渠道，只给极短的渠道冷却。
	// 优先于 597/429 的常规分类，避免健康 Key 被指数退避冷却。
	if IsWebsocketConnectionLimitError(responseBody) {
		return HTTPResponseClassification{
			Level:                   ErrorLevelChannel,
			ChannelCooldownUntil:    now.Add(WebsocketConnectionLimitCooldown),
			HasChannelCooldownUntil: true,
			ChannelCooldownReason:   "websocket_connection_limit",
		}
	}

	if quotaErr, parsed := parseStructuredQuotaError(responseBody, false); parsed {
		if cooldownUntil, reason, level, ok := parseStructuredQuotaCooldown(quotaErr, now, true); ok {
			classification := HTTPResponseClassification{
				Level: level,
				Model: strings.TrimSpace(quotaErr.model),
			}
			if reason == "XAI_FREE_USAGE_EXHAUSTED" {
				if classification.Model == "" {
					if match := xaiFreeUsageModelRegex.FindStringSubmatch(quotaErr.message); len(match) == 2 {
						classification.Model = strings.TrimRight(match[1], ".")
					}
				}
				if classification.Model != "" {
					classification.ModelScoped = true
					classification.ModelCooldownUntil = cooldownUntil
					classification.HasModelCooldownUntil = true
					classification.ModelCooldownReason = reason
				} else {
					classification.Level = ErrorLevelKey
					classification.CredentialScoped = true
					classification.KeyCooldownUntil = cooldownUntil
					classification.HasKeyCooldownUntil = true
					classification.KeyCooldownReason = reason
				}
				return classification
			}
			if reason == "model_cooldown" || reason == CodexUsageFrequencyLimitReason || reason == "INFERENCE_CAP_ERROR" {
				classification.ModelScoped = true
				classification.ModelCooldownReason = reason
				classification.PreventKeyFallback = reason == CodexUsageFrequencyLimitReason
				if cooldownUntil.After(now) {
					classification.ModelCooldownUntil = cooldownUntil
					classification.HasModelCooldownUntil = true
				}
				return classification
			}
			if statusCode == 429 && level == ErrorLevelChannel {
				classification.ModelScoped = true
				classification.ModelCooldownUntil = cooldownUntil
				classification.HasModelCooldownUntil = true
				classification.ModelCooldownReason = reason
				return classification
			}
			switch level {
			case ErrorLevelChannel:
				classification.ChannelCooldownUntil = cooldownUntil
				classification.HasChannelCooldownUntil = true
				classification.ChannelCooldownReason = reason
			default:
				classification.KeyCooldownUntil = cooldownUntil
				classification.HasKeyCooldownUntil = true
				classification.KeyCooldownReason = reason
			}
			return classification
		}
	}

	// 上下文超限由当前请求体决定，切换 Key、模型或渠道都不会改变结果。
	// SSE 路径使用 597 承载 HTTP 200 中的错误事件；普通 Codex 错误使用 400/413。
	if (statusCode == StatusSSEError || statusCode == http.StatusBadRequest || statusCode == http.StatusRequestEntityTooLarge) &&
		IsContextLengthExceededError(responseBody) {
		return HTTPResponseClassification{Level: ErrorLevelClient}
	}

	// 图片解不开同样由请求内容决定：Antigravity/Vertex 把 Anthropic 原错误包在 error.message 里，
	// 后端仍是同一个解码器，换渠道只会重复失败并冷却无辜模型。原样直返，Claude Code 会移除该图片后重试。
	if statusCode == http.StatusBadRequest &&
		strings.Contains(upstreamErrorMessageLower(responseBody), "could not process image") {
		return HTTPResponseClassification{Level: ErrorLevelClient}
	}

	// [INFO] 597 SSE error事件：解析实际错误类型动态判断级别
	// SSE error JSON格式: {"type":"error","error":{"type":"api_error","message":"上游API返回错误: 500"}}
	// 服务类错误切换渠道但只冷却当前模型；认证/限流类错误仍冷却 Key。
	// 模型不可用（如 Codex WS 错误事件 "model is not supported"）与 HTTP 400 同口径：只冷却当前模型。
	if statusCode == StatusSSEError && IsTransientCapacityError(responseBody) {
		statusCode = http.StatusTooManyRequests
	}
	if statusCode == StatusSSEError {
		level := classifySSEError(responseBody)
		return HTTPResponseClassification{
			Level: level,
			ModelScoped: level == ErrorLevelChannel ||
				(level == ErrorLevelKey && isModelUnavailableResponse(responseBody)),
		}
	}

	if IsModelScopedStreamFailure(statusCode) {
		return HTTPResponseClassification{
			Level:       ErrorLevelChannel,
			ModelScoped: true,
		}
	}

	// Anthropic 429 先区分三类：共享 5h/7d 窗口被拒属于整个凭证；fast 模式缺少
	// usage credits 只影响当前渠道的 fast 请求；其余只冷却模型。
	if statusCode == 429 {
		if anthropicUnifiedWindowRejected(headers) {
			classification := HTTPResponseClassification{
				Level:              ErrorLevelKey,
				CredentialScoped:   true,
				PreventKeyFallback: true,
				KeyCooldownReason:  "anthropic_unified_window_rejected",
			}
			if until, ok := anthropicRejectedWindowReset(headers, now); ok {
				classification.KeyCooldownUntil = until
				classification.HasKeyCooldownUntil = true
			}
			return classification
		}
		if AnthropicFastModeCreditsRequired(responseBody) {
			return HTTPResponseClassification{Level: ErrorLevelCapability}
		}
	}

	// 普通 429 默认冷却实际模型。
	if statusCode == 429 {
		level := ErrorLevelKey
		if headers != nil {
			level = classifyRateLimitError(headers, responseBody)
		}
		classification := HTTPResponseClassification{
			Level:       level,
			ModelScoped: true,
		}
		// 每分钟限流与订阅窗口无关：统一 reset 是代表窗口的重置时间，按它冷却会把
		// 几十秒的限流拉长到数小时并锁住同渠道的其他 Key，此时走普通 429 退避。
		if anthropicPerMinuteRateLimit(headers, responseBody) {
			return classification
		}
		if until, ok := parseAnthropicRateLimitReset(headers, now); ok {
			classification.PreventKeyFallback = true
			classification.ModelCooldownUntil = until
			classification.HasModelCooldownUntil = true
			classification.ModelCooldownReason = "anthropic_unified_reset"
			return classification
		}
		if IsTransientCapacityError(responseBody) {
			if until, ok := ParseRetryAfter(headers, now); ok {
				classification.ModelCooldownUntil = until
				classification.HasModelCooldownUntil = true
				classification.ModelCooldownReason = "retry_after"
			}
		}
		return classification
	}

	// WebSocket close 1009 被桥接为 413，必须保留关闭语义，不能按普通 HTTP 413 换渠。
	if statusCode == http.StatusRequestEntityTooLarge {
		var payload sseErrorResponse
		if json.Unmarshal(responseBody, &payload) == nil && strings.TrimSpace(payload.Error.Code) == "message_too_big" {
			return HTTPResponseClassification{Level: ErrorLevelClient}
		}
	}

	// 账号终态的 400 属于整个凭证：Key 级退避；OAuth 渠道的凭证即渠道，由冷却层落到渠道。
	if statusCode == http.StatusBadRequest && anthropicAccountUnusable(responseBody) {
		return HTTPResponseClassification{Level: ErrorLevelKey}
	}

	// 400/413 表示当前模型/上游无法接受该请求。切换渠道，但只冷却实际请求的模型。
	// 413 必须与 400 同级：否则 auto 协议探测把 Anthropic 400 交给下一个候选后，
	// 候选网关的 RequestTooLarge 会 ActionReturnClient，客户端直接中断、后面的匹配渠道进不去。
	if statusCode == 400 || statusCode == http.StatusRequestEntityTooLarge {
		return requestRejectedClassification
	}

	// 410 Gone 通常表示资源已永久移除。只有响应明确指向模型退役时才切换渠道并
	// 冷却当前实际模型；其他资源的 410 仍由客户端处理，避免盲目重放请求。
	if statusCode == http.StatusGone && isModelUnavailableResponse(responseBody) {
		return HTTPResponseClassification{
			Level:       ErrorLevelChannel,
			ModelScoped: true,
		}
	}

	// 404错误：根据响应体智能分类
	if statusCode == 404 {
		return HTTPResponseClassification{
			Level:       classify404Error(responseBody),
			ModelScoped: isModelUnavailableResponse(responseBody),
		}
	}

	// Cloudflare 质询由出口 IP 与 TLS 指纹决定，与具体 Key 无关：同渠道其他 Key
	// 一个都过不去，只有切换渠道才有意义。按渠道级分类交给默认指数退避处理，
	// 不设固定冷却时长（无法预判质询持续时间），也不设置 ModelScoped（切模型无效）。
	// 必须排在响应体关键字匹配之前：质询页 HTML 匹配不上任何渠道级特征，
	// 否则会落到默认的 Key 级，OAuth 渠道下更是既不写 Key 冷却也不写渠道冷却。
	// 1010 浏览器签名封禁同理，只是边缘返回的是纯文本或 JSON 而非 HTML。
	if statusCode == http.StatusForbidden || statusCode == http.StatusServiceUnavailable {
		if isCloudflareChallengeResponse(firstHeaderValueFold(headers, "cf-mitigated"), responseBody) ||
			(statusCode == http.StatusForbidden && isCloudflareBotBlockResponse(responseBody)) {
			return HTTPResponseClassification{
				Level:                 ErrorLevelChannel,
				PreventKeyFallback:    true,
				ChannelCooldownReason: "cloudflare_challenge",
			}
		}
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

	// Gemini 对不存在、已过期或属于其他项目的 cachedContent 统一返回此 403。缓存名由客户端
	// 指定且不绑定 Key，换 Key/渠道无意义；按 Key 级处理会让任意请求冷却健康 Key。
	if statusCode == http.StatusForbidden && strings.Contains(bodyLower, "cachedcontent not found") {
		return HTTPResponseClassification{Level: ErrorLevelClient}
	}

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

// IsTransientCapacityError recognizes structured turn/model admission failures.
// An explicit unrelated code wins over capacity wording in the message.
func IsTransientCapacityError(body []byte) bool {
	type capacityError struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	var payload struct {
		capacityError
		Error    capacityError `json:"error"`
		Response struct {
			Error capacityError `json:"error"`
		} `json:"response"`
		Body struct {
			Error capacityError `json:"error"`
		} `json:"body"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	entries := []capacityError{payload.Error, payload.Response.Error, payload.Body.Error, payload.capacityError}
	for _, entry := range entries {
		if code := strings.ToLower(strings.TrimSpace(entry.Code)); code != "" {
			switch code {
			case "turn_rate_limit_exceeded", "model_at_capacity", "model_is_at_capacity":
				return true
			default:
				return false
			}
		}
	}
	for _, entry := range entries {
		message := strings.ToLower(strings.TrimSpace(entry.Message))
		if strings.Contains(message, "model is at capacity") ||
			(strings.EqualFold(strings.TrimSpace(entry.Type), "rate_limit_error") && strings.Contains(message, "at capacity")) {
			return true
		}
	}
	return false
}

/**
 * @brief 解析 Retry-After 秒数或 HTTP 日期，不接受溢出、非正秒数或无效日期。
 * @param headers 上游响应头，名称不区分大小写。
 * @param now 相对秒数的起算时间。
 * @return 恢复时间；无效值为零，已过期的合法日期仍返回原日期。
 * @return 恢复时间严格晚于 now 时为 true，否则为 false。
 */
func ParseRetryAfter(headers map[string][]string, now time.Time) (time.Time, bool) {
	return parseRetryAfter(headers, now, false)
}

/**
 * @brief 读取 Retry-After 秒数或 HTTP 日期，保留明确要求立即重试的 0。
 * @param headers 上游响应头，名称不区分大小写。
 * @param now 相对秒数的起算时间。
 * @return 恢复时间；字段缺失、秒数无效或溢出时为空。
 * @return 合法的非负秒数或未来日期返回 true。
 */
func ParseRetryAfterHint(headers map[string][]string, now time.Time) (time.Time, bool) {
	return parseRetryAfter(headers, now, true)
}

/**
 * @brief 获取独立 API Key 遇到有效 429 时的冷却截止时间。
 * @param headers 上游响应头。
 * @param body 上游错误正文。
 * @param now 相对延迟的起算时间。
 * @return 上游明确给出的恢复时间（含立即重试）；缺失或晚于 now 加最长限流窗口时为 now 后一分钟。
 * @details Key 冷却只会被更晚的截止时间覆盖，超长提示（毫秒时间戳、异常 Retry-After）
 * 一旦写入便无法被后续短冷却纠正，因此按无效提示处理。
 */
func APIKeyRateLimitUntil(headers map[string][]string, body []byte, now time.Time) time.Time {
	if until := apiKeyRateLimitHint(headers, body, now); until.Sub(now) <= anthropicMaxRateLimitWindow {
		return until
	}
	return now.Add(time.Minute)
}

/**
 * @brief 按优先级提取独立 API Key 429 的上游恢复时间提示。
 * @param headers 上游响应头。
 * @param body 上游错误正文。
 * @param now 相对延迟的起算时间。
 * @return 上游明确给出的恢复时间（含立即重试），不限上限；缺失时为 now 后一分钟。
 */
func apiKeyRateLimitHint(headers map[string][]string, body []byte, now time.Time) time.Time {
	if until, ok := ParseResetTimeFrom1308Error(body); ok && until.After(now) {
		return until
	}
	quotaErr, _ := parseStructuredQuotaError(body, true)
	if until, ok := parseStructuredCooldownUntil(quotaErr, now); ok {
		return until
	}
	if until, _, _, ok := parseStructuredQuotaCooldown(quotaErr, now, false); ok && until.After(now) {
		return until
	}
	message := quotaErr.message
	if message == "" && !json.Valid(body) {
		message = string(body)
	}
	if until, ok := parseGoogleMessageRetryAt(message, now, true); ok {
		return until
	}
	if until, ok := parseRetryAfterSecondsCooldownUntil(message, now); ok {
		return until
	}
	if until, ok := parseTryAgainInCooldownUntil(message, now); ok {
		return until
	}
	if anthropicUnifiedWindowRejected(headers) {
		if until, ok := anthropicRejectedWindowReset(headers, now); ok {
			return until
		}
	} else if !anthropicPerMinuteRateLimit(headers, body) {
		if until, ok := parseAnthropicRateLimitReset(headers, now); ok {
			return until
		}
	}
	if until, ok := ParseRetryAfterHint(headers, now); ok {
		return until
	}
	return now.Add(time.Minute)
}

/**
 * @brief 在保留原有未来时间判断的同时支持允许显式零秒的提供商。
 * @param headers 上游响应头。
 * @param now 起算时间。
 * @param allowZero 是否接受明确的零秒数。
 * @return 恢复时间；无效时为空。
 * @return 是否可采用该提示。
 */
func parseRetryAfter(headers map[string][]string, now time.Time, allowZero bool) (time.Time, bool) {
	value := strings.TrimSpace(firstHeaderValueFold(headers, "Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if (seconds > 0 || allowZero && seconds == 0) && seconds <= int64((1<<63-1)/time.Second) {
			return now.Add(time.Duration(seconds) * time.Second), true
		}
		return time.Time{}, false
	}
	until, err := http.ParseTime(value)
	return until, err == nil && until.After(now)
}

// classifyRateLimitError 分析 429 的限流范围。
// ErrorLevelChannel 仅表示限流信号覆盖 IP/账户/组织；429 的冷却作用域始终由调用方固定为模型级。
//
// 判断逻辑:
//  1. 检查Retry-After头: 如果>60秒,标记为广域限流
//  2. 检查X-RateLimit-Scope: 如果是"global"或"ip",标记为广域限流
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
		// 如果是HTTP日期格式,通常表示长时间广域限流
		if _, err := time.Parse(time.RFC1123, retryAfter); err == nil {
			return ErrorLevelChannel
		}
	}

	// 2. 检查X-RateLimit-Scope头(某些API使用)
	if scopeValues, ok := headers["X-Ratelimit-Scope"]; ok && len(scopeValues) > 0 {
		scope := strings.ToLower(scopeValues[0])
		// global/ip/account 表示广域限流，但冷却仍只作用于当前模型
		if scope == RateLimitScopeGlobal || scope == RateLimitScopeIP || scope == RateLimitScopeAccount {
			return ErrorLevelChannel
		}
	}

	// 3. 分析响应体中的错误描述
	if len(responseBody) > 0 {
		bodyLower := strings.ToLower(string(responseBody))

		// 广域限流特征
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

	// 4. 默认标记为窄域限流；冷却仍只作用于当前模型
	return ErrorLevelKey
}

func parseAnthropicRateLimitReset(headers map[string][]string, now time.Time) (time.Time, bool) {
	for _, value := range headerValuesFold(headers, anthropicRateLimitUnifiedResetHeader) {
		if until, ok := anthropicRateLimitDeadline(headers, anthropicRateLimitUnifiedResetHeader, value, now); ok {
			return until, true
		}
	}
	return time.Time{}, false
}

// anthropicRateLimitDeadline 解析 reset 头为可用的冷却截止时间。
// 超过最长窗口的时间，以及 overage claim 下与 Overage-Reset 相同的统一 reset，
// 都是计费周期边界：按它冷却会把模型或凭证挂到下个计费周期。
func anthropicRateLimitDeadline(headers map[string][]string, name, value string, now time.Time) (time.Time, bool) {
	resetUnix, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	until := time.Unix(resetUnix, 0)
	if !until.After(now) || until.Sub(now) > anthropicMaxRateLimitWindow {
		return time.Time{}, false
	}
	if strings.EqualFold(name, anthropicRateLimitUnifiedResetHeader) &&
		strings.Contains(strings.ToLower(firstHeaderValueFold(headers, "Anthropic-Ratelimit-Unified-Representative-Claim")), "overage") {
		overageUnix, err := strconv.ParseInt(firstHeaderValueFold(headers, "Anthropic-Ratelimit-Unified-Overage-Reset"), 10, 64)
		if err == nil && overageUnix == resetUnix {
			return time.Time{}, false
		}
	}
	return until, true
}

// anthropicPerMinuteRateLimit 识别不涉及配额窗口的 Anthropic 每分钟限流（RPM/ITPM/OTPM）。
// 只认完整的官方措辞：子串匹配会误伤否定说法或与配额耗尽并存的提示；
// 任一窗口状态为 rejected 时以窗口语义为准。
func anthropicPerMinuteRateLimit(headers map[string][]string, body []byte) bool {
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Error.Type != "rate_limit_error" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(payload.Error.Message)) {
	case "this request would exceed the requests per minute rate limit. please try again later.",
		"this request would exceed the input tokens per minute rate limit. please try again later.",
		"this request would exceed the output tokens per minute rate limit. please try again later.":
	default:
		return false
	}
	for _, window := range []string{"", "5h-", "7d-", "7d_oi-", "Overage-"} {
		for _, status := range headerValuesFold(headers, "Anthropic-Ratelimit-Unified-"+window+"Status") {
			if strings.EqualFold(strings.TrimSpace(status), "rejected") {
				return false
			}
		}
	}
	return true
}

// anthropicUnifiedWindowRejected 判断 Anthropic 是否明确拒绝了共享 5h/7d 订阅窗口。
// 仅 overage/7d_oi 被拒而共享窗口仍可用时属于模型级，不能冷却整个凭证。
func anthropicUnifiedWindowRejected(headers map[string][]string) bool {
	status := func(name string) string {
		return strings.ToLower(firstHeaderValueFold(headers, "Anthropic-Ratelimit-Unified-"+name+"Status"))
	}
	status5h, status7d := status("5h-"), status("7d-")
	if status5h == "rejected" || status7d == "rejected" {
		return true
	}
	if status("") != "rejected" {
		return false
	}
	return !anthropicOverageOnlyRejection(headers, status5h, status7d, status("7d_oi-"))
}

func anthropicOverageOnlyRejection(headers map[string][]string, status5h, status7d, status7dOI string) bool {
	claim := strings.ToLower(firstHeaderValueFold(headers, "Anthropic-Ratelimit-Unified-Representative-Claim"))
	overageRejected := status7dOI == "rejected" ||
		strings.EqualFold(firstHeaderValueFold(headers, "Anthropic-Ratelimit-Unified-Overage-Status"), "rejected") ||
		firstHeaderValueFold(headers, "Anthropic-Ratelimit-Unified-Overage-Disabled-Reason") != "" ||
		strings.Contains(claim, "overage")
	if !overageRejected {
		return false
	}
	allowed := func(status string) bool { return status == "allowed" || status == "allowed_warning" }
	healthy := func(window string) bool {
		u, err := strconv.ParseFloat(firstHeaderValueFold(headers, "Anthropic-Ratelimit-Unified-"+window+"-Utilization"), 64)
		return err == nil && u >= 0 && u < 1
	}
	switch {
	case allowed(status5h) && allowed(status7d):
		return true
	case allowed(status7d) && status5h == "":
		return len(headerValuesFold(headers, "Anthropic-Ratelimit-Unified-5h-Utilization")) == 0 || healthy("5h")
	case allowed(status5h) && status7d == "":
		return healthy("7d")
	case status5h == "" && status7d == "" && strings.Contains(claim, "overage"):
		// 调用方已确认统一状态为 rejected。Anthropic 未评估共享窗口时两个状态头都缺失；
		// 只有 overage claim 使用这种缺失，其他 overage 信号不放宽此例外。
		absentOrHealthy := func(window string) bool {
			return firstHeaderValueFold(headers, "Anthropic-Ratelimit-Unified-"+window+"-Utilization") == "" || healthy(window)
		}
		return absentOrHealthy("5h") && absentOrHealthy("7d")
	}
	return false
}

// anthropicRejectedWindowReset 取被拒窗口与统一 reset 中最晚的有效截止时间。
func anthropicRejectedWindowReset(headers map[string][]string, now time.Time) (time.Time, bool) {
	var latest time.Time
	consider := func(name string) {
		for _, value := range headerValuesFold(headers, name) {
			if until, ok := anthropicRateLimitDeadline(headers, name, value, now); ok && until.After(latest) {
				latest = until
			}
		}
	}
	for _, window := range []string{"5h", "7d"} {
		if strings.EqualFold(firstHeaderValueFold(headers, "Anthropic-Ratelimit-Unified-"+window+"-Status"), "rejected") {
			consider("Anthropic-Ratelimit-Unified-" + window + "-Reset")
		}
	}
	consider(anthropicRateLimitUnifiedResetHeader)
	return latest, !latest.IsZero()
}

// upstreamErrorMessageLower 取 error.message（缺失时退回整个响应体）并转小写，供关键字匹配。
func upstreamErrorMessageLower(body []byte) string {
	var payload sseErrorResponse
	message := ""
	if json.Unmarshal(body, &payload) == nil {
		message = payload.Error.Message
	}
	if message == "" {
		message = string(body)
	}
	return strings.ToLower(message)
}

/**
 * @brief 判断上游是否因缺少额度而拒绝 Fast 档位。
 * @param body 上游响应正文。
 * @return 后续尝试需要保留 Fast 档位时返回 true。
 */
func AnthropicFastModeCreditsRequired(body []byte) bool {
	message := upstreamErrorMessageLower(body)
	return strings.Contains(message, "fast request rejected") ||
		(strings.Contains(message, "fast") &&
			(strings.Contains(message, "usage credits") || strings.Contains(message, "credits are required")))
}

// requestRejectedClassification 是 400/413 的通用兜底：当前模型/上游拒绝这份请求，
// 响应体里没有凭证、配额、上下文或连接寿命等更具体的信号。
var requestRejectedClassification = HTTPResponseClassification{Level: ErrorLevelKey, ModelScoped: true}

// IsGenericRequestRejection 判断 400 是否只落到通用兜底分类。Key/凭证终态、
// 配额、上下文超限等专门分类返回 false，调用方应按正常冷却路径处理。
func IsGenericRequestRejection(statusCode int, headers map[string][]string, responseBody []byte) bool {
	return statusCode == http.StatusBadRequest &&
		ClassifyHTTPResponseWithMeta(statusCode, headers, responseBody) == requestRejectedClassification
}

// anthropicAccountUnusable 识别 Anthropic 用 400 报告的账号终态：组织被禁用、
// API 余额耗尽、需完成身份验证。它们与请求和模型无关，换模型重试只会原样失败。
func anthropicAccountUnusable(body []byte) bool {
	message := upstreamErrorMessageLower(body)
	return strings.Contains(message, "organization has been disabled") ||
		strings.Contains(message, "credit balance") ||
		strings.Contains(message, "identity verification is required")
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
	case "api_error", "overloaded_error", "service_unavailable_error", "server_is_overloaded", "1305":
		// 上游服务错误或过载 → 渠道级冷却
		return ErrorLevelChannel
	case "rate_limit_error", "authentication_error", "invalid_request_error", "1308", "1310":
		// 限流/认证/请求错误 → Key级冷却
		return ErrorLevelKey
	default:
		// 未知错误类型，保守处理为Key级
		return ErrorLevelKey
	}
}

/**
 * @brief 上游 WebSocket 单条连接达到寿命上限（60 分钟）的错误码
 */
const WebsocketConnectionLimitCode = "websocket_connection_limit_reached"

/**
 * @brief 只解析判定连接寿命到期所需的字段
 * @details error 既可能是对象也可能是字符串，用 RawMessage 兜住两种形态。
 */
type websocketErrorProbe struct {
	Code  any             `json:"code"`
	Error json.RawMessage `json:"error"`
}

/**
 * @brief 判断响应体是否为上游 WebSocket 单条连接达到寿命上限（60 分钟）
 * @param responseBody 上游错误响应体或 WebSocket 错误帧
 * @return 顶层 code、字符串 error 或 error.type/error.code 为该错误码时返回 true
 * @details 这不是渠道/Key/模型故障：新建连接即可继续，既有 Key 完全健康，
 * 落到默认分类会被误判成 Key 级错误并触发指数退避冷却。
 */
func IsWebsocketConnectionLimitError(responseBody []byte) bool {
	if len(responseBody) == 0 {
		return false
	}

	var probe websocketErrorProbe
	if err := json.Unmarshal(responseBody, &probe); err != nil {
		return false
	}
	if isWebsocketConnectionLimitValue(structuredScalarString(probe.Code)) {
		return true
	}
	if len(probe.Error) == 0 {
		return false
	}

	var errText string
	if err := json.Unmarshal(probe.Error, &errText); err == nil {
		return isWebsocketConnectionLimitValue(errText)
	}

	var errObj struct {
		Type any `json:"type"`
		Code any `json:"code"`
	}
	if err := json.Unmarshal(probe.Error, &errObj); err != nil {
		return false
	}
	return isWebsocketConnectionLimitValue(structuredScalarString(errObj.Type)) ||
		isWebsocketConnectionLimitValue(structuredScalarString(errObj.Code))
}

func isWebsocketConnectionLimitValue(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), WebsocketConnectionLimitCode)
}

/**
 * @brief 解释已知提供商配额错误的恢复时间和作用域。
 * @param quotaErr 上游配额错误字段。
 * @param now 相对延迟的起算时间。
 * @param allowInferred 是否保留提供商规则中根据错误码推断的窗口。
 * @return 恢复时间；模型冷却缺少时间或规则未命中时为零值。
 * @return 规则原因；规则未命中时为空。
 * @return 冷却的错误级别；规则未命中时为 ErrorLevelNone。
 * @return 识别到可应用的规则时为 true。
 */
func parseStructuredQuotaCooldown(quotaErr structuredQuotaError, now time.Time, allowInferred bool) (time.Time, string, ErrorLevel, bool) {
	code := quotaErr.code
	message := quotaErr.message
	messageUpper := strings.ToUpper(message)

	switch {
	case code == "6004":
		if until, ok := parseCodexUsageFrequencyLimitCooldown(message, now); ok {
			return until, CodexUsageFrequencyLimitReason, ErrorLevelKey, true
		}
		return time.Time{}, "", ErrorLevelNone, false
	case code == "MODEL_COOLDOWN":
		if until, ok := parseStructuredCooldownUntil(quotaErr, now); ok {
			return until, "model_cooldown", ErrorLevelKey, true
		}
		return time.Time{}, "model_cooldown", ErrorLevelKey, true
	case quotaErr.status == "RESOURCE_EXHAUSTED" || strings.Contains(messageUpper, "RESOURCE_EXHAUSTED"):
		if until, ok := parseStructuredCooldownUntil(quotaErr, now); ok {
			return until, "RESOURCE_EXHAUSTED", ErrorLevelKey, true
		}
		if until, ok := parseGoogleMessageRetryAt(message, now, false); ok {
			return until, "RESOURCE_EXHAUSTED_RETRY_IN", ErrorLevelKey, true
		}
		return time.Time{}, "", ErrorLevelNone, false
	case strings.Contains(code, "FREE-USAGE-EXHAUSTED") ||
		strings.Contains(messageUpper, "FREE-USAGE-EXHAUSTED") ||
		strings.Contains(messageUpper, "INCLUDED FREE USAGE"):
		if until, ok := parseStructuredCooldownUntil(quotaErr, now); ok {
			return until, "XAI_FREE_USAGE_EXHAUSTED", ErrorLevelChannel, true
		}
		// 没有精确 reset 时沿用滚动 24 小时窗口；分类出口按上游是否
		// 明确命名模型区分模型额度和整个凭证的共享额度。
		if allowInferred {
			return now.Add(xaiFreeUsageExhaustedCooldown), "XAI_FREE_USAGE_EXHAUSTED", ErrorLevelChannel, true
		}
	case allowInferred && code == "API_KEY_QUOTA_EXHAUSTED":
		return now.Add(30 * time.Minute), "API_KEY_QUOTA_EXHAUSTED", ErrorLevelKey, true
	case allowInferred && (code == "FREE_TIER_BUDGET_EXCEEDED" || strings.Contains(messageUpper, "FREE_TIER_BUDGET_EXCEEDED")):
		return now.Add(30 * time.Minute), "FREE_TIER_BUDGET_EXCEEDED", ErrorLevelKey, true
	case allowInferred && (code == "DAILY_LIMIT_EXCEEDED" ||
		(code == "USAGE_LIMIT_EXCEEDED" && strings.Contains(messageUpper, "DAILY_LIMIT_EXCEEDED"))):
		return nextLocalMidnight(now), "DAILY_LIMIT_EXCEEDED", ErrorLevelKey, true
	case code == "GLOBAL_FIXED_WINDOW_QUOTA_EXHAUSTED":
		if until, ok := parseGlobalFixedWindowQuotaCooldownUntil(message, now); ok {
			return until, "GLOBAL_FIXED_WINDOW_QUOTA_EXHAUSTED", ErrorLevelChannel, true
		}
		return time.Time{}, "", ErrorLevelNone, false
	case strings.Contains(message, "用量上限"):
		if until, ok := parseBeijingTomorrowResetTime(message, now); ok {
			return until, "BEIJING_RELATIVE_QUOTA_RESET", ErrorLevelKey, true
		}
		return time.Time{}, "", ErrorLevelNone, false
	case code == "RATE_LIMIT_EXCEEDED":
		if until, ok := parseRetryAfterSecondsCooldownUntil(message, now); ok {
			return until, "RATE_LIMIT_RETRY_AFTER", ErrorLevelKey, true
		}
		return time.Time{}, "", ErrorLevelNone, false
	case code == "RATE_LIMIT_ERROR":
		if until, ok := parseRollingFreeAllowanceCooldownUntil(message, now); ok {
			return until, "ROLLING_FREE_ALLOWANCE_RESET", ErrorLevelKey, true
		}
		return time.Time{}, "", ErrorLevelNone, false
	case code == "INFERENCE_CAP_ERROR":
		// 文案指向单个模型。解析不到时长时不要编一个 Key 冷却，留给后续模型级处理。
		if until, ok := parseTryAgainInCooldownUntil(message, now); ok {
			return until, "INFERENCE_CAP_ERROR", ErrorLevelKey, true
		}
		return time.Time{}, "", ErrorLevelNone, false
	case code == "USAGE_LIMIT_REACHED":
		// 上游 usage limit（如 Claude Plus 计划限额），优先用 resets_in_seconds / resets_at
		if until, ok := parseStructuredCooldownUntil(quotaErr, now); ok {
			return until, "USAGE_LIMIT_REACHED", ErrorLevelKey, true
		}
		// 没有精确重置时间，默认30分钟
		if allowInferred {
			return now.Add(30 * time.Minute), "USAGE_LIMIT_REACHED", ErrorLevelKey, true
		}
	}
	return time.Time{}, "", ErrorLevelNone, false
}

/**
 * @brief 读取配额错误的代码、文案与结构化重置字段。
 * @param responseBody 上游错误 JSON。
 * @param readResponseError 是否允许从 Responses 失败事件的 response.error 中读取错误字段。
 * @return 解析出的配额错误字段，无有效 JSON 时为零值。
 * @return 包含错误码、文案或状态时为 true。
 */
func parseStructuredQuotaError(responseBody []byte, readResponseError bool) (structuredQuotaError, bool) {
	var errResp structuredQuotaErrorResponse
	if err := json.Unmarshal(responseBody, &errResp); err != nil {
		return structuredQuotaError{}, false
	}
	if len(errResp.Error) == 0 && readResponseError && len(errResp.Response) != 0 {
		var response struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(errResp.Response, &response) == nil {
			errResp.Error = response.Error
		}
	}

	message := errResp.Message
	if strings.TrimSpace(message) == "" {
		message = errResp.Msg
	}
	parsed := structuredQuotaError{
		code:         normalizeStructuredScalar(errResp.Code),
		message:      message,
		model:        strings.TrimSpace(errResp.Model),
		resetSeconds: coalesceInt64(errResp.ResetSeconds, errResp.ResetsInSeconds),
		resetsAt:     errResp.ResetsAt,
		resetTime:    errResp.ResetTime,
		status:       strings.ToUpper(strings.TrimSpace(errResp.Status)),
	}
	mergeStructuredQuotaDetails(&parsed, errResp.Details)

	if len(errResp.Error) > 0 {
		var errorText string
		if err := json.Unmarshal(errResp.Error, &errorText); err == nil {
			if parsed.message == "" {
				parsed.message = errorText
			}
		} else {
			var errorObj structuredQuotaErrorObject
			if err := json.Unmarshal(errResp.Error, &errorObj); err == nil {
				if parsed.code == "" {
					parsed.code = normalizeStructuredScalar(errorObj.Code)
				}
				if parsed.code == "" {
					parsed.code = normalizeStructuredScalar(errorObj.Type)
				}
				if parsed.message == "" {
					parsed.message = errorObj.Message
					if strings.TrimSpace(parsed.message) == "" {
						parsed.message = errorObj.Msg
					}
				}
				if parsed.model == "" {
					parsed.model = strings.TrimSpace(errorObj.Model)
				}
				if parsed.resetSeconds == 0 {
					parsed.resetSeconds = coalesceInt64(errorObj.ResetSeconds, errorObj.ResetsInSeconds)
				}
				if parsed.resetsAt == 0 {
					parsed.resetsAt = errorObj.ResetsAt
				}
				if parsed.resetTime == "" {
					parsed.resetTime = errorObj.ResetTime
				}
				if parsed.status == "" {
					parsed.status = strings.ToUpper(strings.TrimSpace(errorObj.Status))
				}
				mergeStructuredQuotaDetails(&parsed, errorObj.Details)
			}
		}
	}

	return parsed, parsed.code != "" || parsed.message != "" || parsed.status != ""
}

func mergeStructuredQuotaDetails(parsed *structuredQuotaError, details []structuredQuotaErrorDetail) {
	if parsed == nil {
		return
	}
	for _, detail := range details {
		if parsed.model == "" {
			parsed.model = strings.TrimSpace(detail.Metadata.Model)
		}
		if parsed.quotaResetTimeStamp == "" {
			parsed.quotaResetTimeStamp = strings.TrimSpace(detail.Metadata.QuotaResetTimeStamp)
		}
		if parsed.quotaResetDelay == "" {
			parsed.quotaResetDelay = strings.TrimSpace(detail.Metadata.QuotaResetDelay)
		}
		if parsed.retryDelay == "" {
			parsed.retryDelay = strings.TrimSpace(detail.RetryDelay)
		}
	}
}

// ExtractUpstreamErrorCodeAndMessage returns the canonical error code and message
// from the JSON shapes accepted by the built-in upstream classifier. It is used by
// configurable cooldown detection so configured rules and built-in handling see
// the same normalized fields.
func ExtractUpstreamErrorCodeAndMessage(responseBody []byte) (string, string) {
	parsed, ok := parseStructuredQuotaError(responseBody, false)
	if !ok {
		return "", ""
	}
	return parsed.code, parsed.message
}

func coalesceInt64(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

func normalizeStructuredScalar(value any) string {
	return strings.ToUpper(strings.TrimSpace(structuredScalarString(value)))
}

func structuredScalarString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	default:
		return ""
	}
}

/**
 * @brief 解析结构化配额重置时间或重试延迟。
 * @param quotaErr 上游配额错误字段。
 * @param now 相对延迟的起算时间。
 * @return 未来的恢复时间；未提供有效时间时为零值。
 * @return 识别到有效截止时间时为 true。
 */
func parseStructuredCooldownUntil(quotaErr structuredQuotaError, now time.Time) (time.Time, bool) {
	if quotaErr.resetSeconds > 0 && quotaErr.resetSeconds <= int64((1<<63-1)/time.Second) {
		return now.Add(time.Duration(quotaErr.resetSeconds) * time.Second), true
	}

	// resets_at: unix 时间戳（秒），必须在当前时刻之后
	if quotaErr.resetsAt > 0 {
		until := time.Unix(quotaErr.resetsAt, 0)
		if until.After(now) {
			return until, true
		}
	}

	if quotaErr.quotaResetTimeStamp != "" {
		until, err := time.Parse(time.RFC3339, quotaErr.quotaResetTimeStamp)
		if err == nil && until.After(now) {
			return until, true
		}
	}

	if quotaErr.resetTime != "" {
		duration, err := time.ParseDuration(quotaErr.resetTime)
		if err == nil && duration > 0 {
			return now.Add(duration), true
		}
	}

	for _, delay := range [...]string{quotaErr.retryDelay, quotaErr.quotaResetDelay} {
		duration, err := time.ParseDuration(delay)
		if err == nil && duration > 0 {
			return now.Add(duration), true
		}
	}

	return time.Time{}, false
}

func parseCodexUsageFrequencyLimitCooldown(message string, now time.Time) (time.Time, bool) {
	matches := codexUsageFrequencyLimitResetRegex.FindStringSubmatch(message)
	if len(matches) < 4 {
		return time.Time{}, false
	}

	hours, err := strconv.Atoi(matches[3])
	if err != nil || hours > 23 {
		return time.Time{}, false
	}
	minutes := 0
	if len(matches) > 4 && matches[4] != "" {
		minutes, err = strconv.Atoi(matches[4])
		if err != nil || minutes > 59 {
			return time.Time{}, false
		}
	}
	offsetSeconds := (hours*60 + minutes) * 60
	if matches[2] == "-" {
		offsetSeconds = -offsetSeconds
	}

	dateTime := strings.Join(strings.Fields(matches[1]), " ")
	location := time.FixedZone("UTC"+matches[2]+strconv.Itoa(hours), offsetSeconds)
	until, err := time.ParseInLocation("2006-01-02 15:04:05", dateTime, location)
	if err != nil || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}

/**
 * @brief 读取 Google 错误正文给出的配额恢复日期或延迟。
 * @param body 上游错误 JSON。
 * @param now 相对延迟的起算时间。
 * @return 恢复时间；无有效提示时为空。
 * @return 有明确计时提示时为 true，含显式零秒。
 */
func GoogleRateLimitRetryAt(body []byte, now time.Time) (time.Time, bool) {
	quota, _ := parseStructuredQuotaError(body, false)
	if until, ok := parseStructuredCooldownUntil(quota, now); ok {
		return until, true
	}
	if delay, err := time.ParseDuration(strings.ToLower(quota.quotaResetDelay)); err == nil && delay >= 0 {
		return now.Add(delay), true
	}
	return parseGoogleMessageRetryAt(quota.message, now, true)
}

/**
 * @brief 解析 Google 错误中明确给出的 retry in 时长。
 * @param message 上游错误文案。
 * @param now 相对延迟的起算时间。
 * @param allowZero 是否允许显式零秒。
 * @return 恢复时间；无有效提示时为空。
 * @return 有可采用的时长时为 true。
 */
func parseGoogleMessageRetryAt(message string, now time.Time, allowZero bool) (time.Time, bool) {
	matches := retryInDurationRegex.FindStringSubmatch(message)
	if matches == nil {
		return time.Time{}, false
	}

	duration, err := time.ParseDuration(matches[1])
	if err != nil || duration < 0 || (!allowZero && duration == 0) {
		return time.Time{}, false
	}
	return now.Add(duration), true
}

/**
 * @brief 读取上游文案明确给出的 Retry after … seconds 延迟。
 * @param message 上游错误文案。
 * @param now 相对延迟的起算时间。
 * @return 未来的恢复时间；未提供有效延迟时为零值。
 * @return 含有效正秒数时为 true。
 */
func parseRetryAfterSecondsCooldownUntil(message string, now time.Time) (time.Time, bool) {
	matches := retryAfterSecondsRegex.FindStringSubmatch(message)
	if matches == nil {
		return time.Time{}, false
	}

	seconds, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil || seconds <= 0 || seconds > int64((1<<63-1)/time.Second) {
		return time.Time{}, false
	}
	return now.Add(time.Duration(seconds) * time.Second), true
}

func parseRollingFreeAllowanceCooldownUntil(message string, now time.Time) (time.Time, bool) {
	matches := rollingFreeAllowanceResetRegex.FindStringSubmatch(message)
	if matches == nil {
		return time.Time{}, false
	}

	until, err := time.Parse(time.RFC3339Nano, matches[1])
	if err != nil || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}

// parseTryAgainInCooldownUntil 解析 "Try again in 2h 18m"、"1.5h"、"2 hours 5 minutes" 等时长。
func parseTryAgainInCooldownUntil(message string, now time.Time) (time.Time, bool) {
	match := tryAgainInHMRegex.FindStringSubmatch(message)
	if match == nil {
		return time.Time{}, false
	}
	var total float64
	for _, part := range tryAgainInPartRegex.FindAllStringSubmatch(match[1], -1) {
		number, err := strconv.ParseFloat(part[1], 64)
		if err != nil {
			return time.Time{}, false
		}
		unit := time.Second
		switch strings.ToLower(part[2])[0] {
		case 'h':
			unit = time.Hour
		case 'm':
			unit = time.Minute
		}
		total += number * float64(unit)
	}
	if total <= 0 || total > float64(366*24*time.Hour) {
		return time.Time{}, false
	}
	return now.Add(time.Duration(total)), true
}

func parseGlobalFixedWindowQuotaCooldownUntil(message string, now time.Time) (time.Time, bool) {
	matches := globalFixedWindowRetryClockRegex.FindStringSubmatch(message)
	if matches == nil {
		return time.Time{}, false
	}

	hour, err := strconv.Atoi(matches[2])
	if err != nil || hour < 0 || hour > 23 {
		return time.Time{}, false
	}

	minute, err := strconv.Atoi(matches[3])
	if err != nil || minute < 0 || minute > 59 {
		return time.Time{}, false
	}

	dayOffset := 0
	if matches[1] == "明天" {
		dayOffset = 1
	}

	// 刻意硬编码东八区：此分支只解析中文公益站文案（"明天 12:00"），
	// 其重置时刻是站点北京时间，与部署机器的 time.Local 无关。
	loc := time.FixedZone("Asia/Shanghai", 8*60*60)
	localNow := now.In(loc)
	y, mon, d := localNow.Date()
	until := time.Date(y, mon, d+dayOffset, hour, minute, 0, 0, loc)
	if !until.After(localNow) {
		return time.Time{}, false
	}

	return until, true
}

func nextLocalMidnight(now time.Time) time.Time {
	local := now.In(time.Local)
	y, m, d := local.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.Local)
}

func parseBeijingTomorrowResetTime(message string, now time.Time) (time.Time, bool) {
	if !strings.Contains(message, "北京时间") {
		return time.Time{}, false
	}

	matches := beijingTomorrowResetRegex.FindStringSubmatch(message)
	if matches == nil {
		return time.Time{}, false
	}

	hour, err := strconv.Atoi(matches[1])
	if err != nil || hour < 0 || hour > 23 {
		return time.Time{}, false
	}

	minute := 0
	if len(matches) > 2 && matches[2] != "" {
		minute, err = strconv.Atoi(matches[2])
		if err != nil || minute < 0 || minute > 59 {
			return time.Time{}, false
		}
	}

	loc := time.FixedZone("Asia/Shanghai", 8*60*60)
	local := now.In(loc)
	y, mon, d := local.Date()
	return time.Date(y, mon, d+1, hour, minute, 0, 0, loc), true
}

// classify404Error 根据响应体内容智能分类 404 错误
// 设计原则：404 本身是异常情况，只有明确的客户端错误才不切换
//   - 模型不存在（客户端级）：明确的 model_not_found，或文案同时指向 model 与不可用状态
//   - 其他情况（渠道级）：空响应、HTML、异常 JSON 等都应切换渠道
func classify404Error(responseBody []byte) ErrorLevel {
	// 仅当明确是"模型不存在"时才视为客户端错误
	if isModelUnavailableResponse(responseBody) {
		return ErrorLevelClient
	}

	// 其他 404 一律视为渠道问题（HTML/JSON/其他）
	// 例如：BaseURL 配错、上游服务异常、路由不存在等
	return ErrorLevelChannel
}

// isModelUnavailableResponse 只识别明确的模型不可用语义。
// 普通 endpoint/BaseURL 错误必须继续按资源自身的故障级别处理，不能因为请求里带了模型名就误伤模型。
func isModelUnavailableResponse(responseBody []byte) bool {
	if len(responseBody) == 0 {
		return false
	}
	bodyLower := strings.ToLower(string(responseBody))
	if strings.Contains(bodyLower, "model_not_found") {
		return true
	}
	if strings.Contains(bodyLower, "模型") {
		return strings.Contains(bodyLower, "不支持") ||
			strings.Contains(bodyLower, "不存在") ||
			strings.Contains(bodyLower, "未找到") ||
			strings.Contains(bodyLower, "找不到") ||
			strings.Contains(bodyLower, "不可用") ||
			strings.Contains(bodyLower, "已下线") ||
			strings.Contains(bodyLower, "停止服务")
	}
	if !strings.Contains(bodyLower, "model") {
		return false
	}
	return strings.Contains(bodyLower, "unsupported") ||
		strings.Contains(bodyLower, "not supported") ||
		strings.Contains(bodyLower, "not found") ||
		strings.Contains(bodyLower, "does not exist") ||
		strings.Contains(bodyLower, "not available") ||
		strings.Contains(bodyLower, "no longer available") ||
		strings.Contains(bodyLower, "end of life")
}

// ShouldFallbackProtocol reports whether automatic protocol negotiation may
// retry the same request using the channel protocol. This decision is separate
// from cooldown classification: a non-model 404 can be a broken base URL or
// deployment and still means the native protocol probe did not succeed. Some
// compatible gateways report an unsupported native request shape as a
// structured 500 instead of an endpoint status. A 400 needs explicit capability
// evidence: rejection before execution alone does not establish incompatibility.
func ShouldFallbackProtocol(statusCode int, responseBody []byte) bool {
	switch statusCode {
	case http.StatusBadRequest:
		return isUnsupportedProtocolRequest(responseBody)
	case http.StatusForbidden:
		return isCloudflareBlockPage(responseBody)
	case 405:
		return true
	case 404:
		return !isModelUnavailableResponse(responseBody)
	case 500:
		return isProtocolConversionNotImplemented(responseBody)
	default:
		return false
	}
}

/**
 * @brief 识别 Sub2API 拒绝当前 Key 所属分组使用 Messages 的错误。
 * @param statusCode 上游 HTTP 状态码。
 * @param responseBody 上游错误响应体。
 * @return 仅当状态、错误类型和消息匹配分组拒绝时返回 true。
 */
func IsMessagesDispatchForbidden(statusCode int, responseBody []byte) bool {
	if statusCode != http.StatusForbidden {
		return false
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal(responseBody, &payload) == nil &&
		payload.Error.Type == "permission_error" &&
		payload.Error.Message == "This group does not allow /v1/messages dispatch"
}

func isUnsupportedProtocolRequest(responseBody []byte) bool {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return false
	}
	if strings.EqualFold(payload.Error.Code, "RESPONSES_MODEL_NOT_SUPPORTED") || isProtocolConversionNotImplemented(responseBody) {
		return true
	}
	message := strings.ToLower(payload.Error.Message)
	// A malformed beta value is a data error; only an explicitly unsupported
	// beta capability permits trying another protocol.
	return strings.Contains(message, "anthropic-beta") &&
		(strings.Contains(message, "unsupported") || strings.Contains(message, "not supported") ||
			strings.Contains(message, "不支持"))
}

// firstHeaderValueFold 大小写无关地取首个 header 值。
// HTTP 转发路径写入的是 canonical 形式，管理测试路径来自 JSON 反序列化，
// 键名大小写不可控，因此不能依赖 map 直接索引。
func firstHeaderValueFold(headers map[string][]string, name string) string {
	values := headerValuesFold(headers, name)
	if len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	return ""
}

func headerValuesFold(headers map[string][]string, name string) []string {
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			return values
		}
	}
	return nil
}

func isCloudflareBlockPage(responseBody []byte) bool {
	body := strings.ToLower(string(responseBody))
	return strings.Contains(body, "<title>attention required! | cloudflare</title>") &&
		strings.Contains(body, "sorry, you have been blocked")
}

// isCloudflareChallengeResponse 判断响应是否为 Cloudflare 质询或封锁页。
// 优先看 cf-mitigated 响应头（质询时为 "challenge"），再回退到正文关键字。
// 两类页面都在模型执行前拒绝请求，对冷却决策而言语义等价。
func isCloudflareChallengeResponse(cfMitigated string, responseBody []byte) bool {
	if strings.EqualFold(strings.TrimSpace(cfMitigated), "challenge") {
		return true
	}
	if strings.Contains(strings.ToLower(string(responseBody)), "<title>just a moment...</title>") {
		return true
	}
	return isCloudflareBlockPage(responseBody)
}

// isCloudflareBotBlockResponse 识别 Cloudflare 1010（browser_signature_banned）边缘拦截
// （对齐 sub2api e0fa4b94f）：纯文本 `error code: 1010`，或 JSON 中 cloudflare_error=true
// 且 code/error_code 为 1010，或 error_name=browser_signature_banned。普通 JSON 里的
// code 1010 与正文提到 1010 都不算，避免把上游业务错误误判为边缘拦截。
func isCloudflareBotBlockResponse(responseBody []byte) bool {
	var payload any
	if json.Unmarshal(responseBody, &payload) == nil {
		return containsCloudflareBotBlock(payload)
	}
	return strings.Contains(strings.ToLower(string(responseBody)), "error code: 1010")
}

func containsCloudflareBotBlock(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		cloudflareError := false
		for key, child := range node {
			if is, ok := child.(bool); ok && is && strings.EqualFold(key, "cloudflare_error") {
				cloudflareError = true
			}
		}
		for key, child := range node {
			if name, ok := child.(string); ok && strings.EqualFold(key, "error_name") &&
				strings.EqualFold(strings.TrimSpace(name), "browser_signature_banned") {
				return true
			}
			if cloudflareError && (strings.EqualFold(key, "code") || strings.EqualFold(key, "error_code")) &&
				isCloudflareBotBlockCode(child) {
				return true
			}
			if containsCloudflareBotBlock(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(node, containsCloudflareBotBlock)
	}
	return false
}

func isCloudflareBotBlockCode(value any) bool {
	switch code := value.(type) {
	case float64:
		return code == 1010
	case string:
		return strings.TrimSpace(code) == "1010"
	default:
		return false
	}
}

func isProtocolConversionNotImplemented(responseBody []byte) bool {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(payload.Error.Code), "convert_request_failed") &&
		strings.Contains(strings.ToLower(payload.Error.Message), "not implemented")
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

	// 2. 检查是否为1308或1310错误（优先使用type，如果为空则使用code）
	errorType := errResp.ErrorType()
	if errorType != "1308" && errorType != "1310" {
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

/**
 * @brief 将传输、取消或请求语义错误归类为代理状态和换渠决策。
 * @param err 本次转发错误；nil 表示成功。
 * @return HTTP 状态码或能保留超时阶段的内部状态码。
 * @return 冷却错误级别；err 为 nil 时返回 ErrorLevelNone。
 * @return 出站故障允许重试时为 true；本地取消和客户端请求错误为 false。
 */
func ClassifyError(err error) (statusCode int, errorLevel ErrorLevel, shouldRetry bool) {
	if err == nil {
		return 200, ErrorLevelNone, false
	}

	// 快速路径1：专门识别上游首字节超时，优先切换渠道
	if errors.Is(err, ErrUpstreamFirstByteTimeout) {
		return StatusFirstByteTimeout, ErrorLevelChannel, true
	}
	if errors.Is(err, ErrUpstreamStreamTimeout) {
		return StatusStreamIncomplete, ErrorLevelChannel, true
	}

	// 快速路径1.2：上游 200 空体是坏网关，不是成功响应。
	if errors.Is(err, ErrUpstreamEmptyResponse) {
		return http.StatusBadGateway, ErrorLevelChannel, true
	}
	if errors.Is(err, ErrUpstreamInvalidResponse) {
		return http.StatusBadGateway, ErrorLevelChannel, true
	}

	// 快速路径1.5：协议转换明确声明为客户端请求结构不支持，或上游拦截了提示词
	if errors.Is(err, protocol.ErrUnsupportedRequestShape) || errors.Is(err, ErrUpstreamPromptBlocked) {
		return http.StatusBadRequest, ErrorLevelClient, false
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
	if netErr, ok := errors.AsType[net.Error](err); ok {
		if netErr.Timeout() {
			return 504, ErrorLevelChannel, true // Gateway Timeout，可重试
		}
	}

	// http.Client.Do 用 *url.Error 包装出站错误，broken pipe 表示上游连接断开。
	// 下游写入断开时按客户端取消处理。
	if _, upstream := errors.AsType[*url.Error](err); !upstream && strings.Contains(strings.ToLower(err.Error()), "broken pipe") {
		return StatusClientClosedRequest, ErrorLevelClient, false
	}
	return http.StatusBadGateway, ErrorLevelChannel, true
}

// IsModelScopedNetworkError 判断网络错误是否只应冷却当前实际模型。
// DNS、连接拒绝、路由不可达等基础设施错误仍属于渠道级。
func IsModelScopedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUpstreamStreamTimeout) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, ErrUpstreamFirstByteTimeout) ||
		errors.Is(err, ErrUpstreamEmptyResponse) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	return isModelScopedNetworkErrorText(strings.ToLower(err.Error()))
}

func isModelScopedNetworkErrorText(errLower string) bool {
	return strings.Contains(errLower, "connection reset by peer") ||
		strings.Contains(errLower, "http2: response body closed") ||
		strings.Contains(errLower, "stream error:") ||
		strings.Contains(errLower, "empty response") ||
		strings.Contains(errLower, "connection timeout")
}
