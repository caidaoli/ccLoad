package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/privacy"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/google/uuid"
)

const cursorSDKBridgeRequestURL = "http://cursor-sdk-bridge/sdk.v1.SdkAgentService/CreateAgent+Send"

// tryCursorOAuthChannel runs inference through the managed Cursor SDK bridge.
// Built-in tools stay disabled. Client tools are registered as native Cursor
// custom tools; their callbacks suspend inside ccLoad until the client returns
// tool_result / role=tool on the next turn.
func (s *Server) tryCursorOAuthChannel(
	ctx context.Context,
	cfg *model.Config,
	reqCtx *proxyRequestContext,
	w http.ResponseWriter,
) (*proxyResult, error) {
	if reqCtx != nil && !cursorSupportsRequestFamily(reqCtx.requestPath) {
		return &proxyResult{
			status: http.StatusBadRequest, channelID: &cfg.ID, succeeded: false,
			protocolCapabilityMissing: true, nextAction: cooldown.ActionRetryChannel,
			body: []byte(`{"error":{"message":"Cursor OAuth supports Anthropic messages, OpenAI chat completions, OpenAI responses, and Gemini generateContent","type":"invalid_request_error"}}`),
		}, nil
	}
	if s.cursorCredentials == nil {
		return oauthCredentialUnavailableResult(cfg, "Cursor"), nil
	}
	credential, err := s.cursorCredentials.credential(ctx, cfg, false)
	if credential == nil {
		if err != nil {
			s.cooldownRejectedOAuthCredential(ctx, cfg, "Cursor")
		}
		return oauthCredentialUnavailableResult(cfg, "Cursor"), nil
	}
	for attempt := range 2 {
		result, forwardErr := s.forwardCursorAgent(ctx, cfg, credential, reqCtx, w)
		if forwardErr != nil || result == nil {
			return result, forwardErr
		}
		credentialRejected := result.status == http.StatusUnauthorized &&
			(result.nextAction == cooldown.ActionRetryChannel || result.succeeded)
		if !credentialRejected {
			return result, nil
		}
		if attempt == 1 {
			s.cooldownRejectedOAuthCredential(ctx, cfg, "Cursor")
			return result, nil
		}
		refreshed, refreshErr := s.cursorCredentials.credentialAfterUnauthorized(ctx, cfg, credential.AccessToken)
		if refreshErr != nil {
			s.cooldownRejectedOAuthCredential(ctx, cfg, "Cursor")
			return result, nil
		}
		credential = refreshed
		if result.succeeded {
			// The SSE error is already committed. Rotate the rejected credential
			// for the next request, but never replay this run.
			return result, nil
		}
		if s.activeRequests != nil {
			s.activeRequests.Retry(reqCtx.activeReqID)
		}
	}
	return nil, errors.New("cursor credential retry loop exhausted")
}

/**
 * @brief 执行一轮 Cursor 请求，向客户端报告当前轮用量并累计真实增量计费。
 * @param ctx 请求上下文；取消时停止当前附着请求。
 * @param cfg 当前 Cursor 渠道配置。
 * @param credential 经过验证的 Cursor 凭证。
 * @param reqCtx 代理请求与计费上下文。
 * @param w 客户端响应写入器。
 * @return 当前轮代理结果，包含成功响应或可分类的上游失败。
 * @return 已分类失败通过代理结果返回，此处始终为 nil。
 * @details SDK 独立 usage 和终态补差都可能包含历史工具轮；恢复轮的客户端用量始终按本次请求估算，累计量仅用于计费。
 * @details 首内容耗时至少记为 1ns，避免时钟精度使已收到内容与尚未收到内容共用零值；心跳不计入首内容。
 * @note 写入客户端响应、代理日志和令牌统计；重放结果不重复记账。
 */
