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
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

const maxCursorCredentialImportBytes = 1 << 16

var cursorChannelCreateMu sync.Mutex

type cursorCredentialImportRequest struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	APIKey       string `json:"api_key"`
}

func (r *cursorCredentialImportRequest) Validate() error {
	r.AccessToken = strings.TrimSpace(r.AccessToken)
	r.RefreshToken = strings.TrimSpace(r.RefreshToken)
	r.APIKey = strings.TrimSpace(r.APIKey)
	if r.AccessToken == "" && r.APIKey == "" {
		return errors.New("access_token or api_key is required")
	}
	if strings.ContainsAny(r.AccessToken, " \r\n\t") || strings.ContainsAny(r.RefreshToken, " \r\n\t") ||
		strings.ContainsAny(r.APIKey, " \r\n\t") {
		return errors.New("credential contains invalid characters")
	}
	return nil
}

type cursorCredentialImportResponse struct {
	ChannelID   int64  `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Created     bool   `json:"created"`
	Email       string `json:"email,omitempty"`
}

// HandleImportCursorCredential provisions a Cursor channel from a session
// token pair or a Cursor user API key.
func (s *Server) HandleImportCursorCredential(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxCursorCredentialImportBytes)
	var request cursorCredentialImportRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.store == nil || s.cursorService == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "cursor credential import is unavailable")
		return
	}
	ctx := c.Request.Context()
	credential, err := s.buildCursorCredential(ctx, request)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	cfg, created, err := s.commitCursorCredential(ctx, credential)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, cursorCredentialImportResponse{
		ChannelID: cfg.ID, ChannelName: cfg.Name, Created: created, Email: credential.Email,
	})
}

func (s *Server) buildCursorCredential(
	ctx context.Context,
	request cursorCredentialImportRequest,
) (*cursorauth.Credential, error) {
	credential := &cursorauth.Credential{
		Type: cursorauth.ChannelType, LastRefresh: time.Now().UTC().Format(time.RFC3339),
	}
	if request.APIKey != "" {
		pair, err := s.cursorService.ExchangeAPIKey(ctx, request.APIKey)
		if err != nil {
			return nil, err
		}
		credential.APIKey = request.APIKey
		credential.AccessToken = pair.AccessToken
		credential.RefreshToken = pair.RefreshToken
	} else {
		credential.AccessToken = request.AccessToken
		credential.RefreshToken = request.RefreshToken
	}
	identity, name, err := s.cursorService.FetchIdentity(ctx, credential.AccessToken)
	if err != nil {
		return nil, err
	}
	credential.UserID, credential.Email, credential.Name = identity.UserID, identity.Email, name
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return credential, nil
}

func (s *Server) commitCursorCredential(
	ctx context.Context,
	credential *cursorauth.Credential,
) (*model.Config, bool, error) {
	cfg, created, err := createOrUpdateCursorChannel(
		ctx, s.store, credential, s.cursorChannelModels(ctx, credential.AccessToken),
	)
	if err != nil {
		return nil, false, err
	}
	s.cursorCredentials.invalidate(cfg.ID)
	s.invalidateChannelRelatedCache(cfg.ID)
	s.InvalidateChannelListCache()
	return cfg, created, nil
}

func (s *Server) cursorChannelModels(ctx context.Context, accessToken string) []string {
	if s == nil || s.cursorService == nil {
		return cursorauth.DefaultModels
	}
	models, err := s.cursorService.ListModels(ctx, accessToken)
	if err != nil || len(models) == 0 {
		if err != nil {
			log.Printf("[WARN] Cursor 模型目录不可用，新渠道使用内置列表: %v", err)
		}
		return cursorauth.DefaultModels
	}
	return models
}

func createOrUpdateCursorChannel(
	ctx context.Context,
	store storage.Store,
	credential *cursorauth.Credential,
	models []string,
) (*model.Config, bool, error) {
	if store == nil || credential == nil {
		return nil, false, errors.New("persist cursor credential: unavailable")
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}

	cursorChannelCreateMu.Lock()
	defer cursorChannelCreateMu.Unlock()
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list channels for cursor credential: %w", err)
	}
	if existing, found, updateErr := updateExistingCursorChannel(ctx, store, configs, credential); found || updateErr != nil {
		return existing, false, updateErr
	}
	name := uniqueCursorChannelName(configs, cursorChannelBaseName(credential))
	created, err := store.CreateConfig(ctx, newCursorOAuthChannel(name, credentialJSON, models))
	if err != nil {
		return nil, false, fmt.Errorf("create cursor channel: %w", err)
	}
	return created, true, nil
}

func updateExistingCursorChannel(
	ctx context.Context,
	store storage.Store,
	configs []*model.Config,
	credential *cursorauth.Credential,
) (*model.Config, bool, error) {
	identity := credential.Identity()
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesCursorOAuth() || strings.TrimSpace(cfg.OAuthCredential) == "" {
			continue
		}
		existing, parseErr := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			continue
		}
		matched := existing.AccessToken == credential.AccessToken
		if !identity.IsZero() && !existing.Identity().IsZero() {
			matched = existing.Identity().Matches(identity)
		}
		if !matched {
			continue
		}
		merged, mergeErr := existing.MergeRefresh(credential)
		if mergeErr != nil {
			return nil, true, mergeErr
		}
		mergedJSON, encodeErr := merged.JSON()
		if encodeErr != nil {
			return nil, true, encodeErr
		}
		updated, updateErr := store.CompareAndSwapOAuthCredential(
			ctx, cfg.ID, model.AuthTypeCursorOAuth, cfg.OAuthCredential, mergedJSON,
		)
		if updateErr != nil {
			return nil, true, updateErr
		}
		if !updated {
			return nil, true, errors.New("cursor credential changed during reauthorization")
		}
		persisted, getErr := store.GetConfig(ctx, cfg.ID)
		return persisted, true, getErr
	}
	return nil, false, nil
}

func cursorChannelBaseName(credential *cursorauth.Credential) string {
	if credential != nil {
		if email := strings.TrimSpace(credential.Email); email != "" {
			return "Cursor-" + email
		}
	}
	return "Cursor-CLI"
}

func uniqueCursorChannelName(configs []*model.Config, base string) string {
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

func newCursorOAuthChannel(name, credentialJSON string, modelNames []string) *model.Config {
	if len(modelNames) == 0 {
		modelNames = cursorauth.DefaultModels
	}
	models := make([]model.ModelEntry, len(modelNames))
	for i, modelName := range modelNames {
		models[i] = model.ModelEntry{Model: modelName}
	}
	return &model.Config{
		Name: name, AuthType: model.AuthTypeCursorOAuth, OAuthCredential: credentialJSON,
		URLs:                  model.ChannelURLs{{URL: cursorauth.APIBaseURL, Protocols: []string{"anthropic", "openai"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0,
		Enabled:               true,
		CostMultiplier:        1,
		ModelEntries:          models,
	}
}
