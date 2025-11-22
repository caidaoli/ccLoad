package app

import (
	"strings"
	"testing"
)

func TestSSEUsageParser_ParseMessageStart(t *testing.T) {
	// 模拟Claude API的message_start事件
	sseData := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01K9hwVdcx7dF7Cq17pZ8HLD","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5-20250929","usage":{"cache_creation_input_tokens":278,"cache_read_input_tokens":17558,"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0}

`

	parser := newSSEUsageParser()
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	// 验证usage数据
	input, output, cacheRead, cacheCreation := parser.GetUsage()

	if input != 12 {
		t.Errorf("InputTokens = %d, 期望 12", input)
	}
	if output != 1 {
		t.Errorf("OutputTokens = %d, 期望 1", output)
	}
	if cacheRead != 17558 {
		t.Errorf("CacheReadInputTokens = %d, 期望 17558", cacheRead)
	}
	if cacheCreation != 278 {
		t.Errorf("CacheCreationInputTokens = %d, 期望 278", cacheCreation)
	}
}

func TestSSEUsageParser_ParseMessageDelta(t *testing.T) {
	// 模拟message_delta事件（最终usage统计）
	sseData := `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"cache_creation_input_tokens":278,"cache_read_input_tokens":17558,"input_tokens":12,"output_tokens":73}}

event: message_stop
data: {"type":"message_stop"}

`

	parser := newSSEUsageParser()
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	// 验证usage数据
	input, output, cacheRead, cacheCreation := parser.GetUsage()

	if input != 12 {
		t.Errorf("InputTokens = %d, 期望 12", input)
	}
	if output != 73 {
		t.Errorf("OutputTokens = %d, 期望 73", output)
	}
	if cacheRead != 17558 {
		t.Errorf("CacheReadInputTokens = %d, 期望 17558", cacheRead)
	}
	if cacheCreation != 278 {
		t.Errorf("CacheCreationInputTokens = %d, 期望 278", cacheCreation)
	}
}

func TestSSEUsageParser_NoUsageData(t *testing.T) {
	// 测试没有usage数据的SSE流
	sseData := `event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

`

	parser := newSSEUsageParser()
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
		"event: mess",                                   // 第1块：事件名被切割
		"age_start\ndata: {\"message\":{\"usa",          // 第2块：JSON被切割
		"ge\":{\"input_tokens\":100,\"output_tok",       // 第3块：JSON继续
		"ens\":50}}}\n\n",                               // 第4块：事件结束
		"event: ping\ndata: {\"type\":\"ping\"}\n\n", // 第5块：完整事件
	}

	parser := newSSEUsageParser()
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
		"event: message_start\ndata: {\"",       // 在引号后切割
		"message",                               // 键名
		"\":{\"usage\"",                         // 在引号和冒号处切割
		":{\"input_tokens\":",                   // 冒号后切割
		"999}}}\n\n",                            // 数字和结束
	}

	parser := newSSEUsageParser()
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

	parser := newSSEUsageParser()
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

// ============================================================================
// 防御性测试：恶意输入
// ============================================================================

func TestSSEUsageParser_MalformedJSON(t *testing.T) {
	// 畸形JSON不应导致崩溃，应静默跳过并记录日志
	malformed := `event: message_start
data: {"message":{"usage":{"input_tokens":INVALID}}}

`

	parser := newSSEUsageParser()
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
	// 超大事件应触发保护机制
	parser := newSSEUsageParser()

	// 构造1MB+的数据
	hugeData := "event: test\ndata: " + strings.Repeat("A", maxSSEEventSize+1) + "\n\n"

	err := parser.Feed([]byte(hugeData))
	if err == nil {
		t.Error("期望返回错误（事件超过大小限制），实际成功")
	}
	if !strings.Contains(err.Error(), "exceeds max size") {
		t.Errorf("错误信息不符合预期: %v", err)
	}
}

func TestSSEUsageParser_EmptyInput(t *testing.T) {
	parser := newSSEUsageParser()
	if err := parser.Feed([]byte("")); err != nil {
		t.Fatalf("空输入不应失败: %v", err)
	}
	if err := parser.Feed(nil); err != nil {
		t.Fatalf("nil输入不应失败: %v", err)
	}
}

func TestSSEUsageParser_InvalidEventType(t *testing.T) {
	// 非message_start/message_delta事件中的usage字段应被忽略
	sseData := `event: unknown_event
data: {"usage":{"input_tokens":999}}

`

	parser := newSSEUsageParser()
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	input, _, _, _ := parser.GetUsage()
	if input != 0 {
		t.Errorf("非目标事件的usage应被忽略，实际: input=%d", input)
	}
}

func TestSSEUsageParser_ParseOpenAIResponseCompleted(t *testing.T) {
	// 模拟OpenAI Responses API (Codex)的response.completed事件
	sseData := `event: response.completed
data: {"type":"response.completed","sequence_number":28,"response":{"id":"resp_0d0d42598bd5c52c01691a963247dc81969f6ece7ebc78d882","object":"response","created_at":1763350066,"status":"completed","usage":{"input_tokens":10309,"input_tokens_details":{"cached_tokens":6016},"output_tokens":17,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":10326}}}

`

	parser := newSSEUsageParser()
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	// 验证usage数据
	input, output, cacheRead, cacheCreation := parser.GetUsage()

	if input != 10309 {
		t.Errorf("InputTokens = %d, 期望 10309", input)
	}
	if output != 17 {
		t.Errorf("OutputTokens = %d, 期望 17", output)
	}
	if cacheRead != 6016 {
		t.Errorf("CacheReadInputTokens (cached_tokens) = %d, 期望 6016", cacheRead)
	}
	if cacheCreation != 0 {
		t.Errorf("CacheCreationInputTokens = %d, 期望 0 (OpenAI不支持)", cacheCreation)
	}
}

func TestSSEUsageParser_ParseMixedEvents(t *testing.T) {
	// 测试混合Claude和OpenAI事件（虽然实际不会发生，但确保解析器健壮性）
	sseData := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":10}}}

event: response.completed
data: {"type":"response.completed","response":{"usage":{"input_tokens":200,"output_tokens":20,"input_tokens_details":{"cached_tokens":50}}}}

`

	parser := newSSEUsageParser()
	if err := parser.Feed([]byte(sseData)); err != nil {
		t.Fatalf("Feed失败: %v", err)
	}

	// 应该使用最后一个事件的数据（response.completed）
	input, output, cacheRead, _ := parser.GetUsage()

	if input != 200 {
		t.Errorf("InputTokens = %d, 期望 200 (最后一个事件)", input)
	}
	if output != 20 {
		t.Errorf("OutputTokens = %d, 期望 20 (最后一个事件)", output)
	}
	if cacheRead != 50 {
		t.Errorf("CacheReadInputTokens = %d, 期望 50 (最后一个事件)", cacheRead)
	}
}

func TestSSEUsageParser_GeminiFormat(t *testing.T) {
	// 测试Gemini SSE格式（无event类型，只有data行，使用usageMetadata字段）
	sseData := `data: {"candidates": [{"content": {"parts": [{"text": "测试文本"}],"role": "model"}}],"usageMetadata": {"promptTokenCount": 772,"candidatesTokenCount": 430,"totalTokenCount": 2332},"modelVersion": "gemini-2.5-pro"}

`

	parser := newSSEUsageParser()
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

	parser := newSSEUsageParser()
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
