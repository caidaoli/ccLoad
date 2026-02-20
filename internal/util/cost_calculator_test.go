package util

import (
	"testing"
)

// ============================================================================
// 成本计算器测试
// ============================================================================

func TestCalculateCost_Sonnet45(t *testing.T) {
	// 场景：Claude Sonnet 4.5正常请求
	// 重要：Claude API的input_tokens不包含缓存，直接就是非缓存部分
	// Input: 12 tokens (非缓存), Output: 73 tokens
	// Cache Read: 17558 tokens, Cache Creation: 278 tokens
	cost := CalculateCostDetailed("claude-sonnet-4-5-20250929", 12, 73, 17558, 278, 0)

	// 预期计算：
	// Input: 12 × $3.00 / 1M = $0.000036
	// Output: 73 × $15.00 / 1M = $0.001095
	// Cache Read: 17558 × ($3.00 × 0.1) / 1M = $0.005267
	// Cache Creation: 278 × ($3.00 × 1.25) / 1M = $0.001043
	// Total: $0.007441
	expected := 0.007441
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("Sonnet 4.5成本 = %.6f, 期望 %.6f", cost, expected)
	}
}

func TestCalculateCost_Haiku45(t *testing.T) {
	// 场景：Claude Haiku 4.5轻量请求
	cost := CalculateCostDetailed("claude-haiku-4-5", 100, 50, 0, 0, 0)

	// 预期计算：
	// Input: 100 × $1.00 / 1M = $0.0001
	// Output: 50 × $5.00 / 1M = $0.00025
	// Total: $0.00035
	expected := 0.00035
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("Haiku 4.5成本 = %.6f, 期望 %.6f", cost, expected)
	}
}

func TestCalculateCost_Opus41(t *testing.T) {
	// 场景：Claude Opus 4.1高端请求
	cost := CalculateCostDetailed("claude-opus-4-1-20250805", 1000, 2000, 0, 0, 0)

	// 预期计算：
	// Input: 1000 × $15.00 / 1M = $0.015
	// Output: 2000 × $75.00 / 1M = $0.150
	// Total: $0.165
	expected := 0.165
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("Opus 4.1成本 = %.6f, 期望 %.6f", cost, expected)
	}
}

func TestCalculateCost_Opus46(t *testing.T) {
	// 场景：Claude Opus 4.6 标准上下文（<=200k）
	cost := CalculateCostDetailed("claude-opus-4-6", 1000, 2000, 0, 0, 0)

	// 预期计算：
	// Input: 1000 × $5.00 / 1M = $0.005
	// Output: 2000 × $25.00 / 1M = $0.050
	// Total: $0.055
	expected := 0.055
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("Opus 4.6 标准上下文成本 = %.6f, 期望 %.6f", cost, expected)
	}
}

func TestCalculateCost_Opus46HighContext(t *testing.T) {
	// 场景：Claude Opus 4.6 长上下文（>200k）+ 缓存
	cost := CalculateCostDetailed("claude-opus-4-6", 250000, 2000, 10000, 10000, 0)

	// 预期计算（>200k 启用高阶价格）：
	// Input: 250000 × $10.00 / 1M = $2.500000
	// Output: 2000 × $37.50 / 1M = $0.075000
	// Cache Read: 10000 × ($10.00 × 0.1) / 1M = $0.010000
	// Cache Creation(5m): 10000 × ($10.00 × 1.25) / 1M = $0.125000
	// Total: $2.710000
	expected := 2.71
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("Opus 4.6 长上下文成本 = %.6f, 期望 %.6f", cost, expected)
	}
}

func TestCalculateCost_CacheOnly(t *testing.T) {
	// 场景：纯缓存读取（cache hit）
	cost := CalculateCostDetailed("claude-sonnet-4-5", 0, 100, 10000, 0, 0)

	// 预期计算：
	// Input: 0
	// Output: 100 × $15.00 / 1M = $0.0015
	// Cache Read: 10000 × ($3.00 × 0.1) / 1M = $0.003
	// Total: $0.0045
	expected := 0.0045
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("缓存读取成本 = %.6f, 期望 %.6f", cost, expected)
	}
}

func TestCalculateCost_LegacyModel(t *testing.T) {
	// 测试遗留模型（Claude 3.0系列）
	testCases := []struct {
		model    string
		expected float64
	}{
		{"claude-3-opus-20240229", 0.165},    // 1000×$15/1M + 2000×$75/1M = 0.015 + 0.15
		{"claude-3-sonnet-20240229", 0.033},  // 1000×$3/1M + 2000×$15/1M = 0.003 + 0.03
		{"claude-3-haiku-20240307", 0.00275}, // 1000×$0.25/1M + 2000×$1.25/1M = 0.00025 + 0.0025
	}

	for _, tc := range testCases {
		cost := CalculateCostDetailed(tc.model, 1000, 2000, 0, 0, 0)
		if !floatEquals(cost, tc.expected, 0.000001) {
			t.Errorf("%s成本 = %.6f, 期望 %.6f", tc.model, cost, tc.expected)
		}
	}
}

