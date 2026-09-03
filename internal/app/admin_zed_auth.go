package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/zedauth"

	"github.com/gin-gonic/gin"
)

var zedChannelCreateMu sync.Mutex

type zedLoginResult struct {
	credential *zedauth.Credential
	models     []string
}

type zedOAuthStartRequest struct {
	SystemID string `json:"system_id"`
}

func (r *zedOAuthStartRequest) Validate() error {
	if r == nil {
		return errors.New("zed OAuth start request is required")
	}
	systemID, err := zedauth.NormalizeSystemID(r.SystemID)
	if err != nil {
		return err
	}
	r.SystemID = systemID
	return nil
}

func newZedOAuthManager(service *zedauth.Service, store storage.Store, invalidate func(int64)) *codexOAuthManager {
	manager := &codexOAuthManager{
		provider: "Zed", callbackPath: "/", listenAddr: "127.0.0.1:0",
		timeout: codexOAuthTimeout, now: time.Now,
		sessions: make(map[string]*codexOAuthSession), invalidate: invalidate,
	}
	manager.parseCallbackRequest = func(request *http.Request, state string) (codexOAuthResult, error) {
		rawURL := "http://" + request.Host + request.URL.RequestURI()
		return parseZedOAuthCallbackURL(rawURL, state)
	}
	manager.parseSubmittedURL = parseZedOAuthCallbackURL
	manager.prepareWithHint = func(redirectURI, requestedSystemID string) (string, string, func(context.Context, string) (any, error), func(context.Context, any) (int64, error), error) {
		if service == nil || store == nil {
			return "", "", nil, nil, errors.New("oauth: Zed is unavailable")
		}
		callbackURL, err := url.Parse(redirectURI)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("parse Zed callback URL: %w", err)
		}
		port, err := strconv.Atoi(callbackURL.Port())
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("parse Zed callback port: %w", err)
		}
		login, err := zedauth.NewLogin()
		if err != nil {
			return "", "", nil, nil, err
		}
		authorizationURL, err := login.AuthorizationURL(port)
		if err != nil {
			return "", "", nil, nil, err
		}
		state, err := codexauth.GenerateState()
		if err != nil {
			return "", "", nil, nil, err
		}
		systemID, err := zedauth.NormalizeSystemID(requestedSystemID)
		if err != nil {
			return "", "", nil, nil, err
		}
		if systemID == "" {
			systemID, err = zedauth.ResolveSystemID(context.Background())
			if err != nil {
				return "", "", nil, nil, err
			}
		}
		exchange := func(ctx context.Context, rawCallbackURL string) (any, error) {
			callback, parseErr := login.DecryptCallbackURL(rawCallbackURL)
			if parseErr != nil {
				return nil, parseErr
			}
			existingSystemID, lookupErr := findZedSystemID(ctx, store, callback.UserID)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if existingSystemID != "" {
				systemID = existingSystemID
			}
			// Keep the installation header optional so Zed can enforce trial
			// policy instead of ccLoad rejecting the native credential early.
			credential, credentialErr := zedauth.NewCredential(callback.UserID, systemID, callback.NativeCredential)
			if credentialErr != nil {
				return nil, credentialErr
			}
			minted, mintErr := service.MintLLMToken(ctx, credential)
			if mintErr != nil {
				return nil, mintErr
			}
			models, modelsErr := service.FetchModels(ctx, minted)
			if modelsErr != nil {
				return nil, modelsErr
			}
			account, accountErr := service.FetchAccount(ctx, minted)
			if accountErr != nil {
				return nil, accountErr
			}
			minted.Username = account.Username
			if account.GitHubUserLogin != "" {
				minted.GitHubUserLogin = account.GitHubUserLogin
			}
			return &zedLoginResult{credential: minted, models: models}, nil
		}
		commit := func(ctx context.Context, raw any) (int64, error) {
			result, ok := raw.(*zedLoginResult)
			if !ok || result == nil || result.credential == nil {
				return 0, errors.New("oauth: Zed exchange returned an invalid credential")
			}
			channel, _, persistErr := createOrUpdateZedChannel(ctx, store, result.credential, result.models)
			if persistErr != nil {
				return 0, persistErr
			}
			return channel.ID, nil
		}
		return state, authorizationURL, exchange, commit, nil
	}
	return manager
}

