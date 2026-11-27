package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/privacy"
	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/util"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const cursorSDKBridgeRequestURL = "http://cursor-sdk-bridge/sdk.v1.SdkAgentService/CreateAgent+Send"

/**
 * @brief 表示客户端可通过缩短输入解决的模型上下文超限。
 */
const cursorContextLengthExceeded = "context_length_exceeded"

/**
 * @brief 转发至 Cursor SDK，并在实时请求被拒绝时恢复渠道凭证。
 * @param ctx 当前代理请求上下文。
 * @param cfg 当前 Cursor 渠道配置。
 * @param reqCtx 请求协议、模型、日志与路由状态。
 * @param w 客户端响应写入器。
 * @return 当前渠道的响应或后续路由动作。
 * @return 转发失败且无法形成代理结果时的错误，否则为 nil。
 * @note 当前请求已提交 SSE 或收到旧工具轮结果时，禁止在此重新推理。
 */
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
	cooldownCredential := func(refreshErr error) {
		expected := cfg.OAuthCredential
		if credential != nil {
			payload, _ := json.Marshal(credential)
			expected = string(payload)
		}
		duration := oauthCredentialRetryAfterDuration(refreshErr, time.Now())
		if duration == 0 {
			duration = rejectedOAuthCredentialCooldown
		}
		s.applyOAuthCredentialCooldown(ctx, cfg.ID, "Cursor", cfg.GetAuthType(), expected, http.StatusUnauthorized, duration, oauthCooldownCredential)
	}
	// 请求取消本身不施加凭证冷却；后台 re-mint 仍会继续并落库新凭证。
	if credential == nil {
		if err != nil && ctx.Err() == nil {
			cooldownCredential(nil)
		}
		return oauthCredentialUnavailableResult(cfg, "Cursor"), nil
	}
	for attempt := range 2 {
		result, forwardErr := s.forwardCursorAgent(ctx, cfg, credential, reqCtx, w)
		if forwardErr != nil || result == nil {
			return result, forwardErr
		}
		credentialRejected := !result.cursorRunReplayed && result.status == http.StatusUnauthorized &&
			(result.nextAction == cooldown.ActionRetryChannel || result.succeeded)
		if !credentialRejected {
			return result, nil
		}
		if attempt == 1 {
			cooldownCredential(nil)
			return result, nil
		}
		refreshed, refreshErr := s.cursorCredentials.credentialAfterUnauthorized(ctx, cfg, credential.AccessToken)
		if refreshErr != nil {
			if ctx.Err() == nil {
				cooldownCredential(refreshErr)
			}
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
 * @details 普通轮优先展示独立 usage；没有独立报告时使用非零终态补差。全零补差不代表当前轮已有报告。
 * SDK 独立 usage 和终态补差都可能包含历史工具轮；恢复轮的客户端用量始终按本次请求估算。
 * 没有可归账的真实用量时，日志与客户端共用当前轮估算。
 * @details 使用 Chat 线协议的超时配置。首内容耗时至少记为 1ns；提交前的心跳仅重置空闲计时。
 * @note 写入客户端响应、代理日志和令牌统计；旧工具轮重放不重复记账、冷却或切换渠道。
 */
func (s *Server) forwardCursorAgent(
	ctx context.Context,
	cfg *model.Config,
	credential *cursorauth.Credential,
	reqCtx *proxyRequestContext,
	w http.ResponseWriter,
) (*proxyResult, error) {
	format := cursorResponseFormat(reqCtx)
	selected := reqCtx.attemptModel
	if selected.logicalModel == "" && reqCtx.originalModel != "" {
		selected, _ = s.firstModelRow(cfg, reqCtx.originalModel)
	}
	modelID := s.resolveFinalUpstreamModel(cfg, selected, string(reqCtx.clientProtocol))
	requestedModel := reqCtx.requestedModel
	if requestedModel == "" {
		requestedModel = reqCtx.originalModel
	}
	translatedClient := cursorTranslatedClientProtocol(reqCtx.requestPath)
	originalResponseBody := reqCtx.body
	clientBody := reqCtx.body
	streaming := reqCtx.isStreaming
	var compaction *responsesCompactionRequest
	if translatedClient == protocol.Codex {
		var err error
		clientBody, compaction, err = prepareCursorResponsesRequest(ctx, reqCtx)
		if err != nil {
			if errors.Is(err, errPrivacyMasking) {
				s.privacyStats.blocked.Add(1)
				return cursorErrorResult(cfg, http.StatusBadRequest, err.Error(), cooldown.ActionReturnClient, format, ""), nil
			}
			result := cursorErrorResult(cfg, http.StatusBadRequest, err.Error(), cooldown.ActionRetryChannel, format, "")
			result.protocolCapabilityMissing, result.requestScopedProtocolMiss = true, true
			return result, nil
		}
		if compaction != nil {
			originalResponseBody = clientBody
			streaming = false
		}
	}
	clientBody = applyThinkingSuffixForModel(clientBody, reqCtx.clientProtocol, protocol.OpenAI, requestedModel, modelID)
	var levels []string
	var knownLevels bool
	if reqCtx.reasoning != nil {
		levels, knownLevels = cursorCatalogEffortLevels(credential, modelID)
		if !knownLevels {
			clientBody = applyAutomaticReasoning(clientBody, reqCtx, protocol.OpenAI, modelID)
		}
	}
	if attachment := cursorUnsupportedAttachment(translatedClient, clientBody); attachment != "" {
		message := fmt.Sprintf("Cursor Local Agent cannot read %s; use text or inline base64 images", attachment)
		if reqCtx.cursorToolRoute.channelID != 0 {
			return cursorErrorResult(cfg, http.StatusBadRequest, message, cooldown.ActionReturnClient, format, ""), nil
		}
		result := cursorErrorResult(cfg, http.StatusBadRequest, message, cooldown.ActionSkipChannel, format, "")
		result.candidateSkipped, result.header = true, nil
		return result, nil
	}

	body := clientBody
	// The SDK only reads chat-shaped requests. Start from the client protocol
	// body (full replay for a locally known Responses continuation).
	if translatedClient != "" {
		if s.protocolRegistry == nil {
			return cursorErrorResult(
				cfg,
				http.StatusServiceUnavailable,
				"protocol registry is unavailable for Cursor "+string(translatedClient)+" requests",
				cooldown.ActionReturnClient,
				format, "",
			), nil
		}
		translatedBody, err := s.protocolRegistry.TranslateRequest(
			translatedClient,
			protocol.OpenAI,
			reqCtx.originalModel,
			clientBody,
			streaming,
		)
		if err != nil {
			return cursorErrorResult(cfg, http.StatusBadRequest, err.Error(), cooldown.ActionReturnClient, format, ""), nil
		}
		// The translators omit empty tool declarations. On a live Cursor Run an
		// explicit empty set revokes tools, while an omitted set inherits them.
		source := gjson.ParseBytes(clientBody)
		if !gjson.GetBytes(translatedBody, "tools").Exists() && (source.Get("tools").IsArray() ||
			(translatedClient == protocol.Codex && source.Get(`input.#(type=="additional_tools").tools`).IsArray())) {
			translatedBody = setJSONRaw(translatedBody, "tools", "[]")
		}
		if translatedClient == protocol.Codex && !gjson.GetBytes(translatedBody, "tool_choice").Exists() {
			choice := source.Get("tool_choice")
			if choice.IsObject() && (choice.Get("namespace").Str != "" || choice.Get("function.namespace").Str != "" || choice.Get("custom.namespace").Str != "") {
				return cursorErrorResult(cfg, http.StatusBadRequest, "namespaced tool_choice requires tool definitions", cooldown.ActionReturnClient, format, ""), nil
			}
			if choice.IsObject() || choice.Str == "none" || choice.Str == "required" {
				translatedBody = setJSONRaw(translatedBody, "tool_choice", choice.Raw)
			}
		}
		if translatedClient == protocol.Codex {
			parallel := source.Get("parallel_tool_calls")
			if parallel.Type == gjson.True || parallel.Type == gjson.False {
				translatedBody = setJSONRaw(translatedBody, "parallel_tool_calls", parallel.Raw)
			}
		}
		reqCtx.translatedBody = translatedBody
		body = translatedBody
	}
	request := cursorauth.ParseRequest(body)
	// 部分客户端协议以删除等级字段表达 (none)，SDK 需要显式关闭。
	if _, suffix, ok := model.ParseThinkingSuffix(requestedModel); ok && suffix.Mode == thinking.ModeNone {
		request.Effort = "none"
	}
	if knownLevels {
		if effort := automaticReasoningEffort(reqCtx, protocol.OpenAI, modelID, levels, true); effort != "" {
			request.Effort, reqCtx.reasoningExpected = effort, effort
		}
	}
	request.SessionScope = reqCtx.cursorSessionScope(cfg.ID)
	if compaction != nil {
		request.SessionScope = cursorauth.SessionScope{}
	}
	if request.Prompt == "" && len(request.ToolResults) == 0 {
		return cursorErrorResult(cfg, http.StatusBadRequest, "cursor prompt is required", cooldown.ActionReturnClient, format, ""), nil
	}
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
	if s.activeRequests != nil {
		s.activeRequests.SetDebugCapture(reqCtx.activeReqID, debugCapture)
	}
	finishStartFailure := func(result *proxyResult, err error) *proxyResult {
		status := result.status
		result.duration = time.Since(started).Seconds()
		result.isClientCanceled = status == StatusClientClosedRequest
		result.proxyLogWritten = !reqCtx.skipProxyLog
		finishCursorSDKDebug(reqCtx, debugCapture, status, result.body, err)
		failed := &fwResult{Status: status, Header: result.header, Body: result.body}
		result.forwarded = failed
		if !reqCtx.skipProxyLog {
			s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", status, result.duration, failed, err.Error())
			s.updateTokenStatsForProxy(reqCtx, failureTokenStatsOutcome(status), result.duration, failed, modelID)
		}
		return result
	}
	runner := s.cursorRunnerSnapshot()
	if runner == nil {
		err := errors.New("cursor SDK runner is unavailable")
		result := cursorErrorResult(cfg, http.StatusServiceUnavailable, err.Error(), cooldown.ActionSkipChannel, format, "")
		result.candidateSkipped = true
		return finishStartFailure(result, err), nil
	}

	timeouts := s.resolveProtocolTimeouts(protocol.TransformPlan{UpstreamProtocol: protocol.OpenAI})
	timeoutCtx := newRequestContextForStreaming(ctx, streaming, timeouts)
	defer timeoutCtx.cleanup()
	classifyFailure := func(runErr error, replayed bool) (*proxyResult, error) {
		timedOut := timeoutCtx.firstByteTimeoutTriggered() || timeoutCtx.streamTimeoutTriggered() ||
			(!streaming && timeoutCtx.ctx.Err() != nil &&
				errors.Is(context.Cause(timeoutCtx.ctx), context.DeadlineExceeded))
		if timedOut && ctx.Err() == nil {
			failed, _, timeoutErr := s.handleRequestError(timeoutCtx, cfg, runErr)
			code := ""
			if failed.Status == util.StatusStreamIncomplete {
				code = responsesWebsocketInterruptedCode
			}
			result := cursorErrorResult(cfg, failed.Status, timeoutErr.Error(), cooldown.ActionRetryChannel, format, code)
			if !replayed && !reqCtx.deferCursorCooldown {
				input := cooldownInputForModel(networkErrorInput(cfg.ID, cooldown.NoKeyIndex, failed.Status), modelID)
				input.ModelScoped = util.IsModelScopedNetworkError(timeoutErr)
				result.nextAction = s.applyCooldownDecision(ctx, cfg, input)
			}
			return result, timeoutErr
		}
		return s.cursorFailureResult(ctx, cfg, modelID, runErr, replayed, reqCtx.deferCursorCooldown, format), runErr
	}
	runBaseCtx := timeoutCtx.ctx
	if debugCapture != nil {
		runBaseCtx = cursorauth.WithRawResponseCapture(runBaseCtx)
	}
	runCtx, cancelRun := context.WithCancel(runBaseCtx)
	defer cancelRun()
	request.Model = modelID
	request.OnDetachedUsage = s.cursorDetachedUsage(logEntryParams{
		RequestModel:     reqCtx.requestLogModel(),
		ActualModel:      modelID,
		ChannelID:        cfg.ID,
		AuthTokenID:      reqCtx.tokenID,
		ClientProtocol:   reqCtx.clientProtocol,
		UpstreamProtocol: reqCtx.upstreamProtocol,
		CostMultiplier:   reqCtx.attemptCostMultiplier,
		StartTime:        started,
	})
	events, err := runner.Run(runCtx, credential, request)
	if errors.Is(err, cursorauth.ErrURLImageUnsupported) {
		result := cursorErrorResult(cfg, http.StatusBadRequest, err.Error(), cooldown.ActionSkipChannel, format, "")
		result.candidateSkipped = true
		result.header = nil
		return result, nil
	}
	if session := privacySessionFromContext(ctx); session != nil && session.Enabled() {
		sent, _ := json.Marshal(request)
		reqCtx.privacyMasked = session.CountSent(sent)
	}
	if err != nil {
		result, failureErr := classifyFailure(err, false)
		return finishStartFailure(result, failureErr), nil
	}

	clientWriter := w
	if reqCtx.isStreaming {
		// The upstream timer stops the run; the client can still receive the
		// terminal SSE error. A client cancellation interrupts downstream writes.
		cancelableWriter, stopWrites := newCancelableResponseWriter(ctx, w)
		defer stopWrites()
		w = cancelableWriter
	}
	w = debugCapture.wrapTranslatedResponseWriter(w)

	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	msgID := "msg_" + id
	openAIID := "chatcmpl-" + msgID
	if translatedClient == protocol.Codex {
		openAIID = cursorResponsesIDPrefix + id
	}
	var responseTransformState any
	var responseErr error
	var responsesCursor responsesStreamCursor
	if streaming && translatedClient != "" && s.protocolRegistry != nil {
		_, responseErr = s.protocolRegistry.TranslateResponseStream(ctx, protocol.OpenAI, translatedClient,
			modelID, originalResponseBody, body, nil, &responseTransformState)
		if responseErr != nil {
			cancelRun()
		}
	}
	var thinking strings.Builder
	var content []map[string]any
	var anthropicStream cursorAnthropicStream
	full := ""
	firstByte := time.Duration(0)
	wroteHeader := false
	streamedPlain := 0
	var calls []cursorauth.ToolCall
	flush := func() error {
		if err := context.Cause(ctx); err != nil {
			return fmt.Errorf("client disconnected: %w", err)
		}
		if err := http.NewResponseController(clientWriter).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return fmt.Errorf("client disconnected: %w", err)
		}
		return nil
	}
	writePayload := func(payload []byte) error {
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("client disconnected: %w", err)
		}
		return nil
	}
	ensureStream := func() error {
		if cause := context.Cause(timeoutCtx.ctx); cause != nil {
			return cause
		}
		if wroteHeader {
			return nil
		}
		disableResponseWriteTimeout(w, "Cursor SDK stream")
		header := w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		if err := responseHeaderWriteError(w); err != nil {
			return fmt.Errorf("client disconnected: %w", err)
		}
		wroteHeader = true
		if format == "anthropic" {
			return writePayload(cursorAnthropicStart(msgID, modelID))
		}
		return nil
	}
	writeStream := func(chunk, kind string) error {
		if err := ensureStream(); err != nil {
			return err
		}
		var payload []byte
		if format == "anthropic" {
			payload = anthropicStream.delta(chunk, kind)
		} else if kind == "thinking" {
			payload = cursorOpenAIChunk(openAIID, modelID, "", chunk, "", nil)
		} else {
			payload = cursorOpenAIChunk(openAIID, modelID, chunk, "", "", nil)
		}
		if translatedClient != "" {
			if err := writeCursorTranslatedStream(
				ctx, s.protocolRegistry, w, translatedClient, modelID, originalResponseBody, body, payload, &responseTransformState,
				&responsesCursor,
			); err != nil {
				return err
			}
		} else if err := writePayload(payload); err != nil {
			return err
		}
		return flush()
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
	done := false
	var usage *cursorauth.Usage
	var turnUsage, billableUsage cursorauth.Usage
	var usageIncrements []util.TokenCostUsage
	billingModel := util.CursorPricingModel(modelID)
	replayed := false
	for event := range events {
		timeoutCtx.touchStreamIdle()
		if event.SentEffort != nil && reqCtx.reasoning != nil && ctx.Err() == nil {
			reqCtx.commitSentReasoning(*event.SentEffort, "unsupported_model_effort")
		}
		if debugCapture != nil && debugCapture.respBuf != nil && len(event.RawResponse) > 0 {
			_, _ = debugCapture.respBuf.Write(event.RawResponse)
			_, _ = debugCapture.respBuf.Write([]byte("\n"))
		}
		if event.Usage != nil && *event.Usage != (cursorauth.Usage{}) {
			if !event.Replayed {
				if event.BillingModel != "" {
					billingModel = event.BillingModel
				}
				billableUsage.Add(event.Usage)
				increments := event.UsageIncrements
				if len(increments) == 0 {
					increments = []cursorauth.Usage{*event.Usage}
				}
				for _, increment := range increments {
					usageIncrements = append(usageIncrements, util.TokenCostUsage{
						InputTokens: increment.InputTokens, OutputTokens: increment.OutputTokens,
						CacheReadTokens: increment.CacheReadTokens, CacheWriteTokens: increment.CacheWriteTokens,
					})
				}
			}
			if !event.Done {
				turnUsage.Add(event.Usage)
				usage = &turnUsage
			} else if usage == nil {
				usage = event.Usage
			}
		}
		replayed = replayed || event.Replayed
		done = done || event.Done
		if runErr == nil {
			runErr = event.Err
		}
		// Drain reported usage after cancellation, including the terminal increment.
		if runErr != nil || responseErr != nil || timeoutCtx.ctx.Err() != nil {
			continue
		}
		if firstByte == 0 && (event.Delta != "" || event.ThinkingDelta != "" || event.Text != "" || event.ToolCall != nil || event.Done) {
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
			full += event.Delta
		}
		thinking.WriteString(event.ThinkingDelta)
		appendContent(event.ThinkingDelta, "thinking")
		textDelta := event.Delta
		if textDelta == "" && strings.HasPrefix(full, previousFull) {
			textDelta = full[len(previousFull):]
		}
		appendContent(textDelta, "text")
		if s.activeRequests != nil {
			s.activeRequests.AddBytes(reqCtx.activeReqID, int64(len(textDelta)+len(event.ThinkingDelta)))
		}
		if event.ToolCall != nil {
			calls = append(calls, *event.ToolCall)
		}
		// A heartbeat before the first output preserves the option to fail over.
		if event.Ping && wroteHeader {
			responseErr = writePayload(cursorStreamPing(format))
			if responseErr == nil {
				responseErr = flush()
			}
		}
		if streaming && event.ThinkingDelta != "" && responseErr == nil {
			responseErr = writeStream(event.ThinkingDelta, "thinking")
		}
		if streaming && event.Delta != "" && responseErr == nil {
			responseErr = writeStream(event.Delta, "text")
			streamedPlain += len(event.Delta)
		}
		if responseErr != nil {
			cancelRun()
		}
	}
	duration := time.Since(started).Seconds()
	var estimatedUsage *cursorauth.Usage
	if usage == nil || len(request.ToolResults) > 0 {
		estimatedUsage = estimatedCursorResponseUsage(estimateCursorInputTokens(request), full, thinking.String(), calls)
		usage = estimatedUsage
	}
	recordUsage := func(result *fwResult, outcome model.TokenStatsOutcome, errMsg string) {
		applyCursorUsage(result, billableUsage, estimatedUsage)
		result.cursorUsageIncrements = usageIncrements
		result.cursorBillingModel = billingModel
		if strings.HasSuffix(billingModel, "-fast") {
			result.ServiceTier = "fast"
		}
		if !reqCtx.skipProxyLog && !replayed {
			entry := buildProxyLogEntry(reqCtx, cfg, modelID, "cursor-oauth", result.Status, duration, result, errMsg)
			cost := entry.Cost
			s.AddLogAsync(entry)
			s.updateTokenStatsAsync(reqCtx.tokenHash, reqCtx.attemptCostMultiplier, reqCtx.attemptModelPrice,
				outcome, duration, reqCtx.isStreaming, result, modelID, &cost)
		}
	}
	finishRunFailure := func(runErr error) (*proxyResult, error) {
		result, runErr := classifyFailure(runErr, replayed)
		if replayed {
			result.cursorRunReplayed = true
			result.nextAction = cooldown.ActionReturnClient
		}
		status := result.status
		clientDisconnected := status == StatusClientClosedRequest
		if streaming && wroteHeader && !clientDisconnected {
			var writeErr error
			switch format {
			case "responses":
				// Preserve the run failure; EOF validation would replace it with a
				// missing-terminator error. Conversion failures were already emitted.
				if protocol.ResponseToolInputError(responseTransformState) == nil {
					code := gjson.GetBytes(result.body, "error.code").String()
					writeErr = writePayload(cursorResponsesStreamError(runErr.Error(), code, responsesCursor.nextSequence))
				}
			case "anthropic":
				writeErr = writePayload(fmt.Appendf(nil, "event: error\ndata: %s\n\n", result.body))
			default:
				writeErr = writePayload(fmt.Appendf(nil, "data: %s\n\n", result.body))
			}
			if writeErr == nil {
				writeErr = flush()
			}
			if writeErr != nil {
				runErr = writeErr
				result = s.cursorFailureResult(ctx, cfg, modelID, writeErr, replayed, reqCtx.deferCursorCooldown, format)
				result.cursorRunReplayed = replayed
				status, clientDisconnected = result.status, true
			}
		}
		result.duration = duration
		result.firstByteTime = firstByte.Seconds()
		finishCursorSDKDebug(reqCtx, debugCapture, status, result.body, runErr)
		failed := &fwResult{
			Status: status, Header: result.header, Body: result.body, FirstByteTime: firstByte.Seconds(),
			ResponseCommitted: wroteHeader,
		}
		recordUsage(failed, failureTokenStatsOutcome(status), runErr.Error())
		result.forwarded = failed
		result.proxyLogWritten = !reqCtx.skipProxyLog && !replayed
		result.isClientCanceled = clientDisconnected
		if wroteHeader {
			// The SSE envelope is already on the wire; the attempt loop must not
			// write a second JSON body.
			result.succeeded = true
			result.nextAction = cooldown.ActionReturnClient
		}
		return result, nil
	}
	if responseErr != nil {
		runErr = responseErr
	}
	if runErr == nil && timeoutCtx.ctx.Err() != nil {
		runErr = context.Cause(timeoutCtx.ctx)
	}
	// Tool batch delivery is also a valid end to a single attached request.
	if runErr == nil && !done && len(calls) == 0 {
		runErr = fmt.Errorf("cursor run stream closed before terminal event: %w", io.ErrUnexpectedEOF)
	}
	if runErr != nil {
		return finishRunFailure(runErr)
	}

	plain := full

	var responseBody []byte
	var header http.Header
	var responsesTurn responsesWebsocketTurnResult
	var hasResponsesTurn bool
	if streaming {
		if err := ensureStream(); err != nil {
			return finishRunFailure(err)
		}
		if len(plain) > streamedPlain {
			if err := writeStream(plain[streamedPlain:], "text"); err != nil {
				return finishRunFailure(err)
			}
			streamedPlain = len(plain)
		}
		if format == "anthropic" {
			responseErr = writePayload(anthropicStream.finish(calls, usage))
		} else {
			finish := cursorOpenAIFinish(openAIID, modelID, plain, calls, streamedPlain > 0, usage)
			finish = append(finish, "data: [DONE]\n\n"...)
			if translatedClient == "" {
				responseErr = writePayload(finish)
			} else {
				responseErr = writeCursorTranslatedStream(
					ctx, s.protocolRegistry, w, translatedClient, modelID, originalResponseBody, body, finish, &responseTransformState,
					&responsesCursor,
				)
			}
		}
		if responseErr == nil {
			responseErr = flush()
		}
		if responseErr != nil {
			return finishRunFailure(responseErr)
		}
		responseBody = []byte(plain)
		header = w.Header()
		responsesTurn = responsesCursor.turn
		hasResponsesTurn = responsesTurn.completedResponseID != ""
	} else {
		if format == "anthropic" {
			responseBody = cursorAnthropicMessage(msgID, modelID, content, calls, usage)
		} else {
			responseBody = cursorOpenAIMessage(openAIID, modelID, plain, thinking.String(), calls, usage)
		}
		if translatedClient != "" {
			responseBody, responseErr = s.protocolRegistry.TranslateResponseNonStream(
				ctx, protocol.OpenAI, translatedClient, modelID, originalResponseBody, body, responseBody,
			)
		}
		if responseErr != nil {
			return finishRunFailure(responseErr)
		}
		header = w.Header()
		header.Set("Content-Type", "application/json")
		if compaction != nil {
			responseBody, responseErr = buildResponsesCompactionResponse(responseBody, compaction.compactEndpoint)
			if responseErr != nil {
				return finishRunFailure(responseErr)
			}
		}
		if translatedClient == protocol.Codex {
			responsesTurn, hasResponsesTurn = responsesHTTPJSONTurn(responseBody)
		}
		if compaction != nil && compaction.stream {
			frames, err := responsesCompactionStream(responseBody)
			if err != nil {
				return finishRunFailure(err)
			}
			responseBody = bytes.Join(frames, nil)
			header.Set("Content-Type", "text/event-stream")
		}
		disableResponseWriteTimeout(w, "Cursor SDK response")
		w.WriteHeader(http.StatusOK)
		if err := responseHeaderWriteError(w); err != nil {
			return finishRunFailure(fmt.Errorf("client disconnected: %w", err))
		}
		wroteHeader = true
		if err := writePayload(responseBody); err != nil {
			return finishRunFailure(err)
		}
		if compaction != nil && compaction.stream {
			if err := flush(); err != nil {
				return finishRunFailure(err)
			}
		}
	}

	channelID := cfg.ID
	responsesTurn.localResponseID = hasResponsesTurn
	if replay := reqCtx.cursorHTTPToolReplay; replay != nil && hasResponsesTurn && modelID != "" {
		replay.channelID = channelID
		replay.key = sha256.Sum256([]byte(credential.APIKey))
		replay.actualModel = modelID
		responsesTurn.cursorHTTPToolReplay = replay
	}
	finishCursorSDKDebug(reqCtx, debugCapture, http.StatusOK, responseBody, nil)
	forwarded := &fwResult{
		Status: http.StatusOK, Header: header, Body: responseBody, FirstByteTime: firstByte.Seconds(),
		ResponseCommitted: wroteHeader, ResponsesTurnResult: responsesTurn, HasResponsesTurnResult: hasResponsesTurn,
	}
	recordUsage(forwarded, model.TokenStatsSuccess, "")
	return &proxyResult{
		status: http.StatusOK, header: header, body: responseBody, channelID: &channelID,
		forwarded: forwarded,
		duration:  duration, firstByteTime: firstByte.Seconds(), succeeded: true,
		nextAction: cooldown.ActionReturnClient, proxyLogWritten: !reqCtx.skipProxyLog && !replayed,
		responsesTurn: forwarded.ResponsesTurnResult, hasResponsesTurn: forwarded.HasResponsesTurnResult,
	}, nil
}

/**
 * @brief 恢复 Responses 历史并生成 Cursor 所需的无工具摘要请求。
 * @param ctx 当前代理上下文，含请求的隐私脱敏会话。
 * @param reqCtx 当前 HTTP 或 WebSocket 轮次；已有的完整历史由执行会话提供。
 * @return 可转换为 Chat 的完整 Responses 请求，输入无效时为 nil。
 * @return 压缩响应需要恢复的客户端传输设置；正常请求为 nil。
 * @return 续接历史不可用、外部压缩无法恢复或请求无效时返回错误。
 */
func prepareCursorResponsesRequest(ctx context.Context, reqCtx *proxyRequestContext) ([]byte, *responsesCompactionRequest, error) {
	body := cursorClientRequestBody(reqCtx)
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return nil, nil, errResponsesPreviousResponseUnavailable
	}
	expanded, err := expandResponsesCompaction(body)
	if err != nil {
		return nil, nil, err
	}
	if hasForeignResponsesCompaction(expanded) {
		return nil, nil, errForeignResponsesCompaction
	}
	if !bytes.Equal(expanded, body) {
		expanded, err = privacySessionFromContext(ctx).MaskJSON(expanded)
		if err != nil {
			return nil, nil, errPrivacyMasking
		}
	}
	if !responsesCompactionRequested(reqCtx.requestPath, reqCtx.body) {
		return expanded, nil, nil
	}
	summary, err := prepareResponsesCompactionSummary(expanded)
	if err != nil {
		return nil, nil, err
	}
	return summary, &responsesCompactionRequest{
		stream:          reqCtx.isStreaming,
		compactEndpoint: strings.HasSuffix(strings.TrimRight(reqCtx.requestPath, "/"), "/responses/compact"),
	}, nil
}