func TestCalculateCost_ModelAlias(t *testing.T) {
	// 测试模型别名
	testCases := []string{
		"claude-sonnet-4-5",
		"claude-haiku-4-5",
		"claude-opus-4-6",
		"claude-opus-4-1",
		"claude-3-5-sonnet-latest",
		"claude-3-opus-latest",
	}

	for _, model := range testCases {
		cost := CalculateCostDetailed(model, 1000, 1000, 0, 0, 0)
		if cost == 0.0 {
			t.Errorf("模型别名 %s 未识别", model)
		}
	}
}

func TestCalculateCost_UnknownModel(t *testing.T) {
	// 未知模型应返回0
	cost := CalculateCostDetailed("unknown-model-xyz", 1000, 1000, 0, 0, 0)
	if cost != 0.0 {
		t.Errorf("未知模型应返回0，实际 = %.6f", cost)
	}
}

func TestCalculateCost_FuzzyMatch(t *testing.T) {
	// 测试模糊匹配
	testCases := []struct {
		model       string
		shouldMatch bool
	}{
		{"claude-3-opus-extended", true},
		{"claude-3-sonnet-custom", true},
		{"claude-sonnet-4-5-custom", true},
		{"claude-opus-4-6-custom", true},
		{"gpt-4-turbo", true},          // 现在支持OpenAI模型
		{"gpt-4o-2024-12-01", true},    // 模糊匹配到gpt-4o
		{"gpt-5.1-codex-custom", true}, // 模糊匹配到gpt-5.1-codex
		{"unknown-model", false},
	}

	for _, tc := range testCases {
		cost := CalculateCostDetailed(tc.model, 1000, 1000, 0, 0, 0)
		matched := cost > 0
		if matched != tc.shouldMatch {
			t.Errorf("模糊匹配 %s: 期望%v，实际%v", tc.model, tc.shouldMatch, matched)
		}
	}
}

func TestCalculateCost_ZeroTokens(t *testing.T) {
	// 全0 tokens应返回0
	cost := CalculateCostDetailed("claude-sonnet-4-5", 0, 0, 0, 0, 0)
	if cost != 0.0 {
		t.Errorf("全0 tokens成本应为0，实际 = %.6f", cost)
	}
}

func TestCalculateCost_OpenAIModels(t *testing.T) {
	// 测试OpenAI模型费用计算
	// [INFO] 重构后：inputTokens应为归一化后的可计费token（已由解析层扣除缓存）
	testCases := []struct {
		model        string
		inputTokens  int // 归一化后的可计费输入token
		outputTokens int
		cacheRead    int
		expectedCost float64
	}{
		// GPT-5 系列（Standard层级 - 官方定价）
		// inputTokens已归一化: 原始10309-缓存6016=4293
		// 2025-12更新: OpenAI缓存改为90%折扣（0.1倍，不是50%折扣）
		{"gpt-5.3-codex-spark", 4293, 17, 6016, 0.00880355}, // 4293×1.75/1M + 17×14/1M + 6016×(1.75×0.1)/1M
		{"gpt-5.3-codex", 4293, 17, 6016, 0.00880355},       // 4293×1.75/1M + 17×14/1M + 6016×(1.75×0.1)/1M
		{"gpt-5.3", 1000, 1000, 0, 0.01575},                 // $1.75/1M input, $14/1M output
		{"gpt-5.1-codex", 4293, 17, 6016, 0.006288},         // 4293×1.25/1M + 17×10/1M + 6016×(1.25×0.1)/1M
		{"gpt-5", 1000, 1000, 0, 0.01125},                   // $1.25/1M input, $10/1M output
		{"gpt-5-mini", 10000, 5000, 0, 0.0125},              // $0.25/1M input, $2/1M output
		{"gpt-5-nano", 100000, 50000, 0, 0.025},             // $0.05/1M input, $0.4/1M output
		{"gpt-5-pro", 1000, 1000, 0, 0.135},                 // $15/1M input, $120/1M output

		// GPT-4.1 系列（新）
		{"gpt-4.1", 1000, 1000, 0, 0.01},         // $2.00/1M input, $8/1M output
		{"gpt-4.1-mini", 10000, 5000, 0, 0.012},  // $0.40/1M input, $1.60/1M output
		{"gpt-4.1-nano", 100000, 50000, 0, 0.03}, // $0.10/1M input, $0.40/1M output

		// GPT-4o 系列
		{"gpt-4o", 1000, 1000, 0, 0.0125},       // $2.50/1M input, $10/1M output
		{"gpt-4o-mini", 10000, 5000, 0, 0.0045}, // $0.15/1M input, $0.60/1M output

		// o系列（推理模型）
		{"o1", 1000, 1000, 0, 0.075},       // $15/1M input, $60/1M output
		{"o1-mini", 10000, 5000, 0, 0.033}, // $1.10/1M input, $4.40/1M output
		{"o3", 1000, 1000, 0, 0.01},        // $2.00/1M input, $8/1M output
		{"o3-mini", 10000, 5000, 0, 0.033}, // $1.10/1M input, $4.40/1M output

		// Legacy模型
		{"gpt-4-turbo", 1000, 1000, 0, 0.04},      // $10/1M input, $30/1M output
		{"gpt-3.5-turbo", 10000, 5000, 0, 0.0125}, // $0.50/1M input, $1.50/1M output
	}

	for _, tc := range testCases {
		cost := CalculateCostDetailed(tc.model, tc.inputTokens, tc.outputTokens, tc.cacheRead, 0, 0)
		if !floatEquals(cost, tc.expectedCost, 0.000001) {
			t.Errorf("%s: 成本 = %.6f, 期望 %.6f", tc.model, cost, tc.expectedCost)
		}
	}
}

