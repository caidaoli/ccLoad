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
	family := protocol.RequestFamilyUnknown
	if reqCtx != nil {
		family = protocol.DetectRequestFamily(reqCtx.requestPath)
	}
	if reqCtx != nil && !cursorSupportsRequestFamily(reqCtx.requestPath) {
		return &proxyResult{
			status: http.StatusBadRequest, channelID: &cfg.ID, succeeded: false,
			protocolCapabilityMissing: true, nextAction: cooldown.ActionRetryChannel,
			body: []byte(`{"error":{"message":"Cursor OAuth supports Anthropic messages, OpenAI chat completions, and OpenAI responses","type":"invalid_request_error"}}`),
		}, nil
	}
	if family == protocol.RequestFamilyResponses {
		if s.protocolRegistry == nil {
			return cursorErrorResult(
				cfg,
				http.StatusServiceUnavailable,
				"protocol registry is unavailable for Cursor responses",
				cooldown.ActionReturnClient,
			), nil
		}
		translatedBody, err := s.protocolRegistry.TranslateRequest(
			protocol.Codex,
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
	for attempt := 0; attempt < 2; attempt++ {
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

func (s *Server) forwardCursorAgent(
	ctx context.Context,
	cfg *model.Config,
	credential *cursorauth.Credential,
	reqCtx *proxyRequestContext,
	w http.ResponseWriter,
) (*proxyResult, error) {
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
	modelID := s.resolveFinalUpstreamModel(cfg, requested, string(reqCtx.clientProtocol))
	started := time.Now()
	reqCtx.attemptStartTime = started
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
		if !reqCtx.skipProxyLog {
			failed := &fwResult{
				Status: status, Body: result.body,
			}
			duration := time.Since(started).Seconds()
			s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", status, duration, failed, err.Error())
			if status != StatusClientClosedRequest {
				s.updateTokenStatsForProxy(reqCtx, cfg, false, duration, failed, modelID)
			}
		}
		return result, nil
	}

	format := cursorResponseFormat(reqCtx)
	msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var responseTransformState any
	var responseTransformErr error
	var out bytes.Buffer
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
	writeStream := func(chunk string) {
		if !streaming || chunk == "" || responseTransformErr != nil {
			return
		}
		ensureStream()
		payload := cursorOpenAIChunk(msgID, modelID, chunk, "", nil)
		if format == "anthropic" {
			payload = cursorAnthropicDelta(chunk)
		}
		if format == "responses" {
			responseTransformErr = writeCursorResponsesStream(
				ctx, s.protocolRegistry, w, modelID, originalResponseBody, body, payload, &responseTransformState,
			)
			if responseTransformErr != nil {
				cancelRun()
			}
		} else {
			_, _ = w.Write(payload)
		}
		flush()
	}

	var runErr error
	var usage *cursorauth.Usage
	resumedToolRun := len(request.ToolResults) > 0
	if resumedToolRun {
		usage = estimatedCursorResponseUsage(request.InputTokenEstimate, "", nil)
	}
	var billableUsage *cursorauth.Usage
	replayed := false
	for event := range events {
		if debugCapture != nil && debugCapture.respBuf != nil && len(event.RawResponse) > 0 {
			_, _ = debugCapture.respBuf.Write(event.RawResponse)
			_, _ = debugCapture.respBuf.Write([]byte("\n"))
		}
		if firstByte == 0 && (event.Delta != "" || event.Text != "" || event.ToolCall != nil || (event.Done && event.Err == nil)) {
			firstByte = time.Since(started)
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
		if s.activeRequests != nil {
			received := len(event.Delta)
			if received == 0 && event.Text != "" && strings.HasPrefix(event.Text, previousFull) {
				received = len(event.Text) - len(previousFull)
			}
			s.activeRequests.AddBytes(reqCtx.activeReqID, int64(received))
		}
		if event.Err != nil {
			runErr = event.Err
		}
		if event.Usage != nil {
			if event.Done && !event.UsageEstimated && !event.Replayed {
				billableUsage = event.Usage
			}
			// A terminal Cursor SDK usage value is cumulative for the whole native
			// run, which may span many Chat Completions requests. It is valid for
			// billing once, but invalid as this request's context usage.
			if !resumedToolRun || event.UsageEstimated {
				usage = event.Usage
			}
		}
		replayed = replayed || event.Replayed
		if event.ToolCall != nil {
			calls = append(calls, *event.ToolCall)
		}
		if streaming && event.Delta != "" {
			writeStream(event.Delta)
			streamedPlain += len(event.Delta)
		}
		if responseTransformErr != nil {
			break
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
				_, _ = w.Write(cursorResponsesStreamError(runErr))
			} else {
				_, _ = w.Write([]byte("data: {\"error\":{\"message\":" + jsonString(runErr.Error()) + "}}\n\n"))
			}
			flush()
		}
		result := cursorErrorResult(cfg, status, runErr.Error(), action)
		result.duration = duration
		result.firstByteTime = firstByte.Seconds()
		finishCursorSDKDebug(reqCtx, debugCapture, status, result.body, runErr)
		if !reqCtx.skipProxyLog && !replayed {
			failed := &fwResult{
				Status: status, Body: result.body, FirstByteTime: firstByte.Seconds(),
			}
			applyCursorUsage(failed, billableUsage)
			s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", status, duration, failed, runErr.Error())
			if status != StatusClientClosedRequest {
				s.updateTokenStatsForProxy(reqCtx, cfg, false, duration, failed, modelID)
			}
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
	if resumedToolRun {
		usage = estimatedCursorResponseUsage(request.InputTokenEstimate, plain, calls)
	}

	var responseBody []byte
	var header http.Header
	if streaming {
		if format == "anthropic" {
			ensureStream()
			if streamedPlain == 0 && plain != "" {
				writeStream(plain)
			}
			_, _ = w.Write(cursorAnthropicStreamFinish(calls, usage))
		} else {
			ensureStream()
			if len(plain) > streamedPlain {
				writeStream(plain[streamedPlain:])
				streamedPlain = len(plain)
			}
			finish := cursorOpenAIFinish(msgID, modelID, plain, calls, streamedPlain > 0, usage)
			if format == "responses" {
				responseTransformErr = writeCursorResponsesStream(
					ctx, s.protocolRegistry, w, modelID, originalResponseBody, body, finish, &responseTransformState,
				)
				if responseTransformErr == nil {
					responseTransformErr = writeCursorResponsesStream(
						ctx, s.protocolRegistry, w, modelID, originalResponseBody, body,
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
			responseBody = cursorAnthropicMessage(msgID, modelID, plain, calls, usage)
		} else {
			responseBody = cursorOpenAIMessage(msgID, modelID, plain, calls, usage)
		}
		if format == "responses" {
			responseBody, responseTransformErr = s.protocolRegistry.TranslateResponseNonStream(
				ctx, protocol.OpenAI, protocol.Codex, modelID, originalResponseBody, body, responseBody,
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
	applyCursorUsage(forwarded, billableUsage)
	if !reqCtx.skipProxyLog && !replayed {
		s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", http.StatusOK, duration, forwarded, "")
		s.updateTokenStatsForProxy(reqCtx, cfg, true, duration, forwarded, modelID)
	}
	return &proxyResult{
		status: http.StatusOK, header: header, body: responseBody, channelID: &channelID,
		duration: duration, firstByteTime: firstByte.Seconds(), succeeded: true,
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

func cursorAnthropicStart(id, modelID string) []byte {
	start, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "content": []any{},
			"model": modelID, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	block, _ := json.Marshal(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	return []byte("event: message_start\ndata: " + string(start) + "\n\n" +
		"event: content_block_start\ndata: " + string(block) + "\n\n")
}

func cursorAnthropicDelta(text string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	return []byte("event: content_block_delta\ndata: " + string(payload) + "\n\n")
}

func normalizedCursorToolArguments(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(bytes.Clone(raw))
}

func cursorAnthropicStreamFinish(calls []cursorauth.ToolCall, usage *cursorauth.Usage) []byte {
	var b bytes.Buffer
	stop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": 0})
	b.WriteString("event: content_block_stop\ndata: " + string(stop) + "\n\n")
	for i, call := range calls {
		blockIndex := i + 1
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
		b.WriteString("event: content_block_start\ndata: " + string(start) + "\n\n")
		b.WriteString("event: content_block_delta\ndata: " + string(delta) + "\n\n")
		b.WriteString("event: content_block_stop\ndata: " + string(blockStop) + "\n\n")
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
	b.WriteString("event: message_delta\ndata: " + string(messageDelta) + "\n\n")
	messageStop, _ := json.Marshal(map[string]any{"type": "message_stop"})
	b.WriteString("event: message_stop\ndata: " + string(messageStop) + "\n\n")
	return b.Bytes()
}

func cursorAnthropicMessage(id, modelID, text string, calls []cursorauth.ToolCall, usage *cursorauth.Usage) []byte {
	content := make([]any, 0, 1+len(calls))
	if text != "" || len(calls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": text})
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

func cursorOpenAIChunk(id, modelID, content, finish string, usage *cursorauth.Usage) []byte {
	delta := map[string]any{}
	var finishReason any
	if content != "" {
		delta["content"] = content
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

func cursorOpenAIFinish(id, modelID, text string, calls []cursorauth.ToolCall, textAlreadyStreamed bool, usage *cursorauth.Usage) []byte {
	var b bytes.Buffer
	if !textAlreadyStreamed && text != "" && len(calls) == 0 {
		b.Write(cursorOpenAIChunk(id, modelID, text, "", nil))
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
		b.WriteString("data: " + string(payload) + "\n\n")
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	b.Write(cursorOpenAIChunk(id, modelID, "", finish, usage))
	return b.Bytes()
}

func cursorOpenAIMessage(id, modelID, text string, calls []cursorauth.ToolCall, usage *cursorauth.Usage) []byte {
	message := map[string]any{"role": "assistant", "content": text}
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

func applyCursorUsage(result *fwResult, usage *cursorauth.Usage) {
	if result == nil || usage == nil {
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

// estimateCursorInputTokens gives clients a context signal while Cursor's
// native run is suspended inside a custom-tool callback. The bridge does not
// expose a reliable usage snapshot at that point, so this estimate is only
// returned to the client and is intentionally excluded from billing/logging.
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
	if strings.HasPrefix(id, "toolu_") {
		return "call_" + strings.TrimPrefix(id, "toolu_")
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

func writeCursorResponsesStream(
	ctx context.Context,
	registry *protocol.Registry,
	w http.ResponseWriter,
	modelID string,
	originalRequest, translatedRequest, raw []byte,
	state *any,
) error {
	if registry == nil {
		return errors.New("protocol registry is unavailable for Cursor responses")
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
			protocol.Codex,
			modelID,
			originalRequest,
			translatedRequest,
			event,
			state,
		)
		if err != nil {
			return err
		}
		for _, chunk := range chunks {
			if len(chunk) == 0 {
				continue
			}
			if _, err := w.Write(chunk); err != nil {
				return fmt.Errorf("client disconnected: %w", err)
			}
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
		family == protocol.RequestFamilyResponses
}
