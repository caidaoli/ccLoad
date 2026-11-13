package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"
	"ccLoad/internal/thinktag"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// HandleChannelChat 对话端点：流式上游实时透传，非流式上游归一化为前端 SSE。
// POST /admin/channels/:id/chat
// 请求体与 /test 完全一致，stream=false 时上游走非流式请求。
// 响应始终为 text/event-stream，每条事件包含 delta 文本，结束时发送 [DONE]。
// 错误时发送 data: {"error":"..."} 事件。
func (s *Server) HandleChannelChat(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		writeChatErrorEvent(c, "invalid channel id")
		return
	}

	var testReq testutil.TestChannelRequest
	if err := BindAndValidate(c, &testReq); err != nil {
		writeChatErrorEvent(c, "invalid request")
		return
	}

	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		writeChatErrorEvent(c, "channel not found")
		return
	}

	if cfg.IsSystemOne() {
		writeChatErrorEvent(c, "System One channels only support native decision tests")
		return
	}
	if err := validateTestClientProtocol(cfg, &testReq); err != nil {
		writeChatErrorEvent(c, err.Error())
		return
	}

	persistedCfg := cfg
	cfg, err = s.selectChannelTestModel(cfg, &testReq, false)
	if err != nil {
		writeChatErrorEvent(c, err.Error())
		return
	}

	apiKeys, err := s.store.GetAPIKeys(c.Request.Context(), id)
	if err != nil {
		writeChatErrorEvent(c, "failed to load api keys")
		return
	}
	cfg, keySelection, err := s.prepareChannelTestAuth(
		c.Request.Context(), cfg, apiKeys, testReq.Model, resolveClientProtocol(&testReq), testReq.KeyIndex, strings.TrimSpace(testReq.APIKey),
		oauthCredentialRefreshIfNeeded,
	)
	if err != nil {
		writeChatErrorEvent(c, err.Error())
		return
	}

	s.bindChannelTestBilling(cfg, &testReq)

	if strings.TrimSpace(testReq.Content) == "" && len(testReq.Messages) == 0 {
		testReq.Content = configuredChannelTestContent(s.configService)
	}

	originalModel := testReq.Model
	clientProtocol := resolveClientProtocol(&testReq)

	urls := cfg.GetURLs()
	if len(urls) == 0 {
		writeChatErrorEvent(c, "渠道URL为空")
		return
	}

	var selector *URLSelector
	if len(urls) > 1 && s.urlSelector != nil {
		selector = s.urlSelector
	}
	orderedURLs := orderChannelAttemptURLs(selector, cfg, urls)
	switch cfg.GetProtocolTransformMode() {
	case model.ProtocolTransformModeAuto:
		orderedURLs = prioritizeAutomaticProtocolURLs(orderedURLs, cfg.URLs)
	case model.ProtocolTransformModeLocal:
		orderedURLs = prioritizeDeclaredProtocolURLs(orderedURLs, cfg.URLs)
	}

	// 设置 SSE 响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	disableResponseWriteTimeout(c.Writer, "聊天流式")

	if cfg.UsesCursorOAuth() {
		s.streamCursorChannelChat(c, persistedCfg, cfg, keySelection, &testReq, originalModel, clientProtocol)
		return
	}

	var lastResult map[string]any
	var urlPolicy channelURLAttemptPolicy
	for idx, entry := range orderedURLs {
		upstreamProtocols := resolveConfiguredURLUpstreamProtocols(
			cfg, configuredURLAt(cfg, entry.idx, entry.url), clientProtocol,
		)
		if len(upstreamProtocols) == 0 {
			lastResult = map[string]any{
				"success":  false,
				"error":    fmt.Sprintf("URL 不支持当前协议 %s", clientProtocol),
				"base_url": entry.url,
			}
			continue
		}

		capabilityExhausted := false
		for protocolIdx, upstreamProtocol := range upstreamProtocols {
			attempt := s.streamChatWithURLForProtocol(
				c, cfg, keySelection, &testReq, clientProtocol, upstreamProtocol, entry.url, originalModel,
			)
			if attempt.handled {
				if attempt.streamResult != nil {
					attempt.streamResult.capacityRetries = urlPolicy.antigravityCapacityRetries
				}
				if attempt.succeeded && urlPolicy.antigravityCapacityRetries > 0 {
					s.applyChannelTestResultCooldown(
						c.Request.Context(), cfg, keySelection.keyIndex, &testReq,
						keySelection.updatePersistedCooldown, map[string]any{
							"success": true, "status_code": http.StatusOK, "actual_model": attempt.actualModel,
						},
					)
				}
				// Write chat log from stream result
				s.writeChatStreamLog(c, persistedCfg, &testReq, keySelection, attempt.streamResult, originalModel)
				return
			}
			lastResult = attempt.result
			if (cfg.UsesZAIOAuth() && testReq.ZAI.Trial) || !isChannelTestProtocolEndpointMissing(cfg, lastResult) {
				break
			}
			capabilityExhausted = protocolIdx == len(upstreamProtocols)-1
		}
		hasNextURL := idx < len(orderedURLs)-1
		decision := urlPolicy.decide(cfg, hasNextURL, channelURLFailureFromTestResult(lastResult))
		if decision.firstCapacity && keySelection.updatePersistedCooldown {
			s.applyChannelTestCapacityCooldown(
				c.Request.Context(), cfg, keySelection.keyIndex, &testReq, lastResult,
			)
		}
		if decision.retry {
			if err := waitForChannelURLRetry(c.Request.Context(), decision.delay); err != nil {
				lastResult["error"] = "渠道测试已取消: " + err.Error()
				break
			}
			continue
		}
		if decision.capacity {
			lastResult["antigravity_capacity_cooldown_applied"] = urlPolicy.antigravityCapacityObserved
			markChannelTestCapacityExhausted(lastResult, urlPolicy.antigravityCapacityRetries)
		}
		if !hasNextURL {
			break
		}
		if cfg.UsesZAIOAuth() && testReq.ZAI.Trial {
			// 体验使用固定端点，不能因协议能力回退而重复发送。
			break
		}
		if capabilityExhausted && cfg.GetProtocolTransformMode() != model.ProtocolTransformModeUpstream {
			continue
		}
		continueFallback, shouldCooldown := shouldFallbackToNextURL(lastResult)
		if shouldCooldown && selector != nil {
			selector.CooldownURL(cfg.ID, entry.url)
		}
		if !continueFallback {
			break
		}
	}

	if lastResult != nil {
		writeChatErrorEvent(c, chatErrorMessageFromResult(lastResult))
		if capacity, _ := lastResult["antigravity_model_capacity"].(bool); capacity || (cfg.UsesZAIOAuth() && testReq.ZAI.Trial) {
			lastResult = s.applyChannelTestResultCooldown(
				c.Request.Context(), cfg, keySelection.keyIndex, &testReq,
				keySelection.updatePersistedCooldown, lastResult,
			)
		}
		logModel, logThinking := channelTestLogIdentity(originalModel, testReq.ThinkingEffort)
		s.persistDetectionLog(c.Request.Context(), detectionLogFromResult(persistedCfg, model.LogSourceManualChat, logModel, channelTestActualModel(lastResult, testReq.Model), keySelection, c.ClientIP(), logThinking, lastResult))
		return
	}
	writeChatErrorEvent(c, "渠道测试失败: 未找到可用URL")
}

