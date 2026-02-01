package util

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func assertClassifyError(t *testing.T, err error, wantStatus int, wantLevel ErrorLevel, wantRetry bool, reason string) {
	t.Helper()

	statusCode, errorLevel, shouldRetry := ClassifyError(err)
	if statusCode != wantStatus {
		t.Errorf("状态码错误: 期望 %d, 实际 %d (%s)", wantStatus, statusCode, reason)
	}
	if errorLevel != wantLevel {
		t.Errorf("错误级别错误: 期望 %v, 实际 %v (%s)", wantLevel, errorLevel, reason)
	}
	if shouldRetry != wantRetry {
		t.Errorf("重试标志错误: 期望 %v, 实际 %v (%s)", wantRetry, shouldRetry, reason)
	}

}

func TestClassifyHTTPResponse(t *testing.T) {
	tests := []struct {
		name         string
		statusCode   int
		responseBody []byte
		expected     ErrorLevel
		reason       string
	}{
		// 401错误 - Key级场景（新设计：额度用尽先尝试其他Key）
		{
			name:         "401_quota_exhausted_chinese",
			statusCode:   401,
			responseBody: []byte(`{"error":{"message":"该令牌额度已用尽"}}`),
			expected:     ErrorLevelKey,
			reason:       "额度用尽可能只是单个Key问题，应先尝试其他Key",
		},
		{
			name:         "401_insufficient_quota",
			statusCode:   401,
			responseBody: []byte(`{"error":"insufficient_quota"}`),
			expected:     ErrorLevelKey,
			reason:       "insufficient_quota可能只是单个Key问题，应先尝试其他Key",
		},
		{
			name:         "401_balance_insufficient",
			statusCode:   401,
			responseBody: []byte(`{"error":"balance insufficient"}`),
			expected:     ErrorLevelKey,
			reason:       "余额不足可能只是单个Key问题，应先尝试其他Key",
		},

		// 401错误 - 渠道级场景（仅限账户级不可逆错误）
		{
			name:         "401_account_suspended",
			statusCode:   401,
			responseBody: []byte(`{"error":"account suspended"}`),
			expected:     ErrorLevelChannel,
			reason:       "账户暂停是账户级不可逆错误，应冷却整个渠道",
		},
		{
			name:         "401_account_disabled",
			statusCode:   401,
			responseBody: []byte(`{"error":"account disabled"}`),
			expected:     ErrorLevelChannel,
			reason:       "账户禁用是账户级不可逆错误，应冷却整个渠道",
		},

		// 401错误 - Key级场景
		{
			name:         "401_invalid_authentication",
			statusCode:   401,
			responseBody: []byte(`{"error":"invalid_authentication"}`),
			expected:     ErrorLevelKey,
			reason:       "认证失败应该仅冷却当前Key",
		},
		{
			name:         "401_token_expired",
			statusCode:   401,
			responseBody: []byte(`{"error":"token expired"}`),
			expected:     ErrorLevelKey,
			reason:       "Token过期应该仅冷却当前Key（与account expired区分）",
		},
		{
			name:         "401_unauthorized",
			statusCode:   401,
			responseBody: []byte(`{"error":"unauthorized"}`),
			expected:     ErrorLevelKey,
			reason:       "未授权应该仅冷却当前Key",
		},
		{
			name:         "401_empty_body",
			statusCode:   401,
			responseBody: []byte{},
			expected:     ErrorLevelKey,
			reason:       "无响应体默认为Key级错误",
		},

		// 403错误 - Key级场景（新设计：额度/限额类错误先尝试其他Key）
		{
			name:         "403_quota_exceeded_openai_style",
			statusCode:   403,
			responseBody: []byte(`{"error":{"type":"quota_exceeded","message":"Daily cost limit reached ($1)"},"details":{"dailyLimit":1,"currentUsage":1.09655195,"remaining":0}}`),
			expected:     ErrorLevelKey,
			reason:       "quota_exceeded可能只是单个Key问题，应先尝试其他Key",
		},
		{
			name:         "403_daily_limit_reached",
			statusCode:   403,
			responseBody: []byte(`{"error":"Daily cost limit reached ($10)"}`),
			expected:     ErrorLevelKey,
			reason:       "Daily limit可能只是单个Key问题，应先尝试其他Key",
		},
		{
			name:         "403_remaining_zero",
			statusCode:   403,
			responseBody: []byte(`{"details":{"remaining":0}}`),
			expected:     ErrorLevelKey,
			reason:       "remaining:0可能只是单个Key问题，应先尝试其他Key",
		},
		{
			name:         "403_quota_chinese",
			statusCode:   403,
			responseBody: []byte(`{"error":"余额不足，请充值"}`),
			expected:     ErrorLevelKey,
			reason:       "余额不足可能只是单个Key问题，应先尝试其他Key",
		},

		// 403错误 - 渠道级场景（仅限账户级不可逆错误）
		{
			name:         "403_service_disabled",
			statusCode:   403,
			responseBody: []byte(`{"error":"service disabled"}`),
			expected:     ErrorLevelChannel,
			reason:       "服务禁用是账户级不可逆错误，应冷却整个渠道",
		},

		// 403错误 - Key级场景
		{
			name:         "403_permission_denied",
			statusCode:   403,
			responseBody: []byte(`{"error":"permission denied"}`),
			expected:     ErrorLevelKey,
			reason:       "403 + permission denied应该仅冷却当前Key",
		},
		{
			name:         "403_forbidden",
			statusCode:   403,
			responseBody: []byte(`{"error":"forbidden"}`),
			expected:     ErrorLevelKey,
			reason:       "403 + forbidden应该仅冷却当前Key",
		},
		{
			name:         "403_empty_body",
			statusCode:   403,
			responseBody: []byte{},
			expected:     ErrorLevelKey,
			reason:       "403无响应体默认为Key级错误",
		},

		// 其他状态码（确保不影响现有逻辑）
		{
			name:         "404_not_found",
			statusCode:   404,
			responseBody: []byte(`{"error":"not found"}`),
			expected:     ErrorLevelClient,
			reason:       "404应该返回客户端错误",
		},
		{
			name:         "500_internal_error",
			statusCode:   500,
			responseBody: []byte(`{"error":"internal server error"}`),
			expected:     ErrorLevelChannel,
			reason:       "500应该冷却渠道",
		},
		{
			name:         "429_rate_limit",
			statusCode:   429,
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelKey,
			reason:       "429应该冷却Key",
		},

		// 边界情况
		{
			name:         "401_case_insensitive",
			statusCode:   401,
			responseBody: []byte(`{"error":"Account SUSPENDED"}`),
			expected:     ErrorLevelChannel,
			reason:       "大小写不敏感匹配（account suspended）",
		},
		{
			name:         "401_mixed_case",
			statusCode:   401,
			responseBody: []byte(`{"error":"Account Disabled"}`),
			expected:     ErrorLevelChannel,
			reason:       "混合大小写应该正确识别（account disabled）",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ClassifyHTTPResponseWithMeta(tt.statusCode, nil, tt.responseBody).Level
			if result != tt.expected {
				t.Errorf("%s\n  期望: %v\n  实际: %v\n  原因: %s\n  响应体: %s",
					tt.name, tt.expected, result, tt.reason, string(tt.responseBody))
			}
		})
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	tests := []struct {
		statusCode int
		expected   ErrorLevel
		reason     string
	}{
		{200, ErrorLevelNone, "2xx成功"},
		{201, ErrorLevelNone, "2xx成功"},
		{402, ErrorLevelKey, "402 应视为Key级（额度/余额/配额）"},
		{401, ErrorLevelKey, "Key级错误（默认）"},
		{403, ErrorLevelKey, "Key级错误"},
		{429, ErrorLevelKey, "Key级限流"},
		// 499 HTTP响应应触发渠道级重试
		{499, ErrorLevelChannel, "499来自HTTP响应时，说明上游API返回，应重试其他渠道"},
		// nginx 非标准状态码
		{444, ErrorLevelChannel, "444 nginx No Response - 服务器主动关闭连接，渠道级错误"},
		{500, ErrorLevelChannel, "渠道级错误"},
		{502, ErrorLevelChannel, "渠道级错误"},
		{503, ErrorLevelChannel, "渠道级错误"},
		{504, ErrorLevelChannel, "渠道级错误"},
		{520, ErrorLevelChannel, "520 Web Server Returned an Unknown Error - 渠道级错误"},
		{521, ErrorLevelChannel, "521 Web Server Is Down - 渠道级错误"},
		{524, ErrorLevelChannel, "524 A Timeout Occurred - 渠道级错误"},
		// 兜底策略测试
		{418, ErrorLevelKey, "418 未知4xx - 兜底策略应为Key级冷却"},
		{451, ErrorLevelKey, "451 未知4xx - 兜底策略应为Key级冷却"},
		{599, ErrorLevelChannel, "599 未知5xx - 兜底策略应为Channel级冷却"},
	}

	for _, tt := range tests {
		t.Run(string(rune(tt.statusCode)), func(t *testing.T) {
			result := ClassifyHTTPStatus(tt.statusCode)
			if result != tt.expected {
				t.Errorf("状态码 %d: 期望 %v, 实际 %v (%s)", tt.statusCode, tt.expected, result, tt.reason)
			}
		})
	}
}