func (s *Server) forwardCursorAgent(
	ctx context.Context,
	cfg *model.Config,
	credential *cursorauth.Credential,
	reqCtx *proxyRequestContext,
	w http.ResponseWriter,
) (*proxyResult, error) {
	// The SDK only reads chat-shaped requests. Always translate from the client
	// body: proxy setup seeds translatedBody with the untranslated request.
	translatedClient := cursorTranslatedClientProtocol(reqCtx.requestPath)
	if translatedClient != "" {
		if s.protocolRegistry == nil {
			return cursorErrorResult(
				cfg,
				http.StatusServiceUnavailable,
				"protocol registry is unavailable for Cursor "+string(translatedClient)+" requests",
				cooldown.ActionReturnClient,
			), nil
		}
		translatedBody, err := s.protocolRegistry.TranslateRequest(
			translatedClient,
			protocol.OpenAI,
			reqCtx.originalModel,
			reqCtx.body,
			reqCtx.isStreaming,
		)
		if err != nil {
			return cursorClientErrorResult(cfg, http.StatusBadRequest, err.Error()), nil
		}
		reqCtx.translatedBody = translatedBody
	}
	body := reqCtx.body
	if len(reqCtx.translatedBody) > 0 {
		body = reqCtx.translatedBody
	}
	originalResponseBody := reqCtx.body
	request := cursorauth.ParseRequest(body)
	request.InputTokenEstimate = estimateCursorInputTokens(request)
	if request.Prompt == "" && len(request.ToolResults) == 0 {
		return cursorClientErrorResult(cfg, http.StatusBadRequest, "cursor prompt is required"), nil
	}
	requested := request.Model
	if requested == "" {
		requested = reqCtx.originalModel
	}
	selected := reqCtx.attemptModel
	if selected.logicalModel == "" && requested != "" {
		selected, _ = s.firstModelRow(cfg, requested)
	}
	modelID := s.resolveFinalUpstreamModel(cfg, selected, string(reqCtx.clientProtocol))
	started := time.Now()
	reqCtx.attemptStartTime, reqCtx.privacyMasked = started, privacy.Counts{}
	if s.activeRequests != nil {
		reqCtx.activeReqID = s.activeRequests.BeginAttempt(reqCtx.activeReqID, activeRequestAttempt{
			StartTime:        started,
			Model:            reqCtx.originalModel,
			ClientIP:         reqCtx.clientIP,
			Streaming:        reqCtx.isStreaming,
			ChannelID:        cfg.ID,
			ChannelName:      cfg.Name,
			ClientProtocol:   string(reqCtx.clientProtocol),
			UpstreamProtocol: "cursor-sdk-bridge",
			APIKey:           credential.APIKey,
			TokenID:          reqCtx.tokenID,
			BaseURL:          cursorSDKBridgeRequestURL,
			CostMultiplier:   cfg.CostMultiplier,
			ThinkingEffort:   reqCtx.thinkingEffort,
		})
		activeReqID := reqCtx.activeReqID
		defer s.activeRequests.Remove(activeReqID)
	}
	debugCapture := s.captureCursorSDKRequest(reqCtx, modelID, request.Prompt)
	if debugCapture != nil {
		w = debugCapture.wrapTranslatedResponseWriter(w)
	}
	if s.activeRequests != nil {
		s.activeRequests.SetDebugCapture(reqCtx.activeReqID, debugCapture)
	}
	runner := s.cursorRunnerSnapshot()
	if runner == nil {
		result := cursorErrorResult(
			cfg,
			http.StatusServiceUnavailable,
			"cursor SDK runner is unavailable",
			cooldown.ActionReturnClient,
		)
		finishCursorSDKDebug(reqCtx, debugCapture, result.status, result.body, errors.New("cursor SDK runner is unavailable"))
		return result, nil
	}

	streaming := reqCtx.isStreaming
	timeoutCtx := newRequestContextForStreaming(ctx, streaming, protocolTimeoutConfig{
		FirstByteTimeout: s.firstByteTimeout,
		StreamTimeout:    s.streamTimeout,
		NonStreamTimeout: s.nonStreamTimeout,
	})
	defer timeoutCtx.cleanup()
	classifyTimeout := func(runErr error) (int, error, bool) {
		timedOut := timeoutCtx.firstByteTimeoutTriggered() || timeoutCtx.streamTimeoutTriggered() ||
			(!streaming && timeoutCtx.ctx.Err() != nil &&
				errors.Is(context.Cause(timeoutCtx.ctx), context.DeadlineExceeded))
		if !timedOut {
			return 0, runErr, false
		}
		failed, _, timeoutErr := s.handleRequestError(timeoutCtx, cfg, runErr)
		return failed.Status, timeoutErr, true
	}
	runBaseCtx := timeoutCtx.ctx
	if debugCapture != nil {
		runBaseCtx = cursorauth.WithRawResponseCapture(runBaseCtx)
	}
	runCtx, cancelRun := context.WithCancel(runBaseCtx)
	defer cancelRun()
	request.Model = modelID
	if session := privacySessionFromContext(ctx); session != nil && session.Enabled() {
		sent, _ := json.Marshal(request)
		reqCtx.privacyMasked = session.CountSent(sent)
	}
	events, err := runner.Run(runCtx, credential, request)
	if err != nil {
		status := http.StatusBadGateway
		action := cooldown.ActionRetryChannel
		if timeoutStatus, timeoutErr, timedOut := classifyTimeout(err); timedOut {
			status = timeoutStatus
			err = timeoutErr
		} else if isClientDisconnectError(err) {
			status = StatusClientClosedRequest
			action = cooldown.ActionReturnClient
		} else if errors.Is(err, cursorauth.ErrAgentMissing) {
			status = http.StatusServiceUnavailable
			action = cooldown.ActionReturnClient
		} else if errors.Is(err, cursorauth.ErrMissingAPIKey) {
			status = http.StatusUnauthorized
			action = cooldown.ActionReturnClient
		} else if cursorauth.IsCredentialRejected(err) {
			status = http.StatusUnauthorized
		}
		result := cursorErrorResult(cfg, status, err.Error(), action)
		result.duration = time.Since(started).Seconds()
		result.isClientCanceled = status == StatusClientClosedRequest
		result.proxyLogWritten = !reqCtx.skipProxyLog
		finishCursorSDKDebug(reqCtx, debugCapture, status, result.body, err)
		failed := &fwResult{Status: status, Body: result.body}
		result.forwarded = failed
		if !reqCtx.skipProxyLog {
			duration := time.Since(started).Seconds()
			s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", status, duration, failed, err.Error())
			s.updateTokenStatsForProxy(reqCtx, failureTokenStatsOutcome(status), duration, failed, modelID)
		}
		return result, nil
	}

	format := cursorResponseFormat(reqCtx)
	msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var responseTransformState any
	var responseTransformErr error
	if streaming && translatedClient != "" && s.protocolRegistry != nil {
		_, responseTransformErr = s.protocolRegistry.TranslateResponseStream(ctx, protocol.OpenAI, translatedClient,
			modelID, originalResponseBody, body, nil, &responseTransformState)
	}
	var out bytes.Buffer
	var thinking strings.Builder
	var content []map[string]any
	var anthropicStream cursorAnthropicStream
	full := ""
	firstByte := time.Duration(0)
	wroteHeader := false
	streamedPlain := 0
	var calls []cursorauth.ToolCall
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	ensureStream := func() {
		if !streaming || wroteHeader {
			return
		}
		disableResponseWriteTimeout(w, "Cursor SDK stream")
		header := w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		wroteHeader = true
		if format == "anthropic" {
			_, _ = w.Write(cursorAnthropicStart(msgID, modelID))
		}
	}
	writeStream := func(chunk, kind string) {
		if !streaming || chunk == "" || responseTransformErr != nil {
			return
		}
		ensureStream()
		var payload []byte
		if format == "anthropic" {
			payload = anthropicStream.delta(chunk, kind)
		} else if kind == "thinking" {
			payload = cursorOpenAIChunk(msgID, modelID, "", chunk, "", nil)
		} else {
			payload = cursorOpenAIChunk(msgID, modelID, chunk, "", "", nil)
		}
		if translatedClient != "" {
			responseTransformErr = writeCursorTranslatedStream(
				ctx, s.protocolRegistry, w, translatedClient, modelID, originalResponseBody, body, payload, &responseTransformState,
			)
			if responseTransformErr != nil {
				cancelRun()
			}
		} else {
			_, _ = w.Write(payload)
		}
		flush()
	}
	writePing := func() {
		if !streaming || responseTransformErr != nil {
			return
		}
		ensureStream()
		// Registry translation swallows SSE comments; write the keepalive
		// directly so clients still see traffic before the first token.
		_, _ = w.Write(cursorStreamPing(format))
		flush()
	}

	appendContent := func(chunk, kind string) {
		if streaming || format != "anthropic" || chunk == "" {
			return
		}
		if last := len(content) - 1; last >= 0 && content[last]["type"] == kind {
			content[last][kind] = content[last][kind].(string) + chunk
		} else {
			content = append(content, map[string]any{"type": kind, kind: chunk})
		}
	}

	var runErr error
	var usage *cursorauth.Usage
	var turnUsage *cursorauth.Usage
	var billableUsage *cursorauth.Usage
	replayed := false
	for event := range events {
		if debugCapture != nil && debugCapture.respBuf != nil && len(event.RawResponse) > 0 {
			_, _ = debugCapture.respBuf.Write(event.RawResponse)
			_, _ = debugCapture.respBuf.Write([]byte("\n"))
		}
		if firstByte == 0 && (event.Delta != "" || event.ThinkingDelta != "" || event.Text != "" || event.ToolCall != nil || (event.Done && event.Err == nil)) {
			firstByte = positiveDuration(time.Since(started))
			timeoutCtx.stopFirstByteTimer()
			if s.activeRequests != nil {
				s.activeRequests.SetClientFirstByteTime(reqCtx.activeReqID, firstByte)
			}
		}
		previousFull := full
		if event.Text != "" {
			full = event.Text
		} else if event.Delta != "" {
			out.WriteString(event.Delta)
			full = out.String()
		}
		if !streaming {
			thinking.WriteString(event.ThinkingDelta)
		}
		appendContent(event.ThinkingDelta, "thinking")
		textDelta := event.Delta
		if textDelta == "" && strings.HasPrefix(full, previousFull) {
			textDelta = full[len(previousFull):]
		}
		appendContent(textDelta, "text")
		if s.activeRequests != nil {
			s.activeRequests.AddBytes(reqCtx.activeReqID, int64(len(textDelta)+len(event.ThinkingDelta)))
		}
		if event.Err != nil {
			runErr = event.Err
		}
		if event.Usage != nil {
			if event.UsageEstimated {
				if turnUsage == nil {
					usage = event.Usage
				}
			} else {
				if !event.Replayed {
					if billableUsage == nil {
						billableUsage = &cursorauth.Usage{}
					}
					billableUsage.Add(event.Usage)
				}
				if !event.Done {
					if turnUsage == nil {
						turnUsage = &cursorauth.Usage{}
					}
					turnUsage.Add(event.Usage)
					usage = turnUsage
				} else if turnUsage == nil {
					usage = event.Usage
				}
			}
		}
		replayed = replayed || event.Replayed
		if event.ToolCall != nil {
			calls = append(calls, *event.ToolCall)
		}
		if event.Ping && responseTransformErr == nil {
			timeoutCtx.stopFirstByteTimer()
			writePing()
		}
		if streaming && event.ThinkingDelta != "" && responseTransformErr == nil {
			writeStream(event.ThinkingDelta, "thinking")
		}
		if streaming && event.Delta != "" && responseTransformErr == nil {
			writeStream(event.Delta, "text")
			streamedPlain += len(event.Delta)
		}
		if responseTransformErr != nil {
			cancelRun()
		}
	}
	duration := time.Since(started).Seconds()
	finishRunFailure := func(runErr error) (*proxyResult, error) {
		status := http.StatusBadGateway
		action := cooldown.ActionRetryChannel
		timeoutStatus, timeoutErr, timedOut := classifyTimeout(runErr)
		if timedOut {
			status = timeoutStatus
			runErr = timeoutErr
		}
		clientDisconnected := !timedOut && isClientDisconnectError(runErr)
		if clientDisconnected {
			status = StatusClientClosedRequest
			action = cooldown.ActionReturnClient
		} else if cursorauth.IsCredentialRejected(runErr) {
			status = http.StatusUnauthorized
		}
		if streaming && wroteHeader && !clientDisconnected {
			if format == "responses" {
				// Preserve the run failure; EOF validation would replace it with a
				// missing-terminator error. Conversion failures were already emitted.
				if protocol.ResponseToolInputError(responseTransformState) == nil {
					_, _ = w.Write(cursorResponsesStreamError(runErr))
				}
			} else {
				_, _ = w.Write([]byte("data: {\"error\":{\"message\":" + jsonString(runErr.Error()) + "}}\n\n"))
			}
			flush()
		}
		result := cursorErrorResult(cfg, status, runErr.Error(), action)
		result.duration = duration
		result.firstByteTime = firstByte.Seconds()
		finishCursorSDKDebug(reqCtx, debugCapture, status, result.body, runErr)
		failed := &fwResult{
			Status: status, Body: result.body, FirstByteTime: firstByte.Seconds(),
			ResponseCommitted: wroteHeader,
		}
		applyCursorUsage(failed, billableUsage, request, full, calls)
		result.forwarded = failed
		if !reqCtx.skipProxyLog && !replayed {
			s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", status, duration, failed, runErr.Error())
			s.updateTokenStatsForProxy(reqCtx, failureTokenStatsOutcome(status), duration, failed, modelID)
		}
		result.proxyLogWritten = !reqCtx.skipProxyLog && !replayed
		result.isClientCanceled = clientDisconnected
		if streaming && wroteHeader && !clientDisconnected {
			// The SSE envelope is already on the wire; the attempt loop must not
			// write a second JSON body.
			result.succeeded = true
			result.nextAction = cooldown.ActionReturnClient
		}
		return result, nil
	}
	if responseTransformErr != nil {
		runErr = responseTransformErr
	}
	if runErr == nil && timeoutCtx.ctx.Err() != nil {
		runErr = context.Cause(timeoutCtx.ctx)
	}
	if runErr != nil {
		return finishRunFailure(runErr)
	}

	plain := full
	if len(request.ToolResults) > 0 {
		usage = estimatedCursorResponseUsage(request.InputTokenEstimate, plain, calls)
	}

	var responseBody []byte
	var header http.Header
	if streaming {
		if format == "anthropic" {
			ensureStream()
			if streamedPlain == 0 && plain != "" {
				writeStream(plain, "text")
			}
			_, _ = w.Write(anthropicStream.finish(calls, usage))
		} else {
			ensureStream()
			if len(plain) > streamedPlain {
				writeStream(plain[streamedPlain:], "text")
				streamedPlain = len(plain)
			}
			finish := cursorOpenAIFinish(msgID, modelID, plain, calls, streamedPlain > 0, usage)
			if translatedClient != "" {
				responseTransformErr = writeCursorTranslatedStream(
					ctx, s.protocolRegistry, w, translatedClient, modelID, originalResponseBody, body, finish, &responseTransformState,
				)
				if responseTransformErr == nil {
					responseTransformErr = writeCursorTranslatedStream(
						ctx, s.protocolRegistry, w, translatedClient, modelID, originalResponseBody, body,
						[]byte("data: [DONE]\n\n"), &responseTransformState,
					)
				}
			} else {
				_, _ = w.Write(finish)
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}
		}
		if responseTransformErr != nil {
			return finishRunFailure(responseTransformErr)
		}
		flush()
		responseBody = []byte(plain)
		header = w.Header()
	} else {
		if format == "anthropic" {
			responseBody = cursorAnthropicMessage(msgID, modelID, content, calls, usage)
		} else {
			responseBody = cursorOpenAIMessage(msgID, modelID, plain, thinking.String(), calls, usage)
		}
		if translatedClient != "" {
			responseBody, responseTransformErr = s.protocolRegistry.TranslateResponseNonStream(
				ctx, protocol.OpenAI, translatedClient, modelID, originalResponseBody, body, responseBody,
			)
		}
		if responseTransformErr != nil {
			return finishRunFailure(responseTransformErr)
		}
		header = make(http.Header)
		header.Set("Content-Type", "application/json")
		writeResponseWithHeaders(w, http.StatusOK, header, responseBody)
	}

	channelID := cfg.ID
	finishCursorSDKDebug(reqCtx, debugCapture, http.StatusOK, responseBody, nil)
	forwarded := &fwResult{
		Status: http.StatusOK, Header: header, Body: responseBody, FirstByteTime: firstByte.Seconds(),
	}
	applyCursorUsage(forwarded, billableUsage, request, plain, calls)
	if !reqCtx.skipProxyLog && !replayed {
		s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", http.StatusOK, duration, forwarded, "")
		s.updateTokenStatsForProxy(reqCtx, model.TokenStatsSuccess, duration, forwarded, modelID)
	}
	return &proxyResult{
		status: http.StatusOK, header: header, body: responseBody, channelID: &channelID,
		forwarded: forwarded,
		duration:  duration, firstByteTime: firstByte.Seconds(), succeeded: true,
		nextAction: cooldown.ActionReturnClient, proxyLogWritten: !reqCtx.skipProxyLog && !replayed,
	}, nil
}

