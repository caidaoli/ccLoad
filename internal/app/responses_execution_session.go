package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/config"
)

const (
	responsesExecutionSessionCleanupInterval       = time.Minute
	responsesExecutionSessionDetachedTransportTTL  = 5 * time.Minute
	defaultResponsesExecutionSessionLimit          = config.DefaultResponsesWebsocketMaxSessions
	defaultResponsesExecutionSessionTTL            = config.DefaultResponsesWebsocketSessionTTLMinutes
	defaultResponsesExecutionTranscriptBudgetBytes = config.DefaultResponsesWebsocketMaxTranscriptBytes
	responsesWebsocketMaxSessionsSetting           = "responses_ws_max_sessions"
	responsesWebsocketSessionTTLSetting            = "responses_ws_session_ttl_minutes"
	responsesWebsocketMaxTranscriptBytesSetting    = "responses_ws_max_transcript_bytes"
)

var (
	errResponsesExecutionSessionCapacity         = errors.New("responses execution session capacity exceeded")
	errResponsesExecutionSessionTranscriptBudget = errors.New("responses execution transcript budget exceeded")
)

type responsesExecutionLimitError struct {
	cause   error
	setting string
	current int64
	limit   int64
	unit    string
}

func (e *responsesExecutionLimitError) Error() string {
	switch {
	case errors.Is(e.cause, errResponsesExecutionSessionCapacity):
		return fmt.Sprintf(
			"Responses WebSocket execution session limit reached (current: %d, limit: %d). Reuse an existing Session-Id, wait for an idle session to expire, or increase %s.",
			e.current,
			e.limit,
			e.setting,
		)
	case errors.Is(e.cause, errResponsesExecutionSessionTranscriptBudget):
		return fmt.Sprintf(
			"Responses WebSocket transcript budget exceeded (current: %s, limit: %s). Disconnect unused sessions and wait for them to expire, or increase %s.",
			formatResponsesWebsocketByteCount(e.current),
			formatResponsesWebsocketByteCount(e.limit),
			e.setting,
		)
	default:
		return e.cause.Error()
	}
}

func formatResponsesWebsocketByteCount(value int64) string {
	unit := "bytes"
	if value == 1 {
		unit = "byte"
	}
	return fmt.Sprintf("%d %s", value, unit)
}

func (e *responsesExecutionLimitError) Unwrap() error {
	return e.cause
}

// responsesExecutionSession owns conversation state. Neither transcript nor
// upstream transport belongs to a particular downstream TCP/WebSocket connection.
type responsesExecutionSession struct {
	turn               chan struct{}
	transcript         *responsesWebsocketSession
	upstream           *codexUpstreamWebsocketSession
	routeMu            sync.RWMutex
	preferredChannelID int64
	lastAccess         time.Time
	active             int
	storeKey           string
	transient          bool

	subjectFingerprint string
	sessionFingerprint string

	transcriptBytes            atomic.Int64
	codexMultiAgentV2Optimized atomic.Bool
}

func (s *responsesExecutionSession) codexMultiAgentV2StateSnapshot() bool {
	if s == nil {
		return false
	}
	return s.codexMultiAgentV2Optimized.Load()
}

func (s *responsesExecutionSession) setCodexMultiAgentV2State(optimized bool) {
	if s == nil {
		return
	}
	s.codexMultiAgentV2Optimized.Store(optimized)
}

func newResponsesExecutionSession(
	now time.Time,
	maxBodyBytes int64,
	upstreamConnectionMaxAge time.Duration,
	websocketFailures *codexWebsocketFailureTracker,
) *responsesExecutionSession {
	return &responsesExecutionSession{
		turn:       make(chan struct{}, 1),
		transcript: newResponsesWebsocketSession(maxBodyBytes),
		upstream: newCodexUpstreamWebsocketSession(
			maxBodyBytes,
			upstreamConnectionMaxAge,
			websocketFailures,
		),
		lastAccess: now,
	}
}

