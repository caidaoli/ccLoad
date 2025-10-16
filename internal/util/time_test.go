package util

import (
	"testing"
	"time"
)

func TestCalculateBackoffDuration_504Error(t *testing.T) {
	now := time.Now()
	statusCode504 := 504

	tests := []struct {
		name           string
		prevMs         int64
		until          time.Time
		statusCode     *int
		expectedMin    time.Duration
		expectedMax    time.Duration
		description    string
	}{
		{
			name:           "首次504错误应冷却2分钟",
			prevMs:         0,
			until:          time.Time{},
			statusCode:     &statusCode504,
			expectedMin:    2 * time.Minute,
			expectedMax:    2 * time.Minute,
			description:    "504 Gateway Timeout should trigger 2-minute cooldown on first occurrence",
		},
		{
			name:           "连续504错误应指数退避",
			prevMs:         int64(2 * time.Minute / time.Millisecond),
			until:          now.Add(2 * time.Minute),
			statusCode:     &statusCode504,
			expectedMin:    4 * time.Minute,
			expectedMax:    4 * time.Minute,
			description:    "Subsequent 504 errors should double the cooldown (2min -> 4min)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			duration := CalculateBackoffDuration(tt.prevMs, tt.until, now, tt.statusCode)

			if duration < tt.expectedMin || duration > tt.expectedMax {
				t.Errorf("❌ %s\n期望冷却时间: %v-%v\n实际冷却时间: %v",
					tt.description, tt.expectedMin, tt.expectedMax, duration)
			} else {
				t.Logf("✅ %s\n冷却时间: %v", tt.description, duration)
			}
		})
	}
}

func TestCalculateBackoffDuration_ChannelErrors(t *testing.T) {
	now := time.Now()

	tests := []struct {
		statusCode int
		expected   time.Duration
	}{
		{500, 2 * time.Minute}, // Internal Server Error
		{502, 2 * time.Minute}, // Bad Gateway
		{503, 2 * time.Minute}, // Service Unavailable
		{504, 2 * time.Minute}, // Gateway Timeout
	}

	for _, tt := range tests {
		t.Run("StatusCode_"+string(rune(tt.statusCode)), func(t *testing.T) {
			duration := CalculateBackoffDuration(0, time.Time{}, now, &tt.statusCode)

			if duration != tt.expected {
				t.Errorf("❌ 状态码%d首次错误应冷却%v，实际%v",
					tt.statusCode, tt.expected, duration)
			} else {
				t.Logf("✅ 状态码%d首次错误正确冷却%v", tt.statusCode, duration)
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
				t.Errorf("❌ 认证错误%d首次应冷却%v，实际%v",
					tt.statusCode, tt.expected, duration)
			} else {
				t.Logf("✅ 认证错误%d首次正确冷却%v", tt.statusCode, duration)
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
		{429, 1 * time.Second}, // Too Many Requests
	}

	for _, tt := range tests {
		t.Run("StatusCode_"+string(rune(tt.statusCode)), func(t *testing.T) {
			duration := CalculateBackoffDuration(0, time.Time{}, now, &tt.statusCode)

			if duration != tt.expected {
				t.Errorf("❌ 状态码%d首次错误应冷却%v，实际%v",
					tt.statusCode, tt.expected, duration)
			} else {
				t.Logf("✅ 状态码%d首次错误正确冷却%v", tt.statusCode, duration)
			}
		})
	}
}

func TestCalculateBackoffDuration_TimeoutError(t *testing.T) {
	now := time.Now()
	statusCode598 := 598

	duration := CalculateBackoffDuration(0, time.Time{}, now, &statusCode598)

	if duration != TimeoutErrorCooldown {
		t.Errorf("❌ 超时错误(598)应固定冷却%v，实际%v",
			TimeoutErrorCooldown, duration)
	} else {
		t.Logf("✅ 超时错误(598)正确固定冷却%v", duration)
	}
}

func TestCalculateBackoffDuration_ExponentialBackoff(t *testing.T) {
	now := time.Now()
	statusCode := 429

	// 测试指数退避序列：1s -> 2s -> 4s -> 8s -> 16s -> 30min(上限)
	expectedSequence := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		64 * time.Second,
		128 * time.Second,
		256 * time.Second,
		512 * time.Second,
		1024 * time.Second,
		MaxCooldownDuration, // 应达到上限
	}

	prevMs := int64(0)
	for i, expected := range expectedSequence {
		duration := CalculateBackoffDuration(prevMs, time.Time{}, now, &statusCode)

		if i < len(expectedSequence)-1 {
			if duration != expected {
				t.Errorf("❌ 第%d次退避应为%v，实际%v", i+1, expected, duration)
			}
		} else {
			// 最后一次应触发上限
			if duration != MaxCooldownDuration {
				t.Errorf("❌ 指数退避应达到上限%v，实际%v", MaxCooldownDuration, duration)
			}
		}

		prevMs = int64(duration / time.Millisecond)
	}

	t.Logf("✅ 指数退避序列测试通过，最终达到上限%v", MaxCooldownDuration)
}