func (s *Server) captureCursorSDKRequest(
	reqCtx *proxyRequestContext,
	modelID, prompt string,
) *debugCapture {
	if s == nil || s.configService == nil || reqCtx == nil {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{
		"create_agent": map[string]any{
			"model": map[string]any{"id": modelID},
			"tools": map[string]any{"names": []string{}},
		},
		"send": map[string]any{"message": map[string]any{"text": prompt}},
	})
	request, err := http.NewRequest(
		http.MethodPost,
		cursorSDKBridgeRequestURL,
		nil,
	)
	if err != nil {
		return nil
	}
	request.Header.Set("Content-Type", "application/json")
	capture := s.captureDebugRequest(request, payload)
	if capture == nil {
		return nil
	}
	capture.markProtocolTransform(reqCtx.requestPath, reqCtx.header, reqCtx.body)
	return capture
}

func finishCursorSDKDebug(
	reqCtx *proxyRequestContext,
	capture *debugCapture,
	status int,
	translatedBody []byte,
	runErr error,
) {
	if reqCtx == nil || capture == nil {
		return
	}
	responseHeader := make(http.Header)
	responseHeader.Set("Content-Type", "application/x-ndjson")
	response := &http.Response{StatusCode: status, Header: responseHeader}
	capture.captureResponseMeta(response)
	if runErr != nil {
		capture.captureUpstreamError(runErr)
	}
	if capture.translatedResponseBuf != nil && len(capture.translatedResponseBuf.Snapshot()) == 0 {
		translatedHeader := make(http.Header)
		translatedHeader.Set("Content-Type", "application/json")
		capture.captureTranslatedResponseMeta(status, translatedHeader)
		capture.captureTranslatedResponse(translatedBody)
	}
	reqCtx.debugData = capture.buildEntry(response)
}

