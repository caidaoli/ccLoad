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
	Cache5mInputTokens       int
	Cache1hInputTokens       int
	ServiceTier              string // OpenAI service_tier: "priority"/"flex"/"default"
	usageVersion             int
}

type sseUsageParser struct {
	usageAccumulator

	// 内部状态（增量解析）
	buffer      bytes.Buffer // 未完成的数据缓冲区
	bufferSize  int          // 当前缓冲区大小
	eventType   string       // 当前正在解析的事件类型（跨Feed保存）
	dataLines   []string     // 当前事件的data行（跨Feed保存）
	oversized   bool         // 当前事件超出大小限制，丢弃到事件边界后恢复解析
	channelType string       // 渠道类型(anthropic/openai/codex/gemini),用于精确平台判断
	discardTail string       // 丢弃超大事件时保留少量尾部，用于识别跨chunk的空行边界
	scanner     jsonUsageParser
	scanVersion int

	// [INFO] 新增：存储SSE流中检测到的error事件（用于1308等错误的延迟处理）
	lastError []byte // 最后一个error事件的完整JSON（data字段内容）

	// [INFO] 新增：流结束标志（用于判断流是否正常完成）
	// OpenAI: data: [DONE]
	// Anthropic: event: message_stop
	streamComplete bool

	// hasStreamOutput 表示已经看到应转发给客户端的非心跳流事件。
	// ping 只是上游保活，不能让 200 空流被误判为成功。
	hasStreamOutput bool
}

type jsonUsageParser struct {
	usageAccumulator
	buffer      bytes.Buffer
	truncated   bool
	channelType string // 渠道类型(anthropic/openai/codex/gemini),用于精确平台判断
	hasBody     bool

	scanInString       bool
	scanEscape         bool
	scanStringBuf      []byte
	scanStringTooLong  bool
	scanHaveToken      bool
	scanStringToken    string
	scanPendingKey     string
	scanExpectValue    bool
	scanCaptureKey     string
	scanCaptureBuf     []byte
	scanCaptureDepth   int
	scanCaptureString  bool
	scanCaptureEscape  bool
	scanCaptureDiscard bool
}

type usageParser interface {
	Feed([]byte) error
	GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int)
	GetCacheBreakdown() (cache5m, cache1h int, serviceTier string) // 返回缓存分桶与 OpenAI service_tier
	GetLastError() []byte                                          // [INFO] 返回SSE流中检测到的最后一个error事件（用于1308等错误的延迟处理）
	IsStreamComplete() bool                                        // [INFO] 返回是否检测到流结束标志（[DONE]/message_stop）
	HasStreamOutput() bool                                         // 返回是否已经看到非心跳的可见响应内容
}

// GetCacheBreakdown 由 sseUsageParser/jsonUsageParser 通过嵌入共享。
func (u *usageAccumulator) GetCacheBreakdown() (cache5m, cache1h int, serviceTier string) {
	return u.Cache5mInputTokens, u.Cache1hInputTokens, u.ServiceTier
}

const (
	// maxSSEEventSize SSE事件最大尺寸（防止内存耗尽攻击）
	maxSSEEventSize = 1 << 20 // 1MB

	// maxUsageBodySize 用于普通JSON响应 usage 提取时的最大缓存（防止内存过大）
	maxUsageBodySize = 1 << 20 // 1MB

	maxJSONUsageFragmentSize = 64 << 10
	maxJSONKeySize           = 128
)

// newSSEUsageParser 创建SSE usage解析器
// channelType: 渠道类型(anthropic/openai/codex/gemini),用于精确识别平台usage格式
func newSSEUsageParser(channelType string) *sseUsageParser {
	p := &sseUsageParser{
		channelType: channelType,
	}
	p.scanner.channelType = channelType
	return p
}

// newJSONUsageParser 创建JSON响应的usage解析器
// channelType: 渠道类型(anthropic/openai/codex/gemini),用于精确识别平台usage格式
func newJSONUsageParser(channelType string) *jsonUsageParser {
	return &jsonUsageParser{channelType: channelType}
}

