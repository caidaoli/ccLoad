package main

import (
	"fmt"
	"testing"
)

// ==================== 基础Token估算测试 ====================

func TestEstimateTextTokens_EnglishText(t *testing.T) {
	// 测试英文文本（4字符/token）
	tests := []struct {
		name     string
		text     string
		expected int
	}{
		{"空文本", "", 0},
		{"短句", "Hello, world", 3}, // 12字符 / 4 ≈ 3
		{"中等长度", "What is a quaternion?", 5}, // 21字符 / 4 ≈ 5
		{"长文本", "The Anthropic Go library provides convenient access to the Anthropic REST API", 19}, // 78字符 / 4 ≈ 19
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := estimateTextTokens(tt.text)
			// 允许±1 token误差
			if result < tt.expected-1 || result > tt.expected+1 {
				t.Errorf("estimateTextTokens(%q) = %d, 期望约 %d", tt.text, result, tt.expected)
			}
		})
	}
}

func TestEstimateTextTokens_ChineseText(t *testing.T) {
	// 测试中文文本（1.5字符/token）
	tests := []struct {
		name     string
		text     string
		minToken int
		maxToken int
	}{
		{"纯中文短句", "你好世界", 2, 4},           // 4字符 / 1.5 ≈ 3
		{"纯中文长句", "这是一个测试中文文本的例子", 8, 12}, // 15字符 / 1.5 ≈ 10
		{"混合语言", "Hello 世界", 2, 4},         // 8字符，约30%中文，≈2.5 tokens
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := estimateTextTokens(tt.text)
			if result < tt.minToken || result > tt.maxToken {
				t.Errorf("estimateTextTokens(%q) = %d, 期望 %d-%d", tt.text, result, tt.minToken, tt.maxToken)
			}
		})
	}
}

func TestEstimateContentBlock_TextBlock(t *testing.T) {
	// 文本块测试
	textBlock := map[string]any{
		"type": "text",
		"text": "Hello, world",
	}

	result := estimateContentBlock(textBlock)
	if result < 2 || result > 5 {
		t.Errorf("文本块token估算异常: %d", result)
	}
}

func TestEstimateContentBlock_ImageBlock(t *testing.T) {
	// 图片块测试（固定1500 tokens）
	imageBlock := map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": "image/png",
			"data":       "base64data...",
		},
	}

	result := estimateContentBlock(imageBlock)
	if result != 1500 {
		t.Errorf("图片块token估算错误: got %d, 期望 1500", result)
	}
}

func TestEstimateContentBlock_UnknownType(t *testing.T) {
	// 未知类型保守估算
	unknownBlock := map[string]any{
		"type": "unknown",
		"data": "some data",
	}

	result := estimateContentBlock(unknownBlock)
	if result < 5 || result > 20 {
		t.Errorf("未知块token估算异常: %d", result)
	}
}

// ==================== 模型验证测试 ====================

func TestIsValidClaudeModel(t *testing.T) {
	tests := []struct {
		model string
		valid bool
	}{
		// Claude模型
		{"claude-3-7-sonnet-20250219", true},
		{"claude-3-5-sonnet-20241022", true},
		{"claude-3-opus-20240229", true},
		{"claude-3-haiku-20240307", true},
		{"CLAUDE-3-SONNET-20240229", true}, // 大小写不敏感

		// OpenAI兼容模型（codex渠道）
		{"gpt-4", true},
		{"gpt-3.5-turbo", true},

		// Gemini兼容模型
		{"gemini-pro", true},
		{"gemini-flash", true},

		// Bedrock格式
		{"anthropic.claude-v2", true},

		// 无效模型
		{"", false},
		{"invalid-model", false},
		{"llama-3", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			result := isValidClaudeModel(tt.model)
			if result != tt.valid {
				t.Errorf("isValidClaudeModel(%q) = %v, 期望 %v", tt.model, result, tt.valid)
			}
		})
	}
}

// ==================== 完整请求估算测试 ====================

func TestEstimateTokens_SimpleMessage(t *testing.T) {
	// 简单文本消息
	req := &CountTokensRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: "Hello, world",
			},
		},
	}

	tokens := estimateTokens(req)

	// 期望: 消息内容(3) + 角色开销(10) + 基础开销(10) ≈ 23
	if tokens < 20 || tokens > 30 {
		t.Errorf("简单消息token估算异常: %d, 期望 20-30", tokens)
	}
}

