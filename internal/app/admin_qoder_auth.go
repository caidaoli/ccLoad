package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/qoderauth"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var qoderChannelCreateMu sync.Mutex

func qoderChannelBaseName(credential *qoderauth.Credential) string {
	digest := sha256.Sum256([]byte(credential.Region + "/" + credential.UserID))
	return fmt.Sprintf("Qoder-%s-%x", credential.Region, digest[:4])
}
func newQoderChannel(name, credential string) *model.Config {
	c, _ := qoderauth.ParseCredential([]byte(credential))
	endpoint := ""
	if c != nil {
		endpoint = c.GatewayURL()
	}
	return &model.Config{Name: name, AuthType: model.AuthTypeQoderOAuth, OAuthCredential: credential,
		URLs:                  model.ChannelURLs{{URL: strings.TrimRight(endpoint, "/") + "/v1/chat/completions", Exact: true, Protocols: []string{"openai"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal, Enabled: true, CostMultiplier: 1}
}
func (s *Server) prepareQoderChannel(ctx context.Context, name, credential string) (*model.Config, error) {
	cfg := newQoderChannel(name, credential)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := s.fetchQoderOAuthModels(ctx, cfg, "")
	if err != nil {
		return nil, err
	}
	cfg.ModelEntries = response.Models
	return cfg, nil
}
func qoderIdentityMatches(a, b *qoderauth.Credential) bool {
	return a.Region == b.Region && a.UserID == b.UserID
}

func (s *Server) commitQoderCredential(ctx context.Context, credential *qoderauth.Credential) (*model.Config, bool, error) {
	payload, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	qoderChannelCreateMu.Lock()
	defer qoderChannelCreateMu.Unlock()
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, false, err
	}
	var cfg *model.Config
	for _, existing := range configs {
		if !existing.UsesQoderOAuth() {
			continue
		}
		stored, parseErr := qoderauth.ParseCredential([]byte(existing.OAuthCredential))
		if parseErr != nil || !qoderIdentityMatches(stored, credential) {
			continue
		}
		credential.AutoCheckin = stored.AutoCheckin
		credential.LastCheckinAt = stored.LastCheckinAt
		credential.LastCheckinCampaign = stored.LastCheckinCampaign
		credential.OAuthUsage = stored.OAuthUsage
		payload, err = credential.JSON()
		if err != nil {
			return nil, false, err
		}
		updated, updateErr := s.store.CompareAndSwapOAuthCredential(ctx, existing.ID, model.AuthTypeQoderOAuth, existing.OAuthCredential, payload)
		if updateErr != nil {
			return nil, false, updateErr
		}
		if !updated {
			return nil, false, errors.New("qoder credential changed concurrently; retry authorization")
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
		name := qoderChannelBaseName(credential)
		cfg, err = s.prepareQoderChannel(ctx, uniqueZedChannelName(configs, name), payload)
		if err != nil {
			return nil, false, err
		}
		cfg, err = s.store.CreateConfig(ctx, cfg)
		if err != nil {
			return nil, false, err
		}
	}
	s.invalidateChannelRelatedCache(cfg.ID)
	s.InvalidateChannelListCache()
	return cfg, created, nil
}

// HandleImportQoderCredential imports a canonical credential or exchanges a PAT.
func (s *Server) HandleImportQoderCredential(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		RespondErrorMsg(c, 400, "invalid Qoder credential body")
		return
	}
	var input struct {
		Region     string          `json:"region"`
		PAT        string          `json:"pat"`
		Credential json.RawMessage `json:"credential"`
	}
	if json.Unmarshal(raw, &input) != nil {
		RespondErrorMsg(c, 400, "invalid Qoder import JSON")
		return
	}
	var credential *qoderauth.Credential
	var canonical map[string]json.RawMessage
	_ = json.Unmarshal(raw, &canonical)
	if input.PAT != "" && len(input.Credential) == 0 && len(canonical["access_token"]) == 0 {
		credential, err = s.qoderService.ExchangePAT(c.Request.Context(), input.Region, input.PAT)
	} else {
		if len(input.Credential) > 0 {
			raw = input.Credential
		}
		credential, err = qoderauth.ParseCredential(raw)
		if err == nil && input.Region != "" && credential.Region != input.Region {
			err = errors.New("qoder credential belongs to a different region")
		}
		if err == nil {
			credential, err = s.qoderService.CompleteIdentity(c.Request.Context(), credential)
		}
	}
	if err != nil {
		RespondError(c, 400, err)
		return
	}
	cfg, created, err := s.commitQoderCredential(c.Request.Context(), credential)
	if err != nil {
		RespondError(c, 502, err)
		return
	}
	RespondJSON(c, 200, gin.H{"channel_id": cfg.ID, "channel_name": cfg.Name, "created": created})
}

// HandleRefreshQoderCredential refreshes and persists a channel credential.
func (s *Server) HandleRefreshQoderCredential(c *gin.Context) {
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
	if !cfg.UsesQoderOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use Qoder OAuth")
		return
	}
	credential, err := s.qoderCredentials.credential(c.Request.Context(), cfg, true, "")
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}

type qoderLoginStatus struct {
	State       string `json:"state"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	ChannelID   int64  `json:"channel_id,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
}
type qoderLoginSession struct {
	owner    string
	status   qoderLoginStatus
	cancel   context.CancelFunc
	finished time.Time
}
type qoderOAuthManager struct {
	mu       sync.Mutex
	sessions map[string]*qoderLoginSession
	server   *Server
	closed   bool
	wg       sync.WaitGroup
}

func (m *qoderOAuthManager) close() {
	m.mu.Lock()
	m.closed = true
	for _, session := range m.sessions {
		session.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (s *Server) handleStartQoderOAuth(c *gin.Context, region string) {
	owner, ok := xaiAdminSessionHash(c)
	if !ok {
		RespondErrorMsg(c, http.StatusUnauthorized, "administrator session required")
		return
	}
	login, err := qoderauth.StartLogin(region)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	m := s.qoderOAuth
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Qoder OAuth closed")
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
	ctx, cancel := context.WithTimeout(s.baseCtx, 5*time.Minute)
	// The public state is independent of the upstream state and never leaks cookies.
	state := uuid.NewString()
	session := &qoderLoginSession{owner: owner, status: qoderLoginStatus{State: state, Status: "pending"}, cancel: cancel}
	m.sessions[state] = session
	m.wg.Add(1)
	m.mu.Unlock()
	go m.run(ctx, session, login)
	RespondJSON(c, http.StatusOK, gin.H{"state": state, "url": login.AuthURL, "status": "pending"})
}

// HandleStartQoderOAuth starts authorization owned by the current admin session.
func (s *Server) HandleStartQoderOAuth(c *gin.Context) {
	var input struct {
		Region string `json:"region"`
	}
	if c.ShouldBindJSON(&input) != nil {
		RespondErrorMsg(c, 400, "region is required")
		return
	}
	s.handleStartQoderOAuth(c, input.Region)
}

func (m *qoderOAuthManager) run(ctx context.Context, session *qoderLoginSession, login *qoderauth.LoginSession) {
	defer m.wg.Done()
	defer session.cancel()
	var credential *qoderauth.Credential
	var err error
	for ctx.Err() == nil {
		credential, err = m.server.qoderService.PollLogin(ctx, login)
		if errors.Is(err, qoderauth.ErrLoginPending) {
			err = nil
		}
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
		session.status.Error = "Qoder authorization expired; start a new login"
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
	cfg, _, err := m.server.commitQoderCredential(ctx, credential)
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

// HandleQoderOAuthStatus returns the current admin's authorization status.
func (s *Server) HandleQoderOAuthStatus(c *gin.Context) {
	owner, ok := xaiAdminSessionHash(c)
	m := s.qoderOAuth
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[c.Query("state")]
	if !ok || session == nil || session.owner != owner {
		RespondErrorMsg(c, http.StatusNotFound, "Qoder OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, session.status)
}

// HandleCancelQoderOAuth cancels a pending authorization owned by this admin.
func (s *Server) HandleCancelQoderOAuth(c *gin.Context) {
	owner, ok := xaiAdminSessionHash(c)
	var request struct {
		State string `json:"state"`
	}
	if c.ShouldBindJSON(&request) != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "state is required")
		return
	}
	m := s.qoderOAuth
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[strings.TrimSpace(request.State)]
	if !ok || session == nil || session.owner != owner {
		RespondErrorMsg(c, http.StatusNotFound, "Qoder OAuth session not found")
		return
	}
	if session.status.Status != "pending" {
		RespondErrorMsg(c, http.StatusConflict, "Qoder OAuth is no longer pending")
		return
	}
	session.status.Status, session.finished = "cancelled", time.Now()
	session.cancel()
	RespondJSON(c, http.StatusOK, session.status)
}
