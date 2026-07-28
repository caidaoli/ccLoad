package app

import (
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
	codexWebsocketReadQueueFrameLimit = 64
)

const codexInputItemIDLimit = 64

var errCodexWebsocketReadQueueOverflow = errors.New("codex websocket read queue overflow")

type codexWebsocketTimeouts struct {
	idle time.Duration
	ping time.Duration
}

type codexWebsocketTarget struct {
	channelID     int64
	keyHash       [sha256.Size]byte
	headerHash    [sha256.Size]byte
	transportHash [sha256.Size]byte
	url           string
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
	turnMu       sync.Mutex
	writeMu      sync.Mutex
	mu           sync.Mutex
	conn         *websocket.Conn
	connDone     chan struct{}
	target       codexWebsocketTarget
	affinity     codexWebsocketTarget
	hasAffinity  bool
	reads        []codexWebsocketRead
	readBytes    int64
	readNotify   chan struct{}
	maxBodyBytes int64

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
}

func newCodexUpstreamWebsocketSession(maxBodyBytes int64) *codexUpstreamWebsocketSession {
	if maxBodyBytes <= 0 {
		maxBodyBytes = config.DefaultMaxBodyBytes
	}
	return &codexUpstreamWebsocketSession{
		readNotify:   make(chan struct{}, 1),
		maxBodyBytes: maxBodyBytes,
	}
}