type chatURLAttemptResult struct {
	handled      bool
	succeeded    bool
	actualModel  string
	result       map[string]any
	streamResult *chatStreamResult
}

/**
 * @brief 保存聊天展示用量、真实 SDK 用量与请求时序。
 */
type chatStreamResult struct {
	start            time.Time
	firstContentTime time.Time
	usageParser      *sseUsageParser
	model            string
	clientProtocol   string
	upstreamProtocol string
	statusCode       int
	requestThinking  string
	capacityRetries  int
	errorResult      map[string]any
	debugData        *model.DebugLogEntry
	forwarded        *fwResult ///< Cursor 的真实转发用量；nil 表示尚未发送或提前失败。

	// requestedServiceTier 是上游请求体声明的计费档位，与上游回显合并后参与计费。
	requestedServiceTier string
}

/**
 * @brief 按真实上游用量计算聊天成本，Cursor 的客户端估算不参与计费。
 * @param testReq 已绑定渠道价格的测试请求。
 * @return 本次聊天的标准成本。
 * @return 存在可计费用量时为 true，否则为 false。
 */
func (sr *chatStreamResult) costUSD(testReq *testutil.TestChannelRequest) (float64, bool) {
	actualModel := sr.model
	if actualModel == "" {
		actualModel = testReq.Model
	}
	if sr.upstreamProtocol == "cursor-sdk-bridge" {
		return channelTestCostFromUsage(testReq, actualModel, sr.requestedServiceTier, sr.forwarded)
	}
	return channelTestCostUSD(testReq, actualModel, sr.requestedServiceTier, sr.usageParser)
}

func chatSummaryEventChunk(sr *chatStreamResult, testReq *testutil.TestChannelRequest) []byte {
	if sr == nil {
		return nil
	}
	summary := map[string]any{}

	durationMs := time.Since(sr.start).Milliseconds()
	summary["duration_ms"] = durationMs

	if !sr.firstContentTime.IsZero() {
		summary["first_byte_ms"] = sr.firstContentTime.Sub(sr.start).Milliseconds()
	}

	if sr.usageParser != nil {
		input, output, cacheRead, cacheCreation := sr.usageParser.GetUsage()
		summary["input_tokens"] = input
		summary["output_tokens"] = output
		if reasoningTokens := sr.usageParser.GetReasoningTokens(); reasoningTokens > 0 {
			summary["reasoning_tokens"] = reasoningTokens
		}
		summary["cache_read"] = cacheRead
		summary["cache_create"] = cacheCreation

		if cost, ok := sr.costUSD(testReq); ok {
			summary["cost_usd"] = cost
		}
	}

	jsonBytes, err := sonic.Marshal(map[string]any{"summary": summary})
	if err != nil {
		return nil
	}
	return []byte("data: " + string(jsonBytes) + "\n\n")
}

/**
 * @brief 通过 SDK 执行 Cursor 管理聊天，展示响应用量并单独记录真实消费。
 * @param c 管理聊天的 HTTP 上下文。
 * @param persistedCfg 用于写日志的持久化渠道。
 * @param cfg 实际执行的渠道和凭证快照。
 * @param keySelection 当前测试凭证及计费倍率。
 * @param testReq 已绑定价格的管理聊天请求。
 * @param originalModel 重定向前的请求模型。
 * @param clientProtocol 客户端协议，用于解码展示 SSE。
 */
