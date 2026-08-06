package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

const (
	antigravityDailyBaseURL   = "https://daily-cloudcode-pa.googleapis.com"
	antigravityProdBaseURL    = "https://cloudcode-pa.googleapis.com"
	maxAntigravityImportBytes = 1 << 20
)

var antigravityOAuthDefaultModels = []string{
	"claude-opus-4-6-thinking",
	"claude-sonnet-4-6",
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
		if err := store.UpdateOAuthCredential(ctx, cfg.ID, credentialJSON); err != nil {
			return nil, err
		}
		cfg.ModelEntries = antigravityOAuthModelEntries()
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
		URLs: model.ChannelURLs{
			{URL: antigravityDailyBaseURL, Protocols: []string{"gemini"}},
			{URL: antigravityProdBaseURL, Protocols: []string{"gemini"}},
		},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0, Enabled: true, CostMultiplier: 1,
		ModelEntries: antigravityOAuthModelEntries(),
	}
}

func antigravityOAuthModelEntries() []model.ModelEntry {
	entries := make([]model.ModelEntry, len(antigravityOAuthDefaultModels))
	for i, name := range antigravityOAuthDefaultModels {
		entries[i] = model.ModelEntry{Model: name}
	}
	return entries
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

func readAntigravityCredentialFile(file *multipart.FileHeader) (*antigravityauth.Credential, error) {
	if file == nil || file.Size <= 0 || file.Size > maxAntigravityImportBytes {
		return nil, errors.New("credential file size is invalid")
	}
	opened, err := file.Open()
	if err != nil {
		return nil, errors.New("open credential file failed")
	}
	raw, readErr := io.ReadAll(io.LimitReader(opened, maxAntigravityImportBytes+1))
	closeErr := opened.Close()
	if readErr != nil || len(raw) > maxAntigravityImportBytes {
		return nil, errors.New("failed to read credential")
	}
	if closeErr != nil {
		return nil, errors.New("close credential file failed")
	}
	return antigravityauth.ParseCredential(raw)
}

func createImportedAntigravityChannel(ctx context.Context, store storage.Store, credential *antigravityauth.Credential) (string, bool, error) {
	credentialJSON, err := credential.JSON()
	if err != nil {
		return "", false, err
	}
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return "", false, fmt.Errorf("list channels for Antigravity credential: %w", err)
	}
	name := antigravityChannelBaseName(credential)
	for _, cfg := range configs {
		if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Name), name) {
			return cfg.Name, false, nil
		}
	}
	created, err := store.CreateConfig(ctx, newAntigravityOAuthChannel(name, credentialJSON))
	if err != nil {
		return "", false, fmt.Errorf("create Antigravity channel: %w", err)
	}
	return created.Name, true, nil
}

// HandleImportAntigravityCredential imports CLIProxyAPI-compatible credential files.
func (s *Server) HandleImportAntigravityCredential(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "credential files are required")
		return
	}
	files := form.File["files"]
	if len(files) == 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "credential files are required")
		return
	}
	summary := codexCredentialImportSummary{Results: make([]codexCredentialImportResult, 0, len(files))}
	for _, file := range files {
		result := codexCredentialImportResult{FileName: file.Filename}
		credential, parseErr := readAntigravityCredentialFile(file)
		if parseErr == nil && (credential.Email == "" || credential.ProjectID == "") {
			credential, parseErr = s.antigravityService.CompleteCredential(c.Request.Context(), credential)
		}
		if parseErr != nil {
			result.Status, result.Error = "failed", parseErr.Error()
			summary.Failed++
			summary.Results = append(summary.Results, result)
			continue
		}
		channelName, created, createErr := createImportedAntigravityChannel(c.Request.Context(), s.store, credential)
		switch {
		case createErr != nil:
			result.Status, result.Error = "failed", createErr.Error()
			summary.Failed++
		case created:
			result.Status, result.ChannelName = "created", channelName
			summary.Created++
		default:
			result.Status, result.ChannelName = "skipped", channelName
			summary.Skipped++
		}
		summary.Results = append(summary.Results, result)
	}
	if summary.Created > 0 {
		s.InvalidateChannelListCache()
	}
	RespondJSON(c, http.StatusOK, summary)
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
	credential, err := s.antigravityCredentials.credential(c.Request.Context(), cfg, true)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}
