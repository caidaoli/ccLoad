package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

const (
	// SSEProbeSize 用于探测 text/plain 内容是否包含 SSE 事件的前缀长度（2KB 足够覆盖小事件）
	SSEProbeSize = 2 * 1024
	// softErrorProbeSize 用于探测 HTTP 200 非流响应里的结构化错误。
	softErrorProbeSize = 512
)

// prependedBody 将已读取的前缀数据与原始Body合并，保留原Closer
type prependedBody struct {
	io.Reader
	io.Closer
}

// onceCloseReadCloser 确保 Close 只执行一次（用于协调 defer 与 context.AfterFunc 的并发关闭）
type onceCloseReadCloser struct {
	io.ReadCloser
	once sync.Once
}

func (rc *onceCloseReadCloser) Close() error {
	var closeErr error
	rc.once.Do(func() {
		closeErr = rc.ReadCloser.Close()
	})
	return closeErr
}

// prependToBody 将前缀数据合并到resp.Body（用于恢复已探测的数据）
func prependToBody(resp *http.Response, prefix []byte) {
	resp.Body = prependedBody{
		Reader: io.MultiReader(bytes.NewReader(prefix), resp.Body),
		Closer: resp.Body,
	}
}

// ============================================================================
// 请求构建和转发
// ============================================================================

// buildProxyRequest 构建上游代理请求（统一处理URL、Header、认证）
// 从proxy.go提取，遵循SRP原则
func (s *Server) buildProxyRequest(
	reqCtx *requestContext,
	cfg *model.Config,
	apiKey string,
	method string,
	body []byte,
	hdr http.Header,
	rawQuery, requestPath string,
	baseURL string,
) (*http.Request, error) {
	// 1. 构建完整 URL
	upstreamURL := buildUpstreamURL(baseURL, requestPath, rawQuery)

	// 1.5 anyrouter 渠道：为 /v1/messages 自动注入 adaptive thinking
	body = maybeInjectAnyrouterAdaptiveThinking(cfg, requestPath, body)

	// 1.6 自定义请求体规则（仅对 JSON body 生效）
	body = applyBodyRules(hdr.Get("Content-Type"), body, cfg.BodyRules())

	// 1.7 Codex Responses 缓存提示：向 body 注入 prompt_cache_key
	codexSessionID := resolveCodexSessionHint(reqCtx, body, apiKey, hdr)
	if codexSessionID != "" {
		body = injectCodexPromptCacheKey(body, codexSessionID)
	}

	// 2. 创建带上下文的请求
	req, err := buildUpstreamRequest(reqCtx.ctx, method, upstreamURL, body)
	if err != nil {
		return nil, err
	}

	// 3. 复制请求头
	copyRequestHeaders(req, hdr)

	// 4. 注入认证头
	injectAPIKeyHeaders(req, apiKey, requestPath)

	// 5. anyrouter渠道：确保anthropic-beta包含context-1m
	if cfg.GetChannelType() == util.ChannelTypeAnthropic &&
		strings.Contains(strings.ToLower(cfg.Name), "anyrouter") {
		injectAnthropicBetaFlag(req, "context-1m-2025-08-07")
	}

	// 5.5 Codex Responses 缓存提示：设置 Session_id 头（仅客户端未自带时）
	if codexSessionID != "" && req.Header.Get("Session_id") == "" && req.Header.Get("Session-Id") == "" {
		req.Header.Set("Session_id", codexSessionID)
	}

	// 6. 自定义请求头规则（认证头黑名单保护）
	applyHeaderRules(req.Header, cfg.HeaderRules())

	// 7. 非 Anthropic 上游：移除 Anthropic 协议专属头（anthropic-version/anthropic-beta 等）
	stripAnthropicProtocolHeaders(req, runtimeUpstreamProtocol(reqCtx, cfg))

	if reqCtx != nil {
		reqCtx.translatedBody = body
		reqCtx.transformPlan.TranslatedBody = body
	}

	return req, nil
}

func runtimeUpstreamProtocol(reqCtx *requestContext, cfg *model.Config) string {
	if reqCtx != nil {
		if reqCtx.transformPlan.UpstreamProtocol != "" {
			return string(reqCtx.transformPlan.UpstreamProtocol)
		}
		if reqCtx.upstreamProtocol != "" {
			return string(reqCtx.upstreamProtocol)
		}
	}
	if cfg == nil {
		return ""
	}
	return cfg.GetChannelType()
}

// ============================================================================
// 响应处理
// ============================================================================

// handleRequestError 处理网络请求错误
// 从proxy.go提取，遵循SRP原则
func (s *Server) handleRequestError(
	reqCtx *requestContext,
	cfg *model.Config,
	err error,
) (*fwResult, float64, error) {
	reqCtx.stopFirstByteTimer()
	duration := reqCtx.Duration()
	durationSec := duration.Seconds()

	// 检测超时错误：使用统一的内部状态码+冷却策略
	var statusCode int
	if reqCtx.firstByteTimeoutTriggered() {
		// 流式请求首字节超时（定时器触发）
		statusCode = util.StatusFirstByteTimeout
		timeoutMsg := fmt.Sprintf("upstream first byte timeout after %.2fs", durationSec)
		timeout := s.firstByteTimeout
		if timeout > 0 {
			timeoutMsg = fmt.Sprintf("%s (threshold=%v)", timeoutMsg, timeout)
		}
		err = fmt.Errorf("%s: %w", timeoutMsg, util.ErrUpstreamFirstByteTimeout)
		log.Printf("[TIMEOUT] [上游首字节超时] 渠道ID=%d, 阈值=%v, 实际耗时=%.2fs", cfg.ID, timeout, durationSec)
	} else if errors.Is(err, context.DeadlineExceeded) {
		if reqCtx.isStreaming {
			// 流式请求超时
			err = fmt.Errorf("upstream timeout after %.2fs (streaming): %w", durationSec, err)
			statusCode = util.StatusFirstByteTimeout
			log.Printf("[TIMEOUT] [流式请求超时] 渠道ID=%d, 耗时=%.2fs", cfg.ID, durationSec)
		} else {
			// 非流式请求超时（context.WithTimeout触发）
			err = fmt.Errorf("upstream timeout after %.2fs (non-stream, threshold=%v): %w",
				durationSec, s.nonStreamTimeout, err)
			statusCode = 504 // Gateway Timeout
			log.Printf("[TIMEOUT] [非流式请求超时] 渠道ID=%d, 阈值=%v, 耗时=%.2fs", cfg.ID, s.nonStreamTimeout, durationSec)
		}
	} else {
		// 其他错误：使用统一分类器
		statusCode, _, _ = util.ClassifyError(err)
	}

	return &fwResult{
		Status:        statusCode,
		Body:          []byte(err.Error()),
		FirstByteTime: 0,
	}, durationSec, err
}

