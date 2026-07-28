package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	responsesExecutionSessionCleanupInterval = 15 * time.Minute
	defaultResponsesExecutionSessionLimit    = 32
	defaultResponsesExecutionSessionTTL      = 60 // minutes
)

var errResponsesExecutionSessionCapacity = errors.New("responses execution session capacity exceeded")

// responsesExecutionSession owns conversation state. Neither transcript nor
// upstream transport belongs to a particular downstream TCP/WebSocket connection.
type responsesExecutionSession struct {
	turn       chan struct{}
	transcript *responsesWebsocketSession
	upstream   *codexUpstreamWebsocketSession
	lastAccess time.Time
	active     int
	storeKey   string
	transient  bool

	transcriptBytes atomic.Int64
}

func newResponsesExecutionSession(now time.Time) *responsesExecutionSession {
	return &responsesExecutionSession{
		turn:       make(chan struct{}, 1),
		transcript: newResponsesWebsocketSession(),
		upstream:   newCodexUpstreamWebsocketSession(),
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

func (s *responsesExecutionSession) close() {
	s.upstream.Close()
}

func (s *responsesExecutionSession) commit(request []byte, result responsesWebsocketTurnResult) {
	s.transcript.commit(request, result)
	s.transcriptBytes.Store(int64(len(s.transcript.lastRequest) + len(s.transcript.lastResponseOutput)))
}

type responsesExecutionSessionStoreStats struct {
	Sessions            int    `json:"sessions"`
	ActiveAttachments   int    `json:"active_attachments"`
	UpstreamConnections int    `json:"upstream_connections"`
	Reconnects          uint64 `json:"reconnects"`
	TranscriptBytes     int64  `json:"transcript_bytes"`
	MaxSessions         int    `json:"max_sessions"`
}

// responsesExecutionSessionStore is a single-process, in-memory session map.
// Single instance only: no cross-process coordination, no persistence, no
// restart recovery. A process restart drops every session; downstream clients
// reconnect and resend the full transcript, which is the documented contract
// of the WebSocket protocol this store backs.
type responsesExecutionSessionStore struct {
	mu              sync.Mutex
	sessions        map[string]*responsesExecutionSession
	configService   *ConfigService
	ttlOverride     time.Duration // non-zero overrides configService (tests only)
	maxSessions     int
	nextTransientID uint64
}

func newResponsesExecutionSessionStore(cfg *ConfigService) *responsesExecutionSessionStore {
	return &responsesExecutionSessionStore{
		sessions:      make(map[string]*responsesExecutionSession),
		configService: cfg,
		maxSessions:   defaultResponsesExecutionSessionLimit,
	}
}

func (s *responsesExecutionSessionStore) sessionTTL() time.Duration {
	if s.ttlOverride > 0 {
		return s.ttlOverride
	}
	minutes := defaultResponsesExecutionSessionTTL
	if s.configService != nil {
		minutes = s.configService.GetInt("responses_ws_session_ttl_minutes", defaultResponsesExecutionSessionTTL)
		if minutes <= 0 {
			minutes = defaultResponsesExecutionSessionTTL
		}
	}
	return time.Duration(minutes) * time.Minute
}

func (s *responsesExecutionSessionStore) maxSessionsLimit() int {
	if s.configService != nil {
		n := s.configService.GetInt("responses_ws_max_sessions", s.maxSessions)
		if n > 0 {
			return n
		}
	}
	return s.maxSessions
}

// responsesExecutionSessionID returns only the explicit local execution-session
// identity. Session_id and prompt_cache_key are upstream cache-routing signals;
// they must never own mutable transcript state or the per-session turn lock.
func responsesExecutionSessionID(header http.Header) string {
	return strings.TrimSpace(header.Get("Session-Id"))
}

func responsesExecutionSessionKey(subject, sessionID string) string {
	sum := sha256.Sum256([]byte(subject + "\x00" + sessionID))
	return hex.EncodeToString(sum[:])
}

// acquire returns a private transient session unless the client supplied an
// explicit stable Session-Id. This prevents unrelated requests sharing a model or IP
// from ever sharing conversation state.
//
// Capacity is one flat ceiling shared by every subject — single instance, no
// per-subject bookkeeping. Once full, acquire evicts the least-recently-used
// *idle* session (active == 0) before giving up: an idle session is only
// cached transcript state, and the evicted client recovers through the
// documented replay path (resend the full conversation input). Without this,
// one subject's 32 idle sessions would starve every other subject for a full
// TTL. Live sessions (active > 0) are never evicted — when all sessions are
// actively attached, acquire rejects with a clear capacity error.
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
	var session *responsesExecutionSession
	if stable {
		session = s.sessions[key]
	}
	if session == nil {
		if limit := s.maxSessionsLimit(); limit > 0 && len(s.sessions) >= limit {
			evicted := s.evictIdleLocked()
			if evicted == nil {
				s.mu.Unlock()
				closeResponsesExecutionSessions(expired)
				return nil, nil, errResponsesExecutionSessionCapacity
			}
			expired = append(expired, evicted)
		}
		if !stable {
			s.nextTransientID++
			key = "transient:" + strconv.FormatUint(s.nextTransientID, 10)
		}
		session = newResponsesExecutionSession(now)
		session.storeKey = key
		session.transient = !stable
		s.sessions[key] = session
	}
	session.active++
	session.lastAccess = now
	s.mu.Unlock()
	closeResponsesExecutionSessions(expired)

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
			s.mu.Unlock()
			closeResponsesExecutionSessions(released)
		})
	}, nil
}

func (s *responsesExecutionSessionStore) stats() responsesExecutionSessionStoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := responsesExecutionSessionStoreStats{
		Sessions:    len(s.sessions),
		MaxSessions: s.maxSessionsLimit(),
	}
	for _, session := range s.sessions {
		stats.ActiveAttachments += session.active
		stats.TranscriptBytes += session.transcriptBytes.Load()
		connected, reconnects := session.upstream.runtimeStats()
		stats.Reconnects += reconnects
		if connected {
			stats.UpstreamConnections++
		}
	}
	return stats
}

// evictIdleLocked removes and returns the least-recently-used idle session,
// or nil when every session is actively attached. Caller must hold s.mu and
// close the returned session after unlocking.
func (s *responsesExecutionSessionStore) evictIdleLocked() *responsesExecutionSession {
	var victim *responsesExecutionSession
	for _, session := range s.sessions {
		if session.active != 0 {
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

func (s *responsesExecutionSessionStore) removeExpiredLocked(now time.Time) []*responsesExecutionSession {
	ttl := s.sessionTTL()
	var expired []*responsesExecutionSession
	for key, session := range s.sessions {
		if session.active == 0 && now.Sub(session.lastAccess) >= ttl {
			delete(s.sessions, key)
			expired = append(expired, session)
		}
	}
	return expired
}

func (s *responsesExecutionSessionStore) cleanup(now time.Time) {
	s.mu.Lock()
	expired := s.removeExpiredLocked(now)
	s.mu.Unlock()
	closeResponsesExecutionSessions(expired)
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
