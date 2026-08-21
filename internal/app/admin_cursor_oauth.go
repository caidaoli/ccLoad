package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/cursorauth"

	"github.com/gin-gonic/gin"
)

// Hosted Cursor CLI login.
//
// The CLI flow is poll-based: ccLoad opens loginDeepControl, then polls
// api2.cursor.sh/auth/poll until the account approves it. The poll belongs to
// the server lifecycle, not to the admin HTTP request that started it.

const (
	cursorOAuthSessionTTL      = 15 * time.Minute
	cursorOAuthTerminalTTL     = 2 * time.Minute
	cursorOAuthJanitorInterval = 30 * time.Second
)

type cursorOAuthStartResponse struct {
	URL    string `json:"url"`
	State  string `json:"state"`
	Status string `json:"status"`
}

type cursorOAuthStatusResponse struct {
	State       string `json:"state"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	ChannelID   int64  `json:"channel_id,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
}

type cursorOAuthCancelRequest struct {
	State string `json:"state"`
}

func (r *cursorOAuthCancelRequest) Validate() error {
	if strings.TrimSpace(r.State) == "" {
		return errors.New("state is required")
	}
	return nil
}

type cursorOAuthSession struct {
	state            string
	adminSessionHash string
	verifier         string
	ctx              context.Context
	cancel           context.CancelFunc
	status           string
	errorMsg         string
	channelID        int64
	channelName      string
	createdAt        time.Time
	finishedAt       time.Time
}

type cursorOAuthManager struct {
	mu          sync.Mutex
	service     *cursorauth.Service
	commit      func(context.Context, *cursorauth.Credential) (int64, string, error)
	baseCtx     context.Context
	now         func() time.Time
	sleep       func(context.Context, time.Duration)
	sessions    map[string]*cursorOAuthSession
	activeByID  map[string]string
	stopJanitor chan struct{}
	janitorDone chan struct{}
	closeOnce   sync.Once
	closed      bool
	pollers     sync.WaitGroup
}

func newCursorOAuthManager(
	baseCtx context.Context,
	service *cursorauth.Service,
	commit func(context.Context, *cursorauth.Credential) (int64, string, error),
) *cursorOAuthManager {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	manager := &cursorOAuthManager{
		service: service, commit: commit, baseCtx: baseCtx,
		now: time.Now, sleep: sleepWithContext,
		sessions: make(map[string]*cursorOAuthSession), activeByID: make(map[string]string),
		stopJanitor: make(chan struct{}), janitorDone: make(chan struct{}),
	}
	go manager.runJanitor()
	return manager
}