// handleErrorResponse 处理错误响应（读取完整响应体）
// 从proxy.go提取，遵循SRP原则
// 限制错误体大小防止 OOM（与入站 DefaultMaxBodyBytes 限制对称）
func (s *Server) handleErrorResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	readStats *streamReadStats,
) (*fwResult, float64, error) {
	rb, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(config.DefaultMaxBodyBytes)))
	diagMsg := ""
	if readErr != nil {
		// 不要创建“孤儿日志”（StatusCode=0），而是把诊断信息合并到本次请求的日志中（KISS）。
		diagMsg = fmt.Sprintf("error reading upstream body: %v", readErr)
	}

	duration := reqCtx.Duration().Seconds()

	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		Body:          rb,
		FirstByteTime: readStats.firstByteSec,
		StreamDiagMsg: diagMsg,
	}, duration, nil
}

// streamAndParseResponse 根据Content-Type选择合适的流式传输策略并解析usage
// 返回: (usageParser, streamErr)
func streamAndParseResponse(
	ctx context.Context,
	body io.ReadCloser,
	w http.ResponseWriter,
	contentType string,
	channelType string,
	isStreaming bool,
	beforeWrite func(usageParser) error,
) (usageParser, error) {
	makeFeed := func(parser usageParser) func([]byte) error {
		return func(data []byte) error {
			if err := parser.Feed(data); err != nil {
				return err
			}
			if beforeWrite != nil {
				return beforeWrite(parser)
			}
			return nil
		}
	}

	// SSE流式响应
	if strings.Contains(contentType, "text/event-stream") {
		parser := newSSEUsageParser(channelType)
		streamErr := streamCopySSE(ctx, body, w, makeFeed(parser))
		return parser, streamErr
	}

	// 非标准SSE场景：上游以text/plain发送SSE事件
	if strings.Contains(contentType, "text/plain") && isStreaming {
		reader := bufio.NewReader(body)
		probe, _ := reader.Peek(SSEProbeSize)

		if looksLikeSSE(probe) {
			parser := newSSEUsageParser(channelType)
			sseErr := streamCopySSE(ctx, io.NopCloser(reader), w, makeFeed(parser))
			return parser, sseErr
		}
		parser := newJSONUsageParser(channelType)
		copyErr := streamCopy(ctx, io.NopCloser(reader), w, makeFeed(parser))
		return parser, copyErr
	}

	// 非SSE响应：边转发边缓存
	parser := newJSONUsageParser(channelType)
	copyErr := streamCopy(ctx, body, w, makeFeed(parser))
	return parser, copyErr
}

// isClientDisconnectError 判断是否为客户端主动断开导致的错误
// 只识别明确的客户端取消信号，不包括上游服务器错误
// 注意：http2: response body closed 和 stream error 是上游服务器问题，不是客户端断开！
func isClientDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	// context.Canceled 是明确的客户端取消信号（用户点"停止"）
	if errors.Is(err, context.Canceled) {
		return true
	}
	// "client disconnected" 是 gin/net/http 报告的客户端断开
	// 注意：http2: response body closed 和 stream error 是上游服务器问题，
	// 不应在此判断，否则会导致上游异常被忽略而不触发冷却逻辑
	errStr := err.Error()
	return strings.Contains(errStr, "client disconnected")
}

// buildStreamDiagnostics 生成流诊断消息
// 触发条件：流传输错误且未检测到流完成语义（原始结束标志或已转译终态）
// streamComplete: 是否已确认流完成（比 hasUsage 更可靠，因为不是所有请求都有 usage）
func buildStreamDiagnostics(streamErr error, readStats *streamReadStats, streamComplete bool, channelType string, contentType string) string {
	if readStats == nil {
		return ""
	}

	bytesRead := readStats.totalBytes
	readCount := readStats.readCount

	// 流传输异常中断(排除客户端主动断开)
	// 关键：如果检测到流完成语义，说明流已完整传输
	if streamErr != nil && !isClientDisconnectError(streamErr) {
		// 已检测到流完成语义 = 流完整，http2关闭只是正常结束信号
		if streamComplete {
			return "" // 不触发冷却，数据已完整
		}
		return fmt.Sprintf("[WARN] 流传输中断: 错误=%v | 已读取=%d字节(分%d次) | 流结束标志=%v | 渠道=%s | Content-Type=%s",
			streamErr, bytesRead, readCount, streamComplete, channelType, contentType)
	}

	return ""
}

func translatedStreamChunksComplete(clientProtocol protocol.Protocol, chunks [][]byte) bool {
	for _, chunk := range chunks {
		if translatedStreamChunkCompletes(clientProtocol, chunk) {
			return true
		}
	}
	return false
}

var sseDoneMarker = []byte("[DONE]")

func translatedStreamChunkCompletes(clientProtocol protocol.Protocol, chunk []byte) bool {
	eventType, data := parseSSEEventChunk(chunk)
	if len(data) == 0 && eventType == "" {
		return false
	}

	switch clientProtocol {
	case protocol.Anthropic:
		return eventType == "message_stop" || ssePayloadType(data) == "message_stop"
	case protocol.Codex:
		return eventType == "response.completed" || ssePayloadType(data) == "response.completed"
	case protocol.OpenAI:
		if bytes.Equal(data, sseDoneMarker) {
			return true
		}
		payload, ok := decodeSSEPayload(data)
		if !ok {
			return false
		}
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			return false
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			return false
		}
		finishReason, hasFinishReason := choice["finish_reason"]
		return hasFinishReason && finishReason != nil
	case protocol.Gemini:
		payload, ok := decodeSSEPayload(data)
		if !ok {
			return false
		}
		candidates, _ := payload["candidates"].([]any)
		if len(candidates) == 0 {
			return false
		}
		candidate, _ := candidates[0].(map[string]any)
		if candidate == nil {
			return false
		}
		finishReason, _ := candidate["finishReason"].(string)
		return strings.TrimSpace(finishReason) != ""
	default:
		return false
	}
}

// parseSSEEventChunk 在 []byte 视图上解析 SSE 事件块，避免 string(chunk) 与 []byte(data) 来回拷贝。
// 返回的 data 是 chunk 的字节副本（拼接多行时已分配新切片），调用方可安全持有。
func parseSSEEventChunk(chunk []byte) (eventType string, data []byte) {
	chunk = bytes.TrimSpace(chunk)
	if len(chunk) == 0 {
		return "", nil
	}
	lines := bytes.Split(chunk, []byte{'\n'})
	dataLines := make([][]byte, 0, 1)
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if after, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			eventType = string(bytes.TrimSpace(after))
			continue
		}
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			dataLines = append(dataLines, bytes.TrimSpace(after))
		}
	}
	if len(dataLines) == 0 {
		return eventType, nil
	}
	return eventType, bytes.Join(dataLines, nil)
}

func ssePayloadType(data []byte) string {
	payload, ok := decodeSSEPayload(data)
	if !ok {
		return ""
	}
	typ, _ := payload["type"].(string)
	return typ
}

func decodeSSEPayload(data []byte) (map[string]any, bool) {
	if len(data) == 0 || bytes.Equal(data, sseDoneMarker) {
		return nil, false
	}

	var payload map[string]any
	if err := sonic.Unmarshal(data, &payload); err != nil {
		return nil, false
	}
	return payload, true
}