func TestEstimateTokens_WithSystemPrompt(t *testing.T) {
	// 包含系统提示词
	req := &CountTokensRequest{
		Model:  "claude-3-5-sonnet-20241022",
		System: "You are a helpful assistant.",
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: "What is AI?",
			},
		},
	}

	tokens := estimateTokens(req)

	// 期望: system(7) + system开销(5) + 消息(3) + 角色(10) + 基础(10) ≈ 35
	if tokens < 30 || tokens > 45 {
		t.Errorf("带系统提示的消息token估算异常: %d, 期望 30-45", tokens)
	}
}

func TestEstimateTokens_WithSystemPromptArray(t *testing.T) {
	// 测试Beta版本的system数组格式
	req := &CountTokensRequest{
		Model: "claude-3-5-sonnet-20241022",
		System: []any{
			map[string]any{
				"type": "text",
				"text": "You are a scientist",
			},
			map[string]any{
				"type": "text",
				"text": "with expertise in quantum mechanics.",
			},
		},
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: "Explain quantum entanglement.",
			},
		},
	}

	tokens := estimateTokens(req)

	// 期望:
	// - system文本块1: "You are a scientist" ≈ 5 tokens
	// - system文本块2: "with expertise in quantum mechanics." ≈ 8 tokens
	// - system开销: 5 tokens
	// - 消息内容: "Explain quantum entanglement." ≈ 7 tokens
	// - 角色开销: 10 tokens
	// - 基础开销: 10 tokens
	// 总计: 5 + 8 + 5 + 7 + 10 + 10 ≈ 45 tokens
	// 允许 ±10 误差
	if tokens < 35 || tokens > 55 {
		t.Errorf("System数组格式token估算异常: %d, 期望 35-55", tokens)
	}

	t.Logf("System数组估算结果: %d tokens", tokens)
}

func TestEstimateTokens_SystemPromptNil(t *testing.T) {
	// 测试system为nil的情况
	req := &CountTokensRequest{
		Model:  "claude-3-5-sonnet-20241022",
		System: nil,
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: "Hello",
			},
		},
	}

	tokens := estimateTokens(req)

	// 期望: 消息(2) + 角色(10) + 基础(10) ≈ 22
	if tokens < 18 || tokens > 28 {
		t.Errorf("nil system token估算异常: %d, 期望 18-28", tokens)
	}
}

func TestEstimateTokens_SystemPromptEmptyString(t *testing.T) {
	// 测试system为空字符串的情况
	req := &CountTokensRequest{
		Model:  "claude-3-5-sonnet-20241022",
		System: "",
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: "Hello",
			},
		},
	}

	tokens := estimateTokens(req)

	// 期望: 消息(2) + 角色(10) + 基础(10) ≈ 22（空字符串不计入）
	if tokens < 18 || tokens > 28 {
		t.Errorf("空system token估算异常: %d, 期望 18-28", tokens)
	}
}

func TestEstimateTokens_WithTools(t *testing.T) {
	// 包含工具定义
	req := &CountTokensRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: "Get weather",
			},
		},
		Tools: []Tool{
			{
				Name:        "get_weather",
				Description: "Get current weather in a location",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]any{
							"type":        "string",
							"description": "City name",
						},
					},
					"required": []string{"location"},
				},
			},
		},
	}

	tokens := estimateTokens(req)

	// 期望: 消息(3+10) + 工具基础开销(400) + 工具名称(~7) + 工具描述(~8) + schema(~100) + 基础(10) ≈ 520-550
	// 实测：约536 tokens
	if tokens < 500 || tokens > 580 {
		t.Errorf("带工具的消息token估算异常: %d, 期望 500-580", tokens)
	}
}

func TestEstimateTokens_ComplexContentBlocks(t *testing.T) {
	// 复杂内容块（文本+图片）
	req := &CountTokensRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []MessageParam{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type": "text",
						"text": "What's in this image?",
					},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "...",
						},
					},
				},
			},
		},
	}

	tokens := estimateTokens(req)

	// 期望: 文本(5) + 图片(1500) + 角色(10) + 基础(10) ≈ 1525
	if tokens < 1500 || tokens > 1550 {
		t.Errorf("复杂内容块token估算异常: %d, 期望 1500-1550", tokens)
	}
}

func TestEstimateTokens_MultipleMessages(t *testing.T) {
	// 多轮对话
	req := &CountTokensRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: "What is AI?",
			},
			{
				Role:    "assistant",
				Content: "AI stands for Artificial Intelligence.",
			},
			{
				Role:    "user",
				Content: "Can you explain more?",
			},
		},
	}

	tokens := estimateTokens(req)

	// 期望: 3条消息 * (内容5-10 + 角色10) + 基础10 ≈ 55-100
	if tokens < 50 || tokens > 110 {
		t.Errorf("多轮对话token估算异常: %d, 期望 50-110", tokens)
	}
}