func TestCalculateCost_MimoModels(t *testing.T) {
	cost := CalculateCostDetailed("mimo-v2-flash", 1_000_000, 1_000_000, 0, 0, 0)
	expected := 0.10 + 0.30
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("mimo-v2-flash: 成本 = %.6f, 期望 %.6f", cost, expected)
	}
}

func TestCalculateCost_QwenModels(t *testing.T) {
	// qwen3-32b: Input $0.08/1M, Output $0.24/1M
	cost := CalculateCostDetailed("qwen3-32b", 1_000_000, 1_000_000, 0, 0, 0)
	expected := 0.08 + 0.24
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("qwen3-32b: 成本 = %.6f, 期望 %.6f", cost, expected)
	}

	// qwen-max: Input $1.60/1M, Output $6.40/1M
	costMax := CalculateCostDetailed("qwen-max", 1_000_000, 1_000_000, 0, 0, 0)
	expectedMax := 1.60 + 6.40
	if !floatEquals(costMax, expectedMax, 0.000001) {
		t.Errorf("qwen-max: 成本 = %.6f, 期望 %.6f", costMax, expectedMax)
	}

	// 测试模糊匹配
	fuzzyCost := CalculateCostDetailed("qwen3-32b-20260102", 1_000_000, 1_000_000, 0, 0, 0)
	if !floatEquals(fuzzyCost, expected, 0.000001) {
		t.Errorf("qwen3-32b 模糊匹配: 成本 = %.6f, 期望 %.6f", fuzzyCost, expected)
	}

	// 测试别名 qwen-3-32b → qwen3-32b
	aliasCost := CalculateCostDetailed("qwen-3-32b", 1_000_000, 1_000_000, 0, 0, 0)
	if !floatEquals(aliasCost, expected, 0.000001) {
		t.Errorf("qwen-3-32b 别名: 成本 = %.6f, 期望 %.6f", aliasCost, expected)
	}
}

func TestCalculateCost_QwenModelsFromPricePerToken(t *testing.T) {
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=qwen
	// 取该接口最新历史点（按模型聚合）的$/1M token价格
	testCases := []struct {
		model       string
		expectedSum float64
	}{
		{"qwen-plus-2025-07-14", 0.40 + 1.20},
		{"qwen3-4b", 0.0715 + 0.273},
		{"qwen3-vl-32b-instruct", 0.104 + 0.416},
		{"qwen3-coder-flash", 0.30 + 1.50},
		{"qwen3-coder-next", 0.07 + 0.30},
		{"qwen3-max", 1.20 + 6.00},
		{"qwen3-max-thinking", 1.20 + 6.00},
		{"qwen3-next-80b-a3b-thinking", 0.15 + 0.30},
		{"qwq-32b-preview", 0.20 + 0.20},
		{"qwen3-coder:exacto", 0.22 + 1.80},
	}

	for _, tc := range testCases {
		cost := CalculateCostDetailed(tc.model, 1_000_000, 1_000_000, 0, 0, 0)
		if !floatEquals(cost, tc.expectedSum, 0.000001) {
			t.Errorf("%s: 成本 = %.6f, 期望 %.6f", tc.model, cost, tc.expectedSum)
		}
	}
}

