package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	responsesWebsocketRequestCreate      = "response.create"
	responsesWebsocketRequestAppend      = "response.append"
	responsesWebsocketIdleTimeout        = 5 * time.Minute
	responsesWebsocketPingInterval       = 2 * time.Minute
	responsesWebsocketWriteTimeout       = 30 * time.Second
	responsesWebsocketRetryCode          = "upstream_unavailable"
	responsesWebsocketRetryMessage       = "upstream channel failed before response output; retry the request"
	responsesWebsocketInterruptedCode    = "upstream_stream_interrupted"
	responsesWebsocketInterruptedMessage = "upstream response was interrupted; reconnect and replay the full conversation state"
)

// responsesWebsocketUpgradePaths lists every downstream path that terminates a
// Responses WebSocket. /backend-api/codex/responses is the Codex CLI direct
// route alias (chatgpt_base_url compatible), mirrored from CLIProxyAPI
// (internal/api/server.go's codexDirect route group). /v1/codex/responses is
// the omp/pi Codex flavor path (baseUrl + "/codex/responses").
var responsesWebsocketUpgradePaths = []string{"/v1/responses", "/v1/codex/responses", "/backend-api/codex/responses"}

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// The endpoint sits behind Bearer token auth (auth_service.go's
	// isResponsesWebsocketUpgradeRequest branch runs after token validation),
	// and a browser cross-origin WebSocket does not carry an Authorization
	// header automatically, so Origin adds no CSRF protection here. Matches
	// CLIProxyAPI, which does not check Origin on this endpoint either.
	CheckOrigin: func(*http.Request) bool { return true },
}

func isResponsesWebsocketUpgradeRequest(r *http.Request) bool {
	return r != nil && r.Method == http.MethodGet &&
		isResponsesWebsocketPath(r.URL.Path) &&
		websocket.IsWebSocketUpgrade(r)
}

func isResponsesWebsocketPath(path string) bool {
	return slices.Contains(responsesWebsocketUpgradePaths, path)
}

// responsesWebsocketTimeouts resolves the idle read deadline and ping
// interval for a connection. The two Server fields exist only so tests can
// shorten the clock to exercise the keepalive path; production never sets
// them, so this always falls back to the package constants below.
func (s *Server) responsesWebsocketTimeouts() (idle, ping time.Duration) {
	idle = s.responsesWebsocketIdleTimeoutOverride
	if idle <= 0 {
		idle = responsesWebsocketIdleTimeout
	}
	ping = s.responsesWebsocketPingIntervalOverride
	if ping <= 0 {
		ping = responsesWebsocketPingInterval
	}
	return idle, ping
}