// Feed 喂入数据进行解析（供streamCopySSE调用）
// 采用增量解析，避免重复扫描已处理数据
func (p *sseUsageParser) Feed(data []byte) error {
	p.scanUsageFragments(data)

	for len(data) > 0 {
		if p.oversized {
			data = p.discardUntilEventBoundary(data)
			continue
		}

		available := maxSSEEventSize - p.bufferSize
		if available <= 0 {
			p.enterOversizedEventMode()
			continue
		}

		n := min(len(data), available)

		p.buffer.Write(data[:n])
		p.bufferSize += n
		data = data[n:]

		if err := p.parseBuffer(); err != nil {
			return err
		}

		if p.bufferSize >= maxSSEEventSize && len(data) > 0 {
			p.enterOversizedEventMode()
		}
	}

	return nil
}

func (p *sseUsageParser) scanUsageFragments(data []byte) {
	p.scanner.scanJSONUsage(data)
	if p.scanner.usageVersion > p.scanVersion {
		p.InputTokens = p.scanner.InputTokens
		p.OutputTokens = p.scanner.OutputTokens
		p.CacheReadInputTokens = p.scanner.CacheReadInputTokens
		p.CacheCreationInputTokens = p.scanner.CacheCreationInputTokens
		p.Cache5mInputTokens = p.scanner.Cache5mInputTokens
		p.Cache1hInputTokens = p.scanner.Cache1hInputTokens
		p.scanVersion = p.scanner.usageVersion
	}
	if p.scanner.ServiceTier != "" {
		p.ServiceTier = p.scanner.ServiceTier
	}
}

func (p *sseUsageParser) enterOversizedEventMode() {
	log.Printf("WARN: SSE usage event exceeds max size (%d bytes), skipping this event for usage extraction", maxSSEEventSize)
	p.oversized = true
	p.buffer.Reset()
	p.bufferSize = 0
	p.eventType = ""
	p.dataLines = nil
	p.discardTail = ""
}

func (p *sseUsageParser) discardUntilEventBoundary(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}

	if len(p.discardTail) > 0 {
		prefixLen := min(len(data), 3)
		combined := make([]byte, 0, len(p.discardTail)+prefixLen)
		combined = append(combined, p.discardTail...)
		combined = append(combined, data[:prefixLen]...)
		if end, ok := findSSEEventBoundary(combined); ok {
			return p.leaveOversizedEventMode(data, end-len(p.discardTail))
		}
	}

	if end, ok := findSSEEventBoundary(data); ok {
		return p.leaveOversizedEventMode(data, end)
	}

	p.discardTail = trailingSSEBoundaryTail(p.discardTail, data)
	return nil
}

func (p *sseUsageParser) leaveOversizedEventMode(data []byte, consume int) []byte {
	if consume < 0 {
		consume = 0
	}
	if consume > len(data) {
		consume = len(data)
	}
	p.oversized = false
	p.discardTail = ""
	return data[consume:]
}

func trailingSSEBoundaryTail(tail string, data []byte) string {
	if len(data) >= 3 {
		return string(data[len(data)-3:])
	}
	combined := append([]byte(tail), data...)
	if len(combined) > 3 {
		combined = combined[len(combined)-3:]
	}
	return string(combined)
}

