package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
)

// ============================================================================
// SSE Usage 解析器 (重构版 - 遵循SRP)
// ============================================================================

// sseUsageParser SSE流式响应的usage数据解析器
// 设计原则（SRP）：仅负责从SSE事件流中提取token统计信息，不负责I/O
// 采用增量解析避免重复扫描（O(n²) → O(n)）
type usageAccumulator struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
}

type sseUsageParser struct {
	usageAccumulator

	// 内部状态（增量解析）
	buffer      bytes.Buffer // 未完成的数据缓冲区
	bufferSize  int          // 当前缓冲区大小
	eventType   string       // 当前正在解析的事件类型（跨Feed保存）
	dataLines   []string     // 当前事件的data行（跨Feed保存）
	oversized   bool         // 标记是否超出大小限制（停止解析但不中断流传输）
	channelType string       // 渠道类型(anthropic/openai/codex/gemini),用于精确平台判断

	// ✅ 新增：存储SSE流中检测到的error事件（用于1308等错误的延迟处理）
	lastError []byte // 最后一个error事件的完整JSON（data字段内容）
}

type jsonUsageParser struct {
	usageAccumulator
	buffer      bytes.Buffer
	truncated   bool
	channelType string // 渠道类型(anthropic/openai/codex/gemini),用于精确平台判断
}

type usageParser interface {
	Feed([]byte) error
	GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int)
	GetLastError() []byte // ✅ 新增：返回SSE流中检测到的最后一个error事件（用于1308等错误的延迟处理）
	GetReceivedData() []byte // ✅ 新增：返回接收到的原始数据（用于诊断流不完整问题）
}

const (
	// maxSSEEventSize SSE事件最大尺寸（防止内存耗尽攻击）
	maxSSEEventSize = 1 << 20 // 1MB

	// maxUsageBodySize 用于普通JSON响应 usage 提取时的最大缓存（防止内存过大）
	maxUsageBodySize = 1 << 20 // 1MB
)

// newSSEUsageParser 创建SSE usage解析器
// channelType: 渠道类型(anthropic/openai/codex/gemini),用于精确识别平台usage格式
func newSSEUsageParser(channelType string) *sseUsageParser {
	return &sseUsageParser{channelType: channelType}
}

// newJSONUsageParser 创建JSON响应的usage解析器
// channelType: 渠道类型(anthropic/openai/codex/gemini),用于精确识别平台usage格式
func newJSONUsageParser(channelType string) *jsonUsageParser {
	return &jsonUsageParser{channelType: channelType}
}

// Feed 喂入数据进行解析（供streamCopySSE调用）
// 采用增量解析，避免重复扫描已处理数据
func (p *sseUsageParser) Feed(data []byte) error {
	// 如果已标记为超限,不再解析usage但继续传输流
	if p.oversized {
		return nil
	}

	// 防御性检查:限制缓冲区大小
	if p.bufferSize+len(data) > maxSSEEventSize {
		log.Printf("WARN: SSE usage buffer exceeds max size (%d bytes), stopping usage extraction for this request", maxSSEEventSize)
		p.oversized = true
		return nil // 不返回错误,让流传输继续
	}

	// 🔍 诊断补丁: 记录异常小的首块数据(用于定位21字节问题)
	// 正常SSE事件至少40-50字节,如果首块<64字节可能是上游异常
	if p.bufferSize == 0 && len(data) <= 64 {
		log.Printf("🔍 [SSE异常首块] 渠道=%s 大小=%d 内容=%q", 
			p.channelType, len(data), data)
	}

	p.buffer.Write(data)
	p.bufferSize += len(data)
	return p.parseBuffer()
}

