package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"

	"ccLoad/internal/util"
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
	ReasoningTokens          int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	Cache5mInputTokens       int
	Cache1hInputTokens       int
	ToolCostUSD              float64
	ServiceTier              string // OpenAI service_tier: "priority"/"flex"/"default"
	ThinkingEffort           string
	usageVersion             int
	imageGenerationToolModel string
	toolUsageSeen            bool
	imageFallbackItemCosts   map[string]float64
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
	sanitizer   sseLargeFieldSanitizer

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

type sseLargeFieldSanitizer struct {
	inString      bool
	escape        bool
	stringBuf     []byte
	stringTooLong bool
	haveToken     bool
	stringToken   string
	pendingKey    string
	expectValue   bool
	dropping      bool
	dropEscape    bool
}

type usageParser interface {
	Feed([]byte) error
	GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int)
	GetCacheBreakdown() (cache5m, cache1h int, serviceTier string) // 返回缓存分桶与 OpenAI service_tier
	GetToolCostUSD() float64                                       // 返回 Responses 工具调用的额外费用
	GetThinkingEffort() string
	GetReasoningTokens() int
	GetLastError() []byte   // [INFO] 返回SSE流中检测到的最后一个error事件（用于1308等错误的延迟处理）
	IsStreamComplete() bool // [INFO] 返回是否检测到流结束标志（[DONE]/message_stop）
	HasStreamOutput() bool  // 返回是否已经看到非心跳的可见响应内容
}

// GetCacheBreakdown 由 sseUsageParser/jsonUsageParser 通过嵌入共享。
func (u *usageAccumulator) GetCacheBreakdown() (cache5m, cache1h int, serviceTier string) {
	return u.Cache5mInputTokens, u.Cache1hInputTokens, u.ServiceTier
}

func (u *usageAccumulator) GetToolCostUSD() float64 {
	return u.ToolCostUSD
}

func (u *usageAccumulator) GetThinkingEffort() string {
	return normalizeThinkingEffort(u.ThinkingEffort)
}

func (u *usageAccumulator) GetReasoningTokens() int {
	return u.ReasoningTokens
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
	data = p.sanitizer.sanitize(data)

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
		p.ReasoningTokens = p.scanner.ReasoningTokens
		p.CacheReadInputTokens = p.scanner.CacheReadInputTokens
		p.CacheCreationInputTokens = p.scanner.CacheCreationInputTokens
		p.Cache5mInputTokens = p.scanner.Cache5mInputTokens
		p.Cache1hInputTokens = p.scanner.Cache1hInputTokens
		p.scanVersion = p.scanner.usageVersion
	}
	if p.scanner.ServiceTier != "" {
		p.ServiceTier = p.scanner.ServiceTier
	}
	if p.scanner.ThinkingEffort != "" {
		p.ThinkingEffort = p.scanner.ThinkingEffort
	}
	if p.scanner.ToolCostUSD > 0 {
		p.ToolCostUSD = p.scanner.ToolCostUSD
		p.toolUsageSeen = true
	}
}

func (p *sseUsageParser) enterOversizedEventMode() {
	log.Printf("[WARN] SSE usage 事件超出最大长度（%d 字节），跳过此事件的 usage 提取", maxSSEEventSize)
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

func (s *sseLargeFieldSanitizer) sanitize(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}

	out := make([]byte, 0, min(len(data), maxSSEEventSize))
	for _, b := range data {
		if s.dropping {
			s.consumeDroppedStringByte(b)
			continue
		}

		if s.expectValue && isLargeJSONStringField(s.pendingKey) {
			if isJSONWhitespace(b) {
				out = append(out, b)
				continue
			}
			if b == '"' {
				out = append(out, '"', '<', 'o', 'm', 'i', 't', 't', 'e', 'd', '>', '"')
				s.dropping = true
				s.dropEscape = false
				s.clearPending()
				continue
			}
			s.clearPending()
		}

		out = append(out, b)

		if s.inString {
			s.scanStringByte(b)
			continue
		}
		if s.haveToken {
			if isJSONWhitespace(b) {
				continue
			}
			if b == ':' {
				s.pendingKey = s.stringToken
				s.expectValue = true
				s.haveToken = false
				s.stringToken = ""
				continue
			}
			s.haveToken = false
			s.stringToken = ""
		}
		if b == '"' {
			s.inString = true
			s.escape = false
			s.stringBuf = s.stringBuf[:0]
			s.stringTooLong = false
		}
	}
	return out
}