func (s *Server) streamCursorChannelChat(
	c *gin.Context,
	persistedCfg, cfg *model.Config,
	keySelection channelTestKeySelection,
	testReq *testutil.TestChannelRequest,
	originalModel, clientProtocol string,
) {
	start := time.Now()
	attemptReq := *testReq
	selected, _ := s.firstModelRow(cfg, originalModel)
	attemptReq.Model = s.resolveFinalUpstreamModel(cfg, selected, clientProtocol)
	// The admin endpoint always speaks SSE to the browser. The SDK itself also
	// streams, so emitting a native streaming envelope avoids buffering a
	// nominally non-streaming test before it is normalized for the frontend.
	attemptReq.Stream = true

	sr := &chatStreamResult{
		start:            start,
		usageParser:      newSSEUsageParser(clientProtocol),
		model:            attemptReq.Model,
		clientProtocol:   clientProtocol,
		upstreamProtocol: "cursor-sdk-bridge",
		statusCode:       http.StatusOK,
		requestThinking:  testReq.ThinkingEffort,
	}

	fail := func(status int, message string) {
		sr.statusCode = status
		sr.errorResult = map[string]any{
			"success":           false,
			"status_code":       status,
			"is_streaming":      true,
			"duration_ms":       time.Since(start).Milliseconds(),
			"error":             message,
			"client_protocol":   clientProtocol,
			"upstream_protocol": "cursor-sdk-bridge",
		}
		writeChatErrorEvent(c, message)
		if summary := chatSummaryEventChunk(sr, &attemptReq); len(summary) > 0 {
			writeChatFrontendChunks(c, summary)
		}
		s.writeChatStreamLog(c, persistedCfg, &attemptReq, keySelection, sr, originalModel)
	}

	requestPath := channelTestClientRequestPath(clientProtocol, &attemptReq)
	if !cursorSupportsRequestFamily(requestPath) {
		fail(http.StatusBadRequest, "Cursor OAuth 仅支持 Anthropic messages、OpenAI chat completions/responses 与 Gemini generateContent")
		return
	}

	clientTester := newChannelTester(clientProtocol)
	_, headers, body, err := clientTester.Build(cfg.Clone(), keySelection.requestCredential, &attemptReq)
	if err != nil {
		fail(http.StatusBadRequest, "构造测试请求失败: "+err.Error())
		return
	}
	body = applyThinkingSuffix(body, protocol.Protocol(clientProtocol), originalModel)

	credential, err := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil || credential == nil {
		message := "Cursor 凭证不可用"
		if err != nil {
			message = "加载 Cursor 凭证失败: " + err.Error()
		}
		fail(http.StatusUnauthorized, message)
		return
	}
	if strings.TrimSpace(credential.APIKey) == "" {
		fail(http.StatusUnauthorized, "Cursor 推理需要导入 User API Key")
		return
	}

	if testReq.WaitForCapacity {
		release, waitErr := s.waitForUpstreamRequest(c.Request.Context(), cfg)
		if waitErr != nil {
			fail(http.StatusServiceUnavailable, waitErr.Error())
			return
		}
		defer release()
	}

	writer := &cursorChatResponseWriter{
		c:     c,
		usage: sr.usageParser,
		state: &thinktag.Parser{},
		onOutput: func() {
			if sr.firstContentTime.IsZero() {
				sr.firstContentTime = time.Now()
			}
		},
	}
	proxyReq := &proxyRequestContext{
		originalModel:  originalModel,
		clientProtocol: protocol.Protocol(clientProtocol),
		requestPath:    requestPath,
		header:         headers,
		body:           body,
		isStreaming:    true,
		skipProxyLog:   true,
	}
	result, forwardErr := s.forwardCursorAgent(c.Request.Context(), cfg, credential, proxyReq, writer)
	finalChunks := chatChunksFromTextParts(writer.state.Flush())
	if chatFrontendChunksHaveVisibleContent(finalChunks) {
		writer.onOutput()
	}
	writeChatFrontendChunks(c, finalChunks...)
	sr.debugData = proxyReq.debugData
	if forwardErr != nil {
		fail(http.StatusBadGateway, forwardErr.Error())
		return
	}
	if result == nil {
		fail(http.StatusBadGateway, "Cursor SDK Bridge 未返回结果")
		return
	}
	sr.forwarded = result.forwarded
	sr.statusCode = result.status
	if result.status < http.StatusOK || result.status >= http.StatusMultipleChoices || !result.succeeded {
		message := extractChatUpstreamError(result.status, result.body)
		sr.errorResult = map[string]any{
			"success":                false,
			"status_code":            result.status,
			"is_streaming":           true,
			"duration_ms":            time.Since(start).Milliseconds(),
			"error":                  message,
			"raw_response":           string(result.body),
			"upstream_response_body": string(result.body),
		}
		// A failure after streaming began was already converted by writer.
		if len(sr.usageParser.GetLastError()) == 0 {
			writeChatErrorEvent(c, message)
		}
	}

	if summary := chatSummaryEventChunk(sr, &attemptReq); len(summary) > 0 {
		writeChatFrontendChunks(c, summary)
	}
	s.writeChatStreamLog(c, persistedCfg, &attemptReq, keySelection, sr, originalModel)
}

/**
 * @brief 将 Cursor 客户端协议 SSE 转为管理聊天使用的正文、思考和终态事件。
 */
type cursorChatResponseWriter struct {
	c        *gin.Context
	usage    *sseUsageParser
	state    *thinktag.Parser ///< 当前响应独占的跨分片思考标签解析状态。
	onOutput func()
}

func (w *cursorChatResponseWriter) Header() http.Header {
	return w.c.Writer.Header()
}

func (w *cursorChatResponseWriter) WriteHeader(_ int) {
	// HandleChannelChat has already committed the endpoint's invariant HTTP 200
	// SSE status. Upstream failures are represented as data.error events.
}

func (w *cursorChatResponseWriter) Write(p []byte) (int, error) {
	if w.usage != nil {
		_ = w.usage.Feed(p)
	}
	chunks := chatFrontendChunksFromSSEEventWithState(p, w.state)
	if chatFrontendChunksHaveVisibleContent(chunks) && w.onOutput != nil {
		w.onOutput()
	}
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		if _, err := w.c.Writer.Write(chunk); err != nil {
			return 0, err
		}
		w.c.Writer.Flush()
	}
	return len(p), nil
}

func (w *cursorChatResponseWriter) Flush() {
	w.c.Writer.Flush()
}

func (w *cursorChatResponseWriter) Unwrap() http.ResponseWriter {
	return w.c.Writer
}

/**
 * @brief 对指定 URL 执行一次聊天请求，保留真实套餐身份用于外层处理。
 * @param c 管理聊天请求及事件输出上下文。
 * @param cfg 待检测的渠道配置。
 * @param keySelection 本次认证材料及持久化冷却目标。
 * @param testReq 检测参数，会回传实际套餐标记。
 * @param clientProtocol 聊天输入协议。
 * @param upstreamProtocol 本次上游协议。
 * @param selectedURL 已选定的上游地址。
 * @param originalModel 原始路由模型名。
 * @return 本次尝试结果；handled 为 true 时已输出响应，不再回退。
 * @note 发送聊天事件并清理本次超时和响应体。
 */