func parseZedOAuthCallbackURL(rawURL, state string) (codexOAuthResult, error) {
	callbackURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !callbackURL.IsAbs() || !strings.EqualFold(callbackURL.Scheme, "http") {
		return codexOAuthResult{}, errors.New("invalid Zed callback URL")
	}
	host := callbackURL.Hostname()
	isLoopback := strings.EqualFold(host, "localhost")
	if !isLoopback {
		ip := net.ParseIP(host)
		isLoopback = ip != nil && ip.IsLoopback()
	}
	if !isLoopback || callbackURL.Path != "/" {
		return codexOAuthResult{}, errors.New("zed callback URL must use a loopback host and root path")
	}
	if strings.TrimSpace(callbackURL.Query().Get("user_id")) == "" || strings.TrimSpace(callbackURL.Query().Get("access_token")) == "" {
		return codexOAuthResult{}, errors.New("zed callback is missing user_id or access_token")
	}
	return codexOAuthResult{code: callbackURL.String(), state: state}, nil
}

func findZedSystemID(ctx context.Context, store storage.Store, userID string) (string, error) {
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return "", fmt.Errorf("list channels for Zed installation identity: %w", err)
	}
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesZedOAuth() {
			continue
		}
		credential, parseErr := zedauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr == nil && credential.UserID == userID {
			return credential.SystemID, nil
		}
	}
	return "", nil
}

func createOrUpdateZedChannel(ctx context.Context, store storage.Store, credential *zedauth.Credential, models []string) (*model.Config, bool, error) {
	if store == nil || credential == nil {
		return nil, false, errors.New("persist Zed credential: unavailable")
	}
	if len(zedModelEntries(models)) == 0 {
		return nil, false, errors.New("persist Zed credential: model catalog is empty")
	}
	zedChannelCreateMu.Lock()
	defer zedChannelCreateMu.Unlock()
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list channels for Zed credential: %w", err)
	}
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesZedOAuth() {
			continue
		}
		existing, parseErr := zedauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil || existing.UserID != credential.UserID {
			continue
		}
		return updateExistingZedChannel(ctx, store, cfg, existing, credential, models)
	}
	payload, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	name := uniqueZedChannelName(configs, zedChannelBaseName(credential))
	created, err := store.CreateConfig(ctx, newZedOAuthChannel(name, payload, models))
	if err != nil {
		return nil, false, fmt.Errorf("create Zed channel: %w", err)
	}
	return created, true, nil
}

func updateExistingZedChannel(ctx context.Context, store storage.Store, cfg *model.Config, existing, replacement *zedauth.Credential, models []string) (*model.Config, bool, error) {
	for {
		next := zedauth.CloneCredential(replacement)
		next.SystemID = existing.SystemID
		next.OAuthUsage = append([]byte(nil), existing.OAuthUsage...)
		payload, err := next.JSON()
		if err != nil {
			return nil, false, err
		}
		updated, err := store.CompareAndSwapOAuthCredential(ctx, cfg.ID, model.AuthTypeZedOAuth, cfg.OAuthCredential, payload)
		if err != nil {
			return nil, false, err
		}
		if updated {
			entries := zedModelEntries(models)
			modelUpdated, err := store.UpdateOAuthModelStateIfCredentialMatches(ctx, cfg.ID, model.AuthTypeZedOAuth, payload, entries, cfg.ScheduledCheckModel)
			if err != nil {
				return nil, false, fmt.Errorf("update Zed model catalog: %w", err)
			}
			if !modelUpdated {
				return nil, false, errors.New("zed credential changed while updating model catalog")
			}
			if err := store.ResetChannelCooldown(ctx, cfg.ID); err != nil {
				return nil, false, fmt.Errorf("clear Zed channel cooldown after reauthorization: %w", err)
			}
			persisted, err := store.UpdateChannelEnabled(ctx, cfg.ID, true)
			return persisted, false, err
		}
		cfg, err = store.GetConfig(ctx, cfg.ID)
		if err != nil {
			return nil, false, fmt.Errorf("reload Zed credential after concurrent update: %w", err)
		}
		if !cfg.UsesZedOAuth() {
			return nil, false, errors.New("zed credential changed provider during reauthorization")
		}
		existing, err = zedauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil || existing.UserID != replacement.UserID {
			return nil, false, errors.New("zed credential changed identity during reauthorization")
		}
	}
}

