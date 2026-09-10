package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/codebuddyauth"
	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var codeBuddyChannelCreateMu sync.Mutex

func codeBuddyChannelBaseName(credential *codebuddyauth.Credential) string {
	identity := credential.UID + "/" + credential.EnterpriseID
	if credential.UID == "" {
		identity = credential.AccessToken
	}
	digest := sha256.Sum256([]byte(identity))
	label := "CodeBuddy"
	if credential.Nickname != "" {
		label += "-" + credential.Nickname
	}
	return fmt.Sprintf("%s-%x", label, digest[:4])
}

func newCodeBuddyChannel(name, credential string) *model.Config {
	entries := make([]model.ModelEntry, 0, len(codebuddyauth.DefaultModels))
	for _, name := range codebuddyauth.DefaultModels {
		entries = append(entries, model.ModelEntry{Model: name})
	}
	return &model.Config{Name: name, AuthType: model.AuthTypeCodeBuddyOAuth, OAuthCredential: credential,
		URLs:                  model.ChannelURLs{{URL: codebuddyauth.CompletionsURL, Exact: true, Protocols: []string{"openai"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal, Enabled: true, CostMultiplier: 1, ModelEntries: entries}
}

func codeBuddyIdentityMatches(a, b *codebuddyauth.Credential) bool {
	if a.UID != "" && b.UID != "" {
		return a.UID == b.UID && a.EnterpriseID == b.EnterpriseID
	}
	return a.AccessToken == b.AccessToken
}

func (s *Server) commitCodeBuddyCredential(ctx context.Context, credential *codebuddyauth.Credential) (*model.Config, bool, error) {
	payload, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	codeBuddyChannelCreateMu.Lock()
	defer codeBuddyChannelCreateMu.Unlock()
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, false, err
	}
	var cfg *model.Config
	for _, existing := range configs {
		if !existing.UsesCodeBuddyOAuth() {
			continue
		}
		stored, parseErr := codebuddyauth.ParseCredential([]byte(existing.OAuthCredential))
		if parseErr != nil || !codeBuddyIdentityMatches(stored, credential) {
			continue
		}
		updated, updateErr := s.store.CompareAndSwapOAuthCredential(ctx, existing.ID, model.AuthTypeCodeBuddyOAuth, existing.OAuthCredential, payload)
		if updateErr != nil {
			return nil, false, updateErr
		}
		if !updated {
			return nil, false, errors.New("CodeBuddy credential changed concurrently; retry authorization")
		}
		if err := s.store.ResetChannelCooldown(ctx, existing.ID); err != nil {
			return nil, false, err
		}
		cfg, err = s.store.UpdateChannelEnabled(ctx, existing.ID, true)
		if err != nil {
			return nil, false, err
		}
		break
	}
	created := cfg == nil
	if created {
		name := codeBuddyChannelBaseName(credential)
		cfg, err = s.store.CreateConfig(ctx, newCodeBuddyChannel(uniqueZedChannelName(configs, name), payload))
		if err != nil {
			return nil, false, err
		}
	}
	s.invalidateChannelRelatedCache(cfg.ID)
	s.InvalidateChannelListCache()
	return cfg, created, nil
}

// HandleImportCodeBuddyCredential imports a canonical or workbuddy credential.
func (s *Server) HandleImportCodeBuddyCredential(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid CodeBuddy credential body")
		return
	}
	credential, err := codebuddyauth.ParseCredential(raw)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	cfg, created, err := s.commitCodeBuddyCredential(c.Request.Context(), credential)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"channel_id": cfg.ID, "channel_name": cfg.Name, "created": created})
}

// HandleRefreshCodeBuddyCredential refreshes and persists a channel credential.
func (s *Server) HandleRefreshCodeBuddyCredential(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}
	if !cfg.UsesCodeBuddyOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use CodeBuddy OAuth")
		return
	}
	credential, err := s.codeBuddyCredentials.credential(c.Request.Context(), cfg, true, "")
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}

type codeBuddyLoginStatus struct {
	State       string `json:"state"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	ChannelID   int64  `json:"channel_id,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
}
type codeBuddyLoginSession struct {
	owner    string
	status   codeBuddyLoginStatus
	cancel   context.CancelFunc
	finished time.Time
}
type codeBuddyOAuthManager struct {
	mu       sync.Mutex
	sessions map[string]*codeBuddyLoginSession
	server   *Server
	closed   bool
	wg       sync.WaitGroup
}