func (s *Server) streamChatWithURLForProtocol(
	c *gin.Context,
	cfg *model.Config,
	keySelection channelTestKeySelection,
	testReq *testutil.TestChannelRequest,
	clientProtocol, upstreamProtocol, selectedURL string,
	originalModel string,
) (out chatURLAttemptResult) {
	attemptReq := *testReq
	callerReq := testReq
	attemptReq.ZAI = testutil.ZAITestOutcome{}
	selected, _ := s.firstModelRow(cfg, originalModel)
	attemptReq.Model = s.resolveFinalUpstreamModel(cfg, selected, upstreamProtocol)
	testReq = &attemptReq
	defer func() {
		callerReq.ZAI = attemptReq.ZAI
		out.actualModel = attemptReq.Model
		if out.result == nil {
			return
		}
		out.result["client_protocol"] = clientProtocol
		out.result["upstream_protocol"] = upstreamProtocol
		out.result["actual_model"] = attemptReq.Model
		markOpenCodeProtocolCapabilityMissing(cfg, selectedURL, out.result)
	}()

	req, requestPlan, cancel, err := s.buildTestUpstreamRequestForProtocol(
		c.Request.Context(), cfg, keySelection.requestCredential, testReq, originalModel, clientProtocol, upstreamProtocol, selectedURL,
	)
	if err != nil {
		if isAutomaticProtocolTranslationFailure(cfg, err) {
			return chatURLAttemptResult{result: map[string]any{
				"success":                     false,
				"error":                       err.Error(),
				"protocol_capability_missing": true,
			}}
		}
		writeChatErrorEvent(c, err.Error())
		return chatURLAttemptResult{handled: true}
	}
	defer cancel()
	requestThinking := testRequestThinkingEffort(testReq, requestPlan)

	start := time.Now()
	resp, err := s.doUpstreamRequest(cfg, req)
	s.syncZAIChannelTestRequest(cfg, requestPlan, req, resp, testReq, err)
	if err != nil {
		result := chatRequestErrorResult(start, testReq, requestPlan.timeout, err)
		if requestThinking != "" {
			result["thinking_effort"] = requestThinking
		}
		return chatURLAttemptResult{result: attachTestDebugData(requestPlan, nil, result)}
	}
	s.persistDetectionCodexPassiveUsage(req.Context(), cfg, resp, gjson.GetBytes(requestPlan.requestBody, "model").String())
	s.persistDetectionAnthropicPassiveUsage(req.Context(), cfg, resp)
	defer func() { _ = resp.Body.Close() }()
	if isAnthropicClaudeCodeMessagesRequest(cfg, protocol.Protocol(requestPlan.upstreamProtocol), requestPlan.endpointPath) {
		if decodeErr := decodeAnthropicResponse(resp); decodeErr != nil {
			return chatURLAttemptResult{result: attachTestDebugData(requestPlan, resp, map[string]any{
				"success": false, "error": "解码 Anthropic 对话响应失败: " + decodeErr.Error(),
				"duration_ms": time.Since(start).Milliseconds(), "status_code": resp.StatusCode,
				"is_streaming": testReq.Stream,
			})}
		}
	}
	if requestPlan.debugCapture != nil {
		requestPlan.debugCapture.wrapResponseBody(resp)
	}

	contentType := resp.Header.Get("Content-Type")
	isSSE := responseIsSSE(resp, requestPlan.upstreamStreaming)
	wrapCodexSSEResponseBody(resp, protocol.Protocol(requestPlan.upstreamProtocol), isSSE)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		msg := extractChatUpstreamError(resp.StatusCode, body)
		result := map[string]any{
			"success":                false,
			"status_code":            resp.StatusCode,
			"is_streaming":           testReq.Stream,
			"duration_ms":            time.Since(start).Milliseconds(),
			"error":                  msg,
			"raw_response":           string(body),
			"upstream_response_body": string(body),
			"response_headers":       flattenHeader(resp.Header),
		}
		if requestThinking != "" {
			result["thinking_effort"] = requestThinking
		}
		return chatURLAttemptResult{result: attachTestDebugData(requestPlan, resp, result)}
	}

	if !isSSE {
		succeeded := s.streamChatNonStreamResponse(c, resp, requestPlan, testReq, contentType, start, cfg, keySelection, requestThinking, originalModel)
		return chatURLAttemptResult{handled: true, succeeded: succeeded, actualModel: attemptReq.Model}
	}

	sr := &chatStreamResult{
		start:                start,
		usageParser:          newSSEUsageParser(requestPlan.upstreamProtocol),
		model:                testReq.Model,
		clientProtocol:       requestPlan.clientProtocol,
		upstreamProtocol:     requestPlan.upstreamProtocol,
		statusCode:           resp.StatusCode,
		requestThinking:      requestThinking,
		requestedServiceTier: requestPlan.requestedServiceTier(),
	}

	firstContentMarked := false
	markFirstContent := func() {
		if firstContentMarked {
			return
		}
		firstContentMarked = true
		sr.firstContentTime = time.Now()
		requestPlan.timeout.markFirstStreamContent()
	}

	var streamErr error
	if clientProtocol == requestPlan.upstreamProtocol && !requestPlan.antigravityOAuth {
		// 原生协议：直接透传 SSE，提取 delta 文本
		streamErr = streamChatNativeWithFirstContent(c, resp.Body, markFirstContent, sr)
	} else {
		// 协议转换：先翻译再透传
		streamErr = streamChatTranslated(c, resp, requestPlan, testReq, s, markFirstContent, sr)
	}
	if streamErr != nil {
		result := chatRequestErrorResult(start, testReq, requestPlan.timeout, streamErr)
		if _, ok := result["status_code"]; !ok && resp.StatusCode > 0 {
			result["status_code"] = resp.StatusCode
		}
		sr.errorResult = result
		writeChatErrorEvent(c, chatErrorMessageFromResult(result))
	} else if errorEvent := sr.usageParser.GetLastError(); len(errorEvent) > 0 {
		sr.errorResult = chatSSEErrorResult(start, resp.StatusCode, errorEvent)
	}

	// Write summary event after stream ends
	if summaryChunk := chatSummaryEventChunk(sr, testReq); len(summaryChunk) > 0 {
		writeChatFrontendChunks(c, summaryChunk)
	}
	if requestPlan.debugCapture != nil {
		sr.debugData = requestPlan.debugCapture.buildEntry(resp)
	}
	return chatURLAttemptResult{handled: true, succeeded: sr.errorResult == nil, actualModel: attemptReq.Model, streamResult: sr}
}

func chatSSEErrorResult(start time.Time, upstreamStatus int, errorEvent []byte) map[string]any {
	errorMsg := "上游返回错误"
	var obj map[string]any
	if err := sonic.Unmarshal(errorEvent, &obj); err == nil {
		if msg, _, matched := extractSSEErrorMessage(obj); matched && strings.TrimSpace(msg) != "" {
			errorMsg = msg
		}
	}
	statusCode := classifySSEErrorStatus(errorEvent)
	if statusCode <= 0 {
		statusCode = upstreamStatus
	}
	return map[string]any{
		"success":                false,
		"status_code":            statusCode,
		"is_streaming":           true,
		"duration_ms":            time.Since(start).Milliseconds(),
		"error":                  errorMsg,
		"raw_response":           string(errorEvent),
		"upstream_response_body": string(errorEvent),
	}
}