func (s *sseLargeFieldSanitizer) consumeDroppedStringByte(b byte) {
	if s.dropEscape {
		s.dropEscape = false
		return
	}
	switch b {
	case '\\':
		s.dropEscape = true
	case '"':
		s.dropping = false
	}
}

func (s *sseLargeFieldSanitizer) scanStringByte(b byte) {
	if s.escape {
		s.escape = false
		s.appendStringByte(b)
		return
	}
	switch b {
	case '\\':
		s.escape = true
	case '"':
		s.inString = false
		if !s.stringTooLong {
			s.haveToken = true
			s.stringToken = string(s.stringBuf)
		}
	default:
		s.appendStringByte(b)
	}
}

func (s *sseLargeFieldSanitizer) appendStringByte(b byte) {
	if s.stringTooLong {
		return
	}
	if len(s.stringBuf) >= maxJSONKeySize {
		s.stringTooLong = true
		s.stringBuf = s.stringBuf[:0]
		return
	}
	s.stringBuf = append(s.stringBuf, b)
}

func (s *sseLargeFieldSanitizer) clearPending() {
	s.pendingKey = ""
	s.expectValue = false
}

func isLargeJSONStringField(key string) bool {
	return key == "result" || key == "partial_image_b64"
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
			if err := p.parseEvent(p.eventType, strings.Join(p.dataLines, "\n")); err != nil {
				// 记录错误但继续处理（容错设计）
				log.Printf("[WARN] SSE 事件解析失败 (type=%s): %v", p.eventType, err)
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

	if data == "[DONE]" {
		p.streamComplete = true
		return nil
	}

	// 特殊处理：error事件（记录日志 + 存储错误体用于后续冷却处理）
	// 兼容不带 event: error 行的不规范上游（如 sub2api），与 isHeartbeatEvent 的 JSON 回退对称。
	if eventType == "error" || isErrorPayload(data) {
		log.Printf("[WARN]  [SSE错误事件] 上游返回error内容(eventType=%q): %s", eventType, data)
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
	payloadType, _ := event["type"].(string)
	if eventType == "message_stop" || (eventType == "response.completed" && payloadType == "response.completed") {
		p.streamComplete = true
	}

	// 提取 service_tier（OpenAI Chat/Responses API 顶层字段）
	if tier, ok := event["service_tier"].(string); ok && tier != "" {
		p.ServiceTier = tier
	} else if resp, ok := event["response"].(map[string]any); ok {
		if tier, ok := resp["service_tier"].(string); ok && tier != "" {
			p.ServiceTier = tier
		}
	}
	if effort := extractThinkingEffortFromPayload(event); effort != "" {
		p.ThinkingEffort = effort
	}

	usage := extractUsage(event)

	if usage == nil {
		p.applyToolUsageFromPayload(event)
		return nil
	}

	// Anthropic fast mode: 从 usage.speed 推断计费层级
	if speed, ok := usage["speed"].(string); ok && speed == "fast" {
		p.ServiceTier = "fast"
	}

	p.applyUsage(usage, p.channelType)
	p.applyToolUsageFromPayload(event)

	return nil
}

// GetUsage 获取累积的usage统计
// 重要: 返回的inputTokens已归一化为"可计费输入token"
// - OpenAI/Codex: input/prompt_tokens 包含 cached_tokens 与 cache_write_tokens，已自动扣除避免双计
// - Gemini: promptTokenCount包含cachedContentTokenCount，已自动扣除
// - Claude: input_tokens本身就是非缓存部分，无需处理
func (p *sseUsageParser) GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	return p.normalizedUsage(p.channelType)
}

func (u *usageAccumulator) normalizedUsage(channelType string) (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	billableInput := u.InputTokens

	// OpenAI/Codex/Gemini语义归一化: prompt_tokens/input_tokens 包含缓存分项，需扣除避免双计
	// - cached_tokens / cache_read → CacheReadInputTokens
	// - cache_write_tokens / cache_creation → CacheCreationInputTokens（仅 openai/codex 计入 input）
	// 设计原则: 平台差异在解析层处理，计费层无需关心
	if channelType == "openai" || channelType == "codex" || channelType == "gemini" {
		includedCache := u.CacheReadInputTokens
		if channelType == "openai" || channelType == "codex" {
			includedCache += u.CacheCreationInputTokens
		}
		if includedCache > 0 {
			if includedCache <= u.InputTokens {
				billableInput = u.InputTokens - includedCache
			} else {
				log.Printf("[WARN] %s usage 中 cacheRead(%d)+cacheCreation(%d) > inputTokens(%d)，将 inputTokens 视为非缓存 token",
					channelType, u.CacheReadInputTokens, u.CacheCreationInputTokens, u.InputTokens)
			}
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

// isErrorPayload 检测 data 是否为 error 事件 JSON，用于兼容不带 event: error 行的不规范上游。
// 判定：顶层 type=="error"（Anthropic 风格）或顶层 error 字段为非空对象（聚合站风格）。
func isErrorPayload(data string) bool {
	if data == "" || !strings.Contains(data, `"error"`) {
		return false
	}
	var event struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(data), &event) != nil {
		return false
	}
	if event.Type == "error" {
		return true
	}
	// error 字段存在且为非空的 JSON 对象（排除 null / 空对象 / 空串）
	trimmed := strings.TrimSpace(string(event.Error))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return false
	}
	return strings.HasPrefix(trimmed, "{")
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
		log.Printf("[WARN] usage 响应体超过最大长度（%d 字节），切换到流式 usage 提取", maxUsageBodySize)
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
			case "usage", "usageMetadata", "usage_metadata", "tool_usage":
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
		case "usage", "usageMetadata", "usage_metadata":
			var usage map[string]any
			if err := json.Unmarshal(p.scanCaptureBuf, &usage); err == nil {
				p.applyUsageMap(usage)
			}
		case "tool_usage":
			var toolUsage map[string]any
			if err := json.Unmarshal(p.scanCaptureBuf, &toolUsage); err == nil {
				p.applyToolUsageMap(toolUsage, "")
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
			log.Printf("[WARN] 类 SSE 格式的 usage 解析失败: %v", err)
		} else {
			p.ServiceTier = sseParser.ServiceTier
			p.ThinkingEffort = sseParser.GetThinkingEffort()
			p.ReasoningTokens = sseParser.GetReasoningTokens()
			p.ToolCostUSD = sseParser.GetToolCostUSD()
			return sseParser.GetUsage()
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[WARN] usage JSON 解析失败: %v", err)
		return 0, 0, 0, 0
	}

	usage := extractUsage(payload)
	// Anthropic fast mode: 从 usage.speed 推断计费层级
	p.applyUsageMap(usage)
	p.applyToolUsageFromPayload(payload)
	if effort := extractThinkingEffortFromPayload(payload); effort != "" {
		p.ThinkingEffort = effort
	}

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

func (u *usageAccumulator) applyToolUsageFromPayload(payload map[string]any) {
	toolUsage, model := extractToolUsageAndImageModel(payload)
	if model != "" {
		u.imageGenerationToolModel = model
	}
	if u.applyToolUsageMap(toolUsage, model) {
		return
	}
	u.applyImageGenerationFallbackFromPayload(payload, model)
}

func (u *usageAccumulator) applyToolUsageMap(toolUsage map[string]any, imageModel string) bool {
	if toolUsage == nil {
		return false
	}
	imageUsage, ok := toolUsage["image_gen"].(map[string]any)
	if !ok {
		return false
	}
	cost := util.CalculateImageGenerationToolCost(imageModel, imageGenerationToolUsageFromMap(imageUsage))
	if cost <= 0 {
		return false
	}
	u.ToolCostUSD = cost
	u.toolUsageSeen = true
	return true
}

func (u *usageAccumulator) applyImageGenerationFallbackFromPayload(payload map[string]any, imageModel string) {
	if u.toolUsageSeen || payload == nil {
		return
	}
	if imageModel == "" {
		imageModel = u.imageGenerationToolModel
	}
	for _, item := range extractCompletedImageGenerationItems(payload) {
		cost := util.CalculateImageGenerationToolFallbackCost(imageModel, item.quality, item.size)
		if cost <= 0 {
			continue
		}
		if u.imageFallbackItemCosts == nil {
			u.imageFallbackItemCosts = make(map[string]float64)
		}
		if prev, ok := u.imageFallbackItemCosts[item.key]; ok {
			if prev != cost {
				u.ToolCostUSD += cost - prev
				u.imageFallbackItemCosts[item.key] = cost
			}
			continue
		}
		u.imageFallbackItemCosts[item.key] = cost
		u.ToolCostUSD += cost
	}
}

func extractToolUsageAndImageModel(payload map[string]any) (map[string]any, string) {
	if payload == nil {
		return nil, ""
	}
	if resp, ok := payload["response"].(map[string]any); ok {
		toolUsage, _ := resp["tool_usage"].(map[string]any)
		return toolUsage, extractImageGenerationModel(resp["tools"])
	}
	toolUsage, _ := payload["tool_usage"].(map[string]any)
	return toolUsage, extractImageGenerationModel(payload["tools"])
}

type completedImageGenerationItem struct {
	key     string
	quality string
	size    string
}

func extractCompletedImageGenerationItems(payload map[string]any) []completedImageGenerationItem {
	items := make([]completedImageGenerationItem, 0, 1)
	if item, ok := payload["item"].(map[string]any); ok {
		if parsed, ok := completedImageGenerationItemFromMap(item, "item"); ok {
			items = append(items, parsed)
		}
	}
	if resp, ok := payload["response"].(map[string]any); ok {
		if output, ok := resp["output"].([]any); ok {
			for i, rawItem := range output {
				item, ok := rawItem.(map[string]any)
				if !ok {
					continue
				}
				if parsed, ok := completedImageGenerationItemFromMap(item, fmt.Sprintf("response.output.%d", i)); ok {
					items = append(items, parsed)
				}
			}
		}
	}
	return items
}

func completedImageGenerationItemFromMap(item map[string]any, fallbackKey string) (completedImageGenerationItem, bool) {
	itemType, _ := item["type"].(string)
	if itemType != "image_generation_call" {
		return completedImageGenerationItem{}, false
	}
	result, _ := item["result"].(string)
	if result == "" {
		return completedImageGenerationItem{}, false
	}
	quality, _ := item["quality"].(string)
	size, _ := item["size"].(string)
	if quality == "" || size == "" {
		return completedImageGenerationItem{}, false
	}
	key, _ := item["id"].(string)
	if strings.TrimSpace(key) == "" {
		key = fallbackKey
	}
	return completedImageGenerationItem{
		key:     key,
		quality: quality,
		size:    size,
	}, true
}

func extractImageGenerationModel(rawTools any) string {
	tools, ok := rawTools.([]any)
	if !ok {
		return ""
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		toolType, _ := tool["type"].(string)
		if toolType != "image_generation" {
			continue
		}
		model, _ := tool["model"].(string)
		return strings.TrimSpace(model)
	}
	return ""
}

func imageGenerationToolUsageFromMap(usage map[string]any) util.ImageGenerationToolUsage {
	inputDetails, _ := usage["input_tokens_details"].(map[string]any)
	outputDetails, _ := usage["output_tokens_details"].(map[string]any)
	return util.ImageGenerationToolUsage{
		InputTokens:       usageInt(usage, "input_tokens"),
		OutputTokens:      usageInt(usage, "output_tokens"),
		TextInputTokens:   usageInt(inputDetails, "text_tokens"),
		TextCachedTokens:  usageInt(inputDetails, "cached_text_tokens"),
		ImageInputTokens:  usageInt(inputDetails, "image_tokens"),
		ImageCachedTokens: usageInt(inputDetails, "cached_image_tokens"),
		ImageOutputTokens: usageInt(outputDetails, "image_tokens"),
	}
}

func usageInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch value := m[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	default:
		return 0
	}
}

func usageFirstInt(m map[string]any, keys ...string) int {
	for _, key := range keys {
		if val := usageInt(m, key); val > 0 {
			return val
		}
	}
	return 0
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
			log.Printf("[WARN] OpenAI 渠道返回未知的 usage 格式，keys: %v", getUsageKeys(usage))
		}

	case "anthropic":
		// Anthropic平台:input_tokens + output_tokens + cache字段
		u.applyAnthropicOrResponsesUsage(usage)

	default:
		// 未知channelType,fallback到字段特征检测(向后兼容)
		log.Printf("[WARN] 未知 channel_type '%s'，回退到字段探测", channelType)
		switch {
		case hasGeminiUsageFields(usage):
			u.applyGeminiUsage(usage)
		case hasOpenAIChatUsageFields(usage):
			u.applyOpenAIChatUsage(usage)
		case hasAnthropicUsageFields(usage):
			u.applyAnthropicOrResponsesUsage(usage)
		default:
			log.Printf("[ERROR] 无法识别 channel_type '%s' 的 usage 格式，keys: %v", channelType, getUsageKeys(usage))
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
	if _, ok := usage["usage_metadata"].(map[string]any); ok {
		return true
	}
	// 检查直接字段格式(至少有一个Gemini特有字段)
	return usageFirstInt(usage,
		"promptTokenCount", "prompt_token_count",
		"candidatesTokenCount", "candidates_token_count",
		"thoughtsTokenCount", "thoughts_token_count",
		"totalThoughtTokens", "total_thought_tokens",
	) > 0
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
	if nested, ok := usage["usageMetadata"].(map[string]any); ok {
		usage = nested
	} else if nested, ok := usage["usage_metadata"].(map[string]any); ok {
		usage = nested
	}

	if val := usageFirstInt(usage, "promptTokenCount", "prompt_token_count"); val > 0 {
		u.InputTokens = val
	}

	// 输出token = candidatesTokenCount + thoughtsTokenCount
	// Gemini 2.5 Pro等模型的思考token需要计入输出
	outputTokens := usageFirstInt(usage, "candidatesTokenCount", "candidates_token_count")
	reasoningTokens := usageFirstInt(usage,
		"thoughtsTokenCount", "thoughts_token_count",
		"totalThoughtTokens", "total_thought_tokens",
	)
	if reasoningTokens > 0 {
		u.ReasoningTokens = reasoningTokens
		outputTokens += reasoningTokens
	}

	// 备选方案：当candidatesTokenCount为0时，尝试从totalTokenCount推算
	// 某些Gemini模型的流式响应中candidatesTokenCount始终为0
	if outputTokens == 0 {
		total := usageFirstInt(usage, "totalTokenCount", "total_token_count")
		prompt := usageFirstInt(usage, "promptTokenCount", "prompt_token_count")
		if calculated := total - prompt; calculated > 0 {
			outputTokens = calculated
		}
	}

	u.OutputTokens = outputTokens

	// Gemini缓存字段: cachedContentTokenCount
	if val := usageFirstInt(usage, "cachedContentTokenCount", "cached_content_token_count"); val > 0 {
		u.CacheReadInputTokens = val
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
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		if val := usageFirstInt(details, "reasoning_tokens", "thinking_tokens"); val > 0 {
			u.ReasoningTokens = val
		}
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		if val := usageFirstInt(details, "reasoning_tokens", "thinking_tokens"); val > 0 {
			u.ReasoningTokens = val
		}
	}
	if val := usageFirstInt(usage, "reasoning_tokens", "thinking_tokens"); val > 0 {
		u.ReasoningTokens = val
	}
	u.applyBillingUsageOpenAIReasoning(usage)
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
	hasAggregateCacheCreation := false
	if val, ok := usage["cache_creation_input_tokens"].(float64); ok {
		hasAggregateCacheCreation = true
		u.CacheCreationInputTokens = int(val)
	}

	// Anthropic缓存细分字段 (新增2025-12)
	hasDetailedCacheCreation := false
	if cacheCreation, ok := usage["cache_creation"].(map[string]any); ok {
		hasDetailedCacheCreation = true
		if val, ok := cacheCreation["ephemeral_5m_input_tokens"].(float64); ok {
			u.Cache5mInputTokens = int(val)
		}
		if val, ok := cacheCreation["ephemeral_1h_input_tokens"].(float64); ok {
			u.Cache1hInputTokens = int(val)
		}
		// 更新兼容字段
		u.CacheCreationInputTokens = u.Cache5mInputTokens + u.Cache1hInputTokens
	}
	if hasAggregateCacheCreation && !hasDetailedCacheCreation && u.CacheCreationInputTokens > 0 {
		u.Cache5mInputTokens = u.CacheCreationInputTokens
	}

	// OpenAI Responses / Codex 缓存字段:
	// input_tokens_details.cached_tokens      → 缓存读
	// input_tokens_details.cache_write_tokens → 缓存建（写入）
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if val, ok := details["cached_tokens"].(float64); ok {
			u.CacheReadInputTokens = int(val)
		}
		// 仅在尚未拿到 Anthropic 风格 cache_creation 字段时采用 cache_write_tokens
		if !hasAggregateCacheCreation && !hasDetailedCacheCreation {
			if val, ok := details["cache_write_tokens"].(float64); ok {
				u.CacheCreationInputTokens = int(val)
				if u.CacheCreationInputTokens > 0 {
					// OpenAI cache write 无 5m/1h 细分，按 5m 写价（1.25x）计费
					u.Cache5mInputTokens = u.CacheCreationInputTokens
				}
			}
		}
	}

	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		if val := usageFirstInt(details, "reasoning_tokens", "thinking_tokens"); val > 0 {
			u.ReasoningTokens = val
		}
	}
	if val := usageFirstInt(usage,
		"reasoning_tokens", "thinking_tokens",
		"total_thought_tokens", "totalThoughtTokens",
	); val > 0 {
		u.ReasoningTokens = val
	}
	// NewAPI 等网关在 Claude 风格 usage 外包一层 billing_usage.openai_usage，
	// 真实 reasoning_tokens 只在 completion_tokens_details 里。
	u.applyBillingUsageOpenAIReasoning(usage)
}

