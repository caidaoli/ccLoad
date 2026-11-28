package app

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	codexresponses "ccLoad/internal/protocol/cliproxy/codex/openai/responses"
	modelregistry "ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxCodexNonStreamOutputBytes = 32 << 20

/**
 * @brief 归一 Responses 事件类型，识别缺少 type 的错误帧。
 * @param payload 上游 SSE data 或 WebSocket 文本帧。
 * @param allowCodeMessageFallback 是否启用“顶层 code 与 message 都非空即错误”的兜底；仅 Codex 上游启用，
 *        避免把非 Codex 流里每 chunk 携带 code/message 的成功帧误判为错误。
 * @return 显式 type 去首尾空白后原样返回；缺少 type 时，error/body.error/response.error 为非空对象或非空字符串
 *        （或启用兜底时顶层 code 与 message 都非空）返回 "error"；其他情况（含非法 JSON）返回空串。
 * @details 显式 type 优先，输出与元数据事件即使带 error 字段也不会被当成失败终态。所有上游协议共用此判断，
 *          空对象 `"error":{}` 不携带错误信息，按成功帧处理。
 */
func codexResponseEventType(payload []byte, allowCodeMessageFallback bool) string {
	if eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String()); eventType != "" {
		return eventType
	}
	if !gjson.ValidBytes(payload) {
		return ""
	}
	for _, path := range [...]string{"error", "body.error", "response.error"} {
		node := gjson.GetBytes(payload, path)
		if node.IsObject() && len(node.Map()) > 0 || node.Type == gjson.String && strings.TrimSpace(node.Str) != "" {
			return "error"
		}
	}
	if allowCodeMessageFallback &&
		strings.TrimSpace(gjson.GetBytes(payload, "code").String()) != "" &&
		strings.TrimSpace(gjson.GetBytes(payload, "message").String()) != "" {
		return "error"
	}
	return ""
}

/**
 * @brief 依据事件类型和 response.done 的嵌套状态识别 Responses 终态。
 * @param eventType JSON 的 type，缺失时采用 SSE event 行。
 * @param event 解析后的上游事件。
 * @return 独立于帧缓冲区的标准终态类型；其他事件返回空串。
 */
func responsesTerminalType(eventType string, event gjson.Result) string {
	switch eventType {
	case "response.completed":
		return "response.completed"
	case "response.incomplete":
		return "response.incomplete"
	case "response.failed":
		return "response.failed"
	case "response.done":
		switch strings.ToLower(strings.TrimSpace(event.Get("response.status").String())) {
		case "failed":
			return "response.failed"
		case "incomplete", "cancelled":
			return "response.incomplete"
		default:
			return "response.completed"
		}
	default:
		return ""
	}
}

/**
 * @brief 判断 Responses 终态是否可用于完成回合并保存输出。
 * @param terminalType 标准终态类型。
 * @return 已完成或不完整终态返回 true。
 */
func isSuccessfulResponsesTerminal(terminalType string) bool {
	return terminalType == "response.completed" || terminalType == "response.incomplete"
}

// normalizeCodexClientPath maps Codex client alias paths (/v1/codex/responses,
// /backend-api/codex/responses) to the canonical upstream /v1/responses, so a
// plain HTTP forward does not leak the alias into the upstream URL. Official
// OAuth channels carry the full URL via the Exact marker, so they are
// unaffected: buildUpstreamURL skips path appending for Exact URLs.
func normalizeCodexClientPath(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	switch trimmed {
	case "/v1/codex/responses", "/backend-api/codex/responses":
		return "/v1/responses"
	case "/v1/codex/alpha/search", "/backend-api/codex/alpha/search":
		return "/v1/alpha/search"
	default:
		return path
	}
}

func isCodexOAuthResponsesRequest(cfg *model.Config, upstreamProtocol protocol.Protocol, requestPath string) bool {
	if cfg == nil || !cfg.UsesCodexOAuth() || upstreamProtocol != protocol.Codex {
		return false
	}
	if protocol.DetectRequestFamily(requestPath) == protocol.RequestFamilyResponses {
		return true
	}
	return strings.HasSuffix(strings.TrimRight(strings.TrimSpace(requestPath), "/"), "/backend-api/codex/responses")
}

const codexRoutingHintHeader = "X-Codex-Routing-Hint"