// handleSuccessResponse 处理成功响应（流式传输）
func (s *Server) handleSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	w http.ResponseWriter,
	channelType string,
	readStats *streamReadStats,
) (*fwResult, float64, error) {
	if reqCtx.isStreaming &&
		s.protocolRegistry != nil &&
		reqCtx.transformPlan.NeedsTransform &&
		(strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") ||
			strings.Contains(resp.Header.Get("Content-Type"), "text/plain")) {
		return s.handleTranslatedStreamSuccessResponse(reqCtx, resp, hdrClone, w, channelType, readStats)
	}

	if !reqCtx.isStreaming &&
		s.protocolRegistry != nil &&
		reqCtx.transformPlan.NeedsTransform {
		return s.handleTranslatedNonStreamSuccessResponse(reqCtx, resp, hdrClone, w, channelType, readStats)
	}

	// [FIX] 流式请求：禁用 WriteTimeout，避免长时间流被服务器自己切断
	// Go 1.20+ http.ResponseController 支持动态调整 WriteDeadline
	if reqCtx.isStreaming {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			log.Printf("[WARN] 无法禁用流式请求的 WriteTimeout: %v", err)
		}
	}

	streamWriter := w
	var deferredWriter *deferredResponseWriter
	if reqCtx.isStreaming {
		deferredWriter = newDeferredResponseWriter(w)
		streamWriter = deferredWriter
	}

	// 写入响应头
	filterAndWriteResponseHeaders(streamWriter, resp.Header)
	streamWriter.WriteHeader(resp.StatusCode)

	// 流式传输并解析usage
	contentType := resp.Header.Get("Content-Type")
	parser, streamErr := streamAndParseResponse(
		reqCtx.ctx, resp.Body, streamWriter, contentType, channelType, reqCtx.isStreaming,
		func(parser usageParser) error {
			if deferredWriter == nil || deferredWriter.Committed() {
				return nil
			}
			if parser.GetLastError() != nil {
				return errAbortStreamBeforeWrite
			}
			if parser.HasStreamOutput() {
				return deferredWriter.Commit()
			}
			return nil
		},
	)
	abortedBeforeCommit := errors.Is(streamErr, errAbortStreamBeforeWrite)
	if abortedBeforeCommit {
		streamErr = nil
	} else if deferredWriter != nil && !deferredWriter.Committed() && isEmptyStreamOutput(parser, readStats) {
		if streamErr == nil {
			return emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, emptyStreamDetail(readStats))
		}
	} else if deferredWriter != nil && !deferredWriter.Committed() {
		if commitErr := deferredWriter.Commit(); commitErr != nil && streamErr == nil {
			streamErr = commitErr
		}
	}

	// 构建结果
	result := &fwResult{
		Status:            resp.StatusCode,
		Header:            hdrClone,
		FirstByteTime:     readStats.firstByteSec,
		BytesReceived:     readStats.totalBytes, // 记录已接收字节数，用于499诊断
		ResponseCommitted: deferredWriter == nil || deferredWriter.Committed(),
	}

	// 提取usage数据和错误事件
	var streamComplete bool
	if parser != nil {
		result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = parser.GetUsage()
		result.Cache5mInputTokens, result.Cache1hInputTokens, result.ServiceTier = parser.GetCacheBreakdown()

		if errorEvent := parser.GetLastError(); errorEvent != nil {
			result.SSEErrorEvent = errorEvent
		}
		streamComplete = parser.IsStreamComplete()
	}

	// 生成流诊断消息（仅流请求）
	if reqCtx.isStreaming {
		// [VALIDATE] 诊断增强: 传递contentType帮助定位问题(区分SSE/JSON/其他)
		// 使用 streamComplete 而非 hasUsage，因为不是所有请求都有 usage 信息
		if diagMsg := buildStreamDiagnostics(streamErr, readStats, streamComplete, channelType, contentType); diagMsg != "" {
			result.StreamDiagMsg = diagMsg
			log.Print(diagMsg)
		} else if streamComplete && streamErr != nil {
			// [FIX] 流式请求：检测到流结束标志（[DONE]/message_stop）说明数据完整
			// 所有收尾阶段的错误都应忽略，包括：
			// - http2 流关闭（正常结束信号）
			// - context.Canceled（客户端在传输完成后取消，不应标记为499）
			streamErr = nil
		}
	} else {
		// [FIX] 非流式请求：如果有数据被传输，且错误是 HTTP/2 流关闭相关的，视为成功
		// 原因：streamCopy 已将数据写入 ResponseWriter，客户端已收到完整响应
		// http2 流关闭只是 "确认结束" 阶段的错误，不影响已传输的数据
		if readStats.totalBytes > 0 && streamErr != nil && isHTTP2StreamCloseError(streamErr) {
			streamErr = nil
		}
	}

	return result, reqCtx.Duration().Seconds(), streamErr
}

func (s *Server) handleTranslatedNonStreamSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	w http.ResponseWriter,
	channelType string,
	readStats *streamReadStats,
) (*fwResult, float64, error) {
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return &fwResult{
			Status:        resp.StatusCode,
			Header:        hdrClone,
			Body:          []byte(err.Error()),
			FirstByteTime: readStats.firstByteSec,
		}, reqCtx.Duration().Seconds(), err
	}

	readStats.totalBytes = int64(len(rawBody))
	if len(rawBody) > 0 {
		readStats.readCount = 1
	}

	parser := newJSONUsageParser(channelType)
	if err := parser.Feed(rawBody); err != nil {
		return &fwResult{
			Status:        resp.StatusCode,
			Header:        hdrClone,
			Body:          rawBody,
			FirstByteTime: readStats.firstByteSec,
		}, reqCtx.Duration().Seconds(), err
	}

	translatedBody, err := s.protocolRegistry.TranslateResponseNonStream(
		reqCtx.ctx,
		reqCtx.transformPlan.UpstreamProtocol,
		reqCtx.transformPlan.ClientProtocol,
		reqCtx.transformPlan.ResponseModel(),
		reqCtx.transformPlan.OriginalBody,
		reqCtx.transformPlan.TranslatedBody,
		rawBody,
	)
	if err != nil {
		return &fwResult{
			Status:        resp.StatusCode,
			Header:        hdrClone,
			Body:          rawBody,
			FirstByteTime: readStats.firstByteSec,
		}, reqCtx.Duration().Seconds(), err
	}

	filterAndWriteResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(translatedBody)

	result := &fwResult{
		Status:            resp.StatusCode,
		Header:            hdrClone,
		FirstByteTime:     readStats.firstByteSec,
		BytesReceived:     readStats.totalBytes,
		ResponseCommitted: true,
	}
	result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = parser.GetUsage()
	result.Cache5mInputTokens = parser.Cache5mInputTokens
	result.Cache1hInputTokens = parser.Cache1hInputTokens
	result.ServiceTier = parser.ServiceTier

	return result, reqCtx.Duration().Seconds(), nil
}

