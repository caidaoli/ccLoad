package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
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
	buffer     bytes.Buffer // 未完成的数据缓冲区
	bufferSize int          // 当前缓冲区大小
	eventType  string       // 当前正在解析的事件类型（跨Feed保存）
	dataLines  []string     // 当前事件的data行（跨Feed保存）
}

type jsonUsageParser struct {
	usageAccumulator
	buffer    bytes.Buffer
	truncated bool
}

type usageParser interface {
	Feed([]byte) error
	GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int)
}

const (
	// maxSSEEventSize SSE事件最大尺寸（防止内存耗尽攻击）
	maxSSEEventSize = 1 << 20 // 1MB

	// maxUsageBodySize 用于普通JSON响应 usage 提取时的最大缓存（防止内存过大）
	maxUsageBodySize = 1 << 20 // 1MB
)

// newSSEUsageParser 创建SSE usage解析器
func newSSEUsageParser() *sseUsageParser {
	return &sseUsageParser{}
}

// newJSONUsageParser 创建JSON响应的usage解析器
func newJSONUsageParser() *jsonUsageParser {
	return &jsonUsageParser{}
}

// Feed 喂入数据进行解析（供streamCopySSE调用）
// 采用增量解析，避免重复扫描已处理数据
func (p *sseUsageParser) Feed(data []byte) error {
	// 防御性检查：限制缓冲区大小
	if p.bufferSize+len(data) > maxSSEEventSize {
		return fmt.Errorf("SSE event exceeds max size (%d bytes)", maxSSEEventSize)
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
	// 事件类型过滤：
	// - Claude: message_start, message_delta
	// - OpenAI Responses API (Codex): response.completed
	// - Gemini: 无event类型（eventType为空字符串）
	if eventType != "" {
		// 有明确事件类型时，只处理已知类型
		if eventType != "message_start" && eventType != "message_delta" && eventType != "response.completed" {
			return nil
		}
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

	p.applyUsage(usage)

	return nil
}

// GetUsage 获取累积的usage统计
func (p *sseUsageParser) GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	return p.InputTokens, p.OutputTokens, p.CacheReadInputTokens, p.CacheCreationInputTokens
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
		sseParser := &sseUsageParser{}
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

	p.applyUsage(extractUsage(payload))
	return p.InputTokens, p.OutputTokens, p.CacheReadInputTokens, p.CacheCreationInputTokens
}

func (u *usageAccumulator) applyUsage(usage map[string]any) {
	if usage == nil {
		return
	}

	// Claude/OpenAI Responses API格式: input_tokens, output_tokens
	if val, ok := usage["input_tokens"].(float64); ok {
		u.InputTokens = int(val)
	}
	if val, ok := usage["output_tokens"].(float64); ok {
		u.OutputTokens = int(val)
	}

	// OpenAI Chat Completions API格式: prompt_tokens, completion_tokens
	if val, ok := usage["prompt_tokens"].(float64); ok {
		u.InputTokens = int(val)
	}
	if val, ok := usage["completion_tokens"].(float64); ok {
		u.OutputTokens = int(val)
	}

	// Gemini格式: promptTokenCount, candidatesTokenCount
	if val, ok := usage["promptTokenCount"].(float64); ok {
		u.InputTokens = int(val)
	}
	if val, ok := usage["candidatesTokenCount"].(float64); ok {
		u.OutputTokens = int(val)
	}

	// Claude缓存字段
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

	// OpenAI Chat Completions API缓存字段: prompt_tokens_details.cached_tokens
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if val, ok := details["cached_tokens"].(float64); ok {
			u.CacheReadInputTokens = int(val)
		}
	}
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