// setCodexRoutingHint mirrors native Codex (rust-v0.155.0): every ChatGPT
// backend request carries model=<slug>[;tier=<service_tier>] so the edge can
// route before parsing the body. It is derived from the final wire body, never
// copied from the client, because channel model redirects change the slug.
func setCodexRoutingHint(h http.Header, body []byte) {
	h.Del(codexRoutingHintHeader)
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		return
	}
	hint := "model=" + model
	if tier := gjson.GetBytes(body, "service_tier"); tier.Type == gjson.String {
		if value := strings.TrimSpace(tier.String()); value != "" {
			hint += ";tier=" + value
		}
	}
	h.Set(codexRoutingHintHeader, hint)
}

// codexResponsesLiteRequested reports the responses-lite signal. Native Codex
// (rust-v0.157.1) sends it per transport: a request header over HTTP, a
// client_metadata key over WebSocket. Either form enables it for both.
func codexResponsesLiteRequested(body []byte, headers http.Header) bool {
	if strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true") {
		return true
	}
	metadata := gjson.GetBytes(body, codexResponsesLiteMetadata)
	return metadata.Type == gjson.True ||
		metadata.Type == gjson.String && strings.EqualFold(strings.TrimSpace(metadata.String()), "true")
}

/**
 * @brief 在协议转换后应用 Codex OAuth 的 HTTP/WS 共同请求约束。
 * @param cfg 本次使用的渠道及认证配置。
 * @param upstreamProtocol 本次上游协议。
 * @param requestPath Responses 请求路径。
 * @param body 已完成协议转换的请求体。
 * @param headers 请求头，用于识别 responses-lite 信号。
 * @return 完成 OAuth wire 归一化的请求体；其他认证或端点原样返回。
 */
func prepareCodexOAuthResponsesBody(
	cfg *model.Config,
	upstreamProtocol protocol.Protocol,
	requestPath string,
	body []byte,
	headers http.Header,
) []byte {
	if !isCodexOAuthResponsesRequest(cfg, upstreamProtocol, requestPath) {
		return body
	}

	// The Responses-to-Codex normalizer never reports an unsendable part.
	body, _ = codexresponses.ConvertOpenAIResponsesRequestToCodex(
		gjson.GetBytes(body, "model").String(), body, true,
	)
	// The normalizer fills max_output_tokens from legacy fields, after the
	// shared Codex clamp has already run.
	body = clampCodexMaxOutputTokens(body)
	body = normalizeCodexOAuthResponsesEffort(body)
	if instructions := gjson.GetBytes(body, "instructions"); !instructions.Exists() ||
		instructions.Type != gjson.String || strings.TrimSpace(instructions.String()) == "" {
		body, _ = sjson.SetBytes(
			body,
			"instructions",
			codexBaseInstructionsForModel(gjson.GetBytes(body, "model").String()),
		)
	}

	if codexResponsesLiteRequested(body, headers) {
		body, _ = sjson.SetBytes(body, "parallel_tool_calls", false)
	} else {
		tools := gjson.GetBytes(body, "tools")
		if !tools.IsArray() || len(tools.Array()) == 0 {
			body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
		}
	}
	return body
}

/**
 * @brief 按实际模型能力分别收敛 Responses 基线及会话更新的思考等级。
 * @param body 已通过 Codex OAuth 请求归一化的 JSON 请求体。
 * @return 保留更新位置和其他配置的请求体；缺少模型等级元数据时原样返回。
 * @details none 在模型不可关闭思考时取最低档；原生 auto 沿用上游的 medium 兼容映射并按模型档位收敛。
 */
func normalizeCodexOAuthResponsesEffort(body []byte) []byte {
	info := modelregistry.LookupModelInfo(gjson.GetBytes(body, "model").String(), "codex")
	if info == nil || info.Thinking == nil || len(info.Thinking.Levels) == 0 {
		return body
	}
	paths := []string{"reasoning.effort"}
	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		for index, item := range input.Array() {
			if item.Get("type").String() == "configuration_update" {
				paths = append(paths, fmt.Sprintf("input.%d.reasoning.effort", index))
			}
		}
	}
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if value.Type != gjson.String {
			continue
		}
		effort := strings.ToLower(strings.TrimSpace(value.String()))
		switch effort {
		case "auto":
			effort = thinking.ClampEffortToLevels("medium", info.Thinking.Levels)
		case "none":
			if !info.Thinking.ZeroAllowed && !thinking.HasLevel(info.Thinking.Levels, effort) {
				effort = thinking.ClampEffortToLevels("minimal", info.Thinking.Levels)
			}
		default:
			if _, ok := thinking.ParseLevelSuffix(effort); !ok {
				continue
			}
			effort = thinking.ClampEffortToLevels(effort, info.Thinking.Levels)
		}
		if effort != value.String() {
			body, _ = sjson.SetBytes(body, path, effort)
		}
	}
	return body
}