func (s *Server) handleTranslatedStreamSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	w http.ResponseWriter,
	channelType string,
	readStats *streamReadStats,
) (*fwResult, float64, error) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		log.Printf("[WARN] 无法禁用流式请求的 WriteTimeout: %v", err)
	}

	deferredWriter := newDeferredResponseWriter(w)
	filterAndWriteResponseHeaders(deferredWriter, resp.Header)
	deferredWriter.WriteHeader(resp.StatusCode)

	parser := newSSEUsageParser(channelType)
	var translatedComplete bool
	var state any
	streamErr := streamTransformSSEEvents(
		reqCtx.ctx,
		resp.Body,
		deferredWriter,
		func(rawEvent []byte) error {
			if err := parser.Feed(rawEvent); err != nil {
				return err
			}
			if !deferredWriter.Committed() && parser.GetLastError() != nil {
				return errAbortStreamBeforeWrite
			}
			if !deferredWriter.Committed() && parser.HasStreamOutput() {
				return deferredWriter.Commit()
			}
			return nil
		},
		func(rawEvent []byte) ([][]byte, error) {
			chunks, err := s.protocolRegistry.TranslateResponseStream(
				reqCtx.ctx,
				reqCtx.transformPlan.UpstreamProtocol,
				reqCtx.transformPlan.ClientProtocol,
				reqCtx.transformPlan.ResponseModel(),
				reqCtx.transformPlan.OriginalBody,
				reqCtx.transformPlan.TranslatedBody,
				rawEvent,
				&state,
			)
			if err != nil {
				return nil, err
			}
			if !translatedComplete && translatedStreamChunksComplete(reqCtx.transformPlan.ClientProtocol, chunks) {
				translatedComplete = true
			}
			return chunks, nil
		},
	)

	abortedBeforeCommit := errors.Is(streamErr, errAbortStreamBeforeWrite)
	if abortedBeforeCommit {
		streamErr = nil
	} else if !deferredWriter.Committed() && isEmptyStreamOutput(parser, readStats) {
		if streamErr == nil {
			return emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, emptyStreamDetail(readStats))
		}
	} else if !deferredWriter.Committed() {
		if commitErr := deferredWriter.Commit(); commitErr != nil && streamErr == nil {
			streamErr = commitErr
		}
	}

	result := &fwResult{
		Status:            resp.StatusCode,
		Header:            hdrClone,
		FirstByteTime:     readStats.firstByteSec,
		BytesReceived:     readStats.totalBytes,
		ResponseCommitted: deferredWriter.Committed(),
	}
	result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = parser.GetUsage()
	result.Cache5mInputTokens = parser.Cache5mInputTokens
	result.Cache1hInputTokens = parser.Cache1hInputTokens
	result.ServiceTier = parser.ServiceTier
	result.SSEErrorEvent = parser.GetLastError()
	streamComplete := parser.IsStreamComplete() || translatedComplete

	if diagMsg := buildStreamDiagnostics(streamErr, readStats, streamComplete, channelType, resp.Header.Get("Content-Type")); diagMsg != "" {
		result.StreamDiagMsg = diagMsg
		log.Print(diagMsg)
	} else if streamComplete && streamErr != nil {
		streamErr = nil
	}

	return result, reqCtx.Duration().Seconds(), streamErr
}

// isHTTP2StreamCloseError 判断是否是 HTTP/2 流关闭相关的错误
// 这类错误发生在数据传输完成后，不影响已传输的数据完整性
func isHTTP2StreamCloseError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "http2: response body closed") ||
		strings.Contains(errStr, "stream error:")
}

// looksLikeSSE 粗略判断文本内容是否包含 SSE 事件结构
func looksLikeSSE(data []byte) bool {
	// 同时包含 event: 与 data: 行。必须是行前缀，避免普通JSON字符串里的
	// "event:" 文本把非流响应误判成SSE。
	hasEvent := false
	hasData := false
	for len(data) > 0 {
		line := data
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			line = data[:idx]
			data = data[idx+1:]
		} else {
			data = nil
		}

		line = bytes.TrimLeft(line, " \t\r")
		if bytes.HasPrefix(line, []byte("event:")) {
			hasEvent = true
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			hasData = true
		}
		if hasEvent && hasData {
			return true
		}
	}
	return false
}

func attachFirstByteDetector(
	reqCtx *requestContext,
	resp *http.Response,
	readStats *streamReadStats,
	observer *ForwardObserver,
) {
	resp.Body = &firstByteDetector{
		ReadCloser: resp.Body,
		stats:      readStats,
		onFirstRead: func() {
			if reqCtx.isStreaming {
				reqCtx.stopFirstByteTimer()
			}
			if readStats.firstByteSec == 0 {
				readStats.firstByteSec = reqCtx.Duration().Seconds()
				if readStats.firstByteSec == 0 {
					readStats.firstByteSec = time.Nanosecond.Seconds()
				}
			}
			if reqCtx.isStreaming && observer != nil && observer.OnFirstByteRead != nil {
				observer.OnFirstByteRead()
			}
		},
		onBytesRead: func(n int64) {
			if observer != nil && observer.OnBytesRead != nil {
				observer.OnBytesRead(n)
			}
		},
	}
}

func shouldProbeSoftError(reqCtx *requestContext, resp *http.Response, channelType string) bool {
	if resp.StatusCode != http.StatusOK || reqCtx.isStreaming {
		return false
	}
	if !shouldCheckSoftErrorForChannelType(channelType) {
		return false
	}
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(ct, "text/plain") || strings.Contains(ct, "application/json")
}

// classifySSEErrorStatus 根据响应体内容判定 SSE 错误的内部状态码：
// 1308 配额超限 → 596（StatusQuotaExceeded，Key 级冷却）；其他 → 597（StatusSSEError）。
func classifySSEErrorStatus(body []byte) int {
	if _, is1308 := util.ParseResetTimeFrom1308Error(body); is1308 {
		return util.StatusQuotaExceeded
	}
	return util.StatusSSEError
}

func (s *Server) probeSoftErrorResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	cfg *model.Config,
	channelType string,
	readStats *streamReadStats,
) (handled bool, res *fwResult, duration float64, err error) {
	if !shouldProbeSoftError(reqCtx, resp, channelType) {
		return false, nil, 0, nil
	}

	ct := resp.Header.Get("Content-Type")
	buf := make([]byte, softErrorProbeSize)
	n, readErr := resp.Body.Read(buf)
	if readErr != nil && readErr != io.EOF {
		log.Printf("[WARN] 软错误检测读取失败: %v", readErr)
	}

	validData := buf[:n]
	if n > 0 && checkSoftError(validData, ct) {
		log.Printf("[WARN] [软错误检测] 渠道ID=%d, 响应200但疑似错误响应: %s", cfg.ID, truncateErr(safeBodyToString(validData)))
		resp.StatusCode = classifySSEErrorStatus(validData)
		prependToBody(resp, validData)
		res, duration, err = s.handleErrorResponse(reqCtx, resp, hdrClone, readStats)
		return true, res, duration, err
	}

	if n > 0 {
		prependToBody(resp, validData)
	}
	return false, nil, 0, nil
}