func cursorResponseFormat(reqCtx *proxyRequestContext) string {
	if reqCtx == nil {
		return "openai"
	}
	if protocol.DetectRequestFamily(reqCtx.requestPath) == protocol.RequestFamilyResponses {
		return "responses"
	}
	if util.NormalizeProtocol(string(reqCtx.clientProtocol)) == util.ProtocolAnthropic ||
		strings.Contains(reqCtx.requestPath, "/messages") {
		return "anthropic"
	}
	return "openai"
}

func cursorClientErrorResult(cfg *model.Config, status int, message string) *proxyResult {
	return cursorErrorResult(cfg, status, message, cooldown.ActionReturnClient)
}

func cursorErrorResult(cfg *model.Config, status int, message string, action cooldown.Action) *proxyResult {
	channelID := cfg.ID
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "api_error"},
	})
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &proxyResult{
		status: status, header: header, body: body, channelID: &channelID,
		succeeded: false, nextAction: action,
	}
}

/**
 * @brief 开始 Anthropic 消息，内容块等到首个正文或思考增量时创建。
 * @param id 消息标识。
 * @param modelID 上游模型标识。
 * @return message_start SSE。
 */
func cursorAnthropicStart(id, modelID string) []byte {
	start, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "content": []any{},
			"model": modelID, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	return []byte("event: message_start\ndata: " + string(start) + "\n\n")
}

