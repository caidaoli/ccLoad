package cursorauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service talks to Cursor's CLI control plane. It never persists anything and
// never returns a secret inside an error.
type Service struct {
	Client     *http.Client
	APIBaseURL string
	WebsiteURL string
	Now        func() time.Time
}

// NewService returns the production Cursor service.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client: client, APIBaseURL: APIBaseURL, WebsiteURL: WebsiteURL, Now: time.Now,
	}
}

// Flow is one pending CLI authorization generated locally. The verifier never
// leaves the server: only uuid+challenge are placed on the login URL.
type Flow struct {
	UUID         string
	Verifier     string
	AuthorizeURL string
}

// PollStatus is the state of a pending authorization.
type PollStatus string

const (
	// PollPending means the user has not finished authorizing yet.
	PollPending PollStatus = "pending"
	// PollReady means the authorization produced session tokens.
	PollReady PollStatus = "ready"
	// PollFailed means the authorization was rejected or abandoned upstream.
	PollFailed PollStatus = "failed"
)

// PollResult is one CLI poll response.
type PollResult struct {
	Status       PollStatus
	AccessToken  string
	RefreshToken string
}

// TokenPair is the session JWT pair stored by the CLI.
type TokenPair struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// InitFlow starts one hosted CLI authorization. No upstream call is required:
// the CLI builds the same PKCE material locally.
func (s *Service) InitFlow() (*Flow, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	raw := make([]byte, verifierBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate cursor PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("generate cursor login uuid: %w", err)
	}
	login, err := url.Parse(strings.TrimRight(s.WebsiteURL, "/") + "/loginDeepControl")
	if err != nil {
		return nil, errors.New("cursor website URL is invalid")
	}
	query := login.Query()
	query.Set("challenge", challenge)
	query.Set("uuid", id.String())
	query.Set("mode", "login")
	query.Set("redirectTarget", "cli")
	login.RawQuery = query.Encode()
	return &Flow{UUID: id.String(), Verifier: verifier, AuthorizeURL: login.String()}, nil
}

// Poll reads the current state of one pending authorization.
func (s *Service) Poll(ctx context.Context, flowUUID, verifier string) (*PollResult, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	flowUUID = strings.TrimSpace(flowUUID)
	verifier = strings.TrimSpace(verifier)
	if flowUUID == "" || verifier == "" {
		return nil, errors.New("cursor poll uuid and verifier are required")
	}
	target := strings.TrimRight(s.APIBaseURL, "/") + AuthPollPath +
		"?uuid=" + url.QueryEscape(flowUUID) + "&verifier=" + url.QueryEscape(verifier)
	body, status, err := s.do(ctx, http.MethodGet, target, "", nil, "OAuth poll")
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusNotFound:
		return &PollResult{Status: PollPending}, nil
	case http.StatusForbidden:
		return &PollResult{Status: PollFailed}, nil
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("cursor OAuth poll returned HTTP %d", status)
	}
	var pair TokenPair
	if err := json.Unmarshal(body, &pair); err != nil {
		return nil, fmt.Errorf("decode cursor OAuth poll response: %w", err)
	}
	pair.AccessToken = strings.TrimSpace(pair.AccessToken)
	pair.RefreshToken = strings.TrimSpace(pair.RefreshToken)
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		return &PollResult{Status: PollFailed}, nil
	}
	return &PollResult{Status: PollReady, AccessToken: pair.AccessToken, RefreshToken: pair.RefreshToken}, nil
}