func emptyOKResponseResult(reqCtx *requestContext, resp *http.Response, hdrClone http.Header, readStats *streamReadStats, detail string) (*fwResult, float64, error) {
	duration := reqCtx.Duration().Seconds()
	err := fmt.Errorf("%w (200 OK %s)", util.ErrUpstreamEmptyResponse, detail)
	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		Body:          []byte(err.Error()),
		FirstByteTime: readStats.firstByteSec,
	}, duration, err
}

func isEmptyStreamOutput(parser usageParser, readStats *streamReadStats) bool {
	if readStats == nil || readStats.totalBytes == 0 {
		return true
	}
	return parser != nil && !parser.HasStreamOutput()
}

func emptyStreamDetail(readStats *streamReadStats) string {
	if readStats == nil || readStats.totalBytes == 0 {
		return "without response body"
	}
	return "without response content"
}

func probeEmptyOKResponse(reqCtx *requestContext, resp *http.Response, hdrClone http.Header, readStats *streamReadStats) (bool, *fwResult, float64, error) {
	if reqCtx.isStreaming || resp.StatusCode != http.StatusOK {
		return false, nil, 0, nil
	}

	if resp.Body == nil {
		res, duration, err := emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, "with nil body")
		return true, res, duration, err
	}

	if resp.Header.Get("Content-Length") == "0" {
		res, duration, err := emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, "with Content-Length: 0")
		return true, res, duration, err
	}

	var firstByte [1]byte
	n, readErr := resp.Body.Read(firstByte[:])
	if n > 0 {
		prependToBody(resp, firstByte[:n])
		return false, nil, 0, nil
	}
	if readErr == io.EOF {
		res, duration, err := emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, "without response body")
		return true, res, duration, err
	}
	return false, nil, 0, nil
}

// handleResponse 处理 HTTP 响应（错误或成功）
// 从proxy.go提取，遵循SRP原则
// channelType: 渠道类型,用于精确识别usage格式
// cfg: 渠道配置,用于提取渠道ID
// apiKey: 使用的API Key,用于日志记录
func (s *Server) handleResponse(
	reqCtx *requestContext,
	resp *http.Response,
	w http.ResponseWriter,
	channelType string,
	cfg *model.Config,
	_ string,
	observer *ForwardObserver,
) (*fwResult, float64, error) {
	hdrClone := resp.Header.Clone()
	readStats := &streamReadStats{}

	attachFirstByteDetector(reqCtx, resp, readStats, observer)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.handleErrorResponse(reqCtx, resp, hdrClone, readStats)
	}

	if handled, res, duration, err := probeEmptyOKResponse(reqCtx, resp, hdrClone, readStats); handled {
		return res, duration, err
	}

	if handled, res, duration, err := s.probeSoftErrorResponse(reqCtx, resp, hdrClone, cfg, channelType, readStats); handled {
		return res, duration, err
	}

	return s.handleSuccessResponse(reqCtx, resp, hdrClone, w, channelType, readStats)
}

// ============================================================================
// 核心转发函数
// ============================================================================

// forwardOnceAsync 异步流式转发，透明转发客户端原始请求
// 从proxy.go提取，遵循SRP原则
// 参数新增 apiKey 用于直接传递已选中的API Key（从KeySelector获取）
// 参数新增 method 用于支持任意HTTP方法（GET、POST、PUT、DELETE等）
func (s *Server) forwardOnceAsync(ctx context.Context, cfg *model.Config, apiKey string, method string, plan protocol.TransformPlan, hdr http.Header, rawQuery string, baseURL string, w http.ResponseWriter, observer *ForwardObserver) (*fwResult, float64, error) {
	// 1. 创建请求上下文（处理超时）
	reqCtx := s.newRequestContext(ctx, plan.UpstreamPath, plan.TranslatedBody)
	reqCtx.transformPlan = plan
	reqCtx.clientProtocol = plan.ClientProtocol
	reqCtx.upstreamProtocol = plan.UpstreamProtocol
	reqCtx.originalBody = plan.OriginalBody
	reqCtx.translatedBody = plan.TranslatedBody
	reqCtx.originalModel = plan.ResponseModel()
	defer reqCtx.cleanup() // [INFO] 统一清理：定时器 + context（总是安全）

	if s.protocolRegistry != nil && plan.NeedsTransform {
		translatedBody, err := s.protocolRegistry.TranslateRequest(plan.ClientProtocol, plan.UpstreamProtocol, plan.RequestModel(), plan.TranslatedBody, plan.Streaming)
		if err != nil {
			return nil, 0, fmt.Errorf("translate request for channel %d: %w", cfg.ID, err)
		}
		plan.TranslatedBody = translatedBody
		switch plan.UpstreamProtocol {
		case protocol.Gemini:
			plan.UpstreamPath = buildGeminiGeneratePath(plan.RequestModel(), plan.Streaming)
		case protocol.Anthropic:
			plan.UpstreamPath = buildAnthropicMessagesPath()
		case protocol.OpenAI:
			plan.UpstreamPath = buildOpenAIChatPath()
		case protocol.Codex:
			plan.UpstreamPath = buildCodexResponsesPath()
		}
		reqCtx.transformPlan = plan
		reqCtx.translatedBody = translatedBody
	}

	// 2. 构建上游请求
	req, err := s.buildProxyRequest(reqCtx, cfg, apiKey, method, reqCtx.transformPlan.TranslatedBody, hdr, rawQuery, reqCtx.transformPlan.UpstreamPath, baseURL)
	if err != nil {
		return nil, 0, err
	}

	// 2.5 Debug捕获：记录发送前的请求信息
	dc := s.captureDebugRequest(req, reqCtx.transformPlan.TranslatedBody)

	// 3. 发送请求
	resp, err := s.client.Do(req)

	// [INFO] 修复（2025-12）：客户端取消时主动关闭 response body，立即中断上游传输
	// 问题：streamCopy 中的 Read 阻塞时，无法立即响应 context 取消，上游继续生成完整响应
	// 解决：使用 Go 1.21+ context.AfterFunc 替代手动 goroutine（零泄漏风险）
	//   - HTTP/1.1: 关闭 TCP 连接 → 上游收到 RST，立即停止发送
	//   - HTTP/2: 发送 RST_STREAM 帧 → 取消当前 stream（不影响同连接的其他请求）
	// 效果：避免 AI 流式生成场景下，用户点"停止"后上游仍生成数千 tokens 的浪费
	if resp != nil {
		// Debug捕获：在 resp.Body 被其他层包装前，用 TeeReader 旁路捕获响应体
		dc.wrapResponseBody(resp)

		// 注意：resp.Body 后续会被包装（例如 firstByteDetector）。
		// 因此需要先把 body 封装成“稳定引用”，避免取消 goroutine 与包装赋值发生 data race。
		body := &onceCloseReadCloser{ReadCloser: resp.Body}
		resp.Body = body

		// 正常返回时关闭（Close 幂等，允许与 AfterFunc 并发触发）
		defer func() { _ = resp.Body.Close() }()

		// [INFO] 使用 context.AfterFunc 监听请求取消/超时（Go 1.21+，标准库保证无泄漏）
		// 必须监听 reqCtx.ctx（而非父 ctx），否则 nonStreamTimeout/firstByteTimeout 触发时无法强制打断阻塞 Read。
		stop := context.AfterFunc(reqCtx.ctx, func() { _ = body.Close() })
		defer stop() // 取消注册（请求正常结束时避免内存泄漏）
	}

	if err != nil {
		errRes, errDur, errErr := s.handleRequestError(reqCtx, cfg, err)
		if errRes != nil {
			errRes.DebugData = dc.buildEntry(resp)
		}
		return errRes, errDur, errErr
	}

	// 4. 处理响应(传递channelType用于精确识别usage格式,传递渠道信息用于日志记录,传递观测回调)
	var res *fwResult
	var duration float64
	res, duration, err = s.handleResponse(reqCtx, resp, w, string(reqCtx.upstreamProtocol), cfg, apiKey, observer)

	// [FIX] 2025-12: 流式传输过程中首字节超时的错误修正
	// 场景：响应头已收到(200 OK)，但在读取响应体时超时定时器触发
	// 此时 streamCopy 返回 context.Canceled，但实际原因是首字节超时
	// 需要将错误包装为 ErrUpstreamFirstByteTimeout，确保正确分类和日志记录
	if err != nil && reqCtx.firstByteTimeoutTriggered() {
		timeoutMsg := fmt.Sprintf("upstream first byte timeout after %.2fs", duration)
		if s.firstByteTimeout > 0 {
			timeoutMsg = fmt.Sprintf("%s (threshold=%v)", timeoutMsg, s.firstByteTimeout)
		}
		err = fmt.Errorf("%s: %w", timeoutMsg, util.ErrUpstreamFirstByteTimeout)
		res.Status = util.StatusFirstByteTimeout
		log.Printf("[TIMEOUT] [上游首字节超时-流传输中断] 渠道ID=%d, 阈值=%v, 实际耗时=%.2fs", cfg.ID, s.firstByteTimeout, duration)
	}

	// 5. Debug捕获：构建完整的 debug 日志条目（响应体已通过 TeeReader 收集完毕）
	if res != nil {
		res.DebugData = dc.buildEntry(resp)
	}

	return res, duration, err
}

