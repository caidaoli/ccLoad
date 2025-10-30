package util

import (
	"testing"
	"time"
)

// TestCalculateCooldownDuration 测试冷却持续时间计算
func TestCalculateCooldownDuration(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		until    time.Time
		now      time.Time
		expected int64
	}{
		{
			name:     "正常冷却(60秒)",
			until:    now.Add(60 * time.Second),
			now:      now,
			expected: 60000, // 60秒 = 60000毫秒
		},
		{
			name:     "已过期冷却",
			until:    now.Add(-10 * time.Second),
			now:      now,
			expected: 0,
		},
		{
			name:     "零时间",
			until:    time.Time{},
			now:      now,
			expected: 0,
		},
		{
			name:     "1分钟冷却",
			until:    now.Add(1 * time.Minute),
			now:      now,
			expected: 60000,
		},
		{
			name:     "30分钟冷却",
			until:    now.Add(30 * time.Minute),
			now:      now,
			expected: 1800000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculateCooldownDuration(tt.until, tt.now)

			// 允许小幅误差(±100毫秒)
			diff := result - tt.expected
			if diff < 0 {
				diff = -diff
			}
			if diff > 100 {
				t.Errorf("期望 %d 毫秒, 实际 %d 毫秒", tt.expected, result)
			}
		})
	}
}

// TestCalculateCooldownDuration_Precision 测试精度
func TestCalculateCooldownDuration_Precision(t *testing.T) {
	now := time.Now()

	// 测试毫秒级精度
	until := now.Add(1234 * time.Millisecond)
	result := CalculateCooldownDuration(until, now)

	expected := int64(1234)
	diff := result - expected
	if diff < 0 {
		diff = -diff
	}

	// 允许±1毫秒误差
	if diff > 1 {
		t.Errorf("精度测试失败: 期望 %d ms, 实际 %d ms", expected, result)
	}
}