/**
 * @brief 跟踪 Cursor Anthropic 流当前内容块，按上游顺序切换思考与正文。
 */
type cursorAnthropicStream struct {
	index int    ///< 当前内容块或下一个待创建块的索引。
	kind  string ///< 当前打开块的类型；空字符串表示没有打开的块。
}

/**
 * @brief 结束当前块并推进索引；尚未打开内容块时不输出事件。
 * @return 当前块的停止事件；没有打开的块时为 nil。
 * @note 修改流的块索引与类型。
 */
func (s *cursorAnthropicStream) closeBlock() []byte {
	if s.kind == "" {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": s.index})
	s.index++
	s.kind = ""
	return []byte("event: content_block_stop\ndata: " + string(payload) + "\n\n")
}

/**
 * @brief 按内容类型创建或切换块，并输出增量；不为 SDK 未提供的思考签名生成占位值。
 * @param text 非空内容增量。
 * @param kind 内容类型，仅为 text 或 thinking。
 * @return 顺序排列的块边界与增量 SSE。
 * @note 修改流的块索引与类型。
 */
func (s *cursorAnthropicStream) delta(text, kind string) []byte {
	var b bytes.Buffer
	if s.kind != kind {
		b.Write(s.closeBlock())
		start, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": s.index,
			"content_block": map[string]any{"type": kind, kind: ""},
		})
		b.WriteString("event: content_block_start\ndata: " + string(start) + "\n\n")
		s.kind = kind
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": s.index,
		"delta": map[string]any{"type": kind + "_delta", kind: text},
	})
	b.WriteString("event: content_block_delta\ndata: " + string(payload) + "\n\n")
	return b.Bytes()
}

