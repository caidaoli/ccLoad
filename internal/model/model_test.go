package model

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
		ID:          1,
		Name:        "test-channel",
		ChannelType: "gemini",
		URL:         "https://api.example.com",
		Priority:    10,
		ModelEntries: []ModelEntry{
			{Model: "model-1", RedirectModel: ""},
			{Model: "model-2", RedirectModel: "model-2-new"},
		},
		Enabled:   true,
		CreatedAt: JSONTime{Time: now},
		UpdatedAt: JSONTime{Time: now},
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

	if restored.Name != "test-channel" {
		t.Errorf("name不匹配: 期望 test-channel, 实际 %s", restored.Name)
	}

	if len(restored.ModelEntries) != 2 {
		t.Errorf("model_entries数量不匹配: 期望 2, 实际 %d", len(restored.ModelEntries))
	}

	// 验证 GetModels() 返回正确的模型列表
	models := restored.GetModels()
	if len(models) != 2 {
		t.Errorf("GetModels()数量不匹配: 期望 2, 实际 %d", len(models))
	}

	// 验证 GetRedirectModel() 返回正确的重定向
	if redirect, ok := restored.GetRedirectModel("model-2"); !ok || redirect != "model-2-new" {
		t.Errorf("GetRedirectModel()不匹配: 期望 (model-2-new, true), 实际 (%s, %v)", redirect, ok)
	}

	// 时间比较：允许1秒误差（JSON序列化精度损失）
	if !restored.CreatedAt.Time.Truncate(time.Second).Equal(now.Truncate(time.Second)) {
		t.Errorf("created_at时间不匹配:\n期望: %v\n实际: %v", now, restored.CreatedAt.Time)
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