/**
 * @brief 归一化聊天网络错误，体验传输失败保留分类、取消不冷却。
 * @param start 请求开始时间，用于计算耗时。
 * @param testReq 检测参数及套餐标记，允许 nil。
 * @param timeout 协议超时状态，允许 nil。
 * @param err 实际请求错误，必须非 nil。
 * @return 统一的网络、限流、取消或超时结果，不写入冷却。
 */
func chatRequestErrorResult(start time.Time, testReq *testutil.TestChannelRequest, timeout *channelTestTimeout, err error) map[string]any {
	isStream := testReq != nil && testReq.Stream

	if errors.Is(err, ErrChannelRPMExceeded) {
		result := channelRPMExceededTestResult(start, channelRPMRetryAfter(err))
		result["is_streaming"] = isStream
		return result
	}
	if errors.Is(err, ErrChannelConcurrencyExceeded) {
		result := channelConcurrencyExceededTestResult(start, err)
		result["is_streaming"] = isStream
		return result
	}

	errorMsg := "网络请求失败: " + err.Error()
	statusCode := 0
	if timeout != nil && timeout.firstStreamContentTimeoutTriggered() {
		threshold := timeout.firstByteTimeout
		errorMsg = fmt.Sprintf(
			"流式请求首个有效内容超时: upstream first valid stream content timeout after %.2fs (threshold=%v): %v",
			time.Since(start).Seconds(),
			threshold,
			err,
		)
		statusCode = util.StatusFirstByteTimeout
	} else if timeout != nil && timeout.streamTimeoutTriggered() {
		errorMsg = fmt.Sprintf(
			"流式请求总超时: upstream stream timeout after %.2fs (threshold=%v): %v",
			time.Since(start).Seconds(),
			timeout.streamTimeout,
			err,
		)
		statusCode = util.StatusStreamIncomplete
	} else if testReq != nil && !testReq.Stream && timeout != nil && timeout.nonStreamTimeout > 0 && errors.Is(err, context.DeadlineExceeded) {
		errorMsg = fmt.Sprintf(
			"非流式请求超时: upstream timeout after %.2fs (threshold=%v): %v",
			time.Since(start).Seconds(),
			timeout.nonStreamTimeout,
			err,
		)
		statusCode = http.StatusGatewayTimeout
	}

	result := map[string]any{
		"success":      false,
		"error":        errorMsg,
		"duration_ms":  time.Since(start).Milliseconds(),
		"is_streaming": isStream,
	}
	if statusCode > 0 {
		result["status_code"] = statusCode
	}
	annotateZAITrialTestError(result, testReq, err)
	return result
}

func chatErrorMessageFromResult(result map[string]any) string {
	if result == nil {
		return "渠道测试失败"
	}
	if msg, _ := result["error"].(string); strings.TrimSpace(msg) != "" {
		return msg
	}
	if statusCode, ok := getResultInt(result["status_code"]); ok && statusCode > 0 {
		return fmt.Sprintf("上游返回错误 HTTP %d", statusCode)
	}
	return "渠道测试失败"
}

/**
 * @brief 将非流式结果输出为聊天事件，体验结果同步更新对应模型冷却。
 * @param c 聊天事件输出上下文。
 * @param resp 待读取的上游响应。
 * @param requestPlan 响应解析、超时及调试计划。
 * @param testReq 检测参数及实际套餐标记。
 * @param contentType 响应媒体类型。
 * @param start 请求开始时间，用于计算耗时。
 * @param cfg 待检测的渠道配置。
 * @param keySelection 本次认证材料及持久化冷却目标。
 * @param requestThinking 请求的思考等级。
 * @param originalModel 原始路由模型名。
 * @return 是否成功取得可显示文本；错误或空内容时为 false。
 * @note 读取响应体、输出事件并记录日志，Trial 失败可写模型冷却；响应体由调用方关闭。
 */
func (s *Server) streamChatNonStreamResponse(
	c *gin.Context,
	resp *http.Response,
	requestPlan *channelTestRequestPlan,
	testReq *testutil.TestChannelRequest,
	contentType string,
	start time.Time,
	cfg *model.Config,
	keySelection channelTestKeySelection,
	requestThinking string,
	originalModel string,
) bool {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		if cfg.UsesZAIOAuth() && testReq.ZAI.Trial {
			failure := chatRequestErrorResult(start, testReq, requestPlan.timeout, err)
			s.applyChannelTestResultCooldown(c.Request.Context(), cfg, keySelection.keyIndex, testReq, keySelection.updatePersistedCooldown, failure)
		}
		errorMsg := "读取响应失败: " + err.Error()
		if _, timeoutMsg, ok := s.describeChannelTestTimeoutError(start, testReq, requestPlan.timeout, err); ok {
			errorMsg = timeoutMsg
		}
		writeChatErrorEvent(c, errorMsg)
		return false
	}

	result := map[string]any{
		"success":           resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code":       resp.StatusCode,
		"is_streaming":      false,
		"client_protocol":   requestPlan.clientProtocol,
		"upstream_protocol": requestPlan.upstreamProtocol,
	}
	if requestThinking != "" {
		result["thinking_effort"] = requestThinking
	}
	result = s.parseTestNonStreamResponse(c.Request.Context(), requestPlan, testReq, resp, contentType, start, respBody, result)
	if cfg.UsesZAIOAuth() && testReq.ZAI.Trial {
		result = s.applyChannelTestResultCooldown(c.Request.Context(), cfg, keySelection.keyIndex, testReq, keySelection.updatePersistedCooldown, result)
	}
	result = attachTestDebugData(requestPlan, resp, result)
	succeeded, _ := result["success"].(bool)
	responseText, _ := result["response_text"].(string)
	if strings.TrimSpace(responseText) == "" {
		succeeded = false
		result["success"] = false
		if msg, _ := result["error"].(string); strings.TrimSpace(msg) == "" {
			result["error"] = "上游响应中没有可显示文本"
		}
	}
	writeChatNonStreamResult(c, result)
	writeChatNonStreamSummary(c, result)
	logModel, logThinking := channelTestLogIdentity(originalModel, requestThinking)
	s.persistDetectionLog(c.Request.Context(), detectionLogFromResult(cfg, model.LogSourceManualChat, logModel, testReq.Model, keySelection, c.ClientIP(), logThinking, result))
	return succeeded
}