/**
 * @brief 按最后一次附着的请求身份保存 SDK 尚未交付的实际消费。
 * @param origin 渠道、模型和令牌快照，不持有 HTTP 请求或响应。
 * @return 会话收尾时调用的记账函数。
 */
func (s *Server) cursorDetachedUsage(origin logEntryParams) func(cursorauth.Usage) {
	return func(usage cursorauth.Usage) {
		result := &fwResult{}
		applyCursorUsage(result, usage, nil)
		params := origin
		params.Result = result
		params.ErrMsg = "Cursor SDK usage for request started " + origin.StartTime.UTC().Format(time.RFC3339Nano)
		params.StartTime = time.Now()
		entry := buildLogEntry(params)
		entry.LogSource = model.LogSourceCursorDetachedUsage
		// Runner 关闭时异步请求日志队列可能已停，但 store 会等 Runner 退出才关闭。
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.LogFlushTimeoutMs)*time.Millisecond)
		defer cancel()
		if err := s.store.AddLog(ctx, entry); err != nil {
			log.Printf("[ERROR] Cursor detached usage write failed (channel=%d, token=%d, model=%q, usage=%+v): %v",
				entry.ChannelID, entry.AuthTokenID, origin.ActualModel, usage, err)
		}
	}
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

/**
 * @brief 确定 Cursor 返回成功或失败时使用的下游协议。
 * @param reqCtx 代理请求，缺省使用 OpenAI Chat。
 * @return 下游协议名称。
 */
