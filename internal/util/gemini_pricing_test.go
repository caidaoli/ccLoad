package util

import "testing"

func TestCalculateCost_Gemini(t *testing.T) {
	tests := []struct {
		name            string
		model           string
		inputTokens     int
		outputTokens    int
		expectedCostUSD float64
		description     string
	}{
		{
			name:            "gemini-2.5-flash基础用量",
			model:           "gemini-2.5-flash",
			inputTokens:     1_000_000,   // 1M tokens
			outputTokens:    1_000_000,   // 1M tokens
			expectedCostUSD: 0.30 + 2.50, // $0.30/M input + $2.50/M output
			description:     "验证gemini-2.5-flash定价：1M input + 1M output = $2.80",
		},
		{
			name:            "gemini-2.5-pro高级模型（会触发长上下文）",
			model:           "gemini-2.5-pro",
			inputTokens:     500_000,              // 0.5M tokens
			outputTokens:    100_000,              // 0.1M tokens (总计600k > 200k)
			expectedCostUSD: 2.50*0.5 + 15.00*0.1, // 触发长上下文定价
			description:     "验证gemini-2.5-pro：0.5M input + 0.1M output (总计600k) = $2.75（长上下文）",
		},
		{
			name:            "gemini-3-pro最新模型（会触发长上下文）",
			model:           "gemini-3-pro",
			inputTokens:     250_000,                // 0.25M tokens
			outputTokens:    50_000,                 // 0.05M tokens (总计300k > 200k)
			expectedCostUSD: 4.00*0.25 + 18.00*0.05, // 触发长上下文定价
			description:     "验证gemini-3-pro：0.25M input + 0.05M output (总计300k) = $1.90（长上下文）",
		},
		{
			name:            "gemini-2.0-flash经济型",
			model:           "gemini-2.0-flash",
			inputTokens:     2_000_000,           // 2M tokens
			outputTokens:    500_000,             // 0.5M tokens
			expectedCostUSD: 0.10*2.0 + 0.40*0.5, // $0.10/M * 2 + $0.40/M * 0.5
			description:     "验证gemini-2.0-flash定价：2M input + 0.5M output = $0.40",
		},
		{
			name:            "gemini-1.5-flash遗留模型",
			model:           "gemini-1.5-flash",
			inputTokens:     1_000_000,
			outputTokens:    500_000,
			expectedCostUSD: 0.20*1.0 + 0.60*0.5,
			description:     "验证gemini-1.5-flash定价：1M input + 0.5M output = $0.50",
		},
		{
			name:            "gemini-2.5-flash-lite超低成本",
			model:           "gemini-2.5-flash-lite",
			inputTokens:     5_000_000, // 5M tokens
			outputTokens:    1_000_000, // 1M tokens
			expectedCostUSD: 0.10*5.0 + 0.40*1.0,
			description:     "验证gemini-2.5-flash-lite定价：5M input + 1M output = $0.90",
		},
		{
			name:            "零token请求",
			model:           "gemini-2.5-flash",
			inputTokens:     0,
			outputTokens:    0,
			expectedCostUSD: 0.0,
			description:     "验证零token场景（如错误响应）",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCost(tt.model, tt.inputTokens, tt.outputTokens, 0, 0)

			// 使用精度容差比较浮点数（允许0.0001美元的误差）
			tolerance := 0.0001
			if abs(got-tt.expectedCostUSD) > tolerance {
				t.Errorf("%s\n模型: %s\n输入: %d tokens, 输出: %d tokens\n期望费用: $%.4f\n实际费用: $%.4f\n差异: $%.4f",
					tt.description,
					tt.model,
					tt.inputTokens,
					tt.outputTokens,
					tt.expectedCostUSD,
					got,
					abs(got-tt.expectedCostUSD),
				)
			} else {
				t.Logf("✅ %s: $%.4f", tt.description, got)
			}
		})
	}
}