// parseBuffer 解析缓冲区中的SSE事件（增量解析）
func (p *sseUsageParser) parseBuffer() error {
	bufData := p.buffer.Bytes()
	offset := 0

	for {
		// 查找下一个换行符
		lineEnd := bytes.IndexByte(bufData[offset:], '\n')
		if lineEnd == -1 {
			// 没有完整的行，保留剩余数据
			break
		}

		// 提取当前行（去除\r\n）
		lineEnd += offset
		line := string(bytes.TrimRight(bufData[offset:lineEnd], "\r"))
		offset = lineEnd + 1

		// SSE事件格式：
		// event: message_start
		// data: {...}
		// (空行表示事件结束)

		if after, ok := strings.CutPrefix(line, "event:"); ok {
			p.eventType = strings.TrimSpace(after)
		} else if after0, ok0 := strings.CutPrefix(line, "data:"); ok0 {
			dataLine := strings.TrimSpace(after0)
			p.dataLines = append(p.dataLines, dataLine)
		} else if line == "" && len(p.dataLines) > 0 {
			// 事件结束，解析数据
			if err := p.parseEvent(p.eventType, strings.Join(p.dataLines, "")); err != nil {
				// 记录错误但继续处理（容错设计）
				log.Printf("WARN: SSE event parse failed (type=%s): %v", p.eventType, err)
			}
			p.eventType = ""
			p.dataLines = nil
		}
	}

	// 保留未处理的数据（从offset开始）
	if offset > 0 {
		remaining := bufData[offset:]
		p.buffer.Reset()
		p.buffer.Write(remaining)
		p.bufferSize = len(remaining)
	}

	return nil
}

// parseEvent 解析单个SSE事件
func (p *sseUsageParser) parseEvent(eventType, data string) error {
	// ✅ 事件类型过滤优化（2025-12-07）
	// 问题：anyrouter等聚合服务使用非标准事件类型（如"."），导致usage丢失
	// 方案：改为黑名单模式 - 只过滤已知无用事件，其他都尝试解析

	// ⚠️ 特殊处理：error事件（记录日志 + 存储错误体用于后续冷却处理）
	if eventType == "error" {
		log.Printf("⚠️  [SSE错误事件] 上游返回error事件: %s", data)
		// ✅ 新增：存储错误事件的完整JSON（用于流结束后触发冷却逻辑）
		p.lastError = []byte(data)
		return nil // 不解析usage，避免误判
	}

	// 已知无用事件（不包含usage）
	ignoredEvents := []string{
		"ping",                // 心跳事件
		"content_block_start", // Claude内容块开始（无usage）
		"content_block_delta", // Claude增量内容（无usage）
	}

	if eventType != "" && slices.Contains(ignoredEvents, eventType) {
		return nil // 跳过已知无用事件
	}

	// 解析JSON数据
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return fmt.Errorf("json unmarshal failed: %w", err)
	}

	usage := extractUsage(event)

	if usage == nil {
		return nil
	}

	p.applyUsage(usage, p.channelType)

	return nil
}

// GetUsage 获取累积的usage统计
// 重要: 返回的inputTokens已归一化为"可计费输入token"
// - OpenAI/Codex: prompt_tokens包含cached_tokens，已自动扣除避免双计
// - Claude/Gemini: input_tokens本身就是非缓存部分，无需处理
func (p *sseUsageParser) GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	billableInput := p.InputTokens

	// OpenAI语义归一化: prompt_tokens包含cached_tokens，需扣除
	// 设计原则: 平台差异在解析层处理，计费层无需关心
	if (p.channelType == "openai" || p.channelType == "codex") && p.CacheReadInputTokens > 0 {
		billableInput = p.InputTokens - p.CacheReadInputTokens
		if billableInput < 0 {
			log.Printf("WARN: %s model has cacheReadTokens(%d) > inputTokens(%d), clamped to 0",
				p.channelType, p.CacheReadInputTokens, p.InputTokens)
			billableInput = 0
		}
	}

	return billableInput, p.OutputTokens, p.CacheReadInputTokens, p.CacheCreationInputTokens
}

// ✅ GetLastError 返回SSE流中检测到的最后一个error事件
func (p *sseUsageParser) GetLastError() []byte {
	return p.lastError
}

func (p *sseUsageParser) GetReceivedData() []byte {
	return p.buffer.Bytes()
}

func (p *jsonUsageParser) Feed(data []byte) error {
	if p.truncated {
		return nil
	}
	if p.buffer.Len()+len(data) > maxUsageBodySize {
		p.truncated = true
		log.Printf("WARN: usage body exceeds max size (%d bytes), skip usage extraction", maxUsageBodySize)
		return nil
	}
	_, err := p.buffer.Write(data)
	return err
}