// HandleResponsesWebsocket terminates the downstream Responses WebSocket.
// An explicit Session-Id binds conversation state to the authenticated subject,
// so a downstream reconnect does not destroy transcript or upstream affinity.
func (s *Server) HandleResponsesWebsocket(c *gin.Context) {
	if !websocket.IsWebSocketUpgrade(c.Request) {
		c.JSON(http.StatusUpgradeRequired, gin.H{"error": "websocket upgrade required"})
		return
	}
	tokenHash, _ := c.Get("token_hash")
	tokenHashString, _ := tokenHash.(string)
	if s.authService == nil || !s.authService.IsTokenActive(tokenHashString) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired authorization"})
		return
	}
	releaseConnection, connectionLimit := s.responsesWebsocketConnections.acquire(tokenHashString)
	if connectionLimit != nil {
		setting := responsesWebsocketMaxConnectionsSetting
		message := fmt.Sprintf(
			"Responses WebSocket process-wide downstream connection limit reached (current: %d, limit: %d). Close an existing connection or increase %s, then try again.",
			connectionLimit.active,
			connectionLimit.limit,
			setting,
		)
		if connectionLimit.scope == "token" {
			setting = responsesWebsocketMaxConnectionsPerTokenSetting
			message = fmt.Sprintf(
				"Responses WebSocket downstream connection limit for this API token reached (current: %d, limit: %d). Close an existing connection for this token or increase %s, then try again.",
				connectionLimit.active,
				connectionLimit.limit,
				setting,
			)
		}
		c.JSON(http.StatusTooManyRequests, gin.H{"error": responsesWebsocketErrorBody{
			Message:      message,
			Type:         "rate_limit_error",
			Code:         "responses_websocket_connection_limit_exceeded",
			Setting:      setting,
			SettingValue: int64(connectionLimit.limit),
			Current:      int64(connectionLimit.active),
			Unit:         "connections",
		}})
		return
	}
	defer releaseConnection()

	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	connectionCtx, cancelConnection := context.WithCancel(context.Background())
	defer cancelConnection()
	stopShutdownClose := context.AfterFunc(s.baseCtx, func() {
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"),
			time.Now().Add(responsesWebsocketWriteTimeout),
		)
		cancelConnection()
		_ = conn.Close()
	})
	defer stopShutdownClose()
	idleTimeout, pingInterval := s.responsesWebsocketTimeouts()
	conn.SetReadLimit(s.bodyLimits.maxForPath("/v1/responses"))
	_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(idleTimeout))
	})
	startResponsesWebsocketPingLoop(connectionCtx, cancelConnection, conn, pingInterval, func() bool {
		return s.authService != nil && s.authService.IsTokenActive(tokenHashString)
	})
	messages := readResponsesWebsocketMessages(connectionCtx, cancelConnection, conn, idleTimeout)
	var executionSession *responsesExecutionSession
	var releaseExecutionSession func()
	defer func() {
		if releaseExecutionSession != nil {
			releaseExecutionSession()
		}
	}()
	for {
		var message responsesWebsocketInboundMessage
		select {
		case <-connectionCtx.Done():
			return
		case message = <-messages:
		}
		if s.authService == nil || !s.authService.IsTokenActive(tokenHashString) {
			_ = closeResponsesWebsocketPolicyViolation(conn, "authorization expired or revoked")
			return
		}
		if message.messageType != websocket.TextMessage {
			if errWrite := writeResponsesWebsocketError(conn, "unsupported_frame", "only text websocket messages are supported"); errWrite != nil {
				return
			}
			continue
		}

		eventType := strings.TrimSpace(gjson.GetBytes(message.payload, "type").String())
		switch eventType {
		case responsesWebsocketRequestCreate, responsesWebsocketRequestAppend:
			if executionSession == nil {
				sessionID := responsesExecutionSessionID(c.Request.Header)
				var errSession error
				executionSession, releaseExecutionSession, errSession = s.responsesExecutionSessions.acquire(tokenHashString, sessionID)
				if errSession != nil {
					if errWrite := writeResponsesWebsocketRateLimit(conn, errSession); errWrite != nil {
						return
					}
					continue
				}
			}
			if errAcquire := executionSession.acquireTurn(connectionCtx); errAcquire != nil {
				return
			}
			if errAdmit := s.responsesExecutionSessions.admitTurn(executionSession); errAdmit != nil {
				executionSession.releaseTurn()
				if errWrite := writeResponsesWebsocketRateLimit(conn, errAdmit); errWrite != nil {
					return
				}
				continue
			}
			emptySession := len(executionSession.transcript.lastRequest) == 0
			allowLocalPrewarm := emptySession
			requestBody, nativeRequestBody, errNormalize := executionSession.transcript.normalizeRequests(message.payload)
			if errNormalize != nil {
				executionSession.releaseTurn()
				if errors.Is(errNormalize, errResponsesWebsocketPreviousResponseNotFound) {
					s.responsesExecutionSessions.recordPreviousResponseMiss(
						executionSession,
						gjson.GetBytes(message.payload, "previous_response_id").String(),
						emptySession,
					)
					if errWrite := writeResponsesWebsocketPreviousResponseNotFound(
						conn,
						s.responsesExecutionSessions.sessionTTL(),
					); errWrite != nil {
						return
					}
					continue
				}
				if errWrite := writeResponsesWebsocketError(conn, "invalid_request", errNormalize.Error()); errWrite != nil {
					return
				}
				continue
			}
			turnResult, errTurn := s.executeResponsesWebsocketTurn(
				connectionCtx, c, conn, requestBody, nativeRequestBody, executionSession, allowLocalPrewarm,
			)
			if len(turnResult.committedRequest) > 0 {
				requestBody = turnResult.committedRequest
			}
			if errTurn != nil {
				if turnResult.interrupted {
					s.responsesExecutionSessions.commit(executionSession, requestBody, turnResult)
				}
				var retryErr *responsesWebsocketClientRetryError
				clientRetry := errors.As(errTurn, &retryErr)
				if clientRetry {
					executionSession.transcript.requireReplacementReplay()
				}
				executionSession.releaseTurn()
				if clientRetry {
					if errWrite := writeResponsesWebsocketClientRetryError(conn, retryErr); errWrite != nil {
						return
					}
					_ = closeResponsesWebsocketForClientRetry(conn)
					return
				}
				var terminalErr *responsesWebsocketTerminalError
				if errors.As(errTurn, &terminalErr) {
					if terminalErr.forwarded {
						continue
					}
					if errWrite := writeResponsesWebsocketPayload(conn, terminalErr.payload); errWrite != nil {
						return
					}
					continue
				}
				if errWrite := writeResponsesWebsocketError(conn, "upstream_error", errTurn.Error()); errWrite != nil {
					return
				}
				continue
			}
			s.responsesExecutionSessions.commit(executionSession, requestBody, turnResult)
			executionSession.releaseTurn()
		default:
			if errWrite := writeResponsesWebsocketError(conn, "unsupported_event", "unsupported websocket request type"); errWrite != nil {
				return
			}
		}
	}
}

type responsesWebsocketInboundMessage struct {
	messageType int
	payload     []byte
}

func readResponsesWebsocketMessages(
	ctx context.Context,
	cancel context.CancelFunc,
	conn *websocket.Conn,
	idleTimeout time.Duration,
) <-chan responsesWebsocketInboundMessage {
	messages := make(chan responsesWebsocketInboundMessage)
	go func() {
		defer cancel()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
			select {
			case messages <- responsesWebsocketInboundMessage{messageType: messageType, payload: payload}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return messages
}

func startResponsesWebsocketPingLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	conn *websocket.Conn,
	interval time.Duration,
	authorized func() bool,
) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if authorized != nil && !authorized() {
					_ = closeResponsesWebsocketPolicyViolation(conn, "authorization expired or revoked")
					cancel()
					_ = conn.Close()
					return
				}
				if err := conn.WriteControl(
					websocket.PingMessage, nil, time.Now().Add(responsesWebsocketWriteTimeout),
				); err != nil {
					cancel()
					_ = conn.Close()
					return
				}
			}
		}
	}()
}

