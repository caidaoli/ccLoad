package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesWebsocketBeta = "responses_websockets=2026-02-06"
	codexResponsesLiteHeader    = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteMetadata  = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"

	codexWebsocketPingInterval        = 45 * time.Second
	codexWebsocketReadTimeout         = 5 * time.Minute
	codexWebsocketControlWriteTimeout = 10 * time.Second
	codexWebsocketReadQueueFrameLimit = 128
)

const (
	codexInputItemIDLimit                 = 64
	codexMessageItemIDPrefix              = "msg"
	codexReasoningItemIDPrefix            = "rs"
	codexFunctionCallItemIDPrefix         = "fc"
	codexCustomToolCallItemIDPrefix       = "ctc"
	codexCustomToolCallOutputItemIDPrefix = "ctco"
)

var codexWebsocketForwardHeaders = []string{
	"X-Codex-Beta-Features",
	"X-Codex-Turn-State",
	"X-Codex-Turn-Metadata",
	"X-Client-Request-Id",
	"X-ResponsesAPI-Include-Timing-Metrics",
	"Version",
	"User-Agent",
	"OpenAI-Beta",
	"Session_id",
	"Session-Id",
	"Thread-Id",
	"Originator",
}

type codexWebsocketTimeouts struct {
	idle time.Duration
	ping time.Duration
}

const (
	codexWebsocketFailureWindow      = 10 * time.Minute
	codexWebsocketFailureThreshold   = 2
	codexWebsocketTargetCooldownBase = 2 * time.Minute
	codexWebsocketTargetCooldownMax  = 30 * time.Minute
)

type codexWebsocketTarget struct {
	channelID     int64
	keyHash       [sha256.Size]byte
	headerHash    [sha256.Size]byte
	transportHash [sha256.Size]byte
	url           string
	baseURL       string
}

// codexWebsocketFailureTarget intentionally excludes request headers. Session-Id,
// Conversation-Id and similar headers define socket reuse, not upstream health.
type codexWebsocketFailureTarget struct {
	channelID     int64
	keyHash       [sha256.Size]byte
	transportHash [sha256.Size]byte
	baseURL       string
}

func (t codexWebsocketTarget) failureTarget() codexWebsocketFailureTarget {
	return codexWebsocketFailureTarget{
		channelID:     t.channelID,
		keyHash:       t.keyHash,
		transportHash: t.transportHash,
		baseURL:       t.baseURL,
	}
}

type codexWebsocketFailureState struct {
	count            int
	lastFailure      time.Time
	cooldownUntil    time.Time
	cooldownDuration time.Duration
}

// codexWebsocketFailureTracker is shared by all execution sessions in one server.
// It is deliberately in-memory: a process restart also creates fresh sockets.
type codexWebsocketFailureTracker struct {
	mu     sync.Mutex
	states map[codexWebsocketFailureTarget]codexWebsocketFailureState
}

func newCodexWebsocketFailureTracker() *codexWebsocketFailureTracker {
	return &codexWebsocketFailureTracker{
		states: make(map[codexWebsocketFailureTarget]codexWebsocketFailureState),
	}
}

func (t *codexWebsocketFailureTracker) recordFailure(target codexWebsocketFailureTarget, now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	state := t.states[target]
	if state.count == 0 || state.cooldownDuration == 0 && now.After(state.lastFailure.Add(codexWebsocketFailureWindow)) {
		state = codexWebsocketFailureState{count: 1, lastFailure: now}
		t.states[target] = state
		t.mu.Unlock()
		return
	}

	state.count++
	state.lastFailure = now
	if state.count >= codexWebsocketFailureThreshold {
		duration := codexWebsocketTargetCooldownBase
		if state.cooldownDuration > 0 {
			duration = min(state.cooldownDuration*2, codexWebsocketTargetCooldownMax)
		}
		state.cooldownDuration = duration
		state.cooldownUntil = now.Add(duration)
	}
	t.states[target] = state
	t.mu.Unlock()

	if !state.cooldownUntil.IsZero() {
		log.Printf(
			"[COOLDOWN] Codex WebSocket目标冷却: 渠道=%d 连续异常关闭=%d 时长=%v",
			target.channelID,
			state.count,
			state.cooldownDuration,
		)
	}
}

func (t *codexWebsocketFailureTracker) recordSuccess(target codexWebsocketFailureTarget) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.states, target)
	t.mu.Unlock()
}

func (t *codexWebsocketFailureTracker) cooldown(
	target codexWebsocketFailureTarget,
	now time.Time,
) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.states[target]
	if !ok || !now.Before(state.cooldownUntil) {
		return time.Time{}, false
	}
	return state.cooldownUntil, true
}

type codexWebsocketDebugSnapshot struct {
	RequestHeaders      http.Header
	ResponseStatus      int
	ResponseHeaders     http.Header
	Reconnects          uint64
	LastReconnectReason string
	LastCloseReason     string
}

// codexUpstreamWebsocketSession belongs to one execution session.
// The socket is disposable; responsesWebsocketSession owns the durable transcript.
type codexUpstreamWebsocketSession struct {
	turnMu                sync.Mutex
	writeMu               sync.Mutex
	mu                    sync.Mutex
	conn                  *websocket.Conn
	connDone              chan struct{}
	target                codexWebsocketTarget
	affinity              codexWebsocketTarget
	hasAffinity           bool
	reads                 []codexWebsocketRead
	readBytes             int64
	readNotify            chan struct{}
	readSpaceNotify       chan struct{}
	maxBodyBytes          int64
	maxAge                time.Duration
	maxAgeTimer           *time.Timer
	maxAgeExpired         bool
	turnTerminal          bool
	pendingHeartbeatConn  *websocket.Conn
	pendingHeartbeatCause error

	handshakeRequestHeaders  http.Header
	handshakeResponseStatus  int
	handshakeResponseHeaders http.Header
	connectedAt              time.Time
	handshakes               uint64
	reuses                   uint64
	heartbeatFailures        uint64
	reconnects               uint64
	lastReconnectReason      string
	lastCloseReason          string
	failureTracker           *codexWebsocketFailureTracker
}

