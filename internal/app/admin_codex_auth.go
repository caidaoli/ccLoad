package app

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

const (
	codexOAuthCallbackAddr = "127.0.0.1:1455"
	codexOAuthTimeout      = 5 * time.Minute
	codexOAuthStatusTTL    = 10 * time.Minute
	codexUpstreamURL       = "https://chatgpt.com/backend-api/codex/responses"
	maxCodexImportBytes    = 1 << 20
)

var codexOAuthDefaultModels = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.3-codex-spark",
	"codex-auto-review",
}

var codexOAuthExcludedModelsByPlan = map[string]map[string]struct{}{
	"free": {
		"gpt-5.6-sol":         {},
		"gpt-5.4":             {},
		"gpt-5.3-codex-spark": {},
	},
	"team": {
		"gpt-5.3-codex-spark": {},
	},
}

type codexOAuthResult struct {
	code     string
	state    string
	errorMsg string
}

type codexOAuthSession struct {
	state            string
	pkce             codexauth.PKCE
	ctx              context.Context
	cancelContext    context.CancelFunc
	status           string
	errorMsg         string
	channelID        int64
	createdAt        time.Time
	finished         time.Time
	callbackReceived bool
	result           chan codexOAuthResult
	server           *http.Server
}

type codexOAuthStatus struct {
	State     string `json:"state"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	ChannelID int64  `json:"channel_id,omitempty"`
}

type codexOAuthCallbackRequest struct {
	CallbackURL string `json:"callback_url"`
}

func (r *codexOAuthCallbackRequest) Validate() error {
	if strings.TrimSpace(r.CallbackURL) == "" {
		return errors.New("callback_url is required")
	}
	return nil
}

type codexOAuthCancelRequest struct {
	State string `json:"state"`
}

func (r *codexOAuthCancelRequest) Validate() error {
	if strings.TrimSpace(r.State) == "" {
		return errors.New("state is required")
	}
	return nil
}

type codexOAuthManager struct {
	startMu    sync.Mutex
	mu         sync.Mutex
	service    *codexauth.Service
	store      storage.Store
	listenAddr string
	timeout    time.Duration
	now        func() time.Time
	sessions   map[string]*codexOAuthSession
	active     *codexOAuthSession
	invalidate func(int64)
}

func newCodexOAuthManager(service *codexauth.Service, store storage.Store, invalidate func(int64)) *codexOAuthManager {
	return &codexOAuthManager{
		service: service, store: store, listenAddr: codexOAuthCallbackAddr,
		timeout: codexOAuthTimeout, now: time.Now,
		sessions: make(map[string]*codexOAuthSession), invalidate: invalidate,
	}
}

func (m *codexOAuthManager) start() (string, string, error) {
	if m == nil || m.service == nil || m.store == nil {
		return "", "", errors.New("codex OAuth is unavailable")
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()

	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if active != nil {
		if err := m.cancelSession(active.state, false); err != nil {
			return "", "", fmt.Errorf("replace existing Codex OAuth login: %w", err)
		}
	}

	state, err := codexauth.GenerateState()
	if err != nil {
		return "", "", err
	}
	pkce, err := codexauth.GeneratePKCE()
	if err != nil {
		return "", "", err
	}

	m.mu.Lock()
	m.pruneLocked()
	if m.active != nil {
		m.mu.Unlock()
		return "", "", errors.New("a Codex OAuth login is already pending")
	}
	listener, err := net.Listen("tcp", m.listenAddr)
	if err != nil {
		m.mu.Unlock()
		return "", "", fmt.Errorf("listen for Codex OAuth callback on %s: %w", m.listenAddr, err)
	}

	service := *m.service
	if _, port, splitErr := net.SplitHostPort(m.listenAddr); splitErr == nil && port == "0" {
		service.RedirectURI = "http://" + listener.Addr().String() + "/auth/callback"
	}
	authURL, err := service.AuthorizationLink(state, pkce)
	if err != nil {
		_ = listener.Close()
		m.mu.Unlock()
		return "", "", err
	}

	sessionCtx, cancelSession := context.WithCancel(context.Background())
	session := &codexOAuthSession{
		state: state, pkce: pkce, status: "pending", createdAt: m.now(),
		ctx: sessionCtx, cancelContext: cancelSession, result: make(chan codexOAuthResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		m.handleCallback(session, w, r)
	})
	session.server = &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second,
	}
	m.sessions[state] = session
	m.active = session
	m.mu.Unlock()

	go func() {
		if serveErr := session.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_ = m.deliverCallback(session, codexOAuthResult{state: state, errorMsg: serveErr.Error()})
		}
	}()
	go m.complete(session, &service)
	return authURL, state, nil
}

func (m *codexOAuthManager) handleCallback(session *codexOAuthSession, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result := codexOAuthResult{
		code:     strings.TrimSpace(r.URL.Query().Get("code")),
		state:    strings.TrimSpace(r.URL.Query().Get("state")),
		errorMsg: strings.TrimSpace(r.URL.Query().Get("error")),
	}
	if result.errorMsg == "" && result.code == "" {
		result.errorMsg = "missing authorization code"
	}
	if result.errorMsg == "" && result.state != session.state {
		result.errorMsg = "invalid OAuth state"
	}
	if err := m.deliverCallback(session, result); err != nil {
		result.errorMsg = err.Error()
	}
	if result.errorMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "<h1>Codex 登录失败</h1><p>%s</p>", html.EscapeString(result.errorMsg))
		return
	}
	_, _ = io.WriteString(w, "<h1>Codex 授权已收到</h1><p>ccLoad 正在创建渠道，可以关闭此窗口。</p>")
}

func (m *codexOAuthManager) deliverCallback(session *codexOAuthSession, result codexOAuthResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session == nil || session.status != "pending" || m.sessions[session.state] != session {
		return errors.New("codex OAuth session is not pending")
	}
	if session.callbackReceived {
		return errors.New("OAuth callback was already consumed")
	}
	session.callbackReceived = true
	select {
	case session.result <- result:
		return nil
	default:
		session.callbackReceived = false
		return errors.New("unable to deliver OAuth callback")
	}
}

func (m *codexOAuthManager) submitCallbackURL(rawURL string) (string, error) {
	result, err := parseCodexOAuthCallbackURL(rawURL)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.pruneLocked()
	session := m.sessions[result.state]
	m.mu.Unlock()
	if session == nil {
		return "", errors.New("codex OAuth session not found")
	}
	if err := m.deliverCallback(session, result); err != nil {
		return "", err
	}
	return result.state, nil
}

func (m *codexOAuthManager) cancel(state string) error {
	return m.cancelSession(state, false)
}

func (m *codexOAuthManager) cancelSession(state string, force bool) error {
	state = strings.TrimSpace(state)
	m.mu.Lock()
	m.pruneLocked()
	session := m.sessions[state]
	if session == nil {
		m.mu.Unlock()
		return errors.New("codex OAuth session not found")
	}
	if session.status == "cancelled" {
		m.mu.Unlock()
		return nil
	}
	if !force && session.status != "pending" {
		m.mu.Unlock()
		return fmt.Errorf("codex OAuth session cannot be cancelled while %s", session.status)
	}
	session.status = "cancelled"
	session.errorMsg = ""
	session.finished = m.now()
	session.callbackReceived = true
	if m.active == session {
		m.active = nil
	}
	cancelContext := session.cancelContext
	callbackServer := session.server
	m.mu.Unlock()

	if cancelContext != nil {
		cancelContext()
	}
	if callbackServer != nil {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
		err := callbackServer.Shutdown(shutdownCtx)
		cancelShutdown()
		if err != nil {
			return fmt.Errorf("stop Codex OAuth callback listener: %w", err)
		}
	}
	return nil
}

func parseCodexOAuthCallbackURL(rawURL string) (codexOAuthResult, error) {
	callbackURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !callbackURL.IsAbs() || callbackURL.Host == "" {
		return codexOAuthResult{}, errors.New("invalid Codex OAuth callback URL")
	}
	if !strings.EqualFold(callbackURL.Scheme, "http") || callbackURL.Path != "/auth/callback" {
		return codexOAuthResult{}, errors.New("invalid Codex OAuth callback URL")
	}
	host := callbackURL.Hostname()
	isLoopback := strings.EqualFold(host, "localhost")
	if !isLoopback {
		ip := net.ParseIP(host)
		isLoopback = ip != nil && ip.IsLoopback()
	}
	if !isLoopback {
		return codexOAuthResult{}, errors.New("codex OAuth callback URL must use a loopback host")
	}

	query := callbackURL.Query()
	result := codexOAuthResult{
		code:     strings.TrimSpace(query.Get("code")),
		state:    strings.TrimSpace(query.Get("state")),
		errorMsg: strings.TrimSpace(query.Get("error")),
	}
	if result.state == "" {
		return codexOAuthResult{}, errors.New("codex OAuth callback state is required")
	}
	if result.errorMsg == "" && result.code == "" {
		return codexOAuthResult{}, errors.New("codex OAuth callback code is required")
	}
	if description := strings.TrimSpace(query.Get("error_description")); result.errorMsg != "" && description != "" {
		result.errorMsg += ": " + description
	}
	return result, nil
}

func (m *codexOAuthManager) complete(session *codexOAuthSession, service *codexauth.Service) {
	defer session.cancelContext()
	timer := time.NewTimer(m.timeout)
	defer timer.Stop()
	var result codexOAuthResult
	select {
	case result = <-session.result:
	case <-session.ctx.Done():
		result = codexOAuthResult{state: session.state, errorMsg: "Codex OAuth login cancelled"}
	case <-timer.C:
		result = codexOAuthResult{state: session.state, errorMsg: "Codex OAuth login timed out"}
	}

	var channelID int64
	if result.errorMsg == "" {
		if result.state != session.state {
			result.errorMsg = "invalid OAuth state"
		} else {
			credential, err := service.ExchangeCode(session.ctx, result.code, session.pkce)
			if err != nil {
				result.errorMsg = err.Error()
			} else {
				m.mu.Lock()
				cancelled := session.status == "cancelled"
				if !cancelled {
					session.status = "committing"
				}
				m.mu.Unlock()
				if cancelled {
					result.errorMsg = "Codex OAuth login cancelled"
				} else {
					channel, _, createErr := createOrUpdateCodexChannel(session.ctx, m.store, credential)
					if createErr != nil {
						result.errorMsg = createErr.Error()
					} else {
						channelID = channel.ID
					}
				}
			}
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = session.server.Shutdown(shutdownCtx)
	cancel()

	m.mu.Lock()
	cancelled := session.status == "cancelled"
	if cancelled {
		// Cancellation is a terminal state. Never let the completion goroutine
		// overwrite it after an in-flight token request is interrupted.
	} else if result.errorMsg != "" {
		session.status = "error"
		session.errorMsg = result.errorMsg
	} else {
		session.status = "complete"
		session.channelID = channelID
	}
	session.finished = m.now()
	if m.active == session {
		m.active = nil
	}
	m.mu.Unlock()
	if !cancelled && result.errorMsg == "" && m.invalidate != nil {
		m.invalidate(channelID)
	}
}

func (m *codexOAuthManager) status(state string) (codexOAuthStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	session := m.sessions[strings.TrimSpace(state)]
	if session == nil {
		return codexOAuthStatus{}, false
	}
	return codexOAuthStatus{
		State: session.state, Status: session.status, Error: session.errorMsg, ChannelID: session.channelID,
	}, true
}

func (m *codexOAuthManager) pruneLocked() {
	now := m.now()
	for state, session := range m.sessions {
		if !session.finished.IsZero() && now.Sub(session.finished) > codexOAuthStatusTTL {
			delete(m.sessions, state)
		}
	}
}

func (m *codexOAuthManager) close() {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if active != nil {
		_ = m.cancelSession(active.state, true)
	}
}

func createOrUpdateCodexChannel(ctx context.Context, store storage.Store, credential *codexauth.Credential) (*model.Config, bool, error) {
	credentialJSON, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list channels for Codex credential: %w", err)
	}
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesCodexOAuth() || cfg.CodexCredential == "" {
			continue
		}
		existing, parseErr := codexauth.ParseCredential([]byte(cfg.CodexCredential))
		if parseErr != nil || !sameCodexIdentity(existing, credential) {
			continue
		}
		models := reconcileCodexOAuthModelEntries(cfg.ModelEntries, existing.PlanType, credential.PlanType)
		if err := store.UpdateCodexCredential(ctx, cfg.ID, credentialJSON); err != nil {
			return nil, false, err
		}
		if !modelEntriesEqual(cfg.ModelEntries, models) {
			cfg.ModelEntries = models
			if !codexOAuthModelAllowed(cfg.ScheduledCheckModel, credential.PlanType) {
				cfg.ScheduledCheckModel = ""
			}
			if _, err := store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
				return nil, false, fmt.Errorf("reconcile Codex models for plan %q: %w", credential.PlanType, err)
			}
		}
		updated, err := store.GetConfig(ctx, cfg.ID)
		return updated, false, err
	}

	name := uniqueCodexChannelName(configs, credential)
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: name, AuthType: model.AuthTypeCodexOAuth, CodexCredential: credentialJSON,
		URLs:       model.ChannelURLs{{URL: codexUpstreamURL, Exact: true, Protocols: []string{"codex"}}},
		Websockets: true, ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority: 0, Enabled: true, CostMultiplier: 1,
		ModelEntries: codexOAuthModelEntries(credential.PlanType),
	})
	if err != nil {
		return nil, false, fmt.Errorf("create Codex channel: %w", err)
	}
	return created, true, nil
}

func codexOAuthPlanTier(planType string) string {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "free":
		return "free"
	case "team", "business", "go":
		return "team"
	case "plus":
		return "plus"
	case "pro":
		return "pro"
	default:
		return "pro"
	}
}

func codexOAuthModelAllowed(name, planType string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "*" {
		return false
	}
	excluded := codexOAuthExcludedModelsByPlan[codexOAuthPlanTier(planType)]
	for _, supported := range codexOAuthDefaultModels {
		if name == supported {
			_, blocked := excluded[name]
			return !blocked
		}
	}
	return false
}

func codexOAuthModelEntries(planType string) []model.ModelEntry {
	entries := make([]model.ModelEntry, 0, len(codexOAuthDefaultModels))
	for _, name := range codexOAuthDefaultModels {
		if codexOAuthModelAllowed(name, planType) {
			entries = append(entries, model.ModelEntry{Model: name})
		}
	}
	return entries
}

func filterCodexOAuthModelEntries(entries []model.ModelEntry, planType string) []model.ModelEntry {
	filtered := make([]model.ModelEntry, 0, len(entries))
	for _, entry := range entries {
		if codexOAuthModelAllowed(entry.Model, planType) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func mergeCodexOAuthModelEntries(entries []model.ModelEntry, planType string) []model.ModelEntry {
	existing := make(map[string]model.ModelEntry, len(entries))
	for _, entry := range entries {
		existing[entry.Model] = entry
	}
	models := codexOAuthModelEntries(planType)
	for i := range models {
		if entry, ok := existing[models[i].Model]; ok {
			models[i] = entry
		}
	}
	return models
}

func reconcileCodexOAuthModelEntries(entries []model.ModelEntry, previousPlanType, planType string) []model.ModelEntry {
	models := filterCodexOAuthModelEntries(entries, planType)
	if hasWildcardCodexModel(entries) || codexOAuthPlanTier(previousPlanType) != codexOAuthPlanTier(planType) || len(models) == 0 {
		return mergeCodexOAuthModelEntries(entries, planType)
	}
	return models
}

func hasWildcardCodexModel(entries []model.ModelEntry) bool {
	for _, entry := range entries {
		if strings.TrimSpace(entry.Model) == "*" {
			return true
		}
	}
	return false
}

func sameCodexIdentity(a, b *codexauth.Credential) bool {
	if a == nil || b == nil {
		return false
	}
	if a.AccountID != "" && b.AccountID != "" {
		return a.AccountID == b.AccountID
	}
	return a.Email != "" && b.Email != "" && strings.EqualFold(a.Email, b.Email)
}

func uniqueCodexChannelName(configs []*model.Config, credential *codexauth.Credential) string {
	identity := strings.TrimSpace(credential.Email)
	if identity == "" {
		identity = strings.TrimSpace(credential.AccountID)
	}
	if identity == "" {
		identity = "OAuth"
	}
	base := "Codex - " + identity
	used := make(map[string]struct{}, len(configs))
	for _, cfg := range configs {
		if cfg != nil {
			used[strings.ToLower(cfg.Name)] = struct{}{}
		}
	}
	if _, exists := used[strings.ToLower(base)]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s (%d)", base, suffix)
		if _, exists := used[strings.ToLower(candidate)]; !exists {
			return candidate
		}
	}
}

// HandleStartCodexOAuth starts the local callback listener and returns the browser URL.
func (s *Server) HandleStartCodexOAuth(c *gin.Context) {
	url, state, err := s.codexOAuth.start()
	if err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"url": url, "state": state, "status": "pending"})
}

// HandleCodexOAuthStatus returns the state of one pending Codex OAuth login.
func (s *Server) HandleCodexOAuthStatus(c *gin.Context) {
	status, ok := s.codexOAuth.status(c.Query("state"))
	if !ok {
		RespondErrorMsg(c, http.StatusNotFound, "Codex OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelCodexOAuth stops one pending login and releases its callback listener.
func (s *Server) HandleCancelCodexOAuth(c *gin.Context) {
	var request codexOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.codexOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Codex OAuth is unavailable")
		return
	}
	if err := s.codexOAuth.cancel(request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": strings.TrimSpace(request.State), "status": "cancelled"})
}

// HandleSubmitCodexOAuthCallback accepts a loopback callback URL copied from
// the browser when the ccLoad host cannot receive the localhost redirect.
func (s *Server) HandleSubmitCodexOAuthCallback(c *gin.Context) {
	var request codexOAuthCallbackRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.codexOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Codex OAuth is unavailable")
		return
	}
	state, err := s.codexOAuth.submitCallbackURL(request.CallbackURL)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": state, "status": "accepted"})
}

// HandleImportCodexCredential imports one CLIProxy-compatible JSON credential
// directly into a channel. No authentication file is created.
func (s *Server) HandleImportCodexCredential(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "credential file is required")
		return
	}
	if file.Size <= 0 || file.Size > maxCodexImportBytes {
		RespondErrorMsg(c, http.StatusBadRequest, "credential file size is invalid")
		return
	}
	opened, err := file.Open()
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	defer func() { _ = opened.Close() }()
	raw, err := io.ReadAll(io.LimitReader(opened, maxCodexImportBytes+1))
	if err != nil || len(raw) > maxCodexImportBytes {
		RespondErrorMsg(c, http.StatusBadRequest, "failed to read credential")
		return
	}
	credential, err := codexauth.ParseCredential(raw)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	channel, created, err := createOrUpdateCodexChannel(c.Request.Context(), s.store, credential)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	s.InvalidateChannelListCache()
	if s.codexCredentials != nil {
		s.codexCredentials.invalidate(channel.ID)
	}
	RespondJSON(c, http.StatusOK, gin.H{"channel": channel, "created": created})
}