func closeResponsesWebsocketPolicyViolation(conn *websocket.Conn, reason string) error {
	if conn == nil {
		return errors.New("websocket connection is nil")
	}
	return conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason),
		time.Now().Add(responsesWebsocketWriteTimeout),
	)
}

type responsesWebsocketTurnResult struct {
	completedOutput     []byte
	completedResponseID string
	pendingToolCallIDs  []string
	interrupted         bool
	committedRequest    []byte
}

type responsesWebsocketTerminalError struct {
	payload   []byte
	forwarded bool
}

func (e *responsesWebsocketTerminalError) Error() string {
	if e == nil {
		return "responses websocket terminal error"
	}
	return safeBodyToString(e.payload)
}

type responsesWebsocketClientRetryError struct {
	status  int
	code    string
	message string
}

func (e *responsesWebsocketClientRetryError) Error() string {
	if e != nil && e.message != "" {
		return e.message
	}
	return responsesWebsocketRetryMessage
}

func (e *responsesWebsocketClientRetryError) responseCode() string {
	if e != nil && e.code != "" {
		return e.code
	}
	return responsesWebsocketRetryCode
}

func (e *responsesWebsocketClientRetryError) responseStatus() int {
	if e != nil && e.status != 0 {
		return e.status
	}
	return http.StatusBadGateway
}

