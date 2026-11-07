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

	"ccLoad/internal/accioauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

var accioChannelCreateMu sync.Mutex

type accioOAuthStartRequest struct {
	Region string `json:"region"`
}

func (r *accioOAuthStartRequest) Validate() error {
	r.Region = strings.TrimSpace(r.Region)
	if r.Region != "cn" && r.Region != "global" {
		return errors.New("accio region must be cn or global")
	}
	return nil
}

func newAccioOAuthManager(s *Server) *codexOAuthManager {
	manager := &codexOAuthManager{
		provider: "Accio", callbackPath: "/auth/callback-accio", listenAddr: "127.0.0.1:0",
		timeout: codexOAuthTimeout, now: time.Now, sessions: make(map[string]*codexOAuthSession),
	}
	manager.parseCallbackRequest = func(request *http.Request, state string) (codexOAuthResult, error) {
		result, err := parseOAuthCallbackURL("http://"+request.Host+request.URL.RequestURI(), manager.callbackPath, manager.provider)
		if err != nil {
			return codexOAuthResult{}, err
		}
		if !oauthStateEqual(result.state, state) {
			return codexOAuthResult{}, errors.New("invalid Accio OAuth state")
		}
		return result, nil
	}
	manager.prepareWithHint = func(redirectURI, region string) (string, string, func(context.Context, string) (any, error), func(context.Context, any) (int64, error), error) {
		if s == nil || s.accioService == nil || s.store == nil {
			return "", "", nil, nil, errors.New("oauth: Accio is unavailable")
		}
		state, err := codexauth.GenerateState()
		if err != nil {
			return "", "", nil, nil, err
		}
		pkce, err := codexauth.GeneratePKCE()
		if err != nil {
			return "", "", nil, nil, err
		}
		loginURL, err := accioauth.AuthorizationLink(region, state, pkce.Challenge, redirectURI)
		if err != nil {
			return "", "", nil, nil, err
		}
		exchange := func(ctx context.Context, code string) (any, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			flowService := *s.accioService
			credential, err := flowService.ExchangeCode(ctx, region, code, pkce.Verifier, redirectURI)
			if err != nil {
				return nil, err
			}
			return flowService.CompleteIdentity(ctx, credential)
		}
		commit := func(ctx context.Context, raw any) (int64, error) {
			credential, ok := raw.(*accioauth.Credential)
			if !ok || credential == nil {
				return 0, errors.New("oauth: Accio exchange returned an invalid credential")
			}
			channel, _, err := s.commitAccioCredential(ctx, credential)
			if err != nil {
				return 0, err
			}
			return channel.ID, nil
		}
		return state, loginURL, exchange, commit, nil
	}
	return manager
}

// HandleStartAccioOAuth starts a browser login for the requested edition.
func (s *Server) HandleStartAccioOAuth(c *gin.Context) {
	var request accioOAuthStartRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.accioOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Accio OAuth is unavailable")
		return
	}
	loginURL, state, err := s.accioOAuth.startWithHint(request.Region)
	if err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"url": loginURL, "state": state, "status": "pending"})
}

// HandleAccioOAuthStatus returns a browser login's current state.
func (s *Server) HandleAccioOAuthStatus(c *gin.Context) {
	if s.accioOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Accio OAuth is unavailable")
		return
	}
	status, ok := s.accioOAuth.status(c.Query("state"))
	if !ok {
		RespondErrorMsg(c, http.StatusNotFound, "Accio OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelAccioOAuth cancels a pending browser login.
func (s *Server) HandleCancelAccioOAuth(c *gin.Context) {
	var request codexOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.accioOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Accio OAuth is unavailable")
		return
	}
	if err := s.accioOAuth.cancel(request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": strings.TrimSpace(request.State), "status": "cancelled"})
}

// HandleSubmitAccioOAuthCallback accepts a copied loopback URL on remote hosts.
func (s *Server) HandleSubmitAccioOAuthCallback(c *gin.Context) {
	var request codexOAuthCallbackRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if s.accioOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Accio OAuth is unavailable")
		return
	}
	state, err := s.accioOAuth.submitCallbackURL(request.CallbackURL)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"state": state, "status": "accepted"})
}

