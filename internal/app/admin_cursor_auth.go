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
	APIKey string `json:"api_key"`
}

func (r *cursorCredentialImportRequest) Validate() error {
	r.APIKey = strings.TrimSpace(r.APIKey)
	if r.APIKey == "" {
		return errors.New("api_key is required")
	}
	if strings.ContainsAny(r.APIKey, " \r\n\t") {
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

// HandleImportCursorCredential provisions a Cursor channel from a user API key.
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

// HandleRefreshCursorCredential remints the session from the stored user API key.
func (s *Server) HandleRefreshCursorCredential(c *gin.Context) {
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
	if !cfg.UsesCursorOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use Cursor OAuth")
		return
	}
	if s.cursorCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Cursor credential refresh is unavailable")
		return
	}
	credential, err := s.cursorCredentials.credential(c.Request.Context(), cfg, true)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}

func (s *Server) buildCursorCredential(
	ctx context.Context,
	request cursorCredentialImportRequest,
) (*cursorauth.Credential, error) {
	credential := &cursorauth.Credential{
		Type: cursorauth.ChannelType, LastRefresh: time.Now().UTC().Format(time.RFC3339),
	}
	pair, err := s.cursorService.ExchangeAPIKey(ctx, request.APIKey)
	if err != nil {
		return nil, err
	}
	credential.APIKey = request.APIKey
	credential.AccessToken = pair.AccessToken
	credential.RefreshToken = pair.RefreshToken
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
		ctx, s.store, credential, s.cursorChannelModels(ctx, credential),
	)
	if err != nil {
		return nil, false, err
	}
	s.cursorCredentials.invalidate(cfg.ID)
	s.invalidateChannelRelatedCache(cfg.ID)
	s.InvalidateChannelListCache()
	s.cursorBridgeRequired.Store(true)
	s.StartCursorSDKBridge()
	return cfg, created, nil
}

func (s *Server) cursorChannelModels(ctx context.Context, credential *cursorauth.Credential) []string {
	models, err := s.listCursorSDKModels(ctx, credential)
	if err != nil || len(models) == 0 {
		if err != nil {
			log.Printf("[WARN] Cursor SDK 模型目录不可用，新渠道使用 default: %v", err)
		}
		return cursorauth.DefaultModels
	}
	return models
}

func (s *Server) listCursorSDKModels(
	ctx context.Context,
	credential *cursorauth.Credential,
) ([]string, error) {
	if credential == nil || strings.TrimSpace(credential.APIKey) == "" {
		return nil, cursorauth.ErrMissingAPIKey
	}
	lister, ok := s.cursorRunnerSnapshot().(cursorauth.ModelLister)
	if !ok || lister == nil {
		return nil, errors.New("cursor SDK model catalog is unavailable")
	}
	return lister.ListModels(ctx, credential.APIKey)
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
	return &model.Config{
		Name: name, AuthType: model.AuthTypeCursorOAuth, OAuthCredential: credentialJSON,
		URLs:                  model.ChannelURLs{{URL: cursorauth.APIBaseURL, Protocols: []string{"anthropic", "openai"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0,
		Enabled:               true,
		CostMultiplier:        1,
		ModelEntries:          oauthModelEntries(modelNames),
	}
}