func (s *Server) executeResponsesWebsocketTurn(
	ctx context.Context,
	c *gin.Context,
	conn *websocket.Conn,
	requestBody []byte,
	nativeRequestBody []byte,
	executionSession *responsesExecutionSession,
	allowLocalPrewarm bool,
) (responsesWebsocketTurnResult, error) {
	nativeCodexWS := executionSession.upstream
	requestedModel := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	if requestedModel == "" {
		return responsesWebsocketTurnResult{}, errors.New("missing model in normalized websocket request")
	}
	clientModel := model.RoutingModelName(requestedModel)
	// 多模态回退：按完整 transcript 检测。一旦某轮带图，历史里会一直留着这张图，
	// 用完整体检测才能让后续每一轮的判定保持稳定。改写必须在 Token 白名单
	// （:431 modelName）与候选选择之前，与 HTTP 入口同契约。
	if fallback := s.multimodalFallbackModel(requestedModel, requestHasNonTextContent(protocol.Codex, requestBody)); fallback != "" {
		requestedModel = fallback
	}
	// 完整 transcript 与增量回合都是 Codex 协议原始体，后缀改写必须同时落到两者上，
	// 否则重放和增量提交会带着不同的思考参数。
	requestBody = applyThinkingSuffix(requestBody, protocol.Codex, requestedModel)
	nativeRequestBody = applyThinkingSuffix(nativeRequestBody, protocol.Codex, requestedModel)
	thinkingEffort := thinkingEffortFromRequest(requestedModel, requestBody)
	modelName := model.RoutingModelName(requestedModel)

	release, err := s.acquireConcurrencySlotForContext(ctx)
	if err != nil {
		return responsesWebsocketTurnResult{}, err
	}
	defer release()

	tokenHash, _ := c.Get("token_hash")
	tokenHashString, _ := tokenHash.(string)
	releaseTokenSlot, activeTokenRequests, maxTokenRequests, acquired := s.authService.acquireTokenConcurrencySlot(tokenHashString)
	if !acquired {
		return responsesWebsocketTurnResult{}, fmt.Errorf(
			"token concurrency limit exceeded: %d active of %d limit",
			activeTokenRequests,
			maxTokenRequests,
		)
	}
	defer releaseTokenSlot()

	if tokenHashString != "" && !s.authService.IsModelAllowed(tokenHashString, modelName) {
		return responsesWebsocketTurnResult{}, fmt.Errorf("model %q is not allowed for this token", modelName)
	}
	if tokenHashString != "" {
		_, _, exceeded := s.authService.IsCostLimitExceeded(tokenHashString)
		if exceeded {
			return responsesWebsocketTurnResult{}, errors.New("token cost limit exceeded")
		}
	}

	candidates, err := s.selectCandidatesByModelAndClientProtocol(ctx, modelName, string(protocol.Codex))
	if err != nil {
		return responsesWebsocketTurnResult{}, fmt.Errorf("select upstream candidates: %w", err)
	}
	if tokenHashString != "" {
		if filtered, restricted := s.authService.FilterAllowedChannels(tokenHashString, candidates); restricted {
			candidates = filtered
		}
	}
	if len(candidates) == 0 {
		return responsesWebsocketTurnResult{}, errors.New("no available upstream")
	}
	if channelID, ok := executionSession.routeChannelSnapshot(); ok {
		candidates = prioritizePinnedCodexChannel(candidates, channelID)
	}
	if allowLocalPrewarm && responsesWebsocketGenerateDisabled(requestBody) &&
		!isNativeCodexWebsocketCandidate(candidates[0]) {
		return writeResponsesWebsocketSyntheticPrewarm(conn, requestBody)
	}

	startTime := time.Now()
	tokenID, _ := c.Get("token_id")
	tokenIDInt64, _ := tokenID.(int64)
	header := responsesWebsocketUpstreamHeaders(c.Request.Header)
	header.Set("Content-Type", "application/json")
	reqCtx := &proxyRequestContext{
		clientModel:                clientModel,
		originalModel:              modelName,
		requestedModel:             requestedModel,
		clientProtocol:             protocol.Codex,
		codexClient:                isCodexMultiAgentClient(codexMultiAgentUserAgent(c.Request.Header)),
		requestMethod:              http.MethodPost,
		requestPath:                "/v1/responses",
		rawQuery:                   c.Request.URL.RawQuery,
		body:                       requestBody,
		translatedBody:             requestBody,
		header:                     header,
		isStreaming:                true,
		tokenHash:                  tokenHashString,
		tokenID:                    tokenIDInt64,
		clientIP:                   c.ClientIP(),
		startTime:                  startTime,
		thinkingEffort:             thinkingEffort,
		routingSession:             executionSession,
		nativeCodexWS:              nativeCodexWS,
		nativeCodexBody:            bytes.Clone(nativeRequestBody),
		codexMultiAgentV2Optimized: executionSession.codexMultiAgentV2StateSnapshot(),
	}
	ctx = withCodexMultiAgentV2RequestContext(ctx, reqCtx)
	reqCtx.observer = &ForwardObserver{
		OnBytesRead: func(n int64) {
			s.activeRequests.AddBytes(reqCtx.activeReqID, n)
		},
		OnFirstByteRead: func(firstByteTime time.Duration) {
			s.activeRequests.SetClientFirstByteTime(reqCtx.activeReqID, firstByteTime)
		},
		OnUpstreamWebsocket: func(upstreamWebsocket bool) {
			s.activeRequests.SetUpstreamWebsocket(reqCtx.activeReqID, upstreamWebsocket)
		},
		OnDebugCapture: func(dc *debugCapture) {
			s.activeRequests.SetDebugCapture(reqCtx.activeReqID, dc)
		},
	}
	defer func() {
		if reqCtx.activeReqID > 0 {
			s.activeRequests.Remove(reqCtx.activeReqID)
		}
	}()

	bridgeWriter := newResponsesWebsocketBridgeWriter(conn, s.bodyLimits.maxForPath("/v1/responses"))
	clientReplay := false
	stopBeforeNativeWebsocket := func(current, next *model.Config, result *proxyResult) bool {
		if isNativeCodexWebsocketCandidate(current) || !isNativeCodexWebsocketCandidate(next) {
			return false
		}
		if !isResponsesWebsocketClientRetryAction(result.nextAction) {
			return false
		}
		clientReplay = true
		return true
	}
	lastResult, succeeded := s.runProxyAttemptLoopWithFailureBoundary(
		ctx, candidates, reqCtx, bridgeWriter, stopBeforeNativeWebsocket,
	)
	committedRequest := bytes.Clone(reqCtx.quotaOverdraftTranscript)
	s.updateCodexMultiAgentV2SessionState(executionSession, reqCtx)
	if bridgeWriter.closedForMessageTooBig {
		return responsesWebsocketTurnResult{}, &responsesWebsocketTerminalError{forwarded: true}
	}
	if clientReplay && !succeeded {
		return responsesWebsocketTurnResult{}, &responsesWebsocketClientRetryError{}
	}
	if succeeded {
		if !bridgeWriter.completed {
			if bridgeWriter.failed && isResponsesWebsocketTerminalPayload(bridgeWriter.failedPayload) {
				return responsesWebsocketTurnResult{}, &responsesWebsocketTerminalError{
					payload: bytes.Clone(bridgeWriter.failedPayload), forwarded: true,
				}
			}
			// 裸 error 事件（server_is_overloaded 等）终结不了回合，和流被截断是同一种
			// 故障：回合没有终结事件，只能中断连接让客户端完整重放。
			message := responsesWebsocketInterruptedMessage
			if bridgeWriter.failed {
				message = responsesWebsocketUpstreamErrorMessage(bridgeWriter.failedPayload)
			}
			interruptedOutput := bridgeWriter.collectedOutput()
			pendingToolCallIDs := responsesWebsocketPendingToolCallIDs(interruptedOutput)
			turnResult := responsesWebsocketTurnResult{}
			if len(pendingToolCallIDs) > 0 {
				// Preserve completed tool calls in the execution transcript so the
				// client's matching tool output is not orphaned on reconnect.
				turnResult = responsesWebsocketTurnResult{
					completedOutput:    interruptedOutput,
					pendingToolCallIDs: pendingToolCallIDs,
					interrupted:        true,
					committedRequest:   committedRequest,
				}
			}
			return turnResult, &responsesWebsocketClientRetryError{
				code:    responsesWebsocketInterruptedCode,
				message: message,
			}
		}
		return responsesWebsocketTurnResult{
			completedOutput:     bytes.Clone(bridgeWriter.completedOutput),
			completedResponseID: bridgeWriter.completedResponseID,
			pendingToolCallIDs:  responsesWebsocketPendingToolCallIDs(bridgeWriter.completedOutput),
			committedRequest:    committedRequest,
		}, nil
	}
	originalStatus := determineFinalClientStatus(lastResult)
	status := s.clientFacingFinalStatus(reqCtx.codexClient, lastResult)
	if originalStatus == http.StatusTooManyRequests && status == http.StatusServiceUnavailable {
		return responsesWebsocketTurnResult{}, &responsesWebsocketClientRetryError{
			status:  http.StatusServiceUnavailable,
			code:    "upstream_rate_limited",
			message: "all upstream channels were rate limited; reconnect and retry the request",
		}
	}
	if lastResult != nil && status == http.StatusRequestEntityTooLarge &&
		isResponsesWebsocketMessageTooBigPayload(lastResult.body) {
		if errClose := conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "upstream websocket message too big"),
			time.Now().Add(responsesWebsocketWriteTimeout),
		); errClose != nil {
			return responsesWebsocketTurnResult{}, errClose
		}
		return responsesWebsocketTurnResult{}, &responsesWebsocketTerminalError{forwarded: true}
	}
	if lastResult != nil && isResponsesWebsocketFailurePayload(lastResult.body) {
		if isResponsesWebsocketTerminalPayload(lastResult.body) {
			return responsesWebsocketTurnResult{}, &responsesWebsocketTerminalError{payload: bytes.Clone(lastResult.body)}
		}
		// 候选全部失败后剩下的裸 error 事件同样终结不了回合，转发给客户端只会让它
		// 一直等下去，必须按中断处理。
		return responsesWebsocketTurnResult{}, &responsesWebsocketClientRetryError{
			code:    responsesWebsocketInterruptedCode,
			message: responsesWebsocketUpstreamErrorMessage(lastResult.body),
		}
	}
	if lastResult != nil && len(lastResult.body) > 0 {
		return responsesWebsocketTurnResult{}, fmt.Errorf("upstream status %d: %s", status, safeBodyToString(lastResult.body))
	}
	return responsesWebsocketTurnResult{}, fmt.Errorf("upstream status %d", status)
}