func findSSEEventBoundary(data []byte) (int, bool) {
	patterns := [][]byte{
		[]byte("\n\n"),
		[]byte("\n\r\n"),
		[]byte("\r\n\r\n"),
	}
	bestStart := -1
	bestEnd := -1
	for _, pattern := range patterns {
		if idx := bytes.Index(data, pattern); idx >= 0 {
			end := idx + len(pattern)
			if bestStart == -1 || idx < bestStart || (idx == bestStart && end > bestEnd) {
				bestStart = idx
				bestEnd = end
			}
		}
	}
	if bestEnd == -1 {
		return 0, false
	}
	return bestEnd, true
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
			// [INFO] 流结束标志检测（按事件类型）
			// - Anthropic: event: message_stop
			// - OpenAI Responses API: event: response.completed
			if p.eventType == "message_stop" || p.eventType == "response.completed" {
				p.streamComplete = true
			}
		} else if after0, ok0 := strings.CutPrefix(line, "data:"); ok0 {
			dataLine := strings.TrimSpace(after0)
			// [INFO] OpenAI 流结束标志: data: [DONE]
			if dataLine == "[DONE]" {
				p.streamComplete = true
				p.hasStreamOutput = true
				continue // [DONE]不是JSON，跳过追加
			}
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
	// [INFO] 事件类型过滤优化（2025-12-07）
	// 问题：anyrouter等聚合服务使用非标准事件类型（如"."），导致usage丢失
	// 方案：改为黑名单模式 - 只过滤已知无用事件，其他都尝试解析

	// [WARN] 特殊处理：error事件（记录日志 + 存储错误体用于后续冷却处理）
	if eventType == "error" {
		log.Printf("[WARN]  [SSE错误事件] 上游返回error事件: %s", data)
		// [INFO] 新增：存储错误事件的完整JSON（用于流结束后触发冷却逻辑）
		p.lastError = []byte(data)
		return nil // 不解析usage，避免误判
	}

	if isHeartbeatEvent(eventType, data) {
		return nil
	}

	p.hasStreamOutput = true

	// 已知无用事件（不包含usage）
	ignoredEvents := []string{
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

	// 提取 service_tier（OpenAI Chat/Responses API 顶层字段）
	if tier, ok := event["service_tier"].(string); ok && tier != "" {
		p.ServiceTier = tier
	} else if resp, ok := event["response"].(map[string]any); ok {
		if tier, ok := resp["service_tier"].(string); ok && tier != "" {
			p.ServiceTier = tier
		}
	}

	usage := extractUsage(event)

	if usage == nil {
		return nil
	}

	// Anthropic fast mode: 从 usage.speed 推断计费层级
	if speed, ok := usage["speed"].(string); ok && speed == "fast" {
		p.ServiceTier = "fast"
	}

	p.applyUsage(usage, p.channelType)

	return nil
}

// GetUsage 获取累积的usage统计
// 重要: 返回的inputTokens已归一化为"可计费输入token"
// - OpenAI/Codex: prompt_tokens包含cached_tokens，已自动扣除避免双计
// - Gemini: promptTokenCount包含cachedContentTokenCount，已自动扣除
// - Claude: input_tokens本身就是非缓存部分，无需处理
func (p *sseUsageParser) GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	return p.normalizedUsage(p.channelType)
}

func (u *usageAccumulator) normalizedUsage(channelType string) (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	billableInput := u.InputTokens

	// OpenAI/Codex/Gemini语义归一化: prompt_tokens包含cached_tokens，需扣除
	// 设计原则: 平台差异在解析层处理，计费层无需关心
	if (channelType == "openai" || channelType == "codex" || channelType == "gemini") && u.CacheReadInputTokens > 0 {
		billableInput = u.InputTokens - u.CacheReadInputTokens
		if billableInput < 0 {
			log.Printf("WARN: %s model has cacheReadTokens(%d) > inputTokens(%d), clamped to 0",
				channelType, u.CacheReadInputTokens, u.InputTokens)
			billableInput = 0
		}
	}

	return billableInput, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens
}

// [INFO] GetLastError 返回SSE流中检测到的最后一个error事件
func (p *sseUsageParser) GetLastError() []byte {
	return p.lastError
}

// [INFO] IsStreamComplete 返回是否检测到流结束标志
func (p *sseUsageParser) IsStreamComplete() bool {
	return p.streamComplete
}

func (p *sseUsageParser) HasStreamOutput() bool {
	return p.hasStreamOutput
}

func isHeartbeatEvent(eventType, data string) bool {
	if eventType == "ping" {
		return true
	}
	if data == "" {
		return false
	}
	var event struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(data), &event) == nil && event.Type == "ping"
}

