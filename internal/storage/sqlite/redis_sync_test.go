package sqlite_test

import (
	"ccLoad/internal/model"
	"testing"
	"time"

	"github.com/bytedance/sonic"
)

// ==================== Redis序列化/反序列化集成测试 ====================

func TestRedisSync_Serialization(t *testing.T) {
	now := time.Now()

	// 创建测试Config对象（注：新架构中APIKey在api_keys表）
	configs := []*model.Config{
		{
			ID:          1,
			Name:        "test-anthropic",
			ChannelType: "anthropic",
			URL:         "https://api.anthropic.com",
			Priority:    10,
			ModelEntries: []model.ModelEntry{
				{Model: "claude-3-sonnet", RedirectModel: ""},
				{Model: "old", RedirectModel: "new"},
			},
			Enabled:   true,
			CreatedAt: model.JSONTime{Time: now},
			UpdatedAt: model.JSONTime{Time: now},
		},
		{
			ID:           2,
			Name:         "test-empty-defaults",
			ChannelType:  "", // 空值，GetChannelType()会返回默认值
			URL:          "https://api.example.com",
			Priority:     5,
			ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
			Enabled:      true,
			CreatedAt:    model.JSONTime{Time: now},
			UpdatedAt:    model.JSONTime{Time: now},
		},
	}

	// 步骤1：序列化到JSON（模拟Redis存储）
	data, err := sonic.Marshal(configs)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	// 验证序列化格式
	var jsonCheck []map[string]any
	if err := sonic.Unmarshal(data, &jsonCheck); err != nil {
		t.Fatalf("JSON格式验证失败: %v", err)
	}

	// 验证时间字段为Unix时间戳（整数）
	createdAtRaw := jsonCheck[0]["created_at"]
	var createdAtTS int64
	switch v := createdAtRaw.(type) {
	case float64:
		createdAtTS = int64(v)
	case int64:
		createdAtTS = v
	default:
		t.Errorf("created_at应为数字类型，实际为 %T", createdAtRaw)
	}

	if createdAtTS <= 0 {
		t.Errorf("created_at时间戳应为正数，实际为 %d", createdAtTS)
	}

	// 步骤2：反序列化（模拟从Redis恢复）
	var restored []*model.Config
	if err := sonic.Unmarshal(data, &restored); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	// 验证恢复数据的完整性
	if len(restored) != 2 {
		t.Fatalf("恢复数据数量错误：期望2，实际 %d", len(restored))
	}

	// 验证第一个Config
	if restored[0].ChannelType != "anthropic" {
		t.Errorf("Config[0] channel_type错误: %s", restored[0].ChannelType)
	}

	if !restored[0].CreatedAt.Time.Truncate(time.Second).Equal(now.Truncate(time.Second)) {
		t.Errorf("Config[0] 时间恢复错误:\n期望: %v\n实际: %v", now, restored[0].CreatedAt.Time)
	}

	// 验证第二个Config（使用GetChannelType获取默认值）
	if restored[1].GetChannelType() != "anthropic" {
		t.Errorf("Config[1] GetChannelType()应返回anthropic，实际为 %s", restored[1].GetChannelType())
	}

	// 验证第二个Config的ModelEntries只有一个模型且无重定向
	if len(restored[1].ModelEntries) != 1 || restored[1].ModelEntries[0].RedirectModel != "" {
		t.Errorf("Config[1] ModelEntries应有1个无重定向的模型，实际: %v", restored[1].ModelEntries)
	}
}

// ==================== Redis恢复时默认值填充测试 ====================

func TestRedisRestore_DefaultValuesFilling(t *testing.T) {
	// 模拟从Redis恢复的原始数据（新架构使用 models 数组）
	rawJSON := `[
		{
			"id": 1,
			"name": "test-redis",
			"channel_type": "",
			"url": "https://api.example.com",
			"priority": 10,
			"models": [{"model": "test-model", "redirect_model": ""}],
			"enabled": true,
			"created_at": 1759575045,
			"updated_at": 1759575045
		}
	]`

	// 反序列化
	var configs []*model.Config
	if err := sonic.Unmarshal([]byte(rawJSON), &configs); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	// 验证：GetChannelType()返回默认值（但不修改字段）
	if configs[0].GetChannelType() != "anthropic" {
		t.Errorf("GetChannelType()应返回anthropic，实际为 %s", configs[0].GetChannelType())
	}

	if configs[0].ChannelType != "" {
		t.Errorf("原始channel_type字段应保持为空，实际为 %s", configs[0].ChannelType)
	}

	// 模拟LoadChannelsFromRedis的填充逻辑
	channelType := configs[0].GetChannelType() // 获取默认值
	configs[0].ChannelType = channelType       // 强制赋值

	// 验证填充后的值
	if configs[0].ChannelType != "anthropic" {
		t.Errorf("填充后channel_type应为anthropic，实际为 %s", configs[0].ChannelType)
	}
}

// ==================== Benchmark 性能测试 ====================

func BenchmarkConfigSerialization(b *testing.B) {
	configs := make([]*model.Config, 100)
	for i := 0; i < 100; i++ {
		configs[i] = &model.Config{
			ID:           int64(i),
			Name:         "test",
			ChannelType:  "anthropic",
			URL:          "https://api.example.com",
			Priority:     10,
			ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			Enabled:      true,
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = sonic.Marshal(configs)
	}
}

func BenchmarkJSONTime_Marshal(b *testing.B) {
	jt := model.JSONTime{Time: time.Now()}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = sonic.Marshal(jt)
	}
}

func BenchmarkJSONTime_Unmarshal(b *testing.B) {
	data := []byte(`"2025-10-04T10:30:45+08:00"`)
	var jt model.JSONTime

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sonic.Unmarshal(data, &jt)
	}
}