func TestCalculateCost_QwenFreeVariants(t *testing.T) {
	// 免费模型必须是0，且不能被前缀模糊匹配误计费
	testCases := []string{
		"qwen3-8b:free",
		"qwen3-4b:free",
		"qwen2.5-vl-72b-instruct:free",
		"qwen3-next-80b-a3b-instruct:free",
		"qwq-32b:free",
	}

	for _, model := range testCases {
		cost := CalculateCostDetailed(model, 1_000_000, 1_000_000, 0, 0, 0)
		if cost != 0 {
			t.Errorf("%s: 免费模型成本应为0，实际 %.6f", model, cost)
		}
	}
}

func TestCalculateCost_QwenTieredPricingFromTable(t *testing.T) {
	// 用户提供表格（2026-02）：
	// - qwen3.5-plus: input 0.4/1.2, output(non-thinking) 2.4/7.2（阈值256K）
	// - qwen-plus: input 0.4/1.2, output(non-thinking) 1.2/3.6（阈值256K）
	//
	// 说明：当前计费器没有“thinking mode”维度，这里按 non-thinking 列做验证。

	// qwen3.5-plus 低档（<=256K）
	low35 := CalculateCostDetailed("qwen3.5-plus", 256_000, 1_000_000, 0, 0, 0)
	expectedLow35 := (256_000 * 0.4 / 1_000_000) + 2.4
	if !floatEquals(low35, expectedLow35, 0.000001) {
		t.Errorf("qwen3.5-plus 低档: 成本 = %.6f, 期望 %.6f", low35, expectedLow35)
	}

	// qwen3.5-plus 高档（>256K）
	high35 := CalculateCostDetailed("qwen3.5-plus", 256_001, 1_000_000, 0, 0, 0)
	expectedHigh35 := (256_001 * 1.2 / 1_000_000) + 7.2
	if !floatEquals(high35, expectedHigh35, 0.000001) {
		t.Errorf("qwen3.5-plus 高档: 成本 = %.6f, 期望 %.6f", high35, expectedHigh35)
	}

	// 版本化模型同价
	dated35 := CalculateCostDetailed("qwen3.5-plus-2026-02-15", 300_000, 1_000_000, 0, 0, 0)
	expectedDated35 := (300_000 * 1.2 / 1_000_000) + 7.2
	if !floatEquals(dated35, expectedDated35, 0.000001) {
		t.Errorf("qwen3.5-plus-2026-02-15: 成本 = %.6f, 期望 %.6f", dated35, expectedDated35)
	}

	// qwen-plus 低档（<=256K）
	lowPlus := CalculateCostDetailed("qwen-plus", 256_000, 1_000_000, 0, 0, 0)
	expectedLowPlus := (256_000 * 0.4 / 1_000_000) + 1.2
	if !floatEquals(lowPlus, expectedLowPlus, 0.000001) {
		t.Errorf("qwen-plus 低档: 成本 = %.6f, 期望 %.6f", lowPlus, expectedLowPlus)
	}

	// qwen-plus 高档（>256K）
	highPlus := CalculateCostDetailed("qwen-plus", 300_000, 1_000_000, 0, 0, 0)
	expectedHighPlus := (300_000 * 1.2 / 1_000_000) + 3.6
	if !floatEquals(highPlus, expectedHighPlus, 0.000001) {
		t.Errorf("qwen-plus 高档: 成本 = %.6f, 期望 %.6f", highPlus, expectedHighPlus)
	}

	// qwen-plus-latest 与 qwen-plus 同价
	latestPlus := CalculateCostDetailed("qwen-plus-latest", 300_000, 1_000_000, 0, 0, 0)
	if !floatEquals(latestPlus, expectedHighPlus, 0.000001) {
		t.Errorf("qwen-plus-latest: 成本 = %.6f, 期望 %.6f", latestPlus, expectedHighPlus)
	}

	// qwen-plus-2025-07-28:thinking 按 thinking 列计费
	thinkingPlus := CalculateCostDetailed("qwen-plus-2025-07-28:thinking", 300_000, 1_000_000, 0, 0, 0)
	expectedThinkingPlus := (300_000 * 1.2 / 1_000_000) + 12.0
	if !floatEquals(thinkingPlus, expectedThinkingPlus, 0.000001) {
		t.Errorf("qwen-plus-2025-07-28:thinking: 成本 = %.6f, 期望 %.6f", thinkingPlus, expectedThinkingPlus)
	}
}