func (m *codeBuddyOAuthManager) close() {
	m.mu.Lock()
	m.closed = true
	for _, session := range m.sessions {
		session.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// HandleStartCodeBuddyOAuth starts authorization owned by the current admin session.
func (s *Server) HandleStartCodeBuddyOAuth(c *gin.Context) {
	owner, ok := xaiAdminSessionHash(c)
	if !ok {
		RespondErrorMsg(c, http.StatusUnauthorized, "administrator session required")
		return
	}
	login, err := s.codeBuddyService.Start(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	m := s.codeBuddyOAuth
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		RespondErrorMsg(c, http.StatusServiceUnavailable, "CodeBuddy OAuth closed")
		return
	}
	for state, old := range m.sessions {
		if old.owner == owner && old.status.Status == "pending" {
			old.status.Status = "cancelled"
			old.finished = time.Now()
			old.cancel()
		}
		if !old.finished.IsZero() && time.Since(old.finished) > 2*time.Minute {
			delete(m.sessions, state)
		}
	}
	ctx, cancel := context.WithTimeout(s.baseCtx, codebuddyauth.LoginTTL)
	// The public state is independent of the upstream state and never leaks cookies.
	state := uuid.NewString()
	session := &codeBuddyLoginSession{owner: owner, status: codeBuddyLoginStatus{State: state, Status: "pending"}, cancel: cancel}
	m.sessions[state] = session
	m.wg.Add(1)
	m.mu.Unlock()
	go m.run(ctx, session, login)
	RespondJSON(c, http.StatusOK, gin.H{"state": state, "url": login.URL, "status": "pending"})
}

func (m *codeBuddyOAuthManager) run(ctx context.Context, session *codeBuddyLoginSession, login *codebuddyauth.Login) {
	defer m.wg.Done()
	defer session.cancel()
	var credential *codebuddyauth.Credential
	var err error
	for ctx.Err() == nil {
		credential, err = m.server.codeBuddyService.Poll(ctx, login)
		if credential != nil || err != nil {
			break
		}
		sleepWithContext(ctx, 2*time.Second)
	}
	m.mu.Lock()
	if session.status.Status != "pending" {
		m.mu.Unlock()
		return
	}
	if ctx.Err() != nil {
		session.status.Status = "error"
		session.status.Error = "CodeBuddy authorization expired; start a new login"
		session.finished = time.Now()
		m.mu.Unlock()
		return
	}
	if err != nil {
		session.status.Status = "error"
		session.status.Error = err.Error()
		session.finished = time.Now()
		m.mu.Unlock()
		return
	}
	// Once committing, cancellation cannot report success while a channel is created.
	session.status.Status = "exchanging"
	m.mu.Unlock()
	cfg, _, err := m.server.commitCodeBuddyCredential(ctx, credential)
	m.mu.Lock()
	defer m.mu.Unlock()
	session.finished = time.Now()
	if err != nil {
		session.status.Status = "error"
		session.status.Error = err.Error()
		return
	}
	session.status.Status, session.status.ChannelID, session.status.ChannelName = "complete", cfg.ID, cfg.Name
}

// HandleCodeBuddyOAuthStatus returns the current admin's authorization status.
func (s *Server) HandleCodeBuddyOAuthStatus(c *gin.Context) {
	owner, ok := xaiAdminSessionHash(c)
	m := s.codeBuddyOAuth
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[c.Query("state")]
	if !ok || session == nil || session.owner != owner {
		RespondErrorMsg(c, http.StatusNotFound, "CodeBuddy OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, session.status)
}

// HandleCancelCodeBuddyOAuth cancels a pending authorization owned by this admin.
func (s *Server) HandleCancelCodeBuddyOAuth(c *gin.Context) {
	owner, ok := xaiAdminSessionHash(c)
	var request struct {
		State string `json:"state"`
	}
	if c.ShouldBindJSON(&request) != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "state is required")
		return
	}
	m := s.codeBuddyOAuth
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[strings.TrimSpace(request.State)]
	if !ok || session == nil || session.owner != owner {
		RespondErrorMsg(c, http.StatusNotFound, "CodeBuddy OAuth session not found")
		return
	}
	if session.status.Status != "pending" {
		RespondErrorMsg(c, http.StatusConflict, "CodeBuddy OAuth is no longer pending")
		return
	}
	session.status.Status, session.finished = "cancelled", time.Now()
	session.cancel()
	RespondJSON(c, http.StatusOK, session.status)
}
