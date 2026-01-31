package app

import (
	"sort"
	"strings"
	"testing"
)

func feedAndAssertUsage(t *testing.T, parser usageParser, data string, wantInput, wantOutput, wantCacheRead, wantCacheCreation int) {
	t.Helper()

	if err := parser.Feed([]byte(data)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, output, cacheRead, cacheCreation := parser.GetUsage()
	if input != wantInput {
		t.Errorf("InputTokens = %d, 期望 %d", input, wantInput)
	}
	if output != wantOutput {
		t.Errorf("OutputTokens = %d, 期望 %d", output, wantOutput)
	}
	if cacheRead != wantCacheRead {
		t.Errorf("CacheReadInputTokens = %d, 期望 %d", cacheRead, wantCacheRead)
	}
	if cacheCreation != wantCacheCreation {
		t.Errorf("CacheCreationInputTokens = %d, 期望 %d", cacheCreation, wantCacheCreation)
	}
}

func TestHasGeminiUsageFields(t *testing.T) {
	t.Parallel()

	if !hasGeminiUsageFields(map[string]any{
		"usageMetadata": map[string]any{"promptTokenCount": float64(1)},
	}) {
		t.Fatal("expected usageMetadata wrapper to be detected")
	}

	if !hasGeminiUsageFields(map[string]any{"promptTokenCount": float64(1)}) {
		t.Fatal("expected promptTokenCount to be detected")
	}

	if !hasGeminiUsageFields(map[string]any{"candidatesTokenCount": float64(1)}) {
		t.Fatal("expected candidatesTokenCount to be detected")
	}

	if hasGeminiUsageFields(map[string]any{}) {
		t.Fatal("expected empty map to not be detected as gemini usage")
	}
}

func TestGetUsageKeys(t *testing.T) {
	t.Parallel()

	keys := getUsageKeys(map[string]any{
		"b": float64(2),
		"a": float64(1),
	})
	sort.Strings(keys)
	if strings.Join(keys, ",") != "a,b" {
		t.Fatalf("unexpected keys: %v", keys)
	}
}

func TestSSEUsageParser_ParseMessageStart(t *testing.T) {
	// 模拟Claude API的message_start事件
	sseData := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01K9hwVdcx7dF7Cq17pZ8HLD","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5-20250929","usage":{"cache_creation_input_tokens":278,"cache_read_input_tokens":17558,"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0}

	`

	feedAndAssertUsage(t, newSSEUsageParser("anthropic"), sseData, 12, 1, 17558, 278)
}

func TestSSEUsageParser_ParseMessageDelta(t *testing.T) {
	// 模拟message_delta事件（最终usage统计）
	sseData := `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"cache_creation_input_tokens":278,"cache_read_input_tokens":17558,"input_tokens":12,"output_tokens":73}}

event: message_stop
data: {"type":"message_stop"}

	`

	feedAndAssertUsage(t, newSSEUsageParser("anthropic"), sseData, 12, 73, 17558, 278)
}

func TestSSEUsageParser_NoUsageData(t *testing.T) {
	// 测试没有usage数据的SSE流
	sseData := `event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