func (p *jsonUsageParser) GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	if p.truncated || p.buffer.Len() == 0 {
		return 0, 0, 0, 0
	}

	data := p.buffer.Bytes()

	// 兼容 text/plain SSE 回退：上游偶尔用 text/plain 发送 SSE 事件
	if bytes.Contains(data, []byte("event:")) {
		sseParser := &sseUsageParser{channelType: p.channelType}
		if err := sseParser.Feed(data); err != nil {
			log.Printf("WARN: usage sse-like parse failed: %v", err)
		} else {
			return sseParser.GetUsage()
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("WARN: usage json parse failed: %v", err)
		return 0, 0, 0, 0
	}

	p.applyUsage(extractUsage(payload), p.channelType)

	// OpenAI语义归一化: 与sseUsageParser保持一致
	billableInput := p.InputTokens
	if (p.channelType == "openai" || p.channelType == "codex") && p.CacheReadInputTokens > 0 {
		billableInput = p.InputTokens - p.CacheReadInputTokens
		if billableInput < 0 {
			log.Printf("WARN: %s model has cacheReadTokens(%d) > inputTokens(%d), clamped to 0",
				p.channelType, p.CacheReadInputTokens, p.InputTokens)
			billableInput = 0
		}
	}

	return billableInput, p.OutputTokens, p.CacheReadInputTokens, p.CacheCreationInputTokens
}

// ✅ GetLastError 返回nil（jsonUsageParser不处理SSE error事件）
func (p *jsonUsageParser) GetLastError() []byte {
	return nil // JSON解析器不处理SSE error事件
}

func (p *jsonUsageParser) GetReceivedData() []byte {
	return p.buffer.Bytes()
}

func (u *usageAccumulator) applyUsage(usage map[string]any, channelType string) {
	if usage == nil {
		return
	}

	// 平台判断:优先使用channelType(配置明确),fallback到字段特征检测
	// 设计原则:Trust Configuration > Guess from Data
	switch channelType {
	case "gemini":
		// Gemini平台:usageMetadata包装或直接字段
		u.applyGeminiUsage(usage)

	case "openai", "codex":
		// OpenAI平台:需区分Chat Completions vs Responses API
		// Chat Completions: prompt_tokens + completion_tokens
		// Responses API: input_tokens + output_tokens
		if hasOpenAIChatUsageFields(usage) {
			u.applyOpenAIChatUsage(usage)
		} else if hasAnthropicUsageFields(usage) {
			// OpenAI Responses API使用类似Anthropic的字段
			u.applyAnthropicOrResponsesUsage(usage)
		} else {
			log.Printf("WARN: OpenAI channel with unknown usage format, keys: %v", getUsageKeys(usage))
		}

	case "anthropic":
		// Anthropic平台:input_tokens + output_tokens + cache字段
		u.applyAnthropicOrResponsesUsage(usage)

	default:
		// 未知channelType,fallback到字段特征检测(向后兼容)
		log.Printf("WARN: unknown channel_type '%s', fallback to field detection", channelType)
		switch {
		case hasGeminiUsageFields(usage):
			u.applyGeminiUsage(usage)
		case hasOpenAIChatUsageFields(usage):
			u.applyOpenAIChatUsage(usage)
		case hasAnthropicUsageFields(usage):
			u.applyAnthropicOrResponsesUsage(usage)
		default:
			log.Printf("ERROR: cannot detect usage format for channel_type '%s', keys: %v", channelType, getUsageKeys(usage))
		}
	}
}

// hasGeminiUsageFields 检测是否为Gemini usage格式
// 组合判断:usageMetadata(包装) 或 promptTokenCount+candidatesTokenCount(直接字段)
func hasGeminiUsageFields(usage map[string]any) bool {
	// 检查usageMetadata包装格式
	if _, ok := usage["usageMetadata"].(map[string]any); ok {
		return true
	}
	// 检查直接字段格式(至少有一个Gemini特有字段)
	_, hasPromptCount := usage["promptTokenCount"].(float64)
	_, hasCandidatesCount := usage["candidatesTokenCount"].(float64)
	return hasPromptCount || hasCandidatesCount
}

// hasOpenAIChatUsageFields 检测是否为OpenAI Chat Completions格式
// 组合判断:必须有prompt_tokens和completion_tokens
func hasOpenAIChatUsageFields(usage map[string]any) bool {
	_, hasPromptTokens := usage["prompt_tokens"].(float64)
	_, hasCompletionTokens := usage["completion_tokens"].(float64)
	// OpenAI Chat格式必须同时有这两个字段
	return hasPromptTokens && hasCompletionTokens
}

// hasAnthropicUsageFields 检测是否为Anthropic/OpenAI Responses格式
// 组合判断:至少有input_tokens或output_tokens之一
func hasAnthropicUsageFields(usage map[string]any) bool {
	_, hasInputTokens := usage["input_tokens"].(float64)
	_, hasOutputTokens := usage["output_tokens"].(float64)
	return hasInputTokens || hasOutputTokens
}