func TestCalculateCost_GeminiLongContext(t *testing.T) {
	tests := []struct {
		name            string
		model           string
		inputTokens     int
		outputTokens    int
		expectedCostUSD float64
		description     string
	}{
		{
			name:            "gemini-3-pro标准上下文（≤200k）",
			model:           "gemini-3-pro",
			inputTokens:     150_000,                // 150k tokens
			outputTokens:    50_000,                 // 50k tokens (总计200k)
			expectedCostUSD: 2.00*0.15 + 12.00*0.05, // 使用标准价格
			description:     "总计200k tokens，应使用标准定价 $2.00/$12.00",
		},
		{
			name:            "gemini-3-pro长上下文（>200k）",
			model:           "gemini-3-pro",
			inputTokens:     150_000,                 // 150k tokens (输入侧未超阈值)
			outputTokens:    51_000,                  // 51k tokens
			expectedCostUSD: 2.00*0.15 + 12.00*0.051, // 使用标准价格（输入150k < 200k）
			description:     "输入150k tokens，使用标准定价 $2.00/$12.00",
		},
		{
			name:            "gemini-2.5-pro标准上下文",
			model:           "gemini-2.5-pro",
			inputTokens:     100_000,
			outputTokens:    100_000, // 总计200k
			expectedCostUSD: 1.25*0.1 + 10.00*0.1,
			description:     "总计200k tokens，应使用标准定价 $1.25/$10.00",
		},
		{
			name:            "gemini-2.5-pro长上下文",
			model:           "gemini-2.5-pro",
			inputTokens:     150_000, // 输入侧未超阈值
			outputTokens:    100_000,
			expectedCostUSD: 1.25*0.15 + 10.00*0.1, // 使用标准价格（输入150k < 200k）
			description:     "输入150k tokens，使用标准定pricing $1.25/$10.00",
		},
		{
			name:            "gemini-2.5-flash无分段定价（大量tokens）",
			model:           "gemini-2.5-flash",
			inputTokens:     500_000,             // 500k tokens
			outputTokens:    500_000,             // 500k tokens (总计1M)
			expectedCostUSD: 0.30*0.5 + 2.50*0.5, // 始终使用相同价格
			description:     "Flash模型无分段定价，即使>200k也用标准价格",
		},
		{
			name:            "gemini-3-pro超大上下文",
			model:           "gemini-3-pro",
			inputTokens:     1_000_000,            // 1M tokens
			outputTokens:    500_000,              // 500k tokens (总计1.5M)
			expectedCostUSD: 4.00*1.0 + 18.00*0.5, // 使用高价格
			description:     "总计1.5M tokens，远超200k阈值，使用长上下文定价",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCost(tt.model, tt.inputTokens, tt.outputTokens, 0, 0)

			tolerance := 0.0001
			if abs(got-tt.expectedCostUSD) > tolerance {
				t.Errorf("%s\n模型: %s\n输入: %d tokens, 输出: %d tokens (总计: %d)\n期望费用: $%.4f\n实际费用: $%.4f\n差异: $%.4f",
					tt.description,
					tt.model,
					tt.inputTokens,
					tt.outputTokens,
					tt.inputTokens+tt.outputTokens,
					tt.expectedCostUSD,
					got,
					abs(got-tt.expectedCostUSD),
				)
			} else {
				t.Logf("✅ %s: $%.4f", tt.description, got)
			}
		})
	}
}

func TestCalculateCost_GeminiFuzzyMatch(t *testing.T) {
	// 测试Gemini模型模糊匹配（带日期后缀的版本）
	tests := []struct {
		name            string
		model           string
		inputTokens     int
		outputTokens    int
		expectedCostUSD float64
		description     string
	}{
		{
			name:            "gemini-2.5-flash带版本后缀",
			model:           "gemini-2.5-flash-preview-05-20",
			inputTokens:     1_000_000,
			outputTokens:    1_000_000,
			expectedCostUSD: 0.30 + 2.50, // 应该匹配到 gemini-2.5-flash
			description:     "gemini-2.5-flash-preview-05-20 应匹配 gemini-2.5-flash 定价",
		},
		{
			name:            "gemini-2.5-pro带版本后缀",
			model:           "gemini-2.5-pro-exp-0827",
			inputTokens:     100_000,
			outputTokens:    100_000, // 总计200k，不触发长上下文
			expectedCostUSD: 1.25*0.1 + 10.00*0.1,
			description:     "gemini-2.5-pro-exp-0827 应匹配 gemini-2.5-pro 定价",
		},
		{
			name:            "gemini-1.5-pro带版本后缀",
			model:           "gemini-1.5-pro-002",
			inputTokens:     1_000_000,
			outputTokens:    500_000,
			expectedCostUSD: 1.25*1.0 + 5.00*0.5,
			description:     "gemini-1.5-pro-002 应匹配 gemini-1.5-pro 定价",
		},
		{
			name:            "gemini-1.5-flash带latest标记",
			model:           "gemini-1.5-flash-latest",
			inputTokens:     1_000_000,
			outputTokens:    1_000_000,
			expectedCostUSD: 0.20 + 0.60,
			description:     "gemini-1.5-flash-latest 应匹配 gemini-1.5-flash 定价",
		},
		{
			name:            "gemini-2.0-flash带实验标记",
			model:           "gemini-2.0-flash-exp",
			inputTokens:     1_000_000,
			outputTokens:    500_000,
			expectedCostUSD: 0.10 + 0.40*0.5,
			description:     "gemini-2.0-flash-exp 应匹配 gemini-2.0-flash 定价",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCost(tt.model, tt.inputTokens, tt.outputTokens, 0, 0)

			tolerance := 0.0001
			if abs(got-tt.expectedCostUSD) > tolerance {
				t.Errorf("%s\n模型: %s\n输入: %d tokens, 输出: %d tokens\n期望费用: $%.4f\n实际费用: $%.4f\n差异: $%.4f",
					tt.description,
					tt.model,
					tt.inputTokens,
					tt.outputTokens,
					tt.expectedCostUSD,
					got,
					abs(got-tt.expectedCostUSD),
				)
			} else {
				t.Logf("✅ %s: $%.4f", tt.description, got)
			}
		})
	}
}

func TestCalculateCost_GeminiUnknownModel(t *testing.T) {
	// 测试未知Gemini模型的fallback行为
	model := "gemini-unknown-model-xyz"
	cost := CalculateCost(model, 1_000_000, 1_000_000, 0, 0)

	if cost != 0.0 {
		t.Errorf("未知Gemini模型应返回0费用，实际返回: $%.4f", cost)
	} else {
		t.Logf("✅ 未知模型正确返回0费用")
	}
}

// abs 返回浮点数的绝对值
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