// 测试context.Canceled与HTTP 499的区分
func TestClassifyError_ContextCanceled(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		expectedStatus int
		expectedLevel  ErrorLevel
		expectedRetry  bool
		reason         string
	}{
		{
			name:           "context_canceled_from_client",
			err:            context.Canceled,
			expectedStatus: 499,
			expectedLevel:  ErrorLevelClient,
			expectedRetry:  false,
			reason:         "下游客户端取消请求（context.Canceled）应返回499+ErrorLevelClient，不重试",
		},
		{
			name:           "context_deadline_exceeded",
			err:            context.DeadlineExceeded,
			expectedStatus: 504,
			expectedLevel:  ErrorLevelChannel,
			expectedRetry:  true,
			reason:         "上游超时（context.DeadlineExceeded）应返回504+ErrorLevelChannel，可重试其他渠道",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertClassifyError(t, tt.err, tt.expectedStatus, tt.expectedLevel, tt.expectedRetry, tt.reason)
		})
	}
}

// 测试空响应错误分类
func TestClassifyError_EmptyResponse(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		expectedStatus int
		expectedLevel  ErrorLevel
		expectedRetry  bool
		reason         string
	}{
		{
			name:           "empty_response_200_with_content_length_0",
			err:            errors.New("upstream returned empty response (200 OK with Content-Length: 0)"),
			expectedStatus: 502,
			expectedLevel:  ErrorLevelChannel,
			expectedRetry:  true,
			reason:         "200状态码但Content-Length=0应视为上游故障，触发渠道级重试",
		},
		{
			name:           "empty_response_uppercase",
			err:            errors.New("Upstream Returned Empty Response (200 OK with Content-Length: 0)"),
			expectedStatus: 502,
			expectedLevel:  ErrorLevelChannel,
			expectedRetry:  true,
			reason:         "大小写不敏感匹配",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertClassifyError(t, tt.err, tt.expectedStatus, tt.expectedLevel, tt.expectedRetry, tt.reason)
		})
	}
}