type codexWebsocketRead struct {
	conn        *websocket.Conn
	messageType int
	payload     []byte
	err         error
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

func (s *codexUpstreamWebsocketSession) enqueueRead(event codexWebsocketRead) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != event.conn {
		return false
	}
	eventBytes := int64(len(event.payload))
	maxQueueBytes := s.maxBodyBytes
	if len(s.reads) >= codexWebsocketReadQueueFrameLimit ||
		eventBytes > maxQueueBytes-s.readBytes {
		overflow := codexWebsocketRead{conn: event.conn, err: errCodexWebsocketReadQueueOverflow}
		s.reads = []codexWebsocketRead{overflow}
		s.readBytes = 0
		if s.connDone != nil {
			close(s.connDone)
			s.connDone = nil
		}
		_ = event.conn.Close()
		s.conn = nil
		s.target = codexWebsocketTarget{}
		s.connectedAt = time.Time{}
		s.lastCloseReason = errCodexWebsocketReadQueueOverflow.Error()
		s.signalReadLocked()
		return false
	}
	s.reads = append(s.reads, event)
	s.readBytes += eventBytes
	s.signalReadLocked()
	return true
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
	if len(s.reads) > 0 {
		// Any data between turns is either a recorded disconnect or an unsolicited
		// response frame. Neither is safe to associate with the next request.
		s.closeLocked()
		return nil, false
	}
	s.reuses++
	return s.conn, true
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
				if s.enqueueRead(codexWebsocketRead{conn: conn, err: err}) {
					s.detachReadFailure(conn, err, false)
				}
				return
			}
			messageType, payload, err := conn.ReadMessage()
			if !s.enqueueRead(codexWebsocketRead{
				conn: conn, messageType: messageType, payload: payload, err: err,
			}) {
				return
			}
			if err != nil {
				s.detachReadFailure(conn, err, isCodexWebsocketHeartbeatFailure(err))
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
					s.detachReadFailure(conn, fmt.Errorf("ping upstream websocket: %w", err), true)
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

func (s *codexUpstreamWebsocketSession) detachReadFailure(
	conn *websocket.Conn,
	cause error,
	heartbeatFailure bool,
) {
	s.mu.Lock()
	if s.conn == conn {
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
	switch eventType {
	case "", "response.created", "response.queued", "response.in_progress":
		return false
	default:
		return true
	}
}

func (s *codexUpstreamWebsocketSession) invalidate(conn *websocket.Conn) {
	s.mu.Lock()
	if s.conn == conn {
		s.closeLocked()
	}
	s.mu.Unlock()
}

func codexWebsocketTargetForRequest(cfg *model.Config, req *http.Request, skipTLSVerify bool) codexWebsocketTarget {
	return codexWebsocketTarget{
		channelID:     cfg.ID,
		keyHash:       sha256.Sum256([]byte(req.Header.Get("Authorization"))),
		headerHash:    codexWebsocketHeaderHash(codexWebsocketHeaders(req.Header)),
		transportHash: codexWebsocketTransportHash(cfg, skipTLSVerify),
		url:           req.URL.String(),
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
	if !gjson.ValidBytes(body) {
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
		if itemID.Type == gjson.String && len([]rune(itemID.String())) <= codexInputItemIDLimit {
			occupied[itemID.String()] = struct{}{}
		}
	}

	mapped := make(map[string]string, len(items))
	rebuilt := make([]string, 0, len(items))
	changed := false
	for _, item := range items {
		if shouldDropCodexEncryptedReasoningItem(item) {
			changed = true
			continue
		}
		raw := item.Raw
		itemID := item.Get("id")
		if itemID.Type == gjson.String && len([]rune(itemID.String())) > codexInputItemIDLimit {
			original := itemID.String()
			shortened, ok := mapped[original]
			if !ok {
				for attempt := 0; ; attempt++ {
					shortened = shortenCodexInputItemID(original, attempt)
					if _, exists := occupied[shortened]; !exists {
						break
					}
				}
				mapped[original] = shortened
				occupied[shortened] = struct{}{}
			}
			if updated, errSet := sjson.SetBytes([]byte(raw), "id", shortened); errSet == nil {
				raw = string(updated)
				changed = true
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
	if len(runes) <= codexInputItemIDLimit {
		return id
	}
	hashInput := id
	if attempt > 0 {
		hashInput += "\x00" + fmt.Sprint(attempt)
	}
	sum := sha256.Sum256([]byte(hashInput))
	suffix := fmt.Sprintf("_%x", sum[:8])
	return string(runes[:codexInputItemIDLimit-len(suffix)]) + suffix
}

func codexWebsocketHeaders(source http.Header) http.Header {
	header := source.Clone()
	for key := range header {
		lower := strings.ToLower(key)
		if lower == "connection" || lower == "upgrade" || lower == "content-length" ||
			strings.HasPrefix(lower, "sec-websocket-") {
			header.Del(key)
		}
	}
	if !strings.Contains(header.Get("OpenAI-Beta"), "responses_websockets=") {
		if existing := strings.TrimSpace(header.Get("OpenAI-Beta")); existing != "" {
			header.Set("OpenAI-Beta", existing+","+codexResponsesWebsocketBeta)
		} else {
			header.Set("OpenAI-Beta", codexResponsesWebsocketBeta)
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

func (b *codexWebsocketResponseBody) Close() error {
	if !b.completed.Load() && b.abort != nil {
		b.abort()
	}
	return b.PipeReader.Close()
}

func writeSyntheticSSEFrame(w io.Writer, payload []byte) error {
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := w.Write([]byte("\n\n"))
	return err
}

func (s *codexUpstreamWebsocketSession) reconnectWithReplay(
	ctx context.Context,
	dialer *websocket.Dialer,
	target codexWebsocketTarget,
	replayReq *http.Request,
	replayBody []byte,
	timeouts codexWebsocketTimeouts,
) (*websocket.Conn, error) {
	prepared, err := buildCodexWebsocketRequestBody(replayBody)
	if err != nil {
		return nil, err
	}
	conn, resp, err := s.dial(ctx, dialer, target, replayReq, timeouts)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if resp != nil || isCodexWebsocketHandshakeFallbackError(err) {
			return nil, &codexWebsocketHTTPFallbackError{cause: err}
		}
		return nil, err
	}
	if err = s.writeRequest(conn, prepared); err != nil {
		s.invalidate(conn)
		return nil, err
	}
	return conn, nil
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
) *http.Response {
	reader, writer := io.Pipe()
	body := &codexWebsocketResponseBody{PipeReader: reader, abort: func() { s.invalidate(conn) }}
	go func() {
		defer s.turnMu.Unlock()
		stopCancel := context.AfterFunc(ctx, func() {
			if !body.completed.Load() {
				s.invalidate(conn)
			}
		})
		defer stopCancel()

		semanticOutput := false
		retried := false
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
					connRetry, errRetry := s.reconnectWithReplay(
						ctx, dialer, target, replayReq, replayBody, timeouts,
					)
					if errRetry == nil {
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
				connRetry, errRetry := s.reconnectWithReplay(
					ctx, dialer, target, replayReq, replayBody, timeouts,
				)
				if errRetry == nil {
					conn = connRetry
					continue
				}
				_ = writer.CloseWithError(errRetry)
				return
			}
			if eventType == "response.completed" || eventType == "response.done" || eventType == "response.incomplete" {
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

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
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
) (resp *http.Response, usedReq *http.Request, usedBody []byte, err error) {
	s.turnMu.Lock()
	target := codexWebsocketTargetForRequest(cfg, replayReq, skipTLSVerify)

	conn, reuse := s.connectionReusable(target)
	s.mu.Lock()
	if !reuse {
		s.closeLocked()
	}
	s.mu.Unlock()

	usedReq, usedBody = replayReq, replayBody
	if reuse && incrementalReq != nil && len(incrementalBody) > 0 {
		usedReq, usedBody = incrementalReq, incrementalBody
	}
	preparedBody, err := buildCodexWebsocketRequestBody(usedBody)
	if err != nil {
		s.turnMu.Unlock()
		return nil, usedReq, usedBody, err
	}

	if !reuse {
		conn, resp, err = s.dial(ctx, dialer, target, replayReq, timeouts)
		if err != nil {
			s.turnMu.Unlock()
			if resp != nil {
				if resp.Body == nil {
					resp.Body = io.NopCloser(strings.NewReader(""))
				}
				return resp, usedReq, usedBody, nil
			}
			return nil, usedReq, usedBody, err
		}
	}

	err = s.writeRequest(conn, preparedBody)
	if err != nil {
		s.invalidate(conn)
		s.recordReconnect("write: " + err.Error())
		preparedReplay, errPrepare := buildCodexWebsocketRequestBody(replayBody)
		if errPrepare == nil {
			connRetry, respRetry, errDial := s.dial(ctx, dialer, target, replayReq, timeouts)
			if errDial == nil {
				if errWrite := s.writeRequest(connRetry, preparedReplay); errWrite == nil {
					return s.streamResponse(
						ctx, connRetry, replayReq, dialer, target, replayReq, replayBody, timeouts,
					), replayReq, replayBody, nil
				}
				s.invalidate(connRetry)
			} else if respRetry != nil {
				if respRetry.Body == nil {
					respRetry.Body = io.NopCloser(strings.NewReader(""))
				}
				s.turnMu.Unlock()
				return respRetry, replayReq, replayBody, nil
			} else if isCodexWebsocketHandshakeFallbackError(errDial) {
				s.turnMu.Unlock()
				return nil, replayReq, replayBody, errDial
			}
		}
		s.turnMu.Unlock()
		return nil, usedReq, usedBody, err
	}
	return s.streamResponse(
		ctx, conn, usedReq, dialer, target, replayReq, replayBody, timeouts,
	), usedReq, usedBody, nil
}

func (s *Server) doCodexWebsocketRequest(
	ctx context.Context,
	cfg *model.Config,
	session *codexUpstreamWebsocketSession,
	replayReq *http.Request,
	replayBody []byte,
	incrementalReq *http.Request,
	incrementalBody []byte,
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
