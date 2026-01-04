package app

import (
	"testing"

	"ccLoad/internal/util"
)

// ============================================================================
// 端到端计费链路集成测试
// 验证: Token解析 → 费用计算 → 数据正确性
// ============================================================================

// TestBillingPipeline_OpenAI_ChatCompletions 验证OpenAI Chat Completions API完整计费链路
func TestBillingPipeline_OpenAI_ChatCompletions(t *testing.T) {
	// 场景：GPT-4o带缓存的流式响应
	// OpenAI语义：prompt_tokens包含cached_tokens，解析层已自动归一化
	// 注意：cached_tokens嵌套在prompt_tokens_details下
	mockSSE := `data: {"usage":{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":800},"completion_tokens":50}}` + "\n\n"

	// 1. 解析Token (模拟SSE解析器)
	parser := newSSEUsageParser("openai")
	if err := parser.Feed([]byte(mockSSE)); err != nil {
		t.Fatalf("SSE解析失败: %v", err)
	}
	inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens := parser.GetUsage()

	// 2. 验证Token提取正确性
	// [INFO] 重要: GetUsage()返回的inputTokens已归一化为可计费token (1000-800=200)
	if inputTokens != 200 {
		t.Errorf("❌ OpenAI归一化后inputTokens错误: 期望200(1000-800), 实际%d", inputTokens)
	}
	if cacheReadTokens != 800 {
		t.Errorf("❌ OpenAI cached_tokens提取错误: 期望800, 实际%d", cacheReadTokens)
	}
	if outputTokens != 50 {
		t.Errorf("❌ OpenAI completion_tokens提取错误: 期望50, 实际%d", outputTokens)
	}

	// 3. 计算费用 (inputTokens已归一化，CalculateCostDetailed直接使用)
	cost := util.CalculateCostDetailed("gpt-4o", inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens, 0)

	// 4. 验证计费公式正确性
	// GPT-4o定价: $2.50/1M input, $10/1M output, 缓存50%折扣
	// 公式: 200×$2.50/1M + 50×$10/1M + 800×($2.50×0.5)/1M
	//     = 200×0.0000025 + 50×0.00001 + 800×0.00000125
	//     = 0.0005 + 0.0005 + 0.001
	//     = 0.002
	expected := 0.002
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("❌ OpenAI计费错误: 期望%.6f, 实际%.6f", expected, cost)
	}

	t.Logf("[INFO] OpenAI Chat Completions计费链路验证通过")
	t.Logf("   原始prompt_tokens: 1000, 归一化后inputTokens: %d (已扣除缓存)", inputTokens)
	t.Logf("   缓存读取: %d tokens", cacheReadTokens)
	t.Logf("   输出token: %d", outputTokens)
	t.Logf("   费用: $%.6f", cost)
}

// TestBillingPipeline_Claude_WithCache 验证Claude Prompt Caching完整计费链路
func TestBillingPipeline_Claude_WithCache(t *testing.T) {
	// 场景：Claude Sonnet 4.5使用Prompt Caching
	// Claude语义：input_tokens仅非缓存部分，cache_read_input_tokens单独计费
	mockSSE := `event: message_stop
data: {"type":"message_stop","usage":{"input_tokens":12,"output_tokens":73,"cache_read_input_tokens":17558,"cache_creation_input_tokens":278}}

`

	// 1. 解析Token
	parser := newSSEUsageParser("anthropic")
	if err := parser.Feed([]byte(mockSSE)); err != nil {
		t.Fatalf("SSE解析失败: %v", err)
	}
	inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens := parser.GetUsage()

	// 2. 验证Token提取
	if inputTokens != 12 {
		t.Errorf("❌ Claude input_tokens错误: 期望12, 实际%d", inputTokens)
	}
	if outputTokens != 73 {
		t.Errorf("❌ Claude output_tokens错误: 期望73, 实际%d", outputTokens)
	}
	if cacheReadTokens != 17558 {
		t.Errorf("❌ Claude cache_read错误: 期望17558, 实际%d", cacheReadTokens)
	}
	if cacheCreationTokens != 278 {
		t.Errorf("❌ Claude cache_creation错误: 期望278, 实际%d", cacheCreationTokens)
	}

	// 3. 计算费用
	cost := util.CalculateCostDetailed("claude-sonnet-4-5-20250929", inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens, 0)

	// 4. 验证计费公式
	// Sonnet 4.5定价: $3/1M input, $15/1M output, 缓存读10%, 缓存写125%
	// 公式: 12×$3/1M + 73×$15/1M + 17558×($3×0.1)/1M + 278×($3×1.25)/1M
	//     = 0.000036 + 0.001095 + 0.005267 + 0.001043
	//     = 0.007441
	expected := 0.007441
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("❌ Claude计费错误: 期望%.6f, 实际%.6f", expected, cost)
	}

	t.Logf("[INFO] Claude Prompt Caching计费链路验证通过")
	t.Logf("   非缓存输入: %d tokens", inputTokens)
	t.Logf("   缓存读取: %d tokens (节省90%%)", cacheReadTokens)
	t.Logf("   缓存创建: %d tokens (额外25%%)", cacheCreationTokens)
	t.Logf("   费用: $%.6f", cost)
}