// applyBillingUsageOpenAIReasoning 从 NewAPI 风格 billing_usage.openai_usage 补齐推理 token。
// 仅在尚未从标准字段拿到 reasoning 时回填，避免覆盖原生路径。
func (u *usageAccumulator) applyBillingUsageOpenAIReasoning(usage map[string]any) {
	if u.ReasoningTokens > 0 || usage == nil {
		return
	}
	billing, ok := usage["billing_usage"].(map[string]any)
	if !ok {
		return
	}
	oai, ok := billing["openai_usage"].(map[string]any)
	if !ok {
		return
	}
	if details, ok := oai["completion_tokens_details"].(map[string]any); ok {
		if val := usageFirstInt(details, "reasoning_tokens", "thinking_tokens"); val > 0 {
			u.ReasoningTokens = val
			return
		}
	}
	if details, ok := oai["output_tokens_details"].(map[string]any); ok {
		if val := usageFirstInt(details, "reasoning_tokens", "thinking_tokens"); val > 0 {
			u.ReasoningTokens = val
			return
		}
	}
	if val := usageFirstInt(oai, "reasoning_tokens", "thinking_tokens"); val > 0 {
		u.ReasoningTokens = val
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
	if usageMetadata, ok := payload["usage_metadata"].(map[string]any); ok {
		return usageMetadata
	}

	return nil
}
