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

	"ccLoad/internal/zaiauth"

	"github.com/gin-gonic/gin"
)

// Hosted Z.ai (ZCode) login.
//
// ZCode's CLI flow is poll-based: ccLoad opens an authorization URL, then polls
// until the account approves it. The poll therefore belongs to the server
// lifecycle, not to the admin HTTP request that started it — closing the
// browser tab or the admin page must not abandon a half-finished login.

const (
	zaiOAuthSessionTTL      = 15 * time.Minute
	zaiOAuthTerminalTTL     = 2 * time.Minute
	zaiOAuthJanitorInterval = 30 * time.Second
)

type zaiOAuthStartResponse struct {
	URL    string `json:"url"`
	State  string `json:"state"`
	Status string `json:"status"`
}

type zaiOAuthStatusResponse struct {
	State       string `json:"state"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	ChannelID   int64  `json:"channel_id,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
}

type zaiOAuthCancelRequest struct {
	State string `json:"state"`
}

func (r *zaiOAuthCancelRequest) Validate() error {
	if strings.TrimSpace(r.State) == "" {
		return errors.New("state is required")
	}
	return nil
}

type zaiOAuthSession struct {
	state            string
	adminSessionHash string
	pollToken        string
	ctx              context.Context
	cancel           context.CancelFunc
	status           string
	errorMsg         string
	channelID        int64
	channelName      string
	createdAt        time.Time
	finishedAt       time.Time
}

type zaiOAuthManager struct {
	mu       sync.Mutex
	service  *zaiauth.Service
	resolve  func(context.Context, string) (*zaiauth.Credential, error)
	commit   func(context.Context, *zaiauth.Credential) (int64, string, error)
	baseCtx  context.Context
	now      func() time.Time
	sleep    func(context.Context, time.Duration)
	sessions map[string]*zaiOAuthSession
	// activeByID keeps one live login per administrator.
	activeByID  map[string]string
	stopJanitor chan struct{}
	janitorDone chan struct{}
	closeOnce   sync.Once
	closed      bool
	pollers     sync.WaitGroup
}

func newZAIOAuthManager(
	baseCtx context.Context,
	service *zaiauth.Service,
	resolve func(context.Context, string) (*zaiauth.Credential, error),
	commit func(context.Context, *zaiauth.Credential) (int64, string, error),
) *zaiOAuthManager {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	manager := &zaiOAuthManager{
		service: service, resolve: resolve, commit: commit, baseCtx: baseCtx,
		now: time.Now, sleep: sleepWithContext,
		sessions: make(map[string]*zaiOAuthSession), activeByID: make(map[string]string),
		stopJanitor: make(chan struct{}), janitorDone: make(chan struct{}),
	}
	go manager.runJanitor()
	return manager
}