`

	parser := newSSEUsageParser("anthropic") // 测试使用默认平台
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	// 验证usage数据为0
	input, output, cacheRead, cacheCreation := parser.GetUsage()

	if input != 0 || output != 0 || cacheRead != 0 || cacheCreation != 0 {
		t.Errorf("期望所有token统计为0，实际: input=%d, output=%d, cacheRead=%d, cacheCreation=%d",
			input, output, cacheRead, cacheCreation)
	}
}

// ============================================================================
// 边界测试：分块读取（真实SSE流场景）
// ============================================================================

func TestSSEUsageParser_ChunkedReading(t *testing.T) {
	// 真实场景：SSE流分多次到达，可能在任意位置切割
	chunks := []string{
		"event: mess",                                // 第1块：事件名被切割
		"age_start\ndata: {\"message\":{\"usa",       // 第2块：JSON被切割
		"ge\":{\"input_tokens\":100,\"output_tok",    // 第3块：JSON继续
		"ens\":50}}}\n\n",                            // 第4块：事件结束
		"event: ping\ndata: {\"type\":\"ping\"}\n\n", // 第5块：完整事件
	}

	parser := newSSEUsageParser("anthropic") // 测试使用默认平台
	for i, chunk := range chunks {
		if err := parser.Feed([]byte(chunk)); err != nil {
			t.Fatalf("Feed第%d块失败: %v", i+1, err)
		}
	}

	input, output, _, _ := parser.GetUsage()
	if input != 100 {
		t.Errorf("InputTokens = %d, 期望 100", input)
	}
	if output != 50 {
		t.Errorf("OutputTokens = %d, 期望 50", output)
	}
}

func TestSSEUsageParser_JSONBoundaryCut(t *testing.T) {
	// 极端场景：JSON在引号、冒号、花括号等位置被切割
	chunks := []string{
		"event: message_start\ndata: {\"", // 在引号后切割
		"message",                         // 键名
		"\":{\"usage\"",                   // 在引号和冒号处切割
		":{\"input_tokens\":",             // 冒号后切割
		"999}}}\n\n",                      // 数字和结束
	}

	parser := newSSEUsageParser("anthropic") // 测试使用默认平台
	for _, chunk := range chunks {
		if err := parser.Feed([]byte(chunk)); err != nil {
			t.Fatalf("Feed失败: %v (chunk: %s)", err, chunk)
		}
	}

	input, _, _, _ := parser.GetUsage()
	if input != 999 {
		t.Errorf("InputTokens = %d, 期望 999", input)
	}
}

func TestSSEUsageParser_MultipleEvents(t *testing.T) {
	// 测试多个usage事件的累积更新（message_delta会覆盖output_tokens）
	events := []string{
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n",
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":20}}\n\n",
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":30}}\n\n", // 最终值
	}

	parser := newSSEUsageParser("anthropic") // 测试使用默认平台
	for _, event := range events {
		if err := parser.Feed([]byte(event)); err != nil {
			t.Fatalf("Feed失败: %v", err)
		}
	}

	input, output, _, _ := parser.GetUsage()
	if input != 10 {
		t.Errorf("InputTokens = %d, 期望 10", input)
	}
	if output != 30 { // 被最后一次message_delta覆盖
		t.Errorf("OutputTokens = %d, 期望 30", output)
	}
}

func TestSSEUsageParser_MessageDeltaWithZeroInputTokens(t *testing.T) {
	// 测试某些中间代理（如anyrouter）在message_delta中添加input_tokens:0的场景
	// 期望：input_tokens应保留message_start中的值，不被0覆盖
	events := []string{
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":2011,\"output_tokens\":1}}}\n\n",
		"event: message_delta\ndata: {\"usage\":{\"input_tokens\":0,\"output_tokens\":144}}\n\n",
	}

	parser := newSSEUsageParser("anthropic")
	for _, event := range events {
		if err := parser.Feed([]byte(event)); err != nil {
			t.Fatalf("Feed失败: %v", err)
		}
	}

	input, output, _, _ := parser.GetUsage()
	if input != 2011 {
		t.Errorf("InputTokens = %d, 期望 2011（不应被message_delta中的0覆盖）", input)
	}
	if output != 144 {
		t.Errorf("OutputTokens = %d, 期望 144", output)
	}
}

// ============================================================================
// 防御性测试：恶意输入
// ============================================================================

func TestSSEUsageParser_MalformedJSON(t *testing.T) {
	// 畸形JSON不应导致崩溃，应静默跳过并记录日志
	malformed := `event: message_start
data: {"message":{"usage":{"input_tokens":INVALID}}}

