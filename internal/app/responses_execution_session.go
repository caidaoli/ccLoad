package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	responsesExecutionSessionTTL             = time.Hour
	responsesExecutionSessionCleanupInterval = 15 * time.Minute
)

// responsesExecutionSession owns conversation state. Neither transcript nor
// upstream transport belongs to a particular downstream TCP/WebSocket connection.
type responsesExecutionSession struct {
	turn       chan struct{}
	transcript *responsesWebsocketSession
	upstream   *codexUpstreamWebsocketSession
	lastAccess time.Time
	active     int
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

type responsesExecutionSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*responsesExecutionSession
	ttl      time.Duration
}

func newResponsesExecutionSessionStore(ttl time.Duration) *responsesExecutionSessionStore {
	if ttl <= 0 {
		ttl = responsesExecutionSessionTTL
	}
	return &responsesExecutionSessionStore{
		sessions: make(map[string]*responsesExecutionSession),
		ttl:      ttl,
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
func (s *responsesExecutionSessionStore) acquire(subject, hint string) (*responsesExecutionSession, func()) {
	now := time.Now()
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(hint) == "" {
		session := newResponsesExecutionSession(now)
		return session, session.close
	}

	key := responsesExecutionSessionKey(subject, hint)
	s.mu.Lock()
	expired := s.removeExpiredLocked(now)
	session := s.sessions[key]
	if session == nil {
		session = newResponsesExecutionSession(now)
		s.sessions[key] = session
	}
	session.active++
	session.lastAccess = now
	s.mu.Unlock()
	closeResponsesExecutionSessions(expired)

	var once sync.Once
	return session, func() {
		once.Do(func() {
			s.mu.Lock()
			session.active--
			session.lastAccess = time.Now()
			s.mu.Unlock()
		})
	}
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