// 测试HTTP/2流错误分类
func TestClassifyError_HTTP2StreamErrors(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		expectedStatus int
		expectedLevel  ErrorLevel
		expectedRetry  bool
		reason         string
	}{
		{
			name:           "http2_response_body_closed",
			err:            fmt.Errorf("http2: response body closed"),
			expectedStatus: 502, // Bad Gateway
			expectedLevel:  ErrorLevelChannel,
			expectedRetry:  true,
			reason:         "上游服务器主动关闭HTTP/2流，应触发渠道级重试",
		},
		{
			name:           "http2_stream_error_internal",
			err:            fmt.Errorf("stream error: stream ID 7; INTERNAL_ERROR"),
			expectedStatus: 502, // Bad Gateway
			expectedLevel:  ErrorLevelChannel,
			expectedRetry:  true,
			reason:         "HTTP/2 RST_STREAM INTERNAL_ERROR，上游服务异常",
		},
		{
			name:           "http2_stream_error_protocol",
			err:            fmt.Errorf("stream error: stream ID 3; PROTOCOL_ERROR"),
			expectedStatus: 502, // Bad Gateway
			expectedLevel:  ErrorLevelChannel,
			expectedRetry:  true,
			reason:         "HTTP/2协议错误，应切换渠道重试",
		},
		{
			name:           "http2_error_wrapped",
			err:            fmt.Errorf("failed to read response: http2: response body closed"),
			expectedStatus: 502, // Bad Gateway
			expectedLevel:  ErrorLevelChannel,
			expectedRetry:  true,
			reason:         "包装后的HTTP/2错误也应正确识别",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertClassifyError(t, tt.err, tt.expectedStatus, tt.expectedLevel, tt.expectedRetry, tt.reason)
		})
	}
}

