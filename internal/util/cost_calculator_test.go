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
	// Input: 1000 × $15.00 / 1M = $0.015
	// Output: 2000 × $75.00 / 1M = $0.150
	// Total: $0.165
	expected := 0.165
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
		{"claude-3-opus-20240229", 0.165},    // 1000×$15/1M + 2000×$75/1M = 0.015 + 0.15
		{"claude-3-sonnet-20240229", 0.033},  // 1000×$3/1M + 2000×$15/1M = 0.003 + 0.03
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
		{"gpt-5.1-codex", 4293, 17, 6016, 0.006288}, // 4293×1.25/1M + 17×10/1M + 6016×(1.25×0.1)/1M
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
	cost := CalculateCost("claude-opus-4-5-20251101", 8, 269, 53660, 816)

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
	opusCacheCost := CalculateCost("claude-opus-4-5", 0, 0, cacheTokens, 0)
	opusInputCost := CalculateCost("claude-opus-4-5", cacheTokens, 0, 0, 0)
	opusRatio := opusCacheCost / opusInputCost

	// Sonnet: 缓存读取 = 输入价格 × 0.1（90%折扣）
	sonnetCacheCost := CalculateCost("claude-sonnet-4-5", 0, 0, cacheTokens, 0)
	sonnetInputCost := CalculateCost("claude-sonnet-4-5", cacheTokens, 0, 0, 0)
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
		t.Logf("[INFO] gpt-4o-legacy带后缀正确匹配: $%.6f", cost)
	}

	// 验证不会误匹配到gpt-4o
	gpt4oCost := CalculateCost("gpt-4o", 1000, 1000, 0, 0)
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

// TestCalculateCost_BackwardCompatibility 验证旧版本CalculateCost的兼容性
// 确保旧代码调用CalculateCost(model, in, out, cacheRead, cacheCreation)时
// cacheCreation被当作5m缓存处理
func TestCalculateCost_BackwardCompatibility(t *testing.T) {
	model := "claude-sonnet-4-5"
	cacheTokens := 1000

	// 旧版本调用: CalculateCost(model, 0, 0, 0, cacheCreation)
	oldWay := CalculateCost(model, 0, 0, 0, cacheTokens)

	// 新版本调用: CalculateCostDetailed(model, 0, 0, 0, cache5m, 0)
	newWay := CalculateCostDetailed(model, 0, 0, 0, cacheTokens, 0)

	// 应该完全相同
	if !floatEquals(oldWay, newWay, 0.000001) {
		t.Errorf("向后兼容性问题: CalculateCost=$%.6f, CalculateCostDetailed=$%.6f", oldWay, newWay)
	}

	t.Logf("[INFO] 向后兼容性测试通过: $%.6f", oldWay)
}