`

	parser := newSSEUsageParser("anthropic") // 测试使用默认平台
	// 不应panic
	if err := parser.Feed([]byte(malformed)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	// usage应该为0（解析失败）
	input, _, _, _ := parser.GetUsage()
	if input != 0 {
		t.Errorf("畸形JSON不应解析出token数据，实际: input=%d", input)
	}
}

func TestSSEUsageParser_OversizedEvent(t *testing.T) {
	// 超大事件应触发保护机制但不中断流传输
	parser := newSSEUsageParser("anthropic") // 测试使用默认平台

	// 构造1MB+的数据
	hugeData := "event: test\ndata: " + strings.Repeat("A", maxSSEEventSize+1) + "\n\n"

	err := parser.Feed([]byte(hugeData))
	if err != nil {
		t.Errorf("不应返回错误以保证流传输继续，实际返回: %v", err)
	}
	if !parser.oversized {
		t.Error("应设置oversized标志以停止后续usage解析")
	}

	// 验证后续Feed不再处理
	err2 := parser.Feed([]byte("event: test\n\n"))
	if err2 != nil {
		t.Errorf("oversized后的Feed应返回nil: %v", err2)
	}
}

func TestSSEUsageParser_EmptyInput(t *testing.T) {
	parser := newSSEUsageParser("anthropic") // 测试使用默认平台
	if err := parser.Feed([]byte("")); err != nil {
		t.Fatalf("空输入不应失败: %v", err)
	}
	if err := parser.Feed(nil); err != nil {
		t.Fatalf("nil输入不应失败: %v", err)
	}
}

func TestSSEUsageParser_InvalidEventType(t *testing.T) {
	// [INFO] 黑名单模式（2025-12-07）：未知事件类型也会尝试提取usage
	// 原因：anyrouter等聚合服务使用非标准事件类型（如"."），需要兼容
	sseData := `event: unknown_event
data: {"usage":{"input_tokens":999}}

`

	parser := newSSEUsageParser("anthropic")
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, _, _, _ := parser.GetUsage()
	// 新预期：未知事件类型也会被解析
	if input != 999 {
		t.Errorf("黑名单模式下应提取usage，实际: input=%d, 期望: 999", input)
	}
}

func TestSSEUsageParser_ParseCodexResponseCompleted(t *testing.T) {
	// 模拟OpenAI Responses API (Codex)的response.completed事件
	// Codex使用input_tokens + input_tokens_details.cached_tokens格式
	// [INFO] 重构后：GetUsage()返回归一化的billable input (10309-6016=4293)
	sseData := `event: response.completed
data: {"type":"response.completed","sequence_number":28,"response":{"id":"resp_0d0d42598bd5c52c01691a963247dc81969f6ece7ebc78d882","object":"response","created_at":1763350066,"status":"completed","usage":{"input_tokens":10309,"input_tokens_details":{"cached_tokens":6016},"output_tokens":17,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":10326}}}

	`

	feedAndAssertUsage(t, newSSEUsageParser("codex"), sseData, 4293, 17, 6016, 0)
}

func TestSSEUsageParser_OpenAIChatCompletionsSSE(t *testing.T) {
	// 测试OpenAI Chat Completions API的SSE流式响应
	// OpenAI Chat使用prompt_tokens + completion_tokens格式
	// [INFO] 重构后：GetUsage()返回归一化的billable input (200-100=100)
	sseData := `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}],"usage":null}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"测试"},"logprobs":null,"finish_reason":null}],"usage":null}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-4o","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":200,"completion_tokens":50,"total_tokens":250,"prompt_tokens_details":{"cached_tokens":100}}}

data: [DONE]

	`

	feedAndAssertUsage(t, newSSEUsageParser("openai"), sseData, 100, 50, 100, 0)
}

func TestSSEUsageParser_GeminiFormat(t *testing.T) {
	// 测试Gemini SSE格式（无event类型，只有data行，使用usageMetadata字段）
	sseData := `data: {"candidates": [{"content": {"parts": [{"text": "测试文本"}],"role": "model"}}],"usageMetadata": {"promptTokenCount": 772,"candidatesTokenCount": 430,"totalTokenCount": 2332},"modelVersion": "gemini-2.5-pro"}