func responsesWebsocketGenerateDisabled(payload []byte) bool {
	generate := gjson.GetBytes(payload, "generate")
	return generate.Exists() && !generate.Bool()
}

func isNativeCodexWebsocketCandidate(candidate *model.Config) bool {
	return candidate != nil && candidate.Websockets && !candidate.UsesXAIOAuth() && !candidate.UsesZedOAuth() &&
		configCanUseUpstreamProtocol(candidate, protocol.Codex)
}

func isResponsesWebsocketClientRetryAction(action cooldown.Action) bool {
	switch action {
	case cooldown.ActionRetryKey, cooldown.ActionRetryModel, cooldown.ActionRetryChannel:
		return true
	default:
		return false
	}
}

func writeResponsesWebsocketSyntheticPrewarm(
	conn *websocket.Conn,
	request []byte,
) (responsesWebsocketTurnResult, error) {
	responseID := "resp_prewarm_" + util.NewUUIDv4()
	createdAt := time.Now().Unix()
	modelName := strings.TrimSpace(gjson.GetBytes(request, "model").String())
	response := map[string]any{
		"id": responseID, "object": "response", "created_at": createdAt,
		"status": "in_progress", "background": false, "error": nil,
		"model": modelName, "output": []any{},
	}
	created, err := json.Marshal(map[string]any{
		"type": "response.created", "sequence_number": 0, "response": response,
	})
	if err != nil {
		return responsesWebsocketTurnResult{}, err
	}
	if err = writeResponsesWebsocketPayload(conn, created); err != nil {
		return responsesWebsocketTurnResult{}, err
	}
	response["status"] = "completed"
	response["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	completed, err := json.Marshal(map[string]any{
		"type": "response.completed", "sequence_number": 1, "response": response,
	})
	if err != nil {
		return responsesWebsocketTurnResult{}, err
	}
	if err = writeResponsesWebsocketPayload(conn, completed); err != nil {
		return responsesWebsocketTurnResult{}, err
	}
	return responsesWebsocketTurnResult{
		completedOutput: []byte("[]"), completedResponseID: responseID,
	}, nil
}

func isResponsesWebsocketFailurePayload(payload []byte) bool {
	if !json.Valid(payload) {
		return false
	}
	switch strings.TrimSpace(gjson.GetBytes(payload, "type").String()) {
	case "error", "response.failed":
		return true
	default:
		return false
	}
}

// isResponsesWebsocketTerminalPayload 判断 payload 能否终结客户端的一个回合。
// response.failed 是协议定义的终结事件；裸 error 事件只有携带上游 HTTP 状态
// （usage_limit_reached 这类明确裁决，判据同 websocketErrorStatusAndHeaders）时
// 才是客户端认得出的最终答案。不带状态的裸 error（server_is_overloaded 等）终结
// 不了回合，客户端收下之后仍在等终结事件，只有连接断开才会重放当前回合。
func isResponsesWebsocketTerminalPayload(payload []byte) bool {
	switch strings.TrimSpace(gjson.GetBytes(payload, "type").String()) {
	case "response.failed":
		return true
	case "error":
		status, _ := websocketErrorStatusAndHeaders(payload)
		return status != 0
	default:
		return false
	}
}