func (m *cursorOAuthManager) start(ctx context.Context, adminSessionHash string) (cursorOAuthStartResponse, error) {
	if m == nil || m.service == nil || m.commit == nil {
		return cursorOAuthStartResponse{}, errors.New("cursor OAuth is unavailable")
	}
	adminSessionHash = strings.TrimSpace(adminSessionHash)
	if adminSessionHash == "" {
		return cursorOAuthStartResponse{}, errors.New("cursor OAuth requires an administrator session")
	}
	flow, err := m.service.InitFlow()
	if err != nil {
		return cursorOAuthStartResponse{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return cursorOAuthStartResponse{}, errors.New("cursor OAuth is unavailable")
	}
	m.pruneExpiredSessionsLocked()
	if old := m.sessions[m.activeByID[adminSessionHash]]; old != nil && cursorOAuthSessionCancelable(old.status) {
		m.finishSessionLocked(old, "cancelled", "", 0, "")
	}
	sessionCtx, cancel := context.WithCancel(m.baseCtx)
	session := &cursorOAuthSession{
		state: flow.UUID, adminSessionHash: adminSessionHash, verifier: flow.Verifier,
		ctx: sessionCtx, cancel: cancel, status: "pending", createdAt: m.now(),
	}
	m.sessions[session.state] = session
	m.activeByID[adminSessionHash] = session.state
	m.pollers.Add(1)
	m.mu.Unlock()

	go m.run(session, flow)
	return cursorOAuthStartResponse{URL: flow.AuthorizeURL, State: session.state, Status: "pending"}, nil
}

func (m *cursorOAuthManager) run(session *cursorOAuthSession, flow *cursorauth.Flow) {
	defer m.pollers.Done()
	interval := cursorauth.PollInterval
	deadline := m.now().Add(cursorauth.PollTimeout)

	for m.now().Before(deadline) {
		if session.ctx.Err() != nil {
			return
		}
		result, err := m.service.Poll(session.ctx, flow.UUID, session.verifier)
		if err != nil {
			if session.ctx.Err() != nil {
				return
			}
			m.finish(session, "error", err.Error(), 0, "")
			return
		}
		switch result.Status {
		case cursorauth.PollReady:
			m.completeAuthorization(session, result)
			return
		case cursorauth.PollFailed:
			m.finish(session, "error", "cursor rejected the authorization", 0, "")
			return
		default:
			m.sleep(session.ctx, interval)
			if interval < 10*time.Second {
				interval = time.Duration(float64(interval) * 1.2)
				if interval > 10*time.Second {
					interval = 10 * time.Second
				}
			}
		}
	}
	if session.ctx.Err() == nil {
		m.finish(session, "error", "cursor authorization timed out", 0, "")
	}
}

func (m *cursorOAuthManager) completeAuthorization(session *cursorOAuthSession, result *cursorauth.PollResult) {
	m.mu.Lock()
	if session.status != "pending" {
		m.mu.Unlock()
		return
	}
	session.status = "committing"
	session.verifier = ""
	if m.activeByID[session.adminSessionHash] == session.state {
		delete(m.activeByID, session.adminSessionHash)
	}
	m.mu.Unlock()

	credential := &cursorauth.Credential{
		Type: cursorauth.ChannelType, AccessToken: result.AccessToken, RefreshToken: result.RefreshToken,
		LastRefresh: m.now().UTC().Format(time.RFC3339),
	}
	identity, name, err := m.service.FetchIdentity(session.ctx, result.AccessToken)
	if err != nil {
		m.finish(session, "error", err.Error(), 0, "")
		return
	}
	credential.UserID, credential.Email, credential.Name = identity.UserID, identity.Email, name
	if err := credential.Normalize(); err != nil {
		m.finish(session, "error", err.Error(), 0, "")
		return
	}
	channelID, channelName, err := m.commit(session.ctx, credential)
	if err != nil {
		m.finish(session, "error", err.Error(), 0, "")
		return
	}
	log.Printf("[INFO] Cursor CLI 渠道已就绪: channel_id=%d", channelID)
	m.finish(session, "complete", "", channelID, channelName)
}

func (m *cursorOAuthManager) finish(session *cursorOAuthSession, status, errorMsg string, channelID int64, channelName string) {
	m.mu.Lock()
	if session.status != "cancelled" {
		m.finishSessionLocked(session, status, errorMsg, channelID, channelName)
	}
	m.mu.Unlock()
}

func (m *cursorOAuthManager) status(adminSessionHash, state string) (cursorOAuthStatusResponse, bool) {
	if m == nil {
		return cursorOAuthStatusResponse{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredSessionsLocked()
	session := m.sessions[strings.TrimSpace(state)]
	if session == nil || session.adminSessionHash != strings.TrimSpace(adminSessionHash) {
		return cursorOAuthStatusResponse{}, false
	}
	return m.statusLocked(session), true
}

func (m *cursorOAuthManager) statusLocked(session *cursorOAuthSession) cursorOAuthStatusResponse {
	return cursorOAuthStatusResponse{
		State: session.state, Status: session.status, Error: session.errorMsg,
		ChannelID: session.channelID, ChannelName: session.channelName,
	}
}

func (m *cursorOAuthManager) cancel(adminSessionHash, state string) error {
	if m == nil {
		return errors.New("cursor OAuth session not found")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[strings.TrimSpace(state)]
	if session == nil || session.adminSessionHash != strings.TrimSpace(adminSessionHash) {
		return errors.New("cursor OAuth session not found")
	}
	if !cursorOAuthSessionCancelable(session.status) {
		return fmt.Errorf("cursor OAuth session cannot be cancelled while %s", session.status)
	}
	m.finishSessionLocked(session, "cancelled", "", 0, "")
	return nil
}

func cursorOAuthSessionCancelable(status string) bool {
	return status == "pending"
}

func (m *cursorOAuthManager) finishSessionLocked(
	session *cursorOAuthSession,
	status, errorMsg string,
	channelID int64,
	channelName string,
) {
	session.status = status
	session.errorMsg = errorMsg
	session.channelID = channelID
	session.channelName = channelName
	session.verifier = ""
	session.finishedAt = m.now()
	session.cancel()
	if m.activeByID[session.adminSessionHash] == session.state {
		delete(m.activeByID, session.adminSessionHash)
	}
}

func (m *cursorOAuthManager) pruneExpiredSessionsLocked() {
	now := m.now()
	for state, session := range m.sessions {
		if session.finishedAt.IsZero() && cursorOAuthSessionCancelable(session.status) &&
			now.Sub(session.createdAt) > cursorOAuthSessionTTL {
			m.finishSessionLocked(session, "error", "cursor OAuth session expired", 0, "")
		}
		if !session.finishedAt.IsZero() && now.Sub(session.finishedAt) > cursorOAuthTerminalTTL {
			delete(m.sessions, state)
		}
	}
}

func (m *cursorOAuthManager) runJanitor() {
	ticker := time.NewTicker(cursorOAuthJanitorInterval)
	defer func() {
		ticker.Stop()
		close(m.janitorDone)
	}()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			m.pruneExpiredSessionsLocked()
			m.mu.Unlock()
		case <-m.stopJanitor:
			return
		}
	}
}

func (m *cursorOAuthManager) close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		close(m.stopJanitor)
		m.mu.Lock()
		m.closed = true
		cancels := make([]context.CancelFunc, 0, len(m.sessions))
		for _, session := range m.sessions {
			session.verifier = ""
			cancels = append(cancels, session.cancel)
		}
		m.sessions = make(map[string]*cursorOAuthSession)
		m.activeByID = make(map[string]string)
		m.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		m.pollers.Wait()
		<-m.janitorDone
	})
}