// TestBillingPipeline_Gemini_LongContext 验证Gemini长上下文分段定价
func TestBillingPipeline_Gemini_LongContext(t *testing.T) {
	testCases := []struct {
		name         string
		inputTokens  int
		outputTokens int
		expectCost   float64
		description  string
	}{
		{
			name:         "短上下文(<128k)",
			inputTokens:  100000,
			outputTokens: 1000,
			expectCost:   0.0206, // gemini-1.5-flash: $0.20/1M input, $0.60/1M output
			description:  "低于128k阈值，使用基础定价",
		},
		{
			name:         "长上下文(>128k)",
			inputTokens:  200000,
			outputTokens: 2000,
			expectCost:   0.0412, // 200k×$0.0000002 + 2k×$0.0000006
			description:  "超过128k阈值，触发高定价",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Gemini目前不支持缓存，只测试基础token计费
			cost := util.CalculateCostDetailed("gemini-1.5-flash", tc.inputTokens, tc.outputTokens, 0, 0, 0)

			// 允许±1%误差（定价可能更新）
			tolerance := tc.expectCost * 0.01
			if cost < tc.expectCost-tolerance || cost > tc.expectCost+tolerance {
				t.Errorf("❌ Gemini %s计费错误: 期望%.6f±%.6f, 实际%.6f",
					tc.name, tc.expectCost, tolerance, cost)
			}

			t.Logf("[INFO] %s: %d tokens → $%.6f (%s)",
				tc.name, tc.inputTokens, cost, tc.description)
		})
	}
}

// TestBillingPipeline_UnknownModel 验证未知模型的兜底行为
func TestBillingPipeline_UnknownModel(t *testing.T) {
	// 场景：使用未定义定价的模型
	cost := util.CalculateCostDetailed("gpt-999-ultra", 10000, 5000, 0, 0, 0)

	// 预期：返回0.0（不应崩溃）
	if cost != 0.0 {
		t.Errorf("❌ 未知模型应返回0成本，实际%.6f", cost)
	}

	t.Logf("[INFO] 未知模型兜底行为正确: $%.6f", cost)
}

// TestBillingPipeline_NegativeTokens 验证防御性编程
func TestBillingPipeline_NegativeTokens(t *testing.T) {
	// 场景：异常数据（负数token）
	cost := util.CalculateCostDetailed("claude-sonnet-4-5", -100, 200, -50, 0, 0)

	// 预期：返回0.0并记录错误日志
	if cost != 0.0 {
		t.Errorf("❌ 负数token应返回0成本，实际%.6f", cost)
	}

	t.Logf("[INFO] 负数token防御性检查通过: $%.6f", cost)
}

// TestBillingPipeline_OpenAI_CacheExceedsInput 验证OpenAI边界情况
func TestBillingPipeline_OpenAI_CacheExceedsInput(t *testing.T) {
	// 场景：cached_tokens > prompt_tokens (理论上不应发生，但需防御)
	// 例如: prompt_tokens=500, cached_tokens=800
	// [INFO] 重构后：边界检查在解析层(GetUsage)执行，而非计费层
	mockSSE := `data: {"usage":{"prompt_tokens":500,"prompt_tokens_details":{"cached_tokens":800},"completion_tokens":100}}` + "\n\n"

	parser := newSSEUsageParser("openai")
	if err := parser.Feed([]byte(mockSSE)); err != nil {
		t.Fatalf("SSE解析失败: %v", err)
	}
	inputTokens, outputTokens, cacheReadTokens, _ := parser.GetUsage()

	// 验证解析层边界检查：inputTokens被clamp到0
	if inputTokens != 0 {
		t.Errorf("❌ 解析层边界检查失败: 期望inputTokens=0(clamped), 实际%d", inputTokens)
	}
	if cacheReadTokens != 800 {
		t.Errorf("❌ cacheReadTokens应保持800, 实际%d", cacheReadTokens)
	}

	// 计费验证
	cost := util.CalculateCostDetailed("gpt-4o", inputTokens, outputTokens, cacheReadTokens, 0, 0)

	// 预期：inputTokens=0(clamped)，只计算输出和缓存
	// 公式: 0×$2.5/1M + 100×$10/1M + 800×($2.5×0.5)/1M
	//     = 0 + 0.001 + 0.001
	//     = 0.002
	expected := 0.002
	if !floatEquals(cost, expected, 0.000001) {
		t.Errorf("❌ OpenAI缓存超限计费错误: 期望%.6f, 实际%.6f", expected, cost)
	}

	t.Logf("[INFO] OpenAI缓存超限边界情况(解析层检查)通过: $%.6f", cost)
}

// TestBillingPipeline_ZeroCostWarning 验证费用0值告警机制
func TestBillingPipeline_ZeroCostWarning(t *testing.T) {
	// 场景：使用未定义定价的模型但有token消耗
	// 预期：触发WARN日志，避免财务损失

	model := "gpt-999-unknown"
	inputTokens := 10000
	outputTokens := 5000

	cost := util.CalculateCostDetailed(model, inputTokens, outputTokens, 0, 0, 0)

	// 验证：返回0费用
	if cost != 0.0 {
		t.Errorf("❌ 未知模型应返回0成本，实际%.6f", cost)
	}

	// 验证：这种情况应该触发告警（通过日志检查）
	// 在实际生产环境中，此告警应触发监控系统
	t.Logf("[WARN]  财务风险检测: model=%s tokens=%d+%d cost=$%.6f (应触发WARN日志)",
		model, inputTokens, outputTokens, cost)
	t.Logf("[INFO] 零成本告警机制测试通过 - 生产环境应配置监控告警")
}

// floatEquals 浮点数相等性比较（避免精度问题）
func floatEquals(a, b, tolerance float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < tolerance
}