// responsesWebsocketUpstreamErrorMessage 把上游错误事件的原因带进中断提示，
// 客户端重放前才能看到真实故障，而不是只有一句通用的中断说明。
func responsesWebsocketUpstreamErrorMessage(payload []byte) string {
	message := strings.TrimSpace(gjson.GetBytes(payload, "error.message").String())
	if message == "" {
		return responsesWebsocketInterruptedMessage
	}
	return message + "; " + responsesWebsocketInterruptedMessage
}

func isResponsesWebsocketMessageTooBigPayload(payload []byte) bool {
	return strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()) == "message_too_big"
}

func prioritizePinnedCodexChannel(candidates []*model.Config, channelID int64) []*model.Config {
	for index, candidate := range candidates {
		if candidate == nil || candidate.ID != channelID || index == 0 {
			continue
		}
		ordered := make([]*model.Config, 0, len(candidates))
		ordered = append(ordered, candidate)
		ordered = append(ordered, candidates[:index]...)
		ordered = append(ordered, candidates[index+1:]...)
		return ordered
	}
	return candidates
}

func responsesWebsocketUpstreamHeaders(source http.Header) http.Header {
	header := source.Clone()
	for key := range header {
		if strings.EqualFold(key, "Origin") || strings.HasPrefix(strings.ToLower(key), "sec-websocket-") {
			header.Del(key)
		}
	}
	header.Del("Connection")
	header.Del("Upgrade")
	return header
}

type responsesWebsocketBridgeWriter struct {
	conn                   *websocket.Conn
	header                 http.Header
	status                 int
	pending                bytes.Buffer
	completed              bool
	completedOutput        []byte
	completedResponseID    string
	failed                 bool
	failedPayload          []byte
	closedForMessageTooBig bool
	outputItemsByIndex     map[int64][]byte
	outputItemsUnindexed   [][]byte
	outputItemsBytes       int64
	maxBodyBytes           int64
}

func newResponsesWebsocketBridgeWriter(conn *websocket.Conn, maxBodyBytes int64) *responsesWebsocketBridgeWriter {
	if maxBodyBytes <= 0 {
		maxBodyBytes = normalizeMaxBodyBytes(maxBodyBytes)
	}
	return &responsesWebsocketBridgeWriter{
		conn:               conn,
		header:             make(http.Header),
		outputItemsByIndex: make(map[int64][]byte),
		maxBodyBytes:       maxBodyBytes,
	}
}

func (w *responsesWebsocketBridgeWriter) Header() http.Header {
	return w.header
}

func (w *responsesWebsocketBridgeWriter) WriteHeader(statusCode int) {
	w.status = statusCode
}

func (w *responsesWebsocketBridgeWriter) Write(data []byte) (int, error) {
	if w == nil || w.conn == nil {
		return 0, errors.New("websocket connection is nil")
	}
	originalLen := len(data)
	_, _ = w.pending.Write(data)
	if int64(w.pending.Len()) > w.maxBodyBytes {
		return 0, errors.New("upstream SSE event exceeds websocket body limit")
	}
	for {
		rawEvent, ok := nextSSEEvent(&w.pending)
		if !ok {
			break
		}
		payload := sseEventData(rawEvent)
		if len(payload) == 0 || bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			continue
		}
		if !json.Valid(payload) {
			return 0, errors.New("invalid JSON in upstream SSE event")
		}
		eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
		if err := w.collectOutputItem(eventType, payload); err != nil {
			return 0, err
		}
		if eventType == "response.completed" || eventType == "response.done" || eventType == "response.incomplete" {
			w.completed = true
			output := gjson.GetBytes(payload, "response.output")
			if output.Exists() && output.IsArray() && len(output.Array()) > 0 {
				w.completedOutput = w.reconcileCompletedOutput(output)
				if !bytes.Equal(w.completedOutput, []byte(output.Raw)) {
					if reconciled, errSet := sjson.SetRawBytes(payload, "response.output", w.completedOutput); errSet == nil {
						payload = reconciled
					}
				}
			} else {
				w.completedOutput = w.collectedOutput()
			}
			w.completedResponseID = strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
		}
		if isResponsesWebsocketMessageTooBigPayload(payload) {
			if err := w.conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "upstream websocket message too big"),
				time.Now().Add(responsesWebsocketWriteTimeout),
			); err != nil {
				return 0, err
			}
			w.failed = true
			w.failedPayload = bytes.Clone(payload)
			w.closedForMessageTooBig = true
			continue
		}
		if eventType == "error" || eventType == "response.failed" {
			w.failed = true
			w.failedPayload = bytes.Clone(payload)
		}
		// 终结不了回合的裸 error 事件不转发：客户端收下之后只会继续等终结事件，
		// 而网关这时还可能切下一个候选，泄漏出去的错误会污染重试出来的回合。
		// 回合层把它翻译成 upstream_stream_interrupted + close 1011。
		if eventType == "error" && !isResponsesWebsocketTerminalPayload(payload) {
			continue
		}
		if err := w.conn.SetWriteDeadline(time.Now().Add(responsesWebsocketWriteTimeout)); err != nil {
			return 0, err
		}
		if err := w.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			return 0, err
		}
	}
	return originalLen, nil
}