func cursorStreamPing(format string) []byte {
	if format == "anthropic" {
		return []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
	}
	return []byte(": ping\n\n")
}

func normalizedCursorToolArguments(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(bytes.Clone(raw))
}

/**
 * @brief 关闭当前内容块，输出工具调用及本轮终态。
 * @param calls 当前轮工具调用，按接收顺序排列。
 * @param usage 当前轮用量；nil 时输出零计数。
 * @return 工具内容块及消息结束 SSE。
 * @note 修改流索引，调用后当前轮已结束。
 */
func (s *cursorAnthropicStream) finish(calls []cursorauth.ToolCall, usage *cursorauth.Usage) []byte {
	var b bytes.Buffer
	b.Write(s.closeBlock())
	for _, call := range calls {
		blockIndex := s.index
		s.index++
		input := normalizedCursorToolArguments(call.Arguments)
		start, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": blockIndex,
			"content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}},
		})
		delta, _ := json.Marshal(map[string]any{
			"type": "content_block_delta", "index": blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)},
		})
		blockStop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": blockIndex})
		b.WriteString("event: content_block_start\ndata: ")
		b.Write(start)
		b.WriteString("\n\n")
		b.WriteString("event: content_block_delta\ndata: ")
		b.Write(delta)
		b.WriteString("\n\n")
		b.WriteString("event: content_block_stop\ndata: ")
		b.Write(blockStop)
		b.WriteString("\n\n")
	}
	reason := "end_turn"
	if len(calls) > 0 {
		reason = "tool_use"
	}
	messageDelta, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": cursorAnthropicUsage(usage),
	})
	b.WriteString("event: message_delta\ndata: ")
	b.Write(messageDelta)
	b.WriteString("\n\n")
	messageStop, _ := json.Marshal(map[string]any{"type": "message_stop"})
	b.WriteString("event: message_stop\ndata: ")
	b.Write(messageStop)
	b.WriteString("\n\n")
	return b.Bytes()
}

/**
 * @brief 生成保留思考与正文顺序的 Anthropic 非流响应。
 * @param id 消息标识。
 * @param modelID 上游模型标识。
 * @param content 当前轮按顺序累计的思考与正文块。
 * @param calls 当前轮工具调用。
 * @param usage 当前轮用量；nil 时输出零计数。
 * @return Anthropic 消息 JSON。
 */
func cursorAnthropicMessage(id, modelID string, content []map[string]any, calls []cursorauth.ToolCall, usage *cursorauth.Usage) []byte {
	if len(content) == 0 && len(calls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	for _, call := range calls {
		input := normalizedCursorToolArguments(call.Arguments)
		content = append(content, map[string]any{
			"type": "tool_use", "id": call.ID, "name": call.Name, "input": input,
		})
	}
	reason := "end_turn"
	if len(calls) > 0 {
		reason = "tool_use"
	}
	body, _ := json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant",
		"content": content, "model": modelID, "stop_reason": reason, "stop_sequence": nil,
		"usage": cursorAnthropicUsage(usage),
	})
	return body
}

