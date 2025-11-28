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
	cost := CalculateCost("claude-sonnet-4-5-20250929", 12, 73, 17558, 278)

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
	cost := CalculateCost("claude-haiku-4-5", 100, 50, 0, 0)

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
	cost := CalculateCost("claude-opus-4-1-20250805", 1000, 2000, 0, 0)

	// 预期计算：
	// Input: 1000 × $5.00 / 1M = $0.005
	// Output: 2000 × $25.00 / 1M = $0.050
	// Total: $0.055
	expected := 0.055
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("Opus 4.1成本 = %.6f, 期望 %.6f", cost, expected)
	}
}

func TestCalculateCost_CacheOnly(t *testing.T) {
	// 场景：纯缓存读取（cache hit）
	cost := CalculateCost("claude-sonnet-4-5", 0, 100, 10000, 0)

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
		{"claude-3-opus-20240229", 0.055},   // 1000×$5/1M + 2000×$25/1M = 0.005 + 0.05
		{"claude-3-sonnet-20240229", 0.033}, // 1000×$3/1M + 2000×$15/1M = 0.003 + 0.03
		{"claude-3-haiku-20240307", 0.00275}, // 1000×$0.25/1M + 2000×$1.25/1M = 0.00025 + 0.0025
	}

	for _, tc := range testCases {
		cost := CalculateCost(tc.model, 1000, 2000, 0, 0)
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
		"claude-opus-4-1",
		"claude-3-5-sonnet-latest",
		"claude-3-opus-latest",
	}

	for _, model := range testCases {
		cost := CalculateCost(model, 1000, 1000, 0, 0)
		if cost == 0.0 {
			t.Errorf("模型别名 %s 未识别", model)
		}
	}
}

func TestCalculateCost_UnknownModel(t *testing.T) {
	// 未知模型应返回0
	cost := CalculateCost("unknown-model-xyz", 1000, 1000, 0, 0)
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
		{"gpt-4-turbo", true},          // 现在支持OpenAI模型
		{"gpt-4o-2024-12-01", true},    // 模糊匹配到gpt-4o
		{"gpt-5.1-codex-custom", true}, // 模糊匹配到gpt-5.1-codex
		{"unknown-model", false},
	}

	for _, tc := range testCases {
		cost := CalculateCost(tc.model, 1000, 1000, 0, 0)
		matched := cost > 0
		if matched != tc.shouldMatch {
			t.Errorf("模糊匹配 %s: 期望%v，实际%v", tc.model, tc.shouldMatch, matched)
		}
	}
}

func TestCalculateCost_ZeroTokens(t *testing.T) {
	// 全0 tokens应返回0
	cost := CalculateCost("claude-sonnet-4-5", 0, 0, 0, 0)
	if cost != 0.0 {
		t.Errorf("全0 tokens成本应为0，实际 = %.6f", cost)
	}
}

func TestCalculateCost_OpenAIModels(t *testing.T) {
	// 测试OpenAI模型费用计算
	// ✅ 重构后：inputTokens应为归一化后的可计费token（已由解析层扣除缓存）
	testCases := []struct {
		model        string
		inputTokens  int // 归一化后的可计费输入token
		outputTokens int
		cacheRead    int
		expectedCost float64
	}{
		// GPT-5 系列（Standard层级 - 官方定价）
		// inputTokens已归一化: 原始10309-缓存6016=4293
		{"gpt-5.1-codex", 4293, 17, 6016, 0.009296}, // 4293×1.25/1M + 17×10/1M + 6016×(1.25×0.5)/1M
		{"gpt-5", 1000, 1000, 0, 0.01125},           // $1.25/1M input, $10/1M output
		{"gpt-5-mini", 10000, 5000, 0, 0.0125},      // $0.25/1M input, $2/1M output
		{"gpt-5-nano", 100000, 50000, 0, 0.025},     // $0.05/1M input, $0.4/1M output
		{"gpt-5-pro", 1000, 1000, 0, 0.135},         // $15/1M input, $120/1M output

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
		cost := CalculateCost(tc.model, tc.inputTokens, tc.outputTokens, tc.cacheRead, 0)
		if !floatEquals(cost, tc.expectedCost, 0.000001) {
			t.Errorf("%s: 成本 = %.6f, 期望 %.6f", tc.model, cost, tc.expectedCost)
		}
	}
}

func TestCalculateCost_CacheSavings(t *testing.T) {
	// 验证缓存节省（Cache Read vs 普通Input）
	normalCost := CalculateCost("claude-sonnet-4-5", 10000, 0, 0, 0)
	cacheCost := CalculateCost("claude-sonnet-4-5", 0, 0, 10000, 0)

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
	inputCost := CalculateCost("claude-sonnet-4-5", 10000, 0, 0, 0)
	cacheWriteCost := CalculateCost("claude-sonnet-4-5", 0, 0, 0, 10000)

	expectedRatio := 1.25
	actualRatio := cacheWriteCost / inputCost

	if !floatEquals(actualRatio, expectedRatio, 0.01) {
		t.Errorf("缓存写入价格倍数 = %.2f, 期望 %.2f", actualRatio, expectedRatio)
	}

	t.Logf("普通输入成本: $%.6f", inputCost)
	t.Logf("缓存写入成本: $%.6f (溢价 %.0f%%)", cacheWriteCost, (actualRatio-1)*100)
}

func TestRealWorldScenario(t *testing.T) {
	// 真实场景：带缓存的长对话
	// - 首次请求：创建缓存（系统prompt 2000 tokens）+ 输入100 + 输出200
	// - 后续请求：读取缓存 + 输入50 + 输出150
	firstCost := CalculateCost("claude-sonnet-4-5", 100, 200, 0, 2000)
	laterCost := CalculateCost("claude-sonnet-4-5", 50, 150, 2000, 0)

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
	cost := CalculateCost("gpt-4o-legacy-2024-05-13", 1000, 1000, 0, 0)

	// gpt-4o-legacy: input=$5/1M, output=$15/1M
	// 1000×$5/1M + 1000×$15/1M = $0.02
	expected := 0.02

	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("gpt-4o-legacy-2024-05-13应匹配legacy价格: 实际$%.6f, 期望$%.6f", cost, expected)
	} else {
		t.Logf("✅ gpt-4o-legacy带后缀正确匹配: $%.6f", cost)
	}

	// 验证不会误匹配到gpt-4o
	gpt4oCost := CalculateCost("gpt-4o", 1000, 1000, 0, 0)
	if floatEquals(cost, gpt4oCost, 0.000001) {
		t.Errorf("gpt-4o-legacy和gpt-4o价格应不同！legacy=$%.6f, gpt-4o=$%.6f", cost, gpt4oCost)
	}
}