func cursorResponseFormat(reqCtx *proxyRequestContext) string {
	if reqCtx == nil {
		return "openai"
	}
	switch protocol.DetectRequestFamily(reqCtx.requestPath) {
	case protocol.RequestFamilyResponses:
		return "responses"
	case protocol.RequestFamilyGenerateContent:
		return "gemini"
	}
	if util.NormalizeProtocol(string(reqCtx.clientProtocol)) == util.ProtocolAnthropic ||
		strings.Contains(reqCtx.requestPath, "/messages") {
		return "anthropic"
	}
	return "openai"
}

/**
 * @brief 分类 Cursor 启动或流中的失败，保留上游业务拒绝的作用域。
 * @param ctx 当前请求上下文；公共冷却管道负责断开写入的取消链。
 * @param cfg 当前 Cursor 渠道。
 * @param modelID 实际发送给 Cursor 的模型名。
 * @param err Cursor Runner 返回的失败。
 * @param replayed 是否为已处理的旧工具轮；重放不续期冷却。
 * @param deferCooldown 是否由管理测试在外层统一处理持久化。
 * @param format 下游协议名称。
 * @return 含代理状态、响应正文和后续路由动作的失败结果。
 * @details 缺终态的 EOF 按流中断冷却模型。仅临时限流使用 SDK 结构化恢复时间；用量上限按现有指数退避。
 */