func sleepWithContext(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

func (m *zaiOAuthManager) start(ctx context.Context, adminSessionHash string) (zaiOAuthStartResponse, error) {
	if m == nil || m.service == nil || m.resolve == nil || m.commit == nil {
		return zaiOAuthStartResponse{}, errors.New("z.ai OAuth is unavailable")
	}
	adminSessionHash = strings.TrimSpace(adminSessionHash)
	if adminSessionHash == "" {
		return zaiOAuthStartResponse{}, errors.New("z.ai OAuth requires an administrator session")
	}
	pollToken, err := zaiauth.GeneratePollToken()
	if err != nil {
		return zaiOAuthStartResponse{}, err
	}
	flow, err := m.service.InitFlow(ctx, pollToken)
	if err != nil {
		return zaiOAuthStartResponse{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return zaiOAuthStartResponse{}, errors.New("z.ai OAuth is unavailable")
	}
	m.pruneExpiredSessionsLocked()
	if old := m.sessions[m.activeByID[adminSessionHash]]; old != nil && zaiOAuthSessionCancelable(old.status) {
		m.finishSessionLocked(old, "cancelled", "", 0, "")
	}
	sessionCtx, cancel := context.WithCancel(m.baseCtx)
	session := &zaiOAuthSession{
		state: flow.FlowID, adminSessionHash: adminSessionHash, pollToken: pollToken,
		ctx: sessionCtx, cancel: cancel, status: "pending", createdAt: m.now(),
	}
	m.sessions[session.state] = session
	m.activeByID[adminSessionHash] = session.state
	m.pollers.Add(1)
	m.mu.Unlock()

	go m.run(session, flow)
	return zaiOAuthStartResponse{URL: flow.AuthorizeURL, State: session.state, Status: "pending"}, nil
}

// run owns one authorization from the browser hand-off to the committed channel.
func (m *zaiOAuthManager) run(session *zaiOAuthSession, flow *zaiauth.Flow) {
	defer m.pollers.Done()
	interval := zaiauth.PollInterval
	if flow.PollIntervalSec > 0 {
		interval = max(interval, time.Duration(flow.PollIntervalSec)*time.Second)
	}
	deadline := m.now().Add(zaiauth.PollTimeout)
	if flow.ExpiresAt > 0 {
		if expiry := time.Unix(flow.ExpiresAt, 0); expiry.Before(deadline) {
			deadline = expiry
		}
	}

	for m.now().Before(deadline) {
		if session.ctx.Err() != nil {
			return
		}
		result, err := m.service.Poll(session.ctx, flow.FlowID, session.pollToken)
		if err != nil {
			if session.ctx.Err() != nil {
				return
			}
			m.finish(session, "error", err.Error(), 0, "")
			return
		}
		switch result.Status {
		case zaiauth.PollReady:
			m.completeAuthorization(session, result)
			return
		case zaiauth.PollFailed:
			m.finish(session, "error", "z.ai rejected the authorization", 0, "")
			return
		default:
			m.sleep(session.ctx, interval)
		}
	}
	if session.ctx.Err() == nil {
		m.finish(session, "error", "z.ai authorization timed out", 0, "")
	}
}

func (m *zaiOAuthManager) completeAuthorization(session *zaiOAuthSession, result *zaiauth.PollResult) {
	m.mu.Lock()
	if session.status != "pending" {
		m.mu.Unlock()
		return
	}
	session.status = "committing"
	session.pollToken = ""
	if m.activeByID[session.adminSessionHash] == session.state {
		delete(m.activeByID, session.adminSessionHash)
	}
	m.mu.Unlock()

	credential, err := m.resolve(session.ctx, result.AccessToken)
	if err != nil {
		m.finish(session, "error", err.Error(), 0, "")
		return
	}
	credential.AccessToken = result.AccessToken
	credential.JWTToken = result.JWTToken
	if credential.UserID == "" {
		credential.UserID = result.Identity.UserID
	}
	if credential.Email == "" {
		credential.Email = result.Identity.Email
	}
	if credential.Name == "" {
		credential.Name = result.Name
	}
	if err := credential.Normalize(); err != nil {
		m.finish(session, "error", err.Error(), 0, "")
		return
	}
	channelID, channelName, err := m.commit(session.ctx, credential)
	if err != nil {
		m.finish(session, "error", err.Error(), 0, "")
		return
	}
	log.Printf("[INFO] z.ai Coding Plan 渠道已就绪: channel_id=%d", channelID)
	m.finish(session, "complete", "", channelID, channelName)
}

func (m *zaiOAuthManager) finish(session *zaiOAuthSession, status, errorMsg string, channelID int64, channelName string) {
	m.mu.Lock()
	if session.status != "cancelled" {
		m.finishSessionLocked(session, status, errorMsg, channelID, channelName)
	}
	m.mu.Unlock()
}

func (m *zaiOAuthManager) status(adminSessionHash, state string) (zaiOAuthStatusResponse, bool) {
	if m == nil {
		return zaiOAuthStatusResponse{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredSessionsLocked()
	session := m.sessions[strings.TrimSpace(state)]
	if session == nil || session.adminSessionHash != strings.TrimSpace(adminSessionHash) {
		return zaiOAuthStatusResponse{}, false
	}
	return m.statusLocked(session), true
}

func (m *zaiOAuthManager) statusLocked(session *zaiOAuthSession) zaiOAuthStatusResponse {
	return zaiOAuthStatusResponse{
		State: session.state, Status: session.status, Error: session.errorMsg,
		ChannelID: session.channelID, ChannelName: session.channelName,
	}
}

func (m *zaiOAuthManager) cancel(adminSessionHash, state string) error {
	if m == nil {
		return errors.New("z.ai OAuth session not found")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[strings.TrimSpace(state)]
	if session == nil || session.adminSessionHash != strings.TrimSpace(adminSessionHash) {
		return errors.New("z.ai OAuth session not found")
	}
	if !zaiOAuthSessionCancelable(session.status) {
		return fmt.Errorf("z.ai OAuth session cannot be cancelled while %s", session.status)
	}
	m.finishSessionLocked(session, "cancelled", "", 0, "")
	return nil
}

func zaiOAuthSessionCancelable(status string) bool {
	return status == "pending"
}

func (m *zaiOAuthManager) finishSessionLocked(
	session *zaiOAuthSession,
	status, errorMsg string,
	channelID int64,
	channelName string,
) {
	session.status = status
	session.errorMsg = errorMsg
	session.channelID = channelID
	session.channelName = channelName
	session.pollToken = ""
	session.finishedAt = m.now()
	session.cancel()
	if m.activeByID[session.adminSessionHash] == session.state {
		delete(m.activeByID, session.adminSessionHash)
	}
}

func (m *zaiOAuthManager) pruneExpiredSessionsLocked() {
	now := m.now()
	for state, session := range m.sessions {
		if session.finishedAt.IsZero() && zaiOAuthSessionCancelable(session.status) &&
			now.Sub(session.createdAt) > zaiOAuthSessionTTL {
			m.finishSessionLocked(session, "error", "z.ai OAuth session expired", 0, "")
		}
		if !session.finishedAt.IsZero() && now.Sub(session.finishedAt) > zaiOAuthTerminalTTL {
			delete(m.sessions, state)
		}
	}
}

func (m *zaiOAuthManager) runJanitor() {
	ticker := time.NewTicker(zaiOAuthJanitorInterval)
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

func (m *zaiOAuthManager) close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		close(m.stopJanitor)
		m.mu.Lock()
		m.closed = true
		cancels := make([]context.CancelFunc, 0, len(m.sessions))
		for _, session := range m.sessions {
			session.pollToken = ""
			cancels = append(cancels, session.cancel)
		}
		m.sessions = make(map[string]*zaiOAuthSession)
		m.activeByID = make(map[string]string)
		m.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		m.pollers.Wait()
		<-m.janitorDone
	})
}

// HandleStartZAIOAuth opens one hosted ZCode authorization.
func (s *Server) HandleStartZAIOAuth(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok {
		RespondErrorMsg(c, http.StatusUnauthorized, "administrator session is unavailable")
		return
	}
	if s.zaiOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "z.ai OAuth is unavailable")
		return
	}
	response, err := s.zaiOAuth.start(c.Request.Context(), adminSessionHash)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, zaiauth.ErrOAuthFlowUnavailable) {
			status = http.StatusServiceUnavailable
		}
		RespondError(c, status, err)
		return
	}
	RespondJSON(c, http.StatusOK, response)
}

// HandleZAIOAuthStatus returns only sessions owned by the current administrator.
func (s *Server) HandleZAIOAuthStatus(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok || s.zaiOAuth == nil {
		RespondErrorMsg(c, http.StatusNotFound, "z.ai OAuth session not found")
		return
	}
	status, exists := s.zaiOAuth.status(adminSessionHash, c.Query("state"))
	if !exists {
		RespondErrorMsg(c, http.StatusNotFound, "z.ai OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelZAIOAuth cancels one pending administrator-owned session.
func (s *Server) HandleCancelZAIOAuth(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok || s.zaiOAuth == nil {
		RespondErrorMsg(c, http.StatusNotFound, "z.ai OAuth session not found")
		return
	}
	var request zaiOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if err := s.zaiOAuth.cancel(adminSessionHash, request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, zaiOAuthStatusResponse{State: strings.TrimSpace(request.State), Status: "cancelled"})
}
