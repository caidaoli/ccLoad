// Package codexauth implements the Codex OAuth credential lifecycle used by
// Codex channels. The wire contract follows CLIProxyAPI's Codex OAuth flow.
package codexauth

import (
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
)

// Production Codex OAuth endpoints and public client configuration.
const (
	DefaultAuthorizationURL = "https://auth.openai.com/oauth/authorize"
	DefaultTokenURL         = "https://auth.openai.com/oauth/token"
	DefaultClientID         = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultRedirectURI      = "http://localhost:1455/auth/callback"
	defaultTokenTimeout     = 30 * time.Second
	maxTokenResponseBytes   = 1 << 20
)

// PKCE is one RFC 7636 verifier/challenge pair.
type PKCE struct {
	Verifier  string
	Challenge string
}

// Service exchanges authorization codes and refresh tokens with OpenAI.
type Service struct {
	Client           *http.Client
	AuthorizationURL string
	TokenURL         string
	ClientID         string
	RedirectURI      string
}

type tokenEndpointError struct {
	statusCode   int
	responseBody string
}

func (e *tokenEndpointError) Error() string {
	if e == nil {
		return "codex token endpoint request failed"
	}
	return fmt.Sprintf("codex token endpoint returned HTTP %d", e.statusCode)
}

// UpstreamResponseBody returns the bounded response body for an explicitly authorized caller.
func (e *tokenEndpointError) UpstreamResponseBody() string {
	if e == nil {
		return ""
	}
	return e.responseBody
}

// StatusCode exposes the token endpoint status without exposing the private error type.
func (e *tokenEndpointError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

// NewService returns the production Codex OAuth service.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client:           client,
		AuthorizationURL: DefaultAuthorizationURL,
		TokenURL:         DefaultTokenURL,
		ClientID:         DefaultClientID,
		RedirectURI:      DefaultRedirectURI,
	}
}

// GeneratePKCE returns a high-entropy S256 verifier/challenge pair.
func GeneratePKCE() (PKCE, error) {
	random := make([]byte, 96)
	if _, err := rand.Read(random); err != nil {
		return PKCE{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(verifier))
	return PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(digest[:]),
	}, nil
}

// GenerateState returns an unguessable OAuth state value.
func GenerateState() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// AuthorizationLink builds the Codex authorization URL for a PKCE session.
func (s *Service) AuthorizationLink(state string, pkce PKCE) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(state) == "" {
		return "", errors.New("oauth state is required")
	}
	if pkce.Verifier == "" || pkce.Challenge == "" {
		return "", errors.New("PKCE verifier and challenge are required")
	}

	parsed, err := url.Parse(s.AuthorizationURL)
	if err != nil {
		return "", fmt.Errorf("parse authorization URL: %w", err)
	}
	query := parsed.Query()
	query.Set("client_id", s.ClientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", s.RedirectURI)
	query.Set("scope", "openid email profile offline_access")
	query.Set("state", state)
	query.Set("code_challenge", pkce.Challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("prompt", "login")
	query.Set("id_token_add_organizations", "true")
	query.Set("codex_cli_simplified_flow", "true")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// ExchangeCode exchanges one authorization code for a persistent credential.
func (s *Service) ExchangeCode(ctx context.Context, code string, pkce PKCE) (*Credential, error) {
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("authorization code is required")
	}
	if pkce.Verifier == "" {
		return nil, errors.New("PKCE verifier is required")
	}
	return s.requestToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {s.ClientID},
		"code":          {strings.TrimSpace(code)},
		"redirect_uri":  {s.RedirectURI},
		"code_verifier": {pkce.Verifier},
	})
}

// Refresh exchanges a refresh token for a new access token.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Credential, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, errors.New("refresh token is required")
	}
	return s.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {s.ClientID},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	})
}

func (s *Service) validate() error {
	if s == nil {
		return errors.New("codex OAuth service is nil")
	}
	if s.Client == nil {
		return errors.New("codex OAuth HTTP client is nil")
	}
	if strings.TrimSpace(s.AuthorizationURL) == "" || strings.TrimSpace(s.TokenURL) == "" ||
		strings.TrimSpace(s.ClientID) == "" || strings.TrimSpace(s.RedirectURI) == "" {
		return errors.New("codex OAuth service configuration is incomplete")
	}
	return nil
}

func (s *Service) requestToken(ctx context.Context, values url.Values) (*Credential, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tokenCtx, cancel := context.WithTimeout(ctx, defaultTokenTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(tokenCtx, http.MethodPost, s.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build Codex token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read Codex token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &tokenEndpointError{statusCode: resp.StatusCode, responseBody: string(body)}
	}

	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("decode Codex token response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("codex token response is missing access_token")
	}
	if token.ExpiresIn <= 0 {
		return nil, errors.New("codex token response has invalid expires_in")
	}

	credential := &Credential{
		Type:         ChannelType,
		IDToken:      token.IDToken,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		Expired:      time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339),
		LastRefresh:  time.Now().UTC().Format(time.RFC3339),
	}
	if claims, parseErr := parseIDToken(token.IDToken); parseErr == nil {
		credential.ChatGPTUserID = claims.Auth.ChatGPTUserID
		credential.AccountID = claims.Auth.ChatGPTAccountID
		credential.Email = claims.Email
		credential.PlanType = claims.Auth.ChatGPTPlanType
	}
	return credential, nil
}

type idTokenClaims struct {
	Email string `json:"email"`
	Auth  struct {
		ChatGPTUserID                  string `json:"chatgpt_user_id"`
		ChatGPTAccountID               string `json:"chatgpt_account_id"`
		ChatGPTPlanType                string `json:"chatgpt_plan_type"`
		ChatGPTSubscriptionActiveStart any    `json:"chatgpt_subscription_active_start"`
		ChatGPTSubscriptionActiveUntil any    `json:"chatgpt_subscription_active_until"`
	} `json:"https://api.openai.com/auth"`
}

func parseIDToken(token string) (idTokenClaims, error) {
	var claims idTokenClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errors.New("invalid ID token format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, fmt.Errorf("decode ID token claims: %w", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("decode ID token JSON: %w", err)
	}
	return claims, nil
}