func newCodexUpstreamWebsocketSession(
	maxBodyBytes int64,
	maxAge time.Duration,
	failureTracker *codexWebsocketFailureTracker,
) *codexUpstreamWebsocketSession {
	if maxBodyBytes <= 0 {
		maxBodyBytes = config.DefaultMaxBodyBytes
	}
	return &codexUpstreamWebsocketSession{
		readNotify:      make(chan struct{}, 1),
		readSpaceNotify: make(chan struct{}, 1),
		maxBodyBytes:    maxBodyBytes,
		maxAge:          maxAge,
		failureTracker:  failureTracker,
	}
}

type codexWebsocketRead struct {
	conn        *websocket.Conn
	messageType int
	payload     []byte
	err         error
	terminal    bool
}

type codexWebsocketHTTPFallbackError struct {
	cause error
}

func (e *codexWebsocketHTTPFallbackError) Error() string {
	if e == nil || e.cause == nil {
		return "Codex websocket reconnect requires HTTP fallback"
	}
	return "Codex websocket reconnect requires HTTP fallback: " + e.cause.Error()
}

func (e *codexWebsocketHTTPFallbackError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (s *codexUpstreamWebsocketSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closeLocked()
	s.clearAffinityLocked()
	s.mu.Unlock()
}

// CloseTransport drops only the physical socket. The durable execution session
// retains its last successful target so a later turn can prefer the same route.
func (s *codexUpstreamWebsocketSession) CloseTransport() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closeLocked()
	s.mu.Unlock()
}

func (s *codexUpstreamWebsocketSession) clearAffinityLocked() {
	s.affinity = codexWebsocketTarget{}
	s.hasAffinity = false
}

func (s *codexUpstreamWebsocketSession) closeLocked() {
	if s.maxAgeTimer != nil {
		s.maxAgeTimer.Stop()
		s.maxAgeTimer = nil
	}
	s.maxAgeExpired = false
	if s.connDone != nil {
		close(s.connDone)
		s.connDone = nil
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn = nil
	s.target = codexWebsocketTarget{}
	s.connectedAt = time.Time{}
	s.pendingHeartbeatConn = nil
	s.pendingHeartbeatCause = nil
	s.reads = nil
	s.readBytes = 0
	s.signalReadLocked()
}

func (s *codexUpstreamWebsocketSession) signalReadLocked() {
	select {
	case s.readNotify <- struct{}{}:
	default:
	}
}

func (s *codexUpstreamWebsocketSession) signalReadSpaceLocked() {
	select {
	case s.readSpaceNotify <- struct{}{}:
	default:
	}
}

func (s *codexUpstreamWebsocketSession) enqueueRead(event codexWebsocketRead) bool {
	eventBytes := int64(len(event.payload))
	for {
		s.mu.Lock()
		if s.conn != event.conn {
			s.mu.Unlock()
			return false
		}
		if event.terminal && !s.turnTerminal {
			s.turnTerminal = true
			s.failureTracker.recordSuccess(s.target.failureTarget())
		}
		maxQueueBytes := s.maxBodyBytes
		// Read failures must remain observable even when the data queue is full.
		if event.err != nil ||
			(len(s.reads) < codexWebsocketReadQueueFrameLimit && eventBytes <= maxQueueBytes-s.readBytes) {
			s.reads = append(s.reads, event)
			s.readBytes += eventBytes
			s.signalReadLocked()
			s.mu.Unlock()
			return true
		}
		done := s.connDone
		s.mu.Unlock()
		if done == nil {
			return false
		}
		select {
		case <-done:
			return false
		case <-s.readSpaceNotify:
		}
	}
}

func (s *codexUpstreamWebsocketSession) nextRead(ctx context.Context, conn *websocket.Conn) (codexWebsocketRead, error) {
	for {
		s.mu.Lock()
		if len(s.reads) > 0 && s.reads[0].conn == conn {
			event := s.reads[0]
			s.reads[0] = codexWebsocketRead{}
			s.reads = s.reads[1:]
			s.readBytes -= int64(len(event.payload))
			if s.readBytes < 0 {
				s.readBytes = 0
			}
			s.signalReadSpaceLocked()
			if len(s.reads) > 0 {
				s.signalReadLocked()
			}
			s.mu.Unlock()
			return event, nil
		}
		if s.conn != conn {
			s.mu.Unlock()
			return codexWebsocketRead{}, net.ErrClosed
		}
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return codexWebsocketRead{}, ctx.Err()
		case <-s.readNotify:
		}
	}
}

type codexWebsocketRuntimeStats struct {
	connected         bool
	connectedAt       time.Time
	handshakes        uint64
	reuses            uint64
	reconnects        uint64
	heartbeatFailures uint64
	queuedReadBytes   int64
}

