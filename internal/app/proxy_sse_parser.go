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
type sseUsageParser struct {
	// 累积的usage数据（从message_start和message_delta事件中提取）
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int

	// 内部状态（增量解析）
	buffer     bytes.Buffer // 未完成的数据缓冲区
	bufferSize int          // 当前缓冲区大小
	eventType  string       // 当前正在解析的事件类型（跨Feed保存）
	dataLines  []string     // 当前事件的data行（跨Feed保存）
}

const (
	// maxSSEEventSize SSE事件最大尺寸（防止内存耗尽攻击）
	maxSSEEventSize = 1 << 20 // 1MB
)

// newSSEUsageParser 创建SSE usage解析器
func newSSEUsageParser() *sseUsageParser {
	return &sseUsageParser{}
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

		if strings.HasPrefix(line, "event:") {
			p.eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLine := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
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
	// 只关注包含usage信息的事件
	// Claude: message_start, message_delta
	// OpenAI Responses API (Codex): response.completed
	if eventType != "message_start" && eventType != "message_delta" && eventType != "response.completed" {
		return nil
	}

	// 解析JSON数据
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return fmt.Errorf("json unmarshal failed: %w", err)
	}

	// 提取usage字段
	var usage map[string]any
	if eventType == "message_start" {
		// Claude message_start: {type, message: {usage: {...}}}
		if msg, ok := event["message"].(map[string]any); ok {
			if u, ok := msg["usage"].(map[string]any); ok {
				usage = u
			}
		}
	} else if eventType == "message_delta" {
		// Claude message_delta: {type, delta, usage: {...}}
		if u, ok := event["usage"].(map[string]any); ok {
			usage = u
		}
	} else if eventType == "response.completed" {
		// OpenAI Responses API (Codex): {type, response: {usage: {...}}}
		if resp, ok := event["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				usage = u
			}
		}
	}

	if usage == nil {
		return nil
	}

	// 累积token统计
	if val, ok := usage["input_tokens"].(float64); ok {
		p.InputTokens = int(val)
	}
	if val, ok := usage["output_tokens"].(float64); ok {
		p.OutputTokens = int(val)
	}

	// Claude格式：cache_read_input_tokens
	if val, ok := usage["cache_read_input_tokens"].(float64); ok {
		p.CacheReadInputTokens = int(val)
	}
	if val, ok := usage["cache_creation_input_tokens"].(float64); ok {
		p.CacheCreationInputTokens = int(val)
	}

	// OpenAI Responses API格式：input_tokens_details.cached_tokens
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if val, ok := details["cached_tokens"].(float64); ok {
			p.CacheReadInputTokens = int(val)
		}
	}

	return nil
}

// GetUsage 获取累积的usage统计
func (p *sseUsageParser) GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	return p.InputTokens, p.OutputTokens, p.CacheReadInputTokens, p.CacheCreationInputTokens
}