// ExchangeAPIKey swaps a Cursor user API key for a session token pair, matching
// `cursor-agent login --api-key`.
func (s *Service) ExchangeAPIKey(ctx context.Context, apiKey string) (*TokenPair, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("cursor API key is required")
	}
	body, status, err := s.do(ctx, http.MethodPost,
		strings.TrimRight(s.APIBaseURL, "/")+ExchangeAPIKeyPath, apiKey, []byte("{}"), "API key exchange")
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, errors.New("cursor rejected the API key")
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("cursor API key exchange returned HTTP %d", status)
	}
	var pair TokenPair
	if err := json.Unmarshal(body, &pair); err != nil {
		return nil, fmt.Errorf("decode cursor API key exchange response: %w", err)
	}
	pair.AccessToken = strings.TrimSpace(pair.AccessToken)
	pair.RefreshToken = strings.TrimSpace(pair.RefreshToken)
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		return nil, errors.New("cursor API key exchange response is incomplete")
	}
	return &pair, nil
}

// FetchIdentity reads the account behind a session token.
func (s *Service) FetchIdentity(ctx context.Context, accessToken string) (Identity, string, error) {
	var payload struct {
		AuthID    string `json:"authId"`
		Email     string `json:"email"`
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	}
	if err := s.connectJSON(ctx, GetMeRPC, accessToken, map[string]any{}, &payload, "identity"); err != nil {
		return Identity{}, "", err
	}
	name := strings.TrimSpace(strings.TrimSpace(payload.FirstName) + " " + strings.TrimSpace(payload.LastName))
	identity := Identity{
		UserID: strings.TrimSpace(payload.AuthID),
		Email:  strings.TrimSpace(payload.Email),
	}
	if identity.IsZero() {
		return Identity{}, "", errors.New("cursor identity response is incomplete")
	}
	return identity, name, nil
}

// ListModels returns the public model ids the account can call.
func (s *Service) ListModels(ctx context.Context, accessToken string) ([]string, error) {
	var payload struct {
		Models []struct {
			ModelID        string   `json:"modelId"`
			DisplayModelID string   `json:"displayModelId"`
			Aliases        []string `json:"aliases"`
		} `json:"models"`
	}
	if err := s.connectJSON(ctx, ModelsRPC, accessToken, map[string]any{}, &payload, "model list"); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(payload.Models))
	seen := make(map[string]struct{}, len(payload.Models))
	add := func(id string) {
		id = PublicModelID(id)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	for _, entry := range payload.Models {
		for _, alias := range entry.Aliases {
			if strings.EqualFold(strings.TrimSpace(alias), "auto") {
				add("auto")
			}
		}
		if strings.EqualFold(strings.TrimSpace(entry.DisplayModelID), "auto") {
			add("auto")
			continue
		}
		if id := strings.TrimSpace(entry.DisplayModelID); id != "" {
			add(id)
			continue
		}
		add(entry.ModelID)
	}
	if len(models) == 0 {
		return nil, errors.New("cursor model catalog is empty")
	}
	return models, nil
}

func (s *Service) connectJSON(ctx context.Context, rpc, accessToken string, payload, dest any, operation string) error {
	if err := s.validate(); err != nil {
		return err
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return errors.New("cursor access token is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode cursor %s request: %w", operation, err)
	}
	raw, status, err := s.do(ctx, http.MethodPost, strings.TrimRight(s.APIBaseURL, "/")+rpc, accessToken, body, operation)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return errors.New("cursor rejected the session token")
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("cursor %s returned HTTP %d", operation, status)
	}
	if dest == nil {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("decode cursor %s response: %w", operation, err)
	}
	return nil
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
		return nil, 0, fmt.Errorf("build cursor %s request: %w", operation, err)
	}
	if strings.TrimSpace(bearer) != "" {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(bearer))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if strings.Contains(target, "/aiserver.") || strings.Contains(target, "/agent.") {
		request.Header.Set("connect-protocol-version", "1")
		ApplySourceHeaders(request.Header)
	}
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("cursor %s request: %w", operation, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read cursor %s response: %w", operation, err)
	}
	return body, response.StatusCode, nil
}

func (s *Service) validate() error {
	if s == nil || s.Client == nil || strings.TrimSpace(s.APIBaseURL) == "" || strings.TrimSpace(s.WebsiteURL) == "" {
		return errors.New("cursor service is unavailable")
	}
	return nil
}

func requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, RequestTimeout)
}