func (s *responsesExecutionSession) acquireTurn(ctx context.Context) error {
	select {
	case s.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *responsesExecutionSession) releaseTurn() {
	<-s.turn
}

func (s *responsesExecutionSession) preferredChannelSnapshot() (int64, bool) {
	if s == nil {
		return 0, false
	}
	s.routeMu.RLock()
	defer s.routeMu.RUnlock()
	return s.preferredChannelID, s.preferredChannelID > 0
}

func (s *responsesExecutionSession) routeChannelSnapshot() (int64, bool) {
	if channelID, ok := s.preferredChannelSnapshot(); ok {
		return channelID, true
	}
	if s == nil {
		return 0, false
	}
	target, ok := s.upstream.affinitySnapshot()
	return target.channelID, ok && target.channelID > 0
}

func (s *responsesExecutionSession) rememberPreferredChannel(channelID int64) {
	if s == nil || channelID <= 0 {
		return
	}
	s.routeMu.Lock()
	if s.preferredChannelID == 0 {
		s.preferredChannelID = channelID
	}
	s.routeMu.Unlock()
}

func (s *responsesExecutionSession) close() {
	s.upstream.Close()
}

func (s *responsesExecutionSession) commit(request []byte, result responsesWebsocketTurnResult) {
	s.transcript.commit(request, result)
	s.transcriptBytes.Store(int64(len(s.transcript.lastRequest) + len(s.transcript.lastResponseOutput)))
}

type responsesExecutionSessionStoreStats struct {
	Sessions                         int    `json:"sessions"`
	ActiveAttachments                int    `json:"active_attachments"`
	DownstreamConnections            int    `json:"downstream_connections"`
	RejectedDownstreamConnections    uint64 `json:"rejected_downstream_connections"`
	MaxDownstreamConnections         int    `json:"max_downstream_connections"`
	MaxDownstreamConnectionsPerToken int    `json:"max_downstream_connections_per_token"`
	UpstreamConnections              int    `json:"upstream_connections"`
	UpstreamHandshakes               uint64 `json:"upstream_handshakes"`
	UpstreamReuses                   uint64 `json:"upstream_reuses"`
	UpstreamHeartbeatFailures        uint64 `json:"upstream_heartbeat_failures"`
	UpstreamQueuedReadBytes          int64  `json:"upstream_queued_read_bytes"`
	OldestUpstreamConnectionSeconds  int64  `json:"oldest_upstream_connection_seconds"`
	Reconnects                       uint64 `json:"reconnects"`
	TranscriptBytes                  int64  `json:"transcript_bytes"`
	MaxTranscriptBytes               int64  `json:"max_transcript_bytes"`
	MaxSessions                      int    `json:"max_sessions"`
	TTLExpired                       uint64 `json:"ttl_expired"`
	CapacityRejected                 uint64 `json:"capacity_rejected"`
	BudgetRejected                   uint64 `json:"budget_rejected"`
	PreviousResponseMisses           uint64 `json:"previous_response_misses"`
}

// responsesExecutionSessionStore is a single-process, in-memory session map.
// Single instance only: no cross-process coordination, no persistence, no
// restart recovery. A process restart drops every session; downstream clients
// reconnect and resend the full transcript, which is the documented contract
// of the WebSocket protocol this store backs.
type responsesExecutionSessionStore struct {
	mu                       sync.Mutex
	sessions                 map[string]*responsesExecutionSession
	configService            *ConfigService
	ttlOverride              time.Duration // non-zero overrides configService (tests only)
	maxSessions              int
	maxTranscriptBytes       int64
	maxBodyBytes             int64
	upstreamConnectionMaxAge time.Duration
	websocketFailures        *codexWebsocketFailureTracker
	nextTransientID          uint64
	ttlExpired               uint64
	capacityRejected         uint64
	budgetRejected           uint64
	previousResponseMisses   uint64
}

func newResponsesExecutionSessionStore(
	cfg *ConfigService,
	maxBodyBytes int64,
	upstreamConnectionMaxAge time.Duration,
) *responsesExecutionSessionStore {
	if maxBodyBytes <= 0 {
		maxBodyBytes = normalizeMaxBodyBytes(maxBodyBytes)
	}
	return &responsesExecutionSessionStore{
		sessions:                 make(map[string]*responsesExecutionSession),
		configService:            cfg,
		maxSessions:              defaultResponsesExecutionSessionLimit,
		maxTranscriptBytes:       defaultResponsesExecutionTranscriptBudgetBytes,
		maxBodyBytes:             maxBodyBytes,
		upstreamConnectionMaxAge: upstreamConnectionMaxAge,
		websocketFailures:        newCodexWebsocketFailureTracker(),
	}
}

func (s *responsesExecutionSessionStore) sessionTTL() time.Duration {
	if s.ttlOverride > 0 {
		return s.ttlOverride
	}
	minutes := defaultResponsesExecutionSessionTTL
	if s.configService != nil {
		minutes = s.configService.GetInt(responsesWebsocketSessionTTLSetting, defaultResponsesExecutionSessionTTL)
		if minutes <= 0 {
			minutes = defaultResponsesExecutionSessionTTL
		}
	}
	ttl, valid := settingDurationFromInt64(int64(minutes), time.Minute)
	if !valid || ttl == 0 {
		return time.Duration(defaultResponsesExecutionSessionTTL) * time.Minute
	}
	return ttl
}

func (s *responsesExecutionSessionStore) maxSessionsLimit() int {
	if s.configService != nil {
		n := s.configService.GetInt(responsesWebsocketMaxSessionsSetting, s.maxSessions)
		if n > 0 {
			return n
		}
	}
	return s.maxSessions
}

func (s *responsesExecutionSessionStore) transcriptBudgetLimit() int64 {
	limit := s.maxTranscriptBytes
	if s.configService != nil {
		n := s.configService.GetInt(responsesWebsocketMaxTranscriptBytesSetting, int(limit))
		if n > 0 {
			return int64(n)
		}
	}
	return limit
}

func (s *responsesExecutionSessionStore) commit(
	session *responsesExecutionSession,
	request []byte,
	result responsesWebsocketTurnResult,
) {
	if session == nil {
		return
	}
	session.commit(request, result)

	s.mu.Lock()
	evicted, _ := s.trimTranscriptBudgetLocked()
	s.mu.Unlock()
	closeResponsesExecutionSessions(evicted)
}

// responsesExecutionSessionID returns the explicit local execution identity.
// Codex uses Session-Id for the top-level session and Thread-Id for independent
// parent/subagent threads. Clients without Thread-Id retain the Session-Id-only
// contract. Body fields such as session_id and prompt_cache_key remain upstream
// cache-routing signals and must never own mutable transcript state.
func responsesExecutionSessionID(header http.Header) string {
	sessionID := strings.TrimSpace(header.Get("Session-Id"))
	if sessionID == "" {
		return ""
	}
	threadID := strings.TrimSpace(header.Get("Thread-Id"))
	if threadID == "" {
		return sessionID
	}
	return sessionID + "\x00thread\x00" + threadID
}

func responsesExecutionSessionKey(subject, sessionID string) string {
	sum := sha256.Sum256([]byte(subject + "\x00" + sessionID))
	return hex.EncodeToString(sum[:])
}

func responsesExecutionFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func logResponsesExecutionSessionEvent(
	reason string,
	subjectFingerprint string,
	sessionFingerprint string,
	responseFingerprint string,
) {
	log.Printf(
		"[WARN] Responses execution session event reason=%s subject_fingerprint=%s session_fingerprint=%s response_fingerprint=%s",
		reason,
		subjectFingerprint,
		sessionFingerprint,
		responseFingerprint,
	)
}

func logResponsesExecutionSessionRemovals(reason string, sessions []*responsesExecutionSession) {
	for _, session := range sessions {
		logResponsesExecutionSessionEvent(
			reason,
			session.subjectFingerprint,
			session.sessionFingerprint,
			responsesExecutionFingerprint(session.transcript.lastResponseID),
		)
	}
}

// acquire returns a private transient session unless the client supplied an
// explicit stable Session-Id. This prevents unrelated requests sharing a model or IP
// from ever sharing conversation state.
//
// Stable sessions are protocol state, not cache entries. Capacity and transcript
// pressure may reclaim only transient sessions; a stable session lives until its
// TTL expires or the process exits.
func (s *responsesExecutionSessionStore) acquire(subject, sessionID string) (*responsesExecutionSession, func(), error) {
	now := time.Now()
	subject = strings.TrimSpace(subject)
	sessionID = strings.TrimSpace(sessionID)
	stable := subject != "" && sessionID != ""
	key := ""
	if stable {
		key = responsesExecutionSessionKey(subject, sessionID)
	}

	s.mu.Lock()
	expired := s.removeExpiredLocked(now)
	s.closeDetachedTransportsLocked(now)
	var session *responsesExecutionSession
	if stable {
		session = s.sessions[key]
	}
	evicted, overBudget := s.trimTranscriptBudgetLocked()
	if overBudget {
		s.budgetRejected++
		limitErr := &responsesExecutionLimitError{
			cause:   errResponsesExecutionSessionTranscriptBudget,
			setting: responsesWebsocketMaxTranscriptBytesSetting,
			current: s.transcriptBytesLocked(),
			limit:   s.transcriptBudgetLimit(),
			unit:    "bytes",
		}
		s.mu.Unlock()
		logResponsesExecutionSessionRemovals("ttl_expired", expired)
		closeResponsesExecutionSessions(append(expired, evicted...))
		logResponsesExecutionSessionEvent(
			"capacity_budget",
			responsesExecutionFingerprint(subject),
			responsesExecutionFingerprint(sessionID),
			"none",
		)
		return nil, nil, limitErr
	}
	if session == nil {
		if limit := s.maxSessionsLimit(); limit > 0 && len(s.sessions) >= limit {
			victim := s.evictIdleLocked(true)
			if victim == nil {
				s.capacityRejected++
				limitErr := &responsesExecutionLimitError{
					cause:   errResponsesExecutionSessionCapacity,
					setting: responsesWebsocketMaxSessionsSetting,
					current: int64(len(s.sessions)),
					limit:   int64(limit),
					unit:    "sessions",
				}
				s.mu.Unlock()
				logResponsesExecutionSessionRemovals("ttl_expired", expired)
				closeResponsesExecutionSessions(append(expired, evicted...))
				logResponsesExecutionSessionEvent(
					"capacity_session",
					responsesExecutionFingerprint(subject),
					responsesExecutionFingerprint(sessionID),
					"none",
				)
				return nil, nil, limitErr
			}
			evicted = append(evicted, victim)
		}
		if !stable {
			s.nextTransientID++
			key = "transient:" + strconv.FormatUint(s.nextTransientID, 10)
		}
		session = newResponsesExecutionSession(
			now,
			s.maxBodyBytes,
			s.upstreamConnectionMaxAge,
			s.websocketFailures,
		)
		session.storeKey = key
		session.transient = !stable
		session.subjectFingerprint = responsesExecutionFingerprint(subject)
		session.sessionFingerprint = responsesExecutionFingerprint(sessionID)
		s.sessions[key] = session
	}
	session.active++
	session.lastAccess = now
	s.mu.Unlock()
	logResponsesExecutionSessionRemovals("ttl_expired", expired)
	closeResponsesExecutionSessions(append(expired, evicted...))

	var once sync.Once
	return session, func() {
		once.Do(func() {
			var released []*responsesExecutionSession
			s.mu.Lock()
			session.active--
			session.lastAccess = time.Now()
			if session.transient && session.active == 0 && s.sessions[session.storeKey] == session {
				delete(s.sessions, session.storeKey)
				released = append(released, session)
			}
			evicted, _ := s.trimTranscriptBudgetLocked()
			released = append(released, evicted...)
			s.mu.Unlock()
			closeResponsesExecutionSessions(released)
		})
	}, nil
}

func (s *responsesExecutionSessionStore) admitTurn(session *responsesExecutionSession) error {
	if session == nil {
		return nil
	}
	s.mu.Lock()
	evicted, overBudget := s.trimTranscriptBudgetLocked()
	var limitErr *responsesExecutionLimitError
	if overBudget {
		s.budgetRejected++
		limitErr = &responsesExecutionLimitError{
			cause:   errResponsesExecutionSessionTranscriptBudget,
			setting: responsesWebsocketMaxTranscriptBytesSetting,
			current: s.transcriptBytesLocked(),
			limit:   s.transcriptBudgetLimit(),
			unit:    "bytes",
		}
	}
	s.mu.Unlock()
	closeResponsesExecutionSessions(evicted)
	if !overBudget {
		return nil
	}
	logResponsesExecutionSessionEvent(
		"capacity_budget",
		session.subjectFingerprint,
		session.sessionFingerprint,
		responsesExecutionFingerprint(session.transcript.lastResponseID),
	)
	return limitErr
}

func (s *responsesExecutionSessionStore) recordPreviousResponseMiss(
	session *responsesExecutionSession,
	previousResponseID string,
	emptySession bool,
) {
	if session == nil {
		return
	}
	s.mu.Lock()
	s.previousResponseMisses++
	s.mu.Unlock()
	reason := "stale_response_id"
	if emptySession {
		reason = "empty_session"
	}
	logResponsesExecutionSessionEvent(
		reason,
		session.subjectFingerprint,
		session.sessionFingerprint,
		responsesExecutionFingerprint(previousResponseID),
	)
}

func (s *responsesExecutionSessionStore) stats() responsesExecutionSessionStoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	stats := responsesExecutionSessionStoreStats{
		Sessions:               len(s.sessions),
		MaxSessions:            s.maxSessionsLimit(),
		MaxTranscriptBytes:     s.transcriptBudgetLimit(),
		TTLExpired:             s.ttlExpired,
		CapacityRejected:       s.capacityRejected,
		BudgetRejected:         s.budgetRejected,
		PreviousResponseMisses: s.previousResponseMisses,
	}
	for _, session := range s.sessions {
		stats.ActiveAttachments += session.active
		stats.TranscriptBytes += session.transcriptBytes.Load()
		upstream := session.upstream.runtimeStats()
		stats.Reconnects += upstream.reconnects
		stats.UpstreamHandshakes += upstream.handshakes
		stats.UpstreamReuses += upstream.reuses
		stats.UpstreamHeartbeatFailures += upstream.heartbeatFailures
		stats.UpstreamQueuedReadBytes += upstream.queuedReadBytes
		if upstream.connected {
			stats.UpstreamConnections++
			if !upstream.connectedAt.IsZero() {
				age := int64(now.Sub(upstream.connectedAt).Seconds())
				if age > stats.OldestUpstreamConnectionSeconds {
					stats.OldestUpstreamConnectionSeconds = age
				}
			}
		}
	}
	return stats
}