func (w *responsesWebsocketBridgeWriter) Flush() {}

func (w *responsesWebsocketBridgeWriter) SetWriteDeadline(deadline time.Time) error {
	if w == nil || w.conn == nil {
		return errors.New("websocket connection is nil")
	}
	return w.conn.SetWriteDeadline(deadline)
}

// collectOutputItem 累积单轮 output item 快照供 transcript 回放。pending 的
// 单事件上限挡不住海量小事件：每个事件解析完就被清空，快照却逐条 clone
// 留在内存里，所以这里必须再压一道累计字节上限（与请求侧 transcript 同限）。
func (w *responsesWebsocketBridgeWriter) collectOutputItem(eventType string, payload []byte) error {
	if eventType != "response.output_item.done" {
		return nil
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || !item.IsObject() {
		return nil
	}
	itemBytes := bytes.Clone([]byte(item.Raw))
	index := gjson.GetBytes(payload, "output_index")
	if index.Exists() {
		key := index.Int()
		w.outputItemsBytes += int64(len(itemBytes)) - int64(len(w.outputItemsByIndex[key]))
		w.outputItemsByIndex[key] = itemBytes
	} else {
		w.outputItemsBytes += int64(len(itemBytes))
		w.outputItemsUnindexed = append(w.outputItemsUnindexed, itemBytes)
	}
	if w.outputItemsBytes > w.maxBodyBytes {
		return errors.New("upstream response output exceeds websocket transcript limit")
	}
	return nil
}

func (w *responsesWebsocketBridgeWriter) collectedOutput() []byte {
	items := make([]json.RawMessage, 0, len(w.outputItemsByIndex)+len(w.outputItemsUnindexed))
	appendItem := func(raw []byte) {
		item := gjson.ParseBytes(raw)
		if isResponsesWebsocketToolCallType(item.Get("type").String()) &&
			!isCompleteResponsesWebsocketToolCall(item) {
			return
		}
		items = append(items, bytes.Clone(raw))
	}
	indices := make([]int, 0, len(w.outputItemsByIndex))
	for index := range w.outputItemsByIndex {
		indices = append(indices, int(index))
	}
	sort.Ints(indices)
	for _, index := range indices {
		appendItem(w.outputItemsByIndex[int64(index)])
	}
	for _, item := range w.outputItemsUnindexed {
		appendItem(item)
	}
	if len(items) == 0 {
		return []byte("[]")
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return []byte("[]")
	}
	return encoded
}

func (w *responsesWebsocketBridgeWriter) reconcileCompletedOutput(output gjson.Result) []byte {
	collected := make(map[string]json.RawMessage)
	record := func(raw []byte) {
		item := gjson.ParseBytes(raw)
		if !isCompleteResponsesWebsocketToolCall(item) {
			return
		}
		collected[strings.TrimSpace(item.Get("call_id").String())] = bytes.Clone(raw)
	}
	for _, raw := range w.outputItemsByIndex {
		record(raw)
	}
	for _, raw := range w.outputItemsUnindexed {
		record(raw)
	}
	if len(collected) == 0 {
		return bytes.Clone([]byte(output.Raw))
	}

	items := output.Array()
	reconciled := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		raw := json.RawMessage(item.Raw)
		if isResponsesWebsocketToolCallType(item.Get("type").String()) {
			callID := strings.TrimSpace(item.Get("call_id").String())
			if complete, ok := collected[callID]; ok && !bytes.Equal(raw, complete) {
				raw = complete
				changed = true
			}
		}
		reconciled = append(reconciled, raw)
	}
	if !changed {
		return bytes.Clone([]byte(output.Raw))
	}
	encoded, err := json.Marshal(reconciled)
	if err != nil {
		return bytes.Clone([]byte(output.Raw))
	}
	return encoded
}

func responsesWebsocketPendingToolCallIDs(output []byte) []string {
	result := gjson.ParseBytes(output)
	if !result.IsArray() {
		return nil
	}
	seen := make(map[string]struct{})
	var callIDs []string
	for _, item := range result.Array() {
		itemType := strings.TrimSpace(item.Get("type").String())
		if !isResponsesWebsocketToolCallType(itemType) || !isCompleteResponsesWebsocketToolCall(item) {
			continue
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			continue
		}
		if _, ok := seen[callID]; ok {
			continue
		}
		seen[callID] = struct{}{}
		callIDs = append(callIDs, callID)
	}
	return callIDs
}

func isResponsesWebsocketToolCallType(itemType string) bool {
	itemType = strings.TrimSpace(itemType)
	return itemType == "function_call" || itemType == "custom_tool_call"
}

// isResponsesWebsocketToolCallOutputType is the output-side counterpart of
// isResponsesWebsocketToolCallType.
func isResponsesWebsocketToolCallOutputType(itemType string) bool {
	itemType = strings.TrimSpace(itemType)
	return itemType == "function_call_output" || itemType == "custom_tool_call_output"
}