func TestCalculateCost_DeepSeekModels(t *testing.T) {
	// deepseek-r1: Input $0.30/1M, Output $1.20/1M
	costR1 := CalculateCostDetailed("deepseek-r1", 1_000_000, 1_000_000, 0, 0, 0)
	expectedR1 := 0.30 + 1.20
	if !floatEquals(costR1, expectedR1, 0.000001) {
		t.Errorf("deepseek-r1: 成本 = %.6f, 期望 %.6f", costR1, expectedR1)
	}

	// deepseek-chat (v3): Input $0.30/1M, Output $1.20/1M
	costChat := CalculateCostDetailed("deepseek-chat", 1_000_000, 1_000_000, 0, 0, 0)
	if !floatEquals(costChat, expectedR1, 0.000001) {
		t.Errorf("deepseek-chat: 成本 = %.6f, 期望 %.6f", costChat, expectedR1)
	}

	// 别名测试
	costV3 := CalculateCostDetailed("deepseek-v3", 1_000_000, 1_000_000, 0, 0, 0)
	if !floatEquals(costV3, expectedR1, 0.000001) {
		t.Errorf("deepseek-v3 别名: 成本 = %.6f, 期望 %.6f", costV3, expectedR1)
	}

	// 蒸馏模型测试
	costDistill := CalculateCostDetailed("deepseek-r1-distill-llama-70b", 1_000_000, 1_000_000, 0, 0, 0)
	expectedDistill := 0.03 + 0.11
	if !floatEquals(costDistill, expectedDistill, 0.000001) {
		t.Errorf("deepseek-distill-llama-70b: 成本 = %.6f, 期望 %.6f", costDistill, expectedDistill)
	}
}

func TestCalculateCost_XAIModels(t *testing.T) {
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=xai
	testCases := []struct {
		model  string
		input  float64 // $/M tokens
		output float64 // $/M tokens
	}{
		{"grok-4", 3.00, 15.00},
		{"grok-4-fast", 0.20, 0.50},
		{"grok-3", 3.00, 15.00},
		{"grok-3-beta", 3.00, 15.00},
		{"grok-3-mini", 0.30, 0.50},
		{"grok-3-mini-beta", 0.30, 0.50},
		{"grok-2", 2.00, 10.00},
		{"grok-2-1212", 2.00, 10.00},
		{"grok-2-vision-1212", 2.00, 10.00},
		{"grok-2-mini", 0.20, 0.50},
		{"grok-code-fast-1", 0.20, 1.50},
		{"grok-vision-beta", 5.00, 15.00},
	}

	for _, tc := range testCases {
		cost := CalculateCostDetailed(tc.model, 1_000_000, 1_000_000, 0, 0, 0)
		expected := tc.input + tc.output
		if !floatEquals(cost, expected, 0.000001) {
			t.Errorf("%s: 成本 = %.6f, 期望 %.6f", tc.model, cost, expected)
		}
	}

	// 别名测试
	costBeta := CalculateCostDetailed("grok-beta", 1_000_000, 1_000_000, 0, 0, 0)
	expected3 := 3.00 + 15.00
	if !floatEquals(costBeta, expected3, 0.000001) {
		t.Errorf("grok-beta 别名: 成本 = %.6f, 期望 %.6f", costBeta, expected3)
	}

	// 模糊匹配测试
	fuzzyTests := []struct {
		model    string
		expected float64
	}{
		{"grok-4-20260101", 3.00 + 15.00},      // 匹配 grok-4
		{"grok-3-mini-custom", 0.30 + 0.50},    // 匹配 grok-3-mini
		{"grok-2-1212-extended", 2.00 + 10.00}, // 匹配 grok-2-1212
	}
	for _, tc := range fuzzyTests {
		cost := CalculateCostDetailed(tc.model, 1_000_000, 1_000_000, 0, 0, 0)
		if !floatEquals(cost, tc.expected, 0.000001) {
			t.Errorf("%s 模糊匹配: 成本 = %.6f, 期望 %.6f", tc.model, cost, tc.expected)
		}
	}
}