func TestClassifyError_ConnectionResetAndBrokenPipe(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		expectedStatus int
		expectedLevel  ErrorLevel
		expectedRetry  bool
		reason         string
	}{
		{
			name:           "broken_pipe_client_closed",
			err:            errors.New("write: broken pipe"),
			expectedStatus: 499,
			expectedLevel:  ErrorLevelClient,
			expectedRetry:  false,
			reason:         "broken pipe 基本是客户端断开，不应重试",
		},
		{
			name:           "connection_reset_by_peer_upstream",
			err:            errors.New("read: connection reset by peer"),
			expectedStatus: 502,
			expectedLevel:  ErrorLevelChannel,
			expectedRetry:  true,
			reason:         "connection reset by peer 通常是上游断开，应按 502 进入健康度统计并允许切换渠道重试",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertClassifyError(t, tt.err, tt.expectedStatus, tt.expectedLevel, tt.expectedRetry, tt.reason)
		})
	}
}

// 测试429错误的智能分类
func TestClassifyRateLimitError(t *testing.T) {
	tests := []struct {
		name         string
		headers      map[string][]string
		responseBody []byte
		expected     ErrorLevel
		reason       string
	}{
		// Retry-After头测试
		{
			name: "retry_after_120_seconds",
			headers: map[string][]string{
				"Retry-After": {"120"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelChannel,
			reason:       "Retry-After > 60秒表示账户级或IP级限流，应冷却渠道",
		},
		{
			name: "retry_after_30_seconds",
			headers: map[string][]string{
				"Retry-After": {"30"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelKey,
			reason:       "Retry-After ≤ 60秒表示Key级限流，应冷却Key",
		},
		{
			name: "retry_after_60_seconds_boundary",
			headers: map[string][]string{
				"Retry-After": {"60"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelKey,
			reason:       "Retry-After = 60秒边界值，保守策略为Key级",
		},
		{
			name: "retry_after_http_date",
			headers: map[string][]string{
				"Retry-After": {"Wed, 29 Oct 2025 12:00:00 GMT"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelChannel,
			reason:       "HTTP日期格式通常表示长时间限流，应冷却渠道",
		},
		{
			name: "retry_after_invalid_format",
			headers: map[string][]string{
				"Retry-After": {"invalid"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelKey,
			reason:       "无效的Retry-After格式应默认为Key级",
		},

		// X-RateLimit-Scope头测试
		{
			name: "scope_global",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"global"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelChannel,
			reason:       "global scope表示全局限流，应冷却渠道",
		},
		{
			name: "scope_ip",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"ip"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelChannel,
			reason:       "IP scope表示IP级限流，应冷却渠道",
		},
		{
			name: "scope_account",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"account"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelChannel,
			reason:       "account scope表示账户级限流，应冷却渠道",
		},
		{
			name: "scope_user",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"user"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelKey,
			reason:       "user scope（非global/ip/account）应默认为Key级",
		},
		{
			name: "scope_case_insensitive",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"GLOBAL"},
			},
			responseBody: []byte(`{"error":"rate limit exceeded"}`),
			expected:     ErrorLevelChannel,
			reason:       "scope匹配应不区分大小写",
		},

		// 响应体错误描述测试
		{
			name:    "body_ip_rate_limit",
			headers: map[string][]string{},
			responseBody: []byte(`{
				"error": {
					"message": "IP rate limit exceeded",
					"type": "rate_limit_error"
				}
			}`),
			expected: ErrorLevelChannel,
			reason:   "响应体包含'ip rate limit'应冷却渠道",
		},
		{
			name:    "body_account_rate_limit",
			headers: map[string][]string{},
			responseBody: []byte(`{
				"error": {
					"message": "Account rate limit exceeded for organization"
				}
			}`),
			expected: ErrorLevelChannel,
			reason:   "响应体包含'account rate limit'应冷却渠道",
		},
		{
			name:    "body_global_rate_limit",
			headers: map[string][]string{},
			responseBody: []byte(`{
				"error": "Global rate limit has been exceeded"
			}`),
			expected: ErrorLevelChannel,
			reason:   "响应体包含'global rate limit'应冷却渠道",
		},
		{
			name:    "body_organization_limit",
			headers: map[string][]string{},
			responseBody: []byte(`{
				"error": "Organization limit reached"
			}`),
			expected: ErrorLevelChannel,
			reason:   "响应体包含'organization limit'应冷却渠道",
		},
		{
			name:    "body_case_insensitive",
			headers: map[string][]string{},
			responseBody: []byte(`{
				"error": "IP Rate Limit Exceeded"
			}`),
			expected: ErrorLevelChannel,
			reason:   "响应体匹配应不区分大小写",
		},

		// 默认Key级限流测试
		{
			name:    "default_key_level_no_special_indicators",
			headers: map[string][]string{},
			responseBody: []byte(`{
				"error": "Too many requests"
			}`),
			expected: ErrorLevelKey,
			reason:   "无特殊指示器时应默认为Key级",
		},
		{
			name:         "nil_headers",
			headers:      nil,
			responseBody: []byte(`{"error":"rate limit"}`),
			expected:     ErrorLevelKey,
			reason:       "nil headers应默认为Key级",
		},
		{
			name:         "empty_headers",
			headers:      map[string][]string{},
			responseBody: []byte(`{"error":"rate limit"}`),
			expected:     ErrorLevelKey,
			reason:       "空headers应默认为Key级",
		},
		{
			name:         "empty_response_body",
			headers:      map[string][]string{},
			responseBody: []byte{},
			expected:     ErrorLevelKey,
			reason:       "空响应体应默认为Key级",
		},
		{
			name:         "nil_response_body",
			headers:      map[string][]string{},
			responseBody: nil,
			expected:     ErrorLevelKey,
			reason:       "nil响应体应默认为Key级",
		},

		// 组合场景测试
		{
			name: "combined_retry_after_and_scope",
			headers: map[string][]string{
				"Retry-After":       {"30"}, // Key级指示器
				"X-Ratelimit-Scope": {"ip"}, // 渠道级指示器
			},
			responseBody: []byte(`{"error":"rate limit"}`),
			expected:     ErrorLevelChannel,
			reason:       "Retry-After检查优先于Scope，但>60秒判断优先",
		},
		{
			name: "combined_scope_and_body",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"user"}, // Key级指示器
			},
			responseBody: []byte(`{"error":"IP rate limit exceeded"}`), // 渠道级指示器
			expected:     ErrorLevelChannel,
			reason:       "响应体中的渠道级指示器应被识别",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ClassifyHTTPResponseWithMeta(429, tt.headers, tt.responseBody).Level
			if result != tt.expected {
				t.Errorf("%s\n  期望: %v\n  实际: %v\n  原因: %s",
					tt.name, tt.expected, result, tt.reason)
			}
		})
	}
}

// TestClassifySSEError 测试SSE error事件分类
func TestClassifySSEError(t *testing.T) {
	tests := []struct {
		name         string
		responseBody []byte
		expected     ErrorLevel
		reason       string
	}{
		{
			name:         "api_error_500",
			responseBody: []byte(`{"type":"error","error":{"type":"api_error","message":"上游API返回错误: 500"}}`),
			expected:     ErrorLevelChannel,
			reason:       "api_error表示上游服务错误，应触发渠道级冷却",
		},
		{
			name:         "overloaded_error",
			responseBody: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"服务过载"}}`),
			expected:     ErrorLevelChannel,
			reason:       "overloaded_error表示上游过载，应触发渠道级冷却",
		},
		{
			name:         "rate_limit_error",
			responseBody: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"请求过于频繁"}}`),
			expected:     ErrorLevelKey,
			reason:       "rate_limit_error可能只是单个Key限流，应触发Key级冷却",
		},
		{
			name:         "authentication_error",
			responseBody: []byte(`{"type":"error","error":{"type":"authentication_error","message":"认证失败"}}`),
			expected:     ErrorLevelKey,
			reason:       "authentication_error是Key级问题，应触发Key级冷却",
		},
		{
			name:         "invalid_request_error",
			responseBody: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"请求无效"}}`),
			expected:     ErrorLevelKey,
			reason:       "invalid_request_error是Key级问题，应触发Key级冷却",
		},
		{
			name:         "1308_error",
			responseBody: []byte(`{"type":"error","error":{"type":"1308","message":"已达到使用上限"}}`),
			expected:     ErrorLevelKey,
			reason:       "1308错误是Key配额问题，应触发Key级冷却",
		},
		{
			name:         "1308_error_with_code_field",
			responseBody: []byte(`{"error":{"code":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2025-12-21 15:00:05 重置。"},"request_id":"202512211335142b05cc4f9bbb4e6c"}`),
			expected:     ErrorLevelKey,
			reason:       "使用code字段的1308错误（非Anthropic格式）应触发Key级冷却",
		},
		{
			name:         "unknown_error_type",
			responseBody: []byte(`{"type":"error","error":{"type":"unknown_type","message":"未知错误"}}`),
			expected:     ErrorLevelKey,
			reason:       "未知错误类型应保守处理为Key级",
		},
		{
			name:         "empty_body",
			responseBody: []byte{},
			expected:     ErrorLevelKey,
			reason:       "空响应体应保守处理为Key级",
		},
		{
			name:         "invalid_json",
			responseBody: []byte(`not valid json`),
			expected:     ErrorLevelKey,
			reason:       "无效JSON应保守处理为Key级",
		},
		{
			name:         "nil_body",
			responseBody: nil,
			expected:     ErrorLevelKey,
			reason:       "nil响应体应保守处理为Key级",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 使用 ClassifyHTTPResponseWithMeta 测试 597 状态码
			result := ClassifyHTTPResponseWithMeta(StatusSSEError, nil, tt.responseBody).Level
			if result != tt.expected {
				t.Errorf("%s\n  期望: %v\n  实际: %v\n  原因: %s",
					tt.name, tt.expected, result, tt.reason)
			}
		})
	}
}

func TestClassify400Error(t *testing.T) {
	tests := []struct {
		name         string
		responseBody []byte
		expected     ErrorLevel
		reason       string
	}{
		{
			name:         "empty_body",
			responseBody: []byte{},
			expected:     ErrorLevelClient,
			reason:       "空响应体应判定为客户端错误",
		},
		{
			name:         "nil_body",
			responseBody: nil,
			expected:     ErrorLevelClient,
			reason:       "nil响应体应判定为客户端错误",
		},
		{
			name:         "invalid_api_key",
			responseBody: []byte(`{"error": {"message": "Invalid API Key provided"}}`),
			expected:     ErrorLevelKey,
			reason:       "包含 invalid_api_key 特征应判定为 Key 级错误",
		},
		{
			name:         "api_key_error",
			responseBody: []byte(`{"error": {"message": "The API key you provided is malformed"}}`),
			expected:     ErrorLevelKey,
			reason:       "包含 api key 特征应判定为 Key 级错误",
		},
		{
			name:         "bad_request_params",
			responseBody: []byte(`{"error": {"message": "Missing required parameter: 'model'"}}`),
			expected:     ErrorLevelClient,
			reason:       "请求参数错误应判定为客户端级错误",
		},
		{
			name:         "invalid_json_format",
			responseBody: []byte(`{"error": {"message": "Invalid JSON format in request body"}}`),
			expected:     ErrorLevelClient,
			reason:       "JSON格式错误应判定为客户端级错误",
		},
		{
			name:         "validation_error",
			responseBody: []byte(`{"error": {"message": "Validation failed: max_tokens must be positive"}}`),
			expected:     ErrorLevelClient,
			reason:       "参数验证错误应判定为客户端级错误",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classify400Error(tt.responseBody)
			if result != tt.expected {
				t.Errorf("%s\n  期望: %v\n  实际: %v\n  原因: %s\n  响应体: %s",
					tt.name, tt.expected, result, tt.reason, string(tt.responseBody))
			}
		})
	}
}

