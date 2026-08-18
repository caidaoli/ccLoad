package zaiauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Service talks to the ZCode and Z.ai control planes. It never persists
// anything and never returns a secret inside an error.
type Service struct {
	Client          *http.Client
	OAuthBaseURL    string
	BizBaseURL      string
	CodingModelsURL string
	ModelsURL       string
	AgentConfigsURL string
	CommunityURL    string
	QuotaLimitURL   string
	Now             func() time.Time
}

// NewService returns the production Z.ai service.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client: client, OAuthBaseURL: OAuthAPIBaseURL, BizBaseURL: BizBaseURL,
		CodingModelsURL: CodingModelsURL, ModelsURL: ModelsURL,
		AgentConfigsURL: AgentConfigsURL, CommunityURL: CommunityCatalogURL,
		QuotaLimitURL: QuotaLimitURL, Now: time.Now,
	}
}

// Flow is one pending ZCode CLI authorization.
type Flow struct {
	FlowID          string `json:"flow_id"`
	AuthorizeURL    string `json:"authorize_url"`
	ExpiresAt       int64  `json:"expires_at"`
	PollIntervalSec int    `json:"poll_interval_sec"`
}

// PollStatus is the state of a pending authorization.
type PollStatus string

const (
	// PollPending means the user has not finished authorizing yet.
	PollPending PollStatus = "pending"
	// PollReady means the authorization produced a credential.
	PollReady PollStatus = "ready"
	// PollFailed means the authorization was rejected upstream.
	PollFailed PollStatus = "failed"
)

// PollResult is one ZCode CLI poll response.
type PollResult struct {
	Status      PollStatus
	AccessToken string
	JWTToken    string
	Identity    Identity
	Name        string
}

// ErrOAuthFlowUnavailable reports that ZCode's hosted CLI OAuth flow rejected
// the request outright. The endpoint is enabled per client release, so this is
// an upstream availability fact rather than a ccLoad misconfiguration.
var ErrOAuthFlowUnavailable = errors.New("z.ai CLI OAuth flow is unavailable upstream")

// GeneratePollToken returns the client-side bearer that binds one CLI flow.
func GeneratePollToken() (string, error) {
	random := make([]byte, pollTokenBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate z.ai poll token: %w", err)
	}
	return hex.EncodeToString(random), nil
}

// InitFlow starts one hosted ZCode authorization bound to pollToken.
func (s *Service) InitFlow(ctx context.Context, pollToken string) (*Flow, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(pollToken) == "" {
		return nil, errors.New("z.ai poll token is required")
	}
	payload, err := json.Marshal(map[string]string{"provider": OAuthProvider})
	if err != nil {
		return nil, fmt.Errorf("encode z.ai OAuth init request: %w", err)
	}
	data, err := s.envelopeRequest(ctx, http.MethodPost,
		strings.TrimRight(s.OAuthBaseURL, "/")+"/oauth/cli/init", pollToken, payload, "OAuth init")
	if err != nil {
		return nil, err
	}
	var flow struct {
		FlowID          string `json:"flow_id"`
		PollToken       string `json:"poll_token"`
		AuthorizeURL    string `json:"authorize_url"`
		ExpiresAt       int64  `json:"expires_at"`
		PollIntervalSec int    `json:"poll_interval_sec"`
	}
	if err := json.Unmarshal(data, &flow); err != nil {
		return nil, fmt.Errorf("decode z.ai OAuth init response: %w", err)
	}
	if strings.TrimSpace(flow.FlowID) == "" || strings.TrimSpace(flow.AuthorizeURL) == "" {
		return nil, errors.New("z.ai OAuth init response is incomplete")
	}
	if _, err := url.Parse(flow.AuthorizeURL); err != nil {
		return nil, errors.New("z.ai OAuth init response has an invalid authorize_url")
	}
	return &Flow{
		FlowID: flow.FlowID, AuthorizeURL: flow.AuthorizeURL,
		ExpiresAt: flow.ExpiresAt, PollIntervalSec: flow.PollIntervalSec,
	}, nil
}