// TestCalculateCost_FixedCostPerRequest 测试按次计费的图像生成模型
func TestCalculateCost_FixedCostPerRequest(t *testing.T) {
	// 图像生成模型：tokens为0时返回固定成本
	testCases := []struct {
		model    string
		expected float64
	}{
		{"grok-2-image-1212", 0.07},
		{"grok-imagine-image", 0.02},
		{"grok-imagine-image-pro", 0.07},
	}

	for _, tc := range testCases {
		// tokens全为0，应返回固定成本
		cost := CalculateCostDetailed(tc.model, 0, 0, 0, 0, 0)
		if !floatEquals(cost, tc.expected, 0.000001) {
			t.Errorf("%s: 成本 = %.6f, 期望 %.6f", tc.model, cost, tc.expected)
		}
	}

	// 如果有tokens，应按token计费（固定成本不叠加）
	// grok-imagine-image InputPrice=0, OutputPrice=0, 所以token成本为0，回退到固定成本
	cost := CalculateCostDetailed("grok-imagine-image", 1000, 0, 0, 0, 0)
	if !floatEquals(cost, 0.02, 0.000001) {
		t.Errorf("grok-imagine-image 有tokens但无token定价，应回退到固定成本: %.6f", cost)
	}

	// 模糊匹配测试
	cost = CalculateCostDetailed("grok-2-image-1212-custom", 0, 0, 0, 0, 0)
	if !floatEquals(cost, 0.07, 0.000001) {
		t.Errorf("grok-2-image-1212-custom 模糊匹配: 成本 = %.6f, 期望 0.07", cost)
	}

	// 视频模型：按秒计费，当前无duration信息，应返回0
	cost = CalculateCostDetailed("grok-imagine-video", 0, 0, 0, 0, 0)
	if cost != 0 {
		t.Errorf("grok-imagine-video 无duration时应返回0, 实际: %.6f", cost)
	}

	// 视频模型模糊匹配：确认模型被识别
	pricing, ok := getPricing("grok-imagine-video")
	if !ok {
		pricing, ok = fuzzyMatchModel("grok-imagine-video")
	}
	if !ok {
		t.Fatal("grok-imagine-video 应被定价表识别")
	}
	if !floatEquals(pricing.CostPerSecond, 0.05, 0.000001) {
		t.Errorf("grok-imagine-video CostPerSecond = %.4f, 期望 0.05", pricing.CostPerSecond)
	}
}

func TestCalculateCost_MiniMaxModels(t *testing.T) {
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=minimax
	testCases := []struct {
		model  string
		input  float64
		output float64
	}{
		{"minimax-01", 0.20, 1.10},
		{"minimax-m1", 0.30, 1.65},
		{"minimax-m2", 0.15, 0.45},
		{"minimax-m2.1", 0.30, 1.20},
	}

	for _, tc := range testCases {
		cost := CalculateCostDetailed(tc.model, 1_000_000, 1_000_000, 0, 0, 0)
		expected := tc.input + tc.output
		if !floatEquals(cost, expected, 0.000001) {
			t.Errorf("%s: 成本 = %.6f, 期望 %.6f", tc.model, cost, expected)
		}
	}

	// 模糊匹配测试
	cost := CalculateCostDetailed("minimax-m2-20260101", 1_000_000, 1_000_000, 0, 0, 0)
	expected := 0.15 + 0.45
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("minimax-m2 模糊匹配: 成本 = %.6f, 期望 %.6f", cost, expected)
	}
}

func TestCalculateCost_CacheSavings(t *testing.T) {
	// 验证缓存节省（Cache Read vs 普通Input）
	normalCost := CalculateCostDetailed("claude-sonnet-4-5", 10000, 0, 0, 0, 0)
	cacheCost := CalculateCostDetailed("claude-sonnet-4-5", 0, 0, 10000, 0, 0)

	// Cache Read应该是普通Input的10%
	expectedRatio := 0.1
	actualRatio := cacheCost / normalCost

	if !floatEquals(actualRatio, expectedRatio, 0.01) {
		t.Errorf("缓存读取节省比例 = %.2f, 期望 %.2f (90%%节省)", actualRatio, expectedRatio)
	}

	t.Logf("普通输入成本: $%.6f", normalCost)
	t.Logf("缓存读取成本: $%.6f (节省 %.0f%%)", cacheCost, (1-actualRatio)*100)
}

func TestCacheWriteCost(t *testing.T) {
	// 验证缓存写入成本（应该是Input的125%）
	inputCost := CalculateCostDetailed("claude-sonnet-4-5", 10000, 0, 0, 0, 0)
	cacheWriteCost := CalculateCostDetailed("claude-sonnet-4-5", 0, 0, 0, 10000, 0)

	expectedRatio := 1.25
	actualRatio := cacheWriteCost / inputCost

	if !floatEquals(actualRatio, expectedRatio, 0.01) {
		t.Errorf("缓存写入价格倍数 = %.2f, 期望 %.2f", actualRatio, expectedRatio)
	}

	t.Logf("普通输入成本: $%.6f", inputCost)
	t.Logf("缓存写入成本: $%.6f (溢价 %.0f%%)", cacheWriteCost, (actualRatio-1)*100)
}

