package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
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
	defaultResponsesSessionsPerSubjectLimit  = 4
	defaultResponsesAttachmentsPerSession    = 2
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
	subjectKey string
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
	Sessions                 int    `json:"sessions"`
	ActiveAttachments        int    `json:"active_attachments"`
	UpstreamConnections      int    `json:"upstream_connections"`
	Reconnects               uint64 `json:"reconnects"`
	TranscriptBytes          int64  `json:"transcript_bytes"`
	Evictions                uint64 `json:"evictions"`
	Rejections               uint64 `json:"rejections"`
	MaxSessions              int    `json:"max_sessions"`
	MaxSessionsPerSubject    int    `json:"max_sessions_per_subject"`
	MaxAttachmentsPerSession int    `json:"max_attachments_per_session"`
}

type responsesExecutionSessionStore struct {
	mu                       sync.Mutex
	sessions                 map[string]*responsesExecutionSession
	ttl                      time.Duration
	maxSessions              int
	maxSessionsPerSubject    int
	maxAttachmentsPerSession int
	nextTransientID          uint64
	evictions                uint64
	rejections               uint64
}

func newResponsesExecutionSessionStore(ttl time.Duration) *responsesExecutionSessionStore {
	if ttl <= 0 {
		ttl = responsesExecutionSessionTTL
	}
	return &responsesExecutionSessionStore{
		sessions:                 make(map[string]*responsesExecutionSession),
		ttl:                      ttl,
		maxSessions:              defaultResponsesExecutionSessionLimit,
		maxSessionsPerSubject:    defaultResponsesSessionsPerSubjectLimit,
		maxAttachmentsPerSession: defaultResponsesAttachmentsPerSession,
	}
}

func newResponsesExecutionSessionStoreFromEnv() *responsesExecutionSessionStore {
	ttlMinutes := positiveEnvInt("CCLOAD_RESPONSES_WS_SESSION_TTL_MINUTES", int(responsesExecutionSessionTTL/time.Minute))
	store := newResponsesExecutionSessionStore(time.Duration(ttlMinutes) * time.Minute)
	store.maxSessions = positiveEnvInt("CCLOAD_RESPONSES_WS_MAX_SESSIONS", defaultResponsesExecutionSessionLimit)
	store.maxSessionsPerSubject = positiveEnvInt(
		"CCLOAD_RESPONSES_WS_MAX_SESSIONS_PER_TOKEN", defaultResponsesSessionsPerSubjectLimit,
	)
	store.maxAttachmentsPerSession = positiveEnvInt(
		"CCLOAD_RESPONSES_WS_MAX_ATTACHMENTS_PER_SESSION", defaultResponsesAttachmentsPerSession,
	)
	return store
}

func positiveEnvInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Printf("[WARN] %s=%q 无效，使用默认值 %d", name, raw, fallback)
		return fallback
	}
	return value
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

func responsesExecutionSubjectKey(subject string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(subject)))
	return hex.EncodeToString(sum[:])
}

// acquire returns a private transient session unless the client supplied an
// explicit stable hint. This prevents unrelated requests sharing a model or IP
// from ever sharing conversation state.
func (s *responsesExecutionSessionStore) acquire(subject, hint string) (*responsesExecutionSession, func(), error) {
	now := time.Now()
	subject = strings.TrimSpace(subject)
	hint = strings.TrimSpace(hint)
	stable := subject != "" && hint != ""
	subjectKey := responsesExecutionSubjectKey(subject)
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
	if session != nil && s.maxAttachmentsPerSession > 0 && session.active >= s.maxAttachmentsPerSession {
		s.rejections++
		s.mu.Unlock()
		closeResponsesExecutionSessions(expired)
		return nil, nil, errResponsesExecutionSessionCapacity
	}
	if session == nil {
		evicted, ok := s.makeCapacityLocked(subjectKey)
		expired = append(expired, evicted...)
		if !ok {
			s.rejections++
			s.mu.Unlock()
			closeResponsesExecutionSessions(expired)
			return nil, nil, errResponsesExecutionSessionCapacity
		}
		if !stable {
			s.nextTransientID++
			key = "transient:" + responsesExecutionSessionKey(subjectKey, strconv.FormatUint(s.nextTransientID, 10))
		}
		session = newResponsesExecutionSession(now)
		session.storeKey = key
		session.subjectKey = subjectKey
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

func (s *responsesExecutionSessionStore) makeCapacityLocked(subjectKey string) ([]*responsesExecutionSession, bool) {
	var evicted []*responsesExecutionSession
	for s.maxSessionsPerSubject > 0 && s.countSubjectLocked(subjectKey) >= s.maxSessionsPerSubject {
		key, session := s.oldestIdleLocked(subjectKey)
		if session == nil {
			return evicted, false
		}
		delete(s.sessions, key)
		s.evictions++
		evicted = append(evicted, session)
	}
	for s.maxSessions > 0 && len(s.sessions) >= s.maxSessions {
		key, session := s.oldestIdleLocked("")
		if session == nil {
			return evicted, false
		}
		delete(s.sessions, key)
		s.evictions++
		evicted = append(evicted, session)
	}
	return evicted, true
}

func (s *responsesExecutionSessionStore) countSubjectLocked(subjectKey string) int {
	count := 0
	for _, session := range s.sessions {
		if session.subjectKey == subjectKey {
			count++
		}
	}
	return count
}

func (s *responsesExecutionSessionStore) oldestIdleLocked(subjectKey string) (string, *responsesExecutionSession) {
	oldestKey := ""
	var oldest *responsesExecutionSession
	for key, session := range s.sessions {
		if session.active != 0 || (subjectKey != "" && session.subjectKey != subjectKey) {
			continue
		}
		if oldest == nil || session.lastAccess.Before(oldest.lastAccess) ||
			(session.lastAccess.Equal(oldest.lastAccess) && key < oldestKey) {
			oldestKey = key
			oldest = session
		}
	}
	return oldestKey, oldest
}

func (s *responsesExecutionSessionStore) stats() responsesExecutionSessionStoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := responsesExecutionSessionStoreStats{
		Sessions:                 len(s.sessions),
		Evictions:                s.evictions,
		Rejections:               s.rejections,
		MaxSessions:              s.maxSessions,
		MaxSessionsPerSubject:    s.maxSessionsPerSubject,
		MaxAttachmentsPerSession: s.maxAttachmentsPerSession,
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