// stripInjectedCodexOAuthInstructionsForWebsocket removes only the default
// instructions synthesized by the shared HTTP/WS request finalizer. Explicit
// non-empty caller instructions remain untouched.
func stripInjectedCodexOAuthInstructionsForWebsocket(
	cfg *model.Config,
	sourceBody []byte,
	body []byte,
) []byte {
	if cfg == nil || !cfg.UsesCodexOAuth() {
		return body
	}
	sourceInstructions := gjson.GetBytes(sourceBody, "instructions")
	if sourceInstructions.Exists() && sourceInstructions.Type == gjson.String &&
		strings.TrimSpace(sourceInstructions.String()) != "" {
		return body
	}
	instructions := gjson.GetBytes(body, "instructions")
	if instructions.Type != gjson.String || strings.TrimSpace(instructions.String()) == "" {
		return body
	}
	defaultInstructions := codexBaseInstructionsForModel(gjson.GetBytes(body, "model").String())
	if instructions.String() != defaultInstructions {
		sourceModel := strings.TrimSpace(gjson.GetBytes(sourceBody, "model").String())
		if sourceModel == "" || instructions.String() != codexBaseInstructionsForModel(sourceModel) {
			return body
		}
	}
	stripped, err := sjson.DeleteBytes(body, "instructions")
	if err != nil {
		return body
	}
	return stripped
}

func prepareCodexOAuthHTTPBody(cfg *model.Config, upstreamProtocol protocol.Protocol, requestPath string, body []byte) []byte {
	if !isCodexOAuthResponsesRequest(cfg, upstreamProtocol, requestPath) {
		return body
	}
	reasoningSummaryDelivery := gjson.GetBytes(body, "stream_options.reasoning_summary_delivery")
	// The responses-lite metadata key is the WebSocket form of a header that
	// buildProxyRequest has already set on the HTTP request.
	for _, field := range []string{
		"previous_response_id", "generate", "prompt_cache_retention", "safety_identifier", "stream_options",
		codexResponsesLiteMetadata,
	} {
		body, _ = sjson.DeleteBytes(body, field)
	}
	if reasoningSummaryDelivery.Exists() {
		body, _ = sjson.SetBytes(
			body,
			"stream_options.reasoning_summary_delivery",
			reasoningSummaryDelivery.Value(),
		)
	}
	return body
}

func (s *Server) handleResponsesSSENonStreamSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	w http.ResponseWriter,
	readStats *streamReadStats,
) (*fwResult, float64, error) {
	parser := newSSEUsageParser(string(protocol.Codex))
	collector := newCodexNonStreamCollector(parser)
	consume := collector.consume
	stopAfterEvent := collector.done
	if isImagesResponsesPlan(reqCtx.transformPlan) {
		stopAfterEvent = collector.doneForImages
	}
	streamErr := streamTransformSSEEventsUntil(
		reqCtx.ctx,
		resp.Body,
		discardHTTPResponseWriter{},
		consume,
		func([]byte) ([][]byte, error) { return nil, nil },
		stopAfterEvent,
	)
	readStats.totalBytes = collector.bytesRead
	if collector.bytesRead > 0 {
		readStats.readCount = 1
	}
	if collector.err != nil {
		streamErr = collector.err
	}

	result := &fwResult{
		Status:            resp.StatusCode,
		UpstreamStatus:    resp.StatusCode,
		Header:            hdrClone,
		FirstByteTime:     responseFirstByteSec(reqCtx, readStats),
		BytesReceived:     readStats.totalBytes,
		ResponseCommitted: false,
	}
	populateFWResultFromUsageParser(result, parser)
	if result.SSEErrorEvent != nil {
		return result, reqCtx.Duration().Seconds(), nil
	}
	if streamErr != nil {
		// 客户端主动断开不是上游故障：留空诊断信息，让上层按 499 处理而非流不完整。
		if !isClientDisconnectError(streamErr) {
			result.StreamDiagMsg = streamErr.Error()
		}
		return result, reqCtx.Duration().Seconds(), streamErr
	}
	if len(collector.terminal) == 0 {
		result.StreamDiagMsg = "Responses SSE stream ended without response.completed or response.incomplete"
		return result, reqCtx.Duration().Seconds(), nil
	}

	terminal := collector.patchedTerminal()
	if isImagesResponsesPlan(reqCtx.transformPlan) &&
		collector.terminalType != "response.completed" {
		err := errors.New("responses image generation did not complete")
		result.Body = terminal
		result.StreamDiagMsg = err.Error()
		return result, reqCtx.Duration().Seconds(), err
	}
	response := gjson.GetBytes(terminal, "response")
	if !response.Exists() || response.Type != gjson.JSON {
		return result, reqCtx.Duration().Seconds(), fmt.Errorf("responses terminal event is missing response")
	}
	responseBody := []byte(response.Raw)
	if isImagesResponsesPlan(reqCtx.transformPlan) {
		translatedBody, err := buildOpenAIImagesResponseFromResponses(
			responseBody,
			reqCtx.transformPlan.OriginalBody,
		)
		if err != nil {
			result.Body = responseBody
			result.StreamDiagMsg = err.Error()
			return result, reqCtx.Duration().Seconds(), err
		}
		responseBody = translatedBody
	} else if reqCtx.transformPlan.NeedsTransform {
		translatedBody, err := s.protocolRegistry.TranslateResponseNonStream(
			reqCtx.ctx,
			reqCtx.transformPlan.UpstreamProtocol,
			reqCtx.transformPlan.ClientProtocol,
			reqCtx.transformPlan.ResponseModel(),
			reqCtx.transformPlan.OriginalBody,
			reqCtx.transformPlan.TranslatedBody,
			terminal,
		)
		if err != nil {
			result.Body = responseBody
			result.StreamDiagMsg = err.Error()
			return result, reqCtx.Duration().Seconds(), err
		}
		responseBody = translatedBody
	}
	responseHeader := resp.Header.Clone()
	responseHeader.Set("Content-Type", "application/json")
	responseHeader.Del("Content-Encoding")
	responseHeader.Del("Content-Length")
	disableResponseWriteTimeout(w, "Codex非流式")
	filterAndWriteResponseHeaders(w, responseHeader)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
	result.ResponseCommitted = true
	return result, reqCtx.Duration().Seconds(), nil
}

/**
 * @brief 将已读取的真实用量和协议元数据保留到转发结果，失败尝试也参与计费
 * @param result 当前转发结果
 * @param parser 已喂入响应数据的 JSON 或 SSE 解析器
 */
func populateFWResultFromUsageParser(result *fwResult, parser usageParser) {
	result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = parser.GetUsage()
	result.ResponseModel = parser.GetResponseModel()
	result.ReasoningTokens = parser.GetReasoningTokens()
	result.Cache5mInputTokens, result.Cache1hInputTokens, result.ServiceTier = parser.GetCacheBreakdown()
	result.Cache30mInputTokens, result.Cache12hInputTokens = parser.GetOpenAICacheBreakdown()
	result.ToolCostUSD = parser.GetToolCostUSD()
	result.ThinkingEffort = parser.GetThinkingEffort()
	result.CodexHasCredits = parser.GetCodexHasCredits()
	result.SSEErrorEvent = parser.GetLastError()
	result.ResponsesTurnResult, result.HasResponsesTurnResult = parser.GetResponsesTurnResult()
}

type discardHTTPResponseWriter struct{}

