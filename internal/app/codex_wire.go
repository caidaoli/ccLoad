package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	codexresponses "ccLoad/internal/protocol/cliproxy/codex/openai/responses"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxCodexNonStreamOutputBytes = 32 << 20

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

// prepareCodexOAuthResponsesBody applies the mandatory ChatGPT Codex wire
// contract after any cross-protocol translation. This deliberately lives above
// the synchronized translator snapshot: it is credential/runtime behavior, not
// a format conversion rule.
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

	body = codexresponses.ConvertOpenAIResponsesRequestToCodex(
		gjson.GetBytes(body, "model").String(), body, true,
	)
	if effort := gjson.GetBytes(body, "reasoning.effort"); effort.Type == gjson.String &&
		strings.EqualFold(strings.TrimSpace(effort.String()), "minimal") {
		body, _ = sjson.SetBytes(body, "reasoning.effort", "low")
	}
	if instructions := gjson.GetBytes(body, "instructions"); !instructions.Exists() ||
		instructions.Type != gjson.String || strings.TrimSpace(instructions.String()) == "" {
		body, _ = sjson.SetBytes(
			body,
			"instructions",
			codexBaseInstructionsForModel(gjson.GetBytes(body, "model").String()),
		)
	}

	responsesLite := strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true")
	if !responsesLite {
		metadata := gjson.GetBytes(body, codexResponsesLiteMetadata)
		responsesLite = metadata.Type == gjson.True ||
			metadata.Type == gjson.String && strings.EqualFold(strings.TrimSpace(metadata.String()), "true")
	}
	if responsesLite {
		body, _ = sjson.SetBytes(body, "parallel_tool_calls", false)
	} else {
		tools := gjson.GetBytes(body, "tools")
		if !tools.IsArray() || len(tools.Array()) == 0 {
			body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
		}
	}
	return body
}

func prepareCodexOAuthHTTPBody(cfg *model.Config, upstreamProtocol protocol.Protocol, requestPath string, body []byte) []byte {
	if !isCodexOAuthResponsesRequest(cfg, upstreamProtocol, requestPath) {
		return body
	}
	reasoningSummaryDelivery := gjson.GetBytes(body, "stream_options.reasoning_summary_delivery")
	for _, field := range []string{
		"previous_response_id", "generate", "prompt_cache_retention", "safety_identifier", "stream_options",
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
	if reqCtx.codexMultiAgentV2Optimized {
		consume = func(rawEvent []byte) error {
			return collector.consume(restoreCodexMultiAgentV2SSEEvent(rawEvent, true))
		}
	}
	streamErr := streamTransformSSEEventsUntil(
		reqCtx.ctx,
		resp.Body,
		discardHTTPResponseWriter{},
		consume,
		func([]byte) ([][]byte, error) { return nil, nil },
		collector.done,
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
		FirstByteTime:     readStats.firstByteSec,
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
	response := gjson.GetBytes(terminal, "response")
	if !response.Exists() || response.Type != gjson.JSON {
		return result, reqCtx.Duration().Seconds(), fmt.Errorf("responses terminal event is missing response")
	}
	responseBody := []byte(response.Raw)
	if reqCtx.codexMultiAgentV2Optimized {
		responseBody = restoreCodexMultiAgentV2Response(responseBody, true)
	}
	if reqCtx.transformPlan.NeedsTransform {
		if s.protocolRegistry == nil {
			return result, reqCtx.Duration().Seconds(), errors.New("protocol registry unavailable for Responses non-stream response transform")
		}
		translatedBody, err := s.protocolRegistry.TranslateResponseNonStream(
			reqCtx.ctx,
			reqCtx.transformPlan.UpstreamProtocol,
			reqCtx.transformPlan.ClientProtocol,
			reqCtx.transformPlan.ResponseModel(),
			reqCtx.transformPlan.OriginalBody,
			reqCtx.transformPlan.TranslatedBody,
			responseBody,
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

func populateFWResultFromUsageParser(result *fwResult, parser *sseUsageParser) {
	if result == nil || parser == nil {
		return
	}
	result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = parser.GetUsage()
	result.ReasoningTokens = parser.GetReasoningTokens()
	result.Cache5mInputTokens, result.Cache1hInputTokens, result.ServiceTier = parser.GetCacheBreakdown()
	result.ToolCostUSD = parser.GetToolCostUSD()
	result.ThinkingEffort = parser.GetThinkingEffort()
	result.SSEErrorEvent = parser.GetLastError()
	result.ResponsesTurnResult, result.HasResponsesTurnResult = parser.GetResponsesTurnResult()
}

type discardHTTPResponseWriter struct{}

func (discardHTTPResponseWriter) Header() http.Header         { return make(http.Header) }
func (discardHTTPResponseWriter) WriteHeader(int)             {}
func (discardHTTPResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

type codexNonStreamCollector struct {
	parser       *sseUsageParser
	terminal     []byte
	indexedItems map[int64]json.RawMessage
	fallback     []json.RawMessage
	outputBytes  int
	bytesRead    int64
	err          error
}

func newCodexNonStreamCollector(parser *sseUsageParser) *codexNonStreamCollector {
	return &codexNonStreamCollector{parser: parser, indexedItems: make(map[int64]json.RawMessage)}
}

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
	data := sseEventData(rawEvent)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	switch gjson.GetBytes(data, "type").String() {
	case "response.output_item.done":
		item := gjson.GetBytes(data, "item")
		if !item.Exists() || item.Type != gjson.JSON {
			return nil
		}
		if !c.reserveOutput(len(item.Raw)) {
			return nil
		}
		copyItem := json.RawMessage(bytes.Clone([]byte(item.Raw)))
		if index := gjson.GetBytes(data, "output_index"); index.Exists() {
			c.indexedItems[index.Int()] = copyItem
		} else {
			c.fallback = append(c.fallback, copyItem)
		}
	case "response.completed", "response.incomplete":
		c.terminal = bytes.Clone(data)
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

func (c *codexNonStreamCollector) done() bool {
	return c.err != nil || c.parser.GetLastError() != nil || len(c.terminal) > 0
}

func (c *codexNonStreamCollector) patchedTerminal() []byte {
	if len(c.terminal) == 0 || len(c.indexedItems)+len(c.fallback) == 0 {
		return c.terminal
	}
	current := gjson.GetBytes(c.terminal, "response.output")
	if current.IsArray() && len(current.Array()) > 0 {
		return c.terminal
	}
	indices := make([]int64, 0, len(c.indexedItems))
	for index := range c.indexedItems {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	items := make([]json.RawMessage, 0, len(indices)+len(c.fallback))
	for _, index := range indices {
		items = append(items, c.indexedItems[index])
	}
	items = append(items, c.fallback...)
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