func (s *Server) cursorFailureResult(ctx context.Context, cfg *model.Config, modelID string, err error, replayed, deferCooldown bool, format string) *proxyResult {
	now := time.Now()
	retryAt, rateLimited := cursorauth.RateLimitRetryTime(err, now)
	status, action, skipped := http.StatusBadGateway, cooldown.ActionRetryChannel, false
	accountCapabilityMissing := false
	code := ""
	switch {
	case isClientDisconnectError(err):
		status, action = StatusClientClosedRequest, cooldown.ActionReturnClient
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		status, code = util.StatusStreamIncomplete, responsesWebsocketInterruptedCode
	case errors.Is(err, cursorauth.ErrAgentMissing):
		status, action, skipped = http.StatusServiceUnavailable, cooldown.ActionSkipChannel, true
	case errors.Is(err, cursorauth.ErrMissingAPIKey):
		status, skipped = http.StatusUnauthorized, true
	case cursorauth.IsCredentialRejected(err):
		status = http.StatusUnauthorized
	case cursorauth.IsAccountCapabilityUnavailable(err):
		status, action, accountCapabilityMissing = http.StatusForbidden, cooldown.ActionSkipChannel, true
	case cursorauth.IsInvalidModel(err):
		status, action = http.StatusBadRequest, cooldown.ActionSkipChannel
	case rateLimited, cursorauth.IsUsageLimitExceeded(err):
		status = http.StatusTooManyRequests
	case cursorauth.IsContextLengthExceeded(err):
		status, action, code = http.StatusBadRequest, cooldown.ActionReturnClient, cursorContextLengthExceeded
	case errors.Is(err, cursorauth.ErrToolTurnInProgress):
		status, action, code = http.StatusConflict, cooldown.ActionReturnClient, "tool_turn_in_progress"
	case errors.Is(err, cursorauth.ErrInvalidRequest):
		status, action = http.StatusBadRequest, cooldown.ActionReturnClient
	}
	result := cursorErrorResult(cfg, status, err.Error(), action, format, code)
	result.candidateSkipped = skipped
	result.protocolCapabilityMissing = accountCapabilityMissing
	if replayed || (status != http.StatusTooManyRequests && status != util.StatusStreamIncomplete) {
		return result
	}
	in := cooldownInputForModel(httpErrorInputFromParts(cfg.ID, cooldown.NoKeyIndex, status, result.body, nil), modelID)
	in.ModelScoped = true
	if !retryAt.IsZero() {
		// 持久化以 Unix 秒记录截止时间，向上取整以免提前重试。
		in.ModelCooldownUntil = retryAt
		if nanos := retryAt.Nanosecond(); nanos != 0 {
			in.ModelCooldownUntil = retryAt.Add(time.Second - time.Duration(nanos))
		}
		if deferCooldown {
			result.header.Set("Retry-After", in.ModelCooldownUntil.UTC().Format(http.TimeFormat))
		} else {
			result.header.Set("Retry-After", strconv.FormatInt(int64((retryAt.Sub(now)-1)/time.Second)+1, 10))
		}
	}
	if !deferCooldown {
		result.nextAction = s.applyCooldownDecision(ctx, cfg, in)
	}
	return result
}