// HandleStartCursorOAuth opens one hosted Cursor CLI authorization.
func (s *Server) HandleStartCursorOAuth(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok {
		RespondErrorMsg(c, http.StatusUnauthorized, "administrator session is unavailable")
		return
	}
	if s.cursorOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "cursor OAuth is unavailable")
		return
	}
	response, err := s.cursorOAuth.start(c.Request.Context(), adminSessionHash)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, response)
}

// HandleCursorOAuthStatus returns only sessions owned by the current administrator.
func (s *Server) HandleCursorOAuthStatus(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok || s.cursorOAuth == nil {
		RespondErrorMsg(c, http.StatusNotFound, "cursor OAuth session not found")
		return
	}
	status, exists := s.cursorOAuth.status(adminSessionHash, c.Query("state"))
	if !exists {
		RespondErrorMsg(c, http.StatusNotFound, "cursor OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelCursorOAuth cancels one pending administrator-owned session.
func (s *Server) HandleCancelCursorOAuth(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok || s.cursorOAuth == nil {
		RespondErrorMsg(c, http.StatusNotFound, "cursor OAuth session not found")
		return
	}
	var request cursorOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if err := s.cursorOAuth.cancel(adminSessionHash, request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, cursorOAuthStatusResponse{State: strings.TrimSpace(request.State), Status: "cancelled"})
}