// evictIdleLocked removes and returns the least-recently-used eligible idle
// session. skipStable protects TTL-bound protocol state from resource pressure.
func (s *responsesExecutionSessionStore) evictIdleLocked(skipStable bool) *responsesExecutionSession {
	var victim *responsesExecutionSession
	for _, session := range s.sessions {
		if session.active != 0 || (skipStable && !session.transient) {
			continue
		}
		if victim == nil || session.lastAccess.Before(victim.lastAccess) {
			victim = session
		}
	}
	if victim == nil {
		return nil
	}
	delete(s.sessions, victim.storeKey)
	return victim
}

func (s *responsesExecutionSessionStore) trimTranscriptBudgetLocked() ([]*responsesExecutionSession, bool) {
	limit := s.transcriptBudgetLimit()
	used := s.transcriptBytesLocked()
	var evicted []*responsesExecutionSession
	for used > limit {
		victim := s.evictIdleLocked(true)
		if victim == nil {
			return evicted, true
		}
		used -= victim.transcriptBytes.Load()
		evicted = append(evicted, victim)
	}
	return evicted, false
}

func (s *responsesExecutionSessionStore) transcriptBytesLocked() int64 {
	var total int64
	for _, session := range s.sessions {
		total += session.transcriptBytes.Load()
	}
	return total
}

