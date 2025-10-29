package util

import (
	"context"
	"testing"
)

func TestClassifyHTTPStatusWithBody(t *testing.T) {
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
			result := ClassifyHTTPStatusWithBody(tt.statusCode, tt.responseBody)
			if result != tt.expected {
				t.Errorf("❌ %s\n  期望: %v\n  实际: %v\n  原因: %s\n  响应体: %s",
					tt.name, tt.expected, result, tt.reason, string(tt.responseBody))
			} else {
				t.Logf("✅ %s - %s", tt.name, tt.reason)
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
		{400, ErrorLevelKey, "Key级错误"},
		{402, ErrorLevelKey, "402 应视为Key级（额度/余额/配额）"},
		{401, ErrorLevelKey, "Key级错误（默认）"},
		{403, ErrorLevelKey, "Key级错误"},
		{404, ErrorLevelClient, "客户端错误"},
		{429, ErrorLevelKey, "Key级限流"},
		// ✅ P3修复（2025-10-28）：499 HTTP响应应触发渠道级重试
		{499, ErrorLevelChannel, "499来自HTTP响应时，说明上游API返回，应重试其他渠道"},
		{500, ErrorLevelChannel, "渠道级错误"},
		{502, ErrorLevelChannel, "渠道级错误"},
		{503, ErrorLevelChannel, "渠道级错误"},
		{504, ErrorLevelChannel, "渠道级错误"},
		{520, ErrorLevelChannel, "520 Web Server Returned an Unknown Error - 渠道级错误"},
		{521, ErrorLevelChannel, "521 Web Server Is Down - 渠道级错误"},
		{524, ErrorLevelChannel, "524 A Timeout Occurred - 渠道级错误"},
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

// ✅ P3修复（2025-10-28）：测试context.Canceled与HTTP 499的区分
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
			statusCode, errorLevel, shouldRetry := ClassifyError(tt.err)

			if statusCode != tt.expectedStatus {
				t.Errorf("❌ 状态码错误: 期望 %d, 实际 %d (%s)", tt.expectedStatus, statusCode, tt.reason)
			}
			if errorLevel != tt.expectedLevel {
				t.Errorf("❌ 错误级别错误: 期望 %v, 实际 %v (%s)", tt.expectedLevel, errorLevel, tt.reason)
			}
			if shouldRetry != tt.expectedRetry {
				t.Errorf("❌ 重试标志错误: 期望 %v, 实际 %v (%s)", tt.expectedRetry, shouldRetry, tt.reason)
			}

			t.Logf("✅ %s - 状态码:%d, 错误级别:%v, 重试:%v", tt.reason, statusCode, errorLevel, shouldRetry)
		})
	}
}

// ✅ P1改进（2025-10-29）：测试429错误的智能分类
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
				"Retry-After":       {"30"},  // Key级指示器
				"X-Ratelimit-Scope": {"ip"},  // 渠道级指示器
			},
			responseBody: []byte(`{"error":"rate limit"}`),
			expected:     ErrorLevelChannel,
			reason:       "Retry-After检查优先于Scope，但>60秒判断优先",
		},
		{
			name: "combined_scope_and_body",
			headers: map[string][]string{
				"X-Ratelimit-Scope": {"user"},  // Key级指示器
			},
			responseBody: []byte(`{"error":"IP rate limit exceeded"}`),  // 渠道级指示器
			expected:     ErrorLevelChannel,
			reason:       "响应体中的渠道级指示器应被识别",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ClassifyRateLimitError(tt.headers, tt.responseBody)
			if result != tt.expected {
				t.Errorf("❌ %s\n  期望: %v\n  实际: %v\n  原因: %s",
					tt.name, tt.expected, result, tt.reason)
			} else {
				t.Logf("✅ %s - %s", tt.name, tt.reason)
			}
		})
	}
}
