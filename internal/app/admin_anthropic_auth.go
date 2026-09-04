package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

var anthropicOAuthDefaultModels = []string{
	"claude-fable-5-1",
	"claude-fable-5",
	"claude-opus-4-5-20251101",
	"claude-opus-4-6",
	"claude-opus-4-7",
	"claude-opus-4-8",
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5-20250929",
	"claude-haiku-4-5-20251001",
}

type anthropicOAuthCodeRequest struct {
	State string `json:"state"`
	Code  string `json:"code"`
}

type anthropicCookieAuthRequest struct {
	SessionKey string `json:"session_key"`
}

func (r *anthropicOAuthCodeRequest) Validate() error {
	if strings.TrimSpace(r.State) == "" {
		return errors.New("state is required")
	}
	if strings.TrimSpace(r.Code) == "" {
		return errors.New("code is required")
	}
	return nil
}

func (r *anthropicCookieAuthRequest) Validate() error {
	if strings.TrimSpace(r.SessionKey) == "" {
		return errors.New("session_key is required")
	}
	return nil
}

func newAnthropicOAuthManager(
	service *anthropicauth.Service,
	store storage.Store,
	invalidate func(int64),
) *codexOAuthManager {
	return &codexOAuthManager{
		provider: "Anthropic", redirectURI: anthropicauth.RedirectURI,
		timeout: 30 * time.Minute, now: time.Now,
		sessions: make(map[string]*codexOAuthSession), invalidate: invalidate,
		prepare: func(string) (string, string, func(context.Context, string) (any, error), func(context.Context, any) (int64, error), error) {
			if service == nil || store == nil {
				return "", "", nil, nil, errors.New("oauth: Anthropic is unavailable")
			}
			state, err := anthropicauth.GenerateState()
			if err != nil {
				return "", "", nil, nil, err
			}
			pkce, err := anthropicauth.GeneratePKCE()
			if err != nil {
				return "", "", nil, nil, err
			}
			authURL, err := service.AuthorizationLink(state, pkce)
			if err != nil {
				return "", "", nil, nil, err
			}
			exchange := func(ctx context.Context, code string) (any, error) {
				return service.ExchangeCode(ctx, code, state, pkce)
			}
			commit := func(ctx context.Context, raw any) (int64, error) {
				credential, ok := raw.(*anthropicauth.Credential)
				if !ok || credential == nil {
					return 0, errors.New("oauth: Anthropic exchange returned an invalid credential")
				}
				channel, _, err := createOrUpdateAnthropicChannel(ctx, store, credential)
				if err != nil {
					return 0, err
				}
				return channel.ID, nil
			}
			return state, authURL, exchange, commit, nil
		},
	}
}

func createOrUpdateAnthropicChannel(
	ctx context.Context,
	store storage.Store,
	credential *anthropicauth.Credential,
) (*model.Config, bool, error) {
	credentialJSON, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list channels for Anthropic credential: %w", err)
	}
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesAnthropicOAuth() || cfg.OAuthCredential == "" {
			continue
		}
		existing, parseErr := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil || !sameAnthropicIdentity(existing, credential) {
			continue
		}
		for {
			currentCfg, getErr := store.GetConfig(ctx, cfg.ID)
			if getErr != nil {
				return nil, false, getErr
			}
			current, parseErr := anthropicauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil || !currentCfg.UsesAnthropicOAuth() || !sameAnthropicIdentity(current, credential) {
				return nil, false, errors.New("anthropic credential changed identity during reauthorization")
			}
			merged, mergeErr := current.MergeRefresh(credential)
			if mergeErr != nil {
				return nil, false, mergeErr
			}
			mergedJSON, encodeErr := merged.JSON()
			if encodeErr != nil {
				return nil, false, encodeErr
			}
			updated, updateErr := store.CompareAndSwapOAuthCredential(
				ctx, currentCfg.ID, model.AuthTypeAnthropicOAuth, currentCfg.OAuthCredential, mergedJSON,
			)
			if updateErr != nil {
				return nil, false, updateErr
			}
			if !updated {
				continue
			}
			winner, getErr := store.GetConfig(ctx, currentCfg.ID)
			return winner, false, getErr
		}
	}
	name := uniqueAnthropicChannelName(configs, credential)
	created, err := store.CreateConfig(ctx, newAnthropicOAuthChannel(name, credentialJSON))
	if err != nil {
		return nil, false, fmt.Errorf("create Anthropic channel: %w", err)
	}
	return created, true, nil
}