func accioChannelBaseName(credential *accioauth.Credential) string {
	digest := sha256.Sum256([]byte(credential.Region + "/" + credential.UserID))
	return fmt.Sprintf("Accio-%s-%x", credential.Region, digest[:4])
}
func newAccioChannel(name, credential string) *model.Config {
	c, _ := accioauth.ParseCredential([]byte(credential))
	endpoint := ""
	if c != nil {
		endpoint = c.GatewayURL()
	}
	return &model.Config{Name: name, AuthType: model.AuthTypeAccioOAuth, OAuthCredential: credential,
		URLs:                  model.ChannelURLs{{URL: strings.TrimRight(endpoint, "/") + "/v1/chat/completions", Exact: true, Protocols: []string{"openai"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal, Enabled: true, CostMultiplier: 1}
}
func (s *Server) prepareAccioChannel(ctx context.Context, name, credential string) (*model.Config, error) {
	cfg := newAccioChannel(name, credential)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := s.fetchAccioOAuthModels(ctx, cfg, "")
	if err != nil {
		return nil, err
	}
	cfg.ModelEntries = response.Models
	return cfg, nil
}
func accioIdentityMatches(a, b *accioauth.Credential) bool {
	return a.Region == b.Region && a.UserID == b.UserID
}

func validateAccioImport(ctx context.Context, service *accioauth.Service, credential *accioauth.Credential) (*accioauth.Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	validated, err := service.CompleteIdentity(ctx, credential)
	var authErr *accioauth.HTTPError
	if errors.As(err, &authErr) && authErr.StatusCode == http.StatusUnauthorized && credential.RefreshToken != "" {
		refreshed, refreshErr := service.Refresh(ctx, credential)
		if refreshErr != nil {
			return nil, refreshErr
		}
		return service.CompleteIdentity(ctx, refreshed)
	}
	return validated, err
}

func (s *Server) commitAccioCredential(ctx context.Context, credential *accioauth.Credential) (*model.Config, bool, error) {
	payload, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	accioChannelCreateMu.Lock()
	defer accioChannelCreateMu.Unlock()
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, false, err
	}
	var cfg *model.Config
	for _, existing := range configs {
		if !existing.UsesAccioOAuth() {
			continue
		}
		stored, parseErr := accioauth.ParseCredential([]byte(existing.OAuthCredential))
		if parseErr != nil || !accioIdentityMatches(stored, credential) {
			continue
		}
		credential.OAuthUsage = stored.OAuthUsage
		if credential.DeviceID == "" {
			credential.DeviceID = stored.DeviceID
		}
		if credential.SGCookie == "" {
			credential.SGCookie = stored.SGCookie
		}
		payload, err = credential.JSON()
		if err != nil {
			return nil, false, err
		}
		updated, updateErr := s.store.CompareAndSwapOAuthCredential(ctx, existing.ID, model.AuthTypeAccioOAuth, existing.OAuthCredential, payload)
		if updateErr != nil {
			return nil, false, updateErr
		}
		if !updated {
			return nil, false, errors.New("accio credential changed concurrently; retry authorization")
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
		name := accioChannelBaseName(credential)
		cfg, err = s.prepareAccioChannel(ctx, uniqueZedChannelName(configs, name), payload)
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

// HandleImportAccioCredential imports an official or canonical login credential.
func (s *Server) HandleImportAccioCredential(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		RespondErrorMsg(c, 400, "invalid Accio credential body")
		return
	}
	var input struct {
		Region     string          `json:"region"`
		Credential json.RawMessage `json:"credential"`
	}
	if json.Unmarshal(raw, &input) != nil {
		RespondErrorMsg(c, 400, "invalid Accio import JSON")
		return
	}
	if len(input.Credential) > 0 {
		raw = input.Credential
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		RespondErrorMsg(c, 400, "invalid Accio credential JSON")
		return
	}
	if input.Region != "" {
		var region string
		if value, ok := fields["region"]; ok && (json.Unmarshal(value, &region) != nil || region != input.Region) {
			RespondErrorMsg(c, 400, "accio credential belongs to a different region")
			return
		}
		fields["region"], _ = json.Marshal(input.Region)
		raw, _ = json.Marshal(fields)
	}
	credential, err := accioauth.ParseCredential(raw)
	if err == nil {
		credential, err = validateAccioImport(c.Request.Context(), s.accioService, credential)
	}
	if err != nil {
		RespondError(c, 400, err)
		return
	}
	cfg, created, err := s.commitAccioCredential(c.Request.Context(), credential)
	if err != nil {
		RespondError(c, 502, err)
		return
	}
	RespondJSON(c, 200, gin.H{"channel_id": cfg.ID, "channel_name": cfg.Name, "created": created})
}

// HandleRefreshAccioCredential refreshes and persists a channel credential.
func (s *Server) HandleRefreshAccioCredential(c *gin.Context) {
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
	if !cfg.UsesAccioOAuth() {
		RespondErrorMsg(c, http.StatusConflict, "channel does not use Accio OAuth")
		return
	}
	credential, err := s.accioCredentials.credential(c.Request.Context(), cfg, true, "")
	if err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"oauth_credential": credential})
}
