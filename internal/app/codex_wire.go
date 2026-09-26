package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	codexresponses "ccLoad/internal/protocol/cliproxy/codex/openai/responses"

	"github.com/klauspost/compress/zstd"
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
		gjson.GetBytes(terminal, "type").String() != "response.completed" {
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
	result.ResponseModel = parser.GetResponseModel()
	result.ReasoningTokens = parser.GetReasoningTokens()
	result.Cache5mInputTokens, result.Cache1hInputTokens, result.ServiceTier = parser.GetCacheBreakdown()
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

func (c *codexNonStreamCollector) doneForImages() bool {
	if c.err != nil || c.parser.GetLastError() != nil {
		return true
	}
	if len(c.terminal) == 0 {
		return false
	}
	if gjson.GetBytes(c.terminal, "type").String() != "response.completed" {
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
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
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

// codexRequestZstdEncoder 对应官方 zstd::stream::encode_all(level 3)：libzstd 默认不写
// 帧校验和。EncodeAll 可并发调用。
var codexRequestZstdEncoder, _ = zstd.NewWriter(nil,
	zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)), zstd.WithEncoderCRC(false))

// compressCodexOAuthResponsesBody 对齐官方 Codex（rust-v0.157.1 core/src/client.rs
// responses_request_compression）：ChatGPT 登录的流式 /responses HTTP 请求默认以 zstd
// 压缩请求体（enable_request_compression，Stable 且默认开启）。WebSocket 帧与
// /responses/compact 不压缩。req 原位替换为压缩字节，uTLS 重试经 GetBody 重放；
// 调用方保留的明文 body 继续用于调试日志与失败重放。
func compressCodexOAuthResponsesBody(cfg *model.Config, req *http.Request) error {
	if cfg == nil || !cfg.UsesCodexOAuth() || req == nil || req.Method != http.MethodPost ||
		req.GetBody == nil || req.Header.Get("Content-Encoding") != "" ||
		!strings.HasSuffix(strings.TrimRight(req.URL.Path, "/"), "/responses") {
		return nil
	}
	reader, err := req.GetBody()
	if err != nil {
		return fmt.Errorf("read Codex request body for zstd: %w", err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return fmt.Errorf("read Codex request body for zstd: %w", err)
	}
	if len(body) == 0 {
		return nil
	}
	compressed := codexRequestZstdEncoder.EncodeAll(body, nil)
	req.Header.Set("Content-Encoding", "zstd")
	req.Body = io.NopCloser(bytes.NewReader(compressed))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(compressed)), nil }
	req.ContentLength = int64(len(compressed))
	return nil
}
