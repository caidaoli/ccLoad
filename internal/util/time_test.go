package util

import (
	"testing"
	"time"
)

func TestCalculateBackoffDuration_ChannelErrors(t *testing.T) {
	now := time.Now()

	tests := []struct {
		statusCode int
		expected   time.Duration
	}{
		{500, 2 * time.Minute}, // Internal Server Error: 2min -> 4min -> 8min ...
		{502, 2 * time.Minute}, // Bad Gateway: 2min -> 4min -> 8min ...
		{503, 2 * time.Minute}, // Service Unavailable: 2min -> 4min -> 8min ...
		{504, 2 * time.Minute}, // Gateway Timeout: 2min -> 4min -> 8min ...
		{520, 2 * time.Minute}, // Web Server Returned an Unknown Error: 2min -> 4min -> 8min ...
		{521, 2 * time.Minute}, // Web Server Is Down: 2min -> 4min -> 8min ...
		{524, 2 * time.Minute}, // A Timeout Occurred: 2min -> 4min -> 8min ...
		{599, 2 * time.Minute}, // Stream Incomplete (内部状态码): 2min -> 4min -> 8min ...
	}

	for _, tt := range tests {
		t.Run("StatusCode_"+string(rune(tt.statusCode)), func(t *testing.T) {
			duration := CalculateBackoffDuration(0, time.Time{}, now, &tt.statusCode)

			if duration != tt.expected {
				t.Errorf("状态码%d首次错误应冷却%v，实际%v",
					tt.statusCode, tt.expected, duration)
			}
		})
	}
}

func TestCalculateBackoffDuration_AuthErrors(t *testing.T) {
	now := time.Now()

	tests := []struct {
		statusCode int
		expected   time.Duration
	}{
		{401, 5 * time.Minute}, // Unauthorized
		{402, 5 * time.Minute}, // Payment Required
		{403, 5 * time.Minute}, // Forbidden
	}

	for _, tt := range tests {
		t.Run("StatusCode_"+string(rune(tt.statusCode)), func(t *testing.T) {
			duration := CalculateBackoffDuration(0, time.Time{}, now, &tt.statusCode)

			if duration != tt.expected {
				t.Errorf("认证错误%d首次应冷却%v，实际%v",
					tt.statusCode, tt.expected, duration)
			}
		})
	}
}

func TestCalculateBackoffDuration_OtherErrors(t *testing.T) {
	now := time.Now()

	tests := []struct {
		statusCode int
		expected   time.Duration
	}{
		{429, time.Minute}, // Too Many Requests - 1分钟冷却
	}

	for _, tt := range tests {
		t.Run("StatusCode_"+string(rune(tt.statusCode)), func(t *testing.T) {
			duration := CalculateBackoffDuration(0, time.Time{}, now, &tt.statusCode)

			if duration != tt.expected {
				t.Errorf("状态码%d首次错误应冷却%v，实际%v",
					tt.statusCode, tt.expected, duration)
			}
		})
	}
}

func TestCalculateBackoffDuration_TimeoutError(t *testing.T) {
	now := time.Now()
	statusCode598 := 598

	duration := CalculateBackoffDuration(0, time.Time{}, now, &statusCode598)

	if duration != TimeoutErrorCooldown {
		t.Errorf("超时错误(598)应固定冷却%v，实际%v",
			TimeoutErrorCooldown, duration)
	}
}

func TestCalculateBackoffDuration_ExponentialBackoff(t *testing.T) {
	now := time.Now()
	statusCode := 500 // 使用服务器错误测试指数退避（2分钟起始）

	// 测试指数退避序列：2min -> 4min -> 8min -> 16min -> 30min(上限)
	expectedSequence := []time.Duration{
		2 * time.Minute,  // 初始
		4 * time.Minute,  // 2x
		8 * time.Minute,  // 4x
		16 * time.Minute, // 8x
		30 * time.Minute, // 达到上限
		30 * time.Minute, // 保持上限
	}

	prevMs := int64(0)
	for i, expected := range expectedSequence {
		duration := CalculateBackoffDuration(prevMs, time.Time{}, now, &statusCode)

		if duration != expected {
			t.Errorf("第%d次退避应为%v，实际%v", i+1, expected, duration)
		}

		prevMs = int64(duration / time.Millisecond)
	}
}
