package util

import (
	"encoding/json"
	"testing"
)

// TestOpenAIChatCompletionsTokenParsing 测试OpenAI Chat Completions API的token统计字段解析
func TestOpenAIChatCompletionsTokenParsing(t *testing.T) {
	// 模拟OpenAI Chat Completions API响应（使用prompt_tokens/completion_tokens）
	responseJSON := `{
		"id": "chatcmpl-123",
		"object": "chat.completion",
		"created": 1677652288,
		"model": "gpt-4o",
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50,
			"total_tokens": 150
		}
	}`

	var response map[string]any
	if err := json.Unmarshal([]byte(responseJSON), &response); err != nil {
		t.Fatalf("JSON解析失败: %v", err)
	}

	usage, ok := response["usage"].(map[string]any)
	if !ok {
		t.Fatal("无法提取usage字段")
	}

	// 验证字段存在
	if _, ok := usage["prompt_tokens"]; !ok {
		t.Error("缺少prompt_tokens字段")
	}
	if _, ok := usage["completion_tokens"]; !ok {
		t.Error("缺少completion_tokens字段")
	}

	// 验证值
	if int(usage["prompt_tokens"].(float64)) != 100 {
		t.Errorf("prompt_tokens = %v, 期望 100", usage["prompt_tokens"])
	}
	if int(usage["completion_tokens"].(float64)) != 50 {
		t.Errorf("completion_tokens = %v, 期望 50", usage["completion_tokens"])
	}
}

// TestOpenAIChatCompletionsWithCacheTokenParsing 测试带缓存的token统计
func TestOpenAIChatCompletionsWithCacheTokenParsing(t *testing.T) {
	// 模拟带prompt caching的OpenAI响应
	responseJSON := `{
		"id": "chatcmpl-456",
		"object": "chat.completion",
		"created": 1677652288,
		"model": "gpt-4o",
		"usage": {
			"prompt_tokens": 200,
			"completion_tokens": 75,
			"total_tokens": 275,
			"prompt_tokens_details": {
				"cached_tokens": 150,
				"audio_tokens": 0
			},
			"completion_tokens_details": {
				"reasoning_tokens": 0,
				"audio_tokens": 0
			}
		}
	}`

	var response map[string]any
	if err := json.Unmarshal([]byte(responseJSON), &response); err != nil {
		t.Fatalf("JSON解析失败: %v", err)
	}

	usage, ok := response["usage"].(map[string]any)
	if !ok {
		t.Fatal("无法提取usage字段")
	}

	// 验证基础字段
	if int(usage["prompt_tokens"].(float64)) != 200 {
		t.Errorf("prompt_tokens = %v, 期望 200", usage["prompt_tokens"])
	}

	// 验证缓存字段
	details, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatal("无法提取prompt_tokens_details字段")
	}

	if int(details["cached_tokens"].(float64)) != 150 {
		t.Errorf("cached_tokens = %v, 期望 150", details["cached_tokens"])
	}
}

// TestOpenAIResponsesAPITokenParsing 测试OpenAI Responses API的token统计字段解析
func TestOpenAIResponsesAPITokenParsing(t *testing.T) {
	// 模拟OpenAI Responses API响应（使用input_tokens/output_tokens）
	responseJSON := `{
		"id": "resp_abc123",
		"object": "response",
		"created": 1677652288,
		"model": "gpt-4o",
		"usage": {
			"input_tokens": 120,
			"output_tokens": 60,
			"total_tokens": 180,
			"input_tokens_details": {
				"cached_tokens": 80
			}
		}
	}`

	var response map[string]any
	if err := json.Unmarshal([]byte(responseJSON), &response); err != nil {
		t.Fatalf("JSON解析失败: %v", err)
	}

	usage, ok := response["usage"].(map[string]any)
	if !ok {
		t.Fatal("无法提取usage字段")
	}

	// 验证Responses API字段
	if int(usage["input_tokens"].(float64)) != 120 {
		t.Errorf("input_tokens = %v, 期望 120", usage["input_tokens"])
	}
	if int(usage["output_tokens"].(float64)) != 60 {
		t.Errorf("output_tokens = %v, 期望 60", usage["output_tokens"])
	}

	// 验证缓存字段
	details, ok := usage["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatal("无法提取input_tokens_details字段")
	}

	if int(details["cached_tokens"].(float64)) != 80 {
		t.Errorf("cached_tokens = %v, 期望 80", details["cached_tokens"])
	}
}