`

	parser := newSSEUsageParser("gemini") // Gemini平台测试
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, output, _, _ := parser.GetUsage()

	if input != 772 {
		t.Errorf("InputTokens = %d, 期望 772 (Gemini promptTokenCount)", input)
	}
	if output != 430 {
		t.Errorf("OutputTokens = %d, 期望 430 (Gemini candidatesTokenCount)", output)
	}
}

func TestSSEUsageParser_GeminiMultipleChunks(t *testing.T) {
	// 测试Gemini多个SSE消息（usageMetadata在每个chunk中递增）
	chunks := []string{
		`data: {"candidates": [{"content": {"parts": [{"text": "第一部分"}]}}],"usageMetadata": {"promptTokenCount": 100,"candidatesTokenCount": 10}}` + "\n\n",
		`data: {"candidates": [{"content": {"parts": [{"text": "第二部分"}]}}],"usageMetadata": {"promptTokenCount": 100,"candidatesTokenCount": 50}}` + "\n\n",
		`data: {"candidates": [{"content": {"parts": [{"text": "完成"}]}}],"usageMetadata": {"promptTokenCount": 100,"candidatesTokenCount": 120},"modelVersion": "gemini-2.5-pro"}` + "\n\n",
	}

	parser := newSSEUsageParser("gemini") // Gemini平台测试
	for _, chunk := range chunks {
		if err := parser.Feed([]byte(chunk)); err != nil {
			t.Fatalf("Feed失败: %v", err)
		}
	}

	input, output, _, _ := parser.GetUsage()

	// 应该使用最后一个消息的值
	if input != 100 {
		t.Errorf("InputTokens = %d, 期望 100", input)
	}
	if output != 120 {
		t.Errorf("OutputTokens = %d, 期望 120 (最终值)", output)
	}
}

func TestSSEUsageParser_OpenAIChatCompletionsFormat(t *testing.T) {
	// 测试OpenAI Chat Completions API格式（使用prompt_tokens/completion_tokens）
	// 注意：Chat Completions通常返回普通JSON而非SSE，但这里测试解析器的兼容性
	sseData := `data: {"id":"chatcmpl-123","object":"chat.completion","created":1677652288,"model":"gpt-4o","usage":{"prompt_tokens":150,"completion_tokens":80,"total_tokens":230}}

`

	parser := newSSEUsageParser("openai") // OpenAI平台测试
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, output, _, _ := parser.GetUsage()

	if input != 150 {
		t.Errorf("InputTokens = %d, 期望 150 (OpenAI prompt_tokens)", input)
	}
	if output != 80 {
		t.Errorf("OutputTokens = %d, 期望 80 (OpenAI completion_tokens)", output)
	}
}

func TestSSEUsageParser_OpenAIChatCompletionsWithCache(t *testing.T) {
	// 测试OpenAI Chat Completions API带缓存的格式（prompt_tokens_details.cached_tokens）
	// [INFO] 重构后：GetUsage()返回归一化的billable input (300-200=100)
	sseData := `data: {"id":"chatcmpl-456","object":"chat.completion","created":1677652288,"model":"gpt-4o","usage":{"prompt_tokens":300,"completion_tokens":120,"total_tokens":420,"prompt_tokens_details":{"cached_tokens":200,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0}}}

	`

	feedAndAssertUsage(t, newSSEUsageParser("openai"), sseData, 100, 120, 200, 0)
}

func TestJSONUsageParser_OpenAIChatCompletionsFormat(t *testing.T) {
	// 测试普通JSON格式的OpenAI Chat Completions响应
	jsonData := `{"id":"chatcmpl-789","object":"chat.completion","created":1677652288,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"测试响应"},"finish_reason":"stop"}],"usage":{"prompt_tokens":25,"completion_tokens":10,"total_tokens":35}}`

	parser := newJSONUsageParser("openai") // OpenAI平台测试
	if err := parser.Feed([]byte(jsonData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, output, _, _ := parser.GetUsage()

	if input != 25 {
		t.Errorf("InputTokens = %d, 期望 25 (OpenAI prompt_tokens)", input)
	}
	if output != 10 {
		t.Errorf("OutputTokens = %d, 期望 10 (OpenAI completion_tokens)", output)
	}
}

func TestJSONUsageParser_OpenAIChatCompletionsWithCacheFormat(t *testing.T) {
	// 测试带缓存的OpenAI Chat Completions JSON响应
	// [INFO] 重构后：GetUsage()返回归一化的billable input (500-350=150)
	jsonData := `{"id":"chatcmpl-abc","object":"chat.completion","created":1677652288,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"测试响应"},"finish_reason":"stop"}],"usage":{"prompt_tokens":500,"completion_tokens":200,"total_tokens":700,"prompt_tokens_details":{"cached_tokens":350,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0}}}`

	feedAndAssertUsage(t, newJSONUsageParser("openai"), jsonData, 150, 200, 350, 0)
}

func TestSSEUsageParser_GeminiThoughtsTokenCount(t *testing.T) {
	// 测试Gemini思考token（thoughtsTokenCount）应计入输出token
	sseData := `data: {"candidates": [{"content": {"parts": [{"text": "回答"}]}}],"usageMetadata": {"promptTokenCount": 100,"candidatesTokenCount": 50,"totalTokenCount": 250,"thoughtsTokenCount": 100}}