func newZedOAuthChannel(name, credentialJSON string, models []string) *model.Config {
	return &model.Config{
		Name: name, AuthType: model.AuthTypeZedOAuth, OAuthCredential: credentialJSON,
		URLs:                  model.ChannelURLs{{URL: zedauth.CompletionsURL, Exact: true, Protocols: []string{"codex"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0, Enabled: true, CostMultiplier: 1,
		ModelEntries: zedModelEntries(models),
	}
}

func zedModelEntries(models []string) []model.ModelEntry {
	entries := make([]model.ModelEntry, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, name := range models {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		entries = append(entries, model.ModelEntry{Model: name})
	}
	sortOAuthModelEntries(entries)
	return entries
}

func zedChannelBaseName(credential *zedauth.Credential) string {
	if credential != nil {
		if username := strings.TrimSpace(credential.Username); username != "" {
			return "Zed-" + username
		}
		if login := strings.TrimSpace(credential.GitHubUserLogin); login != "" {
			return "Zed-" + login
		}
		if userID := strings.TrimSpace(credential.UserID); userID != "" {
			return "Zed-" + userID
		}
	}
	return "Zed-OAuth"
}

func uniqueZedChannelName(configs []*model.Config, base string) string {
	used := make(map[string]struct{}, len(configs))
	for _, cfg := range configs {
		if cfg != nil {
			used[strings.ToLower(strings.TrimSpace(cfg.Name))] = struct{}{}
		}
	}
	if _, ok := used[strings.ToLower(base)]; !ok {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s (%d)", base, suffix)
		if _, ok := used[strings.ToLower(candidate)]; !ok {
			return candidate
		}
	}
}

// HandleStartZedOAuth starts a native Zed browser authorization session.
func (s *Server) HandleStartZedOAuth(c *gin.Context) {
	if s.zedOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Zed OAuth is unavailable")
		return
	}
	var request zedOAuthStartRequest
	if c.Request.ContentLength != 0 {
		if err := BindAndValidate(c, &request); err != nil {
			RespondError(c, http.StatusBadRequest, err)
			return
		}
	}
	loginURL, state, err := s.zedOAuth.startWithHint(request.SystemID)
	if err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"url": loginURL, "state": state, "status": "pending"})
}

// HandleZedOAuthStatus returns the state of a native Zed authorization session.
func (s *Server) HandleZedOAuthStatus(c *gin.Context) {
	if s.zedOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Zed OAuth is unavailable")
		return
	}
	status, ok := s.zedOAuth.status(c.Query("state"))
	if !ok {
		RespondErrorMsg(c, http.StatusNotFound, "Zed OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelZedOAuth cancels a pending native Zed authorization session.
func (s *Server) HandleCancelZedOAuth(c *gin.Context) {
	var request codexOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.zedOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Zed OAuth is unavailable")
		return
	}
	if err := s.zedOAuth.cancel(request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": strings.TrimSpace(request.State), "status": "cancelled"})
}

// HandleSubmitZedOAuthCallback submits a copied loopback callback URL.
func (s *Server) HandleSubmitZedOAuthCallback(c *gin.Context) {
	var request codexOAuthCallbackRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.zedOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Zed OAuth is unavailable")
		return
	}
	state, err := s.zedOAuth.submitCallbackURL(request.CallbackURL)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": state, "status": "accepted"})
}

// HandleRefreshZedCredential forces one channel's short-lived LLM token refresh.
func (s *Server) HandleRefreshZedCredential(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}
	if !cfg.UsesZedOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use Zed OAuth")
		return
	}
	if s.zedCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Zed credential refresh is unavailable")
		return
	}
	credential, err := s.zedCredentials.credential(c.Request.Context(), cfg, true)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}