func (s *codexUpstreamWebsocketSession) runtimeStats() codexWebsocketRuntimeStats {
	if s == nil {
		return codexWebsocketRuntimeStats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return codexWebsocketRuntimeStats{
		connected:         s.conn != nil,
		connectedAt:       s.connectedAt,
		handshakes:        s.handshakes,
		reuses:            s.reuses,
		reconnects:        s.reconnects,
		heartbeatFailures: s.heartbeatFailures,
		queuedReadBytes:   s.readBytes,
	}
}

func (s *codexUpstreamWebsocketSession) recordReconnect(reason string) {
	s.mu.Lock()
	s.reconnects++
	s.lastReconnectReason = strings.TrimSpace(reason)
	s.mu.Unlock()
}

func (s *codexUpstreamWebsocketSession) debugSnapshot() codexWebsocketDebugSnapshot {
	if s == nil {
		return codexWebsocketDebugSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return codexWebsocketDebugSnapshot{
		RequestHeaders:      s.handshakeRequestHeaders.Clone(),
		ResponseStatus:      s.handshakeResponseStatus,
		ResponseHeaders:     s.handshakeResponseHeaders.Clone(),
		Reconnects:          s.reconnects,
		LastReconnectReason: s.lastReconnectReason,
		LastCloseReason:     s.lastCloseReason,
	}
}

func (s *codexUpstreamWebsocketSession) connectionReusable(target codexWebsocketTarget) (*websocket.Conn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || s.target != target {
		return nil, false
	}
	if s.maxAgeExpired || s.maxAge > 0 && time.Since(s.connectedAt) >= s.maxAge {
		s.closeLocked()
		return nil, false
	}
	if len(s.reads) > 0 {
		// Any data between turns is either a recorded disconnect or an unsolicited
		// response frame. Neither is safe to associate with the next request.
		s.closeLocked()
		return nil, false
	}
	s.reuses++
	return s.conn, true
}

func (s *codexUpstreamWebsocketSession) expireConnection(conn *websocket.Conn) {
	s.mu.Lock()
	if s.conn != conn {
		s.mu.Unlock()
		return
	}
	s.maxAgeExpired = true
	s.mu.Unlock()

	// An idle socket can close immediately. An active turn owns turnMu and will
	// retire the socket in finishTurn after its terminal event has drained.
	if !s.turnMu.TryLock() {
		return
	}
	s.mu.Lock()
	if s.conn == conn && s.maxAgeExpired {
		s.closeLocked()
	}
	s.mu.Unlock()
	s.turnMu.Unlock()
}

func (s *codexUpstreamWebsocketSession) finishTurn() {
	s.mu.Lock()
	if s.maxAgeExpired {
		s.closeLocked()
	}
	s.mu.Unlock()
	s.turnMu.Unlock()
}

func (s *codexUpstreamWebsocketSession) startReader(
	conn *websocket.Conn,
	done <-chan struct{},
	timeouts codexWebsocketTimeouts,
) {
	refreshReadDeadline := func() error {
		return conn.SetReadDeadline(time.Now().Add(timeouts.idle))
	}
	conn.SetPingHandler(func(appData string) error {
		if err := refreshReadDeadline(); err != nil {
			return err
		}
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		return conn.WriteControl(
			websocket.PongMessage,
			[]byte(appData),
			time.Now().Add(codexWebsocketControlWriteTimeout),
		)
	})
	conn.SetPongHandler(func(string) error { return refreshReadDeadline() })
	go func() {
		for {
			if err := refreshReadDeadline(); err != nil {
				s.detachReadFailure(conn, err, false)
				return
			}
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				s.detachReadFailure(conn, err, isCodexWebsocketHeartbeatFailure(err))
				return
			}
			// A nested gateway reports its broken upstream socket as a structured
			// retry event. Restore the transport provenance before retry/cooldown.
			if interruptedErr := codexWebsocketInterruptedEventError(messageType, payload); interruptedErr != nil {
				s.detachReadFailure(conn, interruptedErr, false)
				return
			}
			if !s.enqueueRead(codexWebsocketRead{
				conn:        conn,
				messageType: messageType,
				payload:     payload,
				terminal: messageType == websocket.TextMessage && isCodexWebsocketTerminalEvent(
					strings.TrimSpace(gjson.GetBytes(payload, "type").String()),
				),
			}) {
				return
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(timeouts.ping)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s.writeMu.Lock()
				err := conn.WriteControl(
					websocket.PingMessage,
					nil,
					time.Now().Add(codexWebsocketControlWriteTimeout),
				)
				s.writeMu.Unlock()
				if err != nil {
					s.signalHeartbeatFailure(conn, fmt.Errorf("ping upstream websocket: %w", err))
					return
				}
			}
		}
	}()
}

func isCodexWebsocketHeartbeatFailure(cause error) bool {
	var netErr net.Error
	return errors.As(cause, &netErr) && netErr.Timeout()
}

func (s *codexUpstreamWebsocketSession) signalHeartbeatFailure(conn *websocket.Conn, cause error) {
	s.mu.Lock()
	active := s.conn == conn
	if active {
		s.pendingHeartbeatConn = conn
		s.pendingHeartbeatCause = cause
	}
	s.mu.Unlock()
	if active {
		// Reader is the sole detach authority. Closing only wakes it while
		// preserving the order between an already-read terminal frame and EOF.
		_ = conn.Close()
	}
}

func (s *codexUpstreamWebsocketSession) detachReadFailure(
	conn *websocket.Conn,
	cause error,
	heartbeatFailure bool,
) {
	s.mu.Lock()
	if s.conn == conn {
		if s.pendingHeartbeatConn == conn {
			heartbeatFailure = true
			if s.pendingHeartbeatCause != nil {
				cause = s.pendingHeartbeatCause
			}
			s.pendingHeartbeatConn = nil
			s.pendingHeartbeatCause = nil
		}
		target := s.target
		readErr := cause
		if !s.turnTerminal && isCodexWebsocketTransportFailure(cause, heartbeatFailure) {
			failureTarget := target.failureTarget()
			s.failureTracker.recordFailure(failureTarget, time.Now())
			readErr = &codexWebsocketTransportError{cause: cause}
		}
		// Reader is the only detach authority, so one physical connection is counted once.
		s.reads = append(s.reads, codexWebsocketRead{conn: conn, err: readErr})
		if s.maxAgeTimer != nil {
			s.maxAgeTimer.Stop()
			s.maxAgeTimer = nil
		}
		s.maxAgeExpired = false
		if s.connDone != nil {
			close(s.connDone)
			s.connDone = nil
		}
		_ = conn.Close()
		s.conn = nil
		s.target = codexWebsocketTarget{}
		s.connectedAt = time.Time{}
		if heartbeatFailure {
			s.heartbeatFailures++
		}
		if cause != nil {
			s.lastCloseReason = cause.Error()
		}
		s.signalReadLocked()
	}
	s.mu.Unlock()
}

func (s *codexUpstreamWebsocketSession) dial(
	ctx context.Context,
	dialer *websocket.Dialer,
	target codexWebsocketTarget,
	req *http.Request,
	timeouts codexWebsocketTimeouts,
) (*websocket.Conn, *http.Response, error) {
	wsURL, err := codexWebsocketURL(req.URL.String())
	if err != nil {
		return nil, nil, err
	}
	wireHeaders := codexWebsocketHeaders(req.Header)
	conn, resp, err := dialer.DialContext(ctx, wsURL, wireHeaders)
	if err != nil {
		return nil, resp, err
	}
	// Negotiate permessage-deflate for upstream responses, but keep outbound
	// request frames uncompressed to match Codex CLI and avoid flate-tail
	// interoperability bugs seen in some gateways.
	conn.EnableWriteCompression(false)
	conn.SetReadLimit(s.maxBodyBytes)
	done := make(chan struct{})
	s.mu.Lock()
	if s.conn != nil {
		s.closeLocked()
	}
	s.conn = conn
	s.connDone = done
	s.target = target
	s.affinity = target
	s.hasAffinity = true
	s.connectedAt = time.Now()
	s.maxAgeExpired = false
	s.turnTerminal = false
	s.pendingHeartbeatConn = nil
	s.pendingHeartbeatCause = nil
	if s.maxAge > 0 {
		s.maxAgeTimer = time.AfterFunc(s.maxAge, func() { s.expireConnection(conn) })
	}
	s.handshakes++
	s.reads = nil
	s.readBytes = 0
	s.handshakeRequestHeaders = wireHeaders.Clone()
	if resp != nil {
		s.handshakeResponseStatus = resp.StatusCode
		s.handshakeResponseHeaders = resp.Header.Clone()
	} else {
		s.handshakeResponseStatus = http.StatusSwitchingProtocols
		s.handshakeResponseHeaders = nil
	}
	s.mu.Unlock()
	s.startReader(conn, done, timeouts)
	return conn, resp, nil
}

func (s *codexUpstreamWebsocketSession) writeRequest(conn *websocket.Conn, body []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(config.HTTPDialTimeout))
	err := conn.WriteMessage(websocket.TextMessage, body)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func isCodexWebsocketSemanticEvent(eventType string) bool {
	// eventType != "" 不能省：isResponsesMetadataEvent("") == false，
	// 省掉后未解析出 type 的 WS 帧会被当成语义输出。
	return eventType != "" && !isResponsesMetadataEvent(eventType)
}

func isCodexWebsocketTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.incomplete":
		return true
	default:
		return false
	}
}