// applyGeminiUsage 处理Gemini格式的usage
func (u *usageAccumulator) applyGeminiUsage(usage map[string]any) {
	if val, ok := usage["promptTokenCount"].(float64); ok {
		u.InputTokens = int(val)
	}

	// 输出token = candidatesTokenCount + thoughtsTokenCount
	// Gemini 2.5 Pro等模型的思考token需要计入输出
	var outputTokens int
	if val, ok := usage["candidatesTokenCount"].(float64); ok {
		outputTokens = int(val)
	}
	if val, ok := usage["thoughtsTokenCount"].(float64); ok {
		outputTokens += int(val)
	}

	// 备选方案：当candidatesTokenCount为0时，尝试从totalTokenCount推算
	// 某些Gemini模型的流式响应中candidatesTokenCount始终为0
	if outputTokens == 0 {
		if total, ok := usage["totalTokenCount"].(float64); ok {
			if prompt, ok := usage["promptTokenCount"].(float64); ok {
				calculated := int(total) - int(prompt)
				if calculated > 0 {
					outputTokens = calculated
				}
			}
		}
	}

	u.OutputTokens = outputTokens
	// Gemini目前不支持缓存字段
}

// applyOpenAIChatUsage 处理OpenAI Chat Completions API格式
func (u *usageAccumulator) applyOpenAIChatUsage(usage map[string]any) {
	if val, ok := usage["prompt_tokens"].(float64); ok {
		u.InputTokens = int(val)
	}
	if val, ok := usage["completion_tokens"].(float64); ok {
		u.OutputTokens = int(val)
	}
	// OpenAI Chat Completions缓存字段: prompt_tokens_details.cached_tokens
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if val, ok := details["cached_tokens"].(float64); ok {
			u.CacheReadInputTokens = int(val)
		}
	}
}

// applyAnthropicOrResponsesUsage 处理Anthropic或OpenAI Responses API格式
// 重要：Anthropic SSE流中，message_start包含input_tokens，message_delta包含cumulative output_tokens
// 某些中间代理（如anyrouter）会在message_delta中添加input_tokens:0，需要防御性处理
func (u *usageAccumulator) applyAnthropicOrResponsesUsage(usage map[string]any) {
	// input_tokens: 只有 > 0 时才覆盖（防止message_delta中的0覆盖message_start的正确值）
	if val, ok := usage["input_tokens"].(float64); ok && int(val) > 0 {
		u.InputTokens = int(val)
	}
	// output_tokens: 直接覆盖（cumulative语义，后续值包含之前的累计）
	if val, ok := usage["output_tokens"].(float64); ok {
		u.OutputTokens = int(val)
	}

	// Anthropic缓存字段
	if val, ok := usage["cache_read_input_tokens"].(float64); ok {
		u.CacheReadInputTokens = int(val)
	}
	if val, ok := usage["cache_creation_input_tokens"].(float64); ok {
		u.CacheCreationInputTokens = int(val)
	}

	// OpenAI Responses API缓存字段: input_tokens_details.cached_tokens
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if val, ok := details["cached_tokens"].(float64); ok {
			u.CacheReadInputTokens = int(val)
		}
	}
}

// getUsageKeys 获取usage map的所有key用于日志
func getUsageKeys(usage map[string]any) []string {
	keys := make([]string, 0, len(usage))
	for k := range usage {
		keys = append(keys, k)
	}
	return keys
}

func extractUsage(payload map[string]any) map[string]any {
	// Claude/OpenAI格式: {"usage": {...}}
	if usage, ok := payload["usage"].(map[string]any); ok {
		return usage
	}
	// Claude消息格式: {"message": {"usage": {...}}}
	if msg, ok := payload["message"].(map[string]any); ok {
		if usage, ok := msg["usage"].(map[string]any); ok {
			return usage
		}
	}
	// OpenAI部分格式: {"response": {"usage": {...}}}
	if resp, ok := payload["response"].(map[string]any); ok {
		if usage, ok := resp["usage"].(map[string]any); ok {
			return usage
		}
	}
	// Gemini格式: {"usageMetadata": {...}}
	if usageMetadata, ok := payload["usageMetadata"].(map[string]any); ok {
		return usageMetadata
	}

	return nil
}