`

	parser := newSSEUsageParser("gemini")
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, output, _, _ := parser.GetUsage()

	if input != 100 {
		t.Errorf("InputTokens = %d, 期望 100 (Gemini promptTokenCount)", input)
	}
	// 输出token = candidatesTokenCount(50) + thoughtsTokenCount(100) = 150
	if output != 150 {
		t.Errorf("OutputTokens = %d, 期望 150 (candidatesTokenCount + thoughtsTokenCount)", output)
	}
}

func TestSSEUsageParser_GeminiCandidatesZeroFallback(t *testing.T) {
	// 测试当candidatesTokenCount为0时，从totalTokenCount推算输出token
	// 某些Gemini模型的流式响应中candidatesTokenCount始终为0
	sseData := `data: {"candidates": [{"content": {"parts": []}}],"usageMetadata": {"promptTokenCount": 100,"candidatesTokenCount": 0,"totalTokenCount": 250,"thoughtsTokenCount": 0}}

`

	parser := newSSEUsageParser("gemini")
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, output, _, _ := parser.GetUsage()

	if input != 100 {
		t.Errorf("InputTokens = %d, 期望 100 (Gemini promptTokenCount)", input)
	}
	// 输出token = totalTokenCount(250) - promptTokenCount(100) = 150
	if output != 150 {
		t.Errorf("OutputTokens = %d, 期望 150 (totalTokenCount - promptTokenCount)", output)
	}
}

func TestSSEUsageParser_GeminiThoughtsWithZeroCandidates(t *testing.T) {
	// 测试当candidatesTokenCount为0但thoughtsTokenCount有值时
	// 应该使用thoughtsTokenCount，不触发fallback
	sseData := `data: {"candidates": [{"content": {"parts": []}}],"usageMetadata": {"promptTokenCount": 100,"candidatesTokenCount": 0,"totalTokenCount": 300,"thoughtsTokenCount": 150}}

`

	parser := newSSEUsageParser("gemini")
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, output, _, _ := parser.GetUsage()

	if input != 100 {
		t.Errorf("InputTokens = %d, 期望 100 (Gemini promptTokenCount)", input)
	}
	// 输出token = candidatesTokenCount(0) + thoughtsTokenCount(150) = 150
	// 不应该触发fallback（因为outputTokens > 0）
	if output != 150 {
		t.Errorf("OutputTokens = %d, 期望 150 (thoughtsTokenCount)", output)
	}
}

func TestSSEUsageParser_GeminiCachedContentTokenCount(t *testing.T) {
	// 测试Gemini缓存token（cachedContentTokenCount）
	// Gemini API上下文缓存会返回此字段
	sseData := `data: {"candidates": [{"content": {"parts": [{"text": "回答"}]}}],"usageMetadata": {"promptTokenCount": 1000,"candidatesTokenCount": 50,"totalTokenCount": 1050,"cachedContentTokenCount": 800}}