/**
 * @brief 将完整聊天响应拆成前端正文、思考和终态事件，保留末尾未闭合的标签片段。
 * @param c 管理聊天请求上下文。
 * @param result 已解析的上游响应；失败时仅输出错误事件。
 */
func writeChatNonStreamResult(c *gin.Context, result map[string]any) {
	if success, ok := result["success"].(bool); ok && !success {
		msg, _ := result["error"].(string)
		if msg == "" {
			msg = "上游返回错误"
		}
		writeChatErrorEvent(c, msg)
		return
	}

	responseText, _ := result["response_text"].(string)
	if responseText == "" {
		msg, _ := result["error"].(string)
		if msg == "" {
			msg = "上游响应中没有可显示文本"
		}
		writeChatErrorEvent(c, msg)
		return
	}

	state := &thinktag.Parser{}
	writeChatFrontendChunks(c, chatChunksFromTextParts(state.Push(responseText))...)
	writeChatFrontendChunks(c, chatChunksFromTextParts(state.Flush())...)
	writeChatFrontendChunks(c, chatDoneEventChunk())
}

func writeChatNonStreamSummary(c *gin.Context, result map[string]any) {
	success, _ := result["success"].(bool)
	if !success {
		return
	}
	summary := map[string]any{}
	if v, ok := result["duration_ms"]; ok {
		summary["duration_ms"] = v
	}
	if v, ok := result["first_byte_duration_ms"]; ok {
		summary["first_byte_ms"] = v
	}
	if usage, ok := result["usage"].(map[string]any); ok {
		if v, ok := usage["input_tokens"]; ok {
			summary["input_tokens"] = v
		}
		if v, ok := usage["output_tokens"]; ok {
			summary["output_tokens"] = v
		}
		if v, ok := usage["reasoning_tokens"]; ok {
			summary["reasoning_tokens"] = v
		}
		if v, ok := usage["cache_read_input_tokens"]; ok {
			summary["cache_read"] = v
		}
		if v, ok := usage["cache_creation_input_tokens"]; ok {
			summary["cache_create"] = v
		}
	}
	if v, ok := result["cost_usd"]; ok {
		summary["cost_usd"] = v
	}

	jsonBytes, err := sonic.Marshal(map[string]any{"summary": summary})
	if err != nil {
		return
	}
	writeChatFrontendChunks(c, []byte("data: "+string(jsonBytes)+"\n\n"))
}

/**
 * @brief 持久化管理聊天日志，Cursor 使用真实消费并独立保存展示估算。
 * @param c 管理聊天 HTTP 上下文。
 * @param cfg 日志所属的持久化渠道。
 * @param testReq 已绑定模型价格的测试请求。
 * @param keySelection 当前凭证及成本倍率。
 * @param sr 已收集的聊天结果；nil 时不写日志。
 * @param originalModel 重定向前的请求模型。
 */
func (s *Server) writeChatStreamLog(c *gin.Context, cfg *model.Config, testReq *testutil.TestChannelRequest, keySelection channelTestKeySelection, sr *chatStreamResult, originalModel string) {
	if sr == nil {
		return
	}
	result := map[string]any{
		"success":           true,
		"status_code":       sr.statusCode,
		"is_streaming":      true,
		"duration_ms":       time.Since(sr.start).Milliseconds(),
		"message":           "ok",
		"client_protocol":   sr.clientProtocol,
		"upstream_protocol": sr.upstreamProtocol,
	}
	if sr.errorResult != nil {
		result = sr.errorResult
		result["is_streaming"] = true
		if _, ok := result["status_code"]; !ok && sr.statusCode > 0 {
			result["status_code"] = sr.statusCode
		}
	}
	result["client_protocol"] = sr.clientProtocol
	result["upstream_protocol"] = sr.upstreamProtocol
	if cfg.UsesZAIOAuth() && testReq.ZAI.Trial {
		result = s.applyChannelTestResultCooldown(c.Request.Context(), cfg, keySelection.keyIndex, testReq, keySelection.updatePersistedCooldown, result)
	}
	annotateChannelTestCapacityRetries(result, sr.capacityRetries)
	if !sr.firstContentTime.IsZero() {
		result["first_byte_duration_ms"] = sr.firstContentTime.Sub(sr.start).Milliseconds()
	}
	if sr.upstreamProtocol == "cursor-sdk-bridge" {
		populateCursorTestUsageAndCost(result, testReq, sr.requestedServiceTier, sr.forwarded)
	} else if sr.usageParser != nil {
		input, output, cacheRead, cacheCreation := sr.usageParser.GetUsage()
		result["codex_has_credits"] = sr.usageParser.GetCodexHasCredits()
		cache5m, cache1h, _ := sr.usageParser.GetCacheBreakdown()
		cache30m, cache12h := sr.usageParser.GetOpenAICacheBreakdown()
		reasoningTokens := sr.usageParser.GetReasoningTokens()
		if input+output+cacheRead+cacheCreation+reasoningTokens > 0 {
			result["usage"] = map[string]any{
				"input_tokens": input, "output_tokens": output,
				"reasoning_tokens":        reasoningTokens,
				"cache_read_input_tokens": cacheRead, "cache_creation_input_tokens": cacheCreation,
				"cache_5m_input_tokens": cache5m, "cache_1h_input_tokens": cache1h,
				"cache_30m_input_tokens": cache30m, "cache_12h_input_tokens": cache12h,
			}
		}
		if cost, ok := sr.costUSD(testReq); ok {
			result["cost_usd"] = cost
		}
		if effort := sr.usageParser.GetThinkingEffort(); effort != "" {
			result["thinking_effort"] = effort
		}
	}
	if sr.debugData != nil {
		result["debug_data"] = sr.debugData
	}
	logModel, logThinking := channelTestLogIdentity(originalModel, sr.requestThinking)
	actualModel := sr.model
	if actualModel == "" {
		actualModel = testReq.Model
	}
	s.persistDetectionLog(c.Request.Context(), detectionLogFromResult(cfg, model.LogSourceManualChat, logModel, model.RoutingModelName(actualModel), keySelection, c.ClientIP(), logThinking, result))
}

/**
 * @brief 将原生上游 SSE 归一化为前端事件，结束或中断时排空标签解析缓冲。
 * @param c 管理聊天请求上下文。
 * @param body 原生协议的上游 SSE 响应。
 * @param onFirstContent 输出可见正文或思考时调用；nil 时不通知。
 * @param sr 用量收集状态；nil 时不收集。
 * @return 读取、取消或写入失败时返回相应错误；正常 EOF 返回 nil。
 */
