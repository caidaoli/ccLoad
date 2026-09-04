package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

const (
	antigravityDailyBaseURL        = "https://daily-cloudcode-pa.googleapis.com"
	antigravityProdBaseURL         = "https://cloudcode-pa.googleapis.com"
	antigravitySandboxDailyBaseURL = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityOAuthMaxConcurrency = 3
)

// 导入凭证和动态模型发现必须共享这一份 Antigravity 模型目录。
var antigravityOAuthDefaultModels = []string{
	"claude-opus-4-6-thinking",
	"claude-sonnet-4-6",
	"gemini-3.8-flash-high",
	"gemini-3.8-flash-medium",
	"gemini-3.7-flash-high",
	"gemini-3.6-flash-high",
	"gemini-3-flash",
	"gemini-3-flash-agent",
	"gemini-3.1-flash-image",
	"gemini-pro-agent",
	"gemini-3.1-pro-low",
	"gpt-oss-120b-medium",
	"gemini-3.1-flash-lite",
	"gemini-3.5-flash-low",
	"gemini-3.5-flash-extra-low",
}

func createAntigravityChannel(ctx context.Context, store storage.Store, credential *antigravityauth.Credential) (*model.Config, error) {
	credentialJSON, err := credential.JSON()
	if err != nil {
		return nil, err
	}
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels for Antigravity credential: %w", err)
	}
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesAntigravityOAuth() || cfg.OAuthCredential == "" {
			continue
		}
		existing, parseErr := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil || !sameAntigravityIdentity(existing, credential) {
			continue
		}
		for {
			currentCfg, getErr := store.GetConfig(ctx, cfg.ID)
			if getErr != nil {
				return nil, getErr
			}
			current, parseErr := antigravityauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil || !currentCfg.UsesAntigravityOAuth() || !sameAntigravityIdentity(current, credential) {
				return nil, errors.New("antigravity credential changed identity during reauthorization")
			}
			merged, mergeErr := current.MergeRefresh(credential)
			if mergeErr != nil {
				return nil, mergeErr
			}
			mergedJSON, encodeErr := merged.JSON()
			if encodeErr != nil {
				return nil, encodeErr
			}
			credentialUpdated, updateErr := store.CompareAndSwapOAuthCredential(
				ctx, currentCfg.ID, model.AuthTypeAntigravityOAuth, currentCfg.OAuthCredential, mergedJSON,
			)
			if updateErr != nil {
				return nil, updateErr
			}
			if !credentialUpdated {
				continue
			}
			cfg, err = store.GetConfig(ctx, currentCfg.ID)
			if err != nil {
				return nil, err
			}
			break
		}
		cfg.ModelEntries = antigravityOAuthModelEntries()
		cfg.MaxConcurrency = antigravityOAuthMaxConcurrency
		updated, err := store.UpdateConfig(ctx, cfg.ID, cfg)
		if err != nil {
			return nil, fmt.Errorf("update Antigravity channel: %w", err)
		}
		return updated, nil
	}
	created, err := store.CreateConfig(ctx, newAntigravityOAuthChannel(uniqueAntigravityChannelName(configs, credential), credentialJSON))
	if err != nil {
		return nil, fmt.Errorf("create Antigravity channel: %w", err)
	}
	return created, nil
}

func newAntigravityOAuthChannel(name, credentialJSON string) *model.Config {
	return &model.Config{
		Name: name, AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: credentialJSON,
		URLs:                  antigravityOAuthPersistedURLs(),
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0, Enabled: true, CostMultiplier: 1,
		MaxConcurrency: antigravityOAuthMaxConcurrency,
		ModelEntries:   antigravityOAuthModelEntries(),
	}
}

// antigravityOAuthPersistedURLs is the single endpoint written for newly
// created OAuth channels. Runtime fallback stays separate so the editor shows
// only the primary endpoint instead of provider-internal fallback targets.
func antigravityOAuthPersistedURLs() model.ChannelURLs {
	return model.ChannelURLs{
		{URL: antigravityDailyBaseURL, Protocols: []string{"gemini"}},
	}
}