func newAnthropicOAuthChannel(name, credentialJSON string) *model.Config {
	return &model.Config{
		Name: name, AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON,
		URLs:                  model.ChannelURLs{{URL: anthropicauth.DefaultUpstreamURL, Protocols: []string{"anthropic"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0, Enabled: true, CostMultiplier: 1, ModelEntries: oauthModelEntries(anthropicOAuthDefaultModels),
	}
}

func sameAnthropicIdentity(a, b *anthropicauth.Credential) bool {
	if a == nil || b == nil {
		return false
	}
	if a.AccountUUID != "" && b.AccountUUID != "" {
		return a.AccountUUID == b.AccountUUID
	}
	if a.EmailAddress != "" && b.EmailAddress != "" {
		return strings.EqualFold(a.EmailAddress, b.EmailAddress)
	}
	return false
}

func uniqueAnthropicChannelName(configs []*model.Config, credential *anthropicauth.Credential) string {
	identity := strings.TrimSpace(credential.EmailAddress)
	if identity == "" {
		identity = strings.TrimSpace(credential.AccountUUID)
	}
	if identity == "" {
		identity = strings.TrimSpace(credential.OrgUUID)
	}
	if identity == "" {
		identity = "OAuth"
	}
	base := "Anthropic-" + identity
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

// HandleStartAnthropicOAuth starts one hosted Anthropic PKCE login.
func (s *Server) HandleStartAnthropicOAuth(c *gin.Context) {
	if s.anthropicOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic OAuth is unavailable")
		return
	}
	authURL, state, err := s.anthropicOAuth.start()
	if err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"url": authURL, "state": state, "status": "pending"})
}

// HandleAnthropicOAuthStatus returns one Anthropic login state.
func (s *Server) HandleAnthropicOAuthStatus(c *gin.Context) {
	if s.anthropicOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic OAuth is unavailable")
		return
	}
	status, ok := s.anthropicOAuth.status(c.Query("state"))
	if !ok {
		RespondErrorMsg(c, http.StatusNotFound, "Anthropic OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelAnthropicOAuth cancels one pending Anthropic login.
func (s *Server) HandleCancelAnthropicOAuth(c *gin.Context) {
	var request codexOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.anthropicOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic OAuth is unavailable")
		return
	}
	if err := s.anthropicOAuth.cancel(request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": strings.TrimSpace(request.State), "status": "cancelled"})
}

// HandleSubmitAnthropicOAuthCode accepts the code displayed by Anthropic.
func (s *Server) HandleSubmitAnthropicOAuthCode(c *gin.Context) {
	var request anthropicOAuthCodeRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.anthropicOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic OAuth is unavailable")
		return
	}
	state, err := s.anthropicOAuth.submitAuthorizationCode(request.State, request.Code)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": state, "status": "accepted"})
}

// HandleAnthropicCookieAuth exchanges a claude.ai sessionKey and creates or updates one channel.
func (s *Server) HandleAnthropicCookieAuth(c *gin.Context) {
	var request anthropicCookieAuthRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.anthropicService == nil || s.store == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic Cookie authorization is unavailable")
		return
	}
	credential, err := s.anthropicService.CookieAuth(c.Request.Context(), request.SessionKey)
	request.SessionKey = ""
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	channel, created, err := createOrUpdateAnthropicChannel(c.Request.Context(), s.store, credential)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if s.anthropicCredentials != nil {
		s.anthropicCredentials.invalidate(channel.ID)
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{
		"status": "complete", "channel_id": channel.ID, "created": created,
	})
}

// HandleRefreshAnthropicCredential forces the channel's atomic refresh path.
func (s *Server) HandleRefreshAnthropicCredential(c *gin.Context) {
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
	if !cfg.UsesAnthropicOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use Anthropic OAuth")
		return
	}
	if s.anthropicCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic credential refresh is unavailable")
		return
	}
	credential, err := s.anthropicCredentials.credential(c.Request.Context(), cfg, true)
	if err != nil {
		RespondError(c, http.StatusBadGateway, anthropicCredentialRefreshError(err))
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}

func anthropicCredentialRefreshError(err error) error {
	var upstreamErr interface{ UpstreamResponseBody() string }
	if !errors.As(err, &upstreamErr) {
		return err
	}
	body := strings.TrimSpace(upstreamErr.UpstreamResponseBody())
	if body == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, body)
}
