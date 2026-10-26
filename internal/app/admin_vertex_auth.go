package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/vertexauth"

	"github.com/gin-gonic/gin"
)

const (
	maxVertexCredentialImportBytes = 1 << 20

	// sub2api upstream_models.go: the list caps pageSize at 300.
	vertexPublisherModelsPageSize    = 300
	vertexPublisherModelsMaxPages    = 20
	vertexGooglePublisherModelPrefix = "publishers/google/models/"
	maxVertexModelListResponseBytes  = 4 << 20
)

var (
	vertexChannelCreateMu       sync.Mutex
	errVertexCredentialUnusable = errors.New("vertex service account could not mint an access token")
)

type vertexCredentialImportResponse struct {
	ChannelID   int64  `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Created     bool   `json:"created"`
	Email       string `json:"email"`
	ProjectID   string `json:"project_id"`
}

// vertexChannelBaseName is stable per service account and project, so batch
// imports skip accounts that already have a channel.
func vertexChannelBaseName(credential *vertexauth.Credential) string {
	digest := sha256.Sum256([]byte(strings.ToLower(credential.Email)))
	return fmt.Sprintf("Vertex-%s-%x", credential.ProjectID, digest[:4])
}

// newVertexOAuthChannel starts from the embedded catalog; refreshing the
// channel models replaces it with the list its Vertex host serves.
func newVertexOAuthChannel(name, credentialJSON string, credential *vertexauth.Credential) *model.Config {
	return &model.Config{Name: name, AuthType: model.AuthTypeVertexOAuth, OAuthCredential: credentialJSON,
		URLs:                  model.ChannelURLs{{URL: credential.BaseURL(), Protocols: []string{"gemini"}}},
		ModelEntries:          oauthModelEntries(vertexCatalogModels()),
		ProtocolTransformMode: model.ProtocolTransformModeLocal, Enabled: true, CostMultiplier: 1}
}

// listVertexPublisherModels pages through the Google publisher models of the
// channel's Vertex host and keeps the gemini-* entries (sub2api
// listVertexGeminiPublisherModels): regional hosts also list Gemma, Veo and
// AutoML garden models that the Gemini wire cannot serve.
func (s *Server) listVertexPublisherModels(ctx context.Context, cfg *model.Config, channelURL string) ([]string, error) {
	accessToken, err := s.vertexCredentials.accessToken(ctx, cfg, "")
	if err != nil {
		return nil, err
	}
	client := s.getClientForChannel(cfg)
	seen := make(map[string]struct{})
	var models []string
	pageToken := ""
	for range vertexPublisherModelsMaxPages {
		listURL, err := vertexPublisherModelsURL(channelURL, pageToken)
		if err != nil {
			return nil, err
		}
		page, err := fetchVertexPublisherModelsPage(ctx, client, listURL, accessToken)
		if err != nil {
			return nil, err
		}
		for _, entry := range page.PublisherModels {
			id, ok := strings.CutPrefix(strings.TrimSpace(entry.Name), vertexGooglePublisherModelPrefix)
			if !ok || !strings.HasPrefix(id, "gemini-") || id == "gemini-" {
				continue
			}
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				models = append(models, id)
			}
		}
		if pageToken = strings.TrimSpace(page.NextPageToken); pageToken == "" {
			if len(models) == 0 {
				return nil, errors.New("vertex publisher model list has no Gemini models")
			}
			return models, nil
		}
	}
	return nil, fmt.Errorf("vertex publisher model list did not finish within %d pages", vertexPublisherModelsMaxPages)
}

// vertexPublisherModelsURL maps a channel publisher base
// ({prefix}/v1/projects/{p}/locations/{l}/publishers/google) onto the model
// list of the same host. The list exists only as v1beta1 and is not project
// scoped; the regional host decides which models it returns.
func vertexPublisherModelsURL(channelURL, pageToken string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(channelURL))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid Vertex channel URL %q", channelURL)
	}
	prefix, _, found := strings.Cut(parsed.Path, "/v1/")
	if !found {
		return "", fmt.Errorf("vertex channel URL %q has no /v1/ path", channelURL)
	}
	query := url.Values{"pageSize": {strconv.Itoa(vertexPublisherModelsPageSize)}}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	listURL := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: prefix + "/v1beta1/publishers/google/models", RawQuery: query.Encode()}
	return listURL.String(), nil
}

type vertexPublisherModelsPage struct {
	PublisherModels []struct {
		Name string `json:"name"`
	} `json:"publisherModels"`
	NextPageToken string `json:"nextPageToken"`
}

func fetchVertexPublisherModelsPage(ctx context.Context, client *http.Client, listURL, accessToken string) (*vertexPublisherModelsPage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVertexModelListResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("vertex publisher model list HTTP %d: %s", resp.StatusCode, truncateErr(string(body)))
	}
	var page vertexPublisherModelsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("decode Vertex publisher model list: %w", err)
	}
	return &page, nil
}

// validateVertexCredential mints one access token so an unusable key is
// rejected at import instead of cooling the channel on its first request.
func (s *Server) validateVertexCredential(ctx context.Context, cfg *model.Config, credential *vertexauth.Credential) error {
	ctx, cancel := context.WithTimeout(ctx, vertexTokenMintTimeout)
	defer cancel()
	if _, err := credential.MintAccessToken(ctx, s.getClientForChannel(cfg), time.Now()); err != nil {
		return fmt.Errorf("%w: %w", errVertexCredentialUnusable, err)
	}
	return nil
}

// commitVertexCredential replaces the key of the channel serving the same
// service account and project, or creates a channel routed to the credential's
// location. An existing channel keeps its URL: the channel URL owns routing.
func (s *Server) commitVertexCredential(ctx context.Context, credential *vertexauth.Credential) (*model.Config, bool, error) {
	payload, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	vertexChannelCreateMu.Lock()
	defer vertexChannelCreateMu.Unlock()
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, existing := range configs {
		if existing == nil || !existing.UsesVertexOAuth() {
			continue
		}
		stored, parseErr := vertexauth.ParseCredential([]byte(existing.OAuthCredential))
		if parseErr != nil || !stored.SameAccount(credential) {
			continue
		}
		if err := s.validateVertexCredential(ctx, existing, credential); err != nil {
			return nil, false, err
		}
		updated, updateErr := s.store.CompareAndSwapOAuthCredential(ctx, existing.ID, model.AuthTypeVertexOAuth, existing.OAuthCredential, payload)
		if updateErr != nil {
			return nil, false, updateErr
		}
		if !updated {
			return nil, false, errors.New("vertex credential changed concurrently; retry import")
		}
		cfg, err := reactivateReauthorizedChannel(ctx, s.store, existing.ID)
		if err != nil {
			return nil, false, err
		}
		s.invalidateChannelRelatedCache(cfg.ID)
		s.InvalidateChannelListCache()
		return cfg, false, nil
	}
	cfg := newVertexOAuthChannel(uniqueZedChannelName(configs, vertexChannelBaseName(credential)), payload, credential)
	if err := s.validateVertexCredential(ctx, cfg, credential); err != nil {
		return nil, false, err
	}
	cfg, err = s.store.CreateConfig(ctx, cfg)
	if err != nil {
		return nil, false, err
	}
	s.invalidateChannelRelatedCache(cfg.ID)
	s.InvalidateChannelListCache()
	return cfg, true, nil
}

// HandleImportVertexCredential imports a Google service account key or a
// CLIProxyAPI Vertex auth file. The optional location query overrides the
// file's location for a newly created channel.
func (s *Server) HandleImportVertexCredential(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxVertexCredentialImportBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid Vertex credential body")
		return
	}
	credential, err := vertexauth.ParseCredential(raw)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if location := strings.TrimSpace(c.Query("location")); location != "" {
		credential.Location = location
		if err := credential.Normalize(); err != nil {
			RespondError(c, http.StatusBadRequest, err)
			return
		}
	}
	if s.store == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "vertex credential import is unavailable")
		return
	}
	cfg, created, err := s.commitVertexCredential(c.Request.Context(), credential)
	if err != nil {
		if errors.Is(err, errVertexCredentialUnusable) {
			RespondError(c, http.StatusBadGateway, err)
			return
		}
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, vertexCredentialImportResponse{
		ChannelID: cfg.ID, ChannelName: cfg.Name, Created: created,
		Email: credential.Email, ProjectID: credential.ProjectID,
	})
}
