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

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/zaiauth"

	"github.com/gin-gonic/gin"
)

// Z.ai Coding Plan channel provisioning.
//
// Both entry points — the hosted ZCode authorization and a directly supplied
// Coding Plan key — end here, so every Z.ai channel is created with the same
// upstream contract.

const (
	maxZAICredentialImportBytes = 1 << 16
	// zaiCodingPlanName labels the plan in usage summaries. Z.ai exposes no
	// plan tier on the quota endpoint, so the label is the plan itself.
	zaiCodingPlanName = "Coding Plan"
)

var zaiChannelCreateMu sync.Mutex

type zaiCredentialImportRequest struct {
	// APIKey is a Coding Plan key taken from the Z.ai console.
	APIKey string `json:"api_key"`
	// AccessToken is a ZCode account authorization; ccLoad derives the Coding
	// Plan key from it exactly like the official client.
	AccessToken string `json:"access_token"`
}

func (r *zaiCredentialImportRequest) Validate() error {
	r.APIKey = strings.TrimSpace(r.APIKey)
	r.AccessToken = strings.TrimSpace(r.AccessToken)
	if r.APIKey == "" && r.AccessToken == "" {
		return errors.New("api_key or access_token is required")
	}
	if strings.ContainsAny(r.APIKey, " \r\n\t") || strings.ContainsAny(r.AccessToken, " \r\n\t") {
		return errors.New("credential contains invalid characters")
	}
	return nil
}