func antigravityOAuthDefaultURLs() model.ChannelURLs {
	return model.ChannelURLs{
		{URL: antigravityDailyBaseURL, Protocols: []string{"gemini"}},
		{URL: antigravitySandboxDailyBaseURL, Protocols: []string{"gemini"}},
	}
}

func antigravityOAuthModelEntries() []model.ModelEntry {
	return oauthModelEntries(antigravityOAuthDefaultModels)
}

func antigravityOAuthAvailableModels(upstreamModels []string) []string {
	available := make(map[string]struct{}, len(upstreamModels))
	for _, name := range upstreamModels {
		available[name] = struct{}{}
	}
	models := make([]string, 0, len(antigravityOAuthDefaultModels))
	for _, name := range antigravityOAuthDefaultModels {
		if _, ok := available[name]; ok {
			models = append(models, name)
		}
	}
	return models
}

func sameAntigravityIdentity(a, b *antigravityauth.Credential) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Email != "" && b.Email != "" {
		return strings.EqualFold(a.Email, b.Email)
	}
	return a.ProjectID != "" && a.ProjectID == b.ProjectID
}

func antigravityChannelBaseName(credential *antigravityauth.Credential) string {
	identity := strings.TrimSpace(credential.Email)
	if identity == "" {
		identity = strings.TrimSpace(credential.ProjectID)
	}
	if identity == "" {
		identity = "OAuth"
	}
	return "Antigravity-" + identity
}

func uniqueAntigravityChannelName(configs []*model.Config, credential *antigravityauth.Credential) string {
	base := antigravityChannelBaseName(credential)
	used := make(map[string]struct{}, len(configs))
	for _, cfg := range configs {
		if cfg != nil {
			used[strings.ToLower(strings.TrimSpace(cfg.Name))] = struct{}{}
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

// HandleStartAntigravityOAuth starts a local Antigravity OAuth callback flow.
func (s *Server) HandleStartAntigravityOAuth(c *gin.Context) {
	authURL, state, err := s.antigravityOAuth.start()
	if err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"url": authURL, "state": state, "status": "pending"})
}

// HandleAntigravityOAuthStatus returns one Antigravity OAuth flow status.
func (s *Server) HandleAntigravityOAuthStatus(c *gin.Context) {
	status, ok := s.antigravityOAuth.status(c.Query("state"))
	if !ok {
		RespondErrorMsg(c, http.StatusNotFound, "Antigravity OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelAntigravityOAuth cancels one pending Antigravity OAuth flow.
func (s *Server) HandleCancelAntigravityOAuth(c *gin.Context) {
	var request codexOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.antigravityOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Antigravity OAuth is unavailable")
		return
	}
	if err := s.antigravityOAuth.cancel(request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": strings.TrimSpace(request.State), "status": "cancelled"})
}

// HandleSubmitAntigravityOAuthCallback accepts a copied loopback callback URL.
func (s *Server) HandleSubmitAntigravityOAuthCallback(c *gin.Context) {
	var request codexOAuthCallbackRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.antigravityOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Antigravity OAuth is unavailable")
		return
	}
	state, err := s.antigravityOAuth.submitCallbackURL(request.CallbackURL)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": state, "status": "accepted"})
}

// HandleImportAntigravityCredential imports CLIProxyAPI-compatible credential files.
func (s *Server) HandleImportAntigravityCredential(c *gin.Context) {
	s.handleImportOAuthCredentials(c, antigravityauth.ChannelType)
}

// HandleRefreshAntigravityCredential forces and persists one credential refresh.
func (s *Server) HandleRefreshAntigravityCredential(c *gin.Context) {
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
	if !cfg.UsesAntigravityOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use Antigravity OAuth")
		return
	}
	if s.antigravityCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Antigravity credential refresh is unavailable")
		return
	}
	credential, err := s.antigravityCredentials.credential(c.Request.Context(), cfg, true)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}