// ==================== 边界条件测试 ====================

func TestEstimateTokens_EmptyMessages(t *testing.T) {
	// 空消息数组（应该只有基础开销）
	req := &CountTokensRequest{
		Model:    "claude-3-5-sonnet-20241022",
		Messages: []MessageParam{},
	}

	tokens := estimateTokens(req)

	// 期望: 只有基础开销10 tokens
	if tokens < 5 || tokens > 15 {
		t.Errorf("空消息token估算异常: %d, 期望 5-15", tokens)
	}
}

func TestEstimateTokens_LongText(t *testing.T) {
	// 长文本（1000字符）
	longText := ""
	for i := 0; i < 250; i++ {
		longText += "test "
	}

	req := &CountTokensRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: longText,
			},
		},
	}

	tokens := estimateTokens(req)

	// 期望: 1250字符 / 4 ≈ 312 + 开销20 ≈ 332
	if tokens < 300 || tokens > 350 {
		t.Errorf("长文本token估算异常: %d, 期望 300-350", tokens)
	}
}

func TestEstimateTokens_MixedLanguageLongText(t *testing.T) {
	// 混合语言长文本
	mixedText := "Hello 你好 World 世界 " // 重复250次
	longMixedText := ""
	for i := 0; i < 250; i++ {
		longMixedText += mixedText
	}

	req := &CountTokensRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: longMixedText,
			},
		},
	}

	tokens := estimateTokens(req)

	// 混合语言应该在英文和中文估算之间
	// 总长度：21字符 * 250 = 5250字符（包含空格）
	// 中文比例约40%，charsPerToken ≈ 3.0
	// 期望约 5250 / 3.0 ≈ 1750 + 开销20 ≈ 1300-1400
	if tokens < 1200 || tokens > 1500 {
		t.Errorf("混合语言长文本token估算异常: %d, 期望 1200-1500", tokens)
	}
}

func TestEstimateTokens_ManyTools(t *testing.T) {
	// 测试大量工具场景（10个工具）的自适应开销
	tools := make([]Tool, 10)
	for i := 0; i < 10; i++ {
		tools[i] = Tool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: "This is a test tool for validating token estimation",
			InputSchema: map[string]any{
				"$schema": "http://json-schema.org/draft-07/schema#",
				"type":    "object",
				"properties": map[string]any{
					"param": map[string]any{
						"type":        "string",
						"description": "A parameter",
					},
				},
				"required":             []string{"param"},
				"additionalProperties": false,
			},
		}
	}

	req := &CountTokensRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: "Test message",
			},
		},
		Tools: tools,
	}

	tokens := estimateTokens(req)

	// 大量工具场景（>5）：
	// 基础开销: 250
	// 10个工具 × 80增量 = 800
	// 每个工具内容: name(~5) + desc(~12) + schema(~100) ≈ 117
	// 工具总计: 250 + 800 + 10×117 = 2220
	// 消息: ~15
	// 总计: 约2250
	// 允许 ±15% 误差范围（1900-2600）
	if tokens < 1900 || tokens > 2600 {
		t.Errorf("大量工具场景token估算异常: %d, 期望 1900-2600", tokens)
	}

	t.Logf("10个工具的估算结果: %d tokens", tokens)
}

// ==================== 性能基准测试 ====================

func BenchmarkEstimateTextTokens_English(b *testing.B) {
	text := "The Anthropic Go library provides convenient access to the Anthropic REST API from applications written in Go."

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		estimateTextTokens(text)
	}
}

func BenchmarkEstimateTextTokens_Chinese(b *testing.B) {
	text := "这是一个用于测试中文token估算性能的基准测试文本，包含了多个中文字符。"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		estimateTextTokens(text)
	}
}

func BenchmarkEstimateTokens_ComplexRequest(b *testing.B) {
	req := &CountTokensRequest{
		Model:  "claude-3-5-sonnet-20241022",
		System: "You are a helpful assistant with expertise in multiple domains.",
		Messages: []MessageParam{
			{
				Role:    "user",
				Content: "What is quantum computing and how does it differ from classical computing?",
			},
			{
				Role:    "assistant",
				Content: "Quantum computing leverages quantum mechanics principles...",
			},
			{
				Role:    "user",
				Content: "Can you give me a practical example?",
			},
		},
		Tools: []Tool{
			{
				Name:        "search",
				Description: "Search for information",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]string{"type": "string"},
					},
				},
			},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		estimateTokens(req)
	}
}
