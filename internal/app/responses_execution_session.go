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

	"github.com/tidwall/gjson"
)

const (
	responsesExecutionSessionTTL             = time.Hour
	responsesExecutionSessionCleanupInterval = 15 * time.Minute
	defaultResponsesExecutionSessionLimit    = 32
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
	ttl             time.Duration
	maxSessions     int
	nextTransientID uint64
}

func newResponsesExecutionSessionStore(ttl time.Duration) *responsesExecutionSessionStore {
	if ttl <= 0 {
		ttl = responsesExecutionSessionTTL
	}
	return &responsesExecutionSessionStore{
		sessions:    make(map[string]*responsesExecutionSession),
		ttl:         ttl,
		maxSessions: defaultResponsesExecutionSessionLimit,
	}
}

func responsesExecutionSessionHint(header http.Header, payload []byte) string {
	for _, name := range []string{"Session_id", "Session-Id", "X-Session-Id", "X-Claude-Code-Session-Id"} {
		if hint := strings.TrimSpace(header.Get(name)); hint != "" {
			return hint
		}
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_key").String())
}

func responsesExecutionSessionKey(subject, hint string) string {
	sum := sha256.Sum256([]byte(subject + "\x00" + hint))
	return hex.EncodeToString(sum[:])
}

// acquire returns a private transient session unless the client supplied an
// explicit stable hint. This prevents unrelated requests sharing a model or IP
// from ever sharing conversation state.
//
// Capacity is one flat ceiling shared by every subject — single instance, no
// per-subject bookkeeping. Once full, acquire rejects outright; there is no
// LRU eviction. An idle session already frees itself through the TTL sweep
// below, so eviction-on-insert would only ever fire under sustained overload,
// where silently killing another subject's live session is worse than a
// clear capacity error.
func (s *responsesExecutionSessionStore) acquire(subject, hint string) (*responsesExecutionSession, func(), error) {
	now := time.Now()
	subject = strings.TrimSpace(subject)
	hint = strings.TrimSpace(hint)
	stable := subject != "" && hint != ""
	key := ""
	if stable {
		key = responsesExecutionSessionKey(subject, hint)
	}

	s.mu.Lock()
	expired := s.removeExpiredLocked(now)
	var session *responsesExecutionSession
	if stable {
		session = s.sessions[key]
	}
	if session == nil {
		if s.maxSessions > 0 && len(s.sessions) >= s.maxSessions {
			s.mu.Unlock()
			closeResponsesExecutionSessions(expired)
			return nil, nil, errResponsesExecutionSessionCapacity
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
		MaxSessions: s.maxSessions,
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

func (s *responsesExecutionSessionStore) removeExpiredLocked(now time.Time) []*responsesExecutionSession {
	var expired []*responsesExecutionSession
	for key, session := range s.sessions {
		if session.active == 0 && now.Sub(session.lastAccess) >= s.ttl {
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