func isCompleteResponsesWebsocketToolCall(item gjson.Result) bool {
	if !item.Exists() || !item.IsObject() {
		return false
	}
	callID := item.Get("call_id")
	name := item.Get("name")
	if callID.Type != gjson.String || strings.TrimSpace(callID.String()) == "" ||
		name.Type != gjson.String || strings.TrimSpace(name.String()) == "" {
		return false
	}
	switch strings.TrimSpace(item.Get("type").String()) {
	case "function_call":
		return item.Get("arguments").Type == gjson.String
	case "custom_tool_call":
		return item.Get("input").Type == gjson.String
	default:
		return false
	}
}

func nextSSEEvent(buffer *bytes.Buffer) ([]byte, bool) {
	if buffer == nil || buffer.Len() == 0 {
		return nil, false
	}
	data := buffer.Bytes()
	end := bytes.Index(data, []byte("\n\n"))
	delimiterLen := 2
	if crlfEnd := bytes.Index(data, []byte("\r\n\r\n")); crlfEnd >= 0 && (end < 0 || crlfEnd < end) {
		end = crlfEnd
		delimiterLen = 4
	}
	if end < 0 {
		return nil, false
	}
	raw := append([]byte(nil), data[:end+delimiterLen]...)
	buffer.Next(end + delimiterLen)
	return raw, true
}

func sseEventData(rawEvent []byte) []byte {
	lines := bytes.Split(rawEvent, []byte("\n"))
	var dataLines [][]byte
	for _, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimPrefix(line, []byte("data:"))
		data = bytes.TrimPrefix(data, []byte(" "))
		dataLines = append(dataLines, data)
	}
	return bytes.Join(dataLines, []byte("\n"))
}

func writeResponsesWebsocketError(conn *websocket.Conn, code string, message string) error {
	return writeResponsesWebsocketErrorPayload(conn, http.StatusBadRequest, responsesWebsocketErrorBody{
		Type: "invalid_request_error", Code: code, Message: message,
	})
}

func writeResponsesWebsocketRateLimit(conn *websocket.Conn, limitErr error) error {
	body := responsesWebsocketErrorBody{
		Type: "rate_limit_error", Code: "rate_limit", Message: limitErr.Error(),
	}
	var executionLimitErr *responsesExecutionLimitError
	if errors.As(limitErr, &executionLimitErr) {
		body.Setting = executionLimitErr.setting
		body.SettingValue = executionLimitErr.limit
		body.Current = executionLimitErr.current
		body.Unit = executionLimitErr.unit
	}
	return writeResponsesWebsocketErrorPayload(conn, http.StatusTooManyRequests, body)
}

func writeResponsesWebsocketClientRetryError(
	conn *websocket.Conn,
	retryErr *responsesWebsocketClientRetryError,
) error {
	return writeResponsesWebsocketErrorPayload(conn, retryErr.responseStatus(), responsesWebsocketErrorBody{
		Type: "server_error", Code: retryErr.responseCode(), Message: retryErr.Error(),
	})
}

type responsesWebsocketErrorBody struct {
	Message      string `json:"message"`
	Type         string `json:"type"`
	Code         string `json:"code"`
	Param        string `json:"param,omitempty"`
	Setting      string `json:"setting,omitempty"`
	SettingValue int64  `json:"setting_value,omitempty"`
	Current      int64  `json:"current,omitempty"`
	Unit         string `json:"unit,omitempty"`
}

func writeResponsesWebsocketErrorPayload(
	conn *websocket.Conn,
	status int,
	errorBody responsesWebsocketErrorBody,
) error {
	if err := conn.SetWriteDeadline(time.Now().Add(responsesWebsocketWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteJSON(gin.H{
		"type":   "error",
		"status": status,
		"error":  errorBody,
	})
}

func writeResponsesWebsocketPreviousResponseNotFound(conn *websocket.Conn, sessionTTL time.Duration) error {
	minutes := int64(sessionTTL / time.Minute)
	minuteLabel := "minutes"
	if minutes == 1 {
		minuteLabel = "minute"
	}
	return writeResponsesWebsocketErrorPayload(conn, http.StatusBadRequest, responsesWebsocketErrorBody{
		Type:  "invalid_request_error",
		Code:  "previous_response_not_found",
		Param: "previous_response_id",
		Message: fmt.Sprintf(
			"previous_response_id is not available in this execution session. Resend the full conversation input without previous_response_id. Idle sessions are retained for %d %s by %s.",
			minutes,
			minuteLabel,
			responsesWebsocketSessionTTLSetting,
		),
		Setting:      responsesWebsocketSessionTTLSetting,
		SettingValue: minutes,
		Unit:         "minutes",
	})
}

func closeResponsesWebsocketForClientRetry(conn *websocket.Conn) error {
	if conn == nil {
		return errors.New("websocket connection is nil")
	}
	return conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(
			websocket.CloseInternalServerErr,
			"upstream unavailable; reconnect to retry",
		),
		time.Now().Add(responsesWebsocketWriteTimeout),
	)
}

func writeResponsesWebsocketPayload(conn *websocket.Conn, payload []byte) error {
	if conn == nil {
		return errors.New("websocket connection is nil")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(responsesWebsocketWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}