func (p *jsonUsageParser) Feed(data []byte) error {
	if len(data) > 0 {
		p.hasBody = true
	}
	p.scanJSONUsage(data)

	if p.truncated {
		return nil
	}
	if p.buffer.Len()+len(data) > maxUsageBodySize {
		p.truncated = true
		p.buffer = bytes.Buffer{}
		log.Printf("WARN: usage body exceeds max size (%d bytes), switching to streaming usage extraction", maxUsageBodySize)
		return nil
	}
	_, err := p.buffer.Write(data)
	return err
}

func (p *jsonUsageParser) scanJSONUsage(data []byte) {
	for _, b := range data {
		if p.scanCaptureKey != "" {
			p.scanJSONCaptureByte(b)
			continue
		}
		if p.scanInString {
			p.scanJSONStringByte(b)
			continue
		}
		if p.scanExpectValue {
			if isJSONWhitespace(b) {
				continue
			}
			switch p.scanPendingKey {
			case "usage", "usageMetadata":
				if b == '{' {
					p.startJSONValueCapture(b)
					continue
				}
			case "service_tier":
				if b == '"' {
					p.startJSONValueCapture(b)
					continue
				}
			}
			p.clearJSONPendingKey()
		}
		if p.scanHaveToken {
			if isJSONWhitespace(b) {
				continue
			}
			if b == ':' {
				p.scanPendingKey = p.scanStringToken
				p.scanExpectValue = true
				p.scanHaveToken = false
				p.scanStringToken = ""
				continue
			}
			p.scanHaveToken = false
			p.scanStringToken = ""
		}
		if b == '"' {
			p.scanInString = true
			p.scanEscape = false
			p.scanStringBuf = p.scanStringBuf[:0]
			p.scanStringTooLong = false
		}
	}
}

func (p *jsonUsageParser) scanJSONStringByte(b byte) {
	if p.scanEscape {
		p.scanEscape = false
		p.appendJSONKeyByte(b)
		return
	}
	switch b {
	case '\\':
		p.scanEscape = true
	case '"':
		p.scanInString = false
		if !p.scanStringTooLong {
			p.scanHaveToken = true
			p.scanStringToken = string(p.scanStringBuf)
		}
	default:
		p.appendJSONKeyByte(b)
	}
}

func (p *jsonUsageParser) appendJSONKeyByte(b byte) {
	if p.scanStringTooLong {
		return
	}
	if len(p.scanStringBuf) >= maxJSONKeySize {
		p.scanStringTooLong = true
		p.scanStringBuf = p.scanStringBuf[:0]
		return
	}
	p.scanStringBuf = append(p.scanStringBuf, b)
}

func (p *jsonUsageParser) startJSONValueCapture(first byte) {
	p.scanCaptureKey = p.scanPendingKey
	p.scanCaptureBuf = p.scanCaptureBuf[:0]
	p.scanCaptureDepth = 0
	p.scanCaptureString = false
	p.scanCaptureEscape = false
	p.scanCaptureDiscard = false
	p.clearJSONPendingKey()
	p.scanJSONCaptureByte(first)
}

func (p *jsonUsageParser) scanJSONCaptureByte(b byte) {
	if !p.scanCaptureDiscard {
		if len(p.scanCaptureBuf) >= maxJSONUsageFragmentSize {
			p.scanCaptureDiscard = true
			p.scanCaptureBuf = p.scanCaptureBuf[:0]
		} else {
			p.scanCaptureBuf = append(p.scanCaptureBuf, b)
		}
	}

	if p.scanCaptureString {
		if p.scanCaptureEscape {
			p.scanCaptureEscape = false
			return
		}
		switch b {
		case '\\':
			p.scanCaptureEscape = true
		case '"':
			p.scanCaptureString = false
			if p.scanCaptureDepth == 0 {
				p.finishJSONValueCapture()
			}
		}
		return
	}

	switch b {
	case '"':
		p.scanCaptureString = true
	case '{':
		p.scanCaptureDepth++
	case '}':
		if p.scanCaptureDepth > 0 {
			p.scanCaptureDepth--
		}
		if p.scanCaptureDepth == 0 {
			p.finishJSONValueCapture()
		}
	}
}