// ============================================================================
// 单次转发尝试
// ============================================================================

func markSSEErrorForwardResult(res *fwResult) {
	res.Body = res.SSEErrorEvent
	res.Status = classifySSEErrorStatus(res.SSEErrorEvent)
	if res.Status == util.StatusQuotaExceeded {
		res.StreamDiagMsg = fmt.Sprintf("Quota Exceeded (1308): %s", safeBodyToString(res.SSEErrorEvent))
		return
	}
	res.StreamDiagMsg = fmt.Sprintf("SSE error event: %s", safeBodyToString(res.SSEErrorEvent))
}

func markIncompleteStreamForwardResult(res *fwResult) {
	res.Body = []byte(res.StreamDiagMsg)
	res.Status = util.StatusStreamIncomplete
}

func (s *Server) handleCommittedAwareProxyError(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string,
	selectedKey string,
	res *fwResult,
	duration float64,
	reqCtx *proxyRequestContext,
	deferChannelCooldown bool,
) (*proxyResult, cooldown.Action) {
	if !res.ResponseCommitted {
		return s.handleProxyErrorResponse(
			ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown,
		)
	}
	return s.handleStreamingErrorNoRetry(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
}

func (s *Server) handleSuccessfulForwardAnomaly(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string,
	selectedKey string,
	res *fwResult,
	duration float64,
	reqCtx *proxyRequestContext,
	deferChannelCooldown bool,
) (*proxyResult, cooldown.Action, bool) {
	if res.SSEErrorEvent != nil {
		log.Printf("[WARN]  [SSE错误处理] HTTP状态码200但检测到SSE error事件，触发冷却逻辑")
		markSSEErrorForwardResult(res)
		result, action := s.handleCommittedAwareProxyError(
			ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown,
		)
		return result, action, true
	}

	if res.StreamDiagMsg != "" {
		log.Printf("[WARN]  [流响应不完整] HTTP状态码200但检测到流响应不完整，触发冷却逻辑: %s", res.StreamDiagMsg)
		markIncompleteStreamForwardResult(res)
		result, action := s.handleCommittedAwareProxyError(
			ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown,
		)
		return result, action, true
	}

	return nil, cooldown.ActionReturnClient, false
}

// forwardAttempt 单次转发尝试（包含错误处理和日志记录）
// 从proxy.go提取，遵循SRP原则
// 返回：(proxyResult, nextAction)
func (s *Server) forwardAttempt(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	selectedKey string,
	reqCtx *proxyRequestContext,
	actualModel string, // [INFO] 重定向后的实际模型名称
	bodyToSend []byte,
	requestPath string, // [FIX] 2026-01: 可能经过模型名替换的请求路径
	baseURL string, // 显式传入的URL（多URL场景）
	w http.ResponseWriter,
	deferChannelCooldown bool, // 多URL场景下，非最后一个URL不应触发渠道级冷却
) (*proxyResult, cooldown.Action) {
	// 记录渠道尝试开始时间（用于日志记录，每次渠道/Key切换时更新）
	reqCtx.attemptStartTime = time.Now()
	reqCtx.baseURL = baseURL

	// 转发请求（传递实际的API Key字符串和观测回调）
	// [FIX] 2026-01: 使用传入的 requestPath（可能已替换模型名）而非 reqCtx.requestPath
	upstreamProtocol := protocol.Protocol(cfg.ResolveUpstreamProtocol(string(reqCtx.clientProtocol)))
	plan, err := protocol.BuildTransformPlan(
		reqCtx.clientProtocol,
		upstreamProtocol,
		reqCtx.requestPath,
		requestPath,
		reqCtx.body,
		bodyToSend,
		reqCtx.originalModel,
		actualModel,
		reqCtx.isStreaming,
	)
	if err != nil {
		channelID := cfg.ID
		return &proxyResult{
			status:     http.StatusInternalServerError,
			body:       []byte(err.Error()),
			channelID:  &channelID,
			succeeded:  false,
			nextAction: cooldown.ActionRetryChannel,
		}, cooldown.ActionRetryChannel
	}

	res, duration, err := s.forwardOnceAsync(ctx, cfg, selectedKey, reqCtx.requestMethod,
		plan, reqCtx.header, reqCtx.rawQuery, baseURL, w, reqCtx.observer)

	// 传递 debug 数据到 proxyRequestContext（用于日志记录）
	if res != nil && res.DebugData != nil {
		reqCtx.debugData = res.DebugData
	}

	// 处理网络错误或异常响应（如空响应）
	// [INFO] 修复：handleResponse可能返回err即使StatusCode=200（例如Content-Length=0）
	// [FIX] 2025-12: 传递 res 和 reqCtx，用于保留 499 场景下已消耗的 token 统计
	if err != nil {
		return s.handleNetworkError(
			ctx, cfg, keyIndex, actualModel, selectedKey, reqCtx.tokenID, reqCtx.clientIP,
			duration, err, res, reqCtx, deferChannelCooldown,
		)
	}

	// 处理成功响应（仅当err==nil且状态码2xx时）
	if res.Status >= 200 && res.Status < 300 {
		if result, action, handled := s.handleSuccessfulForwardAnomaly(
			ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown,
		); handled {
			return result, action
		}

		return s.handleProxySuccess(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
	}

	// 处理错误响应
	return s.handleProxyErrorResponse(
		ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown,
	)
}

// ============================================================================
// 渠道内Key重试
// ============================================================================

// tryChannelWithKeys 在单个渠道内尝试多个Key（Key级重试）
// 从proxy.go提取，遵循SRP原则
// buildCtxDoneResult 构造 ctx 取消/超时时的 proxyResult，统一 fail-fast 路径。
func buildCtxDoneResult(cfg *model.Config, ctxErr error) *proxyResult {
	status := util.StatusClientClosedRequest
	isClientCanceled := errors.Is(ctxErr, context.Canceled)
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	return &proxyResult{
		status:           status,
		body:             []byte(`{"error":"` + ctxErr.Error() + `"}`),
		channelID:        &cfg.ID,
		succeeded:        false,
		isClientCanceled: isClientCanceled,
		nextAction:       cooldown.ActionReturnClient,
	}
}

// selectKeyWithFallback 在 triedKeys 之外选 Key：先 SelectAvailableKey，
// 启用 cooldown fallback 时再 SelectCooldownFallbackKey；全部失败包装 ErrAllKeysUnavailable。
func (s *Server) selectKeyWithFallback(cfg *model.Config, apiKeys []*model.APIKey, triedKeys map[int]bool) (int, string, error) {
	keyIndex, selectedKey, selectErr := s.keySelector.SelectAvailableKey(cfg.ID, apiKeys, triedKeys)
	if selectErr != nil && cfg.CooldownFallback {
		keyIndex, selectedKey, selectErr = s.keySelector.SelectCooldownFallbackKey(cfg.ID, apiKeys, triedKeys)
	}
	if selectErr != nil {
		return 0, "", fmt.Errorf("%w: %v", ErrAllKeysUnavailable, selectErr)
	}
	return keyIndex, selectedKey, nil
}

// recordSuccessTTFBToSelector 在多URL场景的2xx响应里把TTFB回报给URLSelector，
// 单URL/非2xx/无延迟数据直接跳过。优先用 firstByteTime，缺失时回退到 duration。
func recordSuccessTTFBToSelector(selector *URLSelector, channelID int64, urlsCount int, urlStr string, result *proxyResult) {
	if urlsCount <= 1 || selector == nil || result == nil {
		return
	}
	if result.status < 200 || result.status >= 300 {
		return
	}
	ttfb := time.Duration(result.firstByteTime * float64(time.Second))
	if ttfb <= 0 {
		ttfb = time.Duration(result.duration * float64(time.Second))
	}
	if ttfb > 0 {
		selector.RecordLatency(channelID, urlStr, ttfb)
	}
}

// attemptKeyAcrossURLs 在选定 Key 上按 URL 顺序尝试上游：
//   - immediate != nil 表示调用方需立即 `return immediate, nil`（成功 / ActionReturnClient / ctx 取消）
//   - immediate == nil 时 urlLastFailure 给 Key 重试循环用于决定 continue/break
//
// 多URL场景下：失败URL会被 selector 冷却；明确 5xx（除 598 首字节超时）会立即跳出 URL 循环切换渠道，
// 并在该URL处于 deferChannelCooldown 时补做一次渠道级冷却。
func (s *Server) attemptKeyAcrossURLs(
	ctx context.Context,
	cfg *model.Config,
	urls []string,
	selector *URLSelector,
	keyIndex int,
	selectedKey string,
	reqCtx *proxyRequestContext,
	actualModel string,
	bodyToSend []byte,
	requestPath string,
	w http.ResponseWriter,
) (immediate *proxyResult, urlLastFailure *proxyResult) {
	sortedURLs := orderURLsWithSelector(selector, cfg.ID, urls)
	urlsCount := len(urls)
	for urlIdx, urlEntry := range sortedURLs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return buildCtxDoneResult(cfg, ctxErr), nil
		}

		// 更新活跃请求的当前URL（用于前端显示）
		if reqCtx.activeReqID > 0 {
			s.activeRequests.SetBaseURL(reqCtx.activeReqID, urlEntry.url)
		}

		shouldDeferChannelCooldown := urlsCount > 1 && urlIdx < len(sortedURLs)-1
		result, nextAction := s.forwardAttempt(
			ctx, cfg, keyIndex, selectedKey, reqCtx, actualModel, bodyToSend, requestPath, urlEntry.url, w, shouldDeferChannelCooldown)

		if result != nil && result.succeeded {
			// 成功：记录TTFB到URLSelector（仅多URL场景）
			recordSuccessTTFBToSelector(selector, cfg.ID, urlsCount, urlEntry.url, result)
			return result, nil
		}

		if result != nil {
			urlLastFailure = result
		}

		// Key级错误：换URL无意义，跳出URL循环
		if nextAction == cooldown.ActionRetryKey {
			break
		}
		// 客户端错误：直接返回
		if nextAction == cooldown.ActionReturnClient {
			return urlLastFailure, nil
		}
		// 渠道级错误 (ActionRetryChannel) 或网络错误：
		// 在多URL场景下，默认先尝试下一个URL
		if urlsCount > 1 {
			if selector != nil {
				selector.CooldownURL(cfg.ID, urlEntry.url)
			}

			// 新策略：上游明确返回 5xx（598 首字节超时除外）时，直接切换下一个渠道。
			// 该分支命中时，当前URL若使用了 deferChannelCooldown，需要补做一次渠道级冷却写入。
			if shouldSwitchChannelImmediatelyOnHTTP5xx(result) {
				if shouldDeferChannelCooldown && result != nil {
					input := httpErrorInputFromParts(cfg.ID, keyIndex, result.status, result.body, result.header)
					s.applyCooldownDecision(ctx, cfg, input)
				}
				break
			}
			continue // 下一个URL
		}
		// 单URL：保持原有行为
		break
	}
	return nil, urlLastFailure
}