// TestCalculateCost_OpusCacheRead 验证Opus模型缓存读取定价（10%价）
// 参考：https://docs.claude.com/en/docs/about-claude/pricing
// Opus缓存读取价格 = 基础输入价格 × 0.1（90%折扣）
// 而Sonnet/Haiku缓存读取价格 = 基础输入价格 × 0.1（90%折扣）
func TestCalculateCost_OpusCacheRead(t *testing.T) {
	// 场景：Claude Opus 4.5 使用 Prompt Caching
	// 数据来源：用户实际请求
	// 提示 tokens: 8
	// 缓存读取 tokens: 53660
	// 缓存创建 tokens: 816
	// 补全 tokens: 269
	cost := CalculateCostDetailed("claude-opus-4-5-20251101", 8, 269, 53660, 816, 0)

	// 预期计算（Opus缓存倍率=0.1）：
	// Input: 8 × $5.00 / 1M = $0.00004
	// Cache Read: 53660 × ($5.00 × 0.1) / 1M = $0.02683
	// Cache Creation: 816 × ($5.00 × 1.25) / 1M = $0.0051
	// Output: 269 × $25.00 / 1M = $0.006725
	// Total: $0.038695
	expected := 0.038695
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("Opus 4.5带缓存成本 = %.6f, 期望 %.6f", cost, expected)
	}

	t.Logf("[INFO] Opus缓存定价验证通过: $%.6f", cost)
}

// TestCalculateCost_OpusVsSonnetCacheRatio 验证Opus和Sonnet的缓存倍率差异
func TestCalculateCost_OpusVsSonnetCacheRatio(t *testing.T) {
	cacheTokens := 10000

	// Opus: 缓存读取 = 输入价格 × 0.1（90%折扣）
	opusCacheCost := CalculateCostDetailed("claude-opus-4-5", 0, 0, cacheTokens, 0, 0)
	opusInputCost := CalculateCostDetailed("claude-opus-4-5", cacheTokens, 0, 0, 0, 0)
	opusRatio := opusCacheCost / opusInputCost

	// Sonnet: 缓存读取 = 输入价格 × 0.1（90%折扣）
	sonnetCacheCost := CalculateCostDetailed("claude-sonnet-4-5", 0, 0, cacheTokens, 0, 0)
	sonnetInputCost := CalculateCostDetailed("claude-sonnet-4-5", cacheTokens, 0, 0, 0, 0)
	sonnetRatio := sonnetCacheCost / sonnetInputCost

	// 验证Opus缓存倍率为0.1
	if !floatEquals(opusRatio, 0.1, 0.01) {
		t.Errorf("Opus缓存读取倍率 = %.2f, 期望 0.1", opusRatio)
	}

	// 验证Sonnet缓存倍率为0.1
	if !floatEquals(sonnetRatio, 0.1, 0.01) {
		t.Errorf("Sonnet缓存读取倍率 = %.2f, 期望 0.1", sonnetRatio)
	}

	t.Logf("[INFO] Opus缓存倍率: %.2f (90%%折扣)", opusRatio)
	t.Logf("[INFO] Sonnet缓存倍率: %.2f (90%%折扣)", sonnetRatio)
}

func TestRealWorldScenario(t *testing.T) {
	// 真实场景：带缓存的长对话
	// - 首次请求：创建缓存（系统prompt 2000 tokens）+ 输入100 + 输出200
	// - 后续请求：读取缓存 + 输入50 + 输出150
	firstCost := CalculateCostDetailed("claude-sonnet-4-5", 100, 200, 0, 2000, 0)
	laterCost := CalculateCostDetailed("claude-sonnet-4-5", 50, 150, 2000, 0, 0)

	t.Logf("首次请求成本: $%.6f", firstCost)
	t.Logf("后续请求成本: $%.6f (缓存命中)", laterCost)
	t.Logf("节省: $%.6f (%.1f%%)", firstCost-laterCost, (1-laterCost/firstCost)*100)

	// 后续请求应该更便宜（缓存读取只有10%价格）
	if laterCost >= firstCost {
		t.Errorf("缓存命中后成本应降低：首次$%.6f, 后续$%.6f", firstCost, laterCost)
	}
}

// floatEquals 浮点数相等比较（带误差容忍）
func floatEquals(a, b, epsilon float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < epsilon
}

func TestCalculateCost_Gpt4oLegacyFuzzy(t *testing.T) {
	// 验证gpt-4o-legacy带日期后缀能正确匹配到legacy价格
	cost := CalculateCostDetailed("gpt-4o-legacy-2024-05-13", 1000, 1000, 0, 0, 0)

	// gpt-4o-legacy: input=$5/1M, output=$15/1M
	// 1000×$5/1M + 1000×$15/1M = $0.02
	expected := 0.02

	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("gpt-4o-legacy-2024-05-13应匹配legacy价格: 实际$%.6f, 期望$%.6f", cost, expected)
	} else {
		t.Logf("[INFO] gpt-4o-legacy带后缀正确匹配: $%.6f", cost)
	}

	// 验证不会误匹配到gpt-4o
	gpt4oCost := CalculateCostDetailed("gpt-4o", 1000, 1000, 0, 0, 0)
	if floatEquals(cost, gpt4oCost, 0.000001) {
		t.Errorf("gpt-4o-legacy和gpt-4o价格应不同！legacy=$%.6f, gpt-4o=$%.6f", cost, gpt4oCost)
	}
}

