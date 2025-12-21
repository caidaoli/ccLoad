package util

import (
	"testing"
	"time"
)

func TestParseResetTimeFrom1308Error(t *testing.T) {
	tests := []struct {
		name          string
		responseBody  string
		expectSuccess bool
		expectTime    string // 格式: "2006-01-02 15:04:05"
	}{
		{
			name: "标准1308错误",
			responseBody: `{"type":"error","error":{"type":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2025-12-09 18:08:11 重置。"},"request_id":"20251209155304a15e2cfd9ae44ae8"}`,
			expectSuccess: true,
			expectTime:    "2025-12-09 18:08:11",
		},
		{
			name: "非1308错误",
			responseBody: `{"type":"error","error":{"type":"1307","message":"其他错误"},"request_id":"xxx"}`,
			expectSuccess: false,
		},
		{
			name: "格式错误的JSON",
			responseBody: `{invalid json}`,
			expectSuccess: false,
		},
		{
			name: "缺少时间信息",
			responseBody: `{"type":"error","error":{"type":"1308","message":"错误信息但没有时间"},"request_id":"xxx"}`,
			expectSuccess: false,
		},
		{
			name: "时间格式错误",
			responseBody: `{"type":"error","error":{"type":"1308","message":"您的限额将在 2025/12/09 重置。"},"request_id":"xxx"}`,
			expectSuccess: false,
		},
		{
			name: "使用code字段的1308错误（非Anthropic格式）",
			responseBody: `{"error":{"code":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2025-12-21 15:00:05 重置。"},"request_id":"202512211335142b05cc4f9bbb4e6c"}`,
			expectSuccess: true,
			expectTime:    "2025-12-21 15:00:05",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetTime, ok := ParseResetTimeFrom1308Error([]byte(tt.responseBody))

			if ok != tt.expectSuccess {
				t.Errorf("ParseResetTimeFrom1308Error() ok = %v, want %v", ok, tt.expectSuccess)
				return
			}

			if tt.expectSuccess {
				expectedTime, err := time.ParseInLocation("2006-01-02 15:04:05", tt.expectTime, time.Local)
				if err != nil {
					t.Fatalf("测试用例时间格式错误: %v", err)
				}

				if !resetTime.Equal(expectedTime) {
					t.Errorf("ParseResetTimeFrom1308Error() 解析的时间 = %v, 期望 = %v",
						resetTime.Format("2006-01-02 15:04:05"),
						expectedTime.Format("2006-01-02 15:04:05"))
				}
			}
		})
	}
}

// 测试时区处理
func TestParseResetTimeFrom1308Error_Timezone(t *testing.T) {
	responseBody := `{"type":"error","error":{"type":"1308","message":"您的限额将在 2025-12-09 18:08:11 重置。"},"request_id":"xxx"}`

	resetTime, ok := ParseResetTimeFrom1308Error([]byte(responseBody))
	if !ok {
		t.Fatal("解析失败")
	}

	// 验证使用的是本地时区
	if resetTime.Location() != time.Local {
		t.Errorf("时区不是Local: %v", resetTime.Location())
	}
}

// 测试边界情况: message中包含多个"将在"
func TestParseResetTimeFrom1308Error_MultipleOccurrences(t *testing.T) {
	responseBody := `{"type":"error","error":{"type":"1308","message":"您之前将在某时，现在的限额将在 2025-12-09 18:08:11 重置。"},"request_id":"xxx"}`

	resetTime, ok := ParseResetTimeFrom1308Error([]byte(responseBody))
	if !ok {
		t.Fatal("解析失败")
	}

	// 应该匹配第一个"将在"
	expectedTime, _ := time.ParseInLocation("2006-01-02 15:04:05", "2025-12-09 18:08:11", time.Local)
	if !resetTime.Equal(expectedTime) {
		t.Errorf("时间匹配错误: got %v, want %v", resetTime, expectedTime)
	}
}