type zaiCredentialImportResponse struct {
	ChannelID   int64  `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Created     bool   `json:"created"`
	Email       string `json:"email,omitempty"`
	UpstreamURL string `json:"upstream_url"`
}

// HandleImportZAICredential provisions a Coding Plan channel from a key or a
// ZCode account authorization.
func (s *Server) HandleImportZAICredential(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxZAICredentialImportBytes)
	var request zaiCredentialImportRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.store == nil || s.zaiService == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "z.ai credential import is unavailable")
		return
	}
	ctx := c.Request.Context()
	credential, err := s.buildZAICredential(ctx, request)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	cfg, created, err := s.commitZAICredential(ctx, credential)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	upstream := ""
	if len(cfg.URLs) > 0 {
		upstream = cfg.URLs[0].URL
	}
	RespondJSON(c, http.StatusOK, zaiCredentialImportResponse{
		ChannelID: cfg.ID, ChannelName: cfg.Name, Created: created,
		Email: credential.Email, UpstreamURL: upstream,
	})
}

// HandleRefreshZAICredential re-resolves the Coding Plan key from the stored
// ZCode account authorization. Key-only imports cannot be refreshed.
func (s *Server) HandleRefreshZAICredential(c *gin.Context) {
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
	if !cfg.UsesZAIOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use Z.ai OAuth")
		return
	}
	stored, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if stored.AccessToken == "" {
		RespondErrorMsg(c, http.StatusConflict, "z.ai Coding Plan key cannot be re-resolved without an account authorization")
		return
	}
	if s.zaiCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Z.ai credential refresh is unavailable")
		return
	}
	credential, err := s.zaiCredentials.credential(c.Request.Context(), cfg, true)
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}

// buildZAICredential turns admin input into a validated credential. A supplied
// access token always wins: it can re-mint the Coding Plan key later.
func (s *Server) buildZAICredential(
	ctx context.Context,
	request zaiCredentialImportRequest,
) (*zaiauth.Credential, error) {
	credential := &zaiauth.Credential{
		Type: zaiauth.ChannelType, APIKey: request.APIKey, AccessToken: request.AccessToken,
		LastRefresh: time.Now().UTC().Format(time.RFC3339),
	}
	if request.AccessToken != "" {
		apiKey, identity, err := s.zaiService.ResolveCodingPlanAPIKey(ctx, request.AccessToken)
		if err != nil {
			return nil, err
		}
		credential.APIKey = apiKey
		credential.UserID, credential.Email = identity.UserID, identity.Email
	} else if err := s.zaiService.ValidateAPIKey(ctx, credential.APIKey); err != nil {
		return nil, err
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return credential, nil
}

// commitZAICredential persists the credential and returns its channel.
func (s *Server) commitZAICredential(
	ctx context.Context,
	credential *zaiauth.Credential,
) (*model.Config, bool, error) {
	cfg, created, err := createOrUpdateZAIChannel(
		ctx, s.store, credential, s.zaiCodingPlanBaseURL(ctx), s.zaiChannelModels(ctx, credential.APIKey),
	)
	if err != nil {
		return nil, false, err
	}
	s.zaiCredentials.invalidate(cfg.ID)
	s.invalidateChannelRelatedCache(cfg.ID)
	s.InvalidateChannelListCache()
	return cfg, created, nil
}

// zaiChannelModels seeds a new channel from the live Coding Plan catalog so it
// starts with whatever the account can actually call today.
func (s *Server) zaiChannelModels(ctx context.Context, apiKey string) []string {
	models, err := s.zaiCodingPlanModels(ctx, apiKey)
	if err != nil || len(models) == 0 {
		if err != nil {
			log.Printf("[WARN] Z.ai 模型目录不可用，新渠道使用内置列表: %v", err)
		}
		return zaiauth.DefaultModels
	}
	return models
}

// zaiCodingPlanBaseURL resolves the endpoint ZCode currently routes Coding Plan
// traffic to. Routing is an optimization: an unreachable table falls back to the
// endpoint ZCode shipped with.
func (s *Server) zaiCodingPlanBaseURL(ctx context.Context) string {
	if s == nil || s.zaiService == nil {
		return zaiauth.CodingPlanProxyBaseURL
	}
	base, err := s.zaiService.ResolveProxyBaseURL(ctx)
	if err != nil || strings.TrimSpace(base) == "" {
		return zaiauth.CodingPlanProxyBaseURL
	}
	return base
}

func createOrUpdateZAIChannel(
	ctx context.Context,
	store storage.Store,
	credential *zaiauth.Credential,
	baseURL string,
	models []string,
) (*model.Config, bool, error) {
	if store == nil || credential == nil {
		return nil, false, errors.New("persist z.ai credential: unavailable")
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}

	zaiChannelCreateMu.Lock()
	defer zaiChannelCreateMu.Unlock()
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list channels for z.ai credential: %w", err)
	}
	if existing, found, updateErr := updateExistingZAIChannel(ctx, store, configs, credential); found || updateErr != nil {
		return existing, false, updateErr
	}
	name := uniqueZAIChannelName(configs, zaiChannelBaseName(credential))
	created, err := store.CreateConfig(ctx, newZAIOAuthChannel(name, credentialJSON, baseURL, models))
	if err != nil {
		return nil, false, fmt.Errorf("create z.ai channel: %w", err)
	}
	return created, true, nil
}

// updateExistingZAIChannel re-authorizes the channel that already holds this
// account instead of creating a duplicate.
func updateExistingZAIChannel(
	ctx context.Context,
	store storage.Store,
	configs []*model.Config,
	credential *zaiauth.Credential,
) (*model.Config, bool, error) {
	identity := credential.Identity()
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesZAIOAuth() || strings.TrimSpace(cfg.OAuthCredential) == "" {
			continue
		}
		existing, parseErr := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			continue
		}
		// Without an account identity the API key itself is the only stable
		// handle, so re-importing the same key updates in place.
		matched := existing.APIKey == credential.APIKey
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
			ctx, cfg.ID, model.AuthTypeZAIOAuth, cfg.OAuthCredential, mergedJSON,
		)
		if updateErr != nil {
			return nil, true, updateErr
		}
		if !updated {
			return nil, true, errors.New("z.ai credential changed during reauthorization")
		}
		persisted, getErr := store.GetConfig(ctx, cfg.ID)
		return persisted, true, getErr
	}
	return nil, false, nil
}

func zaiChannelBaseName(credential *zaiauth.Credential) string {
	if credential != nil {
		if email := strings.TrimSpace(credential.Email); email != "" {
			return "Z.ai-" + email
		}
	}
	return "Z.ai-CodingPlan"
}

func uniqueZAIChannelName(configs []*model.Config, base string) string {
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

func newZAIOAuthChannel(name, credentialJSON, baseURL string, modelNames []string) *model.Config {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = zaiauth.CodingPlanProxyBaseURL
	}
	if len(modelNames) == 0 {
		modelNames = zaiauth.DefaultModels
	}
	models := make([]model.ModelEntry, len(modelNames))
	for i, modelName := range modelNames {
		models[i] = model.ModelEntry{Model: modelName}
	}
	return &model.Config{
		Name: name, AuthType: model.AuthTypeZAIOAuth, OAuthCredential: credentialJSON,
		URLs:                  model.ChannelURLs{{URL: baseURL, Protocols: []string{"anthropic"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0,
		Enabled:               true,
		CostMultiplier:        1,
		ModelEntries:          models,
	}
}
