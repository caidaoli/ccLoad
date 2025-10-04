package main

import "testing"

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
			result := classifyHTTPStatusWithBody(tt.statusCode, tt.responseBody)
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
		{401, ErrorLevelKey, "Key级错误（默认）"},
		{403, ErrorLevelKey, "Key级错误"},
		{404, ErrorLevelClient, "客户端错误"},
		{429, ErrorLevelKey, "Key级限流"},
		{500, ErrorLevelChannel, "渠道级错误"},
		{502, ErrorLevelChannel, "渠道级错误"},
		{503, ErrorLevelChannel, "渠道级错误"},
		{504, ErrorLevelChannel, "渠道级错误"},
	}

	for _, tt := range tests {
		t.Run(string(rune(tt.statusCode)), func(t *testing.T) {
			result := classifyHTTPStatus(tt.statusCode)
			if result != tt.expected {
				t.Errorf("状态码 %d: 期望 %v, 实际 %v (%s)", tt.statusCode, tt.expected, result, tt.reason)
			}
		})
	}
}