func (s *codexUpstreamWebsocketSession) invalidate(conn *websocket.Conn) {
	s.mu.Lock()
	if s.conn == conn {
		s.closeLocked()
	}
	s.mu.Unlock()
}

func codexWebsocketTargetForRequest(
	cfg *model.Config,
	req *http.Request,
	skipTLSVerify bool,
	baseURL string,
) codexWebsocketTarget {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = req.URL.String()
	}
	return codexWebsocketTarget{
		channelID:     cfg.ID,
		keyHash:       sha256.Sum256([]byte(req.Header.Get("Authorization"))),
		headerHash:    codexWebsocketHeaderHash(codexWebsocketHeaders(req.Header)),
		transportHash: codexWebsocketTransportHash(cfg, skipTLSVerify),
		url:           req.URL.String(),
		baseURL:       baseURL,
	}
}

func codexWebsocketHeaderHash(header http.Header) [sha256.Size]byte {
	normalized := make(map[string][]string, len(header))
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		left := strings.ToLower(strings.TrimSpace(names[i]))
		right := strings.ToLower(strings.TrimSpace(names[j]))
		if left == right {
			return names[i] < names[j]
		}
		return left < right
	})
	for _, name := range names {
		key := strings.ToLower(strings.TrimSpace(name))
		// net/http and gorilla/websocket synthesize the default User-Agent when
		// the caller omits it. Downstream HTTP and WebSocket transports therefore
		// represent the same effective handshake differently. User-Agent does not
		// define upstream authorization or routing, so it must not split sessions.
		if key == "user-agent" {
			continue
		}
		normalized[key] = append(normalized[key], header[name]...)
	}
	keys := make([]string, 0, len(normalized))
	for key := range normalized {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	hash := sha256.New()
	for _, key := range keys {
		_, _ = io.WriteString(hash, key)
		for _, value := range normalized[key] {
			_, _ = io.WriteString(hash, "\x00"+value)
		}
		_, _ = io.WriteString(hash, "\xff")
	}
	var out [sha256.Size]byte
	copy(out[:], hash.Sum(nil))
	return out
}

func codexWebsocketTransportHash(cfg *model.Config, skipTLSVerify bool) [sha256.Size]byte {
	hash := sha256.New()
	if cfg != nil {
		_, _ = io.WriteString(hash, strings.TrimSpace(cfg.ProxyURL))
		for _, rule := range cfg.HeaderRules() {
			_, _ = io.WriteString(hash, "\x00"+rule.Action+"\x00"+rule.Name+"\x00"+rule.Value)
		}
	}
	if skipTLSVerify {
		_, _ = io.WriteString(hash, "\x00skip-tls-verify")
	}
	var out [sha256.Size]byte
	copy(out[:], hash.Sum(nil))
	return out
}

func codexWebsocketKeyHash(apiKey string) [sha256.Size]byte {
	return sha256.Sum256([]byte("Bearer " + apiKey))
}