func TestClassify404Error(t *testing.T) {
	tests := []struct {
		name         string
		responseBody []byte
		expected     ErrorLevel
		reason       string
	}{
		{
			name:         "empty_body",
			responseBody: []byte{},
			expected:     ErrorLevelChannel,
			reason:       "空响应体应判定为渠道配置错误（路径不存在）",
		},
		{
			name:         "nil_body",
			responseBody: nil,
			expected:     ErrorLevelChannel,
			reason:       "nil响应体应判定为渠道配置错误（路径不存在）",
		},
		{
			name:         "model_not_found",
			responseBody: []byte(`{"error": {"message": "The model 'gpt-5' could not be found", "type": "model_not_found"}}`),
			expected:     ErrorLevelClient,
			reason:       "模型不存在应判定为客户端级错误",
		},
		{
			name:         "resource_not_exist",
			responseBody: []byte(`{"error": {"message": "The requested resource does not exist"}}`),
			expected:     ErrorLevelClient,
			reason:       "资源不存在应判定为客户端级错误",
		},
		{
			name: "html_error_page",
			responseBody: []byte(`<!DOCTYPE html>
<html>
<head><title>404 Not Found</title></head>
<body><h1>404 Not Found</h1></body>
</html>`),
			expected: ErrorLevelChannel,
			reason:   "HTML错误页面应判定为渠道级错误（BaseURL配置错误）",
		},
		{
			name:         "html_lowercase",
			responseBody: []byte(`<html><head><title>Not Found</title></head><body>404</body></html>`),
			expected:     ErrorLevelChannel,
			reason:       "HTML错误页面（小写）应判定为渠道级错误",
		},
		{
			name:         "endpoint_not_found",
			responseBody: []byte(`{"error": {"message": "Endpoint /v1/completions not found"}}`),
			expected:     ErrorLevelClient,
			reason:       "端点不存在应判定为客户端级错误（默认保守策略）",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classify404Error(tt.responseBody)
			if result != tt.expected {
				t.Errorf("%s\n  期望: %v\n  实际: %v\n  原因: %s\n  响应体: %s",
					tt.name, tt.expected, result, tt.reason, string(tt.responseBody))
			}
		})
	}
}

