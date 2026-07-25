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
	responsesWebsocketRequestCreate = "response.create"
	responsesWebsocketRequestAppend = "response.append"
	responsesWebsocketIdleTimeout   = 5 * time.Minute
	responsesWebsocketPingInterval  = 2 * time.Minute
	responsesWebsocketWriteTimeout  = 30 * time.Second
	responsesWebsocketRetryMessage  = "upstream channel failed before response output; retry the request"
)

// responsesWebsocketUpgradePaths lists every downstream path that terminates a
// Responses WebSocket. /backend-api/codex/responses is the Codex CLI direct
// route alias (chatgpt_base_url compatible), mirrored from CLIProxyAPI
// (internal/api/server.go's codexDirect route group).
var responsesWebsocketUpgradePaths = []string{"/v1/responses", "/backend-api/codex/responses"}

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
		slices.Contains(responsesWebsocketUpgradePaths, r.URL.Path) &&
		websocket.IsWebSocketUpgrade(r)
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
// Explicit session hints bind conversation state to the authenticated subject,
// so a downstream reconnect does not destroy transcript or upstream affinity.
func (s *Server) HandleResponsesWebsocket(c *gin.Context) {
	if !websocket.IsWebSocketUpgrade(c.Request) {
		c.JSON(http.StatusUpgradeRequired, gin.H{"error": "websocket upgrade required"})
		return
	}

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
	conn.SetReadLimit(maxProxyBodyBytes("/v1/responses"))
	_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(idleTimeout))
	})
	startResponsesWebsocketPingLoop(connectionCtx, cancelConnection, conn, pingInterval)
	messages := readResponsesWebsocketMessages(connectionCtx, cancelConnection, conn, idleTimeout)
	var executionSession *responsesExecutionSession
	var releaseExecutionSession func()
	defer func() {
		if releaseExecutionSession != nil {
			releaseExecutionSession()
		}
	}()
	tokenHash, _ := c.Get("token_hash")
	tokenHashString, _ := tokenHash.(string)

	for {
		var message responsesWebsocketInboundMessage
		select {
		case <-connectionCtx.Done():
			return
		case message = <-messages:
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
				hint := responsesExecutionSessionHint(c.Request.Header, message.payload)
				var errSession error
				executionSession, releaseExecutionSession, errSession = s.responsesExecutionSessions.acquire(tokenHashString, hint)
				if errSession != nil {
					if errWrite := writeResponsesWebsocketError(conn, "session_capacity", errSession.Error()); errWrite != nil {
						return
					}
					continue
				}
			}
			if errAcquire := executionSession.acquireTurn(connectionCtx); errAcquire != nil {
				return
			}
			allowLocalPrewarm := len(executionSession.transcript.lastRequest) == 0
			requestBody, nativeRequestBody, errNormalize := executionSession.transcript.normalizeRequests(message.payload)
			if errNormalize != nil {
				executionSession.releaseTurn()
				if errors.Is(errNormalize, errResponsesWebsocketPreviousResponseNotFound) {
					if errWrite := writeResponsesWebsocketPreviousResponseNotFound(conn); errWrite != nil {
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
				connectionCtx, c, conn, requestBody, nativeRequestBody, executionSession.upstream, allowLocalPrewarm,
			)
			if errTurn != nil {
				executionSession.releaseTurn()
				var retryErr *responsesWebsocketClientRetryError
				if errors.As(errTurn, &retryErr) {
					if errWrite := writeResponsesWebsocketClientRetryError(conn); errWrite != nil {
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
			executionSession.commit(requestBody, turnResult)
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
) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
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

type responsesWebsocketTurnResult struct {
	completedOutput     []byte
	completedResponseID string
	pendingToolCallIDs  []string
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

type responsesWebsocketClientRetryError struct{}

func (*responsesWebsocketClientRetryError) Error() string {
	return responsesWebsocketRetryMessage
}

func (s *Server) executeResponsesWebsocketTurn(
	ctx context.Context,
	c *gin.Context,
	conn *websocket.Conn,
	requestBody []byte,
	nativeRequestBody []byte,
	nativeCodexWS *codexUpstreamWebsocketSession,
	allowLocalPrewarm bool,
) (responsesWebsocketTurnResult, error) {
	modelName := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	if modelName == "" {
		return responsesWebsocketTurnResult{}, errors.New("missing model in normalized websocket request")
	}

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

	candidates, err := s.selectCandidatesByModelAndType(ctx, modelName, string(protocol.Codex))
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
	if pinnedTarget, ok := nativeCodexWS.targetSnapshot(); ok {
		candidates = prioritizePinnedCodexChannel(candidates, pinnedTarget.channelID)
	}
	if allowLocalPrewarm && responsesWebsocketGenerateDisabled(requestBody) &&
		!isNativeCodexWebsocketCandidate(candidates[0]) {
		return writeResponsesWebsocketSyntheticPrewarm(conn, requestBody)
	}

	startTime := time.Now()
	tokenID, _ := c.Get("token_id")
	tokenIDInt64, _ := tokenID.(int64)
	thinkingEffort := extractThinkingEffortFromJSON(requestBody)
	activeID := s.activeRequests.Register(startTime, modelName, c.ClientIP(), true)
	s.activeRequests.SetThinkingEffort(activeID, thinkingEffort)
	defer s.activeRequests.Remove(activeID)

	header := responsesWebsocketUpstreamHeaders(c.Request.Header)
	header.Set("Content-Type", "application/json")
	reqCtx := &proxyRequestContext{
		originalModel:   modelName,
		clientProtocol:  protocol.Codex,
		requestMethod:   http.MethodPost,
		requestPath:     "/v1/responses",
		rawQuery:        c.Request.URL.RawQuery,
		body:            requestBody,
		translatedBody:  requestBody,
		header:          header,
		isStreaming:     true,
		tokenHash:       tokenHashString,
		tokenID:         tokenIDInt64,
		clientIP:        c.ClientIP(),
		activeReqID:     activeID,
		startTime:       startTime,
		thinkingEffort:  thinkingEffort,
		nativeCodexWS:   nativeCodexWS,
		nativeCodexBody: bytes.Clone(nativeRequestBody),
	}
	reqCtx.observer = &ForwardObserver{
		OnBytesRead: func(n int64) {
			s.activeRequests.AddBytes(activeID, n)
		},
		OnFirstByteRead: func() {
			s.activeRequests.SetClientFirstByteTime(activeID, time.Since(reqCtx.attemptStartTime))
		},
		OnUpstreamWebsocket: func(upstreamWebsocket bool) {
			s.activeRequests.SetUpstreamWebsocket(activeID, upstreamWebsocket)
		},
		OnDebugCapture: func(dc *debugCapture) {
			s.activeRequests.SetDebugCapture(activeID, dc)
		},
	}

	bridgeWriter := newResponsesWebsocketBridgeWriter(conn)
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
	if bridgeWriter.closedForMessageTooBig {
		return responsesWebsocketTurnResult{}, &responsesWebsocketTerminalError{forwarded: true}
	}
	if clientReplay && !succeeded {
		return responsesWebsocketTurnResult{}, &responsesWebsocketClientRetryError{}
	}
	if succeeded {
		if !bridgeWriter.completed {
			if bridgeWriter.failed {
				return responsesWebsocketTurnResult{}, &responsesWebsocketTerminalError{
					payload: bytes.Clone(bridgeWriter.failedPayload), forwarded: true,
				}
			}
			return responsesWebsocketTurnResult{}, errors.New("upstream stream closed before response.completed")
		}
		return responsesWebsocketTurnResult{
			completedOutput:     bytes.Clone(bridgeWriter.completedOutput),
			completedResponseID: bridgeWriter.completedResponseID,
			pendingToolCallIDs:  responsesWebsocketPendingToolCallIDs(bridgeWriter.completedOutput),
		}, nil
	}
	status := determineFinalClientStatus(lastResult)
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
		return responsesWebsocketTurnResult{}, &responsesWebsocketTerminalError{payload: bytes.Clone(lastResult.body)}
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
	return candidate != nil && candidate.Websockets &&
		protocol.Protocol(candidate.ResolveUpstreamProtocol(string(protocol.Codex))) == protocol.Codex
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
	if !gjson.ValidBytes(payload) {
		return false
	}
	switch strings.TrimSpace(gjson.GetBytes(payload, "type").String()) {
	case "error", "response.failed":
		return true
	default:
		return false
	}
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
}

func newResponsesWebsocketBridgeWriter(conn *websocket.Conn) *responsesWebsocketBridgeWriter {
	return &responsesWebsocketBridgeWriter{
		conn:               conn,
		header:             make(http.Header),
		outputItemsByIndex: make(map[int64][]byte),
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
	if int64(w.pending.Len()) > maxProxyBodyBytes("/v1/responses") {
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
		if !gjson.ValidBytes(payload) {
			return 0, errors.New("invalid JSON in upstream SSE event")
		}
		eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
		w.collectOutputItem(eventType, payload)
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
		if err := w.conn.SetWriteDeadline(time.Now().Add(responsesWebsocketWriteTimeout)); err != nil {
			return 0, err
		}
		if err := w.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			return 0, err
		}
		if eventType == "error" || eventType == "response.failed" {
			w.failed = true
			w.failedPayload = bytes.Clone(payload)
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

func (w *responsesWebsocketBridgeWriter) collectOutputItem(eventType string, payload []byte) {
	if eventType != "response.output_item.done" {
		return
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || !item.IsObject() {
		return
	}
	itemBytes := bytes.Clone([]byte(item.Raw))
	index := gjson.GetBytes(payload, "output_index")
	if index.Exists() {
		w.outputItemsByIndex[index.Int()] = itemBytes
		return
	}
	w.outputItemsUnindexed = append(w.outputItemsUnindexed, itemBytes)
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
	return writeResponsesWebsocketErrorPayload(
		conn, http.StatusBadRequest, "invalid_request_error", code, "", message,
	)
}

func writeResponsesWebsocketClientRetryError(conn *websocket.Conn) error {
	return writeResponsesWebsocketErrorPayload(
		conn,
		http.StatusBadGateway,
		"server_error",
		"upstream_unavailable",
		"",
		responsesWebsocketRetryMessage,
	)
}

func writeResponsesWebsocketErrorPayload(
	conn *websocket.Conn,
	status int,
	errorType string,
	code string,
	param string,
	message string,
) error {
	if err := conn.SetWriteDeadline(time.Now().Add(responsesWebsocketWriteTimeout)); err != nil {
		return err
	}
	errorPayload := gin.H{
		"type":    errorType,
		"code":    code,
		"message": message,
	}
	if strings.TrimSpace(param) != "" {
		errorPayload["param"] = param
	}
	return conn.WriteJSON(gin.H{
		"type":   "error",
		"status": status,
		"error":  errorPayload,
	})
}

func writeResponsesWebsocketPreviousResponseNotFound(conn *websocket.Conn) error {
	return writeResponsesWebsocketErrorPayload(
		conn,
		http.StatusBadRequest,
		"invalid_request_error",
		"previous_response_not_found",
		"previous_response_id",
		errResponsesWebsocketPreviousResponseNotFound.Error(),
	)
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
