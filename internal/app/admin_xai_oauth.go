package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
)

const (
	xaiOAuthSessionTTL      = 30 * time.Minute
	xaiOAuthTerminalTTL     = 2 * time.Minute
	xaiOAuthJanitorInterval = 30 * time.Second
)

type xaiOAuthStartResponse struct {
	URL    string `json:"url"`
	State  string `json:"state"`
	Status string `json:"status"`
}

type xaiOAuthStatusResponse struct {
	State     string `json:"state"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	ChannelID int64  `json:"channel_id,omitempty"`
}

type xaiOAuthCancelRequest struct {
	State string `json:"state"`
}

func (r *xaiOAuthCancelRequest) Validate() error {
	if strings.TrimSpace(r.State) == "" {
		return errors.New("state is required")
	}
	return nil
}

type xaiOAuthCallbackRequest struct {
	CallbackURL string `json:"callback_url"`
}

func (r *xaiOAuthCallbackRequest) Validate() error {
	if strings.TrimSpace(r.CallbackURL) == "" {
		return errors.New("callback_url is required")
	}
	return nil
}

type xaiOAuthSession struct {
	state            string
	adminSessionHash string
	codeVerifier     string
	ctx              context.Context
	cancel           context.CancelFunc
	status           string
	errorMsg         string
	channelID        int64
	createdAt        time.Time
	finishedAt       time.Time
}

type xaiOAuthManager struct {
	mu          sync.Mutex
	service     *xaiauth.Service
	complete    func(context.Context, *xaiauth.Credential) (*xaiauth.Credential, error)
	commit      func(context.Context, *xaiauth.Credential) (int64, error)
	baseCtx     context.Context
	now         func() time.Time
	sessions    map[string]*xaiOAuthSession
	activeByID  map[string]string
	stopJanitor chan struct{}
	janitorDone chan struct{}
	closeOnce   sync.Once
	closed      bool
}

func newXAIOAuthManager(
	baseCtx context.Context,
	service *xaiauth.Service,
	complete func(context.Context, *xaiauth.Credential) (*xaiauth.Credential, error),
	commit func(context.Context, *xaiauth.Credential) (int64, error),
) *xaiOAuthManager {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	manager := &xaiOAuthManager{
		service: service, complete: complete, commit: commit, baseCtx: baseCtx, now: time.Now,
		sessions: make(map[string]*xaiOAuthSession), activeByID: make(map[string]string),
		stopJanitor: make(chan struct{}), janitorDone: make(chan struct{}),
	}
	go manager.runJanitor()
	return manager
}

func (m *xaiOAuthManager) start(adminSessionHash string) (xaiOAuthStartResponse, error) {
	if m == nil || m.service == nil || m.complete == nil || m.commit == nil {
		return xaiOAuthStartResponse{}, errors.New("xAI OAuth is unavailable")
	}
	adminSessionHash = strings.TrimSpace(adminSessionHash)
	if adminSessionHash == "" {
		return xaiOAuthStartResponse{}, errors.New("xAI OAuth requires an administrator session")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return xaiOAuthStartResponse{}, errors.New("xAI OAuth is unavailable")
	}
	m.pruneExpiredSessionsLocked()
	authorization, err := m.service.NewAuthorization()
	if err != nil {
		return xaiOAuthStartResponse{}, err
	}
	if old := m.sessions[m.activeByID[adminSessionHash]]; old != nil && xaiOAuthSessionCancelable(old.status) {
		m.cancelSessionLocked(old)
	}
	sessionCtx, cancel := context.WithCancel(m.baseCtx)
	session := &xaiOAuthSession{
		state: authorization.State, adminSessionHash: adminSessionHash,
		codeVerifier: authorization.CodeVerifier, ctx: sessionCtx, cancel: cancel,
		status: "pending", createdAt: m.now(),
	}
	m.sessions[session.state] = session
	m.activeByID[adminSessionHash] = session.state
	return xaiOAuthStartResponse{URL: authorization.URL, State: session.state, Status: "pending"}, nil
}

func (m *xaiOAuthManager) submitCallback(adminSessionHash, rawInput string) (xaiOAuthStatusResponse, error) {
	if m == nil || m.service == nil || m.complete == nil || m.commit == nil {
		return xaiOAuthStatusResponse{}, errors.New("xAI OAuth is unavailable")
	}
	adminSessionHash = strings.TrimSpace(adminSessionHash)
	input := xaiauth.ParseAuthorizationInput(rawInput)
	if strings.TrimSpace(input.Code) == "" {
		return xaiOAuthStatusResponse{}, errors.New("xAI authorization code is required")
	}
	if input.RequiresState && strings.TrimSpace(input.State) == "" {
		return xaiOAuthStatusResponse{}, errors.New("xAI OAuth state is required for callback URLs")
	}

	m.mu.Lock()
	m.pruneExpiredSessionsLocked()
	state := strings.TrimSpace(input.State)
	if state == "" {
		state = m.activeByID[adminSessionHash]
	}
	session := m.sessions[state]
	if session == nil || session.adminSessionHash != adminSessionHash ||
		subtle.ConstantTimeCompare([]byte(state), []byte(session.state)) != 1 {
		m.mu.Unlock()
		return xaiOAuthStatusResponse{}, errors.New("xAI OAuth session not found")
	}
	if session.status != "pending" {
		m.mu.Unlock()
		return xaiOAuthStatusResponse{}, fmt.Errorf("xAI OAuth session is %s", session.status)
	}
	session.status = "processing"
	verifier := session.codeVerifier
	m.mu.Unlock()

	credential, err := m.service.ExchangeCode(session.ctx, input.Code, verifier)
	if err == nil {
		credential, err = m.complete(session.ctx, credential)
	}
	if err != nil {
		m.mu.Lock()
		if session.status != "cancelled" {
			m.finishSessionLocked(session, "error", err.Error(), 0)
		}
		status := m.statusLocked(session)
		m.mu.Unlock()
		return status, err
	}

	m.mu.Lock()
	if session.status == "cancelled" || session.ctx.Err() != nil {
		status := m.statusLocked(session)
		m.mu.Unlock()
		return status, errors.New("xAI OAuth session was cancelled")
	}
	session.status = "committing"
	session.codeVerifier = ""
	if m.activeByID[session.adminSessionHash] == session.state {
		delete(m.activeByID, session.adminSessionHash)
	}
	m.mu.Unlock()

	channelID, err := m.commit(session.ctx, credential)
	m.mu.Lock()
	if err != nil {
		m.finishSessionLocked(session, "error", err.Error(), 0)
	} else {
		m.finishSessionLocked(session, "complete", "", channelID)
	}
	status := m.statusLocked(session)
	m.mu.Unlock()
	return status, err
}

func (m *xaiOAuthManager) status(adminSessionHash, state string) (xaiOAuthStatusResponse, bool) {
	if m == nil {
		return xaiOAuthStatusResponse{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredSessionsLocked()
	session := m.sessions[strings.TrimSpace(state)]
	if session == nil || session.adminSessionHash != strings.TrimSpace(adminSessionHash) {
		return xaiOAuthStatusResponse{}, false
	}
	return m.statusLocked(session), true
}

func (m *xaiOAuthManager) statusLocked(session *xaiOAuthSession) xaiOAuthStatusResponse {
	return xaiOAuthStatusResponse{
		State: session.state, Status: session.status, Error: session.errorMsg, ChannelID: session.channelID,
	}
}

func (m *xaiOAuthManager) cancel(adminSessionHash, state string) error {
	if m == nil {
		return errors.New("xAI OAuth session not found")
	}
	m.mu.Lock()
	session := m.sessions[strings.TrimSpace(state)]
	if session == nil || session.adminSessionHash != strings.TrimSpace(adminSessionHash) {
		m.mu.Unlock()
		return errors.New("xAI OAuth session not found")
	}
	if !xaiOAuthSessionCancelable(session.status) {
		m.mu.Unlock()
		return fmt.Errorf("xAI OAuth session cannot be cancelled while %s", session.status)
	}
	m.cancelSessionLocked(session)
	m.mu.Unlock()
	return nil
}

func (m *xaiOAuthManager) cancelByAdmin(adminSessionHash string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	session := m.sessions[m.activeByID[strings.TrimSpace(adminSessionHash)]]
	if session == nil || !xaiOAuthSessionCancelable(session.status) {
		m.mu.Unlock()
		return
	}
	m.cancelSessionLocked(session)
	m.mu.Unlock()
}

func xaiOAuthSessionCancelable(status string) bool {
	return status == "pending" || status == "processing"
}

func (m *xaiOAuthManager) cancelSessionLocked(session *xaiOAuthSession) {
	m.finishSessionLocked(session, "cancelled", "", 0)
}

func (m *xaiOAuthManager) finishSessionLocked(session *xaiOAuthSession, status, errorMsg string, channelID int64) {
	session.status = status
	session.errorMsg = errorMsg
	session.channelID = channelID
	session.codeVerifier = ""
	session.finishedAt = m.now()
	session.cancel()
	if m.activeByID[session.adminSessionHash] == session.state {
		delete(m.activeByID, session.adminSessionHash)
	}
}

func (m *xaiOAuthManager) pruneExpiredSessionsLocked() {
	now := m.now()
	for state, session := range m.sessions {
		if session.finishedAt.IsZero() && xaiOAuthSessionCancelable(session.status) && now.Sub(session.createdAt) > xaiOAuthSessionTTL {
			m.finishSessionLocked(session, "error", "xAI OAuth session expired", 0)
		}
		if !session.finishedAt.IsZero() && now.Sub(session.finishedAt) > xaiOAuthTerminalTTL {
			delete(m.sessions, state)
		}
	}
}

func (m *xaiOAuthManager) runJanitor() {
	ticker := time.NewTicker(xaiOAuthJanitorInterval)
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

func (m *xaiOAuthManager) close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		close(m.stopJanitor)
		m.mu.Lock()
		m.closed = true
		cancels := make([]context.CancelFunc, 0, len(m.sessions))
		for _, session := range m.sessions {
			session.codeVerifier = ""
			cancels = append(cancels, session.cancel)
		}
		m.sessions = make(map[string]*xaiOAuthSession)
		m.activeByID = make(map[string]string)
		m.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		<-m.janitorDone
	})
}

func xaiAdminSessionHash(c *gin.Context) (string, bool) {
	identity, ok := WebIdentityFromContext(c)
	if !ok || strings.TrimSpace(identity.SessionHash) == "" {
		return "", false
	}
	return identity.SessionHash, true
}

// HandleStartXAIOAuth creates a local PKCE authorization URL without contacting xAI.
func (s *Server) HandleStartXAIOAuth(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok {
		RespondErrorMsg(c, http.StatusUnauthorized, "administrator session is unavailable")
		return
	}
	if s.xaiOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "xAI OAuth is unavailable")
		return
	}
	response, err := s.xaiOAuth.start(adminSessionHash)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, response)
}

// HandleXAIOAuthStatus returns only sessions owned by the current administrator.
func (s *Server) HandleXAIOAuthStatus(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok || s.xaiOAuth == nil {
		RespondErrorMsg(c, http.StatusNotFound, "xAI OAuth session not found")
		return
	}
	status, exists := s.xaiOAuth.status(adminSessionHash, c.Query("state"))
	if !exists {
		RespondErrorMsg(c, http.StatusNotFound, "xAI OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelXAIOAuth cancels one pending administrator-owned session.
func (s *Server) HandleCancelXAIOAuth(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok || s.xaiOAuth == nil {
		RespondErrorMsg(c, http.StatusNotFound, "xAI OAuth session not found")
		return
	}
	var request xaiOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if err := s.xaiOAuth.cancel(adminSessionHash, request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, xaiOAuthStatusResponse{State: strings.TrimSpace(request.State), Status: "cancelled"})
}

// HandleSubmitXAIOAuthCallback accepts a callback URL, query string, or bare code.
func (s *Server) HandleSubmitXAIOAuthCallback(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok || s.xaiOAuth == nil {
		RespondErrorMsg(c, http.StatusNotFound, "xAI OAuth session not found")
		return
	}
	var request xaiOAuthCallbackRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	status, err := s.xaiOAuth.submitCallback(adminSessionHash, request.CallbackURL)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	RespondJSON(c, http.StatusOK, status)
}