/**
 * @brief 构造下游协议可解析的 Cursor JSON 错误，可复用于对应的 SSE 错误帧。
 * @param cfg 当前渠道。
 * @param status 代理判定的 HTTP 或内部状态。
 * @param message 可公开的错误说明。
 * @param action 后续路由动作。
 * @param format 下游协议名称。
 * @param code 客户端可识别的错误原因，空值省略。
 * @return 含协议错误正文和路由动作的失败结果。
 */
func cursorErrorResult(cfg *model.Config, status int, message string, action cooldown.Action, format, code string) *proxyResult {
	channelID := cfg.ID
	var body []byte
	switch format {
	case "anthropic":
		if code != "" && !strings.Contains(message, code) {
			message = code + ": " + message
		}
		body, _ = json.Marshal(anthropicErrorBody(status, message))
	case "gemini":
		body = cursorGeminiErrorBody(status, message, code)
	default:
		kind := "api_error"
		switch status {
		case http.StatusBadRequest, http.StatusConflict:
			kind = "invalid_request_error"
		case http.StatusForbidden:
			kind = "permission_error"
		case http.StatusTooManyRequests:
			kind = "rate_limit_error"
		case util.StatusStreamIncomplete:
			kind = "server_error"
		}
		detail := map[string]any{"message": message, "type": kind}
		if code != "" {
			detail["code"] = code
		}
		body, _ = json.Marshal(map[string]any{"error": detail})
	}
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
		"id": id, "object": "chat.completion.chunk",
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
			"id": id, "object": "chat.completion.chunk",
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
		"id": id, "object": "chat.completion",
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
 * @param result 已启动 Cursor run 的转发或会话结算结果，必须非 nil。
 * @param usage 本次请求已收到的真实增量合计；零值表示没有真实消耗。
 * @param estimate 与协议响应共用的当前轮展示估算；有真实报告时可为空。
 * @note 估算不写入真实 Token 字段，调用方不得在认证或发送前失败时调用。
 */
func applyCursorUsage(result *fwResult, usage cursorauth.Usage, estimate *cursorauth.Usage) {
	if usage == (cursorauth.Usage{}) && estimate != nil {
		result.UsageEstimate = &model.TokenUsageEstimate{InputTokens: estimate.InputTokens, OutputTokens: estimate.OutputTokens}
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
	return max(1, estimateTokens(&count)+len(request.Images)*estimatedTokensPerImage)
}

/**
 * @brief 以当前轮输入和已生成的内容估算供展示的 Token 用量。
 * @param inputTokens 已归一化请求的输入 Token 估算。
 * @param text 当前轮正文。
 * @param thinking 当前轮思考内容。
 * @param calls 当前轮生成的工具调用。
 * @return 估算输入、输出及总用量；没有输出时输出用量为零。
 */
func estimatedCursorResponseUsage(inputTokens int, text, thinking string, calls []cursorauth.ToolCall) *cursorauth.Usage {
	outputTokens := estimateTextTokens(text) + estimateTextTokens(thinking)
	for _, call := range calls {
		outputTokens += estimateToolName(call.Name) + max(1, len(call.Arguments)/4) + 8
	}
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

/**
 * @brief 将完整 Cursor Chat SSE 帧转换后写入下游，并记录 Responses 序列位置。
 * @param ctx 当前请求上下文。
 * @param registry 协议转换注册表。
 * @param w 客户端响应写入器。
 * @param client 下游协议。
 * @param modelID Cursor 实际模型。
 * @param originalRequest 原始客户端请求。
 * @param translatedRequest 转换后的 Chat 请求。
 * @param raw 待转换的上游 Chat SSE 帧。
 * @param state 转换器的跨帧状态。
 * @param responsesCursor 已写入的 Responses 帧位置与成功终态。
 * @return 转换或写入失败时返回错误；全部成功时为 nil。
 */
func writeCursorTranslatedStream(
	ctx context.Context,
	registry *protocol.Registry,
	w http.ResponseWriter,
	client protocol.Protocol,
	modelID string,
	originalRequest, translatedRequest, raw []byte,
	state *any,
	responsesCursor *responsesStreamCursor,
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
			if client == protocol.Codex {
				eventType, data := parseSSEEventChunk(chunk)
				event := parseJSONBytesNoCopy(data)
				responsesCursor.observe(event, responsesTerminalType(eventType, event))
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

/**
 * @brief 构造 Responses 的终结错误事件并接续已发出的事件序号。
 * @param message 可公开的错误说明。
 * @param code 客户端可识别的错误码；空值表示上游失败。
 * @param sequence 当前流的下一个事件序号。
 * @return 完整的 error SSE 帧。
 */
func cursorResponsesStreamError(message, code string, sequence int64) []byte {
	if code == "" {
		code = "server_error"
	}
	payload, _ := json.Marshal(map[string]any{
		"type":            "error",
		"code":            code,
		"message":         message,
		"param":           nil,
		"sequence_number": sequence,
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

/**
 * @brief 把 Cursor 失败映射为 Gemini JSON 或 SSE data 使用的 Google API 错误。
 * @param status 网关代理状态，内部状态码按客户端可见的 HTTP 状态转换。
 * @param message 可公开的错误说明。
 * @param code 可供客户端处理的错误原因，空值省略 ErrorInfo。
 * @return 包含 HTTP 数字码、规范状态名及可选结构化原因的 JSON。
 */
func cursorGeminiErrorBody(status int, message, code string) []byte {
	status = util.ClientStatusFor(status)
	canonical := "INTERNAL"
	switch status {
	case http.StatusBadRequest:
		canonical = "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		canonical = "UNAUTHENTICATED"
	case http.StatusForbidden:
		canonical = "PERMISSION_DENIED"
	case http.StatusConflict:
		canonical = "ABORTED"
	case http.StatusTooManyRequests:
		canonical = "RESOURCE_EXHAUSTED"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		canonical = "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		canonical = "DEADLINE_EXCEEDED"
	case StatusClientClosedRequest:
		canonical = "CANCELLED"
	}
	failure := map[string]any{"code": status, "status": canonical, "message": message}
	if code != "" {
		failure["details"] = []map[string]string{{
			"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
			"domain": "ccload",
			"reason": strings.ToUpper(code),
		}}
	}
	body, _ := json.Marshal(map[string]any{"error": failure})
	return body
}