func (s *Server) tryChannelWithKeys(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	reqCtx.channelStartTime = time.Now()

	// Fail-fast：ctx 已结束（客户端断开/请求超时）时不要再做任何 I/O（查库、选Key、发请求）。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return buildCtxDoneResult(cfg, ctxErr), nil
	}

	// 查询渠道的API Keys（缓存优先，缓存不可用自动降级到数据库查询）
	apiKeys, err := s.getAPIKeys(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get API keys: %w", err)
	}

	// 计算实际重试次数
	actualKeyCount := len(apiKeys)
	if actualKeyCount == 0 {
		return nil, fmt.Errorf("no API keys configured for channel %d", cfg.ID)
	}

	maxKeyRetries := min(s.maxKeyRetries, actualKeyCount)

	triedKeys := make(map[int]bool) // 本次请求内已尝试过的Key

	var lastFailure *proxyResult

	// 准备请求体（处理模型重定向）
	// [INFO] 修复：保存重定向后的模型名称，用于日志记录和调试
	actualModel, bodyToSend := s.prepareRequestBody(cfg, reqCtx)

	// [FIX] 2026-01: 模型名变更时同步替换 URL 路径
	// 场景：Gemini API 的模型名在 URL 路径中（如 /v1beta/models/gemini-3-flash:streamGenerateContent）
	// 如果模糊匹配将 gemini-3-flash 改为 gemini-3-flash-preview，URL 路径也需要同步更新
	requestPath := replaceModelInPath(reqCtx.requestPath, reqCtx.originalModel, actualModel)

	// 获取渠道URL列表（单URL时退化为单元素切片）
	urls := cfg.GetURLs()
	if len(urls) == 0 {
		return nil, fmt.Errorf("no valid URLs configured for channel %d", cfg.ID)
	}
	selector := s.urlSelector

	// 多URL场景：异步做TCP连接探测预热
	// 目的：通过TCP连接耗时（纯网络延迟，与模型推理无关）为URLSelector提供初始EWMA种子，
	// 避免首次请求随机选到网络延迟更高的URL。
	if len(urls) > 1 && selector != nil {
		urlsSnapshot := append([]string(nil), urls...)
		go selector.ProbeURLs(s.baseCtx, cfg.ID, urlsSnapshot)
	}

	// Key重试循环
	for range maxKeyRetries {
		// 检查context是否已取消/超时
		if ctxErr := ctx.Err(); ctxErr != nil {
			return buildCtxDoneResult(cfg, ctxErr), nil
		}

		// 选择可用的API Key（直接传入apiKeys，避免重复查询）
		keyIndex, selectedKey, selectErr := s.selectKeyWithFallback(cfg, apiKeys, triedKeys)
		if selectErr != nil {
			return nil, selectErr
		}

		// 标记Key为已尝试
		triedKeys[keyIndex] = true

		// 更新活跃请求的渠道信息（用于前端显示）
		if reqCtx.activeReqID > 0 {
			s.activeRequests.Update(reqCtx.activeReqID, cfg.ID, cfg.Name, cfg.GetChannelType(), selectedKey, reqCtx.tokenID, cfg.CostMultiplier)
		}

		// URL循环（单URL时退化为单次迭代）
		immediate, urlLastFailure := s.attemptKeyAcrossURLs(
			ctx, cfg, urls, selector,
			keyIndex, selectedKey, reqCtx, actualModel, bodyToSend, requestPath, w)
		if immediate != nil {
			return immediate, nil
		}

		// URL循环结束后的Key级决策
		if urlLastFailure != nil {
			lastFailure = urlLastFailure
			if urlLastFailure.nextAction == cooldown.ActionRetryKey {
				continue // 下一个Key
			}
			break // ActionRetryChannel 或 ActionReturnClient
		}
		break
	}

	// Key重试循环结束：返回最后一次失败结果
	if lastFailure != nil {
		return lastFailure, nil
	}

	// 所有Key都尝试过但都失败（无 lastFailure 说明循环未执行或逻辑异常）
	return nil, ErrAllKeysExhausted
}