func (s *responsesExecutionSessionStore) removeExpiredLocked(now time.Time) []*responsesExecutionSession {
	ttl := s.sessionTTL()
	var expired []*responsesExecutionSession
	for key, session := range s.sessions {
		if session.active == 0 && now.Sub(session.lastAccess) >= ttl {
			delete(s.sessions, key)
			expired = append(expired, session)
		}
	}
	s.ttlExpired += uint64(len(expired))
	return expired
}

func (s *responsesExecutionSessionStore) closeDetachedTransportsLocked(now time.Time) {
	for _, session := range s.sessions {
		if session.active != 0 || now.Sub(session.lastAccess) < responsesExecutionSessionDetachedTransportTTL {
			continue
		}
		session.upstream.CloseTransport()
	}
}

func (s *responsesExecutionSessionStore) cleanup(now time.Time) {
	s.mu.Lock()
	expired := s.removeExpiredLocked(now)
	s.closeDetachedTransportsLocked(now)
	evicted, _ := s.trimTranscriptBudgetLocked()
	s.mu.Unlock()
	logResponsesExecutionSessionRemovals("ttl_expired", expired)
	closeResponsesExecutionSessions(append(expired, evicted...))
}

func (s *responsesExecutionSessionStore) close() {
	s.mu.Lock()
	sessions := make([]*responsesExecutionSession, 0, len(s.sessions))
	for key, session := range s.sessions {
		delete(s.sessions, key)
		sessions = append(sessions, session)
	}
	s.mu.Unlock()
	closeResponsesExecutionSessions(sessions)
}

func closeResponsesExecutionSessions(sessions []*responsesExecutionSession) {
	for _, session := range sessions {
		session.close()
	}
}

func (s *Server) responsesExecutionSessionCleanupLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(responsesExecutionSessionCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCh:
			return
		case now := <-ticker.C:
			if s.responsesExecutionSessions != nil {
				s.responsesExecutionSessions.cleanup(now)
			}
		}
	}
}