/**
 * @brief 编码独立的 OpenAI 正文与思考增量，以及可选终态和用量。
 * @param id 消息标识。
 * @param modelID 上游模型标识。
 * @param content 正文增量；空字符串时省略。
 * @param thinking 思考增量；空字符串时省略。
 * @param finish 结束原因；空字符串时表示未结束。
 * @param usage 当前轮用量；nil 时省略。
 * @return OpenAI chat completion SSE。
 */
func cursorOpenAIChunk(id, modelID, content, thinking, finish string, usage *cursorauth.Usage) []byte {
	delta := map[string]any{}
	var finishReason any
	if content != "" {
		delta["content"] = content
	}
	if thinking != "" {
		delta["reasoning_content"] = thinking
	}
	if finish != "" {
		finishReason = finish
	}
	payloadObject := map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": modelID,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	}
	if usage != nil {
		payloadObject["usage"] = cursorOpenAIUsage(usage)
	}
	payload, _ := json.Marshal(payloadObject)
	return []byte("data: " + string(payload) + "\n\n")
}

/**
 * @brief 输出尚未发送的正文、当前轮工具调用和 OpenAI 终态。
 * @param id 消息标识。
 * @param modelID 上游模型标识。
 * @param text 当前轮正文。
 * @param calls 当前轮工具调用。
 * @param textAlreadyStreamed 正文是否已通过增量发送。
 * @param usage 当前轮用量；nil 时省略。
 * @return 不含 DONE 标记的 OpenAI 收尾 SSE。
 */
func cursorOpenAIFinish(id, modelID, text string, calls []cursorauth.ToolCall, textAlreadyStreamed bool, usage *cursorauth.Usage) []byte {
	var b bytes.Buffer
	if !textAlreadyStreamed && text != "" && len(calls) == 0 {
		b.Write(cursorOpenAIChunk(id, modelID, text, "", "", nil))
	}
	for i, call := range calls {
		args := "{}"
		if len(call.Arguments) > 0 {
			args = string(call.Arguments)
		}
		delta := map[string]any{
			"tool_calls": []any{map[string]any{
				"index": i, "id": openaiToolCallID(call.ID), "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": args},
			}},
		}
		payload, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-" + id, "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": modelID,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
		})
		b.WriteString("data: ")
		b.Write(payload)
		b.WriteString("\n\n")
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	b.Write(cursorOpenAIChunk(id, modelID, "", "", finish, usage))
	return b.Bytes()
}

/**
 * @brief 生成正文与思考分离的 OpenAI 非流响应。
 * @param id 消息标识。
 * @param modelID 上游模型标识。
 * @param text 当前轮正文。
 * @param thinking 当前轮思考；为空时省略 reasoning_content。
 * @param calls 当前轮工具调用。
 * @param usage 当前轮用量；nil 时输出零计数。
 * @return OpenAI chat completion JSON。
 */
func cursorOpenAIMessage(id, modelID, text, thinking string, calls []cursorauth.ToolCall, usage *cursorauth.Usage) []byte {
	message := map[string]any{"role": "assistant", "content": text}
	if thinking != "" {
		message["reasoning_content"] = thinking
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
		if text == "" {
			message["content"] = nil
		}
		toolCalls := make([]any, 0, len(calls))
		for _, call := range calls {
			args := "{}"
			if len(call.Arguments) > 0 {
				args = string(call.Arguments)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": openaiToolCallID(call.ID), "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": args},
			})
		}
		message["tool_calls"] = toolCalls
	}
	body, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion",
		"created": time.Now().Unix(), "model": modelID,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   cursorOpenAIUsage(usage),
	})
	return body
}

func cursorAnthropicUsage(usage *cursorauth.Usage) map[string]any {
	if usage == nil {
		usage = &cursorauth.Usage{}
	}
	result := map[string]any{
		"input_tokens":                usage.InputTokens,
		"output_tokens":               usage.OutputTokens,
		"cache_read_input_tokens":     usage.CacheReadTokens,
		"cache_creation_input_tokens": usage.CacheWriteTokens,
	}
	if usage.ReasoningTokens > 0 {
		result["reasoning_tokens"] = usage.ReasoningTokens
	}
	return result
}

func cursorOpenAIUsage(usage *cursorauth.Usage) map[string]any {
	if usage == nil {
		usage = &cursorauth.Usage{}
	}
	promptTokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	result := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": usage.OutputTokens,
		"total_tokens":      promptTokens + usage.OutputTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":          usage.CacheReadTokens,
			"cached_creation_tokens": usage.CacheWriteTokens,
		},
	}
	if usage.ReasoningTokens > 0 {
		result["completion_tokens_details"] = map[string]any{"reasoning_tokens": usage.ReasoningTokens}
	}
	return result
}