func shouldSwitchChannelImmediatelyOnHTTP5xx(result *proxyResult) bool {
	// 仅针对“上游已返回HTTP响应”的5xx生效，避免把网络错误误判为同一策略。
	if result == nil || result.header == nil {
		return false
	}
	if result.status < 500 || result.status > 599 {
		return false
	}
	return result.status != util.StatusFirstByteTimeout
}

func shouldCheckSoftErrorForChannelType(channelType string) bool {
	switch util.NormalizeChannelType(channelType) {
	case util.ChannelTypeAnthropic, util.ChannelTypeCodex:
		return true
	default:
		return false
	}
}

// checkSoftError 检测“200 OK 但实际是错误”的软错误响应
// 原则：宁可漏判也不要误判（避免把正常响应当错误导致重试/冷却）
//
// 规则：
// - JSON：先用 bytes.Contains 短路，仅含可能错误标记时才完整 Unmarshal；只看顶层结构
// - text/plain：只接受“前缀匹配 + 短消息”，禁止 Contains 误判用户内容
// - SSE：若看起来像 SSE（data:/event:），直接跳过
func checkSoftError(data []byte, contentType string) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return false
	}

	// 非 JSON 形态下，先排除 SSE（上游可能用 text/plain 返回 SSE）
	if trimmed[0] != '{' {
		if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) ||
			bytes.Contains(data, []byte("\ndata:")) || bytes.Contains(data, []byte("\nevent:")) {
			return false
		}
	}

	ctLower := strings.ToLower(contentType)
	isJSONCT := strings.Contains(ctLower, "application/json")

	// JSON：仅看顶层结构
	if isJSONCT || trimmed[0] == '{' {
		// 快速短路：99% 成功响应顶层不含错误标记，跳过 sonic.Unmarshal
		// 同时覆盖紧凑/带空格两种格式；"error" 带引号避免误匹配 "api_error" 等子串
		if !maybeContainsTopLevelError(trimmed) {
			if trimmed[0] == '{' {
				return false // 形态确实是 JSON 对象 → 已确认无错误
			}
			// CT=JSON 但内容不像 JSON 对象（如纯文本错误消息）→ 走兜底
		} else {
			var obj map[string]any
			if err := sonic.Unmarshal(trimmed, &obj); err == nil {
				if v, ok := obj["error"]; ok && v != nil {
					return true
				}
				if t, ok := obj["type"].(string); ok && strings.EqualFold(t, "error") {
					return true
				}
				return false
			}
			// 形态像 JSON（以 '{' 开头）但解析失败：不猜，避免误判
			if trimmed[0] == '{' {
				return false
			}
			// Content-Type 标注为 JSON 但内容不是 JSON：允许继续走 text/plain 的“前缀+短消息”兜底
		}
	}

	// text/plain：仅前缀 + 短消息
	const maxPlainLen = 256
	if len(trimmed) > maxPlainLen {
		return false
	}
	if bytes.HasPrefix(trimmed, []byte("当前模型负载过高")) {
		return true
	}
	if bytes.HasPrefix(trimmed, []byte("Current model load too high")) {
		return true
	}

	return false
}

// maybeContainsTopLevelError 字节级扫描快速判断响应体是否可能含顶层 error 标记。
// 假阳性（如 {"errors":[...]} 含 "error" 子串）会进入慢路径精确判定，结果仍正确。
func maybeContainsTopLevelError(data []byte) bool {
	return bytes.Contains(data, []byte(`"error"`)) ||
		bytes.Contains(data, []byte(`"type":"error"`)) ||
		bytes.Contains(data, []byte(`"type": "error"`))
}