// Poll reads the current state of one pending authorization.
func (s *Service) Poll(ctx context.Context, flowID, pollToken string) (*PollResult, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(flowID) == "" || strings.TrimSpace(pollToken) == "" {
		return nil, errors.New("z.ai flow id and poll token are required")
	}
	target := strings.TrimRight(s.OAuthBaseURL, "/") + "/oauth/cli/poll/" + url.PathEscape(flowID)
	data, err := s.envelopeRequest(ctx, http.MethodGet, target, pollToken, nil, "OAuth poll")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Status string `json:"status"`
		Token  string `json:"token"`
		User   struct {
			UserID string `json:"user_id"`
			Email  string `json:"email"`
			Name   string `json:"name"`
		} `json:"user"`
		ZAI struct {
			AccessToken string `json:"access_token"`
		} `json:"zai"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode z.ai OAuth poll response: %w", err)
	}
	switch PollStatus(strings.TrimSpace(payload.Status)) {
	case PollPending:
		return &PollResult{Status: PollPending}, nil
	case PollFailed:
		return &PollResult{Status: PollFailed}, nil
	case PollReady:
		if strings.TrimSpace(payload.ZAI.AccessToken) == "" {
			return nil, errors.New("z.ai OAuth poll response is missing access_token")
		}
		return &PollResult{
			Status:      PollReady,
			AccessToken: strings.TrimSpace(payload.ZAI.AccessToken),
			JWTToken:    strings.TrimSpace(payload.Token),
			Identity: Identity{
				UserID: strings.TrimSpace(payload.User.UserID),
				Email:  strings.TrimSpace(payload.User.Email),
			},
			Name: strings.TrimSpace(payload.User.Name),
		}, nil
	default:
		return nil, errors.New("z.ai OAuth poll response has an unknown status")
	}
}

// ResolveCodingPlanAPIKey exchanges a ZCode access token for the account's
// Coding Plan API key, creating ZCode's key on first use exactly like the
// official client does.
func (s *Service) ResolveCodingPlanAPIKey(ctx context.Context, accessToken string) (string, Identity, error) {
	if err := s.validate(); err != nil {
		return "", Identity{}, err
	}
	if strings.TrimSpace(accessToken) == "" {
		return "", Identity{}, errors.New("z.ai access token is required")
	}
	bizToken, err := s.bizToken(ctx, accessToken)
	if err != nil {
		return "", Identity{}, err
	}
	customer, identity, err := s.customerInfo(ctx, bizToken)
	if err != nil {
		return "", Identity{}, err
	}
	keysURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys",
		strings.TrimRight(s.BizBaseURL, "/"), url.PathEscape(customer.organizationID), url.PathEscape(customer.projectID))
	apiKeyID, err := s.findOrCreateAPIKey(ctx, bizToken, keysURL)
	if err != nil {
		return "", identity, err
	}
	secret, err := s.copyAPIKeySecret(ctx, bizToken, keysURL, apiKeyID)
	if err != nil {
		return "", identity, err
	}
	return apiKeyID + "." + secret, identity, nil
}

// ListModels returns the model ids the account can call, newest catalog first.
//
// The Coding Plan keeps its own catalog: models reach it before the general API
// lists them (glm-5.3 did), so the plan endpoint is authoritative and the
// general one only fills in what it misses. Discovery is live on purpose — the
// lineup changes without a ccLoad release.
func (s *Service) ListModels(ctx context.Context, apiKey string) ([]string, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("z.ai API key is required")
	}
	models := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	var lastErr error
	for _, catalog := range []string{s.CodingModelsURL, s.ModelsURL} {
		if strings.TrimSpace(catalog) == "" {
			continue
		}
		listed, err := s.listCatalogModels(ctx, catalog, apiKey)
		if err != nil {
			lastErr = err
			continue
		}
		for _, id := range listed {
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			models = append(models, id)
		}
	}
	if len(models) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("z.ai model catalog is empty")
	}
	return models, nil
}

func (s *Service) listCatalogModels(ctx context.Context, catalogURL, apiKey string) ([]string, error) {
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build z.ai model list request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	ApplySourceHeaders(request.Header)
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("z.ai model list request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read z.ai model list response: %w", err)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, errors.New("z.ai rejected the Coding Plan API key")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("z.ai model list returned HTTP %d", response.StatusCode)
	}
	return parseModelCatalog(body)
}

// parseModelCatalog accepts the OpenAI-shaped catalog and the `{code,data}`
// envelope the Z.ai business APIs use, with either a list of objects or a plain
// list of ids.
func parseModelCatalog(body []byte) ([]string, error) {
	raw := body
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Data) > 0 {
		raw = envelope.Data
	}
	var entries []struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		ModelID string `json:"modelId"`
	}
	models := make([]string, 0, 8)
	if json.Unmarshal(raw, &entries) == nil {
		for _, entry := range entries {
			for _, candidate := range []string{entry.ID, entry.Model, entry.ModelID} {
				if id := strings.TrimSpace(candidate); id != "" {
					models = append(models, id)
					break
				}
			}
		}
		if len(models) > 0 {
			return models, nil
		}
	}
	var ids []string
	if json.Unmarshal(raw, &ids) == nil {
		for _, id := range ids {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				models = append(models, trimmed)
			}
		}
	}
	if len(models) == 0 {
		return nil, errors.New("z.ai model list response has no models")
	}
	return models, nil
}

// ListCommunityModels reads the Coding Plan lineup from models.dev. It needs no
// account key, so it answers even when the account catalog is unreachable or
// the key was rejected.
func (s *Service) ListCommunityModels(ctx context.Context) ([]string, error) {
	if s == nil || s.Client == nil || strings.TrimSpace(s.CommunityURL) == "" {
		return nil, errors.New("z.ai community catalog is unavailable")
	}
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, s.CommunityURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build z.ai community catalog request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("z.ai community catalog request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCommunityCatalogSize))
	if err != nil {
		return nil, fmt.Errorf("read z.ai community catalog: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("z.ai community catalog returned HTTP %d", response.StatusCode)
	}
	return parseCommunityCatalog(body, CommunityCatalogProvider)
}

// parseCommunityCatalog returns the provider's models newest first, so a
// channel seeded from it lists the current flagship before the older ones.
func parseCommunityCatalog(body []byte, provider string) ([]string, error) {
	var catalog map[string]struct {
		Models map[string]struct {
			ReleaseDate string `json:"release_date"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode z.ai community catalog: %w", err)
	}
	entry, ok := catalog[provider]
	if !ok || len(entry.Models) == 0 {
		return nil, fmt.Errorf("z.ai community catalog has no provider %q", provider)
	}
	type catalogModel struct {
		id          string
		releaseDate string
	}
	models := make([]catalogModel, 0, len(entry.Models))
	for id, meta := range entry.Models {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			models = append(models, catalogModel{id: trimmed, releaseDate: strings.TrimSpace(meta.ReleaseDate)})
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("z.ai community catalog provider %q has no models", provider)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].releaseDate != models[j].releaseDate {
			return models[i].releaseDate > models[j].releaseDate
		}
		return models[i].id < models[j].id
	})
	ids := make([]string, len(models))
	for i, entry := range models {
		ids[i] = entry.id
	}
	return ids, nil
}