func (discardHTTPResponseWriter) Header() http.Header         { return make(http.Header) }
func (discardHTTPResponseWriter) WriteHeader(int)             {}
func (discardHTTPResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

/**
 * @brief 为非流式客户端从 Responses 事件流收集终态和可能需要回填的输出。
 */
type codexNonStreamCollector struct {
	parser       *sseUsageParser
	terminal     []byte
	terminalType string ///< 已归一化的终态；空串表示尚未收到可交付的响应。
	indexedItems map[int64]json.RawMessage
	fallback     []json.RawMessage
	outputBytes  int
	bytesRead    int64
	err          error
}

func newCodexNonStreamCollector(parser *sseUsageParser) *codexNonStreamCollector {
	return &codexNonStreamCollector{parser: parser, indexedItems: make(map[int64]json.RawMessage)}
}

/**
 * @brief 计入一帧上游用量并保存完整输出项或可交付的终态。
 * @param rawEvent 当前 SSE 帧；收集器独立保存跨帧使用的数据。
 * @return 总是返回 nil。解析失败或超限会记录在收集器中，供结束判定与调用方读取。
 */
func (c *codexNonStreamCollector) consume(rawEvent []byte) error {
	if c.err != nil {
		return nil
	}
	c.bytesRead += int64(len(rawEvent))
	if len(rawEvent) > maxCodexNonStreamOutputBytes {
		c.err = fmt.Errorf("codex SSE event exceeds %d bytes", maxCodexNonStreamOutputBytes)
		return nil
	}
	if err := c.parser.Feed(rawEvent); err != nil {
		c.err = err
		return nil
	}
	eventType, data := parseSSEEventChunk(rawEvent)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	event := parseJSONBytesNoCopy(data)
	payloadType := event.Get("type").Str
	eventType = cmp.Or(payloadType, eventType)
	if eventType == "response.output_item.done" {
		item := event.Get("item")
		if !item.Exists() || item.Type != gjson.JSON {
			return nil
		}
		if !c.reserveOutput(len(item.Raw)) {
			return nil
		}
		copyItem := json.RawMessage(item.Raw)
		if index := event.Get("output_index"); index.Exists() {
			c.indexedItems[index.Int()] = copyItem
		} else {
			c.fallback = append(c.fallback, copyItem)
		}
	} else if terminalType := responsesTerminalType(eventType, event); isSuccessfulResponsesTerminal(terminalType) {
		c.terminalType = terminalType
		if payloadType == "" {
			c.terminal, c.err = sjson.SetBytes(data, "type", eventType)
		} else {
			c.terminal = bytes.Clone(data)
		}
	}
	return nil
}

func (c *codexNonStreamCollector) reserveOutput(size int) bool {
	if size < 0 || c.outputBytes > maxCodexNonStreamOutputBytes-size {
		c.err = fmt.Errorf("codex non-stream output exceeds %d bytes", maxCodexNonStreamOutputBytes)
		return false
	}
	c.outputBytes += size
	return true
}

/**
 * @brief 判断普通 Responses 请求是否可以停止读取上游。
 * @return 已取得可交付终态、具体上游错误或收集错误时为 true。
 */
func (c *codexNonStreamCollector) done() bool {
	return c.err != nil || sseErrorHasDetail(c.parser.GetLastError()) || len(c.terminal) > 0
}

/**
 * @brief 判断图像请求是否已得到结果，允许已完成终态继续等待迟到的图像输出项。
 * @return 已出错、响应不完整，或已完成响应中有图像结果时为 true。
 */
func (c *codexNonStreamCollector) doneForImages() bool {
	if c.err != nil || sseErrorHasDetail(c.parser.GetLastError()) {
		return true
	}
	if len(c.terminal) == 0 {
		return false
	}
	if c.terminalType != "response.completed" {
		return true
	}
	terminal := c.patchedTerminal()
	for _, item := range gjson.GetBytes(terminal, "response.output").Array() {
		if item.Get("type").String() == "image_generation_call" && strings.TrimSpace(item.Get("result").String()) != "" {
			return true
		}
	}
	return false
}

func (c *codexNonStreamCollector) patchedTerminal() []byte {
	if len(c.terminal) == 0 || len(c.indexedItems)+len(c.fallback) == 0 {
		return c.terminal
	}
	current := gjson.GetBytes(c.terminal, "response.output")
	currentItems := current.Array()
	existingIDs := make(map[string]struct{}, len(currentItems))
	for _, item := range currentItems {
		if id := strings.TrimSpace(item.Get("id").String()); id != "" {
			existingIDs[id] = struct{}{}
		}
	}
	indices := make([]int64, 0, len(c.indexedItems))
	for index := range c.indexedItems {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	items := make([]json.RawMessage, 0, len(currentItems)+len(indices)+len(c.fallback))
	for _, item := range currentItems {
		items = append(items, json.RawMessage(bytes.Clone([]byte(item.Raw))))
	}
	appendIfMissing := func(item json.RawMessage) {
		if id := strings.TrimSpace(gjson.GetBytes(item, "id").String()); id != "" {
			if _, exists := existingIDs[id]; exists {
				return
			}
			existingIDs[id] = struct{}{}
		}
		items = append(items, item)
	}
	for _, index := range indices {
		appendIfMissing(c.indexedItems[index])
	}
	for _, item := range c.fallback {
		appendIfMissing(item)
	}
	if len(items) == len(currentItems) {
		return c.terminal
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return c.terminal
	}
	patched, err := sjson.SetRawBytes(c.terminal, "response.output", encoded)
	if err != nil {
		return c.terminal
	}
	return patched
}
