package cursorauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Service talks to Cursor's CLI control plane. It never persists anything and
// never returns a secret inside an error.
type Service struct {
	Client     *http.Client
	APIBaseURL string
	Now        func() time.Time
}

// NewService returns the production Cursor service.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client: client, APIBaseURL: APIBaseURL, Now: time.Now,
	}
}

// TokenPair is the session JWT pair stored by the CLI.
type TokenPair struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
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
	if s == nil || s.Client == nil || strings.TrimSpace(s.APIBaseURL) == "" {
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