// TestOpenAIPricingCalculation 测试OpenAI模型的费用计算
func TestOpenAIPricingCalculation(t *testing.T) {
	tests := []struct {
		model           string
		inputTokens     int
		outputTokens    int
		cacheReadTokens int
		expectedCost    float64
		description     string
	}{
		{
			model:        "gpt-4o",
			inputTokens:  1_000_000,
			outputTokens: 1_000_000,
			expectedCost: 2.50 + 10.00, // $2.50/1M input + $10.00/1M output
			description:  "GPT-4o基础定价",
		},
		{
			model:           "gpt-4o",
			inputTokens:     0, // [INFO] 归一化后: 原始1M - 缓存1M = 0
			outputTokens:    1_000_000,
			cacheReadTokens: 1_000_000,
			expectedCost:    10.00 + 1.25, // 输出$10 + 缓存$1.25（GPT-4o缓存50%折扣）
			description:     "GPT-4o带缓存读取(50%折扣)",
		},
		{
			model:        "gpt-4o-mini",
			inputTokens:  1_000_000,
			outputTokens: 1_000_000,
			expectedCost: 0.15 + 0.60, // $0.15/1M input + $0.60/1M output
			description:  "GPT-4o-mini基础定价",
		},
		{
			model:        "o1",
			inputTokens:  1_000_000,
			outputTokens: 1_000_000,
			expectedCost: 15.00 + 60.00, // $15/1M input + $60/1M output
			description:  "o1基础定价",
		},
		{
			model:        "o1-mini",
			inputTokens:  1_000_000,
			outputTokens: 1_000_000,
			expectedCost: 1.10 + 4.40, // $1.10/1M input + $4.40/1M output
			description:  "o1-mini基础定价",
		},
		{
			model:        "gpt-3.5-turbo",
			inputTokens:  1_000_000,
			outputTokens: 1_000_000,
			expectedCost: 0.50 + 1.50, // $0.50/1M input + $1.50/1M output
			description:  "GPT-3.5-turbo基础定价",
		},
		{
			model:        "gpt-4-turbo",
			inputTokens:  1_000_000,
			outputTokens: 1_000_000,
			expectedCost: 10.00 + 30.00, // $10/1M input + $30/1M output
			description:  "GPT-4-turbo基础定价",
		},
		{
			model:           "gpt-4o",
			inputTokens:     0, // [INFO] 归一化后: 原始500k - 缓存800k = 0 (clamped)
			outputTokens:    100_000,
			cacheReadTokens: 800_000, // 缓存大于原始输入（边界情况）
			expectedCost:    1.00 + 1.00, // 输出$1 + 缓存$1（GPT-4o缓存50%折扣）
			description:     "GPT-4o缓存超过输入token（防御性处理）",
		},
		// 新增: GPT-5系列缓存测试 (90%折扣)
		{
			model:           "gpt-5",
			inputTokens:     0,
			outputTokens:    1_000_000,
			cacheReadTokens: 1_000_000,
			expectedCost:    10.00 + 0.125, // 输出$10 + 缓存$0.125（GPT-5缓存90%折扣）
			description:     "GPT-5带缓存读取(90%折扣)",
		},
		{
			model:           "gpt-5.1-codex-max",
			inputTokens:     997,
			outputTokens:    619,
			cacheReadTokens: 51200,
			expectedCost:    0.01383625, // 用户报告的真实场景
			description:     "GPT-5.1-Codex-Max真实场景(90%折扣)",
		},
		// 新增: GPT-4.1系列缓存测试 (75%折扣)
		{
			model:           "gpt-4.1",
			inputTokens:     0,
			outputTokens:    1_000_000,
			cacheReadTokens: 1_000_000,
			expectedCost:    8.00 + 0.50, // 输出$8 + 缓存$0.50（GPT-4.1缓存75%折扣）
			description:     "GPT-4.1带缓存读取(75%折扣)",
		},
		// 新增: o3系列缓存测试 (75%折扣)
		{
			model:           "o3",
			inputTokens:     0,
			outputTokens:    1_000_000,
			cacheReadTokens: 1_000_000,
			expectedCost:    8.00 + 0.50, // 输出$8 + 缓存$0.50（o3缓存75%折扣）
			description:     "o3带缓存读取(75%折扣)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			cost := CalculateCost(tt.model, tt.inputTokens, tt.outputTokens, tt.cacheReadTokens, 0)

			// 使用小的误差范围进行浮点数比较
			if !floatEquals(cost, tt.expectedCost, 0.0001) {
				t.Errorf("%s: 计算费用 = $%.4f, 期望 = $%.4f", tt.description, cost, tt.expectedCost)
			}
		})
	}
}

// TestOpenAIModelAliases 测试OpenAI模型别名定价
func TestOpenAIModelAliases(t *testing.T) {
	// 测试模型别名是否有正确的定价
	aliases := map[string]string{
		"chatgpt-4o-latest": "gpt-4o系列",
		"gpt-4o-2024-05-13": "gpt-4o历史版本",
	}

	for alias, desc := range aliases {
		t.Run(desc, func(t *testing.T) {
			cost := CalculateCost(alias, 1_000_000, 1_000_000, 0, 0)
			if cost == 0.0 {
				t.Errorf("模型 %s (%s) 没有定价数据", alias, desc)
			}
		})
	}
}