func TestGetStatusCodeMeta(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status      int
		wantLevel   ErrorLevel
		description string
	}{
		// Key级错误
		{401, ErrorLevelKey, "401 -> Key级"},
		{402, ErrorLevelKey, "402 -> Key级"},
		{403, ErrorLevelKey, "403 -> Key级"},
		{429, ErrorLevelKey, "429 -> Key级"},

		// 渠道级错误
		{499, ErrorLevelChannel, "499 -> 渠道级"},
		{500, ErrorLevelChannel, "500 -> 渠道级"},
		{502, ErrorLevelChannel, "502 -> 渠道级"},
		{503, ErrorLevelChannel, "503 -> 渠道级"},
		{504, ErrorLevelChannel, "504 -> 渠道级"},
		{408, ErrorLevelClient, "408 -> 客户端级 (RFC7231: 客户端发送请求慢)"},
		{405, ErrorLevelChannel, "405 -> 渠道级 (上游endpoint/方法不支持)"},

		// 自定义状态码
		{StatusQuotaExceeded, ErrorLevelKey, "596 -> Key级"},
		{StatusSSEError, ErrorLevelKey, "597 -> Key级"},
		{StatusFirstByteTimeout, ErrorLevelChannel, "598 -> 渠道级"},
		{StatusStreamIncomplete, ErrorLevelChannel, "599 -> 渠道级"},

		// 客户端错误
		{406, ErrorLevelClient, "406 -> 客户端级"},
		{413, ErrorLevelClient, "413 -> 客户端级"},

		// 默认行为
		{599, ErrorLevelChannel, "599 -> 渠道级(自定义)"},
		{418, ErrorLevelKey, "418 -> Key级(兜底策略:未知4xx)"},
		{511, ErrorLevelChannel, "511 -> 渠道级(默认5xx)"},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			meta := GetStatusCodeMeta(tt.status)
			if meta.Level != tt.wantLevel {
				t.Errorf("Level: got %v, want %v", meta.Level, tt.wantLevel)
			}
		})
	}
}

func TestClientStatusFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status   int
		expected int
		desc     string
	}{
		{-1, 502, "负值 -> 502"},
		{0, 502, "零值 -> 502"},
		{StatusQuotaExceeded, 429, "596 -> 429"},
		{StatusSSEError, 502, "597 -> 502"},
		{StatusFirstByteTimeout, 504, "598 -> 504"},
		{StatusStreamIncomplete, 502, "599 -> 502"},
		{429, 429, "429 -> 透传"},
		{401, 401, "401 -> 401 (透明代理)"},
		{405, 405, "405 -> 405 (透明代理)"},
		{404, 404, "404 -> 404 (透明代理)"},
		{499, 499, "499 -> 499（语义由上层决定）"},
		{502, 502, "502 -> 502"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := ClientStatusFor(tt.status); got != tt.expected {
				t.Fatalf("ClientStatusFor(%d)=%d, expected %d", tt.status, got, tt.expected)
			}
		})
	}
}

// IsRetryableStatus 已移除：重试决策不应依赖静态状态码表，而应依赖 errorLevel/shouldRetry 等语义信息。