func (p *jsonUsageParser) finishJSONValueCapture() {
	key := p.scanCaptureKey
	discard := p.scanCaptureDiscard
	if !discard && len(p.scanCaptureBuf) > 0 {
		switch key {
		case "usage", "usageMetadata":
			var usage map[string]any
			if err := json.Unmarshal(p.scanCaptureBuf, &usage); err == nil {
				p.applyUsageMap(usage)
			}
		case "service_tier":
			var tier string
			if err := json.Unmarshal(p.scanCaptureBuf, &tier); err == nil && tier != "" {
				p.ServiceTier = tier
			}
		}
	}
	p.scanCaptureKey = ""
	p.scanCaptureBuf = p.scanCaptureBuf[:0]
	p.scanCaptureDepth = 0
	p.scanCaptureString = false
	p.scanCaptureEscape = false
	p.scanCaptureDiscard = false
}

func (p *jsonUsageParser) applyUsageMap(usage map[string]any) {
	if usage == nil {
		return
	}
	if speed, ok := usage["speed"].(string); ok && speed == "fast" {
		p.ServiceTier = "fast"
	}
	p.applyUsage(usage, p.channelType)
}

func (p *jsonUsageParser) clearJSONPendingKey() {
	p.scanPendingKey = ""
	p.scanExpectValue = false
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\n' || b == '\r' || b == '\t'
}

func (p *jsonUsageParser) GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	if p.truncated {
		return p.normalizedUsage(p.channelType)
	}
	if p.buffer.Len() == 0 {
		return p.normalizedUsage(p.channelType)
	}

	data := p.buffer.Bytes()

	// 兼容 text/plain SSE 回退：上游偶尔用 text/plain 发送 SSE 事件
	if looksLikeSSE(data) {
		sseParser := newSSEUsageParser(p.channelType)
		if err := sseParser.Feed(data); err != nil {
			log.Printf("WARN: usage sse-like parse failed: %v", err)
		} else {
			p.ServiceTier = sseParser.ServiceTier
			return sseParser.GetUsage()
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("WARN: usage json parse failed: %v", err)
		return 0, 0, 0, 0
	}

	usage := extractUsage(payload)
	// Anthropic fast mode: 从 usage.speed 推断计费层级
	p.applyUsageMap(usage)

	// 提取 service_tier（OpenAI Chat/Responses API 顶层字段）
	if tier, ok := payload["service_tier"].(string); ok && tier != "" {
		p.ServiceTier = tier
	} else if resp, ok := payload["response"].(map[string]any); ok {
		if tier, ok := resp["service_tier"].(string); ok && tier != "" {
			p.ServiceTier = tier
		}
	}

	return p.normalizedUsage(p.channelType)
}

// [INFO] GetLastError 返回nil（jsonUsageParser不处理SSE error事件）
func (p *jsonUsageParser) GetLastError() []byte {
	return nil // JSON解析器不处理SSE error事件
}

// [INFO] IsStreamComplete 返回false（非流式请求无结束标志概念）
func (p *jsonUsageParser) IsStreamComplete() bool {
	return false // JSON解析器不处理流结束标志
}

func (p *jsonUsageParser) HasStreamOutput() bool {
	return p.hasBody
}

func (u *usageAccumulator) applyUsage(usage map[string]any, channelType string) {
	if usage == nil {
		return
	}
	u.usageVersion++

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

	// Gemini缓存字段: cachedContentTokenCount
	if val, ok := usage["cachedContentTokenCount"].(float64); ok {
		u.CacheReadInputTokens = int(val)
	}
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

	// Anthropic缓存细分字段 (新增2025-12)
	if cacheCreation, ok := usage["cache_creation"].(map[string]any); ok {
		if val, ok := cacheCreation["ephemeral_5m_input_tokens"].(float64); ok {
			u.Cache5mInputTokens = int(val)
		}
		if val, ok := cacheCreation["ephemeral_1h_input_tokens"].(float64); ok {
			u.Cache1hInputTokens = int(val)
		}
		// 更新兼容字段
		u.CacheCreationInputTokens = u.Cache5mInputTokens + u.Cache1hInputTokens
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