func (s *codexUpstreamWebsocketSession) targetSnapshot() (codexWebsocketTarget, bool) {
	if s == nil {
		return codexWebsocketTarget{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return codexWebsocketTarget{}, false
	}
	return s.target, true
}

func (s *codexUpstreamWebsocketSession) affinitySnapshot() (codexWebsocketTarget, bool) {
	if s == nil {
		return codexWebsocketTarget{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasAffinity {
		return codexWebsocketTarget{}, false
	}
	return s.affinity, true
}

func codexWebsocketURL(httpURL string) (string, error) {
	switch {
	case strings.HasPrefix(httpURL, "https://"):
		return "wss://" + strings.TrimPrefix(httpURL, "https://"), nil
	case strings.HasPrefix(httpURL, "http://"):
		return "ws://" + strings.TrimPrefix(httpURL, "http://"), nil
	default:
		return "", fmt.Errorf("unsupported websocket upstream URL: %q", httpURL)
	}
}

func isCodexWebsocketHandshakeFallbackError(err error) bool {
	return errors.Is(err, websocket.ErrBadHandshake) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

func buildCodexWebsocketRequestBody(body []byte) ([]byte, error) {
	if !sonic.Valid(body) {
		return nil, errors.New("invalid Codex websocket request JSON")
	}
	body = sanitizeCodexInputItemIDs(body)
	prepared, err := sjson.SetBytes(body, "type", responsesWebsocketRequestCreate)
	if err != nil {
		return nil, fmt.Errorf("set Codex websocket request type: %w", err)
	}
	prepared, err = sjson.SetBytes(prepared, "stream", true)
	if err != nil {
		return nil, fmt.Errorf("force Codex websocket streaming: %w", err)
	}
	return prepared, nil
}

func normalizeCodexWebsocketParallelToolCalls(body []byte, headers http.Header) []byte {
	responsesLite := strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true")
	if !responsesLite {
		metadata := gjson.GetBytes(body, codexResponsesLiteMetadata)
		responsesLite = metadata.Type == gjson.True ||
			metadata.Type == gjson.String && strings.EqualFold(strings.TrimSpace(metadata.String()), "true")
	}
	if !responsesLite || !gjson.GetBytes(body, "parallel_tool_calls").Bool() {
		return body
	}
	updated, err := sjson.SetBytes(body, "parallel_tool_calls", false)
	if err != nil {
		return body
	}
	return updated
}

func sanitizeCodexInputItemIDs(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	items := input.Array()
	occupied := make(map[string]struct{}, len(items))
	for _, item := range items {
		if shouldDropCodexEncryptedReasoningItem(item) {
			continue
		}
		itemID := item.Get("id")
		if itemID.Type != gjson.String {
			continue
		}
		originalID := itemID.String()
		if normalizeCodexInputItemID(item, originalID) == originalID &&
			len([]rune(originalID)) <= codexInputItemIDLimit {
			occupied[originalID] = struct{}{}
		}
	}

	type inputItemIDMappingKey struct {
		originalID   string
		normalizedID string
	}
	mapped := make(map[inputItemIDMappingKey]string, len(items))
	rebuilt := make([]string, 0, len(items))
	changed := false
	for _, item := range items {
		if shouldDropCodexEncryptedReasoningItem(item) {
			changed = true
			continue
		}
		raw := item.Raw
		itemID := item.Get("id")
		if itemID.Type == gjson.String {
			originalID := itemID.String()
			id := normalizeCodexInputItemID(item, originalID)
			if id != originalID || len([]rune(id)) > codexInputItemIDLimit {
				mappingKey := inputItemIDMappingKey{originalID: originalID, normalizedID: id}
				shortened, ok := mapped[mappingKey]
				if !ok {
					for attempt := 0; ; attempt++ {
						shortened = shortenCodexInputItemID(id, attempt)
						if _, exists := occupied[shortened]; !exists {
							break
						}
					}
					mapped[mappingKey] = shortened
					occupied[shortened] = struct{}{}
				}
				id = shortened
			}
			if id != originalID {
				if updated, errSet := sjson.SetBytes([]byte(raw), "id", id); errSet == nil {
					raw = string(updated)
					changed = true
				}
			}
		}
		rebuilt = append(rebuilt, raw)
	}
	if !changed {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(rebuilt, ",")+"]"))
	if err != nil {
		return body
	}
	return updated
}

func normalizeCodexInputItemID(item gjson.Result, id string) string {
	var prefix string
	switch item.Get("type").String() {
	case "message":
		prefix = codexMessageItemIDPrefix
	case "reasoning":
		prefix = codexReasoningItemIDPrefix
	case "function_call":
		prefix = codexFunctionCallItemIDPrefix
	case "custom_tool_call":
		prefix = codexCustomToolCallItemIDPrefix
	case "custom_tool_call_output":
		prefix = codexCustomToolCallOutputItemIDPrefix
	default:
		return id
	}
	if id == "" || strings.HasPrefix(id, prefix) {
		return id
	}
	return prefix + "_" + id
}

func shouldDropCodexEncryptedReasoningItem(item gjson.Result) bool {
	if item.Get("type").String() != "reasoning" {
		return false
	}
	itemID := item.Get("id")
	if itemID.Type != gjson.String || len([]rune(itemID.String())) <= codexInputItemIDLimit {
		return false
	}
	encrypted := item.Get("encrypted_content")
	return encrypted.Type == gjson.String && encrypted.String() != ""
}

func shortenCodexInputItemID(id string, attempt int) string {
	runes := []rune(id)
	if len(runes) <= codexInputItemIDLimit && attempt == 0 {
		return id
	}
	hashInput := id
	if attempt > 0 {
		hashInput += "\x00" + fmt.Sprint(attempt)
	}
	sum := sha256.Sum256([]byte(hashInput))
	suffix := fmt.Sprintf("_%x", sum[:8])
	prefixLength := min(len(runes), codexInputItemIDLimit-len(suffix))
	return string(runes[:prefixLength]) + suffix
}

func copyCodexWebsocketInputHeaders(target, source http.Header) {
	if target == nil {
		return
	}
	for _, name := range codexWebsocketForwardHeaders {
		if target.Get(name) != "" {
			continue
		}
		if value := strings.TrimSpace(source.Get(name)); value != "" {
			target.Set(name, value)
		}
	}
}

func codexWebsocketHeaders(source http.Header) http.Header {
	header := source.Clone()
	for key := range header {
		lower := strings.ToLower(key)
		if lower == "accept" || lower == "connection" || lower == "content-length" ||
			lower == "content-type" || lower == "upgrade" ||
			strings.HasPrefix(lower, "sec-websocket-") {
			header.Del(key)
		}
	}
	betaHeader := strings.TrimSpace(header.Get("OpenAI-Beta"))
	if betaHeader == "" || !strings.Contains(betaHeader, "responses_websockets=") {
		betaHeader = codexResponsesWebsocketBeta
	}
	header.Set("OpenAI-Beta", betaHeader)

	// Current Codex clients use the canonical Session-Id header. Keep it intact:
	// a downstream ccLoad instance needs Session-Id + Thread-Id to isolate parent
	// and subagent execution sessions. The official websocket still receives its
	// legacy aliases, all normalized from the same canonical value.
	sessionID := strings.TrimSpace(header.Get("Session-Id"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(header.Get("Session_id"))
	}
	if sessionID != "" {
		header.Set("Session-Id", sessionID)
		header.Set("Session_id", sessionID)
		if strings.TrimSpace(header.Get("Conversation_id")) == "" {
			header.Set("Conversation_id", sessionID)
		}
	}
	return header
}

func (s *Server) codexWebsocketDialer(cfg *model.Config) *websocket.Dialer {
	transport := buildHTTPTransport(s.skipTLSVerify)
	if cfg.ProxyURL != "" {
		proxied, err := buildChannelProxyTransport(cfg.ProxyURL, s.skipTLSVerify)
		if err != nil {
			log.Printf("[WARN] 渠道 %d WebSocket 代理 %q 无效，回退全局: %v", cfg.ID, cfg.ProxyURL, err)
		} else {
			transport = proxied
		}
	}
	tlsConfig := transport.TLSClientConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		tlsConfig = tlsConfig.Clone()
	}
	return &websocket.Dialer{
		Proxy:             transport.Proxy,
		NetDialContext:    transport.DialContext,
		HandshakeTimeout:  config.HTTPTLSHandshakeTimeout,
		TLSClientConfig:   tlsConfig,
		EnableCompression: true,
	}
}

func (s *Server) codexWebsocketTimeouts() codexWebsocketTimeouts {
	timeouts := codexWebsocketTimeouts{
		idle: codexWebsocketReadTimeout,
		ping: codexWebsocketPingInterval,
	}
	if s == nil {
		return timeouts
	}
	if s.responsesWebsocketIdleTimeoutOverride > 0 {
		timeouts.idle = s.responsesWebsocketIdleTimeoutOverride
	}
	if s.responsesWebsocketPingIntervalOverride > 0 {
		timeouts.ping = s.responsesWebsocketPingIntervalOverride
	}
	return timeouts
}

type codexWebsocketResponseBody struct {
	*io.PipeReader
	completed atomic.Bool
	abort     func()
}

type codexWebsocketTransportError struct {
	cause error
}

var errCodexWebsocketInterruptedEvent = errors.New("upstream websocket reported interrupted stream")

func (e *codexWebsocketTransportError) Error() string {
	if e == nil || e.cause == nil {
		return "upstream websocket transport interrupted"
	}
	return e.cause.Error()
}

func (e *codexWebsocketTransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type codexWebsocketTargetCooldownError struct {
	until time.Time
}

func (e *codexWebsocketTargetCooldownError) Error() string {
	if e == nil || e.until.IsZero() {
		return "upstream websocket target is cooling down"
	}
	return fmt.Sprintf("upstream websocket target is cooling down until %s", e.until.Format(time.RFC3339))
}

func isCodexWebsocketTransportFailure(err error, heartbeatFailure bool) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr) && closeErr.Code == websocket.CloseAbnormalClosure ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, errCodexWebsocketInterruptedEvent) ||
		heartbeatFailure
}

func codexWebsocketInterruptedEventError(messageType int, payload []byte) error {
	if messageType != websocket.TextMessage ||
		gjson.GetBytes(payload, "error.code").String() != responsesWebsocketInterruptedCode {
		return nil
	}
	message := strings.TrimSpace(gjson.GetBytes(payload, "error.message").String())
	if message == "" {
		return errCodexWebsocketInterruptedEvent
	}
	return fmt.Errorf("%w: %s", errCodexWebsocketInterruptedEvent, message)
}

func (b *codexWebsocketResponseBody) Close() error {
	if !b.completed.Load() && b.abort != nil {
		b.abort()
	}
	return b.PipeReader.Close()
}

func writeSyntheticSSEFrame(w io.Writer, payload []byte) error {
	payload = bytes.ReplaceAll(payload, []byte("\r\n"), []byte("\n"))
	payload = bytes.TrimRight(payload, "\n")
	if len(payload) == 0 {
		_, err := w.Write([]byte("data: \n\n"))
		return err
	}
	var buf bytes.Buffer
	lines := bytes.Split(payload, []byte("\n"))
	buf.Grow(len(payload) + len(lines)*len("data: ") + 2)
	for _, line := range lines {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

func (s *codexUpstreamWebsocketSession) reconnectWithReplay(
	ctx context.Context,
	dialer *websocket.Dialer,
	target codexWebsocketTarget,
	replayReq *http.Request,
	replayBody []byte,
	timeouts codexWebsocketTimeouts,
) (*websocket.Conn, http.Header, error) {
	prepared, err := buildCodexWebsocketRequestBody(replayBody)
	if err != nil {
		return nil, nil, err
	}
	conn, resp, err := s.dial(ctx, dialer, target, replayReq, timeouts)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if resp != nil || isCodexWebsocketHandshakeFallbackError(err) {
			return nil, nil, &codexWebsocketHTTPFallbackError{cause: err}
		}
		return nil, nil, err
	}
	if err = s.writeRequest(conn, prepared); err != nil {
		s.invalidate(conn)
		return nil, nil, err
	}
	var handshakeHeaders http.Header
	if resp != nil {
		handshakeHeaders = cloneCodexQuotaHeaders(resp.Header)
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return conn, handshakeHeaders, nil
}

func (s *codexUpstreamWebsocketSession) streamResponse(
	ctx context.Context,
	conn *websocket.Conn,
	request *http.Request,
	dialer *websocket.Dialer,
	target codexWebsocketTarget,
	replayReq *http.Request,
	replayBody []byte,
	timeouts codexWebsocketTimeouts,
	handshakeHeaders http.Header,
	onReconnectHandshake func(http.Header),
) *http.Response {
	reader, writer := io.Pipe()
	body := &codexWebsocketResponseBody{PipeReader: reader, abort: func() { s.invalidate(conn) }}
	go func() {
		defer s.finishTurn()
		stopCancel := context.AfterFunc(ctx, func() {
			if !body.completed.Load() {
				s.invalidate(conn)
			}
		})
		defer stopCancel()

		semanticOutput := false
		retried := false
		missingStoredItemRetries := 0
		for {
			event, errNext := s.nextRead(ctx, conn)
			if errNext != nil || event.err != nil {
				readErr := errNext
				if readErr == nil {
					readErr = event.err
				}
				s.invalidate(conn)
				if isCodexWebsocketMessageTooBigError(readErr) {
					_ = writeSyntheticSSEFrame(writer, codexWebsocketMessageTooBigPayload())
					_ = writer.Close()
					return
				}
				if !semanticOutput && !retried && ctx.Err() == nil {
					retried = true
					s.recordReconnect("read: " + readErr.Error())
					connRetry, retryHeaders, errRetry := s.reconnectWithReplay(
						ctx, dialer, target, replayReq, replayBody, timeouts,
					)
					if errRetry == nil {
						if onReconnectHandshake != nil && len(retryHeaders) > 0 {
							onReconnectHandshake(retryHeaders)
						}
						conn = connRetry
						continue
					}
					_ = writer.CloseWithError(errRetry)
					return
				}
				_ = writer.CloseWithError(readErr)
				return
			}
			if event.messageType != websocket.TextMessage {
				s.invalidate(conn)
				_ = writer.CloseWithError(fmt.Errorf("unsupported Codex websocket frame type %d", event.messageType))
				return
			}
			payload := event.payload
			eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
			if !semanticOutput && !retried && ctx.Err() == nil &&
				isCodexWebsocketPreviousResponseNotFound(payload) {
				retried = true
				s.invalidate(conn)
				s.recordReconnect("previous_response_not_found")
				connRetry, retryHeaders, errRetry := s.reconnectWithReplay(
					ctx, dialer, target, replayReq, replayBody, timeouts,
				)
				if errRetry == nil {
					if onReconnectHandshake != nil && len(retryHeaders) > 0 {
						onReconnectHandshake(retryHeaders)
					}
					conn = connRetry
					continue
				}
				_ = writer.CloseWithError(errRetry)
				return
			}
			if !semanticOutput && ctx.Err() == nil &&
				missingStoredItemRetries < responsesMissingStoredItemRetryLimit {
				if retryReplay, ok := codexWebsocketMissingStoredInputRetryBody(replayBody, payload); ok {
					missingStoredItemRetries++
					s.invalidate(conn)
					s.recordReconnect("missing_stored_input_item")
					connRetry, retryHeaders, errRetry := s.reconnectWithReplay(
						ctx, dialer, target, replayReq, retryReplay, timeouts,
					)
					if errRetry == nil {
						if onReconnectHandshake != nil && len(retryHeaders) > 0 {
							onReconnectHandshake(retryHeaders)
						}
						conn = connRetry
						replayBody = retryReplay
						continue
					}
					_ = writer.CloseWithError(errRetry)
					return
				}
			}
			if isCodexWebsocketTerminalEvent(eventType) {
				// The HTTP/SSE consumer may stop reading as soon as it sees the
				// terminal event. Mark the turn complete before the pipe write so
				// Response.Body.Close cannot invalidate a reusable socket in that race.
				body.completed.Store(true)
			}
			if err := writeSyntheticSSEFrame(writer, payload); err != nil {
				s.invalidate(conn)
				_ = writer.CloseWithError(err)
				return
			}
			semanticOutput = semanticOutput || isCodexWebsocketSemanticEvent(eventType)

			switch eventType {
			case "response.completed", "response.done", "response.incomplete":
				_ = writer.Close()
				return
			case "response.failed", "error":
				s.invalidate(conn)
				_ = writer.Close()
				return
			}
		}
	}()

	responseHeaders := cloneCodexQuotaHeaders(handshakeHeaders)
	responseHeaders.Set("Content-Type", "text/event-stream")
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     responseHeaders,
		Body:       body,
		Request:    request,
	}
}

func isCodexWebsocketMessageTooBigError(err error) bool {
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr) && closeErr.Code == websocket.CloseMessageTooBig
}

func isCodexWebsocketPreviousResponseNotFound(payload []byte) bool {
	code := strings.TrimSpace(gjson.GetBytes(payload, "error.code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String())
	}
	if code == "previous_response_not_found" {
		return true
	}
	message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.message").String()))
	if message == "" {
		message = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.message").String()))
	}
	return strings.Contains(message, "previous_response_id") && strings.Contains(message, "not found")
}

func codexWebsocketMessageTooBigPayload() []byte {
	return []byte(`{"type":"error","status":413,"error":{"message":"upstream websocket message too big","type":"invalid_request_error","code":"message_too_big"}}`)
}

// roundTrip chooses the incremental request only when the physical connection still matches
// channel, credential and URL. A new target always receives the replay request.
func (s *codexUpstreamWebsocketSession) roundTrip(
	ctx context.Context,
	cfg *model.Config,
	dialer *websocket.Dialer,
	replayReq *http.Request,
	replayBody []byte,
	incrementalReq *http.Request,
	incrementalBody []byte,
	skipTLSVerify bool,
	timeouts codexWebsocketTimeouts,
	baseURL string,
	onReconnectHandshake func(http.Header),
) (resp *http.Response, usedReq *http.Request, usedBody []byte, err error) {
	s.turnMu.Lock()
	handedOff := false
	defer func() {
		if !handedOff {
			s.finishTurn()
		}
	}()
	target := codexWebsocketTargetForRequest(cfg, replayReq, skipTLSVerify, baseURL)
	if until, cooling := s.failureTracker.cooldown(target.failureTarget(), time.Now()); cooling {
		return nil, replayReq, replayBody, &codexWebsocketTargetCooldownError{until: until}
	}

	conn, reuse := s.connectionReusable(target)
	s.mu.Lock()
	if !reuse {
		s.closeLocked()
	}
	s.mu.Unlock()

	usedReq, usedBody = replayReq, replayBody
	var handshakeHeaders http.Header
	if reuse && incrementalReq != nil && len(incrementalBody) > 0 {
		usedReq, usedBody = incrementalReq, incrementalBody
	}
	preparedBody, err := buildCodexWebsocketRequestBody(usedBody)
	if err != nil {
		return nil, usedReq, usedBody, err
	}

	if !reuse {
		conn, resp, err = s.dial(ctx, dialer, target, replayReq, timeouts)
		if err != nil {
			if resp != nil {
				if resp.Body == nil {
					resp.Body = io.NopCloser(strings.NewReader(""))
				}
				return resp, usedReq, usedBody, nil
			}
			return nil, usedReq, usedBody, err
		}
		if resp != nil {
			handshakeHeaders = cloneCodexQuotaHeaders(resp.Header)
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
	} else {
		s.mu.Lock()
		if s.conn == conn {
			s.turnTerminal = false
		}
		s.mu.Unlock()
	}

	err = s.writeRequest(conn, preparedBody)
	if err != nil {
		s.invalidate(conn)
		s.recordReconnect("write: " + err.Error())
		preparedReplay, errPrepare := buildCodexWebsocketRequestBody(replayBody)
		if errPrepare == nil {
			connRetry, respRetry, errDial := s.dial(ctx, dialer, target, replayReq, timeouts)
			if errDial == nil {
				var retryHeaders http.Header
				if respRetry != nil {
					retryHeaders = cloneCodexQuotaHeaders(respRetry.Header)
					if respRetry.Body != nil {
						_ = respRetry.Body.Close()
					}
				}
				if errWrite := s.writeRequest(connRetry, preparedReplay); errWrite == nil {
					response := s.streamResponse(
						ctx, connRetry, replayReq, dialer, target, replayReq, replayBody, timeouts, retryHeaders,
						onReconnectHandshake,
					)
					handedOff = true
					return response, replayReq, replayBody, nil
				}
				s.invalidate(connRetry)
			} else if respRetry != nil {
				if respRetry.Body == nil {
					respRetry.Body = io.NopCloser(strings.NewReader(""))
				}
				return respRetry, replayReq, replayBody, nil
			} else if isCodexWebsocketHandshakeFallbackError(errDial) {
				return nil, replayReq, replayBody, errDial
			}
		}
		return nil, usedReq, usedBody, err
	}
	response := s.streamResponse(
		ctx, conn, usedReq, dialer, target, replayReq, replayBody, timeouts, handshakeHeaders,
		onReconnectHandshake,
	)
	handedOff = true
	return response, usedReq, usedBody, nil
}

func (s *Server) doCodexWebsocketRequest(
	ctx context.Context,
	cfg *model.Config,
	session *codexUpstreamWebsocketSession,
	replayReq *http.Request,
	replayBody []byte,
	incrementalReq *http.Request,
	incrementalBody []byte,
	baseURL string,
) (*http.Response, *http.Request, []byte, error) {
	if replayReq != nil {
		replayBody = normalizeCodexWebsocketParallelToolCalls(replayBody, replayReq.Header)
	}
	if incrementalReq != nil {
		incrementalBody = normalizeCodexWebsocketParallelToolCalls(incrementalBody, incrementalReq.Header)
	}
	release, err := s.reserveUpstreamRequest(cfg)
	if err != nil {
		return nil, replayReq, replayBody, err
	}
	resp, usedReq, usedBody, err := session.roundTrip(
		ctx,
		cfg,
		s.codexWebsocketDialer(cfg),
		replayReq,
		replayBody,
		incrementalReq,
		incrementalBody,
		s.skipTLSVerify,
		s.codexWebsocketTimeouts(),
		baseURL,
		func(headers http.Header) {
			if len(headers) == 0 {
				return
			}
			s.persistCodexPassiveUsage(ctx, cfg, &http.Response{
				StatusCode: http.StatusOK,
				Header:     headers,
			})
		},
	)
	if err != nil {
		release()
		return nil, usedReq, usedBody, err
	}
	if resp == nil || resp.Body == nil {
		release()
		return resp, usedReq, usedBody, nil
	}
	resp.Body = &releaseOnCloseReadCloser{ReadCloser: resp.Body, release: release}
	return resp, usedReq, usedBody, nil
}