// TestCalculateCostDetailed_5mVs1hCache 验证5分钟和1小时缓存的定价差异
// 参考: https://platform.claude.com/docs/en/build-with-claude/prompt-caching
// - 5m缓存写入: 基础价格 × 1.25
// - 1h缓存写入: 基础价格 × 2.0
// - 缓存读取: 基础价格 × 0.1（两种时长相同）
func TestCalculateCostDetailed_5mVs1hCache(t *testing.T) {
	model := "claude-sonnet-4-5"
	// 基础价格: input=$3/MTok, output=$15/MTok

	// 场景1: 仅5m缓存写入 1000 tokens
	cost5m := CalculateCostDetailed(model, 0, 0, 0, 1000, 0)
	// 预期: 1000 × ($3 × 1.25) / 1M = $0.003750
	expected5m := 0.003750
	if !floatEquals(cost5m, expected5m, 0.000001) {
		t.Errorf("5m缓存写入成本错误: 实际$%.6f, 期望$%.6f", cost5m, expected5m)
	}

	// 场景2: 仅1h缓存写入 1000 tokens
	cost1h := CalculateCostDetailed(model, 0, 0, 0, 0, 1000)
	// 预期: 1000 × ($3 × 2.0) / 1M = $0.006000
	expected1h := 0.006000
	if !floatEquals(cost1h, expected1h, 0.000001) {
		t.Errorf("1h缓存写入成本错误: 实际$%.6f, 期望$%.6f", cost1h, expected1h)
	}

	// 场景3: 混合使用 - 500 tokens 5m缓存 + 500 tokens 1h缓存
	costMixed := CalculateCostDetailed(model, 0, 0, 0, 500, 500)
	// 预期: 500 × ($3 × 1.25) / 1M + 500 × ($3 × 2.0) / 1M = $0.004875
	expectedMixed := 0.004875
	if !floatEquals(costMixed, expectedMixed, 0.000001) {
		t.Errorf("混合缓存写入成本错误: 实际$%.6f, 期望$%.6f", costMixed, expectedMixed)
	}

	// 验证定价关系: 1h缓存应该是5m缓存的1.6倍 (2.0 / 1.25)
	ratio := cost1h / cost5m
	expectedRatio := 1.6
	if !floatEquals(ratio, expectedRatio, 0.01) {
		t.Errorf("1h/5m缓存价格比例错误: 实际%.2f, 期望%.2f", ratio, expectedRatio)
	}

	t.Logf("[INFO] 5m缓存: $%.6f (1.25x基础价)", cost5m)
	t.Logf("[INFO] 1h缓存: $%.6f (2.0x基础价)", cost1h)
	t.Logf("[INFO] 混合缓存: $%.6f", costMixed)
	t.Logf("[INFO] 1h/5m价格比例: %.2fx", ratio)
}

// TestCalculateCostDetailed_CompleteScenario 完整场景测试
// 验证包含所有token类型的复杂请求
func TestCalculateCostDetailed_CompleteScenario(t *testing.T) {
	model := "claude-sonnet-4-5"
	// 场景: 普通输入100 + 输出200 + 缓存读1000 + 5m缓存写500 + 1h缓存写300

	cost := CalculateCostDetailed(model, 100, 200, 1000, 500, 300)

	// 预期计算:
	// 1. 普通输入: 100 × $3 / 1M = $0.000300
	// 2. 输出: 200 × $15 / 1M = $0.003000
	// 3. 缓存读: 1000 × ($3 × 0.1) / 1M = $0.000300
	// 4. 5m缓存写: 500 × ($3 × 1.25) / 1M = $0.001875
	// 5. 1h缓存写: 300 × ($3 × 2.0) / 1M = $0.001800
	// Total: $0.007275
	expected := 0.007275

	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("完整场景成本错误: 实际$%.6f, 期望$%.6f", cost, expected)
	}

	t.Logf("[INFO] 完整场景测试通过")
	t.Logf("  普通输入: 100 tokens → $0.000300")
	t.Logf("  输出: 200 tokens → $0.003000")
	t.Logf("  缓存读: 1000 tokens → $0.000300")
	t.Logf("  5m缓存写: 500 tokens → $0.001875")
	t.Logf("  1h缓存写: 300 tokens → $0.001800")
	t.Logf("  总计: $%.6f", cost)
}

// 旧的 CalculateCost() 兼容壳已删除，避免重复API与歧义参数。