// ValidateAPIKey confirms a Coding Plan key is accepted without spending quota.
func (s *Service) ValidateAPIKey(ctx context.Context, apiKey string) error {
	if err := s.validate(); err != nil {
		return err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return errors.New("z.ai API key is required")
	}
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, s.ModelsURL, nil)
	if err != nil {
		return fmt.Errorf("build z.ai model list request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	ApplySourceHeaders(request.Header)
	response, err := s.Client.Do(request)
	if err != nil {
		return fmt.Errorf("z.ai model list request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseSize))
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return errors.New("z.ai rejected the Coding Plan API key")
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return fmt.Errorf("z.ai model list returned HTTP %d", response.StatusCode)
	default:
		return nil
	}
}

// ResolveProxyBaseURL returns the Coding Plan endpoint ZCode currently routes
// to. Callers fall back to CodingPlanProxyBaseURL when the table is
// unavailable: routing is an optimization, never a hard dependency.
func (s *Service) ResolveProxyBaseURL(ctx context.Context) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, s.AgentConfigsURL, nil)
	if err != nil {
		return "", fmt.Errorf("build z.ai routing request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	ApplySourceHeaders(request.Header)
	response, err := s.Client.Do(request)
	if err != nil {
		return "", fmt.Errorf("z.ai routing request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return "", fmt.Errorf("read z.ai routing response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("z.ai routing returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data struct {
			ProxyEndpoint struct {
				Mapping []struct {
					From string `json:"from"`
					To   string `json:"to"`
				} `json:"mapping"`
			} `json:"proxyEndpoint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode z.ai routing response: %w", err)
	}
	const messagesPath = "/v1/messages"
	source := CodingPlanAPIBaseURL + messagesPath
	for _, mapping := range payload.Data.ProxyEndpoint.Mapping {
		if !strings.EqualFold(strings.TrimSpace(mapping.From), source) {
			continue
		}
		target := strings.TrimSpace(mapping.To)
		parsed, parseErr := url.Parse(target)
		if parseErr != nil || !strings.EqualFold(parsed.Scheme, "https") ||
			!strings.HasSuffix(parsed.Path, messagesPath) {
			return "", errors.New("z.ai routing table has an unusable Coding Plan endpoint")
		}
		return strings.TrimSuffix(target, messagesPath), nil
	}
	return "", errors.New("z.ai routing table has no Coding Plan endpoint")
}

type customerSelection struct {
	organizationID string
	projectID      string
}

func (s *Service) bizToken(ctx context.Context, accessToken string) (string, error) {
	payload, err := json.Marshal(map[string]string{"token": accessToken})
	if err != nil {
		return "", fmt.Errorf("encode z.ai business login request: %w", err)
	}
	data, err := s.bizRequest(ctx, http.MethodPost,
		strings.TrimRight(s.BizBaseURL, "/")+"/api/auth/z/login", "", payload, "business login")
	if err != nil {
		return "", err
	}
	var token struct {
		AccessToken string `json:"access_token"`
		Alias       string `json:"accessToken"`
	}
	if err := json.Unmarshal(data, &token); err != nil {
		return "", fmt.Errorf("decode z.ai business login response: %w", err)
	}
	resolved := strings.TrimSpace(token.AccessToken)
	if resolved == "" {
		resolved = strings.TrimSpace(token.Alias)
	}
	if resolved == "" {
		return "", errors.New("z.ai business login response is missing access_token")
	}
	return resolved, nil
}

func (s *Service) customerInfo(ctx context.Context, bizToken string) (customerSelection, Identity, error) {
	data, err := s.bizRequest(ctx, http.MethodGet,
		strings.TrimRight(s.BizBaseURL, "/")+"/api/biz/customer/getCustomerInfo", bizToken, nil, "customer info")
	if err != nil {
		return customerSelection{}, Identity{}, err
	}
	var payload struct {
		UserID        string `json:"userId"`
		Email         string `json:"email"`
		Organizations []struct {
			OrganizationID   string `json:"organizationId"`
			OrganizationName string `json:"organizationName"`
			Projects         []struct {
				ProjectID   string `json:"projectId"`
				ProjectName string `json:"projectName"`
			} `json:"projects"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return customerSelection{}, Identity{}, fmt.Errorf("decode z.ai customer info response: %w", err)
	}
	if len(payload.Organizations) == 0 {
		return customerSelection{}, Identity{}, errors.New("z.ai account has no organization")
	}
	organization := payload.Organizations[0]
	for _, candidate := range payload.Organizations {
		if strings.Contains(candidate.OrganizationName, defaultOrganizationName) {
			organization = candidate
			break
		}
	}
	if len(organization.Projects) == 0 {
		return customerSelection{}, Identity{}, errors.New("z.ai organization has no project")
	}
	project := organization.Projects[0]
	for _, candidate := range organization.Projects {
		if strings.Contains(candidate.ProjectName, defaultProjectName) {
			project = candidate
			break
		}
	}
	selection := customerSelection{
		organizationID: strings.TrimSpace(organization.OrganizationID),
		projectID:      strings.TrimSpace(project.ProjectID),
	}
	if selection.organizationID == "" || selection.projectID == "" {
		return customerSelection{}, Identity{}, errors.New("z.ai customer info is missing organization or project")
	}
	identity := Identity{UserID: strings.TrimSpace(payload.UserID), Email: strings.TrimSpace(payload.Email)}
	return selection, identity, nil
}

func (s *Service) findOrCreateAPIKey(ctx context.Context, bizToken, keysURL string) (string, error) {
	data, err := s.bizRequest(ctx, http.MethodGet, keysURL, bizToken, nil, "API key list")
	if err != nil {
		return "", err
	}
	var existing []struct {
		Name   string `json:"name"`
		APIKey string `json:"apiKey"`
	}
	if len(data) > 0 && json.Unmarshal(data, &existing) == nil {
		for _, item := range existing {
			if item.Name == codingPlanAPIKeyName && strings.TrimSpace(item.APIKey) != "" {
				return strings.TrimSpace(item.APIKey), nil
			}
		}
	}
	payload, err := json.Marshal(map[string]string{"name": codingPlanAPIKeyName})
	if err != nil {
		return "", fmt.Errorf("encode z.ai API key request: %w", err)
	}
	created, err := s.bizRequest(ctx, http.MethodPost, keysURL, bizToken, payload, "API key creation")
	if err != nil {
		return "", err
	}
	var key struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(created, &key); err != nil {
		return "", fmt.Errorf("decode z.ai API key response: %w", err)
	}
	if strings.TrimSpace(key.APIKey) == "" {
		return "", errors.New("z.ai API key response is missing apiKey")
	}
	return strings.TrimSpace(key.APIKey), nil
}

func (s *Service) copyAPIKeySecret(ctx context.Context, bizToken, keysURL, apiKeyID string) (string, error) {
	target := strings.TrimRight(keysURL, "/") + "/copy/" + url.PathEscape(apiKeyID)
	data, err := s.bizRequest(ctx, http.MethodGet, target, bizToken, nil, "API key secret")
	if err != nil {
		return "", err
	}
	var payload struct {
		SecretKey string `json:"secretKey"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("decode z.ai API key secret response: %w", err)
	}
	if strings.TrimSpace(payload.SecretKey) == "" {
		return "", errors.New("z.ai API key secret response is missing secretKey")
	}
	return strings.TrimSpace(payload.SecretKey), nil
}

// envelopeRequest performs one ZCode `{code,msg,data}` request.
func (s *Service) envelopeRequest(
	ctx context.Context,
	method, target, bearer string,
	payload []byte,
	operation string,
) (json.RawMessage, error) {
	body, status, err := s.do(ctx, method, target, bearer, payload, operation)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		if status == http.StatusNotFound {
			return nil, fmt.Errorf("z.ai %s: %w", operation, ErrOAuthFlowUnavailable)
		}
		return nil, fmt.Errorf("decode z.ai %s response: %w", operation, err)
	}
	if err := checkEnvelopeCode(envelope.Code, envelope.Msg, status, operation); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// bizRequest performs one Z.ai business API request. The business API answers
// with the same envelope but tolerates several success codes.
func (s *Service) bizRequest(
	ctx context.Context,
	method, target, bearer string,
	payload []byte,
	operation string,
) (json.RawMessage, error) {
	body, status, err := s.do(ctx, method, target, bearer, payload, operation)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode z.ai %s response: %w", operation, err)
	}
	if err := checkEnvelopeCode(envelope.Code, envelope.Msg, status, operation); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

func (s *Service) do(
	ctx context.Context,
	method, target, bearer string,
	payload []byte,
	operation string,
) ([]byte, int, error) {
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, target, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build z.ai %s request: %w", operation, err)
	}
	if strings.TrimSpace(bearer) != "" {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(bearer))
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	ApplySourceHeaders(request.Header)
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("z.ai %s request: %w", operation, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read z.ai %s response: %w", operation, err)
	}
	if response.StatusCode == http.StatusNotFound && len(bytes.TrimSpace(body)) == 0 {
		return nil, response.StatusCode, fmt.Errorf("z.ai %s: %w", operation, ErrOAuthFlowUnavailable)
	}
	return body, response.StatusCode, nil
}

func checkEnvelopeCode(code json.RawMessage, msg string, status int, operation string) error {
	message := strings.TrimSpace(msg)
	if !envelopeCodeSucceeded(code) {
		if message == "" {
			message = fmt.Sprintf("business error %s", strings.TrimSpace(string(code)))
		}
		return fmt.Errorf("z.ai %s failed: %s", operation, message)
	}
	if status < 200 || status >= 300 {
		if message == "" {
			message = fmt.Sprintf("HTTP %d", status)
		}
		return fmt.Errorf("z.ai %s failed: %s", operation, message)
	}
	return nil
}

func envelopeCodeSucceeded(code json.RawMessage) bool {
	value := strings.Trim(strings.TrimSpace(string(code)), `"`)
	switch value {
	case "", "null", "0", "200":
		return true
	default:
		return false
	}
}

func (s *Service) validate() error {
	if s == nil || s.Client == nil || strings.TrimSpace(s.OAuthBaseURL) == "" ||
		strings.TrimSpace(s.BizBaseURL) == "" || strings.TrimSpace(s.ModelsURL) == "" ||
		strings.TrimSpace(s.AgentConfigsURL) == "" {
		return errors.New("z.ai service is unavailable")
	}
	return nil
}

func requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, RequestTimeout)
}