/**
 * @brief 应用 Cursor 真实消费，缺少上游报告时单独记录展示估算。
 * @param result 已启动 Cursor run 的转发结果，必须非 nil。
 * @param usage 本次请求已收到的真实增量合计；nil 表示没有真实报告。
 * @param request 已发送的请求及其输入估算。
 * @param text 当前轮已生成的文本。
 * @param calls 当前轮已生成的工具调用。
 * @note 估算不写入真实 Token 字段，调用方不得在认证或发送前失败时调用。
 */
func applyCursorUsage(result *fwResult, usage *cursorauth.Usage, request cursorauth.Request, text string, calls []cursorauth.ToolCall) {
	if usage == nil {
		estimated := estimatedCursorResponseUsage(request.InputTokenEstimate, text, calls)
		result.UsageEstimate = &model.TokenUsageEstimate{InputTokens: estimated.InputTokens, OutputTokens: estimated.OutputTokens}
		return
	}
	result.InputTokens = usage.InputTokens
	result.OutputTokens = usage.OutputTokens
	result.ReasoningTokens = usage.ReasoningTokens
	result.CacheReadInputTokens = usage.CacheReadTokens
	result.CacheCreationInputTokens = usage.CacheWriteTokens
	// Cursor exposes one aggregate cache-write counter. ccLoad prices aggregate
	// cache creation at the standard 5-minute write rate, matching other
	// providers that do not report a TTL breakdown.
	result.Cache5mInputTokens = usage.CacheWriteTokens
}

/**
 * @brief 在 Cursor 未报告真实用量时提供客户端上下文与日志展示的估算依据。
 * @param request 已解析的提示词和客户端工具定义。
 * @return 估算输入 Token 数，至少为 1；不参与真实统计或计费。
 */
func estimateCursorInputTokens(request cursorauth.Request) int {
	count := CountTokensRequest{
		Model:    request.Model,
		Messages: []MessageParam{{Role: "user", Content: request.Prompt}},
	}
	for _, tool := range request.Tools {
		var schema any
		if len(tool.Parameters) > 0 {
			_ = json.Unmarshal(tool.Parameters, &schema)
		}
		count.Tools = append(count.Tools, Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	return max(1, estimateTokens(&count))
}

func estimatedCursorResponseUsage(inputTokens int, text string, calls []cursorauth.ToolCall) *cursorauth.Usage {
	outputTokens := estimateTextTokens(text)
	for _, call := range calls {
		outputTokens += estimateToolName(call.Name) + max(1, len(call.Arguments)/4) + 8
	}
	inputTokens = max(1, inputTokens)
	return &cursorauth.Usage{
		InputTokens: inputTokens, OutputTokens: outputTokens,
		TotalTokens: inputTokens + outputTokens,
	}
}

func openaiToolCallID(id string) string {
	if strings.HasPrefix(id, "call_") {
		return id
	}
	if after, ok := strings.CutPrefix(id, "toolu_"); ok {
		return "call_" + after
	}
	return "call_" + id
}

func jsonString(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(raw)
}

func writeCursorTranslatedStream(
	ctx context.Context,
	registry *protocol.Registry,
	w http.ResponseWriter,
	client protocol.Protocol,
	modelID string,
	originalRequest, translatedRequest, raw []byte,
	state *any,
) error {
	if registry == nil {
		return errors.New("protocol registry is unavailable for Cursor " + string(client) + " responses")
	}
	for len(raw) > 0 {
		end := bytes.Index(raw, []byte("\n\n"))
		if end < 0 {
			if len(bytes.TrimSpace(raw)) == 0 {
				return nil
			}
			return errors.New("incomplete Cursor chat completions SSE event")
		}
		end += 2
		event := raw[:end]
		raw = raw[end:]
		chunks, err := registry.TranslateResponseStream(
			ctx,
			protocol.OpenAI,
			client,
			modelID,
			originalRequest,
			translatedRequest,
			event,
			state,
		)
		for _, chunk := range chunks {
			if len(chunk) == 0 {
				continue
			}
			if _, err := w.Write(chunk); err != nil {
				return fmt.Errorf("client disconnected: %w", err)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func cursorResponsesStreamError(err error) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type":    "error",
		"code":    "server_error",
		"message": err.Error(),
	})
	return []byte("event: error\ndata: " + string(payload) + "\n\n")
}

func cursorSupportsRequestFamily(path string) bool {
	family := protocol.DetectRequestFamily(path)
	return family == protocol.RequestFamilyMessages ||
		family == protocol.RequestFamilyChatCompletions ||
		cursorTranslatedClientProtocol(path) != ""
}

// cursorTranslatedClientProtocol names the client protocol that reaches the
// SDK as OpenAI chat completions through the registry and is rendered back in
// its own wire format. Gemini countTokens has no Cursor equivalent.
func cursorTranslatedClientProtocol(path string) protocol.Protocol {
	switch protocol.DetectRequestFamily(path) {
	case protocol.RequestFamilyResponses:
		return protocol.Codex
	case protocol.RequestFamilyGenerateContent:
		if !strings.Contains(path, ":countTokens") {
			return protocol.Gemini
		}
	}
	return ""
}