func streamChatNativeWithFirstContent(c *gin.Context, body io.Reader, onFirstContent func(), sr *chatStreamResult) error {
	frontendState := &thinktag.Parser{}
	streamErr := streamTransformSSEEvents(c.Request.Context(), body, c.Writer,
		func(rawEvent []byte) error {
			// Feed raw SSE bytes to usage parser as side-channel
			if sr != nil && sr.usageParser != nil {
				_ = sr.usageParser.Feed(rawEvent)
			}
			return nil
		},
		func(rawEvent []byte) ([][]byte, error) {
			chunks := chatFrontendChunksFromSSEEventWithState(rawEvent, frontendState)
			if chatFrontendChunksHaveVisibleContent(chunks) && onFirstContent != nil {
				onFirstContent()
			}
			return chunks, nil
		},
	)
	chunks := chatChunksFromTextParts(frontendState.Flush())
	if chatFrontendChunksHaveVisibleContent(chunks) && onFirstContent != nil {
		onFirstContent()
	}
	flushErr := writeSSEChunks(c.Writer, chunks)
	if streamErr != nil {
		return streamErr
	}
	return flushErr
}

/**
 * @brief 将转换后的上游 SSE 归一化为前端事件，结束或中断时排空标签解析缓冲。
 * @param c 管理聊天请求上下文。
 * @param resp 待转换的上游响应。
 * @param requestPlan 上游与客户端协议及调试捕获配置。
 * @param testReq 当前测试请求及模型。
 * @param s 提供协议转换注册表的服务实例。
 * @param onFirstContent 输出可见正文或思考时调用；nil 时不通知。
 * @param sr 用量收集状态；nil 时不收集。
 * @return 读取、转换、取消或写入失败时返回相应错误；正常 EOF 返回 nil。
 */
func streamChatTranslated(c *gin.Context, resp *http.Response, requestPlan *channelTestRequestPlan, testReq *testutil.TestChannelRequest, s *Server, onFirstContent func(), sr *chatStreamResult) error {
	var state any
	frontendState := &thinktag.Parser{}
	ctx := c.Request.Context()
	requestPlan.debugCapture.captureTranslatedResponseMeta(resp.StatusCode, resp.Header)

	src := resp.Body
	if requestPlan.antigravityOAuth {
		src = terminateAntigravitySSE(src)
	}
	streamErr := streamTransformSSEEvents(ctx, src, c.Writer,
		func(rawEvent []byte) error {
			parserEvent := rawEvent
			if requestPlan.antigravityOAuth {
				var err error
				parserEvent, err = unwrapAntigravitySSEEvent(rawEvent)
				if err != nil || parserEvent == nil {
					return err
				}
			}
			if sr != nil && sr.usageParser != nil {
				_ = sr.usageParser.Feed(parserEvent)
			}
			return nil
		},
		func(rawEvent []byte) ([][]byte, error) {
			translatedRequestBody := requestPlan.requestBody
			if requestPlan.antigravityOAuth {
				// 与代理链路一致：后端错误帧只交给 usage parser，不进转换器。
				if isAntigravityErrorPayload(antigravityEventPayload(rawEvent)) {
					return nil, nil
				}
				var err error
				rawEvent, err = unwrapAntigravitySSEEvent(rawEvent)
				if err != nil || rawEvent == nil {
					return nil, err
				}
				translatedRequestBody, err = unwrapAntigravityRequest(requestPlan.requestBody)
				if err != nil {
					return nil, err
				}
			}
			translated, err := s.protocolRegistry.TranslateResponseStream(
				ctx,
				protocol.Protocol(requestPlan.upstreamProtocol),
				protocol.Protocol(requestPlan.clientProtocol),
				testReq.Model,
				requestPlan.clientBody,
				translatedRequestBody,
				rawEvent,
				&state,
			)
			if err != nil {
				return nil, err
			}
			for _, chunk := range translated {
				requestPlan.debugCapture.captureTranslatedResponse(chunk)
			}
			var chunks [][]byte
			for _, chunk := range translated {
				chunks = append(chunks, chatFrontendChunksFromSSEEventWithState(chunk, frontendState)...)
			}
			if chatFrontendChunksHaveVisibleContent(chunks) && onFirstContent != nil {
				onFirstContent()
			}
			return chunks, nil
		},
	)
	chunks := chatChunksFromTextParts(frontendState.Flush())
	if chatFrontendChunksHaveVisibleContent(chunks) && onFirstContent != nil {
		onFirstContent()
	}
	flushErr := writeSSEChunks(c.Writer, chunks)
	if streamErr != nil {
		return streamErr
	}
	return flushErr
}

/**
 * @brief 提取前端增量事件，跨 SSE 事件保留标签状态，在终态或错误前排空残留文本。
 * @param rawEvent 一个上游 SSE 事件。
 * @param state 当前响应专用的标签解析器。
 * @return 按原始顺序排列的前端事件；无可显示增量时为空。
 */
func chatFrontendChunksFromSSEEventWithState(rawEvent []byte, state *thinktag.Parser) [][]byte {
	lines := strings.Split(string(rawEvent), "\n")
	chunks := make([][]byte, 0, 1)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			chunks = append(chunks, chatChunksFromTextParts(state.Flush())...)
			chunks = append(chunks, chatDoneEventChunk())
			continue
		}

		var obj map[string]any
		if err := sonic.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		if candidates, ok := obj["candidates"].([]any); ok && len(candidates) > 0 {
			candidate, _ := candidates[0].(map[string]any)
			content, _ := candidate["content"].(map[string]any)
			parts, _ := content["parts"].([]any)
			for _, value := range parts {
				part, _ := value.(map[string]any)
				if _, toolCall := part["functionCall"]; toolCall {
					chunks = append(chunks, chatChunksFromTextParts(state.Flush())...)
				}
				text, _ := part["text"].(string)
				if thought, _ := part["thought"].(bool); thought {
					if text != "" {
						chunks = append(chunks, chatChunksFromTextParts(state.Flush())...)
						chunks = append(chunks, chatThinkingEventChunk(text))
					}
				} else {
					chunks = append(chunks, chatChunksFromTextParts(state.Push(text))...)
				}
			}
		} else {
			if thinking := extractSSEThinkingDelta(obj); thinking != "" {
				chunks = append(chunks, chatChunksFromTextParts(state.Flush())...)
				chunks = append(chunks, chatThinkingEventChunk(thinking))
			}
			if delta := extractSSEDeltaText(obj); delta != "" {
				chunks = append(chunks, chatChunksFromTextParts(state.Push(delta))...)
			}
		}
		if isChatContentBoundary(obj) {
			chunks = append(chunks, chatChunksFromTextParts(state.Flush())...)
		}
		if isChatStopEvent(obj) {
			chunks = append(chunks, chatChunksFromTextParts(state.Flush())...)
			chunks = append(chunks, chatDoneEventChunk())
			continue
		}
		if errMsg, _, matched := extractSSEErrorMessage(obj); matched {
			if errMsg == "" {
				errMsg = "上游返回错误"
			}
			chunks = append(chunks, chatChunksFromTextParts(state.Flush())...)
			chunks = append(chunks, chatErrorEventChunk(errMsg))
		}
	}
	return chunks
}