`

	parser := newSSEUsageParser("gemini")
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, output, cacheRead, _ := parser.GetUsage()

	if input != 200 {
		t.Errorf("InputTokens = %d, 期望 200 (promptTokenCount 1000 - cachedContentTokenCount 800)", input)
	}
	if output != 50 {
		t.Errorf("OutputTokens = %d, 期望 50 (candidatesTokenCount)", output)
	}
	if cacheRead != 800 {
		t.Errorf("CacheReadInputTokens = %d, 期望 800 (cachedContentTokenCount)", cacheRead)
	}
}

// TestJSONUsageParser_CacheCreationDetailed_5mOnly 验证非流式JSON响应解析5m缓存细分字段
// 新增2025-12：支持 cache_creation.ephemeral_5m_input_tokens
func TestJSONUsageParser_CacheCreationDetailed_5mOnly(t *testing.T) {
	jsonData := `{
		"usage": {
			"input_tokens": 12,
			"output_tokens": 73,
			"cache_read_input_tokens": 17558,
			"cache_creation_input_tokens": 278,
			"cache_creation": {
				"ephemeral_5m_input_tokens": 278,
				"ephemeral_1h_input_tokens": 0
			}
		}
	}`

	parser := newJSONUsageParser("anthropic")
	if err := parser.Feed([]byte(jsonData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	// 验证 GetUsage() 返回的兼容字段
	input, output, cacheRead, cacheCreation := parser.GetUsage()
	if input != 12 || output != 73 || cacheRead != 17558 || cacheCreation != 278 {
		t.Errorf("GetUsage() 返回错误: input=%d, output=%d, cacheRead=%d, cacheCreation=%d",
			input, output, cacheRead, cacheCreation)
	}

	// 验证细分字段（通过类型断言访问）
	if parser.Cache5mInputTokens != 278 {
		t.Errorf("Cache5mInputTokens = %d, 期望 278", parser.Cache5mInputTokens)
	}
	if parser.Cache1hInputTokens != 0 {
		t.Errorf("Cache1hInputTokens = %d, 期望 0", parser.Cache1hInputTokens)
	}

	t.Logf("[INFO] 非流式JSON响应5m缓存解析正确: cache_5m=%d, cache_1h=%d",
		parser.Cache5mInputTokens, parser.Cache1hInputTokens)
}

// TestJSONUsageParser_CacheCreationDetailed_Mixed 验证非流式JSON响应解析5m+1h混合缓存
func TestJSONUsageParser_CacheCreationDetailed_Mixed(t *testing.T) {
	jsonData := `{
		"usage": {
			"input_tokens": 50,
			"output_tokens": 200,
			"cache_read_input_tokens": 5000,
			"cache_creation_input_tokens": 500,
			"cache_creation": {
				"ephemeral_5m_input_tokens": 300,
				"ephemeral_1h_input_tokens": 200
			}
		}
	}`

	parser := newJSONUsageParser("anthropic")
	if err := parser.Feed([]byte(jsonData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	// 验证 GetUsage() 返回的兼容字段（应该是5m+1h总和）
	_, _, _, cacheCreation := parser.GetUsage()
	if cacheCreation != 500 {
		t.Errorf("CacheCreationInputTokens = %d, 期望 500 (300+200)", cacheCreation)
	}

	// 验证细分字段
	if parser.Cache5mInputTokens != 300 {
		t.Errorf("Cache5mInputTokens = %d, 期望 300", parser.Cache5mInputTokens)
	}
	if parser.Cache1hInputTokens != 200 {
		t.Errorf("Cache1hInputTokens = %d, 期望 200", parser.Cache1hInputTokens)
	}

	t.Logf("[INFO] 非流式JSON响应混合缓存解析正确: total=%d (5m=%d + 1h=%d)",
		cacheCreation, parser.Cache5mInputTokens, parser.Cache1hInputTokens)
}

// TestSSEUsageParser_CacheCreationDetailed_1hOnly 验证流式SSE响应解析1h缓存
func TestSSEUsageParser_CacheCreationDetailed_1hOnly(t *testing.T) {
	sseData := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":500,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":500}}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":100}}

`

	parser := newSSEUsageParser("anthropic")
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	// 验证细分字段
	if parser.Cache5mInputTokens != 0 {
		t.Errorf("Cache5mInputTokens = %d, 期望 0", parser.Cache5mInputTokens)
	}
	if parser.Cache1hInputTokens != 500 {
		t.Errorf("Cache1hInputTokens = %d, 期望 500", parser.Cache1hInputTokens)
	}
	if parser.CacheCreationInputTokens != 500 {
		t.Errorf("CacheCreationInputTokens = %d, 期望 500 (兼容字段)", parser.CacheCreationInputTokens)
	}

	t.Logf("[INFO] 流式SSE响应1h缓存解析正确: cache_5m=%d, cache_1h=%d",
		parser.Cache5mInputTokens, parser.Cache1hInputTokens)
}
