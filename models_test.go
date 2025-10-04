package main

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// ==================== JSONTime 序列化测试 ====================

func TestJSONTime_MarshalJSON(t *testing.T) {
	testTime := time.Date(2025, 10, 4, 10, 30, 45, 0, time.UTC)
	testTimestamp := testTime.Unix()

	tests := []struct {
		name     string
		time     time.Time
		expected string
	}{
		{
			name:     "标准时间序列化为Unix时间戳",
			time:     testTime,
			expected: strconv.FormatInt(testTimestamp, 10),
		},
		{
			name:     "带时区时间序列化为Unix时间戳",
			time:     time.Date(2025, 10, 4, 18, 30, 45, 0, time.FixedZone("CST", 8*3600)),
			expected: strconv.FormatInt(testTimestamp, 10), // CST 18:30 = UTC 10:30
		},
		{
			name:     "零时间序列化为0",
			time:     time.Time{},
			expected: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jt := JSONTime{Time: tt.time}
			data, err := json.Marshal(jt)
			if err != nil {
				t.Fatalf("序列化失败: %v", err)
			}

			if string(data) != tt.expected {
				t.Errorf("序列化结果不匹配:\n期望: %s\n实际: %s", tt.expected, string(data))
			}
		})
	}
}

func TestJSONTime_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected time.Time
		wantErr  bool
	}{
		{
			name:     "Unix时间戳反序列化",
			input:    `1759575045`,
			expected: time.Unix(1759575045, 0),
			wantErr:  false,
		},
		{
			name:     "Unix时间戳字符串反序列化",
			input:    `"1759575045"`, // 带引号的字符串（兼容性测试）
			expected: time.Unix(1759575045, 0),
			wantErr:  true, // 新实现不支持字符串格式，应该报错
		},
		{
			name:     "null值处理",
			input:    `null`,
			expected: time.Time{},
			wantErr:  false,
		},
		{
			name:     "零值处理",
			input:    `0`,
			expected: time.Time{},
			wantErr:  false,
		},
		{
			name:     "无效格式返回错误",
			input:    `"invalid-time"`,
			expected: time.Time{},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var jt JSONTime
			err := json.Unmarshal([]byte(tt.input), &jt)

			if tt.wantErr {
				if err == nil {
					t.Errorf("期望错误但成功反序列化")
				}
				return
			}

			if err != nil {
				t.Fatalf("反序列化失败: %v", err)
			}

			if !jt.Time.Equal(tt.expected) {
				t.Errorf("时间不匹配:\n期望: %v\n实际: %v", tt.expected, jt.Time)
			}
		})
	}
}

// ==================== Config 序列化完整性测试 ====================

func TestConfig_JSONSerialization(t *testing.T) {
	now := time.Now()
	config := &Config{
		ID:             1,
		Name:           "test-channel",
		ChannelType:    "gemini",
		KeyStrategy:    "round_robin",
		APIKey:         "test-key",
		APIKeys:        []string{"key1", "key2"},
		URL:            "https://api.example.com",
		Priority:       10,
		Models:         []string{"model-1", "model-2"},
		ModelRedirects: map[string]string{"old": "new"},
		Enabled:        true,
		CreatedAt:      JSONTime{Time: now},
		UpdatedAt:      JSONTime{Time: now},
	}

	// 序列化
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	// 反序列化
	var restored Config
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	// 验证关键字段
	if restored.ChannelType != "gemini" {
		t.Errorf("channel_type不匹配: 期望 gemini, 实际 %s", restored.ChannelType)
	}

	if restored.KeyStrategy != "round_robin" {
		t.Errorf("key_strategy不匹配: 期望 round_robin, 实际 %s", restored.KeyStrategy)
	}

	// 时间比较：允许1秒误差（JSON序列化精度损失）
	if !restored.CreatedAt.Time.Truncate(time.Second).Equal(now.Truncate(time.Second)) {
		t.Errorf("created_at时间不匹配:\n期望: %v\n实际: %v", now, restored.CreatedAt.Time)
	}

	if len(restored.APIKeys) != 2 {
		t.Errorf("api_keys数量不匹配: 期望 2, 实际 %d", len(restored.APIKeys))
	}
}

// ==================== GetChannelType 默认值测试 ====================

func TestConfig_GetChannelType(t *testing.T) {
	tests := []struct {
		name        string
		channelType string
		expected    string
	}{
		{
			name:        "非空值原样返回",
			channelType: "gemini",
			expected:    "gemini",
		},
		{
			name:        "空字符串返回默认值",
			channelType: "",
			expected:    "anthropic",
		},
		{
			name:        "codex值正确返回",
			channelType: "codex",
			expected:    "codex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{ChannelType: tt.channelType}
			result := config.GetChannelType()

			if result != tt.expected {
				t.Errorf("GetChannelType()结果不匹配:\n期望: %s\n实际: %s", tt.expected, result)
			}
		})
	}
}

// ==================== GetKeyStrategy 默认值测试 ====================

func TestConfig_GetKeyStrategy(t *testing.T) {
	tests := []struct {
		name        string
		keyStrategy string
		expected    string
	}{
		{
			name:        "非空值原样返回",
			keyStrategy: "round_robin",
			expected:    "round_robin",
		},
		{
			name:        "空字符串返回默认值",
			keyStrategy: "",
			expected:    "sequential",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{KeyStrategy: tt.keyStrategy}
			result := config.GetKeyStrategy()

			if result != tt.expected {
				t.Errorf("GetKeyStrategy()结果不匹配:\n期望: %s\n实际: %s", tt.expected, result)
			}
		})
	}
}

// ==================== normalizeConfigDefaults 测试 ====================

func TestNormalizeConfigDefaults(t *testing.T) {
	configs := []*Config{
		{
			ID:             1,
			Name:           "empty-defaults",
			ChannelType:    "",
			KeyStrategy:    "",
			ModelRedirects: nil,
			APIKeys:        nil,
		},
		{
			ID:             2,
			Name:           "with-values",
			ChannelType:    "gemini",
			KeyStrategy:    "round_robin",
			ModelRedirects: map[string]string{"test": "value"},
			APIKeys:        []string{"key1"},
		},
	}

	normalizeConfigDefaults(configs)

	// 验证第一个Config（空默认值）
	if configs[0].ChannelType != "anthropic" {
		t.Errorf("空channel_type未填充默认值: %s", configs[0].ChannelType)
	}

	if configs[0].KeyStrategy != "sequential" {
		t.Errorf("空key_strategy未填充默认值: %s", configs[0].KeyStrategy)
	}

	if configs[0].ModelRedirects == nil {
		t.Errorf("nil ModelRedirects未初始化为空map")
	}

	if configs[0].APIKeys == nil {
		t.Errorf("nil APIKeys未初始化为空slice")
	}

	// 验证第二个Config（已有值不被覆盖）
	if configs[1].ChannelType != "gemini" {
		t.Errorf("非空channel_type被错误覆盖: %s", configs[1].ChannelType)
	}

	if configs[1].KeyStrategy != "round_robin" {
		t.Errorf("非空key_strategy被错误覆盖: %s", configs[1].KeyStrategy)
	}
}

// ==================== 边界条件测试 ====================

func TestNormalizeConfigDefaults_EmptySlice(t *testing.T) {
	var configs []*Config
	// 不应崩溃
	normalizeConfigDefaults(configs)
}

func TestNormalizeConfigDefaults_NilConfig(t *testing.T) {
	configs := []*Config{nil}

	// 验证nil config会导致panic（当前实现不处理nil）
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("期望panic但未发生")
		}
	}()

	normalizeConfigDefaults(configs)
}