/**
 * @brief 识别隔断连续正文的工具调用、内容块结束与 Responses 消息结束事件。
 * @param obj 已解析的单个上游 SSE 事件。
 * @return 当前事件结束连续文本段时为 true；普通文本增量为 false。
 */
func isChatContentBoundary(obj map[string]any) bool {
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if calls, _ := delta["tool_calls"].([]any); len(calls) > 0 {
			return true
		}
	}
	typ, _ := obj["type"].(string)
	switch typ {
	case "content_block_start":
		block, _ := obj["content_block"].(map[string]any)
		return block["type"] == "tool_use"
	case "content_block_delta":
		delta, _ := obj["delta"].(map[string]any)
		return delta["type"] == "input_json_delta"
	case "content_block_stop":
		return true
	case "response.output_item.added", "response.output_item.done":
		item, _ := obj["item"].(map[string]any)
		return item["type"] == "function_call" || item["type"] == "custom_tool_call" || (typ == "response.output_item.done" && item["type"] == "message")
	case "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return true
	}
	return false
}

func chatFrontendChunksHaveVisibleContent(chunks [][]byte) bool {
	return slices.ContainsFunc(chunks, chatFrontendChunkHasVisibleContent)
}

func chatFrontendChunkHasVisibleContent(chunk []byte) bool {
	for line := range strings.SplitSeq(string(chunk), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := sonic.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		if delta, _ := obj["delta"].(string); delta != "" {
			return true
		}
		if thinking, _ := obj["thinking_delta"].(string); thinking != "" {
			return true
		}
	}
	return false
}

/**
 * @brief 将已分离且非空的正文与思考片段编码为前端 SSE。
 * @param parts 按原始顺序排列的标签解析结果。
 * @return 对应的正文或思考事件；parts 为空时返回空切片。
 */
func chatChunksFromTextParts(parts []thinktag.Part) [][]byte {
	chunks := make([][]byte, 0, len(parts))
	for _, part := range parts {
		if part.Thinking {
			chunks = append(chunks, chatThinkingEventChunk(part.Text))
		} else {
			chunks = append(chunks, chatDeltaEventChunk(part.Text))
		}
	}
	return chunks
}

func extractSSEThinkingDelta(obj map[string]any) string {
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := choice["delta"].(map[string]any); ok {
				if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
					return reasoning
				}
			}
		}
	}

	if candidates, ok := obj["candidates"].([]any); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok {
			if content, ok := candidate["content"].(map[string]any); ok {
				if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]any); ok {
						if thought, _ := part["thought"].(bool); thought {
							if text, ok := part["text"].(string); ok && text != "" {
								return text
							}
						}
					}
				}
			}
		}
	}

	if typ, _ := obj["type"].(string); typ == "content_block_delta" {
		if delta, ok := obj["delta"].(map[string]any); ok {
			if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
				return thinking
			}
		}
	}
	if typ, _ := obj["type"].(string); typ == "response.reasoning_summary_text.delta" {
		if delta, ok := obj["delta"].(string); ok && delta != "" {
			return delta
		}
	}
	return ""
}

func isChatStopEvent(obj map[string]any) bool {
	typ, _ := obj["type"].(string)
	return typ == "message_stop" || typ == "response.completed"
}

func writeChatFrontendChunks(c *gin.Context, chunks ...[]byte) {
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		if _, err := c.Writer.Write(chunk); err != nil {
			return
		}
		c.Writer.Flush()
	}
}

func chatThinkingEventChunk(thinking string) []byte {
	return []byte("data: " + jsonMustMarshalString(map[string]any{"thinking_delta": thinking}) + "\n\n")
}

func chatDeltaEventChunk(delta string) []byte {
	return []byte("data: " + jsonMustMarshalString(map[string]any{"delta": delta}) + "\n\n")
}

func chatDoneEventChunk() []byte {
	return []byte("data: [DONE]\n\n")
}

func chatErrorEventChunk(msg string) []byte {
	return []byte("data: " + jsonMustMarshalString(map[string]any{"error": msg}) + "\n\n")
}

// writeChatErrorEvent 写错误事件并刷新（通过 gin.Context，尚未写 SSE 头时也能用）。
func writeChatErrorEvent(c *gin.Context, msg string) {
	// 若 SSE 头已写出，直接用 ResponseWriter；否则先写头
	w := c.Writer
	if !c.Writer.Written() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
	}
	writeChatErrorEventWriter(w, msg)
}

func writeChatErrorEventWriter(w http.ResponseWriter, msg string) {
	_, _ = w.Write(chatErrorEventChunk(msg))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// jsonMustMarshalString 序列化为 JSON 字符串，失败返回空对象字符串。
func jsonMustMarshalString(v any) string {
	b, err := sonic.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// extractChatUpstreamError 从非流式错误响应提取可读消息。
func extractChatUpstreamError(statusCode int, body []byte) string {
	if len(body) > 0 {
		var obj map[string]any
		if err := sonic.Unmarshal(body, &obj); err == nil {
			if msg := extractTestAPIErrorMessage(obj); msg != "" {
				return msg
			}
		}
		if snippet := strings.TrimSpace(string(body)); len(snippet) > 0 && len(snippet) <= 300 {
			return snippet
		}
	}
	return fmt.Sprintf("上游返回错误 HTTP %d", statusCode)
}
